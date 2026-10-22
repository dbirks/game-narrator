package pipeline_test

// Regression test for the unbounded-growth class of pipeline defect: a 24-frame
// replay at speed 0 must finish with its heap inside a small fixed budget. The
// bound exists because the failure this test prevents (a loop re-allocating on
// every iteration of a spin, or a queue that never drains) grows the heap by
// hundreds of MiB per second and OOM-killed the host on a 15 GB box — it is not
// a tuning knob. If a legitimate change grows steady-state memory past 256 MiB,
// that is a finding to report, not a number to bump.

import (
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
)

const (
	// replayPeakHeapLimit is the ceiling for the whole 24-frame replay,
	// including the test harness and the race detector's overhead.
	replayPeakHeapLimit = 256 << 20 // 256 MiB
	// replayHeapTick is the sampling cadence. 2 ms is short enough to catch a
	// growth curve that doubles every 500 ms (the signature of the original
	// bug) and cheap enough that the sampler itself cannot be the growth.
	replayHeapTick = 2 * time.Millisecond
)

// TestPeakHeapBoundedDuringReplay replays the full 24-frame clip at speed 0 while
// sampling HeapAlloc, and asserts the peak stays under replayPeakHeapLimit. It
// also asserts the replay still did its job (two commits), so the budget cannot
// be met by quietly skipping work. It runs in both modes: ModeLive is the
// queue-and-supersede shape the runtime runs, ModeReplay the lossless CI gate
// (docs/spec/12), and both must stay inside the same budget.
func TestPeakHeapBoundedDuringReplay(t *testing.T) {
	modes := []struct {
		name  string
		mode  pipeline.Mode
		queue pipeline.QueuePolicy
	}{
		// Live keeps the harness default (lossless queue, supersede-cancel on):
		// this test measures memory, and the live shape it must match is the one
		// the runner_test suite runs.
		{"live", pipeline.ModeLive, pipeline.QueueLossless},
		{"replay", pipeline.ModeReplay, pipeline.QueueLossless},
	}
	for _, m := range modes {
		t.Run(m.name, func(t *testing.T) {
			eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
			h := newHarness(t, eng, func(c *pipeline.Config) { c.Mode = m.mode; c.Queue = m.queue })

			var peak atomic.Uint64
			stop := make(chan struct{})
			defer close(stop)
			go func() {
				ticker := time.NewTicker(replayHeapTick)
				defer ticker.Stop()
				for {
					select {
					case <-stop:
						return
					case <-ticker.C:
						var ms runtime.MemStats
						runtime.ReadMemStats(&ms)
						for {
							old := peak.Load()
							if ms.HeapAlloc <= old || peak.CompareAndSwap(old, ms.HeapAlloc) {
								break
							}
						}
					}
				}
			}()

			// Live plays the clip at its real 10 fps cadence: at speed 0 a
			// live run is a race between the OCR and a clip that has already
			// finished (CI's slower runners lost the first line to it).
			speed := 0.0
			if m.mode == pipeline.ModeLive {
				speed = 1
			}
			st, evs, err := h.run(t, h.source(speed, false))
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			// Let the sampler observe the final (largest in-flight) state before it stops.
			time.Sleep(replayHeapTick)

			if got := committed(t, evs); len(got) != 2 || st.Commits != 2 {
				t.Fatalf("committed = %v (Stats.Commits %d): the memory budget must not be met by skipping work", got, st.Commits)
			}
			if p := peak.Load(); p > replayPeakHeapLimit {
				t.Fatalf("peak heap %d MiB during the 24-frame replay exceeds the %d MiB bound: an unbounded buffer, a retained frame, or a spinning loop has crept back in",
					p>>20, replayPeakHeapLimit>>20)
			}
			t.Logf("peak heap %d MiB during replay (limit %d MiB); frames=%d ocr=%d",
				peak.Load()>>20, replayPeakHeapLimit>>20, st.FramesProcessed, st.OCRCalls)
		})
	}
}
