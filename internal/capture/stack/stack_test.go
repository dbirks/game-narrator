// The selection layer's table (ADR-0078's validation): the stable-id
// allowlist and its three accepted spellings, the persisted selection's
// fallbacks, the saved-vs-policy mode decision, the no-mode and absent
// refusals, the Save validation, the device list's states, and the
// labelled-synthetic fake seam's enumeration. Everything runs on injected
// seams (a push-based source, an in-memory store, a fixed link): no v4l2
// node, no sysfs, no subprocess — the hardware is the ADR's NOT RUN list.
package stack

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/usb"
)

// testFormats is the documented MS2130-class set (the ADR-0076 fixtures'
// shape): two pixel formats, two sizes, four discrete rates. Under the
// default policy at HighSpeed the pick is 640x480 mjpeg @ 10 fps; the only
// rejected candidate is the 720p mjpeg size (its decode rate misses the 8
// Mpps budget). The policy scores one candidate per size at its picked rate.
func testFormats() []devices.Format {
	return []devices.Format{
		{
			PixFmt:      "mjpeg",
			Description: "MJPG (Motion JPEG)",
			Sizes: []devices.Size{
				{W: 640, H: 480, FPS: []float64{10, 15, 30, 60}},
				{W: 1280, H: 720, FPS: []float64{10, 15, 30, 60}},
			},
		},
		{
			PixFmt:      "yuyv422",
			Description: "YUYV (YUYV 4:2:2)",
			Sizes: []devices.Size{
				{W: 640, H: 480, FPS: []float64{10, 15, 30, 60}},
				{W: 1280, H: 720, FPS: []float64{10, 15, 30, 60}},
			},
		},
	}
}

// The test enumeration: the usable card, a card with no policy fit, a
// non-capture node and a card whose formats read failed. Under the default
// policy at HighSpeed the card's pick is 640x480 mjpeg @ 10 fps, with one
// rejected candidate (the 720p mjpeg set misses the 8 Mpps decode budget).
var (
	devCard = devices.Device{
		Path: "/dev/video0", Name: "Test Capture", BusInfo: "usb-1-1.1",
		StableID: "usb-TestCapture_0001", ByPath: "usb-1-1.1-video:00",
		Formats: testFormats(), Kind: "Video Capture",
	}
	devSmall = devices.Device{
		Path: "/dev/video1", Name: "Test Small", BusInfo: "usb-1-1.2",
		StableID: "usb-TestCapture_0002",
		Formats: []devices.Format{{
			PixFmt: "mjpeg",
			Sizes:  []devices.Size{{W: 320, H: 240, FPS: []float64{10}}},
		}},
		Kind: "Video Capture",
	}
	devNotCapture = devices.Device{
		Path: "/dev/video2", Name: "Test Metadata", BusInfo: "usb-1-1.1",
		Kind: "Media controller devices",
	}
	devNoFormats = devices.Device{
		Path: "/dev/video3", Name: "Test Denied", BusInfo: "usb-1-1.3",
		FormatsError: "v4l2-ctl: Permission denied", Kind: "Video Capture",
	}
)

var testList = []devices.Device{devCard, devSmall, devNotCapture, devNoFormats}

// devOther is a second fully usable card (its own test list, so the
// stored-selection fallback has a target the policy actually fits).
var devOther = devices.Device{
	Path: "/dev/video4", Name: "Test Other", BusInfo: "usb-1-2.1",
	StableID: "usb-TestCapture_0003", ByPath: "usb-1-2.1-video:00",
	Formats: testFormats(), Kind: "Video Capture",
}

// memStore is the SelectionStore seam for tests.
type memStore struct {
	sel Selection
	ok  bool
}

func (m *memStore) GetSelection(ctx context.Context) (Selection, bool, error) {
	return m.sel, m.ok, nil
}
func (m *memStore) SetSelection(ctx context.Context, sel Selection) error {
	m.sel, m.ok = sel, true
	return nil
}

// pushSource is the capture.Source seam: pre-built frames on a channel.
type pushSource struct {
	id      string
	frames  []capture.Frame
	dropped int64
	fps     float64
}

func (p *pushSource) ID() string { return p.id }

