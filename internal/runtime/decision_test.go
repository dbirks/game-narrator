package runtime_test

// Runtime coverage for the Clef (System One) adjudication wiring
// (docs/spec/05 "candidate retrieval → graph-aware scoring → optional Clef/VLM
// adjudication → commitment policy", ADR-0050). The pipeline composes the
// decision step the way the scenario harness does: a locally ambiguous
// ranking holds its commit for one adjudication through the decision role's
// router entry, the answer is applied to the session's reducer before the
// commitment policy reads it, and every failure is an honest abstain — the
// commit proceeds on the local evidence and the session reports degraded.
//
// The provider is always an httptest fake speaking the System One wire
// protocol (the scenario harness's "fake decision gateway", never a real
// model — docs/spec/16's synthetic-stub category). The live end-to-end
// proof of a real Clef answering a real ambiguous frame is TestLiveDecision.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/runtime"
	ses "github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// decisionTestModel is the model id the fake advertises and the role
// configures; the client validates the response's model against it, so a
// mismatch is a wiring failure, not a sampling one.
const decisionTestModel = "clef-fake"

// fakeClef is one fake System One gateway: it speaks the bundled adjudication
// protocol (six questions, the response validator's rules included: choice
// answers carry the declared option ids with probabilities summing to 1 and
// a confidence that matches the chosen option), and it records the requests
// it saw, the concurrency it saw, and how the request contexts ended — the
// three things the assertions need that the session's events cannot show
// (that the call really went over the wire, that at most one was in flight
// per session, and that the deadline the client carried was the role's).
type fakeClef struct {
	server *httptest.Server

	mu          sync.Mutex
	hits        int
	inFlight    int
	maxConcur   int
	reqIDs      []string        // the X-Request-Id the gateway assigned, in order
	deadlineObs []time.Duration // request contexts cancelled before the scripted sleep finished
	ocrTexts    []string        // the state.ocr of each request (the text adjudicated)
	images      [][]string      // the images of each request (ADR-0154: the screenshot)

	pick       string // the candidate option to answer ("" = the protocol's c2-or-first)
	failStatus int    // 0 = answer; >0 = fail every request with this status
	sleep      time.Duration
}

func newFakeClef(t *testing.T, opts ...func(*fakeClef)) *fakeClef {
	t.Helper()
	g := &fakeClef{pick: "c2"}
	for _, o := range opts {
		o(g)
	}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeClef) url() string { return g.server.URL }

// clefPick overrides the candidate option the gateway answers (the default
// "c2" — the runner-up — is the narrow case: the model refuses the local
// winner and the commit must follow the model to a candidate retrieval
// actually offered).
func clefPick(opt string) func(*fakeClef) { return func(g *fakeClef) { g.pick = opt } }

// clefFail makes every request fail with the given HTTP status (the
// provider-down shape: 503 maps to the taxonomy's unavailable).
func clefFail(status int) func(*fakeClef) { return func(g *fakeClef) { g.failStatus = status } }

// clefSleep delays the answer by d before responding, bounded by the
// request's context: a client that never carries the role's deadline will
// sit here until its own sleep, which is exactly what the deadline test
// must not do.
func clefSleep(d time.Duration) func(*fakeClef) { return func(g *fakeClef) { g.sleep = d } }

