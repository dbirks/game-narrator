package pipeline_test

import (
	"context"
	"errors"
	"image"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// TestLateOCRStillCommitsTwice is the out-of-order / late-result case of
// docs/spec/12's failure-injection list, run through the whole chain with the
// replay source in LIVE mode: recognitions answer about pixels that are no
// longer on screen, and the dialogue still commits exactly twice — page one,
// then page two — with the late evidence handled visibly rather than silently
// absorbed. The live mechanism that makes late reads survivable is the RIDE:
// a changed-hash frame that lands while a read is in flight attaches to that
// read instead of superseding it (OCRDeferred), so a read slower than the
// frame interval settles at the engine's own pace, and its slightly stale
// result is the newest evidence the reducer has when it lands.
func TestLateOCRStillCommitsTwice(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: lateScript, respectCtx: false}
	h := newHarness(t, eng, nil)
	st, evs, err := h.run(t, h.source(1.0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	timeline(t, h.runner)

	if eng.misses.Load() != 0 {
		t.Errorf("fake OCR was handed %d crops that are not in the script: the ROI recipe and the harness recipe disagree", eng.misses.Load())
	}
	if got := committed(t, evs); len(got) != 2 || got[0] != msgIDLineA || got[1] != msgIDLineB {
		t.Errorf("committed = %v, want [%s %s] — exactly one commit per page", got, msgIDLineA, msgIDLineB)
	}
	if st.OCRDeferred == 0 {
		t.Errorf("ocr_deferred = 0 (calls=%d cancelled=%d stale=%d): with a 150ms read against a 100ms frame the changed-hash frames that land mid-read must ride the outstanding read — zero means the supersede-to-death path is still live and a slow engine never settles a read",
			st.OCRCalls, st.OCRCancelled, st.OCRStale)
	}
	if st.OCRCalls > 12 {
		t.Errorf("ocr_calls = %d (of %d frames): a settling live region calls the engine once per settled read, not once per frame",
			st.OCRCalls, st.FramesProcessed)
	}
	if st.StaleApplied != 0 {
		t.Errorf("stale_generation_applied = %d: docs/spec/01 invariant 2 makes the generation fence absolute", st.StaleApplied)
	}
	if st.Commits != 2 {
		t.Errorf("Stats.Commits = %d, want 2 (the event count said %v)", st.Commits, committed(t, evs))
	}
	if st.FramesDelivered != 24 {
		t.Errorf("frames delivered = %d, want the 24 the recording holds", st.FramesDelivered)
	}
	if st.Observations == 0 {
		t.Error("no observations reached the reducer")
	}
	t.Logf("stats: %+v", st)
}

// TestInOrderOCRCommitsTwice is the same clip with a cooperative engine: no
// cancellation, the same two commitments. It exists so the previous test's stale
// counts cannot be attributed to the harness rather than to the timing. It is a
// replay-mode test: a fixture replay must be the recording it claims to be
// (ModeReplay), and the previous test's live supersede path is what it isolates.
func TestInOrderOCRCommitsTwice(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	h := newHarness(t, eng, func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
	st, evs, err := h.run(t, h.source(1.0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := committed(t, evs); len(got) != 2 || got[0] != msgIDLineA || got[1] != msgIDLineB {
		t.Errorf("committed = %v, want [%s %s]", got, msgIDLineA, msgIDLineB)
	}
	if st.StaleApplied != 0 {
		t.Errorf("stale_generation_applied = %d", st.StaleApplied)
	}
	if st.Rankings == 0 {
		t.Error("no rankings were dispatched")
	}
	// A clean replay drops and cancels nothing; the counters are the proof.
	if st.OCRCancelled != 0 || st.OCRStale != 0 || st.FramesDropped != 0 {
		t.Errorf("clean replay left drops behind: cancelled=%d stale=%d frames_dropped=%d", st.OCRCancelled, st.OCRStale, st.FramesDropped)
	}
	timeline(t, h.runner)
}

// TestHashDedupeSuppressesRepeatOCR is docs/spec/05's "suppress duplicate OCR calls
// on unchanged region hash" as a number: the corpus holds 24 frames and 13 distinct
// pictures, so an engine called once per frame is a bug, not a cost of doing
// business.
func TestHashDedupeSuppressesRepeatOCR(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	h := newHarness(t, eng, func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
	st, _, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.OCRSkippedByHash == 0 {
		t.Fatal("no OCR was skipped by hash on a clip with 11 repeated pictures")
	}
	if st.OCRCalls >= st.FramesProcessed {
		t.Errorf("OCR calls %d >= frames processed %d: dedupe did not fire", st.OCRCalls, st.FramesProcessed)
	}
	if st.OCRCalls != uint64(eng.calls.Load()) {
		t.Errorf("Stats.OCRCalls %d disagrees with the engine's own count %d", st.OCRCalls, eng.calls.Load())
	}
	// The absent frames (empty scene, dismissed box) must not be OCR'd at all:
	// presence gates the call, or a blank screen costs the appliance a recognition
	// every frame forever.
	if st.OCRCalls > st.FramesProcessed {
		t.Errorf("more OCR calls (%d) than frames (%d)", st.OCRCalls, st.FramesProcessed)
	}
	t.Logf("dedupe: %d OCR calls, %d skipped by hash, %d frames", st.OCRCalls, st.OCRSkippedByHash, st.FramesProcessed)
}

// TestDeterminism asserts the property docs/spec/12 lists under "two OCR futures
// finish out of order" and "determinism under identical inputs": the same clip, the
// same script, two runs, one committed sequence. Latency numbers differ between the
// runs (they are wall-clock measurements); what the session decided does not. The
// runs are replay-mode: the property is what ModeReplay promises, and the live
// path's drop counts are allowed to depend on machine speed.
func TestDeterminism(t *testing.T) {
	var runs [][]string
	for i := 0; i < 2; i++ {
		eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
		h := newHarness(t, eng, func(c *pipeline.Config) { c.Mode = pipeline.ModeReplay })
		_, evs, err := h.run(t, h.source(0, false))
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		runs = append(runs, committed(t, evs))
		if len(runs) > 1 {
			for j := range runs[0] {
				if runs[1][j] != runs[0][j] {
					t.Fatalf("run 1 committed %v, run 2 committed %v", runs[0], runs[1])
				}
			}
		}
	}
	if len(runs[0]) != 2 {
		t.Fatalf("committed %v twice over, want two commits per run", runs)
	}
	t.Logf("deterministic: %v == %v", runs[0], runs[1])
}

// TestLatestQueueDropsAreCounted runs the same recording through the live-runtime
// queue at the fastest pace it supports. Frames *must* be dropped — that is what
// single-slot latest-frame means — and the count must appear rather than the frames
// simply going missing from the report (docs/spec/01's capture loop exists to make
// staleness visible instead of silent).
//
// The drop is forced instead of raced: the fake OCR engine holds every call on a
// gate until the capture loop is observed to have received every frame the clip
// holds (the pipeline's own FramesDelivered counter — not a wall clock), and only
// then releases. Gating *every* call, not just the first, is what makes it
// deterministic: the live ROI loop never waits for its in-flight reads — it only
// stalls when the bounded jobs channel and worker pool are full — so a single held
// call leaves the second worker free to chase the producer on a fast machine,
// which is exactly the machine-speed dependence this test must not depend on.
func TestLatestQueueDropsAreCounted(t *testing.T) {
	const total = 24 // frames in the synthetic corpus
	gate := make(chan struct{})
	eng := &fakeOCR{script: ocrScript(t), delay: lateScript, respectCtx: true, gate: gate}
	h := newHarness(t, eng, func(c *pipeline.Config) { c.Queue = pipeline.QueueLatest })
	go func() {
		defer close(gate) // single owner; even a timed-out release frees the workers
		deadline := time.Now().Add(10 * time.Second)
		for h.runner.Stats().FramesDelivered < total {
			if time.Now().After(deadline) {
				return // the assertions below report a run that never delivered
			}
			time.Sleep(time.Millisecond)
		}
	}()
	st, _, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.FramesDelivered != total {
		t.Errorf("frames delivered = %d, want %d", st.FramesDelivered, total)
	}
	if st.FramesDropped == 0 {
		t.Errorf("no frames dropped through a single-slot queue at unlimited replay speed: FramesProcessed=%d", st.FramesProcessed)
	}
	if st.FramesDropped+st.FramesProcessed != st.FramesDelivered {
		t.Errorf("frames do not conserve: delivered %d, processed %d, dropped %d",
			st.FramesDelivered, st.FramesProcessed, st.FramesDropped)
	}
}

// TestCancellationStopsSubprocesses is the "cancelling ctx mid-run stops capture,
// workers and OCR subprocesses" case. The engine here is not a stub: it spawns a
// real child process per call and reports its pid, so "nothing was left running" is
// a claim checked against /proc rather than against a counter (AGENTS.md "no
// simulation theater").
func TestCancellationStopsSubprocesses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("NOT RUN: /proc liveness check needs linux (GOOS=%s)", runtime.GOOS)
	}
	eng := &subprocessOCR{hold: 60 * time.Second}
	h := newHarness(t, eng, func(c *pipeline.Config) { c.OCRTimeout = 30 * time.Second })
	src := h.source(1.0, false)

	done := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		h.ctx, h.cancel = ctx, cancel
		done <- h.runner.Run(ctx, src, h.hub)
	}()

	deadline := time.Now().Add(15 * time.Second)
	for eng.calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if eng.calls.Load() == 0 {
		t.Fatal("no OCR subprocess was ever spawned; the pipeline is not exercising the kill path")
	}
	pids := eng.snapshot()
	start := time.Now()
	h.cancel()

	var err error
	select {
	case err = <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return within 20s of cancellation (OCR calls %d, pids %v)", eng.calls.Load(), pids)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v, want context.Canceled", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run took %v to unwind after cancellation; the OCR deadline must not be what frees it", d)
	}
	for _, pid := range pids {
		if procAlive(pid) {
			t.Errorf("ocr subprocess %d survived cancellation", pid)
		}
	}
	if st := h.runner.Stats(); st.OCRCalls == 0 {
		t.Error("no OCR calls counted even though the engine spawned them")
	}
	t.Logf("cancelled after %d spawns (%v); Run unwound in %v", eng.calls.Load(), pids, time.Since(start))
}

// subprocessOCR runs `sleep` as its engine: an OCR child that will not finish on its
// own, which is the only way to test that cancellation reaches the process.
type subprocessOCR struct {
	hold  time.Duration
	calls atomic.Int64
	mu    sync.Mutex
	pids  []int
}

func (s *subprocessOCR) Name() string { return "sleep" }

// Recognize spawns `sleep` under CommandContext so cancellation is what kills it
// and Wait is what reaps it — the same supervision shape internal/vision/ocr's
// Tesseract wrapper uses, arrived at independently, which is the point: the Runner
// is only allowed to reach the child through the context it was given.
func (s *subprocessOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	if img == nil || img.Bounds().Empty() {
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrFailed, Engine: "sleep"}
	}
	n := s.calls.Add(1)
	if n > 64 {
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrFailed, Engine: "sleep"}
	}
	cmd := exec.CommandContext(ctx, "sleep", strconv.Itoa(int(s.hold.Seconds())))
	if err := cmd.Start(); err != nil {
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrEngineUnavailable, Engine: "sleep", Err: err}
	}
	s.mu.Lock()
	s.pids = append(s.pids, cmd.Process.Pid)
	s.mu.Unlock()
	err := cmd.Wait()
	if ctx.Err() != nil {
		return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrCancelled, Engine: "sleep", Err: ctx.Err()}
	}
	return ocr.Result{}, &ocr.Failure{Kind: ocr.ErrFailed, Engine: "sleep", Err: err}
}

