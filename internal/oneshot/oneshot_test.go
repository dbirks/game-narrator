package oneshot

import (
	"context"
	"errors"
	"image"
	"image/color"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// The tests run the one-shot path over synthetic frames generated in-test
// (AGENTS.md rule 5: no real game frames in the repo) with a scripted engine
// (class (a) synthetic stub — the engine seam is the product's own
// ocr.Engine interface, and the real Tesseract binary is exercised by the
// hardware gate and the cmd-level test where a real read is the point).

// fakeEngine is a scripted ocr.Engine that records what it was handed, so a
// test can assert the crop/preprocess the one-shot path prepared — the same
// assertions the pipeline's harness makes against its own ROI stage.
type fakeEngine struct {
	name    string
	text    string
	conf    float64
	fail    error
	calls   int
	lastIn  image.Image
	lastRaw image.Image
}

func (f *fakeEngine) Name() string { return f.name }

func (f *fakeEngine) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	f.calls++
	f.lastIn = img
	if f.fail != nil {
		return ocr.Result{}, f.fail
	}
	return ocr.Result{
		Text:     f.text,
		MeanConf: f.conf,
		Engine:   f.name,
		Duration: 100 * 1e6, // a fixed 100 ms so a test can assert the timing surfaced
	}, nil
}

// rawFakeEngine additionally implements ocr.RawCropRecognizer (the
// ADR-0040 two-input shape) and records both hands.
type rawFakeEngine struct {
	fakeEngine
	gotRaw bool
}

func (f *rawFakeEngine) RecognizeWithRawCrop(_ context.Context, preprocessed, raw image.Image) (ocr.Result, error) {
	f.gotRaw = raw != nil && !raw.Bounds().Empty()
	f.lastRaw = raw
	return f.fakeEngine.Recognize(context.Background(), preprocessed)
}

// letterboxedFrame builds a 640×360 canvas with 80 px black bars left and
// right and a bright picture between them, with a dark panel (the synthetic
// dialogue box) in the lower half of the picture. At 640×360 the bar scan's
// stride is 1 (640/512), so the detection is exact.
func letterboxedFrame() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			var c color.Color
			switch {
			case x < 80 || x >= 560:
				c = color.Black
			case y >= 200 && x >= 130 && x < 510:
				c = color.Gray{Y: 20} // the dark panel
			default:
				c = color.Gray{Y: 200}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// flatFrame builds a 640×480 (4:3) canvas, so the bar scan does not run
// (aspect ≤ minWideAspect) and the letterbox answer is the whole frame — the
// shape a profile's regions are calibrated against.
func flatFrame() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.Gray{Y: 200})
		}
	}
	// A dark panel in the lower left: present in the region that covers it,
	// absent in one that does not.
	for y := 240; y < 432; y++ {
		for x := 32; x < 300; x++ {
			img.Set(x, y, color.Gray{Y: 20})
		}
	}
	return img
}

func TestRunNoGameReadsWholeActivePicture(t *testing.T) {
	img := letterboxedFrame()
	eng := &fakeEngine{name: "fake", text: "hello there", conf: 0.9}

	rep := Run(context.Background(), img, Options{Source: "image:test.png", Engine: eng})
	if len(rep.Regions) != 1 {
		t.Fatalf("regions = %d, want 1 (the fallback region)", len(rep.Regions))
	}
	r := rep.Regions[0]
	if r.ID != FallbackRegionID || r.Label != FallbackRegionLabel {
		t.Fatalf("fallback region = %q/%q, want %q/%q", r.ID, r.Label, FallbackRegionID, FallbackRegionLabel)
	}
	if r.RectX != 0 || r.RectY != 0 || r.RectW != 1 || r.RectH != 1 {
		t.Fatalf("fallback rect = [%.2f %.2f %.2f %.2f], want [0 0 1 1]", r.RectX, r.RectY, r.RectW, r.RectH)
	}
	// The crop must be the active picture: the 80 px bars trimmed off both
	// sides, the full 360 height (no vertical bars in this frame).
	want := image.Rect(80, 0, 560, 360)
	if got := image.Rect(r.CropX, r.CropY, r.CropX+r.CropW, r.CropY+r.CropH); got != want {
		t.Fatalf("crop = %v, want the active picture %v", got, want)
	}
	if r.Text != "hello there" || r.MeanConf != 0.9 || r.Engine != "fake" {
		t.Fatalf("read = %q conf=%.2f engine=%q, want the engine's answer", r.Text, r.MeanConf, r.Engine)
	}
	if r.Duration != 100*1e6 {
		t.Fatalf("duration = %v, want the engine's reported duration", r.Duration)
	}
	if !r.Present {
		t.Fatal("presence = false on the frame with a dark panel; the panel detector should read it")
	}
	if eng.calls != 1 {
		t.Fatalf("engine called %d times, want 1", eng.calls)
	}
	if eng.lastIn == nil || eng.lastIn.Bounds().Dx() <= want.Dx() {
		t.Fatalf("engine input bounds = %v: the preprocessed input must be the 2× recipe output, larger than the crop",
			boundsOrEmpty(eng.lastIn))
	}
	if rep.Source != "image:test.png" {
		t.Fatalf("report source = %q, want the stamped provenance line", rep.Source)
	}
}