type clefWireRequest struct {
	Model  string   `json:"model"`
	Images []string `json:"images"`
	State  struct {
		OCR string `json:"ocr"`
	} `json:"state"`
	Questions map[string]struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

func (g *fakeClef) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/systemone" {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	g.mu.Lock()
	g.hits++
	g.inFlight++
	if g.inFlight > g.maxConcur {
		g.maxConcur = g.inFlight
	}
	start := time.Now()
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.inFlight--
		g.mu.Unlock()
	}()

	body, _ := io.ReadAll(r.Body)
	if r.Context().Err() != nil {
		// The client dropped the request while its body (the frame JPEG,
		// ADR-0154) was still arriving — on a slow runner the role's
		// deadline can fire before the handler reaches the hold below. That
		// is the same evidence: record how long the context lived.
		g.mu.Lock()
		g.deadlineObs = append(g.deadlineObs, time.Since(start))
		g.mu.Unlock()
		return
	}
	var req clefWireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.ocrTexts = append(g.ocrTexts, req.State.OCR)
	g.images = append(g.images, req.Images)
	g.mu.Unlock()

	if g.failStatus > 0 {
		w.Header().Set("X-Request-Id", "req-fake-fail")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.failStatus)
		fmt.Fprint(w, `{"detail":"synthetic outage"}`)
		return
	}

	if g.sleep > 0 {
		select {
		case <-time.After(g.sleep):
		case <-r.Context().Done():
			// The client dropped the request before the answer was ready:
			// record how long its context lived. That is the deadline the
			// call actually carried — the assertion the deadline test makes.
			g.mu.Lock()
			g.deadlineObs = append(g.deadlineObs, time.Since(start))
			g.mu.Unlock()
			return
		}
	}

	answers, ok := g.renderAnswers(req)
	if !ok {
		http.Error(w, "unexpected question set", http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	id := fmt.Sprintf("req-fake-%d", g.hits)
	g.reqIDs = append(g.reqIDs, id)
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", id)
	out, _ := json.Marshal(map[string]any{
		"model":   decisionTestModel,
		"answers": answers,
		"usage":   map[string]int{"input_tokens": 214, "output_tokens": 12},
	})
	_, _ = w.Write(out)
}

// renderAnswers answers every declared question of the request the way a
// conforming model does: choice answers pick a declared option (the scripted
// candidate pick for the candidate question, "unknown" for the speaker,
// "dialogue" for the text kind) with probabilities over exactly the declared
// options summing to 1 and a confidence equal to the chosen option's
// probability; noul answers carry a 0..1 score.
func (g *fakeClef) renderAnswers(req clefWireRequest) (map[string]any, bool) {
	answers := make(map[string]any, len(req.Questions))
	for id, q := range req.Questions {
		switch q.Type {
		case "choice":
			if len(q.Criteria) == 0 {
				return nil, false
			}
			choice := ""
			switch id {
			case "candidate_message":
				choice = g.pick
				if choice == "" {
					choice = "c2"
				}
				if _, ok := q.Criteria[choice]; !ok {
					if _, ok := q.Criteria["c1"]; ok {
						choice = "c1"
					}
				}
				if _, ok := q.Criteria[choice]; !ok {
					choice = firstKey(q.Criteria)
				}
			case "speaker":
				choice = "unknown"
			case "text_kind":
				choice = "dialogue"
			default:
				choice = firstKey(q.Criteria)
			}
			others := len(q.Criteria) - 1
			probs := make(map[string]float64, len(q.Criteria))
			for opt := range q.Criteria {
				if opt == choice {
					probs[opt] = 0.9
				} else if others > 0 {
					probs[opt] = 0.1 / float64(others)
				} else {
					probs[opt] = 1.0
				}
			}
			conf := probs[choice]
			answers[id] = map[string]any{
				"type":          "choice",
				"choice":        choice,
				"confidence":    conf,
				"probabilities": probs,
			}
		case "noul":
			answers[id] = map[string]any{"type": "noul", "noul": 0.9}
		default:
			return nil, false
		}
	}
	return answers, true
}

func firstKey(m map[string]string) string {
	for k := range m {
		return k
	}
	return ""
}

// newDecisionEnv builds the decision session's environment: the shared env
// (real store, real hub, the scripted OCR) plus the Clef wiring the production
// main builds over the decision role's router entry — one router over a
// registry whose decision role points at the fake, the same DecisionOptions
// shape cmd/game-narrator wires (ADR-0050). Margin 1.0 is the test's lever on
// the policy: with the adjudication on at a threshold no accepted ranking can
// clear, every locally-accepted line is offered to Clef, so the test proves
// the ask-apply-commit path on every line instead of hunting for the corpus's
// one ambiguous read.
func newDecisionEnv(t *testing.T, g *fakeClef, envOpts ...func(*env)) *env {
	t.Helper()
	return newDecisionEnvWithTimeout(t, g, 2*time.Second, envOpts...)
}

// newDecisionEnvWithTimeout is newDecisionEnv with the role's timeout set: the
// one knob the deadline test owns (the role's timeout is the adjudication's
// deadline, docs/spec/06 "Timeout and cancellation defaults").
func newDecisionEnvWithTimeout(t *testing.T, g *fakeClef, roleTimeout time.Duration, envOpts ...func(*env)) *env {
	t.Helper()
	role := providers.RoleConfig{
		Role:     providers.RoleDecision,
		Protocol: providers.ProtocolSystemOne,
		BaseURL:  g.url(),
		Model:    decisionTestModel,
		Timeout:  roleTimeout,
	}
	reg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{providers.RoleDecision: role})
	rt := router.New(reg, router.Options{})
	opts := &runtime.DecisionOptions{Router: rt, Role: role, Margin: 1.0}
	// withDecision goes first: the env applies options in order and the
	// decision wiring must exist before the manager is built with it.
	all := append([]func(*env){withDecision(opts)}, envOpts...)
	return newEnv(t, all...)
}

