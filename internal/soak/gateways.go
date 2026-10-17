// gateways.go are the chaos-injecting fake provider endpoints the soak runs
// the real provider clients (systemone.Client, tts.Client) against — the same
// wire-contract arrangement as the scenario harness (ADR-0037), with the
// faults changed from fixed capture-clock windows to seeded, rate-controlled
// chaos (docs/spec/12 "2h simulated network/provider chaos").
//
// The faults split in two. The *scenario* faults are the M7 vocabulary,
// replayed in chaos form: decision_delay (a fixed hold), decision_duplicate
// (the sidecar re-applies the answer), voice_missing_on_replica (the TTS
// replica answers the unknown-voice error), endpoint_down (a 503, here as a
// random burst instead of a window) and drop_frames (a per-pass mask, applied
// by the harness to both layers). The *network-level* faults are new: latency
// spikes, 5xx bursts, connection resets (the response body is cut off and the
// TCP connection is closed, which the real clients classify as
// providers.KindUnavailable — "endpoint unreachable") and slow bodies (the
// response is written in chunks with sleeps between them).
//
// A panic inside a fault injector is recovered and counted, not swallowed:
// the soak's no-panic criterion covers the harness itself (AGENTS.md rule 3 —
// the faults are named, and a fault that crashes must be visible as a crash).

package soak

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
)

// Chaos fault kinds: the closed vocabulary the report counts per kind.
const (
	FaultLatencySpike      = "latency_spike"
	FaultHTTP5xx           = "http_5xx"
	FaultConnReset         = "conn_reset"
	FaultSlowBody          = "slow_body"
	FaultDecisionDelay     = "decision_delay"
	FaultVoiceMissing      = "voice_missing_on_replica"
	FaultTTSDown           = "tts_endpoint_down"
	FaultDropFrames        = "drop_frames"
	FaultDecisionDuplicate = "decision_duplicate"
)

// chaosRNG is the seeded, mutex-guarded source all fault selection runs
// through: one seed in the report makes a whole chaos run reproducible.
type chaosRNG struct {
	mu sync.Mutex
	r  *rand.Rand
}

func newChaosRNG(seed int64) *chaosRNG { return &chaosRNG{r: rand.New(rand.NewSource(seed))} }

func (c *chaosRNG) float64() float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.r.Float64()
}

// intn returns [0, n); n <= 0 is an error in the caller, not here.
func (c *chaosRNG) intn(n int) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.r.Intn(n)
}

// chaosDialer records the seeded RNG and the shared fault reporter.
type chaosDialer struct {
	rng    *chaosRNG
	report func(kind, role string, detail string)
}

// decisionGW is the fake System One endpoint with chaos injection.
type decisionGW struct {
	srv     *httptest.Server
	model   string
	chaos   *ChaosConfig
	rep     chaosDialer
	drainCh chan struct{} // closed by Close, so in-flight faults can bail

	mu             sync.Mutex
	hits           int
	faulted        int
	burstRemaining int
	panics         int
}

