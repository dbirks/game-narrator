package runtime_test

// The default frame source (ADR-0119, issue #28): a Start that names no
// frame_source resolves one — a usable capture device runs v4l2 on that
// device, the synthetic test pattern is the developer-mode fallback only,
// and with no usable device and developer mode off the answer is the 409
// sentinel that names the operator action (connect the capture card). The
// resolved kind rides back in the snapshot and the session.started event:
// a default is visible in the run's own evidence, never guessed from the
// request.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// defaultCaptureResolve is the default-resolution half of the test's
// enumeration seam: an empty request (the default resolution's request) and
// the known id both settle on the one device — the ADR-0078 chain's
// "persisted selection, then the first usable device" — and anything else
// gets the stack's unknown-device refusal.
func defaultCaptureResolve() func(context.Context, string) (stack.Resolved, error) {
	return func(ctx context.Context, requestedID string) (stack.Resolved, error) {
		if requestedID != "" && requestedID != stack.FakeID {
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

// absentCaptureResolve is the enumeration of a box with no usable capture
// device attached: the ADR-0078 chain's refusal the default resolution
// answers with its fallback instead of the 503 the explicit kind gets.
func absentCaptureResolve() func(context.Context, string) (stack.Resolved, error) {
	return func(ctx context.Context, requestedID string) (stack.Resolved, error) {
		return stack.Resolved{}, stack.ErrDeviceAbsent
	}
}

// neverCalledSource is the Source half of a CaptureOptions wiring whose
// resolution can never succeed: the closure is the contract that a wired
// process has, and it must not run for a device that is not there.
func neverCalledSource() func(context.Context, stack.Resolved) (capture.Source, error) {
	return func(ctx context.Context, res stack.Resolved) (capture.Source, error) {
		return nil, errors.New("the source builder ran for an absent device")
	}
}

// TestDefaultFrameSourceResolvesCaptureDevice is the appliance case: a Start
// that names no frame source runs the ready capture device — the snapshot's
// frame_source is the resolved kind, the capture fields name the device the
// resolution settled on, and the session.started event carries the same
// evidence.
func TestDefaultFrameSourceResolvesCaptureDevice(t *testing.T) {
	corpusPresent(t)
	src := newCapturePushSource()
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: defaultCaptureResolve(),
			Source:  func(ctx context.Context, res stack.Resolved) (capture.Source, error) { return src, nil },
		}),
	)
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	// The request names no frame source at all.
	info, err := e.start(runtime.StartRequest{
		ProfileID: profileID,
	})
	if err != nil {
		t.Fatalf("start (no frame source, device ready): %v", err)
	}
	waitRunning(t, e.mgr, info.ID)
	t.Cleanup(func() { _, _ = e.mgr.Stop(info.ID) })

	if info.SourceKind != runtime.SourceKindV4L2 {
		t.Fatalf("snapshot frame_source = %q, want %q (the resolved default)", info.SourceKind, runtime.SourceKindV4L2)
	}
	if info.CaptureDevice != stack.FakeID {
		t.Fatalf("snapshot capture_device = %q, want %q", info.CaptureDevice, stack.FakeID)
	}
	if info.CaptureMode == "" || info.CaptureModeSource != stack.ModeSourcePolicy {
		t.Fatalf("snapshot capture mode = %q (%q), want the resolved mode with its provenance", info.CaptureMode, info.CaptureModeSource)
	}

	// The session.started event is the same evidence: the resolved kind and
	// the device, not the absence the request carried.
	started := latestEvent(t, e, info.ID, "session.started")
	if started == nil {
		t.Fatal("no session.started event")
	}
	var data map[string]any
	if err := json.Unmarshal(started.Data, &data); err != nil {
		t.Fatalf("session.started data does not decode: %v", err)
	}
	fs, _ := data["frame_source"].(map[string]any)
	if fs == nil || fs["kind"] != runtime.SourceKindV4L2 || fs["device"] != stack.FakeID {
		t.Fatalf("session.started frame_source = %v, want kind v4l2 with the resolved device", fs)
	}
}

