package pipeline_test

import (
	"context"
	"image"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// countingSecondary is a stand-in VLM: it counts every call and answers with
// the given text at the VLM's own prior confidence. It is an ocr.Engine, not
// an ocr.VLMEngine, on purpose: the pipeline wraps whatever Engine the runtime
// hands it, and the wrap must not branch on the concrete type.
type countingSecondary struct {
	calls atomic.Int32
	text  string
}

func (c *countingSecondary) Name() string { return "counting-vlm" }

func (c *countingSecondary) Recognize(context.Context, image.Image) (ocr.Result, error) {
	c.calls.Add(1)
	return ocr.Result{Text: c.text, Engine: "counting-vlm", MeanConf: 0.6}, nil
}

// scriptSecondary recites the corpus truth per crop hash (what a VLM that
// reads well would return: the text that is actually on the pixels), counting
// calls like countingSecondary does.
type scriptSecondary struct {
	script map[string]string
	calls  atomic.Int32
}

func (s *scriptSecondary) Name() string { return "scripted-vlm" }

func (s *scriptSecondary) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	s.calls.Add(1)
	res := ocr.Result{Text: s.script[vision.ImageHash(img)], Engine: "scripted-vlm", MeanConf: 0.6}
	if res.Text == "" {
		return res, nil
	}
	res.Lines = []ocr.Line{{Text: res.Text, Conf: 0.6}}
	for _, w := range strings.Fields(res.Text) {
		res.Words = append(res.Words, ocr.Word{Text: w, Conf: 0.6})
	}
	return res, nil
}

// lowConfPrimary recites the corpus truth (exactly what a real local engine
// would) at a confidence below the fallback threshold, so every present frame
// is unresolved and the fallback path is exercised on the whole clip.
type lowConfPrimary struct{ script map[string]string }

func (s *lowConfPrimary) Name() string { return "lowconf" }

func (s *lowConfPrimary) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	const conf = 0.3
	res := ocr.Result{Text: s.script[vision.ImageHash(img)], Engine: "lowconf", MeanConf: conf}
	if res.Text == "" {
		return res, nil
	}
	res.Lines = []ocr.Line{{Text: res.Text, Conf: conf}}
	for _, w := range strings.Fields(res.Text) {
		res.Words = append(res.Words, ocr.Word{Text: w, Conf: conf})
	}
	return res, nil
}

// distinctPresentEmptyCrops counts the crops OCR actually runs on that show
// the box with no text (docs/spec/05 makes "empty text on a frame classified
// as textbox" unresolved). The pipeline's presence gate and crop dedupe decide
// which crops reach an engine, so the count comes from the truth rows that
// survive both: present, empty, and distinct on the RAW crop's hash — the
// identity the fallback's secondary cache keys on (ADR-0040, the 2026-10-18 amendment: the
// secondary's input is the raw crop, not the recipe's output).
func distinctPresentEmptyCrops(t *testing.T) int {
	t.Helper()
	dir := corpusDir(t)
	seen := map[string]bool{}
	for _, r := range readTruth(t) {
		if !r.Present || r.Text != "" {
			continue
		}
		img := decodeFrame(t, dir, r.File)
		crop := vision.CropNormalized(img, vision.Rect{X: r.Rect.X, Y: r.Rect.Y, W: r.Rect.W, H: r.Rect.H}, vision.CropOptions{})
		if crop == nil || crop.Bounds().Empty() {
			t.Fatalf("%s: rect %v crops nothing", r.File, r.Rect)
		}
		seen[vision.ImageHash(crop)] = true
	}
	return len(seen)
}

