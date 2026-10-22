package runtime

// The ADR-0160 early-speaker adjudication, from the internal side: the
// speaker-only bundle the confident local top candidate on a shared slot owes,
// the answer it overlays, and the early ask's own deadline (the
// EarlySpeakerBudget the proposal named — the documented default is 300 ms —
// after which the commit proceeds on the local evidence alone and speaks the
// narrator, never stalling on the answer).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"log/slog"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/store"
)

// earlySpeakerTestModel is the model id the fake gateway answers with; the
// client validates the response's model against the role's.
const earlySpeakerTestModel = "clef-fake"

// earlyClef is one fake System One gateway for the early-speaker ask: it
// answers the speaker + speaker_kind questions the speaker-only bundle renders
// (the scripted spN alias and kind id, each with the chosen option's
// probability as the confidence) and can hold the answer or fail it.
type earlyClef struct {
	server  *httptest.Server
	mu      sync.Mutex
	hits    int
	sleep   time.Duration
	fail    int    // 0 = answer; >0 = fail every request with this status
	speaker string // the speaker option id the scripted answer picks ("" = unknown)
	kind    string // the kind option id the scripted answer picks ("" = unknown)
}

func newEarlyClef(t *testing.T, o ...func(*earlyClef)) *earlyClef {
	t.Helper()
	g := &earlyClef{speaker: "sp1", kind: "boy"}
	for _, f := range o {
		f(g)
	}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *earlyClef) url() string { return g.server.URL }

func (g *earlyClef) hitsCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

type earlyClefWire struct {
	Questions map[string]struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

func (g *earlyClef) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/systemone" {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.hits++
	hits := g.hits
	g.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var wire earlyClefWire
	if err := json.Unmarshal(body, &wire); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if g.fail > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.fail)
		_, _ = w.Write([]byte(`{"detail":"synthetic outage"}`))
		return
	}
	if g.sleep > 0 {
		select {
		case <-time.After(g.sleep):
		case <-r.Context().Done():
			return
		}
	}
	answers := map[string]any{}
	for id, q := range wire.Questions {
		if q.Type != "choice" {
			continue
		}
		choice := ""
		switch id {
		case "speaker":
			g.mu.Lock()
			choice = g.speaker
			g.mu.Unlock()
		case "speaker_kind":
			g.mu.Lock()
			choice = g.kind
			g.mu.Unlock()
		default:
			continue
		}
		probs := map[string]float64{}
		switch id {
		case "speaker":
			others := len(q.Criteria) - 1
			probs[choice] = 0.9
			for opt := range q.Criteria {
				if opt == choice {
					continue
				}
				if others > 0 {
					probs[opt] = 0.1 / float64(others)
				} else {
					probs[opt] = 1.0
				}
			}
		case "speaker_kind":
			probs[choice] = 0.9
			for opt := range q.Criteria {
				if opt == choice {
					continue
				}
				probs[opt] = 0.1 / float64(len(q.Criteria)-1)
			}
		}
		answers[id] = map[string]any{
			"type":          "choice",
			"choice":        choice,
			"confidence":    probs[choice],
			"probabilities": probs,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", fmt.Sprintf("req-early-%d", hits))
	out, _ := json.Marshal(map[string]any{
		"model":   earlySpeakerTestModel,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 214, "output_tokens": 12},
	})
	_, _ = w.Write(out)
}

// newEarlySpeakerWiring builds the decision wiring the production Manager
// builds per session: a real store and event hub behind the Manager, the fake
// gateway behind the decision role's router entry, and the ADR-0160 options.
func newEarlySpeakerWiring(t *testing.T, g *earlyClef, decisionOpts *DecisionOptions) *decisionWiring {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	mgr, err := New(Options{Store: st, Hub: hub, Log: slog.New(slog.DiscardHandler), Decision: decisionOpts})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	t.Cleanup(func() { _ = hub.Close() })
	t.Cleanup(mgr.Close)
	return mgr.newDecisionWiring("early-speaker", map[string]string{})
}

