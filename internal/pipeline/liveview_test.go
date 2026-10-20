package pipeline_test

// The live view's bounded observations (ADR-0110 §3): the newest frame in
// memory (one slot, overwritten per frame — the preview serves it without a
// device grab) and the last per-region crop box (one entry per region id,
// the profile's bounded set). Both are written on the frame-ordered stage
// and read from other goroutines, so a reader sees one whole frame or one
// whole rectangle, never a torn pair; a finished run's runner is
// unreferenced with the session, which releases the held frame's pixels.

import (
	"context"
	"image"
	"image/color"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// livePushSource is a push-based capture.Source for the pipeline's Run: each
// push is a real frame the frame-ordered stage processes.
type livePushSource struct {
	mu     sync.Mutex
	ch     chan capture.Frame
	closed bool
	seq    int64
}

func newLivePushSource() *livePushSource {
	return &livePushSource{ch: make(chan capture.Frame, 64)}
}

func (s *livePushSource) ID() string { return "v4l2:liveview-test" }

func (s *livePushSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.ch, nil
}

func (s *livePushSource) push(t *testing.T, n int, w, h int) {
	t.Helper()
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(seq), G: uint8(x % 256), B: uint8(y % 256), A: 255})
		}
	}
	f := capture.Frame{
		SourceClock:        capture.ClockMonotonic,
		CaptureTimestampNS: int64(seq) * 100_000_000,
		Width:              w, Height: h,
		PixFmt: capture.PixRGBA,
		Image:  img,
		Hash:   capture.HashFrame(img),
		Seq:    seq,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case s.ch <- f:
	case <-ctx.Done():
		t.Fatalf("live push %d timed out: the pipeline stopped consuming", seq)
	}
}

func (s *livePushSource) Stats() capture.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq == 0 {
		return capture.Stats{}
	}
	return capture.Stats{State: capture.StateRunning, Delivered: uint64(s.seq), FPS: 10, LastFrameAt: time.Now()}
}

func (s *livePushSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.ch)
	return nil
}

// liveFixedOCR is the run's scripted engine: one reading for whatever crop
// arrives (the live view's assertions are about the frame and the box, not
// the reading).
type liveFixedOCR struct{ text string }

func (o liveFixedOCR) Name() string { return "liveview" }
func (o liveFixedOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	return ocr.Result{Text: o.text, MeanConf: 0.9, Engine: "liveview"}, nil
}

// TestLiveViewHoldsOneNewestFrameAndLastCrops pins the live view's bounds and
// its frame-coordinate crop: before the first frame everything answers "not
// yet"; after the run processes frames, LatestFrame is the newest (one slot —
// the run holds exactly one extra frame, overwritten per frame) and the
// dialogue region's last crop is its mapped box in frame coordinates (the
// textbox region 0.096875, 0.75625, 0.80625, 0.158333 of a 640x480 frame:
// ~(62, 363, 578, 439)).
func TestLiveViewHoldsOneNewestFrameAndLastCrops(t *testing.T) {
	reg := games.Region{
		ID: "bottom", RegionType: games.RegionTextbox,
		RectX: 0.096875, RectY: 0.75625, RectW: 0.80625, RectH: 0.158333,
	}
	idx, err := matcher.Build(
		[]games.Message{{ID: "m1", ProfileID: "p", OriginalText: "A line.", SpeechText: "A line.", TextKind: games.TextDialogue}},
		nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	cfg := pipeline.Config{
		SessionID: "liveview", ProfileRevision: "1",
		Regions: []games.Region{reg},
		OCR:     liveFixedOCR{text: "A line."},
		Index:   idx,
	}
	r, err := pipeline.New(cfg)
	if err != nil {
		t.Fatalf("pipeline.New: %v", err)
	}

	// Before the first frame: not "an empty frame", "not yet".
	if _, ok := r.LatestFrame(); ok {
		t.Fatal("LatestFrame answers before the first frame; it must stay 'not yet'")
	}
	if _, ok := r.LastCrop("bottom"); ok {
		t.Fatal("LastCrop answers before the region's first crop; it must stay 'not yet'")
	}
	if _, ok := r.DialogueCrop(); ok {
		t.Fatal("DialogueCrop answers before the run has computed one")
	}

	src := newLivePushSource()
	t.Cleanup(func() { _ = src.Close() })
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	t.Cleanup(func() { _ = hub.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx, src, hub) }()

	// The textbox region's mapped box on the 640x480 frame: x 0.096875 (62),
	// y 0.75625 (363), w 0.80625 of 640 (516), h 0.158333 of 480 (76).
	wantCrop := image.Rectangle{Min: image.Point{X: 62, Y: 363}, Max: image.Point{X: 578, Y: 439}}
	by := time.Now().Add(30 * time.Second)
	for n := 1; ; n++ {
		src.push(t, n, 640, 480)
		f, ok := r.LatestFrame()
		if ok && f.Seq >= 4 {
			crop, cok := r.DialogueCrop()
			if !cok {
				t.Fatalf("DialogueCrop is still 'not yet' after four processed frames")
			}
			if crop.Dx() != wantCrop.Dx() || crop.Dy() != wantCrop.Dy() ||
				crop.Min.X < wantCrop.Min.X-2 || crop.Min.X > wantCrop.Min.X+2 ||
				crop.Min.Y < wantCrop.Min.Y-2 || crop.Min.Y > wantCrop.Min.Y+2 {
				t.Fatalf("dialogue crop = %v, want the mapped textbox box %v (frame coordinates)", crop, wantCrop)
			}
			if got, ok := r.LastCrop("bottom"); !ok || got != crop {
				t.Fatalf("LastCrop(%q) = (%v, %v), want the dialogue region's box", "bottom", got, ok)
			}
			// The newest frame in memory is the newest pushed: the slot
			// overwrites, so the run holds one frame, not a history.
			if f.Seq != int64(n) {
				// A later frame may have landed between the two reads; the
				// bound that matters is one slot, and LatestFrame is the
				// newest — re-read and require monotonicity.
				if f2, _ := r.LatestFrame(); f2.Seq < f.Seq {
					t.Fatalf("LatestFrame went backwards (%d then %d); the slot is written in frame order", f.Seq, f2.Seq)
				}
			}
			break
		}
		if time.Now().After(by) {
			t.Fatalf("the run never delivered four frames (last seq %d)", f.Seq)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	if err := <-done; err != nil && ctx.Err() == nil {
		t.Fatalf("Run after cancel: %v", err)
	}
}
