package runtime_test

// The v4l2 (UVC HDMI capture dongle) frame source (ADR-0078): the session
// path that the physical console player runs. Hardware-free coverage — the
// "full session through the capture source" the gap audit G1 names — runs on
// a fake enumeration (a stable-id device, the ADR-0074 audio-fake
// discipline) and a push-based source that carries the "v4l2:" ID the
// no-signal wrapper keys on, so the production wrapper, the resolution
// allowlist and the snapshot's device/mode fields all run exactly as the
// dongle would. The real-dongle path (FFmpeg reading a UVC node) is
// hardware-gated: hwsmoke and scripts/hw/sustained-on-device.sh --source
// capture run it on-device and are NOT RUN here.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/scenario"
)

// fakeCaptureResolve is the test's enumeration seam: one device (the ADR-0074
// synthetic identity, so the label discipline holds) that answers every
// request for FakeID, and nothing else. Anything else gets the stack's
// unknown-device refusal — the allowlist is the enumeration, as in
// production.
func fakeCaptureResolve() func(context.Context, string) (stack.Resolved, error) {
	return func(ctx context.Context, requestedID string) (stack.Resolved, error) {
		if requestedID != stack.FakeID {
			return stack.Resolved{}, fmt.Errorf("device %q: %w", requestedID, stack.ErrDeviceUnknown)
		}
		return stack.Resolved{
			Device: devices.Device{
				Path: stack.FakePath, Name: "Synthetic HDMI",
				StableID: stack.FakeID, Kind: "Video Capture",
			},
			Mode:       mode.Mode{W: 640, H: 480, FPS: 10, PixFmt: "mjpeg"},
			ModeSource: stack.ModeSourcePolicy,
			Reason:     "test wiring",
		}, nil
	}
}

// capturePushSource is the push-based capture source behind the test seam:
// the manualSource mechanism, with the "v4l2:<node>" ID the production
// V4L2 source carries — so the session wraps it with the no-signal monitor
// exactly as a dongle session would.
type capturePushSource struct {
	mu     sync.Mutex
	ch     chan capture.Frame
	closed bool
	seq    int64
}

func newCapturePushSource() *capturePushSource {
	return &capturePushSource{ch: make(chan capture.Frame, 16)}
}

func (s *capturePushSource) ID() string { return "v4l2:" + stack.FakePath }

func (s *capturePushSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return s.ch, nil
}

func (s *capturePushSource) push(t *testing.T, f capture.Frame) {
	t.Helper()
	s.mu.Lock()
	s.seq++
	f.Seq = s.seq
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	select {
	case s.ch <- f:
	case <-ctx.Done():
		t.Fatalf("capture source push of frame %d timed out: the pipeline stopped consuming frames", f.Seq)
	}
}

// Stats is the source's self-report: the running state and the delivered
// count once it has delivered, the way the production V4L2 source reports
// (the runtime's source self-report — the signal's measured fps and last-
// frame time — reads this).
func (s *capturePushSource) Stats() capture.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seq == 0 {
		return capture.Stats{}
	}
	return capture.Stats{State: capture.StateRunning, Delivered: uint64(s.seq), FPS: 10, LastFrameAt: time.Now()}
}

func (s *capturePushSource) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.ch)
	return nil
}