func (p *pushSource) Start(ctx context.Context) (<-chan capture.Frame, error) {
	ch := make(chan capture.Frame, len(p.frames))
	for i, f := range p.frames {
		f.Seq = int64(i + 1)
		f.Dropped = p.dropped
		ch <- f
	}
	close(ch)
	return ch, nil
}
func (p *pushSource) Stats() capture.Stats {
	return capture.Stats{
		Delivered: uint64(len(p.frames)), FPS: p.fps,
		LastFrameAt: time.Now(), State: capture.StateRunning,
	}
}
func (p *pushSource) Close() error { return nil }

func testFrame(w, h int) capture.Frame {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	img.Set(0, 0, color.White)
	return capture.Frame{
		SourceClock: "monotonic", CaptureTimestampNS: time.Now().UnixNano(),
		Width: w, Height: h, Image: img,
	}
}

func newTestStack(store SelectionStore, frames []capture.Frame) (*Stack, *pushSource) {
	src := &pushSource{id: "v4l2:/dev/video0", frames: frames, dropped: 1, fps: 10}
	st := &Stack{
		List: func(ctx context.Context) ([]devices.Device, error) { return testList, nil },
		Link: func(ctx context.Context, node string) (usb.Info, error) {
			return usb.Info{SpeedMBps: usb.SpeedHighSpeed, MaxPortSpeedMBps: usb.SpeedHighSpeed}, nil
		},
		Store:  store,
		Policy: mode.Policy{},
		Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) { return src, nil },
	}
	return st, src
}

// TestResolveRequestedID is the allowlist's accepted spellings: the stable
// id, the by-path name and the node path all resolve to the same device; a
// fourth name is refused.
func TestResolveRequestedID(t *testing.T) {
	st, _ := newTestStack(nil, nil)
	for _, tc := range []struct {
		name    string
		request string
		wantID  string
		wantErr error
	}{
		{"stable id", "usb-TestCapture_0001", "usb-TestCapture_0001", nil},
		{"by-path name", "usb-1-1.1-video:00", "usb-TestCapture_0001", nil},
		{"node path", "/dev/video0", "usb-TestCapture_0001", nil},
		{"whitespace-trimmed", "  usb-TestCapture_0001  ", "usb-TestCapture_0001", nil},
		{"unknown raw path", "/dev/video9", "", ErrDeviceUnknown},
		{"unknown stable id", "usb-TestCapture_9999", "", ErrDeviceUnknown},
		{"non-usable id", "usb-TestCapture_0002-no-such", "", ErrDeviceUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := st.Resolve(context.Background(), tc.request)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("want %v, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.ID() != tc.wantID {
				t.Fatalf("want device %s, got %s", tc.wantID, res.ID())
			}
		})
	}
}

