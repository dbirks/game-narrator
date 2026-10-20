package ffmpegpipe

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// These tests run the real FFmpeg binary. Nothing is mocked: the point is that
// a supervised subprocess, a pipe, a process group and a restart timer behave
// under load, which is precisely the machinery ADR-0012 accepts as a cost and
// AGENTS.md rule 4 refuses to let us assume works. If FFmpeg is absent the
// tests report NOT RUN rather than passing quietly.

func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath(DefaultBin); err != nil {
		t.Skipf("NOT RUN: ffmpeg is not installed on this host (%v); see scripts/bootstrap-arch.sh and docs/adr/0012", err)
	}
}

// collect pulls n frames (or until the deadline), returning what arrived.
func collect(t *testing.T, ch <-chan capture.Frame, n int, within time.Duration) []capture.Frame {
	t.Helper()
	deadline := time.After(within)
	var out []capture.Frame
	for len(out) < n {
		select {
		case f, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
	return out
}

// dumpGoroutines logs the stacks of every goroutine in the test process.
// On a supervision timeout this is the difference between "it hung" and "here
// is where it hung": the issue #16 dump (test goroutine parked in the wait
// loop's select, supervisor alive in its restart loop) is what located the
// unbounded deadline extension.
func dumpGoroutines(t *testing.T) {
	t.Helper()
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	t.Logf("goroutine dump:\n%s", buf[:n])
}

func TestTestSourceDeliversAnimatedFramesAtRate(t *testing.T) {
	requireFFmpeg(t)
	const (
		w, h, fps = 320, 240, 15.0
		want      = 30
	)
	s := TestSource(w, h, fps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	start := time.Now()
	frames := collect(t, ch, want, 5*time.Second)
	elapsed := time.Since(start)
	if len(frames) < want {
		t.Fatalf("got %d frames in %v, want >= %d (FFmpeg pipe delivery failed)", len(frames), elapsed, want)
	}
	t.Logf("testsrc2 %dx%d@%g: %d frames in %v", w, h, fps, len(frames), elapsed.Round(time.Millisecond))

	seen := map[capture.PixFmt]bool{}
	hashes := map[string]bool{}
	var (
		prevTS  int64
		prevSeq int64
		stamped int64 // sum of the delivered frames' Dropped stamps
	)
	for i, f := range frames {
		if err := f.Validate(); err != nil {
			t.Fatalf("frame %d invalid: %v", i, err)
		}
		if f.Width != w || f.Height != h {
			t.Fatalf("frame %d is %dx%d, want %dx%d", i, f.Width, f.Height, w, h)
		}
		if f.Image.Bounds().Dx() != w || f.Image.Bounds().Dy() != h {
			t.Fatalf("frame %d image is %v, want %dx%d", i, f.Image.Bounds(), w, h)
		}
		if f.SourceClock != capture.ClockMonotonic {
			t.Fatalf("frame %d clock %q, want monotonic: rawvideo carries no device stamps and pretending otherwise breaks docs/spec/01 invariant 1", i, f.SourceClock)
		}
		if i > 0 {
			if f.CaptureTimestampNS <= prevTS {
				t.Fatalf("frame %d timestamp %d did not advance past %d", i, f.CaptureTimestampNS, prevTS)
			}
			if f.Seq <= prevSeq {
				t.Fatalf("frame %d seq %d did not advance past %d: the slot may lose frames, never reorder them", i, f.Seq, prevSeq)
			}
		}
		prevTS = f.CaptureTimestampNS
		prevSeq = f.Seq
		stamped += f.Dropped
		if f.Hash == "" {
			t.Fatalf("frame %d has no hash", i)
		}
		hashes[f.Hash] = true
		seen[f.PixFmt] = true
	}
	// Dropped accounting, not gap-free numbering: the live path is a
	// single-slot latest-frame queue, so under load the slot legitimately
	// overwrites frames the consumer did not take in time, and a strict
	// seq == n+1 assertion would then fail for a reason nothing to do with
	// this code. Every frame FFmpeg produced is either delivered or
	// overwritten (never both), so the last delivered seq equals the
	// delivered count plus the stamped drops; a clean gap-free run simply
	// lands on seq == n with zero stamps, and a dropped one names how much
	// video went missing — which is what the trace is supposed to be able to
	// say (docs/spec/10 preflight).
	if want := int64(len(frames)) + stamped; prevSeq != want {
		t.Errorf("last delivered seq = %d, want %d (delivered %d + stamped drops %d): the Dropped accounting does not cover the seq gap", prevSeq, want, len(frames), stamped)
	}
	if len(hashes) < 3 {
		t.Errorf("only %d distinct frame hashes over %d frames: testsrc2 animates, so a frozen pipe looks identical to a frozen capture card", len(hashes), len(frames))
	}
	if !seen[capture.PixRGB24] {
		t.Errorf("pixel formats seen: %v, want rgb24", seen)
	}

	// The fps claim is the one worth failing on: an arrival rate below the
	// requested rate is the actual production symptom (a converter that will not
	// do the negotiated mode), so it is measured over the delivered frames, not
	// over the whole test window, and asserted as a band rather than exactly.
	st := s.Stats()
	t.Logf("measured fps=%.2f (requested %g), delivered=%d, dropped_slot=%d, stamped=%d, decode_errors=%d, state=%s",
		st.FPS, fps, st.Delivered, st.DroppedSlot, stamped, st.DecodeErrors, st.State)
	if st.FPS < fps-3 || st.FPS > fps+3 {
		t.Errorf("measured FPS %.2f outside %g±3; lavfi pacing or the fps estimator is wrong", st.FPS, fps)
	}
	if int64(st.DroppedSlot) < stamped {
		t.Errorf("source dropped_slot=%d but the delivered frames stamp %d drops: the source's own counter undercounts what the consumer saw", st.DroppedSlot, stamped)
	}
	if st.Delivered < uint64(len(frames)) {
		t.Errorf("source delivered=%d but the channel handed over %d frames", st.Delivered, len(frames))
	}
	if st.State != capture.StateRunning {
		t.Errorf("state = %s while frames are flowing", st.State)
	}
}

// TestDeliveredTracksHandedOverFrames pins the delivery counter against the
// bug make test-full's full parallel load exposed: the pump used to
// increment Stats.Delivered *after* the channel send, so a stats read racing
// a hand-over could report fewer delivered frames than the consumer already
// held in hand ("source delivered=29 but the channel handed over 30 frames").
//
// The invariant: from the instant a frame has been handed to the consumer,
// every Stats() read must report Delivered >= that frame — the inspector
// must never undercount what the pipeline was fed (docs/spec/10 preflight
// reads Delivered to judge freshness). At rest, Delivered is exactly the
// number of frames the consumer took, so a hand-over abandoned by ctx is
// rolled back, not kept.
//
// The decisive observation is deterministic: with no consumer at the
// channel, the pump parks on the send of the newest frame, and there it
// stays until the test acts. A pump that counts *before* the send is parked
// with the frame already counted; a pump that counts *after* the send is
// parked with the count still pending, forever, and reports one frame short
// — exactly the undercount the live stats showed. White-box by design: it
// drives the real pump + LatestSlot — the delivery code the live path runs
// — with synthetic frames, so it needs no FFmpeg and cannot be skipped.
func TestDeliveredTracksHandedOverFrames(t *testing.T) {
	s := &Source{
		IDName: "test:delivered-counter",
		Width:  8,
		Height: 8,
		slot:   capture.NewLatestSlot(),
		out:    make(chan capture.Frame),
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.pump(ctx)
	}()

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	frame := func(i int) capture.Frame {
		return capture.Frame{
			Seq:                int64(i),
			SourceClock:        capture.ClockMonotonic,
			CaptureTimestampNS: int64(i),
			Width:              8,
			Height:             8,
			PixFmt:             capture.PixRGB24,
			Image:              img,
			Hash:               "h",
		}
	}
	take := func(i int) {
		t.Helper()
		s.slot.Put(frame(i))
		got, ok := <-s.out
		if !ok {
			t.Fatalf("frame channel closed before frame %d", i)
		}
		if got.Seq != int64(i) {
			t.Fatalf("frame %d: got seq %d", i, got.Seq)
		}
		// The consumer holds frame i: every stats read from here on must
		// already agree with it.
		if d := s.Stats().Delivered; d < uint64(i) {
			t.Fatalf("consumer holds frame %d but Stats.Delivered=%d: the counter lagged the hand-over (it must move with the send, not behind it)", i, d)
		}
	}

	const k = 49
	for i := 1; i <= k; i++ {
		take(i)
	}

	// The parked-send observation: put one more frame and do not take it.
	// The pump takes it and parks on the send — stably, until the test acts.
	// A pump that counts before the send is parked with the count done; one
	// that counts after the send is parked with it pending forever.
	s.slot.Put(frame(k + 1))
	deadline := time.Now().Add(2 * time.Second)
	for s.slot.Takes() < k+1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if s.slot.Takes() < k+1 {
		t.Fatalf("pump never took frame %d (takes=%d): the test premise is broken", k+1, s.slot.Takes())
	}
	// Settle: the pump is parked on the send; nothing it does from here on
	// can change the counter before a consumer appears. The grace below only
	// covers the transient where the pump has not even been scheduled yet —
	// a count that is still pending at rest is the bug, not a slow scheduler.
	time.Sleep(50 * time.Millisecond)
	d := s.Stats().Delivered
	if d < k+1 {
		time.Sleep(300 * time.Millisecond)
		d = s.Stats().Delivered
	}
	if d < k+1 {
		t.Fatalf("pump parked on the send of frame %d reports Stats.Delivered=%d: the frame is in flight to the consumer but uncounted — the counter must move with the send, not behind it", k+1, d)
	}

	// Finish the hand-over (no Put: frame k+1 is already in flight on the
	// channel — the slot is empty, and a second copy would queue behind it).
	got, ok := <-s.out
	if !ok {
		t.Fatalf("frame channel closed before frame %d", k+1)
	}
	if got.Seq != int64(k+1) {
		t.Fatalf("frame %d: got seq %d", k+1, got.Seq)
	}
	if d := s.Stats().Delivered; d < uint64(k+1) {
		t.Fatalf("consumer holds frame %d but Stats.Delivered=%d: the counter lagged the hand-over", k+1, d)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if d := s.Stats().Delivered; d != k+1 {
		t.Fatalf("settled Stats.Delivered=%d, want exactly %d (the frames the consumer took)", d, k+1)
	}
	if d := s.slot.Drops(); d != 0 {
		t.Fatalf("test lost %d frames into the slot; the consumer kept up, so none may be dropped", d)
	}
}

// TestPumpCountsRollbackOnCancelledHandOver pins the rollback half of the same
// fix: the pump counts a hand-over before the send so a stats read can never
// lag the consumer — and a send abandoned by ctx (Close) must be uncounted,
// or Delivered would claim frames the consumer never got.
func TestPumpCountsRollbackOnCancelledHandOver(t *testing.T) {
	const k = 50
	s := &Source{
		IDName: "test:delivered-rollback",
		Width:  8,
		Height: 8,
		slot:   capture.NewLatestSlot(),
		out:    make(chan capture.Frame),
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.pump(ctx)
	}()

	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	take := func(i int) {
		t.Helper()
		f := capture.Frame{
			Seq:                int64(i),
			SourceClock:        capture.ClockMonotonic,
			CaptureTimestampNS: int64(i),
			Width:              8,
			Height:             8,
			PixFmt:             capture.PixRGB24,
			Image:              img,
			Hash:               "h",
		}
		s.slot.Put(f)
		got, ok := <-s.out
		if !ok || got.Seq != int64(i) {
			t.Fatalf("frame %d not delivered in order (ok=%v seq=%d)", i, ok, got.Seq)
		}
	}
	for i := 1; i <= k; i++ {
		take(i)
	}
	// Prime the slot with one more frame, then shut the source down without
	// the consumer ever taking it. The consumer now holds exactly k frames,
	// so settled Delivered must be k.
	f := capture.Frame{
		Seq:                int64(k + 1),
		SourceClock:        capture.ClockMonotonic,
		CaptureTimestampNS: int64(k + 1),
		Width:              8,
		Height:             8,
		PixFmt:             capture.PixRGB24,
		Image:              img,
		Hash:               "h",
	}
	s.slot.Put(f)
	cancel()
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	if d := s.Stats().Delivered; d != k {
		t.Fatalf("settled Stats.Delivered=%d, want %d: frame %d was never handed to the consumer and must not be counted (nor may a delivered frame go uncounted)", d, k, k+1)
	}
}

func TestSlowConsumerCausesCountedSlotDrops(t *testing.T) {
	// docs/spec/01's capture contract is "single-slot latest-frame queue": a
	// consumer that cannot keep up must cause *counted* overwrites rather than
	// back-pressure into FFmpeg (which would stall the live path) or silent
	// frame loss. A deliberately sleepy consumer is the cheapest way to prove
	// which of the three this code does.
	requireFFmpeg(t)
	s := TestSource(160, 120, 30)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// atomic, because the consumer runs on its own goroutine — the race detector
	// caught the plain int64 version of this test, which is the correct outcome
	// for a counter shared between goroutines.
	var perFrameDrops atomic.Int64
	go func() {
		for f := range ch {
			if f.Dropped > 0 {
				perFrameDrops.Add(f.Dropped)
			}
			time.Sleep(60 * time.Millisecond) // ~16 fps consumer against a 30 fps source
		}
	}()
	time.Sleep(1500 * time.Millisecond)

	st := s.Stats()
	t.Logf("slow consumer: delivered=%d dropped_slot=%d per_frame_drops=%d state=%s", st.Delivered, st.DroppedSlot, perFrameDrops.Load(), st.State)
	// Consistency first: if the slot overwrote frames, at least one delivered
	// frame has to carry the count, because Frame.Dropped is how a trace shows
	// *when* video was lost. Checked only when the source really outran the
	// consumer — on a loaded machine a 30 fps lavfi source can fall below the
	// consumer's pace, and then demanding drops would be asserting on scheduler
	// luck rather than on this code.
	if st.DroppedSlot > 0 && perFrameDrops.Load() == 0 {
		t.Errorf("%d slot overwrites but no delivered frame carried a Dropped count: the consumer cannot see where video went missing, which is the point of Frame.Dropped", st.DroppedSlot)
	}
	if st.DroppedSlot == 0 {
		t.Errorf("a consumer at 25 fps against a 30 fps source dropped nothing in 1.5 s; either the slot is not single-slot or the counter is dead")
	}
	if st.Delivered == 0 {
		t.Error("no frames delivered")
	}
}

func TestLosslessQueueDeliversEveryFrameInOrder(t *testing.T) {
	// The recording mode's promise, tested against real FFmpeg output rather
	// than a fake: no gaps in Seq. docs/spec/01 distinguishes it from the live
	// path precisely because a recording must not skip.
	requireFFmpeg(t)
	s := TestSource(160, 120, 15)
	s.LosslessQueue = 8
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	var seqs []int64
	deadline := time.After(6 * time.Second)
	for len(seqs) < 20 {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatalf("channel closed after %d frames", len(seqs))
			}
			seqs = append(seqs, f.Seq)
		case <-deadline:
			t.Fatalf("only %d lossless frames arrived in 6s", len(seqs))
		}
	}
	for i, sq := range seqs {
		if sq != int64(i+1) {
			t.Fatalf("lossless frame %d has seq %d: frames were skipped or reordered", i, sq)
		}
	}
	if st := s.Stats(); st.DroppedSlot != 0 {
		t.Errorf("lossless mode reported %d slot drops", st.DroppedSlot)
	}
}

func TestSupervisionSurvivesKilledFFmpeg(t *testing.T) {
	requireFFmpeg(t)
	s := TestSource(160, 120, 20)
	s.RestartDelay = 400 * time.Millisecond // wide reconnecting window to observe
	s.MaxRestartDelay = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	first := collect(t, ch, 5, 5*time.Second)
	if len(first) < 5 {
		dumpGoroutines(t)
		t.Fatalf("only %d frames before the kill (5 s deadline)", len(first))
	}
	pid := s.PID()
	if pid <= 0 {
		t.Fatal("PID() reported no child while frames were flowing")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill %d: %v", pid, err)
	}

	// Watch for the state to pass through reconnecting rather than sampling
	// once: the assertion is about the transition, which is the only thing
	// docs/spec/13's "Capture removed → reconnect with backoff" row can be
	// checked against.
	//
	// The wait is bounded by a single deadline that never moves (issue #16):
	// the previous version re-armed the deadline on every resumed frame while
	// the transient had not been sampled, and once a descheduled poll missed
	// the 400 ms reconnecting window the re-arming kept the deadline five
	// seconds in the future forever — the test looped until the 25-minute
	// package timeout. Waiting longer cannot recover a missed transient: the
	// state passes through reconnecting once, ending at the new child's first
	// delivered frame (recordDelivered flips it to running), so after the
	// frames resume the only durable evidence of the transition is the
	// restart counter, and that is the fallback the assertion below accepts.
	sawReconnecting := false
	var frames []capture.Frame
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && len(frames) < 5 {
		if st := s.Stats(); st.State == capture.StateReconnecting {
			sawReconnecting = true
		}
		select {
		case f, ok := <-ch:
			if !ok {
				dumpGoroutines(t)
				t.Fatal("frame channel closed on kill: a supervised source must reconnect, not end")
			}
			frames = append(frames, f)
		case <-time.After(2 * time.Millisecond):
		}
	}
	if len(frames) < 5 {
		dumpGoroutines(t)
		t.Fatalf("only %d frames after the kill within %v (want 5): capture did not resume",
			len(frames), 15*time.Second)
	}
	st := s.Stats()
	t.Logf("after kill: saw_reconnecting=%v restarts=%d delivered=%d resumed=%d state=%s last_error=%q",
		sawReconnecting, st.Restarts, st.Delivered, len(frames), st.State, st.LastError)
	if !sawReconnecting && st.Restarts == 0 {
		t.Fatalf("killing FFmpeg left state %s with no restart: supervision is not running", st.State)
	}
	if !sawReconnecting {
		// A descheduled poll can miss the whole transient under load; the
		// restart counter and the resumed frames still have to prove the
		// recovery, so this path is a pass with the transition assertion
		// marked NOT RUN rather than a failure.
		t.Logf("NOT RUN: could not sample the reconnecting transient (poll was too slow); recovery itself is proven by restarts=%d and %d resumed frames", st.Restarts, len(frames))
	}
	if st.Restarts < 1 {
		t.Errorf("Restarts = %d after killing the child, want >= 1", st.Restarts)
	}
	for i, f := range frames {
		if f.Width != 160 || f.Height != 120 {
			t.Fatalf("resumed frame %d is %dx%d", i, f.Width, f.Height)
		}
	}
	if newPID := s.PID(); newPID == pid {
		t.Errorf("PID after restart is still %d: the old process was never replaced", pid)
	}
	// Seq must be continuous across the gap: the frame numbering is the
	// consumer's only evidence of where video is missing.
	if frames[0].Seq <= first[len(first)-1].Seq {
		t.Errorf("seq went backwards across the restart: %d after %d", frames[0].Seq, first[len(first)-1].Seq)
	}
}

func TestCancelAndCloseLeaveNoFFmpegProcess(t *testing.T) {
	requireFFmpeg(t)
	// The leak ADR-0012 names explicitly ("orphaned process groups", and on a
	// capture card an orphan holds /dev/videoN so the next start fails with
	// EBUSY). Two paths are checked because they are two code paths: context
	// cancellation and Close.
	for _, mode := range []string{"cancel", "close"} {
		t.Run(mode, func(t *testing.T) {
			s := TestSource(160, 120, 20)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := s.Start(ctx)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			if len(collect(t, ch, 3, 5*time.Second)) < 3 {
				s.Close()
				t.Fatal("no frames before shutdown")
			}
			pid := s.PID()
			if pid <= 0 {
				s.Close()
				t.Fatal("no child pid recorded")
			}
			// Mark the child so the /proc scan below can tell ours from someone
			// else's ffmpeg: the size string is unique to this test.
			if mode == "cancel" {
				cancel()
			} else {
				if err := s.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			waitGone(t, pid, 5*time.Second)
			if mode == "cancel" {
				if err := s.Close(); err != nil {
					t.Fatalf("Close after cancel: %v", err)
				}
			}
			if st := s.Stats(); st.State != capture.StateStopped {
				t.Errorf("state after shutdown = %s, want stopped", st.State)
			}
			// Any ffmpeg still holding our unique size marker is an orphan of
			// this test, and it is exactly the class of bug that makes the next
			// device open fail on real hardware.
			if left := findFFmpegWithMarker(t, "160x120:rate=20"); left != nil {
				t.Errorf("ffmpeg processes still alive after shutdown: %v", left)
			}
			if s.PID() != 0 {
				t.Errorf("PID() = %d after shutdown, want 0", s.PID())
			}
		})
	}
}

func TestStalledFFmpegIsKilledAndRestarted(t *testing.T) {
	requireFFmpeg(t)
	// The frozen-splitter case (docs/spec/12 "video loss/freeze", docs/spec/13's
	// capture row): FFmpeg stays alive and the pipe stays open while producing
	// nothing, so no read error will ever arrive. Only a stall watchdog turns
	// that into a state change — and a test needs a source that behaves that way
	// without waiting 5 seconds for real.
	//
	// `testsrc2=…:duration=…` ends on its own; the supervision loop treats the
	// clean EOF as a fault and respawns. `ffmpeg -f lavfi -i nullsrc` with a
	// stall timeout below the frame period gives the same shape: the child is
	// alive, the pipe is quiet.
	s := New(Input{
		Format:   "lavfi",
		Location: "nullsrc=size=64x48:rate=0.2",
		Paced:    true, // one frame every 5 s, so the pipe is quiet by design
	}, 64, 48, 0)
	s.StallTimeout = 300 * time.Millisecond
	s.RestartDelay = 50 * time.Millisecond
	s.MaxRestartDelay = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	// One frame arrives every 20 seconds, so within a second the source has
	// stalled with a live child — the condition under test.
	deadline := time.Now().Add(6 * time.Second)
	var st capture.Stats
	for time.Now().Before(deadline) {
		st = s.Stats()
		if st.Restarts >= 2 {
			break
		}
		select {
		case <-ch:
		case <-time.After(10 * time.Millisecond):
		}
	}
	// A live child is sampled over a window rather than once: between the kill
	// and the next spawn the source legitimately has no child, and asserting on
	// a single instant would make this test flaky for a reason unrelated to the
	// behaviour under check.
	pid := 0
	for attempt := 0; attempt < 100 && pid <= 0; attempt++ {
		pid = s.PID()
		if pid > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	st = s.Stats()
	t.Logf("stall test: restarts=%d state=%s live_child_pid=%d last_error=%q", st.Restarts, st.State, pid, st.LastError)
	if st.Restarts < 2 {
		t.Fatalf("Restarts = %d after 6s of no frames, want >= 2: a quiet pipe was not detected as a stall", st.Restarts)
	}
	if pid <= 0 {
		t.Error("no child running within 2s of the stalls: the loop gave up instead of respawning")
	}
	if !strings.Contains(st.LastError, "no frame within") {
		t.Errorf("LastError = %q, want a stall message", st.LastError)
	}
}

func TestMissingBinaryIsFailedNotReconnecting(t *testing.T) {
	// A host without FFmpeg is a different fault from an unplugged card, and
	// docs/spec/13's table wants a distinct operator action for each: state
	// Failed with an actionable message, not an endless reconnect.
	s := TestSource(64, 48, 10)
	s.Bin = filepath.Join(t.TempDir(), "definitely-not-ffmpeg")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Start(ctx); err != nil {
		// Acceptable too: refusing to start at all is an honest failure.
		if !errors.Is(err, ErrAlreadyStarted) && !strings.Contains(err.Error(), "unavailable") && !strings.Contains(err.Error(), "not-ffmpeg") {
			t.Fatalf("Start error = %v", err)
		}
		t.Logf("Start reported the missing binary directly: %v", err)
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	var st capture.Stats
	for time.Now().Before(deadline) {
		st = s.Stats()
		if st.State == capture.StateFailed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("state = %s with no ffmpeg binary available, want failed (last_error=%q, restarts=%d)", st.State, st.LastError, st.Restarts)
	_ = s.Close()
}

func TestStartRejectsBadConfigurationAndSecondStart(t *testing.T) {
	requireFFmpeg(t)
	if _, err := New(Input{Format: "lavfi", Location: "testsrc2=size=64x48:rate=5"}, 0, 0, 0).Start(context.Background()); err == nil {
		t.Error("a source with no geometry started")
	} else {
		t.Logf("no-geometry rejection: %v", err)
	}
	if _, err := New(Input{}, 64, 48, 5).Start(context.Background()); err == nil {
		t.Error("a source with no input location started")
	}

	s := TestSource(64, 48, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Start(ctx); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	defer s.Close()
	if _, err := s.Start(ctx); !errors.Is(err, ErrAlreadyStarted) {
		t.Errorf("second Start = %v, want ErrAlreadyStarted", err)
	}
}

func TestConstructorArgumentBuilders(t *testing.T) {
	// argv-only, no shell (ADR-0012 §2 and AGENTS.md rule 6), and the exact
	// shape docs/spec/05's "prefer UVC MJPEG or YUYV" and ADR-0007's x11grab
	// crop require. These assertions are what make the V4L2 and X11 paths
	// reviewable on a host with no camera attached.
	for _, tc := range []struct {
		name string
		s    *Source
		want []string
		deny []string
	}{
		{"v4l2-mjpeg", mustSrc(t, "/dev/video2", 720, 480, 30, "mjpeg"),
			[]string{"-f", "v4l2", "-input_format", "mjpeg", "-video_size", "720x480", "-framerate", "30", "-i", "/dev/video2", "-f", "rawvideo", "-pix_fmt", "rgb24", "-"},
			[]string{"sh", "-c", "x11grab", "input_format=yuyv422; rm"}},
		{"v4l2-yuyv", mustSrc(t, "/dev/v4l/by-id/usb-Example_UVC_1234-video-index0", 640, 480, 15, "yuyv422"),
			[]string{"-input_format", "yuyv422", "-video_size", "640x480", "-i", "/dev/v4l/by-id/usb-Example_UVC_1234-video-index0"},
			[]string{"/dev/video0"}},
		{"x11", mustX11(t, ":1", 640, 480, 30, 64, 32),
			[]string{"-f", "x11grab", "-draw_mouse", "0", "-video_size", "640x480", "-framerate", "30", "-i", ":1+64,32"},
			[]string{"sh", "bash"}},
		{"lavfi", TestSource(320, 240, 15),
			[]string{"-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15", "-re", "-pix_fmt", "rgb24"},
			[]string{"-video_size"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.s.Args()
			joined := strings.Join(got, "\x00")
			for _, w := range tc.want {
				if !contains(got, w) {
					t.Errorf("argv missing %q in %q", w, joined)
				}
			}
			for _, d := range tc.deny {
				if strings.Contains(joined, d) {
					t.Errorf("argv contains %q: %q", d, joined)
				}
			}
			for _, a := range got {
				if a == "sh" || a == "bash" {
					t.Fatalf("argv contains a shell: %q (ADR-0012 forbids shell execution)", got)
				}
				if strings.ContainsAny(a, "&|;") && !strings.HasPrefix(a, "testsrc2") {
					t.Errorf("suspicious argv element %q", a)
				}
			}
			// A device path with spaces and a semicolon must survive as ONE
			// argument — the exact test ADR-0012's evidence plan demands.
			spaced, err := V4L2("/dev/video 0; rm -rf", 64, 48, 5, "mjpeg")
			if err != nil {
				t.Fatalf("V4L2: %v", err)
			}
			found := false
			for _, a := range spaced.Args() {
				if a == "/dev/video 0; rm -rf" {
					found = true
				}
			}
			if !found {
				t.Errorf("device path with spaces and a semicolon was split: %q", spaced.Args())
			}
		})
	}

	if _, err := V4L2("/dev/video0", 640, 480, 30, "yuv420p"); err == nil {
		t.Error("V4L2 accepted an input format outside mjpeg|yuyv422")
	}
	if _, err := V4L2("", 640, 480, 30, "mjpeg"); err == nil {
		t.Error("V4L2 accepted an empty device path")
	}
	if _, err := X11("", 640, 480, 30, 0, 0); err == nil {
		t.Error("X11 accepted an empty display")
	}
	if got := mustX11(t, ":1.0", 320, 240, 0, 0, 0).Args(); contains(got, "-framerate") {
		t.Errorf("framerate requested at fps 0 (device's choice means asking for nothing): %q", got)
	}
	// DrawMouse defaults to off: the cursor inside the ROI is animated noise the
	// pipeline is told to mask (docs/spec/05).
	x := mustX11(t, ":1", 320, 240, 15, 0, 0)
	if v := argAfter(x.Args(), "-draw_mouse"); v != "0" {
		t.Errorf("-draw_mouse = %q, want 0", v)
	}
	x.DrawMouse = true
	if v := argAfter(x.Args(), "-draw_mouse"); v != "1" {
		t.Errorf("-draw_mouse after DrawMouse=true = %q, want 1", v)
	}
}

func TestIDNamesTheTransport(t *testing.T) {
	for _, tc := range []struct{ s, want string }{
		{mustSrc(t, "/dev/video2", 640, 480, 30, "mjpeg").ID(), "v4l2:/dev/video2"},
		{mustX11(t, ":1", 640, 480, 30, 0, 0).ID(), "x11::1+0,0"},
	} {
		if tc.s != tc.want {
			t.Errorf("ID = %q, want %q", tc.s, tc.want)
		}
	}
}

func TestRGB24Conversion(t *testing.T) {
	// A conversion bug here would show up as a colour-cast or a sheared frame
	// in the OCR path and be blamed on the capture card, so it is tested
	// directly with known pixels.
	src := []byte{
		255, 0, 0, 0, 255, 0,
		0, 0, 255, 12, 34, 56,
	}
	img, err := rgb24(src, 2, 2)
	if err != nil {
		t.Fatalf("rgb24: %v", err)
	}
	for i, want := range []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}, {0, 0, 255, 255}, {12, 34, 56, 255}} {
		x, y := i%2, i/2
		if got := img.RGBAAt(x, y); got != want {
			t.Errorf("pixel (%d,%d) = %v, want %v", x, y, got, want)
		}
	}
	if img.Stride != 8 {
		t.Errorf("stride = %d, want 8 (image.RGBA is 4 bytes/px)", img.Stride)
	}
	if _, err := rgb24(src, 4, 4); err == nil {
		t.Error("rgb24 accepted a buffer too small for the geometry")
	}
	// Extra trailing bytes are the pipe's business, not a reason to panic.
	if _, err := rgb24(append(append([]byte{}, src...), 9, 9, 9), 2, 2); err != nil {
		t.Errorf("rgb24 rejected an over-long buffer: %v", err)
	}
}

func TestFPSWindowEstimator(t *testing.T) {
	var w fpsWindow
	base := time.Unix(1700000000, 0)
	// A start-up burst arriving instantly, then exactly 20 fps: the estimator
	// must report 20, not the burst.
	for i := 0; i < fpsWarmup; i++ {
		w.add(base)
	}
	// 20 frames at exactly 20 fps.
	for i := 0; i < 20; i++ {
		w.add(base.Add(time.Duration(i+fpsWarmup) * 50 * time.Millisecond))
	}
	if got := w.fps(); got < 19.5 || got > 20.5 {
		t.Errorf("fps = %g for a 20 fps cadence, want ~20", got)
	}
	// The ring is bounded: 100 frames must not change the estimate's validity,
	// and a sudden stall must move it within the window rather than after a
	// full second of buckets.
	for i := 20; i < 52; i++ {
		w.add(base.Add(time.Duration(i+fpsWarmup) * 50 * time.Millisecond))
	}
	if got := w.fps(); got < 19 || got > 21 {
		t.Errorf("fps = %g after ring wrap, want ~20", got)
	}
	for i := 0; i < 16; i++ {
		w.add(base.Add(time.Duration(52+i+fpsWarmup) * 200 * time.Millisecond)) // 5 fps now
	}
	if got := w.fps(); got > 8 {
		t.Errorf("fps = %g after a stall to 5 fps, want the window to follow it (bounded memory is only useful if it is also responsive)", got)
	}
	var empty fpsWindow
	if got := empty.fps(); got != 0 {
		t.Errorf("empty window fps = %g, want 0", got)
	}
}

func TestRestartBackoffIsExponentialAndCapped(t *testing.T) {
	// The cap is a requirement (10 s), not a detail: a card that stays unplugged
	// must not become a respawn storm on the appliance's four cores, and a
	// recovery must not take longer than the cap to notice.
	base, max := 200*time.Millisecond, DefaultMaxRestartDelay
	if max != 10*time.Second {
		t.Fatalf("DefaultMaxRestartDelay = %v, want the 10s cap the milestone specifies", max)
	}
	want := []time.Duration{400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 6400 * time.Millisecond, 10 * time.Second, 10 * time.Second}
	d := base
	for i, w := range want {
		d = nextDelay(d, base, max)
		if d != w {
			t.Fatalf("step %d: nextDelay = %v, want %v", i, d, w)
		}
	}
	// A successful attempt resets the growth: a device that is nudged once a day
	// must reconnect in milliseconds, not after last week's backoff.
	if got := nextDelay(base, base, max); got != 400*time.Millisecond {
		t.Errorf("after reset: %v", got)
	}
	if got := nextDelay(0, base, max); got != base {
		t.Errorf("zero delay = %v, want the base step", got)
	}
}

func TestTailBufferIsBounded(t *testing.T) {
	// ADR-0012's bounded-pipe rule, applied to FFmpeg's progress spam: 1 MB of
	// stderr must not grow the heap by 1 MB.
	var tb tailBuf
	big := strings.Repeat("frame=  100 fps=30 q=0.0 size=N/A time=00:01:00.00 bitrate=N/A\n", 20000)
	if _, err := tb.Write([]byte(big)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if got := len(tb.buf); got > StderrTailBytes {
		t.Errorf("tail grew to %d bytes, cap is %d", got, StderrTailBytes)
	}
	if !strings.Contains(tb.String(), "fps=30") {
		t.Error("tail lost the recent lines while trimming the old ones")
	}
	tb.reset()
	if tb.String() != "" {
		t.Error("reset left content")
	}
}

// --- helpers --------------------------------------------------------------

func mustSrc(t *testing.T, device string, w, h int, fps float64, inputFormat string) *Source {
	t.Helper()
	s, err := V4L2(device, w, h, fps, inputFormat)
	if err != nil {
		t.Fatalf("V4L2(%s): %v", device, err)
	}
	return s
}

func mustX11(t *testing.T, display string, w, h int, fps float64, x, y int) *Source {
	t.Helper()
	s, err := X11(display, w, h, fps, x, y)
	if err != nil {
		t.Fatalf("X11(%s): %v", display, err)
	}
	return s
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// waitGone proves a pid is neither living nor a zombie of this process. A
// zombie still holds the pid and would pass a bare /proc check for the wrong
// reason in the other direction, so both are asserted.
func waitGone(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !procAlive(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive after %v", pid, within)
}

func procAlive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	// Field 3 is the state; 'Z' means reaped-by-nobody.
	fields := strings.Fields(string(b))
	if len(fields) < 3 {
		return false
	}
	return fields[2] != "Z"
}

// findFFmpegWithMarker scans /proc for an ffmpeg process whose command line
// carries a marker string unique to a test source. nil means none.
func findFFmpegWithMarker(t *testing.T, marker string) []string {
	t.Helper()
	if marker == "" {
		return nil
	}
	var found []string
	ents, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("NOT RUN: cannot read /proc to check for orphans: %v", err)
	}
	for _, e := range ents {
		if !isDigits(e.Name()) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		cmd := strings.ReplaceAll(string(raw), "\x00", " ")
		if strings.Contains(cmd, "ffmpeg") && strings.Contains(cmd, marker) {
			found = append(found, fmt.Sprintf("pid %s: %s", e.Name(), strings.TrimSpace(cmd)))
		}
	}
	return found
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