// waitFrameSettled is the push loop's channel synchronisation (issue #22):
// it waits until the session's live pipeline stats show the frame's
// observation reached the reducer — `Observations` counts one per processed
// frame per region (plus the hash-reuse observations that skip a read), so
// for the session's single region the count reaching n is the frame settled
// past the single-slot capture bridge and past the OCR stage. Pushing the
// next frame only then keeps two failure modes out at any host load: the
// latest-slot bridge overwriting a frame the pipeline has not consumed, and
// the next observation outrunning the settled frame's rank reply (a reply
// the next observation beats is fenced stale, and a fenced reply is the
// wall-clock dependence the held-frame tail is there to recover).
// The bound is the race-scaled patience: a frame that never settles means
// the pipeline stopped consuming, and the test must say so.
func waitFrameSettled(t *testing.T, m *runtime.Manager, id string, n int64) {
	t.Helper()
	by := time.Now().Add(30 * time.Second * patienceScale)
	var lastObs int64
	stableSince := time.Time{}
	for {
		if info, ok := m.Get(id); ok && int64(info.Stats.FramesProcessed) >= n {
			if int64(info.Stats.Observations) >= n {
				return // every frame observed: settled
			}
			// A frame with an absent box emits no observation, so the settled
			// mark is the observation count going quiet after the frame has
			// been through the stage.
			if int64(info.Stats.Observations) == lastObs {
				if stableSince.IsZero() {
					stableSince = time.Now()
				} else if time.Since(stableSince) >= 200*time.Millisecond {
					return
				}
			} else {
				lastObs = int64(info.Stats.Observations)
				stableSince = time.Time{}
			}
		}
		if time.Now().After(by) {
			t.Fatalf("frame %d never settled in the pipeline (frames_processed stuck below %d): the capture session stopped consuming frames", n, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastTextFrameN is the clip's last frame that still shows a line: the
// screen the dongle is still delivering when the clip's recorded frames run
// out, the frame the capture test holds in the tail while the pipeline's
// rank reply settles.
func lastTextFrameN(t *testing.T) int {
	t.Helper()
	truth, err := scenario.LoadTruth(syntheticClipDir())
	if err != nil {
		t.Fatalf("load truth: %v", err)
	}
	last := 0
	for _, row := range truth {
		if row.Text == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(row.File, "frame-%03d.png", &n); err != nil || n < 1 {
			t.Fatalf("truth row %s: unparseable frame file", row.File)
		}
		if n > last {
			last = n
		}
	}
	if last == 0 {
		t.Fatalf("the clip's truth has no non-empty text frame")
	}
	return last
}

// TestCaptureSessionCommitsSyntheticText is G1's hardware-free core: a full
// session from kind v4l2 (device by stable id, mode from the "policy" seam)
// commits the synthetic profile's text through the capture source — the same
// frames the replay test commits, pushed by the test through a source that
// carries the v4l2 identity. The snapshot and the session.started event name
// the device and the mode: the fallback visibility the ADR requires.
func TestCaptureSessionCommitsSyntheticText(t *testing.T) {
	corpusPresent(t)
	src := newCapturePushSource()
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: fakeCaptureResolve(),
			Source: func(ctx context.Context, res stack.Resolved) (capture.Source, error) {
				return src, nil
			},
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

	// The snapshot names what the session runs: the device's stable id and
	// the mode it asked, with the mode's provenance.
	if info.CaptureDevice != stack.FakeID {
		t.Fatalf("snapshot capture_device = %q, want %q", info.CaptureDevice, stack.FakeID)
	}
	if info.CaptureMode == "" || info.CaptureModeSource != stack.ModeSourcePolicy {
		t.Fatalf("snapshot capture mode = %q (%q), want a named mode with its provenance", info.CaptureMode, info.CaptureModeSource)
	}

	// Drive the run the way the dongle drives it: the clip frames carry a
	// 10 fps source clock (100 ms apart), and a real UVC dongle delivers at
	// that rate — ffmpegpipe backpressures to hold it. Pushing faster would
	// overdrive the pipeline: its OCR aging runs on wall clock (the live
	// 4.25 s budget), so a burst of frames outruns the recognitions and every
	// read ages out before it resolves, and the text never stabilizes to a
	// commit. A fixed 100 ms sleep between pushes is only a guess at the
	// pipeline's pace, and under -race on a saturated host (issue #22) a
	// losing guess means the single-slot live queue overwrites the frame that
	// would have committed the line. Each push therefore waits for the
	// pipeline to have settled the frame — its observation in the reducer,
	// the frame ordered past the bridge — before the next one goes in.
	nFrames := clipFrameCount(t)
	for n := 1; n <= nFrames; n++ {
		src.push(t, manualClipFrame(t, n))
		waitFrameSettled(t, e.mgr, info.ID, int64(n))
	}
	// The clip ends on empty-text frames: if the last line's rank reply is
	// still in flight when they land, the reply is fenced stale and the empty
	// frames would re-trigger nothing — they carry no text to re-rank. A real
	// dongle keeps delivering the screen the line is still on — the same
	// pixels, at the source's own pace — so the test holds the clip's last
	// text frame the same way (hash-reuse observations, no new reads) until
	// the commit lands. The hold cadence is slower than the dongle's 10 fps:
	// a dongle under load delivers late, never early, and the slower cadence
	// gives the in-flight rank reply the room to land between two
	// observations instead of being fenced by the next one.
	holdN := lastTextFrameN(t)
	by := time.Now().Add(60 * time.Second * patienceScale)
	for latestEvent(t, e, info.ID, events.MessageCommitted) == nil {
		if time.Now().After(by) {
			t.Fatalf("no line committed within the commit budget of the stream (the capture path must commit the synthetic text the replay path commits)")
		}
		src.push(t, manualClipFrame(t, holdN))
		time.Sleep(500 * time.Millisecond)
	}
	// The clip's recorded tail (the empty-text frames), then the source end:
	// the session stops on source_end, the way the dongle's end does.
	for n := holdN + 1; n <= nFrames; n++ {
		src.push(t, manualClipFrame(t, n))
	}
	src.Close()
	info = waitState(t, e.mgr, info.ID, 30*time.Second*patienceScale)
	if info.State != runtime.StateStopped {
		t.Fatalf("session ended %s (reason %s), want stopped", info.State, info.Reason)
	}
	evs := eventsFrom(t, e, info.ID)
	committed := 0
	for _, ev := range evs {
		if ev.Name == events.MessageCommitted {
			committed++
		}
	}
	if committed == 0 {
		t.Fatalf("the capture-sourced session committed no lines (events: %d); the capture path must commit the synthetic text the replay path commits", len(evs))
	}

	// The session.started event's frame_source names the device and the mode:
	// the correlation the dongle's identity needs (the event is
	// developer-readable — the stable id, never the host node path, is the
	// event data).
	var started map[string]any
	for _, ev := range evs {
		if ev.Name == "session.started" {
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("session.started data does not decode: %v", err)
			}
			started = data
		}
	}
	if started == nil {
		t.Fatalf("no session.started event (events: %d)", len(evs))
	}
	fs, _ := started["frame_source"].(map[string]any)
	if fs == nil || fs["device"] != stack.FakeID || fs["mode"] == "" {
		t.Fatalf("session.started frame_source = %v, want the device id and a named mode", fs)
	}
}

// TestCaptureSourceAdmission pins the v4l2 refusal and error mapping
// (ADR-0078 §2): an unwired process refuses (ErrCaptureUnavailable — 503
// territory, the capture subsystem is not configured); a wired process
// refuses an unknown device (ErrCaptureDeviceUnknown — 400 territory, the
// client named a device the enumeration does not have) and names the
// absent-device refusal (ErrCaptureDeviceAbsent — 503 territory, no dongle
// attached). Each maps to a distinct API problem, so the operator sees the
// remedy, not a generic failure.
func TestCaptureSourceAdmission(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(map[string]string{}, 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	req := runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindV4L2, Dir: stack.FakeID},
		Mode:        runtime.ModeConservative,
	}

	// Unwired (the default env): the kind is refused, not faked.
	if _, err := e.start(req); !errors.Is(err, runtime.ErrCaptureUnavailable) {
		t.Fatalf("unwired start = %v, want %v", err, runtime.ErrCaptureUnavailable)
	}

	// Wired, but the client names a device the enumeration does not have.
	e2 := newEnv(t, withFakeOCR(map[string]string{}, 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: func(ctx context.Context, requestedID string) (stack.Resolved, error) {
				return stack.Resolved{}, fmt.Errorf("device %q: %w", requestedID, stack.ErrDeviceUnknown)
			},
			Source: func(ctx context.Context, res stack.Resolved) (capture.Source, error) {
				t.Fatalf("the source builder ran for an unknown device")
				return nil, nil
			},
		}))
	profileID2, _ := seedSyntheticProfile(t, e2.st, true)
	req2 := req
	req2.ProfileID = profileID2
	if _, err := e2.start(req2); !errors.Is(err, runtime.ErrCaptureDeviceUnknown) {
		t.Fatalf("unknown-device start = %v, want %v", err, runtime.ErrCaptureDeviceUnknown)
	}

	// Wired, no dongle attached: the absence is named.
	e3 := newEnv(t, withFakeOCR(map[string]string{}, 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: func(ctx context.Context, requestedID string) (stack.Resolved, error) {
				return stack.Resolved{}, stack.ErrDeviceAbsent
			},
			Source: func(ctx context.Context, res stack.Resolved) (capture.Source, error) {
				t.Fatalf("the source builder ran with no device attached")
				return nil, nil
			},
		}))
	profileID3, _ := seedSyntheticProfile(t, e3.st, true)
	req3 := req
	req3.ProfileID = profileID3
	if _, err := e3.start(req3); !errors.Is(err, runtime.ErrCaptureDeviceAbsent) {
		t.Fatalf("absent-device start = %v, want %v", err, runtime.ErrCaptureDeviceAbsent)
	}
}

