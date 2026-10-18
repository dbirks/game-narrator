package ocr

import (
	"context"
	"errors"
	"image"
	"sync/atomic"
	"testing"
	"time"
)

// scriptedPrimary is a fake local engine: it answers from a map keyed by image
// pointer, so one test image is one "crop" and a distinct image is a distinct
// crop. The map value is the Result or an error.
type scriptedPrimary struct {
	results map[image.Image]resultOrErr
	calls   atomic.Int64
}

type resultOrErr struct {
	res Result
	err error
}

func (s *scriptedPrimary) Name() string { return "scripted" }

func (s *scriptedPrimary) Recognize(_ context.Context, img image.Image) (Result, error) {
	s.calls.Add(1)
	ro, ok := s.results[img]
	if !ok {
		return Result{}, errors.New("scripted: no script for this image")
	}
	return ro.res, ro.err
}

// vlmSecondary is a VLMEngine over a fake client; the test reads the fake's
// call log to assert on "how many remote calls actually went out".
func vlmSecondary(answer string) (*VLMEngine, *fakeVLMClient) {
	c := &fakeVLMClient{answer: answer}
	return NewVLMEngine(c, WithMaxSide(512)), c
}

// newFallback builds a FallbackEngine with a pinned clock and generous
// defaults, then applies the caller's overrides on top: rate-limit and cache
// tests pass exactly the fields they exercise, everything else stays default.
func newFallback(t *testing.T, primary, secondary Engine, p FallbackPolicy) *FallbackEngine {
	t.Helper()
	if p.ConfidenceThreshold <= 0 {
		p.ConfidenceThreshold = 0.6
	}
	if p.MaxSide <= 0 {
		p.MaxSide = 512
	}
	if p.RatePerSecond <= 0 {
		p.RatePerSecond = 1000 // generous: rate-limit tests set their own
	}
	if p.RateBurst < 1 {
		p.RateBurst = 1000
	}
	if p.MaxCache < 1 {
		p.MaxCache = 64
	}
	p.Now = func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }
	return &FallbackEngine{Primary: primary, Secondary: secondary, Policy: p}
}

func TestFallbackResolvedPrimaryNeverCallsVLM(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "The harbour bell rang twice.", MeanConf: 0.93, Engine: "scripted"},
	}}}
	sec, c := vlmSecondary("irrelevant")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	if c.calls != 0 {
		t.Fatalf("VLM was called %d times on a resolved read, want 0", c.calls)
	}
	if res.Fallback != nil {
		t.Errorf("resolved read carries a Fallback row %+v, want none", res.Fallback)
	}
	if res.MeanConf != 0.93 || res.Engine != "scripted" {
		t.Errorf("resolved read was modified: %+v", res)
	}
}

// recordingSecondary is a plain Engine (not a PreparedRecognizer) that logs
// the exact image it was handed, so a test can assert WHICH pixels the
// fallback sent the secondary (ADR-0040, the 2026-10-18 amendment) rather than only that a call went
// out.
type recordingSecondary struct {
	calls  atomic.Int64
	images []image.Image
}

func (s *recordingSecondary) Name() string { return "recording" }
func (s *recordingSecondary) Recognize(_ context.Context, img image.Image) (Result, error) {
	s.calls.Add(1)
	s.images = append(s.images, img)
	return Result{Text: "The harbour bell rang twice.", MeanConf: 0.6, Engine: "recording"}, nil
}

