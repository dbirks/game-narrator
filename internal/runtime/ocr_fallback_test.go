package runtime_test

// Unit proof of the VLM OCR fallback's session wiring (docs/spec/05
// "Compare lightweight OCR against an optional VLM fallback for difficult
// fonts; include both and the discrepancy", ADR-0040): the session's OCR
// engine is wrapped in an ocr.FallbackEngine over the configured secondary,
// the ocr.fallback trace field appears in the session's terminal events, and
// an unconfigured role (the nil option) leaves the single-engine behaviour
// — and the session's commits — untouched. The secondary is a fake
// VLMClient behind a real ocr.VLMEngine: the provider seam is fake, the
// fallback policy, the crop preparation and the pipeline's trace path are
// the real thing. The live proof (real gateway, hard-font crop) is
// TestLiveOCRFallbackSession in ocr_live_test.go.

import (
	"context"
	"encoding/json"
	"errors"
	"image"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// ── fakes ------------------------------------------------------------------

// lowConfPrimary is the scripted primary for the fallback test: it "reads"
// the crop the way Tesseract does on a hard font — right ballpark, wrong
// glyphs — at a confidence below the fallback's 0.6 threshold, so every read
// is unresolved and the secondary gets consulted (the documented trigger).
type lowConfPrimary struct {
	script map[string]string
}

func (e *lowConfPrimary) Name() string { return "low-conf-primary" }

func (e *lowConfPrimary) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	text, ok := e.script[vision.ImageHash(img)]
	if !ok {
		return ocr.Result{}, errors.New("low-conf-primary: no script for this crop")
	}
	// The garbled reading: a prefix of the truth plus noise, so the
	// normalised comparison against the VLM's clean reading disagrees —
	// the "include both and the discrepancy" case, not the agreement case.
	garbled := text
	if len(garbled) > 8 {
		garbled = garbled[:8]
	}
	return ocr.Result{Text: garbled + " ▓▒░", MeanConf: 0.2, Engine: e.Name()}, nil
}

// fakeVLM is the scripted VLMClient seam: it counts calls, records the last
// request (the privacy contract's assertion: the crop is the only payload
// that leaves, and the instruction and the MaxTokens bound pass through),
// and answers with the configured reading.
type fakeVLM struct {
	mu      sync.Mutex
	calls   int
	answer  string
	lastReq ocr.VLMRequest
}

func (f *fakeVLM) Chat(_ context.Context, req ocr.VLMRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.lastReq = req
	return f.answer, nil
}

func (f *fakeVLM) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeVLM) lastRequest() ocr.VLMRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastRequestLocked()
}

func (f *fakeVLM) lastRequestLocked() ocr.VLMRequest { return f.lastReq }

// fallbackPolicyForTests is the production default with the rate budget
// widened so a sub-second replay clip cannot spend its burst before the
// assertion: the rate limit's own behaviour is pinned by the ocr package's
// clock-driven tests; this test is about the session wiring, not the
// limiter.
func fallbackPolicyForTests() ocr.FallbackPolicy {
	return ocr.FallbackPolicy{RatePerSecond: 1000, RateBurst: 100}
}