func TestRunProfileRegionsCropLikeThePipeline(t *testing.T) {
	img := flatFrame()
	// Region A covers the dark panel (a textbox); region B is the bright
	// upper right, where no box is.
	regions := []games.Region{
		{ID: "reg-a", Label: "dialogue", RectX: 0.05, RectY: 0.5, RectW: 0.9, RectH: 0.4},
		{ID: "reg-b", Label: "hud", RectX: 0.6, RectY: 0.05, RectW: 0.3, RectH: 0.2},
	}
	eng := &fakeEngine{name: "fake", text: "read text", conf: 0.8}

	rep := Run(context.Background(), img, Options{Source: "image:test.png", Engine: eng, Regions: regions})
	if len(rep.Regions) != 2 {
		t.Fatalf("regions = %d, want 2", len(rep.Regions))
	}
	a := rep.Regions[0]
	// MapRect of {0.05,0.5,0.9,0.4} on a 640×480 frame: x 0.05·640=32 ..
	// 0.95·640=608, y 0.5·480=240 .. 0.9·480=432.
	if got := image.Rect(a.CropX, a.CropY, a.CropX+a.CropW, a.CropY+a.CropH); got != image.Rect(32, 240, 608, 432) {
		t.Fatalf("region A crop = %v, want (32,240,608,432) — the exact MapRect the pipeline's analyze computes", got)
	}
	if !a.Present {
		t.Fatal("region A presence = false: the dark panel fills most of the crop")
	}
	b := rep.Regions[1]
	if got := image.Rect(b.CropX, b.CropY, b.CropX+b.CropW, b.CropY+b.CropH); got != image.Rect(384, 24, 576, 120) {
		t.Fatalf("region B crop = %v, want (384,24,576,120)", got)
	}
	if b.Present {
		t.Fatal("region B presence = true on the bright frame area; the panel detector should read it absent")
	}
	if eng.calls != 2 {
		t.Fatalf("engine called %d times, want one per region", eng.calls)
	}
}

func TestRunBorderedProfileRegionMapsThroughTheSettledWindow(t *testing.T) {
	// ADR-0082 amendment 1: the one-shot reader settles the active-picture
	// window from the frame itself (Tracker.Settle), so a profile's stored
	// region — fractions of the active picture — maps through the window,
	// not the canvas. The frame is the existing 640×360 letterbox fixture
	// (80 px bars left and right, active picture (80,0)-(560,360)); the
	// region {0.1,0.5,0.8,0.4} of the active picture is (128,180)-(512,324).
	// The historic whole-canvas mapping would be (64,180)-(576,324): inside
	// the black bars on the left, exactly the mis-placement the tracker
	// exists to remove.
	img := letterboxedFrame()
	regions := []games.Region{{ID: "box", Label: "dialogue", RectX: 0.1, RectY: 0.5, RectW: 0.8, RectH: 0.4}}
	eng := &fakeEngine{name: "fake", text: "a line", conf: 0.9}
	rep := Run(context.Background(), img, Options{Engine: eng, Regions: regions})
	if len(rep.Regions) != 1 {
		t.Fatalf("regions = %d, want 1", len(rep.Regions))
	}
	got := image.Rect(rep.Regions[0].CropX, rep.Regions[0].CropY, rep.Regions[0].CropX+rep.Regions[0].CropW, rep.Regions[0].CropY+rep.Regions[0].CropH)
	want := image.Rect(128, 180, 512, 324)
	if got != want {
		t.Fatalf("region crop = %v, want %v — the MapRect of the region through the settled window, not the whole canvas", got, want)
	}
	if eng.calls != 1 {
		t.Fatalf("engine called %d times, want 1", eng.calls)
	}
}