// TestFallbackRawCropPath pins ADR-0040, the 2026-10-18 amendment at the engine level: the
// dual-input path runs the primary on the recipe's preprocessed image and the
// secondary on the raw crop — the actual pixels — and the cache is keyed on
// the prepared RAW bytes, so a stable raw crop whose preprocessed input drifts
// (the recipe's own variance) still costs exactly one remote call.
func TestFallbackRawCropPath(t *testing.T) {
	raw, raw2 := solidImage(300, 80), solidImage(301, 80)
	pre, pre2 := solidImage(301, 81), solidImage(301, 82) // the recipe's output: distinct images
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{
		pre:  {res: Result{Text: "garbled", MeanConf: 0.2, Engine: "scripted"}},
		pre2: {res: Result{Text: "garbled", MeanConf: 0.2, Engine: "scripted"}},
	}}
	sec := &recordingSecondary{}
	f := newFallback(t, primary, sec, FallbackPolicy{})

	// Frame 1: both inputs are fresh. The secondary must receive the RAW
	// crop, not the recipe's output — that is the whole point of the path.
	res, err := f.RecognizeWithRawCrop(context.Background(), pre, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback == nil || res.Fallback.Chosen != "secondary" {
		t.Fatalf("first read = %+v, want the secondary's reading chosen (primary conf 0.2)", res.Fallback)
	}
	if len(sec.images) != 1 || sec.images[0] != image.Image(raw) {
		t.Fatalf("the secondary saw %d image(s) %v, want exactly the raw crop", len(sec.images), sec.images)
	}
	if primary.calls.Load() != 1 {
		t.Fatalf("primary called %d times, want 1", primary.calls.Load())
	}

	// Frame 2: the raw crop is the same pixels, the recipe's output drifted
	// (pre2). The cache key is the prepared RAW bytes — unchanged — so the
	// secondary is not called again and the cached reading serves the frame.
	res, err = f.RecognizeWithRawCrop(context.Background(), pre2, raw)
	if err != nil {
		t.Fatal(err)
	}
	if res.Fallback == nil || res.Fallback.Reason != "secondary_cached" {
		t.Fatalf("second read reason = %+v, want secondary_cached (raw crop unchanged, recipe drifted)", res.Fallback)
	}
	if sec.calls.Load() != 1 {
		t.Fatalf("secondary called %d times, want 1 (cache keyed on the prepared RAW bytes, not the recipe's)", sec.calls.Load())
	}

	// Frame 3: the raw crop changes (a new typewriter state) → a fresh remote
	// call, and it is the new raw pixels the secondary sees.
	res, err = f.RecognizeWithRawCrop(context.Background(), pre, raw2)
	if err != nil {
		t.Fatal(err)
	}
	if sec.calls.Load() != 2 {
		t.Fatalf("secondary called %d times after the raw crop changed, want 2", sec.calls.Load())
	}
	if len(sec.images) != 2 || sec.images[1] != image.Image(raw2) {
		t.Fatalf("the second call saw %v, want the new raw crop", sec.images)
	}
	_ = res
}

// TestFallbackRawCropEmptyRawDegrades pins the guard: a nil or empty raw crop
// degrades to the single-input behaviour (both readers see the preprocessed
// input) instead of sending an empty crop to a remote model.
func TestFallbackRawCropEmptyRawDegrades(t *testing.T) {
	pre := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{pre: {
		res: Result{Text: "garbled", MeanConf: 0.2, Engine: "scripted"},
	}}}
	sec := &recordingSecondary{}
	f := newFallback(t, primary, sec, FallbackPolicy{})
	if _, err := f.RecognizeWithRawCrop(context.Background(), pre, nil); err != nil {
		t.Fatal(err)
	}
	if len(sec.images) != 1 || sec.images[0] != image.Image(pre) {
		t.Fatalf("nil raw crop: the secondary saw %v, want the preprocessed input (degraded single-input behaviour)", sec.images)
	}
	if _, err := f.RecognizeWithRawCrop(context.Background(), pre, image.Image(image.NewRGBA(image.Rectangle{}))); err != nil {
		t.Fatal(err)
	}
	// The degraded path prepares the same preprocessed bytes the first call
	// cached: the cache hits and no second remote call goes out.
	if sec.calls.Load() != 1 {
		t.Fatalf("empty raw crop: secondary called %d times, want 1 (the degraded path caches on the preprocessed bytes like any single-input read)", sec.calls.Load())
	}
}

