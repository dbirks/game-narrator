package pipeline_test

// The UNO Q's analog HDMI feed never delivers two pixel-identical crops (the
// PerceptualSkipTolerance comment in config.go), so the perceptual skip is the
// only thing that keeps a static box from re-reading at the frame's cadence
// (ADR-0115 §3: the board is CPU-bound already). The 2026-10-23
// final-crop-never-read bug (a ridden crop skipping forever behind a read of
// DIFFERENT pixels) had to be fixed without turning this feed's stable box back
// into a per-frame OCR loop: at most ONE confirming read per settled box, then
// the skip covers the rest.
//
// The clip: a static box for 60 frames whose every crop differs by a couple of
// noise pixels — distinct exact hashes, inside the perceptual tolerance. The
// total OCR calls for the region must stay bounded (the first read, at most one
// confirming read, plus at most one more) and the skip counter must cover the
// rest.

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
	analogFrames   = 60
	analogInterval = 100 * time.Millisecond
)

const analogText = "The long night had finally settled over the water."

// analogScene is frame k of the feed: the same static scene as rideScene's box
// (mid-grey background, dark panel, bright text band) with k-1 noise pixels
// flipped on the panel — frame 1 is clean, every later frame carries a
// different pixel change.
func analogScene(k int) image.Image {
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
	by := y0 + (y1-y0)/2
	// The band walks with k: width, height and x-offset on three coprime
	// periods (7, 3 and 11, joint period 231 — no repeat inside the clip),
	// each step small enough that every crop stays inside the perceptual
	// tolerance of the anchor yet distinct in the lossless exact hash.
	// That is the analog feed's property in crop form: no two consecutive
	// crops pixel-identical, and the movement is the typewriter-growth regime
	// the tolerance was measured on (TestPhashToleranceSitsInTheMeasuredGap).
	bw := 180 + (k*4)%28
	bh := 10 + (k*4)%8
	nx := x0 + 8 + (k*8)%40
	for y := by - bh/2; y < by+bh/2; y++ {
		for x := nx; x < nx+bw; x++ {
			img.Pix[y*w+x] = 230 // the "text"
		}
	}
	return img
}

// analogSource delivers analogFrames at analogInterval, straight, no hold.
type analogSource struct{}

func (s *analogSource) ID() string           { return "synthetic:analog-noise" }
func (s *analogSource) Stats() capture.Stats { return capture.Stats{} }
func (s *analogSource) Close() error         { return nil }

func (s *analogSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	out := make(chan capture.Frame, 16)
	go func() {
		defer close(out)
		for k := 1; k <= analogFrames; k++ {
			if k > 1 {
				select {
				case <-time.After(analogInterval):
				case <-ctx.Done():
					return
				}
			}
			f := capture.Frame{
				Seq:                int64(k),
				SourceClock:        capture.ClockMonotonic,
				CaptureTimestampNS: int64(k) * analogInterval.Nanoseconds(),
				Width:              320,
				Height:             240,
				Image:              analogScene(k),
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

// analogClipShape pins the noise the test depends on: every crop has a distinct
// exact hash (the analog feed's property — the exact-hash dedupe never fires)
// yet sits inside the perceptual tolerance of the first crop (the box did not
// change perceptually). If a preprocess or hash change breaks either half the
// test fails on the clip, not on a verdict the clip cannot support.
func analogClipShape(t *testing.T) (hashes []string, anchorPh uint64) {
	t.Helper()
	ph0, ok := vision.PerceptualHash(vision.CropNormalized(analogScene(1),
		vision.Rect{X: rideBoxX, Y: rideBoxY, W: rideBoxW, H: rideBoxH}, vision.CropOptions{}))
	if !ok {
		t.Fatalf("clip degenerate: the perceptual hash refused frame 1")
	}
	seen := map[string]bool{}
	for k := 1; k <= analogFrames; k++ {
		crop := vision.CropNormalized(analogScene(k),
			vision.Rect{X: rideBoxX, Y: rideBoxY, W: rideBoxW, H: rideBoxH}, vision.CropOptions{})
		pre := vision.Preprocess(crop, steps())
		h := vision.ImageHash(pre)
		if k > 1 && seen[h] {
			t.Fatalf("clip degenerate: frame %d's crop hashes alike with an earlier frame; the analog feed's distinct-hash property is gone", k)
		}
		seen[h] = true
		ph, ok := vision.PerceptualHash(crop)
		if !ok {
			t.Fatalf("clip degenerate: the perceptual hash refused frame %d", k)
		}
		if d := vision.PerceptualDistance(ph0, ph); d > 10 {
			t.Fatalf("clip degenerate: frame %d's crop is %d bits from the anchor, outside the 10-bit tolerance; the noise would read as a page change, not analog noise", k, d)
		}
		hashes = append(hashes, h)
	}
	return hashes, ph0
}

func TestAnalogNoiseSettleCostsOneConfirmingRead(t *testing.T) {
	hashes, _ := analogClipShape(t)
	script := make(map[string]string, len(hashes))
	for _, h := range hashes {
		script[h] = analogText
	}
	eng := &fakeOCR{script: script, delay: func(string) time.Duration { return 300 * time.Millisecond }, respectCtx: true}
	reg := games.Region{ID: "analogbox", RegionType: games.RegionTextbox,
		RectX: rideBoxX, RectY: rideBoxY, RectW: rideBoxW, RectH: rideBoxH}
	h := newHarness(t, eng, func(c *pipeline.Config) {
		c.Regions = []games.Region{reg}
		c.Mode = pipeline.ModeLive
		c.Queue = pipeline.QueueLatest
	})
	h.src = &analogSource{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- h.runner.Run(ctx, h.src, h.hub)
	}()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	st := h.runner.Stats()
	timeline(t, h.runner)
	t.Logf("stats: %+v", st)

	// Bounded: the first read, at most one confirming read, plus at most one
	// more. Anything past that is the rejected always-owe shape — the analog
	// feed re-reading a static box on the engine's own cadence.
	if got := st.OCRCalls - st.OCRCancelled; got > 3 {
		t.Errorf("settled ocr_calls = %d, want <= 3 (first read + at most one confirming read + at most one more): the static analog box must not re-read on the engine's cadence", got)
	}
	// The rest is covered by the skip: of the 60 frames, at most three went to
	// the engine, so at least 50 must ride the dedupe.
	if got := st.OCRSkippedByHash; got < analogFrames-10 {
		t.Errorf("ocr_skipped_by_hash = %d, want at least %d: after the settling reads the static box must ride the skip, not the engine", got, analogFrames-10)
	}
	evs, serr := h.store.After(context.Background(), 0, events.Filter{}, 4096)
	if serr != nil {
		t.Fatalf("read events: %v", serr)
	}
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
	if lastText != "" && !strings.EqualFold(lastText, analogText) {
		t.Errorf("last committed text = %q, want the static line %q", lastText, analogText)
	}
}
