package pipeline_test

import (
	"context"
	"image"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// TestMaxConcurrentOCRNormalize pins the Config contract (issue #8, ADR-0095):
// the bound on concurrent engine calls is explicit, clamped to [1, Workers],
// and the auto-default is the measured board behaviour — live mode on a
// machine with four or fewer cores serialises OCR; everything else keeps the
// pool width.
func TestMaxConcurrentOCRNormalize(t *testing.T) {
	runs := map[string]struct {
		cfg  func(*pipeline.Config)
		want int
	}{
		"explicit value is honoured": {
			cfg:  func(c *pipeline.Config) { c.Workers = 4; c.MaxConcurrentOCR = 1 },
			want: 1,
		},
		"the bound can never exceed the pool": {
			cfg:  func(c *pipeline.Config) { c.Workers = 2; c.MaxConcurrentOCR = 9 },
			want: 2,
		},
		"live mode auto-defaults to serial on a small board": {
			cfg:  func(c *pipeline.Config) { c.Mode = pipeline.ModeLive; c.Workers = 2 },
			want: autoLive(runtime.NumCPU() <= 4),
		},
		"replay auto-defaults to the pool width": {
			cfg:  func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay; c.Workers = 3 },
			want: 3,
		},
	}
	for name, tc := range runs {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, &fakeOCR{script: map[string]string{}}, tc.cfg)
			if got := h.runner.Config().MaxConcurrentOCR; got != tc.want {
				t.Fatalf("MaxConcurrentOCR = %d, want %d (Workers=%d Mode=%d)", got, tc.want, h.runner.Config().Workers, h.runner.Config().Mode)
			}
		})
	}
}

func autoLive(smallBoard bool) int {
	if smallBoard {
		return 1
	}
	return 2 // DefaultWorkers
}

// serialProbe is an ocr.Engine that records how many of its calls ever ran at
// the same time and holds every call on a gate the test controls. It is the
// instrumentation the concurrency cap is asserted against.
type serialProbe struct {
	gate   chan struct{}
	script map[string]string

	mu        sync.Mutex
	active    int
	maxActive int
	done      atomic.Int64
}

func (p *serialProbe) Name() string { return "serial-probe" }

func (p *serialProbe) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	p.mu.Lock()
	p.active++
	if p.active > p.maxActive {
		p.maxActive = p.active
	}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.active--
		p.mu.Unlock()
	}()
	select {
	case <-p.gate:
	case <-ctx.Done():
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrCancelled, Engine: p.Name(), Err: ctx.Err()}
	}
	p.done.Add(1)
	if img == nil || img.Bounds().Empty() {
		return ocr.Result{}, nil
	}
	return ocr.Result{Text: "probe", Engine: p.Name(), MeanConf: 0.9375,
		Lines: []ocr.Line{{Text: "probe", Conf: 0.9375, Box: image.Rect(0, 0, 100, 100)}},
		Words: []ocr.Word{{Text: "probe", Conf: 0.9375, Box: image.Rect(0, 0, 100, 100)}},
	}, nil
}

// twoRegionSource delivers a handful of frames carrying two distinct dark
// panels (the two regions' crops), so both regions start a read while the
// probe's gate holds the first one.
type twoRegionSource struct {
	frames int
}

func (s *twoRegionSource) ID() string           { return "synthetic:two-region" }
func (s *twoRegionSource) Stats() capture.Stats { return capture.Stats{} }
func (s *twoRegionSource) Close() error         { return nil }

