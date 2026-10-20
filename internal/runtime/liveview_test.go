package runtime_test

// The Manager's live capture-session view (ADR-0110 §1/§2): a running v4l2
// session reports its live state — the session id, the held device, the
// negotiated format and target fps, the source's self-report, the
// active-picture tracker's window, the last dialogue-region crop and the
// newest in-memory frame — and a finished session reports nothing (the view
// is the session's, released with it). The API's signal and preview routes
// read this view; the seam is what this test pins.

import (
	"context"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// TestCaptureLiveReportsTheRunningSession pins the view's contract on a
// real v4l2 session (the push seam standing in for the dongle): before the
// first delivery it holds no frame and no crop; after the run processes
// frames it holds the newest frame, the tracker's window and the dialogue
// region's last crop; after the session ends it answers nothing.
func TestCaptureLiveReportsTheRunningSession(t *testing.T) {
	corpusPresent(t)
	src := newCapturePushSource()
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: fakeCaptureResolve(),
			Source:  func(ctx context.Context, res stack.Resolved) (capture.Source, error) { return src, nil },
		}),
	)
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	info, err := e.start(runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindV4L2, Dir: stack.FakeID},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start (v4l2, capture wired): %v", err)
	}
	waitRunning(t, e.mgr, info.ID)

	// Before the first delivery: the view exists (the session is running)
	// but holds no frame and no crop — the "no frame in memory yet" state,
	// not an empty frame the UI would render.
	live, ok := e.mgr.CaptureLive()
	if !ok {
		t.Fatal("CaptureLive answers false on the running session")
	}
	if live.SessionID != info.ID || live.Device != stack.FakeID {
		t.Fatalf("session/device = %q/%q, want the running session holding the device", live.SessionID, live.Device)
	}
	if live.Format != info.CaptureMode || live.FPSTarget != info.CaptureFPSTarget {
		t.Fatalf("format/fps_target = %q/%v, want the session's negotiated mode %q/%v", live.Format, live.FPSTarget, info.CaptureMode, info.CaptureFPSTarget)
	}
	if live.Latest != nil {
		t.Fatal("the view holds a frame before the first delivery")
	}
	if live.Dialogue != nil {
		t.Fatal("the view holds a dialogue crop before the run has computed one")
	}

	// Drive the run at the source's own cadence (10 fps clip frames): the
	// view acquires the newest frame, the tracker's window and the dialogue
	// region's last crop.
	for n := 1; n <= 6; n++ {
		src.push(t, manualClipFrame(t, n))
		time.Sleep(100 * time.Millisecond)
	}
	by := time.Now().Add(60 * time.Second)
	for {
		live, ok = e.mgr.CaptureLive()
		if ok && live.Latest != nil && live.Latest.Image != nil && live.Dialogue != nil {
			break
		}
		if time.Now().After(by) {
			t.Fatalf("the view never acquired the newest frame and the dialogue crop (last: ok=%v frame=%v crop=%v)", ok, live.Latest != nil, live.Dialogue != nil)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The tracker's window: settled (an accepted box) or the whole frame
	// (not settled yet) — never a torn or missing half. The source's
	// self-report has caught up: the running state and a measured rate.
	if live.Source.State != string(capture.StateRunning) {
		t.Fatalf("source state = %q, want running (after the run has delivered frames)", live.Source.State)
	}
	if !live.Picture.Settled && (live.Picture.W <= 0 || live.Picture.H <= 0) {
		t.Fatalf("an unsettled window must be the whole frame, got %+v", live.Picture)
	}
	// The dialogue crop is inside the frame and non-empty: a box the
	// runtime really cut, in frame coordinates.
	b := live.Latest.Image.Bounds()
	crop := live.Dialogue
	if crop.W <= 0 || crop.H <= 0 {
		t.Fatalf("the dialogue crop is empty: %+v", crop)
	}
	if crop.X < 0 || crop.Y < 0 || crop.X+crop.W > b.Dx() || crop.Y+crop.H > b.Dy() {
		t.Fatalf("the dialogue crop %d,%d %dx%d escapes the %dx%d frame", crop.X, crop.Y, crop.W, crop.H, b.Dx(), b.Dy())
	}

	// The session ends (the source stops delivering): the view is the
	// session's, so it answers nothing after the stop.
	src.Close()
	info = waitState(t, e.mgr, info.ID, 30*time.Second)
	if info.State != runtime.StateStopped {
		t.Fatalf("session ended %s (reason %s), want stopped", info.State, info.Reason)
	}
	if _, ok := e.mgr.CaptureLive(); ok {
		t.Fatal("CaptureLive still reports the finished session; the view is released with it")
	}
}

// TestCaptureLiveIgnoresNonCaptureSessions pins the v4l2-only gate: a
// session whose source is not the card (a replay run) never answers the
// HDMI input's question — CaptureLive stays false for its whole life, so
// the API's one-shot path answers against the card instead.
func TestCaptureLiveIgnoresNonCaptureSessions(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(truthScript(t), 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	info, err := e.start(runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindReplay, Dir: syntheticDir},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start (replay): %v", err)
	}
	waitRunning(t, e.mgr, info.ID)
	if _, ok := e.mgr.CaptureLive(); ok {
		t.Fatal("CaptureLive reports a replay session; only a v4l2 session holds the card")
	}
	// The ActiveCaptureDevice gate agrees: no device is held.
	if dev, sess, active := e.mgr.ActiveCaptureDevice(); active {
		t.Fatalf("ActiveCaptureDevice = (%q, %q) on a replay session; the card is not held", dev, sess)
	}
}
