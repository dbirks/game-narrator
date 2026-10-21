package runtime_test

// Gateway 404/503 outage graceful degradation (issue #41): with the model
// gateway 404ing the first request and 503ing every later one on the
// session's roles mid-session, the session keeps committing on the local
// evidence, the decision view reports degraded, cached TTS still plays, and
// an uncached utterance ends failed without stalling commits or shutdown.
// All providers are synthetic stubs; outageTTS returns the classified 503.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/providers/router"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// outageGateway is the one fake gateway the model roles talk to: the Clef
// /systemone route and the VLM's /v1/chat/completions route, each answering
// 404 on its first request and 503 on every later one, counting both phases.
type outageGateway struct {
	server                           *httptest.Server
	mu                               sync.Mutex
	clef404, clef503, vlm404, vlm503 int
}

func newOutageGateway(t *testing.T) *outageGateway {
	t.Helper()
	g := &outageGateway{}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *outageGateway) handle(w http.ResponseWriter, r *http.Request) {
	var n404, n503 *int
	switch r.URL.Path {
	case "/systemone":
		n404, n503 = &g.clef404, &g.clef503
	case "/v1/chat/completions":
		n404, n503 = &g.vlm404, &g.vlm503
	default:
		http.Error(w, "no such route", http.StatusNotFound)
		return
	}
	status := http.StatusServiceUnavailable
	g.mu.Lock()
	if *n404 == 0 && *n503 == 0 { // first hit on the route: the 404 phase
		status = http.StatusNotFound
	}
	if status == http.StatusNotFound {
		*n404++
	} else {
		*n503++
	}
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, `{"error":{"message":"synthetic outage"}}`)
}

func (g *outageGateway) hits() (c404, c503, v404, v503 int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.clef404, g.clef503, g.vlm404, g.vlm503
}

// outageTTS is the programmable Base (voice_clone) client: warm answers are
// the suite's tone; in the outage it returns the classified 503 (KindUnavailable).
type outageTTS struct {
	mu    sync.Mutex
	down  bool
	toneS float64
}

func (o *outageTTS) Generate(_ context.Context, _ tts.GenerateRequest) (tts.Audio, error) {
	return o.synth()
}

func (o *outageTTS) GenerateWithReference(_ context.Context, req tts.GenerateRequest, _ []byte, _ string) (tts.Audio, error) {
	return o.synth()
}

func (o *outageTTS) synth() (tts.Audio, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.down {
		err := providers.NewError(providers.KindUnavailable, http.StatusServiceUnavailable, "503 service unavailable")
		err.Model = "fake-base"
		return tts.Audio{}, err
	}
	wav := speech.MakeToneWAV(440, o.toneS, 16000)
	return tts.Audio{WAV: wav, SampleRate: 16000, Channels: 1, BitsPerSample: 16,
		Duration: time.Duration(o.toneS * float64(time.Second)), Model: "fake-base"}, nil
}

// TestGatewayOutage404And503ClefAndVLMDegradeToLocal is the Clef + VLM half
// of issue #41: both the decision role's /systemone route and the VLM
// fallback's /v1/chat/completions route meet the 404-then-503 outage; the
// session commits both lines locally and reports the decision role degraded.
func TestGatewayOutage404And503ClefAndVLMDegradeToLocal(t *testing.T) {
	corpusPresent(t)
	gw := newOutageGateway(t)
	// Margin 0.99 puts the clip's accepted rankings below the threshold, so
	// every line is offered to Clef and the outage is met on the wire.
	role := providers.RoleConfig{
		Role: providers.RoleDecision, Protocol: providers.ProtocolSystemOne,
		BaseURL: gw.server.URL, Model: "clef-fake", Timeout: 2 * time.Second,
	}
	reg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{providers.RoleDecision: role})
	decision := &runtime.DecisionOptions{Router: router.New(reg, router.Options{}), Role: role, Margin: 0.99}
	vlmRole := providers.RoleConfig{
		Role: providers.RoleVisionOCRFallback, Protocol: providers.ProtocolOpenAIChatVision,
		BaseURL: gw.server.URL + "/v1", Model: "vlm-fake", Timeout: 5 * time.Second,
	}
	client, err := openai.NewFromRole(vlmRole)
	if err != nil {
		t.Fatalf("vlm client: %v", err)
	}
	// Threshold 0.99: every scripted truth read (conf 0.95) is unresolved,
	// so the VLM is consulted on every frame; when it fails the local read
	// stands and the line still commits.
	policy := fallbackPolicyForTests()
	policy.ConfidenceThreshold = 0.99
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withOCRFallback(&pipeline.OCRFallback{Secondary: ocr.NewVLMEngine(ocr.OpenAIClient{Client: client}), Policy: policy}),
		withDecision(decision),
	)
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	info, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 120*time.Second)
	if final.State != runtime.StateStopped || final.Reason != runtime.StopReasonSourceEnd {
		t.Fatalf("state = %q (reason %s), want stopped/source_end (the outage must not fail the session)", final.State, final.Reason)
	}
	if final.Stats.Commits < 2 {
		t.Fatalf("commits = %d, want 2 (local evidence must stand while Clef and VLM are down)", final.Stats.Commits)
	}
	c404, c503, v404, v503 := gw.hits()
	if c404 < 1 || c503 < 1 || v404 < 1 || v503 < 1 {
		t.Fatalf("gateway 404/503 hits = clef %d/%d, vlm %d/%d, want 404-then-503 on both routes", c404, c503, v404, v503)
	}
	snap, ok := e.mgr.Get(info.ID)
	if !ok {
		t.Fatalf("session %s vanished", info.ID)
	}
	if d := snap.Decision; d == nil {
		t.Fatal("the session's decision view is nil: the decision role is not wired into the session")
	} else if !d.Configured || !d.Degraded || d.Calls < 1 || d.Answered != 0 || d.Reason == "" {
		t.Fatalf("session decision view = %+v, want configured, degraded with a non-empty reason, calls ≥1, answered 0", d)
	}
}

