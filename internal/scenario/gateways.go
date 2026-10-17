// gateways.go are the fault-injecting fake provider endpoints the scenario
// harness runs against: a System One (decision) gateway and a /audio/speech
// (TTS) gateway. The harness drives the *real* provider clients
// (systemone.Client, tts.Client) at these endpoints, so the wire contract,
// the response validation and the error taxonomy run exactly as in
// production; only the model behind the endpoint is scripted.
//
// Faults the gateways inject:
//
//	decision endpoint_down — inside [from_ms, to_ms) on the capture clock
//	                       (the request's state carries capture_ts_ms) the
//	                       gateway answers 503; the real client classifies
//	                       that as providers.KindUnavailable, the abstain
//	                       path.
//	decision decision_delay — requests at or after after_ms are held for
//	                          delay_ms before the answer, so the reply can
//	                          land after the dialogue advanced.
//	tts voice_missing_on_replica — for one voice the gateway answers the
//	                              unknown-voice error a replica that lost
//	                              the voice answers (400 "voice '…' not
//	                              found"); the real client maps that to
//	                              providers.KindVoiceMissing.
//
// The tts endpoint_down window cannot ride the wire (the synthesis payload
// carries no capture clock), so it is applied by the faultedTTS wrapper
// below instead, with the typed error a 503 classifies to — the real client
// does that classification itself, and TestTTSClientClassifies503As
// Unavailable pins the equivalence.

package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
)

// decisionGateway is the fake System One endpoint for one scenario run.
type decisionGateway struct {
	srv *httptest.Server
	// model is echoed into the answer so the client's model check passes.
	model string
	// Fault windows, capture-clock milliseconds; zero switches the fault off.
	downFromMS, downToMS  int
	delayAfterMS, delayMS int

	injector *faultInjector
	idxDown  int
	idxDelay int

	mu     sync.Mutex
	hits   int
	errors int
}

// newDecisionGateway starts the endpoint; Close stops it.
func newDecisionGateway(model string, injector *faultInjector, downFromMS, downToMS, idxDown, delayAfterMS, delayMS, idxDelay int) *decisionGateway {
	g := &decisionGateway{
		model:        model,
		downFromMS:   downFromMS,
		downToMS:     downToMS,
		delayAfterMS: delayAfterMS,
		delayMS:      delayMS,
		injector:     injector,
		idxDown:      idxDown,
		idxDelay:     idxDelay,
	}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	return g
}

// url is the base URL the systemone.Client is pointed at.
func (g *decisionGateway) url() string { return g.srv.URL }

// Close stops the endpoint.
func (g *decisionGateway) Close() { g.srv.Close() }

// Hits counts every request the gateway saw.
func (g *decisionGateway) Hits() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// Errors counts the faulted 503 answers the gateway sent.
func (g *decisionGateway) Errors() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.errors
}

