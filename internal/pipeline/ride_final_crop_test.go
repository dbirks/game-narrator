package pipeline_test

// Riding a read of different pixels must not mark the rider's pixels as read
// (2026-10-23, local Dolphin run, TestLiveCatchUpRapidDialogue): a typewriter
// box finished its line while the read of the PREFIX crop was still in
// flight. The final crop was inside the perceptual tolerance of the prefix, so
// it RODE the in-flight read (p.job = rs.inflight) and got the prefix's text.
// Every later frame was pixel-identical to the final crop, so the dedupe memory
// treated those pixels as "already read" forever and the region emitted
// NOTHING for 45 s, although the box sat fully typed on screen and Tesseract
// read the final crop correctly when given it. The final pixels were never
// recognised.
//
// The clip below is the synthetic shape of that run: a static box showing the
// prefix crop, then the FINAL crop (same box, a few more characters: different
// exact hash, inside the perceptual tolerance) repeated identically. The
// prefix read is still in flight when the final crop first arrives, so it
// rides; once the prefix read settles, the final crop must get exactly ONE
// read of its own, and the identical crops after that read must skip (no OCR
// storm).

import (
	"context"
	"encoding/json"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision"
)

const (
	rideFrames   = 16
	rideInterval = 100 * time.Millisecond
	// The prefix is on screen through frame 2; the final crop from frame 3 on.
	// The source holds delivery after frame 2 until the prefix read settles, so
	// "the final crop repeats identically" is the run's tail, not a race.
	ridePrefixLast = 2
	rideFinalFirst = 3
)

// rideBoxX ... are the normalized box rect on the synthetic 320x240 frames.
const (
	rideBoxX, rideBoxY = 0.10, 0.55
	rideBoxW, rideBoxH = 0.80, 0.35
)

const (
	ridePrefixText = "The long night had finally settled over the water."
	rideFinalText  = "The long night had finally settled over the water. It did not break."
)

// rideScene is one frame of the synthetic feed: a static mid-grey scene, the
// dark box panel, and one bright text band. The band is wider on the final
// crop — the shape of a line that gained a couple of characters — so the two
// crops have DIFFERENT exact hashes but stay inside the perceptual tolerance
// (the test asserts that below; a degenerate clip fails the test, not the
// verdict).
func rideScene(k int, finalW, prefixW int) image.Image {
	const w, h = 320, 240
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	x0, y0 := int(rideBoxX*float64(w)), int(rideBoxY*float64(h))
	x1, y1 := int((rideBoxX+rideBoxW)*float64(w)), int((rideBoxY+rideBoxH)*float64(h))
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.Pix[y*w+x] = 30 // panel
		}
	}
	bandW := prefixW
	if k >= rideFinalFirst {
		bandW = finalW
	}
	by := y0 + (y1-y0)/2
	for y := by - 5; y < by+5; y++ {
		for x := x0 + 8; x < x0+8+bandW; x++ {
			img.Pix[y*w+x] = 230 // the "text"
		}
	}
	return img
}

func rideCrop(k, finalW, prefixW int) image.Image {
	return vision.CropNormalized(rideScene(k, finalW, prefixW), vision.Rect{X: rideBoxX, Y: rideBoxY, W: rideBoxW, H: rideBoxH}, vision.CropOptions{})
}

// rideSource delivers frames 1..ridePrefixLast, then holds until release closes
// (the test's main goroutine holds it open until the prefix read has settled —
// waitQuiescent — so the final crop's repetition is the settled tail), and
// delivers the rest at rideInterval.
type rideSource struct {
	finalW, prefixW int
	release         chan struct{}
}

func (s *rideSource) ID() string           { return "synthetic:ride-final" }
func (s *rideSource) Stats() capture.Stats { return capture.Stats{} }
func (s *rideSource) Close() error         { return nil }