// TestGatewayOutage503CachedTTSPlaysAndUncachedFailsCleanly is the TTS half
// of issue #41: session 1 runs the clip warm (warming line 1 into
// speech.Cache); the Base endpoint then 503s every request, and in session
// 2 the cached line still plays from the cache while the uncached line ends
// failed, and the session still stops cleanly (startReplay reuses the profile, so the cache key is identical across the runs).
func TestGatewayOutage503CachedTTSPlaysAndUncachedFailsCleanly(t *testing.T) {
	outage := &outageTTS{toneS: 0.1}
	e := newVoiceEnv(t, 10*time.Millisecond, 100*time.Millisecond, true, dummy.New(0, 0), "", nil)
	e.svc.Base = outage // warm for session 1, the 503 for session 2

	// Session 1 (warm): the clip runs to the source's end, warming line 1 into speech.Cache.
	sid1 := e.startReplay()
	waitState(t, e.mgr, sid1, 120*time.Second)
	// The successor prewarm (FR-11) also filled line 2 into the cache during
	// session 1; reset the cache to exactly line 1 so session 2's uncached
	// line is a TTS round trip, not a prewarm hit. Line 1's key is read off
	// its ready event (the commit path's identity) and re-warmed alone.
	var key speech.CacheKey
	for _, ev := range e.sessionEvents(sid1) {
		if ev.Name == events.SpeechReady {
			if k, _ := payload(t, ev)["cache_key"].(string); k != "" {
				key = speech.CacheKey(k)
				break
			}
		}
	}
	if _, _, err := e.svc.Speech.EvictTransient(e.ctx, 0); err != nil {
		t.Fatalf("evicting the cache to exactly line 1: %v", err)
	}
	if _, err := e.svc.Speech.GetOrGenerate(e.ctx, key, speech.PriorityCurrent,
		func(ctx context.Context) (tts.Audio, error) {
			return outage.Generate(ctx, tts.GenerateRequest{Text: "line 1", Voice: testVoiceName})
		}); err != nil {
		t.Fatalf("re-warming line 1: %v", err)
	}
	outage.mu.Lock()
	outage.down = true // the outage: every TTS request from here on is a 503
	outage.mu.Unlock()

	// Session 2 (503 upstream): the cached line still plays (ready from the
	// cache, started, natural end); the uncached line ends failed; the session stops cleanly.
	sid2 := e.startReplay()
	final2 := waitState(t, e.mgr, sid2, 120*time.Second)
	if final2.State != runtime.StateStopped || final2.Reason != runtime.StopReasonSourceEnd {
		t.Fatalf("state = %q (reason %s), want stopped/source_end (the failed utterance must not stall shutdown)", final2.State, final2.Reason)
	}
	if final2.Stats.Commits < 2 {
		t.Fatalf("session 2 committed %d line(s), want 2 (the TTS outage must not stall commits)", final2.Stats.Commits)
	}
	var cacheReady, started, endedOK, failedErr int
	for _, ev := range e.sessionEvents(sid2) {
		p := payload(t, ev)
		switch {
		case ev.Name == events.SpeechReady && p["from_cache"] == true:
			cacheReady++
		case ev.Name == events.SpeechStarted:
			started++
		case ev.Name == events.SpeechEnded && p["result"] == "ended":
			endedOK++
		case ev.Name == events.SpeechEnded && p["result"] == "failed" && p["reason"] == "provider_error":
			failedErr++
		}
	}
	if cacheReady < 1 || started < 1 || endedOK < 1 {
		t.Fatalf("cached line: from_cache ready %d, started %d, natural end %d — the cache hit must play", cacheReady, started, endedOK)
	}
	if failedErr < 1 {
		t.Fatal("the uncached line did not end failed/provider_error while the TTS endpoint was 503")
	}
}