// terminalEvent pulls the session's terminal event (session.stopped, or
// session.failed when the run could not end cleanly) and returns its raw
// payload plus the decoded trace timeline.
func terminalEvent(t *testing.T, e *env, sessionID string) (string, []pipeline.TraceEntry) {
	t.Helper()
	evs := eventsFrom(t, e, sessionID)
	var raw string
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Name == events.SessionStopped || evs[i].Name == runtime.EventSessionFailed {
			raw = string(evs[i].Data)
			break
		}
	}
	if raw == "" {
		t.Fatal("the session's terminal event never landed")
	}
	var payload struct {
		Trace []pipeline.TraceEntry `json:"trace"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("terminal event payload: %v", err)
	}
	return raw, payload.Trace
}

// ── tests --------------------------------------------------------------------

// TestSessionOCRFallbackEngages is the wired proof: a primary that cannot
// resolve the crop hands every frame to the secondary, the trace rows carry
// the decision (chosen=secondary with the discrepancy recorded), and the
// session's terminal event is where the developer UI reads it.
func TestSessionOCRFallbackEngages(t *testing.T) {
	corpusPresent(t)
	script := truthScript(t)
	vlm := &fakeVLM{answer: "The harbour bell rang twice before anyone looked up."}

	e := newEnv(t,
		withOCR(&lowConfPrimary{script: script}),
		withOCRFallback(&pipeline.OCRFallback{
			Secondary: ocr.NewVLMEngine(vlm),
			Policy:    fallbackPolicyForTests(),
		}),
	)

	profileID, _ := seedSyntheticProfile(t, e.st, true)
	info, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 120*time.Second)
	if final.State != runtime.StateStopped && final.State != runtime.StateFailed {
		t.Fatalf("state = %q, want a terminal state", final.State)
	}

	if got := vlm.callCount(); got == 0 {
		t.Fatal("the secondary was never called: the fallback is not wired into the session")
	}

	raw, rows := terminalEvent(t, e, info.ID)
	var secondary, annotated int
	for _, r := range rows {
		if r.OCRFallback == nil {
			continue
		}
		annotated++
		if r.OCRFallback.Chosen == "secondary" {
			secondary++
		}
	}
	if annotated == 0 {
		t.Fatalf("no trace row carries the ocr.fallback field (trace rows: %d)", len(rows))
	}
	if secondary == 0 {
		t.Fatalf("no trace row with ocr.fallback chosen=secondary (annotated=%d)", annotated)
	}
	// The garbled primary disagrees with the VLM's clean reading on every
	// annotated frame, so every decision this test's fakes produce is the
	// secondary's — a primary choice would mean the choice rule misfired.
	if secondary < annotated {
		t.Fatalf("annotated=%d but chosen=secondary=%d: the choice rule misfired", annotated, secondary)
	}
	// The field in the session's event stream is the documented one.
	if !strings.Contains(raw, `"ocr_fallback"`) {
		t.Fatal("the ocr.fallback field is missing from the session's terminal event")
	}

	// The privacy contract at the seam: the request carries the prepared
	// crop and the fixed instruction, and the MaxTokens bound the engine
	// set (256 default) passed through — never raised.
	last := vlm.lastRequest()
	if len(last.PNG) == 0 || last.Instruction == "" || last.MaxTokens != 256 {
		t.Fatalf("VLM request = (png=%dB instruction=%q max_tokens=%d), want crop + instruction + the 256 bound",
			len(last.PNG), last.Instruction, last.MaxTokens)
	}
}

// TestSessionOCRFallbackUnconfigured is the negative half: without the role
// the session runs the single local engine — no secondary call, no ocr.
// fallback field in the trace — and the run is otherwise unchanged (both
// lines still commit).
func TestSessionOCRFallbackUnconfigured(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(truthScript(t), 0))

	profileID, _ := seedSyntheticProfile(t, e.st, true)
	info, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 120*time.Second)
	if final.State != runtime.StateStopped {
		t.Fatalf("state = %q, want stopped (the unconfigured fallback must not degrade the run)", final.State)
	}
	if final.Stats.OCRFallbackFrames != 0 || final.Stats.OCRFallbackSecondaryCalls != 0 {
		t.Fatalf("unconfigured session reported fallback activity: %+v", final.Stats)
	}
	raw, rows := terminalEvent(t, e, info.ID)
	if strings.Contains(raw, `"ocr_fallback"`) {
		t.Fatal("the ocr.fallback field must not appear when the role is not configured")
	}
	for _, r := range rows {
		if r.OCRFallback != nil {
			t.Fatalf("unconfigured trace row carries a fallback annotation: %+v", r)
		}
	}
	// Unchanged behaviour: the clip still commits both lines.
	if final.Stats.Commits != 2 {
		t.Fatalf("commits = %d, want 2 (the single-engine run is unchanged)", final.Stats.Commits)
	}
}