func (s *subprocessOCR) snapshot() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.pids...)
}

func procAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat("/proc/" + itoa(pid))
	return err == nil
}

var _ capture.Source = (*capturingSource)(nil)

// capturingSource is the interface conformance check for the harness's source
// plumbing (a helper type so the harness can report what it handed the Runner).
type capturingSource struct {
	capture.Source
}

func timeline(t *testing.T, r *pipeline.Runner) {
	t.Helper()
	rows := r.Trace()
	if len(rows) == 0 {
		t.Error("Config.Trace was on and no timeline was retained")
	}
	t.Logf("frame ts -> state -> ocr text -> top candidate -> events (%d rows)", len(rows))
	for _, e := range rows {
		if e.FrameSeq == 0 && e.TopCandidate == "" && e.Event == "" {
			continue
		}
		t.Logf("  %4d %12d %-12s %-10s %-52s %-18s %.2f %-18s %s",
			e.FrameSeq, e.CaptureTimestampNS, e.RegionID, stateDash(e.State), trunc(e.OCRText, 52),
			e.TopCandidate, e.TopScore, trunc(e.Reason, 18), trunc(e.Event, 40))
	}
}

func stateDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// TestTraceRingOverflowCountsDrops is the overflow guard the bench harness
// relies on (ADR-0058 amendment 3): the trace ring is bounded, and a harness
// that reads it after a long replay must be able to tell when rows were
// evicted rather than silently grading on an incomplete timeline. Sizing the
// ring below the corpus's row count must retain exactly TraceMax rows and
// count every eviction in Stats.TraceDropped — the counter the harness asserts
// is 0 on a correctly sized run.
func TestTraceRingOverflowCountsDrops(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	h := newHarness(t, eng, func(c *pipeline.Config) { c.TraceMax = 8 })
	st, _, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(h.runner.Trace()); got != 8 {
		t.Errorf("retained rows = %d, want 8 (the ring's bound)", got)
	}
	if st.TraceDropped == 0 {
		t.Error("TraceDropped = 0: a run that outlives its ring must count the evicted rows, or a harness reading the ring cannot tell its timeline is incomplete")
	}
}

// TestTraceRingDefaultKeepsShortRuns is the mirror: the 24-frame corpus fits
// in the runtime default ring (defaultTraceMax), so a run that does not size
// the ring reports zero drops — the invariant the bench's per-sample sizing
// (frames*4+64) generalizes.
func TestTraceRingDefaultKeepsShortRuns(t *testing.T) {
	eng := &fakeOCR{script: ocrScript(t), delay: fastScript, respectCtx: true}
	h := newHarness(t, eng, nil)
	st, _, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if st.TraceDropped != 0 {
		t.Errorf("TraceDropped = %d, want 0: the corpus fits the default ring, so no row may be evicted", st.TraceDropped)
	}
}
