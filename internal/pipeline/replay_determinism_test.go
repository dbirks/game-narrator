package pipeline_test

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// slowOCR is the artificial CPU-speed knob of the replay determinism proof: it
// wraps another engine and adds a fixed per-call delay (300 ms here, well over
// the corpus's 100 ms frame interval) before forwarding. The wrapped engine is
// what answers; the delay is the only change, which is what lets the test claim
// "same engine, slower machine".
type slowOCR struct {
	inner ocr.Engine
	d     time.Duration
}

func (s *slowOCR) Name() string { return "slow(" + engineName(s.inner) + ")" }

// engineName resolves the wrapped engine's label through ocr.Namer for the
// provenance string; an anonymous fake has no name.
func engineName(e ocr.Engine) string {
	if n, ok := e.(ocr.Namer); ok {
		if s := n.Name(); s != "" {
			return s
		}
	}
	return "unnamed"
}

func (s *slowOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	t := time.NewTimer(s.d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrCancelled, Engine: "slow", Err: ctx.Err()}
	}
	return s.inner.Recognize(ctx, img)
}

// TestReplaySlowOCRCommitsLikeFastPath is the CPU-speed-independence half of
// docs/spec/12's determinism requirement, with fakes: the same clip through the
// same scripted engine, once answering immediately and once with 300 ms added
// per call. Both runs are ModeReplay, and both must commit the same messages at
// the same capture timestamps. In ModeLive the slow run would look different
// (the live supersede path cancels reads that outlive a frame, and which reads
// outlive which frames is a machine-speed coin flip), which is why the
// assertion is scoped to the replay mode that promises it.
func TestReplaySlowOCRCommitsLikeFastPath(t *testing.T) {
	script := ocrScript(t)

	fast := &fakeOCR{script: script, delay: fastScript, respectCtx: true}
	hFast := newHarness(t, fast, func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
	stFast, evsFast, err := hFast.run(t, hFast.source(0, false))
	if err != nil {
		t.Fatalf("fast run: %v", err)
	}
	if fast.misses.Load() != 0 {
		t.Fatalf("fast run: fake OCR was handed %d crops that are not in the script", fast.misses.Load())
	}

	slow := &slowOCR{inner: &fakeOCR{script: script, delay: fastScript, respectCtx: true}, d: 300 * time.Millisecond}
	hSlow := newHarness(t, slow, func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
	stSlow, evsSlow, err := hSlow.run(t, hSlow.source(0, false))
	if err != nil {
		t.Fatalf("slow run: %v", err)
	}
	timeline(t, hFast.runner)
	timeline(t, hSlow.runner)

	// The same decisions at the same capture times, no matter the per-call cost.
	cFast, cSlow := commitStamps(t, evsFast), commitStamps(t, evsSlow)
	if len(cFast) != 2 {
		t.Fatalf("fast run committed %v, want two commits", cFast)
	}
	if !slicesEqual(cFast, cSlow) {
		t.Errorf("fast committed %v, slow (300 ms/call) committed %v: replay mode must not depend on OCR speed", cFast, cSlow)
	}

	// A clean replay drops and cancels nothing in either run: the slow run in
	// particular must not pay for its latency in superseded reads.
	for name, st := range map[string]pipeline.Stats{"fast": stFast, "slow": stSlow} {
		if st.OCRCancelled != 0 || st.OCRStale != 0 || st.FramesDropped != 0 {
			t.Errorf("%s run: clean replay left drops behind: cancelled=%d stale=%d frames_dropped=%d",
				name, st.OCRCancelled, st.OCRStale, st.FramesDropped)
		}
		if st.StaleApplied != 0 {
			t.Errorf("%s run: stale_generation_applied = %d", name, st.StaleApplied)
		}
	}
	if stFast.OCRCalls != stSlow.OCRCalls {
		t.Errorf("fast run made %d OCR calls, slow run %d: the delay must slow calls, not change how many are made",
			stFast.OCRCalls, stSlow.OCRCalls)
	}
	t.Logf("replay determinism: fast=%v slow=%v (fast ocr_calls=%d, slow ocr_calls=%d)", cFast, cSlow, stFast.OCRCalls, stSlow.OCRCalls)
}