func TestFallbackUnresolvedCallsVLMOncePerCropHash(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "The harbo… bell rang.", MeanConf: 0.2, Engine: "scripted"},
	}}}
	sec, c := vlmSecondary("The harbour bell rang twice.")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	for i := 0; i < 6; i++ {
		res, err := f.Recognize(context.Background(), img)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if i == 0 {
			if res.Fallback == nil {
				t.Fatal("unresolved read has no Fallback row")
			}
			if res.Fallback.Reason != "primary_below_threshold" {
				t.Errorf("reason = %q, want primary_below_threshold", res.Fallback.Reason)
			}
			if res.Fallback.Chosen != "secondary" {
				t.Errorf("chosen = %q, want secondary (primary conf 0.2 < 0.6)", res.Fallback.Chosen)
			}
		} else {
			if res.Fallback.Reason != "secondary_cached" {
				t.Errorf("frame %d reason = %q, want secondary_cached", i, res.Fallback.Reason)
			}
		}
		if res.Text != "The harbour bell rang twice." {
			t.Errorf("frame %d text = %q", i, res.Text)
		}
	}
	if c.calls != 1 {
		t.Fatalf("VLM called %d times over six identical crops, want 1 (crop-hash cache)", c.calls)
	}
	if primary.calls.Load() != 6 {
		t.Errorf("primary called %d times, want 6 (it is local and free)", primary.calls.Load())
	}
}

func TestFallbackDifferentCropCallsVLMAgain(t *testing.T) {
	a, b := solidImage(300, 80), solidImage(301, 80) // one pixel of noise = a new crop
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{
		a: {res: Result{Text: "a", MeanConf: 0.2, Engine: "scripted"}},
		b: {res: Result{Text: "b", MeanConf: 0.2, Engine: "scripted"}},
	}}
	sec, c := vlmSecondary("The second page.")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	if _, err := f.Recognize(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Recognize(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if c.calls != 2 {
		t.Fatalf("VLM called %d times over two distinct crops, want 2", c.calls)
	}
}

func TestFallbackRateLimit(t *testing.T) {
	// 1/s, burst 3: three distinct crops at the same instant spend the burst;
	// the fourth is rate-limited without an error (the local read stands), and
	// a fifth one a second later is served again.
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{}}
	var img *image.RGBA
	mk := func() *image.RGBA {
		img = solidImage(300+int(primary.calls.Load()), 80)
		primary.results[img] = resultOrErr{res: Result{Text: "x", MeanConf: 0.2, Engine: "scripted"}}
		return img
	}
	sec, c := vlmSecondary("rescued")
	nowAt := t0
	f := &FallbackEngine{
		Primary:   primary,
		Secondary: sec,
		Policy: FallbackPolicy{
			ConfidenceThreshold: 0.6,
			MaxSide:             512,
			RatePerSecond:       1,
			RateBurst:           3,
			Now:                 func() time.Time { return nowAt },
		},
	}
	for i := 0; i < 3; i++ {
		if _, err := f.Recognize(context.Background(), mk()); err != nil {
			t.Fatalf("burst frame %d: %v", i, err)
		}
	}
	fourth := mk()
	res, err := f.Recognize(context.Background(), fourth)
	if err != nil {
		t.Fatalf("rate-limited frame must return the local read, not an error: %v", err)
	}
	if res.Fallback == nil || res.Fallback.Reason != "secondary_rate_limited" {
		t.Fatalf("fourth frame row = %+v, want reason secondary_rate_limited", res.Fallback)
	}
	if res.Text != "x" {
		t.Errorf("rate-limited frame text = %q, want the primary's own read", res.Text)
	}
	if c.calls != 3 {
		t.Fatalf("VLM called %d times, want 3 (burst) + 0 (rate-limited)", c.calls)
	}
	nowAt = t0.Add(2 * time.Second)
	if _, err := f.Recognize(context.Background(), mk()); err != nil {
		t.Fatalf("post-refill frame: %v", err)
	}
	if c.calls != 4 {
		t.Fatalf("VLM called %d times after the refill, want 4", c.calls)
	}
}