// earlySpeakerOptions builds the DecisionOptions for one early-speaker ask:
// the decision role's System One client over the fake gateway, the ADR-0160
// budget, and a breaker that trips after one transient failure (the breaker
// behaviour is one of the ADR-0160 ask's own requirements).
func earlySpeakerOptions(g *earlyClef, budget time.Duration) *DecisionOptions {
	role := providers.RoleConfig{
		Role:     providers.RoleDecision,
		Protocol: providers.ProtocolSystemOne,
		BaseURL:  g.url(),
		Model:    earlySpeakerTestModel,
		Timeout:  5 * time.Second,
	}
	reg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{providers.RoleDecision: role})
	return &DecisionOptions{
		Router:             router.New(reg, router.Options{BreakerThreshold: 1, BreakerCooldown: time.Hour}),
		Role:               role,
		Margin:             1.0,
		EarlySpeakerBudget: budget,
	}
}

// earlySpeakerRequest is one confident local top candidate on a shared slot
// the early-speaker ask attributes: the top candidate the commit will speak.
func earlySpeakerRequest(top string) pipeline.DecisionRequest {
	return pipeline.DecisionRequest{
		SessionID:       "early-speaker",
		ProfileRevision: "rev1",
		RegionID:        "bottom",
		ObservationSeq:  7,
		Generation:      1,
		SceneID:         "outset",
		Text:            "the boy shouts back",
		RawText:         "the boy shouts back",
		Confidence:      0.9,
		Ranking: matcher.Ranking{
			Candidates: []matcher.Candidate{{MessageID: top, Score: 1.0, Probability: 0.9}},
			Margin:     0.5,
			Reason:     matcher.ReasonFuzzy,
		},
		Speakers:    []pipeline.SpeakerOption{{ID: "char-boy", Description: "a boy"}},
		LastMessage: "m-7",
		SpeakerOnly: true,
	}
}

// TestEarlySpeakerAnswerOverlaysOnConfidentLocalTop is the ADR-0160 happy
// path at the wiring seam: a confident local top candidate on a shared slot
// (the live shape that committed MATCH_FUZZY with high confidence and never
// asked Clef) gets the speaker-only adjudication, and the answer overlays the
// speaker + kind onto the local line through the attribution-only path — the
// line's own choice, eligibility and reason stand, and the answer may not
// narrow them.
func TestEarlySpeakerAnswerOverlaysOnConfidentLocalTop(t *testing.T) {
	g := newEarlyClef(t)
	w := newEarlySpeakerWiring(t, g, earlySpeakerOptions(g, 300*time.Millisecond))
	reply := w.Decide(context.Background(), earlySpeakerRequest("msg-line-a"))
	if reply.Reason != "answered" {
		t.Fatalf("reply.Reason = %q, want answered (the early answer landed)", reply.Reason)
	}
	if reply.Accepted != true {
		t.Fatalf("reply.Accepted = %v, want true (the early answer is valid)", reply.Accepted)
	}
	if reply.Msg == nil {
		t.Fatalf("the early answer must overlay onto the confident local top candidate")
	}
	if !reply.Msg.AttributionOnly {
		t.Fatalf("the early answer must carry AttributionOnly, got %+v", reply.Msg)
	}
	if reply.Msg.MessageID != "msg-line-a" {
		t.Fatalf("reply.Msg.MessageID = %q, want the local top candidate %q", reply.Msg.MessageID, "msg-line-a")
	}
	if !reply.Msg.SpeechEligible || !reply.Msg.Accepted {
		t.Fatalf("the local line's own verdict must stand: eligible=%v accepted=%v", reply.Msg.SpeechEligible, reply.Msg.Accepted)
	}
	if reply.Msg.Reason != "answered" {
		t.Fatalf("reply.Msg.Reason = %q, want answered", reply.Msg.Reason)
	}
	if reply.ChosenID != "msg-line-a" {
		t.Fatalf("reply.ChosenID = %q, want the local top candidate the answer attributes (no narrowing)", reply.ChosenID)
	}
	if reply.Choice != "" || reply.Probabilities != nil {
		t.Fatalf("the early answer may not narrow the candidate: choice=%q probabilities=%v", reply.Choice, reply.Probabilities)
	}
	if reply.Speaker != "char-boy" || !reply.SpeakerKnown || reply.SpeakerConfidence != 0.9 {
		t.Fatalf("reply speaker = %q known=%v conf=%.1f, want char-boy/true/0.9 (the early answer's attribution)", reply.Speaker, reply.SpeakerKnown, reply.SpeakerConfidence)
	}
	if reply.SpeakerKind != "boy" || reply.SpeakerKindConfidence != 0.9 {
		t.Fatalf("reply speaker kind = %q conf=%.1f, want boy/0.9 (the early answer's kind)", reply.SpeakerKind, reply.SpeakerKindConfidence)
	}
	if st := w.status(); !st.Configured || st.Degraded || st.Calls != 1 || st.Answered != 1 {
		t.Fatalf("session decision status = %+v, want configured, not degraded, 1 call/answered", st)
	}
}