// TestDefaultFrameSourceDeveloperFallback is the card-less developer host:
// with no usable capture device the default resolves the synthetic test
// pattern only in developer mode (GAME_NARRATOR_DEV=1), and the snapshot
// names the kind it ran.
func TestDefaultFrameSourceDeveloperFallback(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: absentCaptureResolve(),
			Source:  neverCalledSource(),
		}),
	)
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	t.Setenv(runtime.EnvDeveloperMode, "1")

	info, err := e.start(runtime.StartRequest{ProfileID: profileID})
	if err != nil {
		t.Fatalf("start (no device, developer mode): %v", err)
	}
	waitRunning(t, e.mgr, info.ID)
	t.Cleanup(func() { _, _ = e.mgr.Stop(info.ID) })

	if info.SourceKind != runtime.SourceKindTestPattern {
		t.Fatalf("snapshot frame_source = %q, want %q (the developer-mode fallback)", info.SourceKind, runtime.SourceKindTestPattern)
	}
	if info.CaptureDevice != "" {
		t.Fatalf("snapshot capture_device = %q, want empty (the fallback runs no device)", info.CaptureDevice)
	}
}

// TestDefaultFrameSourceRefusedWithoutDeveloperMode is the appliance case:
// no frame source named, no usable capture device, developer mode off — the
// default has nothing to resolve to, and the answer is the sentinel the API
// maps to the 409 that names the operator action. A silent test-pattern
// session here is the failure issue #28 describes.
func TestDefaultFrameSourceRefusedWithoutDeveloperMode(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: absentCaptureResolve(),
			Source:  neverCalledSource(),
		}),
	)
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	// Developer mode unset (and, belt: explicitly off).
	if _, err := e.start(runtime.StartRequest{ProfileID: profileID}); !errors.Is(err, runtime.ErrNoFrameSource) {
		t.Fatalf("start (no device, no developer mode) = %v, want %v", err, runtime.ErrNoFrameSource)
	}
}

// TestDefaultFrameSourceBrokenDeviceIsNotHidden: a device that is present
// but offers no policy-fitting mode keeps its refusal through the default
// path too — a broken card is never hidden behind a silent synthetic
// session, even when developer mode is on.
func TestDefaultFrameSourceBrokenDeviceIsNotHidden(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t,
		withFakeOCR(truthScript(t), 0),
		withCapture(&runtime.CaptureOptions{
			Resolve: func(ctx context.Context, requestedID string) (stack.Resolved, error) {
				return stack.Resolved{}, stack.ErrModeUnavailable
			},
			Source: neverCalledSource(),
		}),
	)
	profileID, _ := seedSyntheticProfile(t, e.st, true)
	t.Setenv(runtime.EnvDeveloperMode, "1")

	if _, err := e.start(runtime.StartRequest{ProfileID: profileID}); !errors.Is(err, runtime.ErrCaptureModeUnavailable) {
		t.Fatalf("start (broken card, developer mode) = %v, want %v (never the fallback)", err, runtime.ErrCaptureModeUnavailable)
	}
}

// TestDefaultFrameSourceIncomplete pins the 400: a dir or a clip id without
// a kind is ambiguous (a replay dir and a device id both live in
// frame_source.dir), so the answer is the sentinel that names the rule.
func TestDefaultFrameSourceIncomplete(t *testing.T) {
	corpusPresent(t)
	e := newEnv(t, withFakeOCR(truthScript(t), 0))
	profileID, _ := seedSyntheticProfile(t, e.st, true)

	cases := []struct {
		name string
		src  runtime.FrameSource
	}{
		{"a dir without a kind", runtime.FrameSource{Dir: syntheticClipDir()}},
		{"a clip id without a kind", runtime.FrameSource{ClipID: "a-clip"}},
		{"both without a kind", runtime.FrameSource{Dir: syntheticClipDir(), ClipID: "a-clip"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.start(runtime.StartRequest{ProfileID: profileID, FrameSource: tc.src}); !errors.Is(err, runtime.ErrIncompleteFrameSource) {
				t.Fatalf("start = %v, want %v", err, runtime.ErrIncompleteFrameSource)
			}
		})
	}
}