func TestFallbackRateLimitOnErroredPrimarySurfacesError(t *testing.T) {
	t0 := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	nowAt := t0
	// Primary always fails; burst 1: the first frame spends the only token on a
	// VLM answer that also says NONE, and the second frame is budgeted out with
	// nothing to stand on — the primary's error must surface.
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{}}
	img := solidImage(200, 60)
	primary.results[img] = resultOrErr{err: &Failure{Kind: ErrTimeout, Engine: "scripted"}}
	sec, c := vlmSecondary("NONE")
	f := &FallbackEngine{
		Primary:   primary,
		Secondary: sec,
		Policy: FallbackPolicy{
			ConfidenceThreshold: 0.6, MaxSide: 512,
			RatePerSecond: 0.0001, RateBurst: 1,
			Now: func() time.Time { return nowAt },
		},
	}
	if _, err := f.Recognize(context.Background(), img); err == nil {
		t.Fatal("first frame: expected the primary's error to surface (VLM read NONE)")
	}
	if _, err := f.Recognize(context.Background(), img); err == nil {
		t.Fatal("second frame: expected the rate-limited failure to surface the primary's error")
	}
	if c.calls != 1 {
		t.Fatalf("VLM called %d times, want 1", c.calls)
	}
}

func TestFallbackNoneFromVLMKeepsPrimary(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "Maybe a word.", MeanConf: 0.3, Engine: "scripted"},
	}}}
	sec, c := vlmSecondary("NONE")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("a NONE answer is a reading, not an error: %v", err)
	}
	if res.Text != "Maybe a word." {
		t.Errorf("text = %q, want the primary's read standing when the VLM found nothing", res.Text)
	}
	if res.Fallback == nil || res.Fallback.Reason != "secondary_empty" {
		t.Fatalf("row = %+v, want reason secondary_empty", res.Fallback)
	}
	if res.Fallback.SecondaryText != "" {
		t.Errorf("SecondaryText = %q, want empty (the VLM said NONE)", res.Fallback.SecondaryText)
	}
	_ = c
}

func TestFallbackDiscrepancyAndChoice(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "the harbour bell rang twice", MeanConf: 0.3, Engine: "scripted"},
	}}}
	sec, _ := vlmSecondary("the harbour bell rang thrice")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	fb := res.Fallback
	if fb == nil {
		t.Fatal("no Fallback row")
	}
	want := normalizedDistance(NormaliseForCompare("the harbour bell rang twice"), NormaliseForCompare("the harbour bell rang thrice"))
	if fb.Discrepancy == 0 || fb.Discrepancy > 1 {
		t.Fatalf("discrepancy = %v, want in (0,1]", fb.Discrepancy)
	}
	if fb.Discrepancy != want {
		t.Errorf("discrepancy = %v, want %v (normalised edit distance of the two reads)", fb.Discrepancy, want)
	}
	if fb.Chosen != "secondary" {
		t.Errorf("chosen = %q, want secondary (primary below threshold)", fb.Chosen)
	}
	// The choice confidence is the prior scaled by the disagreement: worse
	// agreement, lower confidence — and it must stay below the prior.
	if res.MeanConf >= vlmConfidencePrior || res.MeanConf <= 0 {
		t.Errorf("MeanConf = %v, want the prior %v scaled down by %v", res.MeanConf, vlmConfidencePrior, fb.Discrepancy)
	}
}

func TestFallbackAgreementCorroboratesPrimary(t *testing.T) {
	// DifficultFont forces the secondary even on a resolved primary: this is
	// the "compare both and record the discrepancy" case docs/spec/05 names,
	// with the two readers agreeing across a punctuation/case surface.
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "The harbour bell rang twice.", MeanConf: 0.93, Engine: "scripted"},
	}}}
	sec, _ := vlmSecondary("the harbour bell rang twice")
	f := newFallback(t, primary, sec, FallbackPolicy{DifficultFont: true, ConfidenceThreshold: 0.6})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	fb := res.Fallback
	if fb == nil {
		t.Fatal("DifficultFont must still run the comparison and record the row")
	}
	if fb.Reason != "secondary_agreement" {
		t.Errorf("reason = %q, want secondary_agreement (NormaliseForCompare strips the period and case)", fb.Reason)
	}
	if fb.Chosen != "primary" {
		t.Errorf("chosen = %q, want primary (the local read is free; the VLM only corroborates)", fb.Chosen)
	}
	if fb.Discrepancy != 0 {
		t.Errorf("discrepancy = %v, want 0 for agreeing reads", fb.Discrepancy)
	}
	if res.MeanConf != corroborationConf {
		t.Errorf("MeanConf = %v, want the corroboration confidence %v", res.MeanConf, corroborationConf)
	}
	if res.MeanConf >= 1.0 {
		t.Errorf("MeanConf %v claims certainty", res.MeanConf)
	}
}