func TestRunEmptyCropIsReportedNotRun(t *testing.T) {
	// A zero-width rect covers nothing: the read must be reported as empty,
	// the engine never called, and nothing panicked (docs/spec/05 "a
	// mis-calibrated region degrades into read no text").
	eng := &fakeEngine{name: "fake", text: "nope"}
	regions := []games.Region{{ID: "reg-zero", RectW: 0}}
	rep := Run(context.Background(), flatFrame(), Options{Engine: eng, Regions: regions})
	if len(rep.Regions) != 1 || !rep.Regions[0].Empty {
		t.Fatalf("region report = %+v, want one empty region", rep.Regions)
	}
	if eng.calls != 0 {
		t.Fatalf("engine called %d times on an empty crop, want 0", eng.calls)
	}
}

func TestRunNilImageReadsEmptyWithoutPanicking(t *testing.T) {
	rep := Run(context.Background(), nil, Options{Engine: &fakeEngine{name: "fake"}})
	if len(rep.Regions) != 1 || !rep.Regions[0].Empty {
		t.Fatalf("nil image report = %+v, want one empty region", rep.Regions)
	}
}

func TestRunEngineFailureIsPerRegion(t *testing.T) {
	img := flatFrame()
	// Two regions, one engine that fails: the failure is reported for the
	// read without dropping the report or the other region's evidence.
	regions := []games.Region{
		{ID: "reg-a", RectX: 0.05, RectY: 0.5, RectW: 0.9, RectH: 0.4},
		{ID: "reg-b", RectX: 0.6, RectY: 0.05, RectW: 0.3, RectH: 0.2},
	}
	failed := errors.New("engine exploded")
	eng := &fakeEngine{name: "fake", fail: &ocr.Failure{Kind: failed}}
	rep := Run(context.Background(), img, Options{Engine: eng, Regions: regions})
	for i, r := range rep.Regions {
		if !strings.Contains(r.OCRError, "engine exploded") {
			t.Fatalf("region %d: OCRError = %q, want the engine's failure named", i, r.OCRError)
		}
		if r.Text != "" {
			t.Fatalf("region %d: text %q on a failed read, want empty", i, r.Text)
		}
	}
}