// decisionStats reads the decision counters off the session's terminal Stats.
func decisionStats(t *testing.T, e *env, id string) (decisions, applied, fenced uint64) {
	t.Helper()
	info, ok := e.mgr.Get(id)
	if !ok {
		t.Fatalf("session %s vanished", id)
	}
	return info.Stats.Decisions, info.Stats.DecisionApplied, info.Stats.DecisionFenced
}

// waitDecisionEvents polls the session's event log until pred holds or the
// deadline passes, and returns the log it matched on (the same poll shape the
// speech tests use, over env instead of voiceEnv).
func (e *env) waitDecisionEvents(t *testing.T, id, desc string, deadline time.Duration, pred func([]events.Envelope) bool) []events.Envelope {
	t.Helper()
	by := time.Now().Add(deadline)
	for {
		evs := eventsFrom(t, e, id)
		if pred(evs) {
			return evs
		}
		if time.Now().After(by) {
			t.Fatalf("%s: condition not met within %s (%d events so far)", desc, deadline, len(evs))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// commitPayload decodes the message.committed payload the assertions need.
type commitPayload struct {
	MessageID  string `json:"message_id"`
	Reason     string `json:"reason"`
	RegionID   string `json:"region_id"`
	ObsSeq     int64  `json:"observation_seq"`
	Generation int64  `json:"generation"`
	SpeechOK   bool   `json:"speech_eligible"`
}

func commitOf(t *testing.T, evs []events.Envelope, n int) commitPayload {
	t.Helper()
	var p commitPayload
	seen := 0
	for _, ev := range evs {
		if ev.Name != events.MessageCommitted {
			continue
		}
		seen++
		if seen == n {
			if err := json.Unmarshal(ev.Data, &p); err != nil {
				t.Fatalf("message.committed payload: %v", err)
			}
			return p
		}
	}
	t.Fatalf("wanted the %dnd message.committed, the log has %d", n, seen)
	return p
}

// resolvedOf returns the decision.resolved event for one observation seq.
func resolvedOf(t *testing.T, evs []events.Envelope, obsSeq int64) (events.Envelope, map[string]any) {
	t.Helper()
	for _, ev := range evs {
		if ev.Name != events.DecisionResolved {
			continue
		}
		m := payload(t, ev)
		if v, _ := m["observation_seq"].(float64); int64(v) == obsSeq {
			return ev, m
		}
	}
	t.Fatalf("no decision.resolved for observation_seq %d", obsSeq)
	return events.Envelope{}, nil
}

// TestDecisionAdjudicatesAmbiguousCommit is the happy path at the session
// level: a replay clip whose two lines the local matcher accepts (no
// abstention) is run with the adjudication on at a margin no accepted
// ranking can clear (AdjudicateMargin 1.0 — the policy asks Clef for every
// line it would otherwise commit). The fake answers the runner-up ("c2"):
// the commits must follow the model, not the local winner, and every step
// of the chain must carry the ids that join it.
//
// The push-wait-push protocol makes the ordering deterministic (docs/spec/12):
// the line-a frames land, the test waits on decision.requested (the
// adjudication is provably in flight), more line-a frames are pushed while it
// is out (a stable box keeps advancing its observation seq — the reply-time
// stamp the scenario harness's comment calls a coin toss without it), and
// the commit is waited on only after.
func TestDecisionAdjudicatesAmbiguousCommit(t *testing.T) {
	corpusPresent(t)
	g := newFakeClef(t, clefSleep(600*time.Millisecond))
	src := newManualSource(64)
	e := newDecisionEnv(t, g, withFakeOCR(truthScript(t), 0), withManualSource(func() (capture.Source, error) {
		return src, nil
	}))

	profileID, msgMap := seedSyntheticProfile(t, e.st, true)
	_ = msgMap
	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := info.ID
	defer waitState(t, e.mgr, sid, 60*time.Second)
	defer func() { _ = src.Close() }()

	// Line a: the box appears, types out and stabilises (clip frames 4..15):
	// the full line is first observed at 1.3 s (frame 13) and the box leaves
	// REVEALING for READY only once the tracker's 250 ms stable window has
	// held (frame 15, on the source clock — machine speed is irrelevant).
	// Conservative mode ranks only a READY box, so this is the earliest the
	// first adjudication can be asked.
	for n := 4; n <= 15; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	// The first adjudication is in flight (the fake holds its answer 600 ms);
	// wait on its decision.requested so the in-flight window the frames below
	// land in is observed, not assumed.
	e.waitDecisionEvents(t, sid, "first decision.requested", 60*time.Second, func(evs []events.Envelope) bool {
		return hasName(evs, events.DecisionRequested)
	})

	// While the answer is out, the box's dwell frames keep flowing (16..18):
	// the source coalesces them in its latest slot while the rank worker is
	// blocked on the adjudication, and the reply's reply-time observation
	// stamp keeps the fences honest about the reading the answer applies to.
	for n := 16; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}

	// Line a's commit lands under the applied answer; its decision.resolved
	// is published on the publisher chain just after the commit, so the wait
	// demands both — a snapshot read between the two would dangle the join.
	evs := e.waitDecisionEvents(t, sid, "line a committed", 60*time.Second, func(evs []events.Envelope) bool {
		return hasName(evs, events.MessageCommitted) && countName(evs, events.DecisionResolved) >= 1
	})
	c1 := commitOf(t, evs, 1)

	// Line b: the page advance (clip frames 19..22).
	for n := 19; n <= 22; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	// Same ordering for line b: both commits and both resolved events, so the
	// event log the assertions read is not a snapshot taken between a commit
	// and the resolved event that follows it.
	evs = e.waitDecisionEvents(t, sid, "line b committed", 60*time.Second, func(evs []events.Envelope) bool {
		return countName(evs, events.MessageCommitted) >= 2 && countName(evs, events.DecisionResolved) >= 2
	})
	c2 := commitOf(t, evs, 2)

	// The advance: the box dismisses (clip frames 23..24), then EOF.
	for n := 23; n <= 24; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	_ = src.Close()
	final := waitState(t, e.mgr, sid, 60*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("state = %q (reason %s), want stopped", final.State, final.Reason)
	}

	// --- the commits followed the model, not the local winner. ---
	for i, c := range []commitPayload{c1, c2} {
		if c.Reason != "CLEF_DISAMBIGUATED" {
			t.Fatalf("commit %d reason = %q, want CLEF_DISAMBIGUATED (the model's pick drove the commit): %dnd commit", i+1, c.Reason, i+1)
		}
		if c.Generation == 0 || c2.Generation <= c1.Generation {
			t.Fatalf("commit generations = %d, %d: the second commit must name a newer generation", c1.Generation, c2.Generation)
		}
		// The model's choice (c2, the runner-up) maps back to the message the
		// retrieval offered it: the candidates event for the same observation
		// lists it, and it is not the local winner (the first candidate).
		cands := candidatesOf(t, evs, c.ObsSeq)
		if len(cands) < 2 {
			t.Fatalf("observation %d carried %d candidates, want ≥2 (the narrow case needs a runner-up)", c.ObsSeq, len(cands))
		}
		if c.MessageID == cands[0] {
			t.Fatalf("commit %d kept the local winner %s despite the model picking the runner-up %s", i+1, c.MessageID, cands[1])
		}
		if c.MessageID != cands[1] {
			t.Fatalf("commit %d message %s, want the model's pick (candidate 2: %s)", i+1, c.MessageID, cands[1])
		}
		if !c.SpeechOK {
			t.Fatalf("commit %d is not speech-eligible although the model said safe_to_speak", i+1)
		}
	}

	// --- every call is traced: requested and resolved agree on the ids the
	// developer UI joins on, and the request id is the gateway's own. ---
	for _, c := range []commitPayload{c1, c2} {
		resEv, res := resolvedOf(t, evs, c.ObsSeq)
		if res["provider"] != "systemone" || res["model"] != decisionTestModel {
			t.Fatalf("decision.resolved provider/model = %v/%v, want systemone/%s", res["provider"], res["model"], decisionTestModel)
		}
		if res["applied"] != true {
			t.Fatalf("decision.resolved applied = %v for observation %d, want true (the answer drove the commit)", res["applied"], c.ObsSeq)
		}
		if res["choice"] != "c2" {
			t.Fatalf("decision.resolved choice = %v, want c2 (the scripted pick)", res["choice"])
		}
		if mid, _ := res["message_id"].(string); mid != c.MessageID {
			t.Fatalf("decision.resolved message_id = %q, want the commit's %q", mid, c.MessageID)
		}
		rid, _ := res["request_id"].(string)
		if rid == "" || !strings.HasPrefix(rid, "req-fake-") {
			t.Fatalf("decision.resolved request_id = %q, want the gateway's own id", rid)
		}
		lat, _ := res["latency_ms"].(float64)
		if lat < 500 {
			t.Fatalf("decision.resolved latency_ms = %v, want ≥500 (the scripted 600 ms hold)", lat)
		}
		// The candidates event the decision answers: the same observation,
		// with the commit causally attached to it — the join docs/spec/13's
		// timeline is built on. The candidates event carries the ask-time
		// generation (the commit lands on the next one: the increment is the
		// commit), so the join is observation seq plus the commit's causation
		// id, never the generation.
		if ok, why := candidatesChainOK(evs, c); !ok {
			t.Fatalf("%s (observation %d, generation %d): the causal chain dangles", why, c.ObsSeq, c.Generation)
		}
		// The request the gateway saw carried the line's text (frame → OCR →
		// candidates → Clef request: the OCR's reading is what was adjudicated).
		if !g.sawTextFor(t, c.ObsSeq, evs) {
			t.Fatalf("the gateway's request for observation %d did not carry the box's text", c.ObsSeq)
		}
		_ = resEv
	}

	// --- bounded concurrency: the two adjudications ran, never overlapped. ---
	if got := g.maxConcurrent(); got != 1 {
		t.Fatalf("gateway max concurrency = %d, want 1 (at most one adjudication in flight per session)", got)
	}
	if got := g.hitsCount(); got < 2 {
		t.Fatalf("gateway hits = %d, want ≥2 (both lines were adjudicated)", got)
	}
	// --- the screenshot (ADR-0154): every adjudication carried the frame. ---
	g.mu.Lock()
	for i, imgs := range g.images {
		if len(imgs) != 1 || !strings.HasPrefix(imgs[0], "data:image/jpeg;base64,") {
			g.mu.Unlock()
			t.Fatalf("request %d carried %d images (%.40q…), want the frame as one JPEG", i, len(imgs), imgs)
		}
	}
	g.mu.Unlock()

	// --- the counters and the session's decision view. ---
	decisions, applied, fenced := decisionStats(t, e, sid)
	if decisions < 2 || applied < 2 || fenced != 0 {
		t.Fatalf("stats decisions/applied/fenced = %d/%d/%d, want ≥2/≥2/0", decisions, applied, fenced)
	}
	if d := final.Decision; d == nil || !d.Configured || d.Degraded || d.Calls < 2 || d.Answered < 2 {
		t.Fatalf("session decision view = %+v, want configured, not degraded, ≥2 calls/answered", d)
	}
}

func hasName(evs []events.Envelope, name string) bool {
	for _, ev := range evs {
		if ev.Name == name {
			return true
		}
	}
	return false
}

func countName(evs []events.Envelope, name string) int {
	n := 0
	for _, ev := range evs {
		if ev.Name == name {
			n++
		}
	}
	return n
}

// candidatesOf decodes the message.candidates event for one observation seq
// and returns the candidate message ids in rank order.
func candidatesOf(t *testing.T, evs []events.Envelope, obsSeq int64) []string {
	t.Helper()
	var cands []string
	found := false
	for _, ev := range evs {
		if ev.Name != events.MessageCandidates {
			continue
		}
		var p struct {
			ObservationSeq int64 `json:"observation_seq"`
			Candidates     []struct {
				MessageID string `json:"message_id"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil {
			t.Fatalf("message.candidates payload: %v", err)
		}
		if p.ObservationSeq != obsSeq {
			continue
		}
		found = true
		for _, c := range p.Candidates {
			cands = append(cands, c.MessageID)
		}
	}
	if !found {
		t.Fatalf("no message.candidates for observation %d", obsSeq)
	}
	return cands
}

// hasCandidatesFor reports a message.candidates event for the exact
// observation seq and generation (the causal pair the commit's causation_id
// references).
// candidatesChainOK reports the causal chain the developer UI's timeline is
// built on: a message.candidates for the commit's observation, with the
// commit's causation id pointing at exactly that event. (The candidates'
// generation is the ask-time generation and the commit's is the post-commit
// one, so the join key is observation seq plus causation, not generation.)
func candidatesChainOK(evs []events.Envelope, c commitPayload) (bool, string) {
	var cand events.Envelope
	for _, ev := range evs {
		if ev.Name != events.MessageCandidates {
			continue
		}
		var p struct {
			ObservationSeq int64 `json:"observation_seq"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil || p.ObservationSeq != c.ObsSeq {
			continue
		}
		cand = ev
	}
	if cand.EventID == "" {
		return false, "no message.candidates for the commit's observation"
	}
	for _, ev := range evs {
		if ev.Name != events.MessageCommitted || ev.CausationID != cand.EventID {
			continue
		}
		var cp commitPayload
		if err := json.Unmarshal(ev.Data, &cp); err != nil {
			continue
		}
		if cp.ObsSeq == c.ObsSeq && cp.Generation == c.Generation {
			return true, ""
		}
	}
	return false, "the commit is not causally attached to its candidates event"
}

// sawTextFor reports the gateway saw a request whose state.ocr is the text
// the candidates event records for the observation: the chain from frame to
// Clef request is the OCR's reading, not a copy of the profile.
func (g *fakeClef) sawTextFor(t *testing.T, obsSeq int64, evs []events.Envelope) bool {
	t.Helper()
	for _, ev := range evs {
		if ev.Name != events.MessageCandidates {
			continue
		}
		var p struct {
			ObservationSeq int64  `json:"observation_seq"`
			Text           string `json:"text"`
		}
		if err := json.Unmarshal(ev.Data, &p); err != nil || p.ObservationSeq != obsSeq {
			continue
		}
		for _, got := range g.ocrTextList() {
			if got == p.Text && p.Text != "" {
				return true
			}
		}
	}
	return false
}

func (g *fakeClef) hitsCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

func (g *fakeClef) maxConcurrent() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxConcur
}

func (g *fakeClef) ocrTextList() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.ocrTexts...)
}

// TestDecisionProviderDownRunsLocalAndDegrades is docs/spec/13's "Decision
// down" shape at the session level: the decision role's endpoint is down
// (every request a 503), and the session must commit on the local evidence —
// both lines, the matcher's own verdicts — report the role degraded on the
// session status, and stop hitting the endpoint the breaker opens on (the
// gateway sees exactly the one call's retry budget, not one per line).
func TestDecisionProviderDownRunsLocalAndDegrades(t *testing.T) {
	corpusPresent(t)
	g := newFakeClef(t, clefFail(http.StatusServiceUnavailable))
	src := newManualSource(64)
	e := newDecisionEnv(t, g, withFakeOCR(truthScript(t), 0), withManualSource(func() (capture.Source, error) {
		return src, nil
	}))

	profileID, msgMap := seedSyntheticProfile(t, e.st, true)
	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := info.ID
	defer waitState(t, e.mgr, sid, 60*time.Second)
	defer func() { _ = src.Close() }()

	for n := 4; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs := e.waitDecisionEvents(t, sid, "line a committed", 60*time.Second, func(evs []events.Envelope) bool {
		return hasName(evs, events.MessageCommitted)
	})
	c1 := commitOf(t, evs, 1)
	// The local verdict stands: the exact read commits on its own evidence.
	if c1.MessageID != msgMap["demo-page-1"] {
		t.Fatalf("commit 1 message = %s, want the local exact match %s (the model never answered)", c1.MessageID, msgMap["demo-page-1"])
	}
	if c1.Reason == "CLEF_DISAMBIGUATED" {
		t.Fatalf("commit 1 reason = CLEF_DISAMBIGUATED although the provider never answered")
	}

	for n := 19; n <= 24; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	// The second resolved event is published after line b's commit, so the
	// wait demands both: the assertion below reads the log once.
	evs = e.waitDecisionEvents(t, sid, "line b committed", 60*time.Second, func(evs []events.Envelope) bool {
		return countName(evs, events.MessageCommitted) >= 2 && countName(evs, events.DecisionResolved) >= 2
	})
	c2 := commitOf(t, evs, 2)
	if c2.MessageID != msgMap["demo-page-2"] {
		t.Fatalf("commit 2 message = %s, want the local exact match %s", c2.MessageID, msgMap["demo-page-2"])
	}

	_ = src.Close()
	final := waitState(t, e.mgr, sid, 60*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("state = %q (reason %s), want stopped", final.State, final.Reason)
	}

	// Both lines asked (the policy asked; the role could not answer), neither
	// answer applied, none fenced (nothing was delivered to fence).
	decisions, applied, fenced := decisionStats(t, e, sid)
	if decisions < 2 || applied != 0 || fenced != 0 {
		t.Fatalf("stats decisions/applied/fenced = %d/%d/%d, want ≥2/0/0", decisions, applied, fenced)
	}

	// The degraded state is on the session view, and the event log says so
	// once (the state transition, not one event per failed retry).
	if d := final.Decision; d == nil || !d.Configured || !d.Degraded {
		t.Fatalf("session decision view = %+v, want configured and degraded", d)
	}
	if d := final.Decision; d.Reason != "unavailable" {
		t.Fatalf("degraded reason = %q, want unavailable (the 503's taxonomy class)", d.Reason)
	}
	var degradedEvents int
	for _, ev := range evs {
		if ev.Name != events.ProviderDegraded {
			continue
		}
		m := payload(t, ev)
		if m["role"] == "decision" && m["degraded"] == true {
			degradedEvents++
		}
	}
	if degradedEvents < 1 {
		t.Fatal("no provider.degraded(role=decision) event: the down role is invisible to the developer UI")
	}
	// The breaker: the second call failed fast without another endpoint hit.
	// The router's budget is three attempts for the first call; one more hit
	// would mean the open circuit let a request through.
	if hits := g.hitsCount(); hits != 3 {
		t.Fatalf("gateway hits = %d, want 3 (the first call's retry budget; the breaker opened before line b's call)", hits)
	}
	// The failed calls are traced too: resolved events name the failure class.
	if n := countName(evs, events.DecisionResolved); n < 2 {
		t.Fatalf("decision.resolved events = %d, want ≥2 (every call is traced, failures included)", n)
	}
	if !resolvedReasonContains(t, evs, "provider_error") {
		t.Fatal("no decision.resolved names the provider error: the local commits are unexplained")
	}
}

func resolvedReasonContains(t *testing.T, evs []events.Envelope, sub string) bool {
	t.Helper()
	for _, ev := range evs {
		if ev.Name != events.DecisionResolved {
			continue
		}
		if m := payload(t, ev); strings.Contains(fmt.Sprint(m["reason"]), sub) {
			return true
		}
	}
	return false
}

// TestDecisionDeadlineIsTheRoleTimeout is docs/spec/06's "Timeout and
// cancellation defaults" at the session level: the role's timeout is the
// adjudication's deadline. The fake holds every answer for two seconds; the
// role's timeout is 300 ms. The gateway must see its request context die
// near the deadline (not at the answer), and the session must commit on the
// local evidence with the role reported degraded.
func TestDecisionDeadlineIsTheRoleTimeout(t *testing.T) {
	corpusPresent(t)
	g := newFakeClef(t, clefSleep(2*time.Second))
	src := newManualSource(64)
	// The role's timeout is the whole point: 300 ms, far under the fake's
	// 2 s hold. The DecisionOptions.Timeout stays zero: the role's own
	// timeout is what bounds the call (the documented default).
	e := newDecisionEnvWithTimeout(t, g, 300*time.Millisecond,
		withFakeOCR(truthScript(t), 0), withManualSource(func() (capture.Source, error) {
			return src, nil
		}))

	profileID, _ := seedSyntheticProfile(t, e.st, true)
	info, err := e.mgr.Start(e.ctx, runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	sid := info.ID
	defer waitState(t, e.mgr, sid, 60*time.Second)
	defer func() { _ = src.Close() }()

	for n := 4; n <= 18; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	evs := e.waitDecisionEvents(t, sid, "line a committed", 60*time.Second, func(evs []events.Envelope) bool {
		return hasName(evs, events.MessageCommitted)
	})
	c1 := commitOf(t, evs, 1)
	if c1.Reason == "CLEF_DISAMBIGUATED" {
		t.Fatal("the commit is CLEF_DISAMBIGUATED although every adjudication timed out")
	}

	_ = src.Close()
	final := waitState(t, e.mgr, sid, 60*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("state = %q (reason %s), want stopped", final.State, final.Reason)
	}

	// The deadline: the gateway's request context died inside the role's
	// budget, not at the fake's 2 s answer. A client that carried no
	// deadline (or the fake's own sleep) would have observed ~2 s.
	obs := g.deadlines()
	if len(obs) == 0 {
		t.Fatal("the gateway never saw a cancelled request: the call did not carry the role's deadline")
	}
	for i, d := range obs {
		if d > time.Second {
			t.Fatalf("request %d lived %v before the client dropped it, want ≤~1 s (the role's 300 ms budget, not the fake's 2 s hold)", i+1, d)
		}
	}
	// And it is explained on the session, not just felt: degraded, timeout.
	if d := final.Decision; d == nil || !d.Degraded || !strings.Contains(d.Reason, "timeout") {
		t.Fatalf("session decision view = %+v, want degraded with a timeout reason", d)
	}
}

func (g *fakeClef) deadlines() []time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]time.Duration(nil), g.deadlineObs...)
}

