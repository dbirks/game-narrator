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
	"encoding/base64"
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

// ttsGateway is the fake /audio/speech endpoint for one scenario run. It
// speaks both wires the real client knows: the atomic answer (200 audio/
// wav) and the measured streaming wire (200 text/event-stream, the
// speech.audio.delta / speech.audio.done frames, ADR-0057), so the real
// tts.Client's wire parsing and validation run on the harness exactly as
// in production. The tts endpoint_down window rides the capture clock the
// harness stamps on the gateway (setStampMS) before each call — the
// synthesis payload carries no capture clock — and applies at the wire: a
// streaming request stamped inside the window gets its connection dropped
// after half the audio, with no terminal event: the endpoint death the
// fault names.
type ttsGateway struct {
	srv *httptest.Server
	// model is the deployment's voice model; the gateway ignores it, the
	// client's payload carries it.
	model string
	// missingVoice is the voice this replica cannot resolve; the gateway
	// answers the unknown-voice error for it.
	missingVoice string
	// The endpoint_down window on the capture clock; zero switches the
	// fault off. stampMS is the capture time of the request in flight.
	downFromMS, downToMS int
	stampMS              int64
	// toneMS is the tone the endpoint answers with, in milliseconds; the
	// harness default (100) is the historical shape. A scenario that models
	// an utterance still playing when the next line commits (the catch-up
	// policy's batched path, issue #25, ADR-0122) names a tone longer than
	// its clip's commit cadence.
	toneMS int

	mu       sync.Mutex
	injector *faultInjector
	idx      int
	fired    bool
	hits     int
	missing  int
	streams  int
}

// newTTSGateway starts the endpoint; Close stops it.
func newTTSGateway(model, missingVoice string, injector *faultInjector, idx, downFromMS, downToMS int) *ttsGateway {
	g := &ttsGateway{model: model, missingVoice: missingVoice, injector: injector, idx: idx, downFromMS: downFromMS, downToMS: downToMS, toneMS: 100}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	return g
}

// setToneMS sizes the tone the endpoint answers with (the scenario's
// inference.tts_tone_ms, issue #25, ADR-0122): a tone longer than the
// clip's commit cadence is what makes an utterance still playing when the
// next line commits, the overlap the catch-up policy's batched path grades.
func (g *ttsGateway) setToneMS(ms int) {
	if ms > 0 {
		g.toneMS = ms
	}
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

// setStampMS records the capture time of the synthesis request about to be
// issued; the endpoint_down window is applied against it at the wire.
func (g *ttsGateway) setStampMS(ms int64) {
	g.mu.Lock()
	g.stampMS = ms
	g.mu.Unlock()
}

// Streams counts the streaming (SSE) requests the gateway served.
func (g *ttsGateway) Streams() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.streams
}

// wireSynthesis is the slice of the synthesis payload the gateway inspects.
type wireSynthesis struct {
	Input  string `json:"input"`
	Voice  string `json:"voice"`
	Stream bool   `json:"stream"`
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

	// A real deployment returns the voice's audio; the fake returns a tone
	// so the client's WAV validation and the session lifecycle run on real
	// bytes. The tone's duration is the scenario's (g.toneMS): short by
	// default (100 ms, too short to matter in the 2.4 s scenario budget),
	// longer when the scenario models an utterance still playing when the
	// next line commits (issue #25, ADR-0122).
	wav := speech.MakeToneWAV(440, float64(g.toneMS)/1000.0, 24000)
	if !req.Stream {
		// The atomic wire: one validated WAV answer.
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("X-Request-Id", "req-fake-tts")
		_, _ = w.Write(wav)
		return
	}
	g.serveStream(w, wav)
}

// serveStream answers a streaming request on the measured SSE wire
// (ADR-0057). When the request's capture stamp falls inside the
// endpoint_down window the connection is dropped after half the audio — no
// further frame, no terminal event: the endpoint dies mid-synthesis and
// the real client sees the mid-stream EOF.
func (g *ttsGateway) serveStream(w http.ResponseWriter, wav []byte) {
	g.mu.Lock()
	g.streams++
	down := g.downToMS > 0 && inFaultWindow(g.downFromMS, g.downToMS, int(g.stampMS))
	first := !g.fired
	if down {
		g.fired = true
	}
	stamp := g.stampMS
	g.mu.Unlock()
	if down && first && g.injector != nil {
		g.injector.fire(g.idx, Fault{Type: FaultEndpointDown, Role: RoleTTS, FromMS: g.downFromMS, ToMS: g.downToMS}, stamp,
			fmt.Sprintf("tts endpoint died mid-stream in window %d..%d ms (SSE connection dropped after half the audio)", g.downFromMS, g.downToMS))
	}
	fl, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("X-Request-Id", "req-fake-tts")
	w.WriteHeader(http.StatusOK)
	parts := splitBytes(wav, 4)
	for i, p := range parts {
		f, _ := json.Marshal(map[string]any{"type": "speech.audio.delta", "audio": base64.StdEncoding.EncodeToString(p), "response_format": "wav"})
		fmt.Fprintf(w, "event: speech.audio.delta\ndata: %s\n\n", f)
		if fl != nil {
			fl.Flush()
		}
		if down && i == len(parts)/2-1 {
			return // the endpoint dies: the handler returns, the connection closes
		}
	}
	d, _ := json.Marshal(map[string]any{"type": "speech.audio.done", "usage": map[string]int{"input_tokens": 6, "output_tokens": 3}})
	fmt.Fprintf(w, "event: speech.audio.done\ndata: %s\n\n", d)
	if fl != nil {
		fl.Flush()
	}
}

// splitBytes cuts b into n contiguous pieces (n >= 1).
func splitBytes(b []byte, n int) [][]byte {
	if n <= 0 {
		n = 1
	}
	step := (len(b) + n - 1) / n
	var out [][]byte
	for i := 0; i < len(b); i += step {
		end := i + step
		if end > len(b) {
			end = len(b)
		}
		out = append(out, b[i:end])
	}
	return out
}

// ttsClient is the slice of the real TTS client the harness drives; the
// interface keeps the wrapper unit-testable without a network.
type ttsClient interface {
	Generate(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error)
	GenerateStream(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error)
}

// faultedTTS wraps the real tts.Client with the endpoint_down window for the
// tts role. The window cannot ride the synthesis wire (the payload carries
// no capture clock), so the wrapper stamps the request's capture time on
// the gateway before the call; the gateway then applies the window at the
// wire — a streaming request inside the window is dropped mid-stream (the
// real client classifies the mid-stream EOF as the endpoint-down failure),
// and an atomic request inside the window is refused with the typed error
// a 503 classifies to.
type faultedTTS struct {
	client ttsClient
	gw     *ttsGateway
	fromMS int
	toMS   int
	// stampMS is the capture time of the request about to be issued; the
	// harness sets it before every call.
	stampMS int64

	injector *faultInjector
	idx      int
	mu       sync.Mutex
	fired    bool
	blocked  int
}

// GenerateStream stamps the gateway with the request's capture time and
// hands to the real client: the endpoint_down window applies at the wire
// (the connection is dropped mid-stream inside it), not before the request.
func (f *faultedTTS) GenerateStream(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error) {
	if f.gw != nil {
		f.gw.setStampMS(f.stampMS)
	}
	return f.client.GenerateStream(ctx, req)
}

// Generate enforces the window on the atomic wire (the fallback path: a
// deployment that refused the stream flag), then hands to the real client.
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