// TestRunRanksAgainstTheIndex drives the matcher half of the path: the raw
// text is folded and ranked against a synthetic corpus, and the top-k bound
// is honoured.
func TestRunRanksAgainstTheIndex(t *testing.T) {
	msgs := []games.Message{
		{ID: "msg-1", OriginalText: "Where does the sea wind blow to day", NormalizedText: "where does the sea wind blow to day"},
		{ID: "msg-2", OriginalText: "I shall be gone by morning", NormalizedText: "i shall be gone by morning"},
		{ID: "msg-3", OriginalText: "The bridge is broken beyond repair", NormalizedText: "the bridge is broken beyond repair"},
	}
	idx, err := matcher.Build(msgs, nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	eng := &fakeEngine{name: "fake", text: "where does the sea wind blow to day", conf: 0.95}

	rep := Run(context.Background(), flatFrame(), Options{
		Engine:  eng,
		Regions: []games.Region{{ID: "reg", RectX: 0.05, RectY: 0.5, RectW: 0.9, RectH: 0.4}},
		Index:   idx,
	})
	r := rep.Regions[0]
	if r.Rank == nil {
		t.Fatalf("rank = nil, want the matcher's answer")
	}
	if len(r.Rank.Candidates) == 0 {
		t.Fatalf("rank has no candidates: %+v", r.Rank)
	}
	if top := r.Rank.Candidates[0]; top.MessageID != "msg-1" {
		t.Fatalf("top candidate = %q, want msg-1 (the read matches its text)", top.MessageID)
	}
	if top := r.Rank.Candidates[0]; top.Probability <= 0 || top.Probability > 1 {
		t.Fatalf("top probability = %v, want in (0,1]", top.Probability)
	}
	// The normalized field is the matcher's fold of the raw text — the same
	// key the ranking was computed over (the fold strips the period the
	// engine's spelling and the stored text do not agree on).
	if r.Normalized == "" || r.Normalized == r.Text {
		t.Fatalf("normalized = %q (raw %q): want the folded key, visibly folded", r.Normalized, r.Text)
	}
}

func TestRunTopKBoundHonoured(t *testing.T) {
	msgs := make([]games.Message, 0, 6)
	for i := 0; i < 6; i++ {
		msgs = append(msgs, games.Message{
			ID:             string(rune('a'+i)) + "-msg",
			OriginalText:   "common opening line for rank bound " + string(rune('1'+i)),
			NormalizedText: "common opening line for rank bound " + string(rune('1'+i)),
		})
	}
	idx, err := matcher.Build(msgs, nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	eng := &fakeEngine{name: "fake", text: "common opening line for rank bound"}

	rep := Run(context.Background(), flatFrame(), Options{
		Engine:  eng,
		Regions: []games.Region{{ID: "reg", RectX: 0.05, RectY: 0.5, RectW: 0.9, RectH: 0.4}},
		Index:   idx,
		RankOptions: matcher.RankOptions{
			TopK:    3,
			Weights: matcher.DefaultWeights(),
		},
	})
	r := rep.Regions[0]
	if r.Rank == nil || len(r.Rank.Candidates) > 3 {
		t.Fatalf("rank = %+v, want at most 3 candidates (TopK bound)", r.Rank)
	}
	if r.Rank == nil {
		t.Fatal("rank = nil with a matching read and an index")
	}
}

// TestRunRawCropEngineGetsBothInputs proves the ADR-0040 shape: an engine
// that implements ocr.RawCropRecognizer is handed the preprocessed crop and
// the raw crop, exactly as the pipeline's ocrRecognize hands them.
func TestRunRawCropEngineGetsBothInputs(t *testing.T) {
	img := flatFrame()
	eng := &rawFakeEngine{fakeEngine: fakeEngine{name: "raw-fake", text: "text", conf: 0.9}}
	regions := []games.Region{{ID: "reg", RectX: 0.05, RectY: 0.5, RectW: 0.9, RectH: 0.4}}

	rep := Run(context.Background(), img, Options{Engine: eng, Regions: regions})
	if !eng.gotRaw {
		t.Fatal("the raw crop was not handed to the RawCropRecognizer engine")
	}
	// The raw crop is the exact pixels of region A's crop at 1× — the
	// pipeline's "actual pixels" (ADR-0040), not the recipe's output. The
	// copy's origin is (0,0), so size and pixel identity are what is
	// asserted against the frame box (32,240)-(608,432).
	wantRaw := image.Rect(32, 240, 608, 432)
	if got := eng.lastRaw.Bounds().Size(); got != (image.Point{X: 576, Y: 192}) {
		t.Fatalf("raw crop size = %v, want the frame box size %dx%d", got, wantRaw.Dx(), wantRaw.Dy())
	}
	for _, at := range []image.Point{{X: 100, Y: 100}, {X: 400, Y: 150}} {
		got := color.GrayModel.Convert(eng.lastRaw.At(at.X, at.Y)).(color.Gray)
		want := color.GrayModel.Convert(img.At(wantRaw.Min.X+at.X, wantRaw.Min.Y+at.Y)).(color.Gray)
		if got != want {
			t.Fatalf("raw crop pixel at %v = %v, want the frame's %v (the actual pixels)", at, got, want)
		}
	}
	// The preprocessed input is the recipe's output: 2× scale and padding
	// make it strictly larger than the raw crop.
	if eng.lastIn.Bounds().Dx() <= wantRaw.Dx() {
		t.Fatalf("preprocessed input %v is not larger than the raw crop %v: the 2× recipe must have run",
			eng.lastIn.Bounds(), wantRaw)
	}
	if rep.Regions[0].Text != "text" {
		t.Fatalf("text = %q, want the engine's answer", rep.Regions[0].Text)
	}
}

// TestRunCancelledContextStopsTheRun: the caller's context decides the run;
// a cancelled context is reported per region, never panicked, and the
// remaining regions are not spent on.
func TestRunCancelledContextStopsTheRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	eng := &fakeEngine{name: "fake", text: "late"}
	regions := []games.Region{
		{ID: "reg-a", RectX: 0.05, RectY: 0.5, RectW: 0.9, RectH: 0.4},
		{ID: "reg-b", RectX: 0.6, RectY: 0.05, RectW: 0.3, RectH: 0.2},
	}
	rep := Run(ctx, flatFrame(), Options{Engine: eng, Regions: regions})
	if eng.calls > 1 {
		t.Fatalf("engine called %d times after the caller cancelled, want at most 1", eng.calls)
	}
	if len(rep.Regions) > 1 {
		t.Fatalf("regions = %d after cancellation, want the run to stop", len(rep.Regions))
	}
}

func boundsOrEmpty(img image.Image) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	return img.Bounds()
}