// wireDecisionRequest is the slice of the System One wire request the gateway
// needs (same shape as the scenario's): state and the declared questions.
type wireDecisionRequest struct {
	State     map[string]json.RawMessage `json:"state"`
	Questions map[string]struct {
		Type     string            `json:"type"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

func newDecisionGW(model string, chaos *ChaosConfig, rep chaosDialer) *decisionGW {
	g := &decisionGW{model: model, chaos: chaos, rep: rep, drainCh: make(chan struct{})}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	return g
}

func (g *decisionGW) url() string   { return g.srv.URL }
func (g *decisionGW) close()        { g.srv.Close(); close(g.drainCh) }
func (g *decisionGW) hitCount() int { g.mu.Lock(); defer g.mu.Unlock(); return g.hits }
func (g *decisionGW) faultedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.faulted
}

func (g *decisionGW) panicCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.panics
}

// guardPanic converts a panic in the injector into a counted, honest 500:
// the client sees a provider error, and the soak's panic counter sees the bug.
func (g *decisionGW) guardPanic(w http.ResponseWriter) {
	if p := recover(); p != nil {
		g.mu.Lock()
		g.panics++
		g.mu.Unlock()
		http.Error(w, fmt.Sprintf("fault injector panic: %v", p), http.StatusInternalServerError)
	}
}

// pickFault chooses this request's fault: an active 5xx burst always holds,
// otherwise a fresh fault fires with probability Chaos.Rate, chosen from the
// decision chaos set.
func (g *decisionGW) pickFault() string {
	if g.chaos == nil || !g.chaos.Enabled {
		return ""
	}
	g.mu.Lock()
	if g.burstRemaining > 0 {
		g.burstRemaining--
		g.mu.Unlock()
		return FaultHTTP5xx
	}
	fresh := g.rep.rng.float64() < g.chaos.Rate
	var kind string
	if fresh {
		switch g.rep.rng.intn(5) {
		case 0:
			kind = FaultLatencySpike
		case 1:
			kind = FaultHTTP5xx
		case 2:
			kind = FaultConnReset
		case 3:
			kind = FaultSlowBody
		default:
			kind = FaultDecisionDelay
		}
	}
	g.mu.Unlock()
	return kind
}

func (g *decisionGW) burstLen() int {
	b := g.chaos.BurstMax
	if g.chaos.BurstMax < g.chaos.BurstMin {
		b = g.chaos.BurstMin
	}
	return b
}

func (g *decisionGW) handle(w http.ResponseWriter, r *http.Request) {
	defer g.guardPanic(w)
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
	g.mu.Lock()
	g.hits++
	g.mu.Unlock()

	switch g.pickFault() {
	case FaultLatencySpike:
		d := g.chaos.spikeDuration(g.rep.rng)
		g.rep.report(FaultLatencySpike, "decision", fmt.Sprintf("response held %v", d))
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		case <-g.drainCh:
			return
		}
	case FaultDecisionDelay:
		g.rep.report(FaultDecisionDelay, "decision", fmt.Sprintf("scenario-shape hold of %v", g.chaos.DecisionDelayMS))
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		select {
		case <-time.After(time.Duration(g.chaos.DecisionDelayMS) * time.Millisecond):
		case <-r.Context().Done():
			return
		case <-g.drainCh:
			return
		}
	case FaultHTTP5xx:
		g.mu.Lock()
		g.faulted++
		if g.burstRemaining == 0 {
			g.burstRemaining = g.burstLen() - 1
		}
		g.mu.Unlock()
		g.rep.report(FaultHTTP5xx, "decision", "503 (burst)")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"detail":"upstream unavailable"}`)
		return
	case FaultConnReset:
		g.rep.report(FaultConnReset, "decision", "response cut off, connection closed")
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"model":"`+g.model+`","answers":{"q":`)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
		return
	case FaultSlowBody:
		g.rep.report(FaultSlowBody, "decision", fmt.Sprintf("body written in 4 chunks (%v gaps)", g.chaos.SlowBodyGapMS))
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		full := g.answer(req)
		chunk := len(full) / 4
		w.Header().Set("Content-Type", "application/json")
		for i := 0; i < 4; i++ {
			lo, hi := i*chunk, (i+1)*chunk
			if i == 3 {
				hi = len(full)
			}
			w.Write([]byte(full[lo:hi]))
			gap := time.Duration(g.chaos.SlowBodyGapMS)*time.Millisecond/2 +
				time.Duration(g.rep.rng.intn(g.chaos.SlowBodyGapMS/2+1))*time.Millisecond
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			case <-g.drainCh:
				return
			}
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Request-Id", "req-soak-decision")
	io.WriteString(w, g.answer(req))
}

// answer renders the healthy decision reply: every declared choice question
// answered with the line the harness asked to confirm (the scenario gateway's
// contract, so the real client's validation and model check run for real).
func (g *decisionGW) answer(req wireDecisionRequest) string {
	answers := map[string]any{}
	for qid, q := range req.Questions {
		if q.Type != "choice" || len(q.Criteria) == 0 {
			continue
		}
		choice := ""
		var s string
		if err := json.Unmarshal(req.State["confirm_choice"], &s); err == nil {
			choice = s
		}
		if choice == "" || q.Criteria[choice] == "" {
			for k := range q.Criteria {
				choice = k
				break
			}
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
	b, _ := json.Marshal(answers)
	return fmt.Sprintf(`{"model":%q,"answers":%s,"usage":{"input_tokens":8,"output_tokens":0}}`, g.model, b)
}

// ttsGW is the fake /audio/speech endpoint with chaos injection.
type ttsGW struct {
	srv     *httptest.Server
	model   string
	voice   string // the voice the run requests; voice_missing faults hit it
	chaos   *ChaosConfig
	rep     chaosDialer
	drainCh chan struct{}

	mu             sync.Mutex
	hits           int
	faulted        int
	missing        int
	burstRemaining int
	panics         int
}

func newTTSGW(model, voice string, chaos *ChaosConfig, rep chaosDialer) *ttsGW {
	g := &ttsGW{model: model, voice: voice, chaos: chaos, rep: rep, drainCh: make(chan struct{})}
	g.srv = httptest.NewServer(http.HandlerFunc(g.handle))
	return g
}

func (g *ttsGW) url() string   { return g.srv.URL }
func (g *ttsGW) close()        { g.srv.Close(); close(g.drainCh) }
func (g *ttsGW) hitCount() int { g.mu.Lock(); defer g.mu.Unlock(); return g.hits }
func (g *ttsGW) faultedCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.faulted
}
func (g *ttsGW) panicCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.panics
}

func (g *ttsGW) guardPanic(w http.ResponseWriter) {
	if p := recover(); p != nil {
		g.mu.Lock()
		g.panics++
		g.mu.Unlock()
		http.Error(w, fmt.Sprintf("fault injector panic: %v", p), http.StatusInternalServerError)
	}
}

func (g *ttsGW) pickFault() string {
	if g.chaos == nil || !g.chaos.Enabled {
		return ""
	}
	g.mu.Lock()
	if g.burstRemaining > 0 {
		g.burstRemaining--
		g.mu.Unlock()
		return FaultTTSDown
	}
	var kind string
	if g.rep.rng.float64() < g.chaos.Rate {
		switch g.rep.rng.intn(5) {
		case 0:
			kind = FaultLatencySpike
		case 1:
			kind = FaultTTSDown
		case 2:
			kind = FaultConnReset
		case 3:
			kind = FaultSlowBody
		default:
			kind = FaultVoiceMissing
		}
	}
	g.mu.Unlock()
	return kind
}

type wireSynthesis struct {
	Input string `json:"input"`
	Voice string `json:"voice"`
}

func (g *ttsGW) handle(w http.ResponseWriter, r *http.Request) {
	defer g.guardPanic(w)
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

	switch g.pickFault() {
	case FaultLatencySpike:
		d := g.chaos.spikeDuration(g.rep.rng)
		g.rep.report(FaultLatencySpike, "tts", fmt.Sprintf("synthesis held %v", d))
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		case <-g.drainCh:
			return
		}
	case FaultTTSDown:
		g.mu.Lock()
		g.faulted++
		if g.burstRemaining == 0 {
			g.burstRemaining = g.burstLen() - 1
		}
		g.mu.Unlock()
		g.rep.report(FaultTTSDown, "tts", "503 (burst)")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		io.WriteString(w, `{"error":{"message":"overloaded"}}`)
		return
	case FaultConnReset:
		g.rep.report(FaultConnReset, "tts", "audio cut off, connection closed")
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		wav := speech.MakeToneWAV(440, 0.1, 24000)
		w.Header().Set("Content-Type", "audio/wav")
		w.WriteHeader(http.StatusOK)
		w.Write(wav[:len(wav)/2])
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
		return
	case FaultVoiceMissing:
		g.rep.report(FaultVoiceMissing, "tts", fmt.Sprintf("replica answered the unknown-voice error for %q", req.Voice))
		g.mu.Lock()
		g.faulted++
		g.missing++
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "req-soak-tts")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, `{"error":{"message":"voice '%s' not found"}}`, req.Voice)
		return
	case FaultSlowBody:
		g.rep.report(FaultSlowBody, "tts", fmt.Sprintf("audio written in 4 chunks (%v gaps)", g.chaos.SlowBodyGapMS))
		g.mu.Lock()
		g.faulted++
		g.mu.Unlock()
		wav := speech.MakeToneWAV(440, 0.1, 24000)
		chunk := len(wav) / 4
		w.Header().Set("Content-Type", "audio/wav")
		for i := 0; i < 4; i++ {
			lo, hi := i*chunk, (i+1)*chunk
			if i == 3 {
				hi = len(wav)
			}
			w.Write(wav[lo:hi])
			gap := time.Duration(g.chaos.SlowBodyGapMS)*time.Millisecond/2 +
				time.Duration(g.rep.rng.intn(g.chaos.SlowBodyGapMS/2+1))*time.Millisecond
			select {
			case <-time.After(gap):
			case <-r.Context().Done():
				return
			case <-g.drainCh:
				return
			}
		}
		return
	}

	// The healthy reply: a short tone, the way the scenario's gateway serves
	// one, so the client's WAV validation runs on real bytes.
	wav := speech.MakeToneWAV(440, 0.1, 24000)
	w.Header().Set("Content-Type", "audio/wav")
	w.Header().Set("X-Request-Id", "req-soak-tts")
	w.Write(wav)
}

// spikeDuration draws the latency spike length from the chaos config bounds.
func (c *ChaosConfig) spikeDuration(rng *chaosRNG) time.Duration {
	min := int64(c.LatencySpikeMinMS)
	max := int64(c.LatencySpikeMaxMS)
	if max <= min {
		max = min + 1
	}
	return time.Duration(min+int64(rng.intn(int(max-min)+1))) * time.Millisecond
}

func (g *ttsGW) burstLen() int {
	b := g.chaos.BurstMax
	if g.chaos.BurstMax < g.chaos.BurstMin {
		b = g.chaos.BurstMin
	}
	return b
}

// classifyKind reduces one provider failure to its taxonomy kind (the same
// reduction the scenario harness makes for its timeline).
func classifyKind(err error) string {
	var pe *providers.Error
	if errors.As(err, &pe) {
		return string(pe.Kind)
	}
	return "unknown"
}

// ttsClient is the slice of the real TTS client the sidecar drives.
type ttsClient interface {
	Generate(ctx context.Context, req tts.GenerateRequest) (tts.Audio, error)
}