func TestFallbackConfidentPrimaryKeptOverVLMDisagreement(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "The harbour bell rang twice.", MeanConf: 0.93, Engine: "scripted"},
	}}}
	sec, _ := vlmSecondary("a completely different sentence")
	f := newFallback(t, primary, sec, FallbackPolicy{DifficultFont: true, ConfidenceThreshold: 0.6})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("Recognize: %v", err)
	}
	fb := res.Fallback
	if fb.Reason != "primary_confident" {
		t.Errorf("reason = %q, want primary_confident", fb.Reason)
	}
	if fb.Chosen != "primary" || res.Text != "The harbour bell rang twice." {
		t.Errorf("the confident local read must stand: chosen=%q text=%q", fb.Chosen, res.Text)
	}
	if fb.Discrepancy == 0 {
		t.Error("discrepancy = 0 for disagreeing reads, want > 0")
	}
}

func TestFallbackPrimaryErrorRescuedByVLM(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		err: &Failure{Kind: ErrTimeout, Engine: "scripted"},
	}}}
	sec, _ := vlmSecondary("The harbour bell rang twice.")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("the VLM rescued the frame, so no error: %v", err)
	}
	if res.Text != "The harbour bell rang twice." || res.Fallback.Chosen != "secondary" {
		t.Fatalf("rescue failed: %+v", res)
	}
	if res.Fallback.Reason != "primary_errored" {
		t.Errorf("reason = %q, want primary_errored", res.Fallback.Reason)
	}
	if res.MeanConf != vlmConfidencePrior {
		t.Errorf("MeanConf = %v, want the prior %v", res.MeanConf, vlmConfidencePrior)
	}
}

func TestFallbackPrimaryErrorAndVLMFailureSurfacesError(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		err: &Failure{Kind: ErrTimeout, Engine: "scripted"},
	}}}
	sec, _ := vlmSecondary("The harbour bell rang twice.")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	// Swap in a failing secondary for the call.
	f.Secondary = failingEngine{err: &Failure{Kind: ErrFailed, Engine: "vlm"}}
	_, err := f.Recognize(context.Background(), img)
	if err == nil {
		t.Fatal("both readers failed: the error must surface so Stats.OCRErrors counts it")
	}
}

func TestFallbackSecondaryFailureKeepsPrimary(t *testing.T) {
	img := solidImage(300, 80)
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "Maybe a word.", MeanConf: 0.3, Engine: "scripted"},
	}}}
	f := newFallback(t, primary, failingEngine{err: &Failure{Kind: ErrFailed, Engine: "vlm"}}, FallbackPolicy{})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("a dead VLM must not kill a local read: %v", err)
	}
	if res.Text != "Maybe a word." {
		t.Errorf("text = %q, want the primary's read", res.Text)
	}
	if res.Fallback.Reason != "secondary_failed" || res.Fallback.SecondaryReason == "" {
		t.Errorf("row = %+v, want reason secondary_failed with the cause", res.Fallback)
	}
	// A failed secondary must not be cached: the next frame retries the call.
	if _, err := f.Recognize(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	if res2, _ := f.Recognize(context.Background(), img); res2.Fallback.Reason != "secondary_failed" {
		t.Errorf("row = %+v: a failed call was cached", res2.Fallback)
	}
}

