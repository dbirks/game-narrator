package pipeline_test

// The live region's OCR reads must settle even when the engine's latency
// exceeds the frame interval — docs/spec/12's "delayed model responses" case.
// Before the fix, a live region whose crop changed on every frame started a
// fresh read on EVERY frame and killed the one in flight (supersede): a read
// slower than the frame interval was cancelled before it finished, so a region
// whose engine takes 300 ms against a 100 ms frame never delivered a single
// text — its rankings stayed at zero and nothing committed. The shape that hit
// this on the real host: a 12-minute Dolphin session with a perfectly readable
// box on screen (the box region sits over an animated scene, so its crop hash
// changes every frame) produced 3861 OCR calls, 3862 cancelled, 0 rankings,
// 0 commits.
//
// The synthetic clip below reproduces that shape: a box over an always-moving
// picture (every frame's crop differs, so the hash dedupe never fires), with a
// stable line of text. A live region now RIDES the read in flight: a
// changed-hash frame attaches to it instead of starting a duplicate, so the
// read settles at the engine's own pace (the docs/spec/05 2–5 Hz OCR cadence
// the engine actually sustains) and the clip still commits. Replay is
// untouched: ModeReplay serializes per region with waitInflight and must stay
// deterministic.

import (
	"context"
	"image"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision"
)

const (
	churnFrames   = 24
	churnInterval = 100 * time.Millisecond

	// The box rect, normalized on the synthetic 320x240 frames.
	churnBoxX, churnBoxY = 0.10, 0.55
	churnBoxW, churnBoxH = 0.80, 0.35
)

// churnScene is one frame of the synthetic feed: a mid-grey scene (never a
// panel, so presence reads absent when the box is off), the dark box panel, a
// bright text band (its width marks the page), and a 6x6 "shimmer" mark that
// moves along the box's bottom edge every frame — the animated-pixel shape a
// real box region has over a moving scene, which is what makes every crop's
// hash differ.
func churnScene(k int) image.Image {
	const w, h = 320, 240
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	if k < 2 || k >= 20 {
		return img // box absent
	}
	x0, y0 := int(churnBoxX*float64(w)), int(churnBoxY*float64(h))
	x1, y1 := int((churnBoxX+churnBoxW)*float64(w)), int((churnBoxY+churnBoxH)*float64(h))
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			img.Pix[y*w+x] = 30 // panel
		}
	}
	bandW := 200
	if k >= 14 {
		bandW = 120 // page two
	}
	by := y0 + (y1-y0)/2
	for y := by - 5; y < by+5; y++ {
		for x := x0 + 8; x < x0+8+bandW; x++ {
			img.Pix[y*w+x] = 230 // the "text"
		}
	}
	sx := x0 + 10 + (k*7)%(x1-x0-20)
	sy := y1 - 10
	for dy := 0; dy < 6; dy++ {
		for dx := 0; dx < 6; dx++ {
			img.Pix[(sy+dy)*w+(sx+dx)] = 200
		}
	}
	return img
}

// churnText is what frame k shows: line A through the box's first page, line B
// on page two, nothing when the box is gone.
func churnText(k int) string {
	switch {
	case k < 2 || k >= 20:
		return ""
	case k < 14:
		return lineA
	default:
		return lineB
	}
}

func churnCrop(k int) image.Image {
	return vision.CropNormalized(churnScene(k), vision.Rect{X: churnBoxX, Y: churnBoxY, W: churnBoxW, H: churnBoxH}, vision.CropOptions{})
}

// churnSource delivers the synthetic clip at a steady interval — the live-feed
// shape (the bridge's x11grab at N fps), unlike a recording's bursty
// timestamps.
type churnSource struct {
	interval time.Duration
}

func (s *churnSource) ID() string           { return "synthetic:churn" }
func (s *churnSource) Stats() capture.Stats { return capture.Stats{} }
func (s *churnSource) Close() error         { return nil }

func (s *churnSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	out := make(chan capture.Frame, 16)
	go func() {
		defer close(out)
		for k := 0; k < churnFrames; k++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			if k > 0 {
				select {
				case <-time.After(s.interval):
				case <-ctx.Done():
					return
				}
			}
			f := capture.Frame{
				Seq:                int64(k) + 1,
				SourceClock:        capture.ClockMonotonic,
				CaptureTimestampNS: int64(k+1) * s.interval.Nanoseconds(),
				Width:              320,
				Height:             240,
				Image:              churnScene(k),
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

func TestLiveOCRReadSettlesUnderFrameChurn(t *testing.T) {
	// The script: the hash of each frame's PREPROCESSED box crop (the same
	// recipe the Runner under test uses) to what that frame shows.
	script := map[string]string{}
	for k := 0; k < churnFrames; k++ {
		script[vision.ImageHash(vision.Preprocess(churnCrop(k), steps()))] = churnText(k)
	}
	// Sanity: the clip must actually churn — every box frame's crop distinct,
	// so the hash dedupe never has a chance and the only way text reaches the
	// tracker is through a read that SETTLES.
	distinct := map[string]bool{}
	for k := 2; k < 20; k++ {
		distinct[vision.ImageHash(vision.Preprocess(churnCrop(k), steps()))] = true
	}
	if len(distinct) != 18 {
		t.Fatalf("churn clip degenerate: %d distinct box crops, want 18 (every box frame's crop must differ — the shape of a real box region over an animated scene)", len(distinct))
	}

	// Every read takes 300 ms: three frame intervals. A well-behaved engine
	// that respects cancellation (the production tesseract wrapper does —
	// supervise kills the child on context death).
	const readTime = 300 * time.Millisecond
	eng := &fakeOCR{script: script, delay: func(string) time.Duration { return readTime }, respectCtx: true}
	reg := games.Region{ID: "churnbox", RegionType: games.RegionTextbox,
		RectX: churnBoxX, RectY: churnBoxY, RectW: churnBoxW, RectH: churnBoxH}
	h := newHarness(t, eng, func(c *pipeline.Config) {
		c.Regions = []games.Region{reg}
		c.Queue = pipeline.QueueLatest // the live queue the runtime runs
	})
	src := &churnSource{interval: churnInterval}
	h.src = src
	st, evs, err := h.run(t, src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	timeline(t, h.runner)

	// The livelock signature: no read settles, the region never perceives a
	// character, nothing commits.
	if st.Rankings == 0 {
		t.Fatalf("no OCR read settled (ocr_calls=%d cancelled=%d stale=%d rankings=%d): a %s read against a %s frame whose crop changes every frame was superseded to death — the live region must ride the read in flight so it settles at the engine's own pace",
			st.OCRCalls, st.OCRCancelled, st.OCRStale, st.Rankings, readTime, churnInterval)
	}
	if st.OCRCalls > 16 {
		t.Errorf("ocr_calls = %d (of %d frames): the region re-read the same box on nearly every frame instead of letting the in-flight read settle (a settling region calls the engine once per settled read, ~%d here)",
			st.OCRCalls, st.FramesProcessed, st.Rankings)
	}
	if got := committed(t, evs); len(got) < 1 {
		t.Errorf("committed = %v, want at least line A: settled reads must reach the tracker and commit", got)
	}
	t.Logf("stats: %+v", st)
}