func (s *twoRegionSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	out := make(chan capture.Frame, 16)
	go func() {
		defer close(out)
		for k := 0; k < s.frames; k++ {
			img := image.NewGray(image.Rect(0, 0, 320, 240))
			for i := range img.Pix {
				img.Pix[i] = 128
			}
			// Region A panel (left half) with a shimmer that moves every frame,
			// region B panel (right half) with its own shimmer: both regions'
			// crop hashes differ frame to frame, like a box over an animated
			// scene, so neither region's dedupe suppresses its read.
			for y := 40; y < 120; y++ {
				for x := 20; x < 140; x++ {
					img.Pix[y*320+x] = 30
				}
			}
			for y := 40; y < 120; y++ {
				for x := 180; x < 300; x++ {
					img.Pix[y*320+x] = 30
				}
			}
			sx := 24 + (k*11)%90
			img.Pix[(44+sx/20)*320+sx] = 230
			sx2 := 184 + (k*13)%90
			img.Pix[(44+sx2/20)*320+sx2] = 230
			f := capture.Frame{
				Seq:                int64(k) + 1,
				SourceClock:        capture.ClockMonotonic,
				CaptureTimestampNS: int64(k+1) * 100 * time.Millisecond.Nanoseconds(),
				Width:              320,
				Height:             240,
				Image:              img,
			}
			select {
			case out <- f:
			case <-ctx.Done():
				return
			}
			select {
			case <-time.After(50 * time.Millisecond):
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// TestLiveOCRNeverRunsMoreThanTheCap is the board's scheduling failure turned
// into an assertion (issue #8): with two regions both owed a read and the
// probe's gate holding the first, the second read must WAIT for a worker, not
// run alongside the first. On the A53 two concurrent Tesseract processes cost
// 19 s each against 1.1 s alone (ADR-0096), so the live bound there is one.
func TestLiveOCRNeverRunsMoreThanTheCap(t *testing.T) {
	// Closing the gate releases every held call at once: the first read (region
	// A) settles, the single worker takes region B's queued job, and the second
	// read settles too — serially, with maxActive == 1 the whole time.
	probe := &serialProbe{gate: make(chan struct{}), script: map[string]string{}}
	regA := games.Region{ID: "ra", RegionType: games.RegionTextbox, RectX: 0.06, RectY: 0.17, RectW: 0.38, RectH: 0.33}
	regB := games.Region{ID: "rb", RegionType: games.RegionTextbox, RectX: 0.56, RectY: 0.17, RectW: 0.38, RectH: 0.33}
	h := newHarness(t, probe, func(c *pipeline.Config) {
		c.Regions = []games.Region{regA, regB}
		c.Queue = pipeline.QueueLatest
		c.Workers = 2          // the pool is two…
		c.MaxConcurrentOCR = 1 // …but live mode runs one recognition at a time.
	})
	go func() {
		// Both regions' frame-1 jobs are dispatched within the first ~100 ms
		// (frame interval 50 ms, one region-state walk per frame); by 600 ms the
		// first read has been held long enough for the second region's job to be
		// queued behind it — the shape the cap must serialise.
		time.Sleep(600 * time.Millisecond)
		close(probe.gate)
	}()
	src := &twoRegionSource{frames: 8}
	st, _, err := h.run(t, src)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	timeline(t, h.runner)
	if got := probe.maxActive; got > 1 {
		t.Fatalf("max %d OCR recognitions ran at the same time, want <= 1: on the target board concurrent Tesseract processes thrash each other to 19 s each (issue #8, ADR-0096)", got)
	}
	if st.OCRCalls < 2 {
		t.Fatalf("ocr_calls = %d, want >= 2 (both regions' reads must run, serially: the cap limits concurrency, it does not drop work)", st.OCRCalls)
	}
	if probe.done.Load() < 2 {
		t.Fatalf("engine completed %d reads, want >= 2 (both regions' reads must settle)", probe.done.Load())
	}
}

// TestAgeOutDropsStaleObservationNotTheSharedRead is the second half of the
// board's cancellation failure: the ROI stage's probe tick aged the head
// observation out and, with it, KILLED the region's shared in-flight read — the
// very read the newer frames were riding on. A read that outran its head is
// still the newest evidence the region has (the riders are exactly the newer
// frames), so aging out drops the stale observation and lets the read settle;
// only a read nobody rides on any more is cancelled.
//
// The hostile engine (respectCtx=false) is what makes ageOut the trigger: a
// well-behaved engine dies on its own deadline (OCRTimeout) before the belt
// (OCRTimeout+250ms) can fire, and the belt exists precisely for an engine
// that ignores cancellation and answers late.
func TestAgeOutDropsStaleObservationNotTheSharedRead(t *testing.T) {
	// Script: every box frame's preprocessed crop to its line (the churn clip
	// from the pacing test: a box over an always-moving picture, stable text).
	script := map[string]string{}
	for k := 0; k < churnFrames; k++ {
		script[vision.ImageHash(vision.Preprocess(churnCrop(k), steps()))] = churnText(k)
	}
	// The read takes 500 ms: past the head observation's age-out bound
	// (OCRTimeout 100ms + 250ms) so the belt fires while the read is in flight,
	// and the engine ignores the job's deadline so the read settles at 500 ms
	// with the riders still waiting on it.
	eng := &fakeOCR{script: script, delay: func(string) time.Duration { return 500 * time.Millisecond }, respectCtx: false}
	reg := games.Region{ID: "churnbox", RegionType: games.RegionTextbox,
		RectX: churnBoxX, RectY: churnBoxY, RectW: churnBoxW, RectH: churnBoxH}
	h := newHarness(t, eng, func(c *pipeline.Config) {
		c.Regions = []games.Region{reg}
		c.Queue = pipeline.QueueLatest
		c.OCRTimeout = 100 * time.Millisecond
	})
	st, _, err := h.run(t, &churnSource{interval: churnInterval})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	timeline(t, h.runner)
	if st.OCRCancelled > 0 {
		t.Fatalf("ocr_cancelled = %d, want 0: aging out the head observation must not cancel the shared in-flight read the newer frames ride on (issue #8)", st.OCRCancelled)
	}
	if st.OCRCalls == 0 {
		t.Fatal("ocr_calls = 0; the clip never started a read")
	}
	// The riders' observations must carry the settled read's text: at least one
	// observation out of the box frames (k=2..19) resolved with a non-empty OCR
	// payload. The trace row is the per-observation record.
	var withText int
	for _, row := range h.runner.Trace() {
		if row.RegionID == "churnbox" && row.OCRText != "" {
			withText++
		}
	}
	if withText == 0 {
		t.Fatalf("no observation carried the settled read's text (ocr_calls=%d stale=%d): killing the shared read on the head's age-out is the cancel-every-read failure the board measured", st.OCRCalls, st.OCRStale)
	}
}