// TestEarlySpeakerAskRunsOnTheDecisionRoleTimeoutAndBindsTheSpeechWait is
// the ADR-0160 latency half after the review-redesign: the early-speaker ask
// no longer bounds itself at the early budget (that budget moved to the
// speech side's bounded wait, the ADR-0160 proposal's 300 ms) — it runs on
// the decision role's normal timeout and waits for the answer. A fake that
// answers inside that timeout overlays; a fake that answers late does not.
// The speech side's bounded wait is what holds the commit to the budget, and
// a wait that outlives it must fall back to the narrator without stalling.
func TestEarlySpeakerAskRunsOnTheDecisionRoleTimeoutAndBindsTheSpeechWait(t *testing.T) {
	g := newEarlyClef(t, func(o *earlyClef) { o.sleep = 2 * time.Second })
	w := newEarlySpeakerWiring(t, g, earlySpeakerOptions(g, 60*time.Millisecond))
	start := time.Now()
	reply := w.Decide(context.Background(), earlySpeakerRequest("msg-line-a"))
	elapsed := time.Since(start)
	if reply.Reason != "answered" {
		t.Fatalf("reply.Reason = %q, want answered (the early answer landed inside the decision role's timeout)", reply.Reason)
	}
	if reply.Msg == nil {
		t.Fatalf("the early answer must overlay onto the confident local top candidate")
	}
	if reply.Speaker != "char-boy" || !reply.SpeakerKnown || reply.SpeakerConfidence != 0.9 {
		t.Fatalf("reply speaker = %q known=%v conf=%.1f, want char-boy/true/0.9", reply.Speaker, reply.SpeakerKnown, reply.SpeakerConfidence)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("the early ask waited %s — it must run on the decision role's normal timeout, not the early budget", elapsed)
	}

	// The speech side's bounded wait: a tight budget (60 ms) that the answer
	// (which landed at ~2 s) outlives. The wait must expire inside the budget,
	// and the commit then proceeds on the local evidence alone — the line
	// speaks the narrator, with no stall.
	gate := newSpeakerGateWiring(map[string]string{"msg-line-a": "character-aryll"}, map[string]bool{"msg-line-a": true})
	key := earlyAskKey("bottom", 1, "msg-line-a")
	if p := gate.beginEarlyAttribution(key, 60*time.Millisecond); p == nil {
		t.Fatalf("the early attribution wait must be registerable for the shared-slot occurrence")
	}
	start = time.Now()
	res, pending := gate.waitEarlyAttribution(context.Background(), key, 60*time.Millisecond)
	if pending {
		t.Fatalf("the speech-side wait must be bounded by the early budget, got pending after the answer landed")
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("the bounded wait took %s — it must not stall past the early budget", waited)
	}
	character, route := gate.commitVoice(commitPayload{
		MessageID:         "msg-line-a",
		SpeakerID:         res.speakerID,
		SpeakerKnown:      res.speakerKnown,
		SpeakerConfidence: res.speakerConf,
	})
	if character != narratorCharacter || route != voiceRouteNarrator {
		t.Fatalf("the commit's voice = %q/%q, want the narrator fallback when the early answer never landed in time", character, route)
	}
}