// TestResolveFallback is the empty-requested-id chain: the persisted
// selection, then the first usable device — and the absent-device refusal.
func TestResolveFallback(t *testing.T) {
	t.Run("no store, first usable", func(t *testing.T) {
		st, _ := newTestStack(nil, nil)
		res, err := st.Resolve(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.ID() != "usb-TestCapture_0001" {
			t.Fatalf("want the first usable device, got %s", res.ID())
		}
	})
	t.Run("stored selection outranks the first", func(t *testing.T) {
		list := []devices.Device{devCard, devOther}
		st := &Stack{
			List: func(ctx context.Context) ([]devices.Device, error) { return list, nil },
			Link: func(ctx context.Context, node string) (usb.Info, error) {
				return usb.Info{SpeedMBps: usb.SpeedHighSpeed, MaxPortSpeedMBps: usb.SpeedHighSpeed}, nil
			},
			Store:  &memStore{sel: Selection{DeviceID: "usb-TestCapture_0003"}, ok: true},
			Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) { return nil, nil },
		}
		res, err := st.Resolve(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.ID() != "usb-TestCapture_0003" {
			t.Fatalf("want the stored device, got %s", res.ID())
		}
	})
	t.Run("stale stored selection falls to the first usable", func(t *testing.T) {
		st, _ := newTestStack(&memStore{sel: Selection{DeviceID: "usb-TestCapture_9999"}, ok: true}, nil)
		res, err := st.Resolve(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.ID() != "usb-TestCapture_0001" {
			t.Fatalf("want the first usable device, got %s", res.ID())
		}
	})
	t.Run("no usable device is the absent refusal", func(t *testing.T) {
		st := &Stack{
			List: func(ctx context.Context) ([]devices.Device, error) {
				return []devices.Device{devNotCapture, devNoFormats}, nil
			},
			Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) { return nil, nil },
		}
		if _, err := st.Resolve(context.Background(), ""); !errors.Is(err, ErrDeviceAbsent) {
			t.Fatalf("want ErrDeviceAbsent, got %v", err)
		}
	})
}

// TestResolveMode is the mode half: the policy's pick, the operator's pin
// when it still fits, the policy's re-derivation when the pin stopped
// fitting (with the store refreshed), and the no-fit refusal.
func TestResolveMode(t *testing.T) {
	t.Run("policy decides without a selection", func(t *testing.T) {
		st, _ := newTestStack(nil, nil)
		res, err := st.Resolve(context.Background(), "usb-TestCapture_0001")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := mode.Mode{W: 640, H: 480, FPS: 10, PixFmt: "mjpeg"}
		if res.Mode != want {
			t.Fatalf("want the policy's pick %s, got %s", want, res.Mode)
		}
		if res.ModeSource != ModeSourcePolicy {
			t.Fatalf("want source policy, got %s", res.ModeSource)
		}
		if res.Rejected != 1 {
			t.Fatalf("want 1 rejected candidate (the 720p mjpeg set misses the decode budget), got %d", res.Rejected)
		}
	})
	t.Run("the pinned mode wins while it is offered", func(t *testing.T) {
		pin := mode.Mode{W: 1280, H: 720, FPS: 15, PixFmt: "mjpeg"}
		st, _ := newTestStack(&memStore{sel: Selection{DeviceID: "usb-TestCapture_0001", Mode: &pin}, ok: true}, nil)
		res, err := st.Resolve(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Mode != pin {
			t.Fatalf("want the pin %s, got %s", pin, res.Mode)
		}
		if res.ModeSource != ModeSourceSaved {
			t.Fatalf("want source saved, got %s", res.ModeSource)
		}
	})
	t.Run("a stale pin is re-derived and the store refreshed", func(t *testing.T) {
		stale := mode.Mode{W: 1920, H: 1080, FPS: 30, PixFmt: "mjpeg"}
		store := &memStore{sel: Selection{DeviceID: "usb-TestCapture_0001", Mode: &stale}, ok: true}
		st, _ := newTestStack(store, nil)
		res, err := st.Resolve(context.Background(), "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		want := mode.Mode{W: 640, H: 480, FPS: 10, PixFmt: "mjpeg"}
		if res.Mode != want || res.ModeSource != ModeSourcePolicy {
			t.Fatalf("want the policy's pick %s (source %s), got %s (source %s)", want, ModeSourcePolicy, res.Mode, res.ModeSource)
		}
		// The best-effort refresh left the store holding the mode the card
		// actually offers, so the next list shows the policy's pick.
		if store.sel.Mode == nil || *store.sel.Mode != want {
			t.Fatalf("want the store refreshed to %s, got %+v", want, store.sel.Mode)
		}
	})
	t.Run("no mode fitting the policy is the refusal", func(t *testing.T) {
		st, _ := newTestStack(nil, nil)
		if _, err := st.Resolve(context.Background(), "usb-TestCapture_0002"); !errors.Is(err, ErrModeUnavailable) {
			t.Fatalf("want ErrModeUnavailable, got %v", err)
		}
	})
}

// TestSave is the PUT route's validation: the store's seam, the device's
// presence in the live enumeration, the mode's presence in the device's
// offered set — so the store cannot hold a selection the next resolution
// would silently discard.
func TestSave(t *testing.T) {
	ctx := context.Background()
	t.Run("no store is the unwired refusal", func(t *testing.T) {
		st := &Stack{List: func(ctx context.Context) ([]devices.Device, error) { return testList, nil }}
		if _, err := st.Save(ctx, "usb-TestCapture_0001", nil); !errors.Is(err, ErrNoSelectionStore) {
			t.Fatalf("want ErrNoSelectionStore, got %v", err)
		}
	})
	t.Run("an unknown device is refused", func(t *testing.T) {
		st, _ := newTestStack(&memStore{}, nil)
		if _, err := st.Save(ctx, "usb-TestCapture_9999", nil); !errors.Is(err, ErrDeviceUnknown) {
			t.Fatalf("want ErrDeviceUnknown, got %v", err)
		}
	})
	t.Run("a non-usable device is refused", func(t *testing.T) {
		st, _ := newTestStack(&memStore{}, nil)
		if _, err := st.Save(ctx, "/dev/video3", nil); !errors.Is(err, ErrDeviceUnknown) {
			t.Fatalf("want ErrDeviceUnknown for the formats-error node, got %v", err)
		}
	})
	t.Run("a mode the device does not offer is refused", func(t *testing.T) {
		st, _ := newTestStack(&memStore{}, nil)
		m := mode.Mode{W: 1920, H: 1080, FPS: 60, PixFmt: "yuyv422"}
		if _, err := st.Save(ctx, "usb-TestCapture_0001", &m); !errors.Is(err, ErrModeNotOffered) {
			t.Fatalf("want ErrModeNotOffered, got %v", err)
		}
	})
	t.Run("an offered mode persists and loads back", func(t *testing.T) {
		store := &memStore{}
		st, _ := newTestStack(store, nil)
		m := mode.Mode{W: 1280, H: 720, FPS: 30, PixFmt: "yuyv422"}
		sel, err := st.Save(ctx, "usb-TestCapture_0001", &m)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel.DeviceID != "usb-TestCapture_0001" || sel.Mode == nil || *sel.Mode != m {
			t.Fatalf("want the saved row back, got %+v", sel)
		}
		loaded, ok, err := st.Load(ctx)
		if err != nil || !ok || loaded != sel {
			t.Fatalf("want the stored row, got %+v (ok=%v err=%v)", loaded, ok, err)
		}
	})
	t.Run("a device-only selection records the policy decision", func(t *testing.T) {
		store := &memStore{}
		st, _ := newTestStack(store, nil)
		sel, err := st.Save(ctx, "usb-TestCapture_0001", nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if sel.Mode != nil {
			t.Fatalf("want a nil mode (the policy decides), got %+v", *sel.Mode)
		}
	})
}

// TestEnumerateView is the device list's shapes: the ready device with its
// link and the policy's chosen mode, the no-mode refusal with the policy's
// reason, the non-capture and enumeration-error states, and the selection's
// echo after a save.
func TestEnumerateView(t *testing.T) {
	ctx := context.Background()
	store := &memStore{}
	st, _ := newTestStack(store, nil)

	view, err := st.Enumerate(ctx)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if view.Synthetic {
		t.Fatal("the injected seams are not the fake seam; synthetic must be false")
	}
	if view.SelectedID != "" {
		t.Fatalf("want no selection yet, got %q", view.SelectedID)
	}
	if len(view.Devices) != len(testList) {
		t.Fatalf("want %d devices, got %d", len(testList), len(view.Devices))
	}
	byID := map[string]DeviceView{}
	for _, d := range view.Devices {
		byID[d.ID] = d
	}
	card := byID["usb-TestCapture_0001"]
	if card.State != StateReady {
		t.Fatalf("want the card ready, got %s (%s)", card.State, card.Detail)
	}
	if card.Link == nil || card.Link.SpeedMBps != usb.SpeedHighSpeed || card.Link.Downgraded {
		t.Fatalf("want the HighSpeed link, got %+v", card.Link)
	}
	if card.Chosen == nil || card.Chosen.Source != ModeSourcePolicy {
		t.Fatalf("want the policy's pick, got %+v", card.Chosen)
	}
	if card.Chosen.W != 640 || card.Chosen.H != 480 || card.Chosen.FPS != 10 || card.Chosen.PixFmt != "mjpeg" {
		t.Fatalf("want 640x480 mjpeg @ 10, got %+v", card.Chosen)
	}
	if card.Chosen.Rejected != 1 {
		t.Fatalf("want 1 rejected candidate, got %d", card.Chosen.Rejected)
	}
	if got := byID["usb-TestCapture_0002"].State; got != StateNoMode {
		t.Fatalf("want the small card no-mode, got %s", got)
	}
	if got := byID["/dev/video2"].State; got != StateNotCapture {
		t.Fatalf("want the metadata node not-capture, got %s", got)
	}
	if got := byID["/dev/video3"]; got.State != StateEnumerationErr || got.Detail == "" {
		t.Fatalf("want the denied node enumeration-error with the fault, got %+v", got)
	}

	// The save echoes back: the selected id and the pinned mode's provenance.
	pin := mode.Mode{W: 1280, H: 720, FPS: 30, PixFmt: "yuyv422"}
	if _, err := st.Save(ctx, "usb-TestCapture_0001", &pin); err != nil {
		t.Fatalf("saving the selection: %v", err)
	}
	view, err = st.Enumerate(ctx)
	if err != nil {
		t.Fatalf("re-enumerating: %v", err)
	}
	if view.SelectedID != "usb-TestCapture_0001" {
		t.Fatalf("want the stored selection echoed, got %q", view.SelectedID)
	}
	if view.SelectedMode == nil || view.SelectedMode.Source != ModeSourceSaved || view.SelectedMode.FPS != 30 {
		t.Fatalf("want the pinned mode with source saved, got %+v", view.SelectedMode)
	}
}

// TestFakeSeam is the labelled-synthetic seam (ADR-0078 §4): the env switch,
// the fixed device with its documented format set, the fixed link, the
// source builder's id and path discipline, and the flag every answer
// carries. A build without the env is not synthetic — the seam is a test
// property, never a silent default.
func TestFakeSeam(t *testing.T) {
	ctx := context.Background()
	lookup := func(key string) (string, bool) {
		if key == EnvFake {
			return "1", true
		}
		return "", false
	}
	st := DefaultOptions(lookup, &memStore{}, nil)
	if !st.Synthetic || st.Note == "" {
		t.Fatalf("the fake seam must be flagged synthetic with its provenance, got synthetic=%v note=%q", st.Synthetic, st.Note)
	}
	view, err := st.Enumerate(ctx)
	if err != nil {
		t.Fatalf("enumerating the fake seam: %v", err)
	}
	if !view.Synthetic {
		t.Fatal("the fake seam's list must carry the synthetic flag")
	}
	if len(view.Devices) != 1 || view.Devices[0].ID != FakeID {
		t.Fatalf("want the one fake device %s, got %+v", FakeID, view.Devices)
	}
	d := view.Devices[0]
	if d.State != StateReady || d.Path != FakePath {
		t.Fatalf("want the fake device ready on %s, got %+v", FakePath, d)
	}
	if d.Link == nil || d.Link.SpeedMBps != usb.SpeedHighSpeed {
		t.Fatalf("want the fixed HighSpeed link, got %+v", d.Link)
	}
	if d.Chosen == nil || d.Chosen.W != 640 || d.Chosen.H != 480 || d.Chosen.FPS != 10 || d.Chosen.PixFmt != "mjpeg" || d.Chosen.Source != ModeSourcePolicy {
		t.Fatalf("want the documented policy pick, got %+v", d.Chosen)
	}

	// The source builder: only the fake's own node, carrying the v4l2: id the
	// no-signal wrapper keys on (the construction is pure — no subprocess).
	if _, err := st.Source(ctx, "/dev/video0", mode.Mode{}); err == nil {
		t.Fatal("the fake seam must refuse a node it does not own")
	}
	src, err := st.Source(ctx, FakePath, mode.Mode{W: 640, H: 480, FPS: 10, PixFmt: "mjpeg"})
	if err != nil {
		t.Fatalf("the fake seam must feed its own node: %v", err)
	}
	if want := "v4l2:" + FakePath; src.ID() != want {
		t.Fatalf("want the source id %s, got %s", want, src.ID())
	}

	// Without the env, the production stack: not synthetic, real seams.
	prod := DefaultOptions(nil, &memStore{}, nil)
	if prod.Synthetic {
		t.Fatal("the production stack must not be labelled synthetic")
	}
}

// TestSnapshot is the preview's one-frame half on the push seam: a JPEG out
// the top, the device's stable id in the meta, and the width cap.
func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	t.Run("a small frame passes through", func(t *testing.T) {
		st, _ := newTestStack(nil, []capture.Frame{testFrame(320, 240)})
		jpeg, meta, err := st.Snapshot(ctx, "")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if !bytes.HasPrefix(jpeg, []byte{0xFF, 0xD8, 0xFF}) {
			t.Fatalf("want JPEG bytes, got % x", jpeg[:min(4, len(jpeg))])
		}
		if meta.Device != "usb-TestCapture_0001" || meta.Width != 320 || meta.Height != 240 {
			t.Fatalf("want the device's id and geometry, got %+v", meta)
		}
		if meta.Mode == "" {
			t.Fatal("the meta must name the mode asked of the device")
		}
	})
	t.Run("a wide frame is capped to the preview width", func(t *testing.T) {
		st, _ := newTestStack(nil, []capture.Frame{testFrame(1920, 1080)})
		_, meta, err := st.Snapshot(ctx, "")
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		if meta.Width != 640 || meta.Height != 360 {
			t.Fatalf("want the 640 cap (ADR-0110 §2) with the aspect kept, got %dx%d", meta.Width, meta.Height)
		}
	})
}

// TestProbe is the capture test's frame stats on the push seam: the grabbed
// and dropped counts, the source's self-reported state and rate, the
// freshness, the default and explicit OCR regions, and the degradable OCR
// block.
func TestProbe(t *testing.T) {
	ctx := context.Background()
	t.Run("frames, drops, and the default OCR region", func(t *testing.T) {
		st, _ := newTestStack(nil, []capture.Frame{
			testFrame(320, 240), testFrame(320, 240), testFrame(320, 240),
			testFrame(320, 240), testFrame(320, 240),
		})
		out, err := st.Probe(ctx, "", 5, Box{}, func(ctx context.Context, img image.Image) (string, error) {
			return "the caption", nil
		})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if out.Device != "usb-TestCapture_0001" || out.ModeSource != ModeSourcePolicy {
			t.Fatalf("want the device and the policy provenance, got %+v", out)
		}
		if out.Requested != 5 || out.Grabbed != 5 || out.Dropped != 5 {
			t.Fatalf("want 5 grabbed with 5 dropped (1 per frame), got %+v", out)
		}
		if out.FPS != 10 || out.State != "running" {
			t.Fatalf("want the source's measured rate and state, got %g %s", out.FPS, out.State)
		}
		if out.FreshnessMS < 0 {
			t.Fatalf("freshness must not be negative, got %d", out.FreshnessMS)
		}
		if out.OCR == nil || out.OCR.Text != "the caption" || out.OCR.Engine != "tesseract" {
			t.Fatalf("want the OCR block, got %+v", out.OCR)
		}
		// The default region is the centre third of the 320x240 frame.
		want := Box{X: 80, Y: 96, W: 160, H: 48}
		if out.OCR.Region != want {
			t.Fatalf("want the centre third %+v, got %+v", want, out.OCR.Region)
		}
	})
	t.Run("an explicit region is the one OCR reads", func(t *testing.T) {
		st, _ := newTestStack(nil, []capture.Frame{testFrame(320, 240)})
		box := Box{X: 10, Y: 20, W: 64, H: 32}
		out, err := st.Probe(ctx, "", 1, box, func(ctx context.Context, img image.Image) (string, error) {
			if b := img.Bounds(); b.Dx() != 64 || b.Dy() != 32 {
				t.Fatalf("the crop must be the requested box, got %v", b)
			}
			return "", nil
		})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if out.OCR == nil || out.OCR.Region != box {
			t.Fatalf("want the explicit region, got %+v", out.OCR)
		}
	})
	t.Run("a failed engine degrades the block, not the probe", func(t *testing.T) {
		st, _ := newTestStack(nil, []capture.Frame{testFrame(320, 240)})
		out, err := st.Probe(ctx, "", 1, Box{}, func(ctx context.Context, img image.Image) (string, error) {
			return "", errors.New("tesseract: binary not found")
		})
		if err != nil {
			t.Fatalf("the probe must not fail on a dead engine: %v", err)
		}
		if out.OCR == nil || out.OCR.Engine != "unavailable" || out.OCR.Note == "" {
			t.Fatalf("want the unavailable block with the operator action, got %+v", out.OCR)
		}
	})
	t.Run("a nil engine skips the OCR block", func(t *testing.T) {
		st, _ := newTestStack(nil, []capture.Frame{testFrame(320, 240)})
		out, err := st.Probe(ctx, "", 1, Box{}, nil)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if out.OCR != nil {
			t.Fatalf("want no OCR block, got %+v", out.OCR)
		}
	})
}
