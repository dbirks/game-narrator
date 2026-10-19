package runtime_test

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/ffmpegpipe"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// deadXDisplay returns an X display number in 900-999 whose socket and lock
// are both absent — a display no server owns, so an x11grab against it fails
// the measured way ("Cannot open display") without touching a live server.
func deadXDisplay(t *testing.T) string {
	t.Helper()
	for n := 900; n <= 999; n++ {
		if _, err := os.Stat(fmt.Sprintf("/tmp/.X11-unix/X%d", n)); err == nil {
			continue
		}
		if _, err := os.Stat(fmt.Sprintf("/tmp/.X%d-lock", n)); err == nil {
			continue
		}
		return fmt.Sprintf(":%d", n)
	}
	t.Skipf("NOT RUN: no free X display number in :900-:999 for the dead-source regression")
	return ""
}

// TestSessionSnapshotStatsAreLive is the product regression for the stale
// session Stats: the stop path used to be the ONLY writer of s.info.Stats, so
// a running session's snapshot carried the zeros the struct was initialised
// with for the entire run. The Play page's Frames/OCR/Commits (and any
// watchdog reading them) therefore read 0 mid-run and jumped to the final
// totals only at stop — the 2026-10-19 live-play "0 frames delivered" page
// snapshot was this staleness, not a dead capture source. The snapshot now
// reads the pipeline's live counters, so a running session reports the frames
// it has actually delivered.
func TestSessionSnapshotStatsAreLive(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	info, err := e.start(e.replayStart(profileID, false))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRunning(t, e.mgr, info.ID)

	// Poll the live snapshot until the session ends, recording whether any
	// still-running snapshot carried a non-zero frames_delivered. The bug this
	// guards: every running snapshot read 0 (the counters were only written at
	// stop). The synthetic clip is ~2.4 s of 100 ms frames, so a running
	// snapshot with delivered frames is reliably observable.
	var (
		seenLive bool
		final    runtime.Info
	)
	by := time.Now().Add(30 * time.Second)
	for {
		got, ok := e.mgr.Get(info.ID)
		if !ok {
			break
		}
		final = got
		if got.State == runtime.StateRunning && got.Stats.FramesDelivered > 0 {
			seenLive = true
		}
		if got.State == runtime.StateStopped || got.State == runtime.StateFailed {
			break
		}
		if time.Now().After(by) {
			t.Fatalf("session %s did not reach a terminal state within 30s (last state %s)", info.ID, got.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !seenLive {
		t.Fatalf("no still-running snapshot carried a non-zero frames_delivered (final state=%s stats=%+v); the stale-Stats bug (always-0 mid-run counters) is back", final.State, final.Stats)
	}
	if final.State != runtime.StateStopped {
		t.Errorf("final state = %s (reason %q), want stopped", final.State, final.Reason)
	}
	if final.Stats.FramesDelivered == 0 {
		t.Errorf("final frames_delivered = 0, want >0 (the clip delivered frames): %+v", final.Stats)
	}
}

// TestSessionSnapshotNamesDeadSource is the product regression for the
// 2026-10-19 live-play failure: a dolphin-kind session whose x11grab child
// could not open the display ran "running" for 25 minutes with zero frames
// delivered and no fault named anywhere in the snapshot, so neither the
// operator nor the test could distinguish "the source is dead" from "the
// game has not spoken yet". The session's snapshot now carries the source's
// own self-report (state, delivered, restarts, last error), and this test
// proves the fault surfaces through the documented shape: the manual seam
// runs a real ffmpeg x11grab against a display no server owns, so the
// supervised child fails exactly the way the live one did — and the
// snapshot must say so while the session still reads "running" (a live
// source that cannot deliver is a reconnecting source, not a failed session;
// docs/spec/13 keeps the session up for the backoff and names the fault).
func TestSessionSnapshotNamesDeadSource(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("NOT RUN: ffmpeg not installed (the dead source must be a real x11grab, not a stub)")
	}
	display := deadXDisplay(t)

	e := newEnv(t, withManualSource(func() (capture.Source, error) {
		return ffmpegpipe.X11(display, 640, 480, 10, 0, 0)
	}))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	req := runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindManual},
		Mode:        runtime.ModeConservative,
	}
	info, err := e.start(req)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRunning(t, e.mgr, info.ID)

	// The x11grab child exits within milliseconds ("Cannot open display");
	// the supervisor's setState lands the fault on the source's stats, and
	// the snapshot must carry it while the session still runs.
	var got runtime.Info
	by := time.Now().Add(30 * time.Second)
	for {
		got, _ = e.mgr.Get(info.ID)
		if got.Source.LastError != "" {
			break
		}
		if time.Now().After(by) {
			t.Fatalf("the snapshot never named the source's fault (last: %+v); the dead-capture shape of 2026-10-19 returned", got.Source)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.HasPrefix(got.Source.ID, "x11:") {
		t.Errorf("source id = %q, want the x11 grab's identity", got.Source.ID)
	}
	if got.Source.Delivered != 0 {
		t.Errorf("delivered = %d, want 0 (the dead display emits nothing)", got.Source.Delivered)
	}
	if got.Source.State != "reconnecting" && got.Source.State != "failed" {
		t.Errorf("source state = %q, want reconnecting or failed (a dead capture is neither running nor silent)", got.Source.State)
	}
	if !strings.Contains(strings.ToLower(got.Source.LastError), "display") {
		t.Errorf("last_error = %q, want the X display failure named (docs/spec/13: an actionable fault, not \"it stopped\")", got.Source.LastError)
	}
	// The pipeline's downstream counters agree: nothing downstream of a dead
	// source can move.
	if got.Stats.FramesDelivered != 0 {
		t.Errorf("pipeline frames_delivered = %d, want 0 (the source delivered nothing)", got.Stats.FramesDelivered)
	}
	if got.State != runtime.StateRunning {
		t.Errorf("session state = %q, want running (the backoff keeps the session up; the fault is the source's, named above)", got.State)
	}

	// The terminal path: stopping a session whose source cannot deliver must
	// still finalize cleanly (the run context cancels the supervised child's
	// process group; a leaked ffmpeg is the ADR-0012 orphan bug).
	if _, err := e.mgr.Stop(info.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	final := waitState(t, e.mgr, info.ID, 30*time.Second)
	if final.State != runtime.StateStopped && final.State != runtime.StateFailed {
		t.Errorf("final state = %q (reason %q), want a terminal state", final.State, final.Reason)
	}
	// The fault survives to the terminal snapshot: the inspector reads the
	// ended run, and "why did it deliver nothing" must still be answerable.
	if final.Source.LastError == "" {
		t.Errorf("terminal snapshot lost the source fault (last_error empty); the ended run must keep its explanation")
	}
}
