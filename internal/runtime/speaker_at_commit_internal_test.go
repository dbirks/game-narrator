package runtime

// The ADR-0167 speaker-at-commit tests: a committed shared-slot line whose
// payload carries no confident speaker owes the adjudicator's speaker answer
// at commit time — asked through the decision wiring's own client, the speaker
// + speaker_kind questions only, the frame included — and the utterance waits
// at most the ADR-0167 budget (the ADR-0160 proposal's 300 ms) for that answer
// before resolving its voice, falling back to the assigned voice when the
// answer is late or never came.

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

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/store"
)

// speakerAtCommitTestModel is the model id the fake gateway answers with; the
// client validates the response's model against the role's.
const speakerAtCommitTestModel = "clef-fake"

// speakerAtCommitClef is one fake System One gateway for the speaker-at-
// commit ask: it answers the speaker + speaker_kind questions the speaker-only
// bundle renders (the scripted choices and confidences) and can hold the answer
// or fail it.
type speakerAtCommitClef struct {
	server      *httptest.Server
	mu          sync.Mutex
	hits        int
	sleep       time.Duration
	fail        int    // 0 = answer; >0 = fail every request with this status
	speaker     string // the speaker question's choice ("" = the unknown fallback)
	kind        string // the speaker_kind question's choice ("" = the unknown fallback)
	kindConf    float64
	speakerConf float64
}

func newSpeakerAtCommitClef(t *testing.T, o ...func(*speakerAtCommitClef)) *speakerAtCommitClef {
	t.Helper()
	g := &speakerAtCommitClef{speaker: "sp1", kind: "boy", kindConf: 0.9, speakerConf: 0.9}
	for _, f := range o {
		f(g)
	}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *speakerAtCommitClef) url() string { return g.server.URL }

func (g *speakerAtCommitClef) hitsCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

type speakerAtCommitClefWire struct {
	Questions map[string]struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

func (g *speakerAtCommitClef) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/systemone" {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.hits++
	hits := g.hits
	g.mu.Unlock()
	body, _ := io.ReadAll(r.Body)
	var wire speakerAtCommitClefWire
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
			probs[choice] = g.speakerConf
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
			probs[choice] = g.kindConf
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
	w.Header().Set("X-Request-Id", fmt.Sprintf("req-speaker-at-commit-%d", hits))
	out, _ := json.Marshal(map[string]any{
		"model":   speakerAtCommitTestModel,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 214, "output_tokens": 12},
	})
	_, _ = w.Write(out)
}

// newSpeakerAtCommitWiring builds the decision wiring the ADR-0167 ask runs
// through: a real store and event hub behind the Manager, the fake gateway
// behind the decision role's router entry, and the ADR-0167 options.
func newSpeakerAtCommitWiring(t *testing.T, g *speakerAtCommitClef, decisionOpts *DecisionOptions) *decisionWiring {
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
	return mgr.newDecisionWiring("speaker-at-commit", map[string]string{}, nil, []systemone.Speaker{
		{ID: "character-boy", Description: "a boy"},
	})
}

// speakerAtCommitOptions builds the DecisionOptions for the speaker-at-commit
// ask: the decision role's System One client over the fake gateway, and a
// breaker that trips after one transient failure.
func speakerAtCommitOptions(g *speakerAtCommitClef) *DecisionOptions {
	role := providers.RoleConfig{
		Role:     providers.RoleDecision,
		Protocol: providers.ProtocolSystemOne,
		BaseURL:  g.url(),
		Model:    speakerAtCommitTestModel,
		Timeout:  5 * time.Second,
	}
	reg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{providers.RoleDecision: role})
	return &DecisionOptions{
		Router: router.New(reg, router.Options{BreakerThreshold: 1, BreakerCooldown: time.Hour}),
		Role:   role,
		Margin: 1.0,
	}
}