// TestEarlySpeakerWaitResolvesWithinTheBudget: the speech side's bounded
// wait holds while the early answer is in flight and resolves it inside the
// budget, so the committed shared-slot line voices the adjudicator's speaker
// — the ADR-0160 shape the live board needs (the kind route included).
func TestEarlySpeakerWaitResolvesWithinTheBudget(t *testing.T) {
	gate := newSpeakerGateWiring(map[string]string{"msg-line-a": "character-aryll"}, map[string]bool{"msg-line-a": true})
	gate.kindSpeakerByKey = map[string]string{"kind:boy": "character-boy"}
	key := earlyAskKey("bottom", 1, "msg-line-a")
	if p := gate.beginEarlyAttribution(key, DefaultEarlySpeakerBudget); p == nil {
		t.Fatalf("the early attribution wait must be registerable")
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		gate.resolveEarlyAttribution(key, attributionEarly{
			speakerID:       "char-boy",
			speakerKnown:    true,
			speakerConf:     0.9,
			speakerKind:     "boy",
			speakerKindConf: 0.9,
		})
	}()
	res, pending := gate.waitEarlyAttribution(context.Background(), key, DefaultEarlySpeakerBudget)
	if !pending {
		t.Fatalf("the wait must resolve while the answer is in flight")
	}
	if !res.speakerKnown || res.speakerID != "char-boy" {
		t.Fatalf("the resolved attribution = %+v, want the confident named speaker", res)
	}
	if res.speakerKind != "boy" || res.speakerKindConf != 0.9 {
		t.Fatalf("the resolved kind = %q/%.1f, want boy/0.9", res.speakerKind, res.speakerKindConf)
	}
	character, route := gate.commitVoice(commitPayload{
		MessageID:             "msg-line-a",
		SpeakerID:             res.speakerID,
		SpeakerKnown:          res.speakerKnown,
		SpeakerConfidence:     res.speakerConf,
		SpeakerKind:           res.speakerKind,
		SpeakerKindConfidence: res.speakerKindConf,
	})
	if character != "char-boy" || route != voiceRouteSpeaker {
		t.Fatalf("the commit's voice = %q/%q, want the kind/speaker route the answer resolved", character, route)
	}
}

// TestEarlySpeakerAskBreakerOpenSendsNothing: once the decision breaker has
// opened (the early ask failed transiently enough to trip it), the next early
// ask for another shared-slot occurrence sends no request at all — the breaker
// fail-fasts, and the commit proceeds on the local evidence.
func TestEarlySpeakerAskBreakerOpenSendsNothing(t *testing.T) {
	g := newEarlyClef(t, func(o *earlyClef) { o.fail = http.StatusServiceUnavailable })
	w := newEarlySpeakerWiring(t, g, earlySpeakerOptions(g, 300*time.Millisecond))
	first := w.Decide(context.Background(), earlySpeakerRequest("msg-line-a"))
	if first.Msg != nil || first.Reason == "" {
		t.Fatalf("the first early ask must abstain on the provider failure, got %+v", first)
	}
	hits := g.hitsCount()
	second := w.Decide(context.Background(), earlySpeakerRequest("msg-line-b"))
	if second.Msg != nil {
		t.Fatalf("the second early ask must abstain while the breaker is open, got %+v", second.Msg)
	}
	if got := g.hitsCount(); got != hits {
		t.Fatalf("the breaker was open: the second early ask sent %d new requests, want 0 (hits before = %d)", got-hits, hits)
	}
}