func (s *rideSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	out := make(chan capture.Frame, 16)
	go func() {
		defer close(out)
		for k := 1; k <= rideFrames; k++ {
			if k == rideFinalFirst {
				<-s.release
			}
			select {
			case <-ctx.Done():
				return
			default:
			}
			if k > 1 {
				select {
				case <-time.After(rideInterval):
				case <-ctx.Done():
					return
				}
			}
			f := capture.Frame{
				Seq:                int64(k),
				SourceClock:        capture.ClockMonotonic,
				CaptureTimestampNS: int64(k) * rideInterval.Nanoseconds(),
				Width:              320,
				Height:             240,
				Image:              rideScene(k, s.finalW, s.prefixW),
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// findBandWidths searches the 150..240 band-width range for a pair whose
// PREPROCESSED crops (the same recipe the Runner under test uses) have
// different exact hashes (the typewriter gained characters) but stay inside
// the perceptual tolerance (the final crop is not a page change). The search
// guarantees the ridden-growth shape instead of assuming it.
func findBandWidths(t *testing.T) (prefixW, finalW int) {
	t.Helper()
	for p := 150; p <= 240; p++ {
		pc := vision.Preprocess(rideCrop(1, 0, p), steps())
		pHash := vision.ImageHash(pc)
		pPh, ok := vision.PerceptualHash(pc)
		if !ok {
			continue
		}
		for f := p + 5; f <= 240; f += 5 {
			fc := vision.Preprocess(rideCrop(rideFinalFirst, f, p), steps())
			fHash := vision.ImageHash(fc)
			if fHash == pHash {
				continue
			}
			fPh, ok2 := vision.PerceptualHash(fc)
			if !ok2 || vision.PerceptualDistance(pPh, fPh) > 10 {
				continue
			}
			return p, f
		}
	}
	t.Fatalf("no band-width pair with a different exact hash inside the 10-bit perceptual tolerance: the clip cannot reproduce the ridden-final-crop shape")
	return 0, 0
}

func TestRiddenFinalCropGetsExactlyOneRead(t *testing.T) {
	prefixW, finalW := findBandWidths(t)
	prefixCrop := vision.Preprocess(rideCrop(1, finalW, prefixW), steps())
	finalCrop := vision.Preprocess(rideCrop(rideFinalFirst, finalW, prefixW), steps())
	prefixHash := vision.ImageHash(prefixCrop)
	finalHash := vision.ImageHash(finalCrop)
	if prefixHash == finalHash {
		t.Fatalf("clip degenerate: prefix and final crops hash alike")
	}
	pd, okP := vision.PerceptualHash(prefixCrop)
	fd, okF := vision.PerceptualHash(finalCrop)
	if !okP || !okF {
		t.Fatalf("clip degenerate: the perceptual hash refused a crop")
	}
	if d := vision.PerceptualDistance(pd, fd); d > 10 {
		t.Fatalf("clip degenerate: crops %d bits apart, outside the 10-bit tolerance (the final crop would be a page change, not a ridden growth)", d)
	}

	script := map[string]string{
		prefixHash: ridePrefixText,
		finalHash:  rideFinalText,
	}
	eng := &fakeOCR{script: script, delay: func(string) time.Duration { return 300 * time.Millisecond }, respectCtx: true}
	reg := games.Region{ID: "ridebox", RegionType: games.RegionTextbox,
		RectX: rideBoxX, RectY: rideBoxY, RectW: rideBoxW, RectH: rideBoxH}
	h := newHarness(t, eng, func(c *pipeline.Config) {
		c.Regions = []games.Region{reg}
		c.Mode = pipeline.ModeLive
		c.Queue = pipeline.QueueLatest
	})
	release := make(chan struct{})
	src := &rideSource{finalW: finalW, prefixW: prefixW, release: release}
	h.src = src

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.runner.Run(ctx, src, h.hub)
	}()
	// Hold the tail until the prefix read has settled: the run's tail is "the
	// final crop, repeated, while no read is in flight" — the exact shape the
	// dedupe memory must answer with one read of its own.
	waitQuiescent(t, eng, ctx)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := h.runner.Stats()
	evs, serr := h.store.After(context.Background(), 0, events.Filter{}, 4096)
	if serr != nil {
		t.Fatalf("read events: %v", serr)
	}
	timeline(t, h.runner)
	t.Logf("stats: %+v", st)

	// The final pixels must be recognised exactly once.
	if st.OCRCalls < 2 {
		t.Fatalf("ocr_calls = %d: the prefix read and the final read are both owed; the ridden crop was never read", st.OCRCalls)
	}
	if got := st.OCRCalls - st.OCRCancelled; got != 2 {
		t.Fatalf("settled ocr_calls = %d, want 2 (prefix once, final exactly once; the %d repeated final crops must dedupe after their read)",
			got, rideFrames-rideFinalFirst+1)
	}
	// The region's emitted text must END as the final line, not the prefix.
	// message.committed carries what the tracker settled; the prefix may commit
	// mid-typing, but the last committed line of this box is the full one.
	var lastText string
	for _, e := range evs {
		if e.Name != events.MessageCommitted {
			continue
		}
		var m struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(e.Data, &m)
		lastText = m.Text
	}
	if !strings.EqualFold(lastText, rideFinalText) {
		t.Errorf("last committed text = %q, want the final line %q: the fully typed box must not freeze at the prefix its crop rode", lastText, rideFinalText)
	}
}

// TestIdenticalCropsAfterRiddenReadSkip guards the other side: once the final
// crop's read has settled, its identical repeats must skip — the fix must not
// turn a stable final line into an OCR storm.
func TestIdenticalCropsAfterRiddenReadSkip(t *testing.T) {
	prefixW, finalW := findBandWidths(t)
	prefixHash := vision.ImageHash(vision.Preprocess(rideCrop(1, finalW, prefixW), steps()))
	finalHash := vision.ImageHash(vision.Preprocess(rideCrop(rideFinalFirst, finalW, prefixW), steps()))
	script := map[string]string{prefixHash: ridePrefixText, finalHash: rideFinalText}
	eng := &fakeOCR{script: script, delay: func(string) time.Duration { return 300 * time.Millisecond }, respectCtx: true}
	reg := games.Region{ID: "ridebox2", RegionType: games.RegionTextbox,
		RectX: rideBoxX, RectY: rideBoxY, RectW: rideBoxW, RectH: rideBoxH}
	h := newHarness(t, eng, func(c *pipeline.Config) {
		c.Regions = []games.Region{reg}
		c.Mode = pipeline.ModeLive
		c.Queue = pipeline.QueueLatest
	})
	release := make(chan struct{})
	src := &rideSource{finalW: finalW, prefixW: prefixW, release: release}
	h.src = src
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.runner.Run(ctx, src, h.hub)
	}()
	waitQuiescent(t, eng, ctx)
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := h.runner.Stats()
	if got := st.OCRCalls - st.OCRCancelled; got != 2 {
		t.Errorf("settled ocr_calls = %d, want 2: the %d identical final crops after the final read must skip (no OCR storm), and the prefix read settles exactly once",
			got, rideFrames-rideFinalFirst+1)
	}
	if st.OCRSkippedByHash < rideFrames-rideFinalFirst-2 {
		t.Errorf("ocr_skipped_by_hash = %d, want at least %d: the repeated final crops must ride the dedupe, not the engine (of the %d final-crop frames, at most one rides the prefix read and one starts the final read; the rest skip)",
			st.OCRSkippedByHash, rideFrames-rideFinalFirst-2, rideFrames-rideFinalFirst+1)
	}
}