// wireDecisionRequest is the slice of the System One wire request the gateway
// needs: state (the capture clock and the line to confirm) and the declared
// questions.
type wireDecisionRequest struct {
	State     map[string]json.RawMessage `json:"state"`
	Questions map[string]struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

// stateCaptureMS reads state["capture_ts_ms"], the capture clock the harness
// stamps into every decision request so the gateway's fault windows run on
// the recording's time, not the wall clock.
func stateCaptureMS(state map[string]json.RawMessage) int64 {
	if state == nil {
		return 0
	}
	var v int64
	if err := json.Unmarshal(state["capture_ts_ms"], &v); err != nil {
		return 0
	}
	return v
}

func (g *decisionGateway) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/systemone" {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req wireDecisionRequest
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ts := stateCaptureMS(req.State)

	g.mu.Lock()
	g.hits++
	g.mu.Unlock()

	// endpoint_down: the window is on the capture clock the request carries.
	if g.downToMS > 0 && inFaultWindow(g.downFromMS, g.downToMS, int(ts)) {
		g.mu.Lock()
		g.errors++
		g.mu.Unlock()
		if g.injector != nil {
			g.injector.fire(g.idxDown, Fault{Type: FaultEndpointDown, Role: RoleDecision, FromMS: g.downFromMS, ToMS: g.downToMS}, ts,
				fmt.Sprintf("decision endpoint answered 503 (window %d..%d ms)", g.downFromMS, g.downToMS))
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"detail":"upstream unavailable"}`)
		return
	}

	// decision_delay: hold the answer so it can arrive after the dialogue
	// advanced. The request context owns the deadline, exactly like the live
	// runtime's decision budget; a reply held past its deadline is a timeout,
	// and the harness treats it like any other provider error.
	if g.delayMS > 0 && ts >= int64(g.delayAfterMS) {
		if g.injector != nil {
			g.injector.fire(g.idxDelay, Fault{Type: FaultDecisionDelay, AfterMS: g.delayAfterMS, DelayMS: g.delayMS}, ts,
				fmt.Sprintf("decision reply held %d ms (requests at/after %d ms)", g.delayMS, g.delayAfterMS))
		}
		select {
		case <-time.After(time.Duration(g.delayMS) * time.Millisecond):
		case <-r.Context().Done():
			return
		}
	}

	// Answer every declared choice question with the line the harness asked
	// it to confirm: state["confirm_choice"], falling back to the first
	// declared option when the state names none (the probe shape). The
	// probabilities cover the declared set and sum to 1, the way the client's
	// validator requires.
	answers := map[string]any{}
	for qid, q := range req.Questions {
		if q.Type != "choice" || len(q.Criteria) == 0 {
			continue
		}
		choice, ok := confirmChoice(req.State)
		if !ok || q.Criteria[choice] == "" {
			choice, _ = firstKey(q.Criteria)
		}
		probs := map[string]float64{}
		for opt := range q.Criteria {
			if opt == choice {
				probs[opt] = 0.95
			} else {
				probs[opt] = 0.05 / float64(len(q.Criteria)-1)
			}
		}
		answers[qid] = map[string]any{
			"type":          "choice",
			"choice":        choice,
			"confidence":    0.95,
			"probabilities": probs,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req-fake-decision")
	fmt.Fprintf(w, `{"model":%q,"answers":%s,"usage":{"input_tokens":8,"output_tokens":0}}`, g.model, mustJSON(answers))
}

func confirmChoice(state map[string]json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(state["confirm_choice"], &s); err != nil || s == "" {
		return "", false
	}
	return s, true
}

func firstKey(m map[string]string) (string, bool) {
	for k := range m {
		return k, true
	}
	return "", false
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ttsGateway is the fake /audio/speech endpoint for one scenario run.
type ttsGateway struct {
	srv *httptest.Server
	// model is the deployment's voice model; the gateway ignores it, the
	// client's payload carries it.
	model string
	// missingVoice is the voice this replica cannot resolve; the gateway
	// answers the unknown-voice error for it.
	missingVoice string

	mu       sync.Mutex
	injector *faultInjector
	idx      int
	fired    bool
	hits     int
	missing  int
}

// newTTSGateway starts the endpoint; Close stops it.
func newTTSGateway(model, missingVoice string, injector *faultInjector, idx int) *ttsGateway {
	g := &ttsGateway{model: model, missingVoice: missingVoice, injector: injector, idx: idx}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	return g
}

// url is the base URL the tts.Client is pointed at.
func (g *ttsGateway) url() string { return g.srv.URL }

// Close stops the endpoint.
func (g *ttsGateway) Close() { g.srv.Close() }

// Hits counts every synthesis request the gateway saw.
func (g *ttsGateway) Hits() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.hits
}

// Missing counts the unknown-voice answers the gateway sent.
func (g *ttsGateway) Missing() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.missing
}

// wireSynthesis is the slice of the synthesis payload the gateway inspects.
type wireSynthesis struct {
	Input string `json:"input"`
	Voice string `json:"voice"`
}

func (g *ttsGateway) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/audio/speech" {
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req wireSynthesis
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	g.mu.Lock()
	g.hits++
	g.mu.Unlock()

	if g.missingVoice != "" && req.Voice == g.missingVoice {
		g.mu.Lock()
		g.missing++
		first := !g.fired
		g.fired = true
		g.mu.Unlock()
		if first && g.injector != nil {
			g.injector.fire(g.idx, Fault{Type: FaultVoiceMissingReplica, Voice: req.Voice}, 0,
				fmt.Sprintf("replica answered the unknown-voice error for voice %q", req.Voice))
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-fake-tts")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":"voice '%s' not found"}}`, req.Voice)
		return
	}

	// A real deployment returns the voice's audio; the fake returns a short
	// tone so the client's WAV validation and the session lifecycle run on
	// real bytes. One tenth of a second at 24 kHz: a playable utterance,
	// too short to matter in the 2.4 s scenario budget.
	wav := speech.MakeToneWAV(440, 0.1, 24000)
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("X-Request-Id", "req-fake-tts")
	_, _ = w.Write(wav)
}

// ttsClient is the slice of the real TTS client the harness drives; the
// interface keeps the wrapper unit-testable without a network.
type ttsClient interface {
	Generate(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error)
}

// faultedTTS wraps the real tts.Client with the endpoint_down window for the
// tts role. The window cannot ride the synthesis wire (the payload carries no
// capture clock), so it is enforced here with the typed error the client
// would classify a 503 into.
type faultedTTS struct {
	client ttsClient
	fromMS int
	toMS   int
	// stampMS is the capture time of the request about to be issued; the
	// harness sets it before every Generate call.
	stampMS int64

	injector *faultInjector
	idx      int
	mu       sync.Mutex
	fired    bool
	blocked  int
}

// Generate enforces the window, then hands to the real client.
func (f *faultedTTS) Generate(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	if f.toMS > 0 && inFaultWindow(f.fromMS, f.toMS, int(f.stampMS)) {
		f.mu.Lock()
		f.blocked++
		first := !f.fired
		if first {
			f.fired = true
		}
		f.mu.Unlock()
		if first && f.injector != nil {
			f.injector.fire(f.idx, Fault{Type: FaultEndpointDown, Role: RoleTTS, FromMS: f.fromMS, ToMS: f.toMS}, f.stampMS,
				fmt.Sprintf("tts endpoint refused in window %d..%d ms (503 classification)", f.fromMS, f.toMS))
		}
		e := providers.NewError(providers.KindUnavailable, http.StatusServiceUnavailable, "tts: Service Unavailable (fault injection: endpoint down)")
		e.Retryable = true
		return tts.Audio{}, e
	}
	return f.client.Generate(ctx, req)
}

// Blocked counts the requests the wrapper refused inside the window.
func (f *faultedTTS) Blocked() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blocked
}