func TestFallbackEmptyBoxReadsEmpty(t *testing.T) {
	img := solidImage(300, 80)
	// Primary read the box and found nothing (empty page before the
	// typewriter); the VLM also says NONE: the read stays empty, and that is a
	// reading, not an error.
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{img: {
		res: Result{Text: "", MeanConf: 0, Engine: "scripted"},
	}}}
	sec, _ := vlmSecondary("NONE")
	f := newFallback(t, primary, sec, FallbackPolicy{})
	res, err := f.Recognize(context.Background(), img)
	if err != nil {
		t.Fatalf("an empty box is not an error: %v", err)
	}
	if res.Text != "" {
		t.Errorf("text = %q, want empty", res.Text)
	}
	if res.Fallback.Reason != "secondary_empty" {
		t.Errorf("reason = %q, want secondary_empty", res.Fallback.Reason)
	}
}

func TestFallbackCacheEvictionIsBounded(t *testing.T) {
	primary := &scriptedPrimary{results: map[image.Image]resultOrErr{}}
	sec, c := vlmSecondary("x")
	f := newFallback(t, primary, sec, FallbackPolicy{MaxCache: 4, MaxSide: 512})
	// Nine distinct crops through a 4-slot cache: every crop is a fresh key,
	// so the VLM is called nine times and the cache never grows past 4.
	for i := 0; i < 9; i++ {
		img := solidImage(300+i, 80)
		primary.results[img] = resultOrErr{res: Result{Text: "x", MeanConf: 0.2, Engine: "scripted"}}
		if _, err := f.Recognize(context.Background(), img); err != nil {
			t.Fatalf("crop %d: %v", i, err)
		}
	}
	if c.calls != 9 {
		t.Fatalf("VLM called %d times, want 9 (eviction must not serve stale keys)", c.calls)
	}
	f.mu.Lock()
	n := len(f.cache)
	f.mu.Unlock()
	if n > 4 {
		t.Errorf("cache holds %d entries, want ≤ 4", n)
	}
}

// --- helpers ---------------------------------------------------------------

type failingEngine struct{ err error }

func (f failingEngine) Name() string { return "failing" }
func (f failingEngine) Recognize(context.Context, image.Image) (Result, error) {
	return Result{}, f.err
}

// normalizedDistanceSanity pins the metric's own contract: symmetry, the
// empty-side ratio, and the identity case.
func TestNormalizedDistanceSanity(t *testing.T) {
	if got := normalizedDistance("", ""); got != 0 {
		t.Errorf("empty/empty = %v, want 0", got)
	}
	if got := normalizedDistance("abc", ""); got != 1 {
		t.Errorf("abc/empty = %v, want 1", got)
	}
	if got := normalizedDistance("same", "same"); got != 0 {
		t.Errorf("same/same = %v, want 0", got)
	}
	ab := normalizedDistance("abcd", "abce")
	ba := normalizedDistance("abce", "abcd")
	if ab != ba {
		t.Errorf("distance not symmetric: %v vs %v", ab, ba)
	}
	if ab == 0 || ab > 1 {
		t.Errorf("abcd/abce = %v, want in (0,1]", ab)
	}
	if got := normalizedDistance("abcd", "efgh"); got != 1 {
		t.Errorf("abcd/efgh = %v, want 1 (no common characters)", got)
	}
}

func TestNormaliseForCompare(t *testing.T) {
	cases := map[string]string{
		"The Harbour Bell!": "theharbourbell",
		"the harbour bell…": "theharbourbell",
		"THE HARBOUR BELL":  "theharbourbell",
		"harbour bell":      "harbourbell",
		"abc123":            "abc123",
		"":                  "",
	}
	for in, want := range cases {
		if got := NormaliseForCompare(in); got != want {
			t.Errorf("NormaliseForCompare(%q) = %q, want %q", in, got, want)
		}
	}
	// Case-insensitive equality is the whole point of the form.
	if NormaliseForCompare("Hello, World!") != NormaliseForCompare("hello world") {
		t.Error("the two forms must compare equal")
	}
}