// TestActiveCaptureDevice pins the manager's busy check (ADR-0078 §3): a
// running v4l2 session reports the device it holds (the preview route's
// same-device 503 names this session), and the report ends when the session
// does.
func TestActiveCaptureDevice(t *testing.T) {
	corpusPresent(t)
	src := newCapturePushSource()
	e := newEnv(t, withFakeOCR(map[string]string{}, 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: fakeCaptureResolve(),
			Source:  func(ctx context.Context, res stack.Resolved) (capture.Source, error) { return src, nil },
		}))
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	info, err := e.start(runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindV4L2, Dir: stack.FakeID},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitRunning(t, e.mgr, info.ID)

	if dev, sess, ok := e.mgr.ActiveCaptureDevice(); !ok || dev != stack.FakeID || sess != info.ID {
		t.Fatalf("active capture device = %q (session %q, ok=%v), want %q held by %q", dev, sess, ok, stack.FakeID, info.ID)
	}

	// The source ends (the push channel closes): the session stops and the
	// device is released.
	src.Close()
	waitState(t, e.mgr, info.ID, 30*time.Second)
	if dev, sess, ok := e.mgr.ActiveCaptureDevice(); ok {
		t.Fatalf("after the session stopped, the capture device is still reported held: %q by %q", dev, sess)
	}
}