// TestDecisionLateReplyFencedBySession is the fence the pipeline leans on
// (docs/spec/01 invariant 2, the session's four fences): a decision reply
// that names a generation the session has passed is discarded as
// stale_generation, and a redelivered one (the same seq) as duplicate_seq.
// The live pipeline stamps its replies the way this test's messages are
// stamped — ask-time generation, reply-time observation seq — so what the
// session does to these messages is what it does to the pipeline's.
func TestDecisionLateReplyFencedBySession(t *testing.T) {
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	t.Cleanup(func() { _ = hub.Close() })

	red, err := ses.New("dec-fence", "1", hub,
		ses.WithConfig(ses.DefaultConfig()),
		ses.WithTrackerConfig(recognition.DefaultConfig()))
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ctx := context.Background()
	text := "The harbour bell rang twice before anyone looked up."

	// Set the box to READY with the line's text: enough stable observations
	// for the text machine's vote window (recognition.DefaultConfig's
	// VoteWindow is 3; ten leave no room for a slow test host).
	for i := 1; i <= 10; i++ {
		res, err := red.Apply(ctx, ses.ObservationMsg{
			Header: ses.Header{
				SessionID:       "dec-fence",
				Seq:             int64(i),
				Generation:      0,
				ProfileRevision: "1",
			},
			RegionID: "box",
			Observation: recognition.Observation{
				FrameSeq:           int64(i),
				CaptureTimestampNS: int64(i) * 100_000_000,
				SourceClock:        "replay",
				RegionID:           "box",
				RegionPresent:      true,
				RegionHash:         fmt.Sprintf("h-%d", i),
				OCR:                &ocr.Result{Text: text, MeanConf: 0.95, Engine: "truth"},
			},
		})
		if err != nil {
			t.Fatalf("observation %d: %v", i, err)
		}
		if res.Reason != "applied" {
			t.Fatalf("observation %d: reason %q, want applied", i, res.Reason)
		}
	}
	st := red.State()
	if len(st.Regions) != 1 || st.Regions[0].TextState != recognition.StateReady {
		t.Fatalf("box state = %+v, want READY before the commit", st.Regions)
	}

	// The commit: the ranking the pipeline would have applied — generation 0
	// (ask-time), the box's current observation seq (reply-time), and the
	// box's own accepted text (the stale_text fence re-checks it verbatim).
	obsNow := st.Regions[0].ObservationSeq
	boxText := st.Regions[0].Text
	res, err := red.Apply(ctx, ses.RankingMsg{
		Header: ses.Header{
			SessionID:       "dec-fence",
			Seq:             11,
			Generation:      0,
			ProfileRevision: "1",
		},
		RegionID:       "box",
		ObservationSeq: obsNow,
		Text:           boxText,
		Ranking: matcher.Ranking{
			Candidates: []matcher.Candidate{{MessageID: "m-a", Score: 1, Probability: 0.9}},
			Margin:     0.9,
			Reason:     matcher.ReasonExact,
		},
	})
	if err != nil {
		t.Fatalf("ranking: %v", err)
	}
	if !res.Applied {
		t.Fatalf("ranking fenced: %s", res.Reason)
	}
	committed := false
	for _, e := range res.Events {
		if e.Name == events.MessageCommitted {
			committed = true
		}
	}
	if !committed {
		t.Fatalf("the ranking applied but committed nothing: %+v", res.Events)
	}
	if got := red.Generation(); got != 1 {
		t.Fatalf("generation = %d after the commit, want 1 (a commit advances the generation)", got)
	}

	// The late reply: the adjudication was asked under generation 0 (the
	// commit's ask-time generation), and it lands after the commit advanced
	// the session to generation 1. The fence must discard it.
	late := ses.DecisionMsg{
		Header: ses.Header{
			SessionID:       "dec-fence",
			Seq:             12,
			Generation:      0, // asked before the commit; the session is past it
			ProfileRevision: "1",
		},
		RegionID:       "box",
		ObservationSeq: obsNow,
		MessageID:      "m-a",
		SpeechEligible: true,
		Accepted:       true,
		Provider:       "systemone",
		Model:          decisionTestModel,
	}
	res, err = red.Apply(ctx, late)
	if err != nil {
		t.Fatalf("late decision: %v", err)
	}
	if res.Applied || res.Reason != "stale_generation" {
		t.Fatalf("late reply: applied=%v reason=%q, want fenced as stale_generation", res.Applied, res.Reason)
	}

	// And a reply the session is still standing behind applies: the
	// generation the session now stands at, a fresh seq.
	current := late
	current.Header.Seq = 13
	current.Header.Generation = 1
	res, err = red.Apply(ctx, current)
	if err != nil {
		t.Fatalf("current decision: %v", err)
	}
	if !res.Applied || res.Reason != "applied" {
		t.Fatalf("current reply: applied=%v reason=%q, want applied", res.Applied, res.Reason)
	}

	// The duplicate: that same delivered message, the same seq, delivered
	// again (the scenario harness's decision_duplicate fault). Only a seq
	// the session actually applied can be a duplicate: the fenced late reply
	// above consumed no seq, so the redelivery is the applied one's.
	dup := current
	res, err = red.Apply(ctx, dup)
	if err != nil {
		t.Fatalf("duplicate decision: %v", err)
	}
	if res.Applied || res.Reason != "duplicate_seq" {
		t.Fatalf("duplicate reply: applied=%v reason=%q, want fenced as duplicate_seq", res.Applied, res.Reason)
	}
}