// TestSpeakerAtCommitAskAnswersWithinBudget is the ADR-0167 happy path at the
// commit seam: a committed shared-slot line whose payload carries no confident
// speaker gets the speaker-at-commit ask, the answer lands inside the budget,
// and the line voices the confident speaker the frame named — the slot's
// assigned character must not hold the line's voice.
func TestSpeakerAtCommitAskAnswersWithinBudget(t *testing.T) {
	g := newSpeakerAtCommitClef(t, func(o *speakerAtCommitClef) { o.speaker = "sp1"; o.kind = "boy"; o.kindConf = 0.9; o.speakerConf = 0.9 })
	w := newSpeakerAtCommitWiring(t, g, speakerAtCommitOptions(g))
	e := newFenceEnv(t, 0.2, true, map[string]string{"msg-line-a": "character-aryll"})
	e.w.speakerSlotByMessage = map[string]bool{"msg-line-a": true}
	e.w.decision = w
	e.w.speakerAskBudget = 5 * time.Second // the race detector slows the fake gateway past 300 ms
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	boyVoice := seedFallbackVoice(t, e.st, a, "character-boy")
	e.commit(t, "occ-a", "msg-line-a", "bottom", 1, "the boy shouts back", true)

	e.waitEvents(t, "line A started", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechStarted && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	u := e.w.lookupUtterance("occ-a")
	if u.voiceID != boyVoice {
		t.Fatalf("the committed shared-slot line resolved to voice %s, want the confident speaker's %s", u.voiceID, boyVoice)
	}
	if st := w.status(); !st.Configured || st.Degraded || st.Calls != 1 || st.Answered != 1 {
		t.Fatalf("the speaker-at-commit ask did not land: status = %+v", st)
	}
}

// TestSpeakerAtCommitKindOnlyAnswerRoutesKindVoice: the ADR-0167 ask's answer
// that names only a confident apparent kind voices the kind character — the
// generic kind voice the roster carries — instead of the assigned character.
func TestSpeakerAtCommitKindOnlyAnswerRoutesKindVoice(t *testing.T) {
	g := newSpeakerAtCommitClef(t, func(o *speakerAtCommitClef) {
		o.speaker = "unknown"
		o.kind = "boy"
		o.kindConf = 0.9
		o.speakerConf = 0.9
	})
	w := newSpeakerAtCommitWiring(t, g, speakerAtCommitOptions(g))
	e := newFenceEnv(t, 0.2, true, map[string]string{})
	e.w.speakerSlotByMessage = map[string]bool{"msg-line-a": true}
	e.w.kindSpeakerByKey = map[string]string{"kind:boy": "character-boy"}
	e.w.decision = w
	e.w.speakerAskBudget = 5 * time.Second // the race detector slows the fake gateway past 300 ms
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	boyVoice := seedFallbackVoice(t, e.st, a, "character-boy")
	e.commit(t, "occ-a", "msg-line-a", "bottom", 1, "the boy shouts back", true)

	e.waitEvents(t, "line A started", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechStarted && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	u := e.w.lookupUtterance("occ-a")
	if u.voiceID != boyVoice {
		t.Fatalf("the committed shared-slot line resolved to voice %s, want the confident kind's %s", u.voiceID, boyVoice)
	}
}

// TestSpeakerAtCommitLateAnswerFallsBackToAssignedAndDoesNotStall: the ADR-0167
// ask's answer arriving after the ADR-0167 budget resolves the wait with no
// attribution, so the line speaks the slot's assigned voice — and the wait did
// not stall the utterance past the budget.
func TestSpeakerAtCommitLateAnswerFallsBackToAssignedAndDoesNotStall(t *testing.T) {
	g := newSpeakerAtCommitClef(t, func(o *speakerAtCommitClef) { o.speaker = "sp1"; o.kind = "boy"; o.kindConf = 0.9; o.speakerConf = 0.9 })
	g.sleep = 2 * time.Second
	w := newSpeakerAtCommitWiring(t, g, speakerAtCommitOptions(g))
	e := newFenceEnv(t, 0.2, true, map[string]string{"msg-line-a": "character-aryll"})
	e.w.speakerSlotByMessage = map[string]bool{"msg-line-a": true}
	e.w.decision = w
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	aryllVoice := seedFallbackVoice(t, e.st, a, "character-aryll")
	e.commit(t, "occ-a", "msg-line-a", "bottom", 1, "the boy shouts back", true)

	e.waitEvents(t, "line A ended", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	if u := e.w.lookupUtterance("occ-a"); u.voiceID != aryllVoice {
		t.Fatalf("the late answer fell back to the assigned voice: resolved voice = %s, want %s", u.voiceID, aryllVoice)
	}
}

// TestSpeakerAtCommitNoAskForNonSharedLine: a committed line whose speaker
// slot is not shared owes no speaker-at-commit ask — the decision role's
// gateway sees nothing.
func TestSpeakerAtCommitNoAskForNonSharedLine(t *testing.T) {
	g := newSpeakerAtCommitClef(t, func(o *speakerAtCommitClef) { o.speaker = "sp1"; o.kind = "boy"; o.kindConf = 0.9; o.speakerConf = 0.9 })
	w := newSpeakerAtCommitWiring(t, g, speakerAtCommitOptions(g))
	e := newFenceEnv(t, 0.2, true, map[string]string{"msg-line-a": "character-aryll"})
	e.w.decision = w
	// No shared slot for this message: the slot→character link is the
	// identity here, and no ask is owed.
	e.commit(t, "occ-a", "msg-line-a", "bottom", 1, "the boy shouts back", true)
	e.waitEvents(t, "line A ended", 10*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechEnded && genOfEv(t, ev) == 1 {
				return true
			}
		}
		return false
	})
	if got := g.hitsCount(); got != 0 {
		t.Fatalf("a non-shared line must not owe a speaker-at-commit ask: %d gateway hits, want 0", got)
	}
}

// TestSpeakerAtCommitAskCap: the global cap of speaker-at-commit asks per
// minute holds — the asks beyond the cap are refused, so the commit proceeds on
// the local evidence alone instead of spending the decision budget's worth of
// round trips.
func TestSpeakerAtCommitAskCap(t *testing.T) {
	e := newFenceEnv(t, 0.2, true, map[string]string{"msg-line-a": "character-aryll"})
	e.w.speakerSlotByMessage = map[string]bool{"msg-line-a": true}
	for i := 0; i <= maxSpeakerAsksPerMinute; i++ {
		if p := e.w.beginAsk(fmt.Sprintf("bottom\x001\x00msg-line-%d", i), DefaultSpeakerAskBudget); p == nil {
			if i != maxSpeakerAsksPerMinute {
				t.Fatalf("ask %d was refused before the cap", i)
			}
			break
		}
	}
	if p := e.w.beginAsk("bottom\x001\x00msg-line-after", DefaultSpeakerAskBudget); p != nil {
		t.Fatalf("the %dth ask in a minute must be refused (the cap)", maxSpeakerAsksPerMinute+1)
	}
}