// TestOCRFallbackOptionOffByDefault pins the contract the runtime depends on:
// with the option unset (the vision_ocr_fallback role unconfigured), the
// configured engine runs as-is and no fallback machinery engages.
func TestOCRFallbackOptionOffByDefault(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	h := newHarness(t, eng, func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
	st, _, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, ok := h.runner.Config().OCR.(*ocr.FallbackEngine); ok {
		t.Fatal("the OCR field was wrapped although the option was unset")
	}
	if st.OCRFallbackFrames != 0 || st.OCRFallbackSecondaryCalls != 0 {
		t.Errorf("fallback counters ran (%d frames, %d calls) with the option unset", st.OCRFallbackFrames, st.OCRFallbackSecondaryCalls)
	}
}

// TestOCRFallbackOptionWrapsAndValidates checks the seam the runtime will use
// later: the option wraps Config.OCR with the FallbackEngine (readable back
// from Runner.Config()), and a missing secondary is a configuration fault,
// not a silent no-op.
func TestOCRFallbackOptionWrapsAndValidates(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	base := func() pipeline.Config {
		return pipeline.Config{
			SessionID:       "synthsession",
			ProfileRevision: "1",
			Regions:         []games.Region{corpusRegion(t)},
			OCR:             eng,
			Index:           index(t),
			Preprocess:      steps(),
			Queue:           pipeline.QueueLossless,
			Trace:           true,
		}
	}
	cfg := base()
	cfg.OCRFallback = &pipeline.OCRFallback{Secondary: nil}
	if _, err := pipeline.New(cfg); err == nil {
		t.Fatal("a fallback option without a secondary must be rejected")
	}
	cfg = base()
	cfg.OCRFallback = &pipeline.OCRFallback{Secondary: &countingSecondary{text: lineA}}
	r, err := pipeline.New(cfg)
	if err != nil {
		t.Fatalf("New with the option set: %v", err)
	}
	wrapped, ok := r.Config().OCR.(*ocr.FallbackEngine)
	if !ok {
		t.Fatalf("Config().OCR = %T, want *ocr.FallbackEngine", r.Config().OCR)
	}
	if wrapped.Primary != eng {
		t.Error("the wrapped primary is not the configured engine")
	}
}

// TestOCRFallbackResolvedNeverCalls is the cost property: a resolved local
// read costs exactly what it costs today — no crop prepared, nothing sent.
// The clip's only unresolved reads were its blank-panel crops (truth: box
// present, no text). Since the ADR-0137 sea amendment the presence gate
// requires a text share, so a blank panel reads absent and never reaches OCR
// or the VLM: the clip sends nothing at all. (A panel that carries strokes
// the local engine cannot read still reaches the VLM — docs/spec/05's case —
// and the resolving tests below exercise it.)
func TestOCRFallbackResolvedNeverCalls(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	sec := &countingSecondary{text: lineA}
	h := newHarness(t, eng, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.OCRFallback = &pipeline.OCRFallback{
			Secondary: sec,
			Policy:    ocr.FallbackPolicy{MaxSide: 512},
		}
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := distinctPresentEmptyCrops(t); n == 0 {
		t.Fatal("the clip has no blank-panel crops: the property below would be vacuous")
	}
	want := 0 // blank panels read absent (ADR-0137 sea amendment); resolved reads never send
	if got := sec.calls.Load(); got != int32(want) {
		t.Errorf("the VLM was called %d times, want %d (blank panels read absent; every non-empty read is resolved and never sends anything)", got, want)
	}
	if st.OCRFallbackFrames != uint64(want) || st.OCRFallbackSecondaryCalls != uint64(want) {
		t.Errorf("fallback counters = %d frames / %d calls, want %d/%d", st.OCRFallbackFrames, st.OCRFallbackSecondaryCalls, want, want)
	}
	// The wrap must not change what the session decides.
	if got := committed(t, evs); len(got) != 2 || got[0] != msgIDLineA || got[1] != msgIDLineB {
		t.Errorf("committed = %v, want both lines", got)
	}
}

// TestOCRFallbackUnresolvedEngages is the engagement property end to end:
// every present frame is unresolved, so the fallback runs on the whole clip;
// the counters agree with the engine's own call log, the trace rows carry the
// ocr.fallback field, and the session still commits both lines.
func TestOCRFallbackUnresolvedEngages(t *testing.T) {
	script := ocrScript(t)
	prim := &lowConfPrimary{script: script}
	// The secondary reads the RAW crop (ocr.RawCropRecognizer, ADR-0040, the 2026-10-18 amendment),
	// so its truth is keyed on the raw crop's hash, not the recipe's output.
	sec := &scriptSecondary{script: ocrRawScript(t)}
	h := newHarness(t, prim, func(c *pipeline.Config) {
		c.Mode = pipeline.ModeReplay
		c.OCRFallback = &pipeline.OCRFallback{
			Secondary: sec,
			Policy: ocr.FallbackPolicy{
				ConfidenceThreshold: 0.6,
				MaxSide:             512,
				// Generous budget: this test exercises the engagement, not the
				// rate limit (the ocr package unit tests own that behaviour).
				RatePerSecond: 1000,
				RateBurst:     1000,
			},
		}
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sec.calls.Load() == 0 {
		t.Fatal("the VLM was never called although every read was unresolved")
	}
	if st.OCRFallbackSecondaryCalls != uint64(sec.calls.Load()) {
		t.Errorf("Stats.OCRFallbackSecondaryCalls = %d disagrees with the engine's own count %d", st.OCRFallbackSecondaryCalls, sec.calls.Load())
	}
	if st.OCRFallbackFrames < st.OCRFallbackSecondaryCalls {
		t.Errorf("engaged frames (%d) below secondary calls (%d): a call was counted twice or a row lost", st.OCRFallbackFrames, st.OCRFallbackSecondaryCalls)
	}
	// The trace row carries the ocr.fallback field on an engaged frame.
	sawRow := false
	for _, e := range h.runner.Trace() {
		if e.OCRFallback == nil {
			continue
		}
		sawRow = true
		if e.OCRFallback.Chosen != "primary" && e.OCRFallback.Chosen != "secondary" {
			t.Errorf("trace ocr.fallback.chosen = %q, want primary or secondary", e.OCRFallback.Chosen)
		}
		if e.OCRFallback.Reason == "" {
			t.Error("trace ocr.fallback.reason is empty")
		}
		break
	}
	if !sawRow {
		t.Error("no trace row carried the ocr.fallback field")
	}
	// The fallback must not break what the session decides.
	if got := committed(t, evs); len(got) != 2 || got[0] != msgIDLineA || got[1] != msgIDLineB {
		t.Errorf("committed = %v, want both lines", got)
	}
	timeline(t, h.runner)
}
