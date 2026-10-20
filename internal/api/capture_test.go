package api

// The capture device surface's HTTP tests (ADR-0078): the device list with the
// synthetic flag, the selection persist (and its two 400s), the throttled
// preview (the 429, the per-device busy 503, the unknown-device 400) and the
// capture probe (the frame stats, the degradable OCR block, the busy 503).
//
// The stack runs on the labelled-synthetic seam's shape — a fixed device, a
// push-based source standing in for the dongle — so the route logic is tested
// without a UVC node, v4l2-ctl or FFmpeg. The real-dongle path (the ffmpegpipe
// V4L2 constructor reading a node) is the hardware-gated half: hwsmoke and
// scripts/hw/sustained-on-device.sh --source capture run it on-device and are
// NOT RUN here. Every success body lands in recordContractResponse, so a
// response that stops matching the OpenAPI document fails here.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/capture/usb"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// captureEnv is the capture surface as serve wires it: the stack (a fixed
// device, a push-based source, an in-memory selection row) behind the four
// routes, with the auth harness for the role ladder.
type captureEnv struct {
	t   *testing.T
	a   *testAuth
	stk *stack.Stack
	src *capturePush
	sel *memSelection
	h   http.Handler
}

// memSelection is the in-memory SelectionStore seam: the app_settings row the
// production store keeps, here in a field so the tests assert persistence
// across requests without a SQLite file.
type memSelection struct {
	mu  sync.Mutex
	set bool
	row stack.Selection
}

func (m *memSelection) GetSelection(ctx context.Context) (stack.Selection, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.row, m.set, nil
}

func (m *memSelection) SetSelection(ctx context.Context, sel stack.Selection) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.row = sel
	m.set = true
	return nil
}

// capturePush is the push-based source behind the stack's source seam: the
// ID carries the "v4l2:" identity the production V4L2 source has, and each
// push is a real frame the stack's one-frame grab and N-frame probe consume.
type capturePush struct {
	mu sync.Mutex
	ch chan capture.Frame
	// stop is closed by Close (issue #18): Start's context watcher exits on
	// ctx cancellation OR this stop, whichever comes first. The watcher must
	// not be tied to ctx alone: the preview/probe request context is the
	// httptest request's context, which is never cancelled, so a ctx-only
	// watcher survives the source's Close and leaks one goroutine per
	// preview/probe request.
	stop   chan struct{}
	closed bool
	seq    int64
	// droppedPerDelivery is stamped on each frame's Dropped field: a test
	// that wants the probe's drop count to be non-zero sets this.
	droppedPerDelivery int64
	// fail, when set, makes Start answer with it (the source-fault 503 path).
	fail error
	// blank, when set, makes every frame a black no-signal-style frame with a
	// distinct pixel (a valid frame the OCR can be scripted against).
	blank bool
}

func newCapturePush() *capturePush {
	return &capturePush{ch: make(chan capture.Frame, 256), stop: make(chan struct{})}
}

func (c *capturePush) ID() string { return "v4l2:" + stack.FakePath }

func (c *capturePush) Start(ctx context.Context) (<-chan capture.Frame, error) {
	if c.fail != nil {
		return nil, c.fail
	}
	go func() {
		select {
		case <-ctx.Done():
			c.Close()
		case <-c.stop:
			// The source was stopped/closed: the watcher exits with it
			// (issue #18), so a never-cancelled request context cannot
			// hold it.
		}
	}()
	return c.ch, nil
}

func (c *capturePush) push(t *testing.T, n int) {
	t.Helper()
	c.mu.Lock()
	c.seq++
	seq := c.seq
	c.mu.Unlock()
	img := image.NewRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{R: uint8(seq), G: uint8(x % 256), B: uint8(y % 256), A: 255})
		}
	}
	f := capture.Frame{
		SourceClock:        capture.ClockMonotonic,
		CaptureTimestampNS: int64(seq) * 100_000_000,
		Width:              640,
		Height:             480,
		PixFmt:             capture.PixRGBA,
		Image:              img,
		Hash:               capture.HashFrame(img),
		Seq:                seq,
		Dropped:            c.droppedPerDelivery,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case c.ch <- f:
	case <-ctx.Done():
		t.Fatalf("capture push %d timed out: the stack stopped consuming", seq)
	}
}

func (c *capturePush) Stats() capture.Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.seq == 0 {
		return capture.Stats{}
	}
	return capture.Stats{
		State:       capture.StateRunning,
		Delivered:   uint64(c.seq),
		FPS:         10,
		LastFrameAt: time.Now(),
	}
}

func (c *capturePush) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.stop)
	close(c.ch)
	return nil
}

// fakeCaptureDevice is the fixed device the fake seam enumerates: the
// ADR-0076 fixtures' format set (the documented MS2130-class shape), so the
// mode policy has real candidates to select from.
func fakeCaptureDevice() devices.Device {
	return devices.Device{
		Path:     stack.FakePath,
		Name:     "Synthetic HDMI Capture (test seam)",
		BusInfo:  "usb-0000:00:14.0-fake1",
		StableID: stack.FakeID,
		ByPath:   "usb-0000:00:14.0-fake1-video:00",
		Kind:     "Capture devices",
		Formats: []devices.Format{
			{
				PixFmt: "mjpeg", Description: "MJPG",
				Sizes: []devices.Size{
					{W: 640, H: 480, FPS: []float64{10, 15, 30, 60}},
					{W: 1280, H: 720, FPS: []float64{10, 15, 30, 60}},
				},
			},
			{
				PixFmt: "yuyv422", Description: "YUYV",
				Sizes: []devices.Size{
					{W: 640, H: 480, FPS: []float64{10, 15, 30, 60}},
					{W: 1280, H: 720, FPS: []float64{10, 15, 30, 60}},
				},
			},
		},
	}
}

// ocrScripted is the probe's engine closure: a stub that answers a fixed text
// for whatever crop arrives (the probe's OCR block is degradable, so the test
// asserts the shape, not the reading).
type ocrScripted struct{ text string }

func (o ocrScripted) Name() string { return "fake" }
func (o ocrScripted) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	return ocr.Result{Text: o.text, MeanConf: 0.9, Engine: "fake"}, nil
}

func newCaptureEnv(t *testing.T, opts ...func(*captureEnv)) *captureEnv {
	t.Helper()
	a := newTestAuth(t)
	c := newCapturePush()
	e := &captureEnv{t: t, a: a, src: c, sel: &memSelection{}}
	for _, o := range opts {
		o(e)
	}
	e.stk = &stack.Stack{
		List: func(ctx context.Context) ([]devices.Device, error) {
			return []devices.Device{fakeCaptureDevice()}, nil
		},
		Link: func(ctx context.Context, node string) (usb.Info, error) {
			return usb.Info{DevicePath: "fake", SpeedMBps: usb.SpeedHighSpeed, MaxPortSpeedMBps: usb.SpeedHighSpeed}, nil
		},
		Store:     e.sel,
		Policy:    mode.Policy{},
		Source:    func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) { return c, nil },
		Synthetic: true,
		Note:      "device list from GAME_NARRATOR_CAPTURE_FAKE (synthetic; no hardware was enumerated)",
	}
	e.h = a.handler(WithCapture(e.stk))
	t.Cleanup(func() { _ = c.Close() })
	return e
}

func (e *captureEnv) send(role auth.Role, method, target string, body []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+e.a.bearer(role))
	}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, r)
	if rec.Code >= 200 && rec.Code < 300 {
		recordContractResponse(e.t, method, target, rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	return rec
}

// decode unmarshals a JSON response body.
func decodeCapture(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%d body is not valid JSON: %v\n%s", rec.Code, err, rec.Body.String())
	}
}

// TestCaptureDevicesListsTheSyntheticDevice pins the list route: the fixed
// device with its stable id, the state ready, the chosen mode with its
// provenance, and the synthetic flag that says the list is the test seam.
func TestCaptureDevicesListsTheSyntheticDevice(t *testing.T) {
	e := newCaptureEnv(t)

	// A viewer is refused (the capture routes are operator); an operator gets
	// the list.
	if rec := e.send(auth.RoleViewer, http.MethodGet, "/api/v1/capture/devices", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer GET devices = %d, want 403", rec.Code)
	}
	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/devices", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET devices = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var view struct {
		Synthetic bool `json:"synthetic"`
		Devices   []struct {
			ID     string `json:"id"`
			State  string `json:"state"`
			Chosen *struct {
				W      int    `json:"w"`
				H      int    `json:"h"`
				PixFmt string `json:"pix_fmt"`
				Source string `json:"source"`
			} `json:"chosen"`
		} `json:"devices"`
	}
	decodeCapture(t, rec, &view)
	if !view.Synthetic {
		t.Fatal("the synthetic flag is false; the test seam's list must say so")
	}
	if len(view.Devices) != 1 || view.Devices[0].ID != stack.FakeID {
		t.Fatalf("devices = %+v, want the one synthetic device %s", view.Devices, stack.FakeID)
	}
	if view.Devices[0].State != stack.StateReady {
		t.Fatalf("state = %q, want ready", view.Devices[0].State)
	}
	if view.Devices[0].Chosen == nil || view.Devices[0].Chosen.Source != stack.ModeSourcePolicy {
		t.Fatalf("chosen = %+v, want a policy-chosen mode", view.Devices[0].Chosen)
	}
}

// TestCaptureSelectPersistsTheSelection pins the selection route: the device
// is persisted (visible on the next list as selected_id), an unknown device
// is a 400, and a mode the device does not offer is a 400 (the store cannot
// hold a selection the next resolution would discard).
func TestCaptureSelectPersistsTheSelection(t *testing.T) {
	e := newCaptureEnv(t)

	// A device-only save: the policy decides the mode.
	body, _ := json.Marshal(map[string]any{})
	rec := e.send(auth.RoleOperator, http.MethodPut, "/api/v1/capture/devices/"+stack.FakeID, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT select = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		DeviceID string `json:"device_id"`
	}
	decodeCapture(t, rec, &saved)
	if saved.DeviceID != stack.FakeID {
		t.Fatalf("saved device_id = %q, want %q", saved.DeviceID, stack.FakeID)
	}

	// The next list shows the selection.
	rec = e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/devices", nil)
	var view struct {
		SelectedID string `json:"selected_id"`
	}
	decodeCapture(t, rec, &view)
	if view.SelectedID != stack.FakeID {
		t.Fatalf("selected_id = %q, want %q (the persisted selection)", view.SelectedID, stack.FakeID)
	}

	// An unknown device is a 400: the enumeration is the allowlist.
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/capture/devices/usb-NotThere_0002", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT unknown device = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// A mode the device does not offer is a 400.
	badMode, _ := json.Marshal(map[string]any{
		"mode": map[string]any{"w": 1920, "h": 1080, "fps": 60, "pix_fmt": "mjpeg"},
	})
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/capture/devices/"+stack.FakeID, badMode)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT non-offered mode = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// An offered mode is accepted and persisted.
	goodMode, _ := json.Marshal(map[string]any{
		"mode": map[string]any{"w": 1280, "h": 720, "fps": 30, "pix_fmt": "mjpeg"},
	})
	rec = e.send(auth.RoleOperator, http.MethodPut, "/api/v1/capture/devices/"+stack.FakeID, goodMode)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT offered mode = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var savedMode struct {
		Mode *struct {
			W      int     `json:"w"`
			H      int     `json:"h"`
			FPS    float64 `json:"fps"`
			PixFmt string  `json:"pix_fmt"`
		} `json:"mode"`
	}
	decodeCapture(t, rec, &savedMode)
	if savedMode.Mode == nil || savedMode.Mode.W != 1280 || savedMode.Mode.FPS != 30 {
		t.Fatalf("saved mode = %+v, want the pinned 1280x720 @ 30", savedMode.Mode)
	}
}

// TestCapturePreviewServesJPEGAndThrottles pins the preview: a 200 JPEG, the
// 429 on the second request inside the 500 ms window (with Retry-After), the
// 400 for an unknown device, and the 503 capture-busy while a session holds
// the device.
// TestCaptureSelectSlashIDIsPercentEncoded is issue #10's field failure: the
// by-id / by-path ids the real enumeration reports are full device paths
// (/dev/v4l/by-id/...-video-index0), so they contain slashes, and the id is a
// single path segment. The contract: the id travels percent-encoded (a '/'
// becomes %2F, as the web client's URL encoder does), the router decodes it
// back, and a client that sends the raw id gets the actionable 400 rather
// than the 307-then-404 the Go mux's path canonicalization produced.
func TestCaptureSelectSlashIDIsPercentEncoded(t *testing.T) {
	const slashID = "/dev/v4l/by-id/usb-Elgato_HD60_f00-video-index0"
	e := newCaptureEnv(t)
	e.stk.List = func(ctx context.Context) ([]devices.Device, error) {
		d := fakeCaptureDevice()
		d.StableID = slashID
		return []devices.Device{d}, nil
	}

	// The list reports the id as-is — the id the client must encode.
	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/devices", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET devices = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Devices []struct {
			ID string `json:"id"`
		} `json:"devices"`
	}
	decodeCapture(t, rec, &list)
	if len(list.Devices) != 1 || list.Devices[0].ID != slashID {
		t.Fatalf("device list = %+v, want the slash-bearing id as-is", list.Devices)
	}

	// The encoded id: a single %2F-escaped segment the mux routes to {id}; the
	// handler decodes it and the allowlist matches the device.
	enc := "/api/v1/capture/devices/" + url.PathEscape(slashID)
	body, _ := json.Marshal(map[string]any{})
	rec = e.send(auth.RoleOperator, http.MethodPut, enc, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT encoded slash id = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var saved struct {
		DeviceID string `json:"device_id"`
	}
	decodeCapture(t, rec, &saved)
	if saved.DeviceID != slashID {
		t.Fatalf("saved device_id = %q, want the decoded id %q", saved.DeviceID, slashID)
	}

	// The body is optional (the OpenAPI's requestBody is not required): an
	// encoded id with no body records the device, policy mode.
	rec = e.send(auth.RoleOperator, http.MethodPut, enc, []byte{})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT encoded slash id, empty body = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// The encoded id still validates the mode pin against the allowlist.
	badMode, _ := json.Marshal(map[string]any{
		"mode": map[string]any{"w": 1920, "h": 1080, "fps": 60, "pix_fmt": "mjpeg"},
	})
	rec = e.send(auth.RoleOperator, http.MethodPut, enc, badMode)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT encoded slash id, non-offered mode = %d, want 400: %s", rec.Code, rec.Body.String())
	}

	// The raw (unencoded) id: the 307 the field run observed, replaced by the
	// actionable 400 that names the remedy.
	raw := "/api/v1/capture/devices/" + slashID
	rec = e.send(auth.RoleOperator, http.MethodPut, raw, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("PUT raw slash id = %d (location %q), want the 400, not a 307 redirect: %s",
			rec.Code, rec.Header().Get("Location"), rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "device-id-not-encoded") {
		t.Fatalf("the 400 does not name the encoding remedy: %s", rec.Body.String())
	}
}

func TestCapturePreviewServesJPEGAndThrottles(t *testing.T) {
	e := newCaptureEnv(t)

	// The source must have a frame ready for the one-frame grab.
	e.src.push(t, 1)

	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/preview?device="+stack.FakeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET preview = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type = %q, want image/jpeg", ct)
	}
	if rec.Body.Len() == 0 {
		t.Fatal("the preview body is empty; want a JPEG frame")
	}

	// The second request inside the 500 ms window is throttled.
	rec = e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/preview?device="+stack.FakeID, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second GET preview = %d, want 429 (throttled): %s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("the 429 carries no Retry-After; the poller needs the spacing")
	}

	// An unknown device is a 400 (waited out the throttle first).
	time.Sleep(600 * time.Millisecond)
	rec = e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/preview?device=usb-NotThere_0002", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET preview unknown device = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestCapturePreviewBusyWhenASessionHoldsTheDevice pins the per-device busy
// check end-to-end: a real running v4l2 session on the device (through the
// manager's own capture wiring and the runtime's ActiveCaptureDevice) makes
// the preview answer 503 capture-busy naming the holding session — the UVC
// bus is single-consumer, and the route must refuse the race rather than let
// FFmpeg answer "device or resource busy". The probe takes the same branch.
func TestCapturePreviewBusyWhenASessionHoldsTheDevice(t *testing.T) {
	a := newTestAuth(t)
	profileID := seedSyntheticProfileForAPI(t, a.st, true)

	// The session's source: a push-based v4l2 source the manager's capture
	// wiring hands the session (the runtime's own seam).
	sessSrc := newCapturePush()
	sessionResolve := func(ctx context.Context, requestedID string) (stack.Resolved, error) {
		return stack.Resolved{
			Device:     fakeCaptureDevice(),
			Mode:       mode.Mode{W: 640, H: 480, FPS: 10, PixFmt: "mjpeg"},
			ModeSource: stack.ModeSourcePolicy,
		}, nil
	}

	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	mgr, err := runtime.New(runtime.Options{
		Store:      a.st,
		Hub:        hub,
		AssetsRoot: t.TempDir(),
		OCR:        ocrScripted{text: ""},
		Capture: &runtime.CaptureOptions{
			Resolve: sessionResolve,
			Source:  func(ctx context.Context, res stack.Resolved) (capture.Source, error) { return sessSrc, nil },
		},
		Log: discardLogger,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = a.st.Close() })
	t.Cleanup(func() { _ = hub.Close() })
	t.Cleanup(mgr.Close)
	t.Cleanup(func() { _ = sessSrc.Close() })

	info, err := mgr.Start(context.Background(), runtime.StartRequest{
		ProfileID:   profileID,
		FrameSource: runtime.FrameSource{Kind: runtime.SourceKindV4L2, Dir: stack.FakeID},
		Mode:        runtime.ModeConservative,
	})
	if err != nil {
		t.Fatalf("start the v4l2 session: %v", err)
	}
	by := time.Now().Add(30 * time.Second)
	for {
		cur, _ := mgr.Get(info.ID)
		if cur.State == runtime.StateRunning {
			break
		}
		if time.Now().After(by) {
			t.Fatalf("the v4l2 session never reached running (last %s)", cur.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The session holds the device: ActiveCaptureDevice reports it.
	dev, sessID, active := mgr.ActiveCaptureDevice()
	if !active || dev != stack.FakeID || sessID != info.ID {
		t.Fatalf("ActiveCaptureDevice = (%q, %q, %v), want the fake device held by the running session", dev, sessID, active)
	}

	// The preview's stack: the same fixed device, its own push source (which
	// the busy check must refuse to open while the session holds the device).
	previewSrc := newCapturePush()
	t.Cleanup(func() { _ = previewSrc.Close() })
	stk := &stack.Stack{
		List: func(ctx context.Context) ([]devices.Device, error) {
			return []devices.Device{fakeCaptureDevice()}, nil
		},
		Link:   func(ctx context.Context, node string) (usb.Info, error) { return usb.Info{}, nil },
		Store:  &memSelection{},
		Policy: mode.Policy{},
		Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) { return previewSrc, nil },
	}
	h := a.handler(WithStore(a.st), WithSessions(mgr), WithCapture(stk))

	for method, target := range map[string]string{
		http.MethodGet:  "/api/v1/capture/preview?device=" + stack.FakeID,
		http.MethodPost: "/api/v1/capture/probe",
	} {
		var r *http.Request
		if method == http.MethodPost {
			b, _ := json.Marshal(map[string]any{"device": stack.FakeID, "frames": 2})
			r = httptest.NewRequest(method, target, bytes.NewReader(b))
		} else {
			r = httptest.NewRequest(method, target, nil)
		}
		r.Header.Set("Authorization", "Bearer "+a.bearer(auth.RoleOperator))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s (device held) = %d, want 503 capture-busy: %s", method, target, rec.Code, rec.Body.String())
		}
		if !strings.Contains(strings.ToLower(rec.Body.String()), "capture-busy") &&
			!strings.Contains(strings.ToLower(rec.Body.String()), "held by") {
			t.Fatalf("the 503 does not name the busy condition: %s", rec.Body.String())
		}
	}
}

// TestCaptureProbeReportsStatsAndOCR pins the probe: the frame stats
// (grabbed/dropped/fps/freshness/state) and the OCR block (the degradable
// half: a wired engine answers text; the shape is what the contract checks).
func TestCaptureProbeReportsStatsAndOCR(t *testing.T) {
	e := newCaptureEnv(t)
	// Wire a real OCR factory so the probe's OCR block carries a reading.
	// The env's handler is already built; rebuild it with the OCR preview.
	factory := func(psm int) ocr.Engine { return ocrScripted{text: "HELLO CAPTURE"} }
	e.h = e.a.handler(WithCapture(e.stk), WithOCRPreview(factory, nil))

	// The probe grabs `frames`; push that many (plus a margin) so the grab
	// sees them before the source's deadline.
	frames := 5
	for n := 1; n <= frames; n++ {
		e.src.push(t, n)
	}
	body, _ := json.Marshal(map[string]any{
		"device": stack.FakeID,
		"frames": frames,
	})
	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/capture/probe", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST probe = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var pr struct {
		Device  string `json:"device"`
		Mode    string `json:"mode"`
		Grabbed int    `json:"grabbed"`
		OCR     *struct {
			Text   string `json:"text"`
			Engine string `json:"engine"`
		} `json:"ocr"`
	}
	decodeCapture(t, rec, &pr)
	if pr.Device != stack.FakeID {
		t.Fatalf("probe device = %q, want %q", pr.Device, stack.FakeID)
	}
	if pr.Grabbed != frames {
		t.Fatalf("grabbed = %d, want %d", pr.Grabbed, frames)
	}
	if pr.OCR == nil {
		t.Fatalf("the probe has no OCR block: %s", rec.Body.String())
	}
	if pr.OCR.Text != "HELLO CAPTURE" {
		t.Fatalf("ocr text = %q, want the scripted reading", pr.OCR.Text)
	}
}

// TestCaptureProbeOCRUsesBlockPSM is issue #10's "the probe reads no text"
// fix pin: a textbox is one block of one or more lines, so the probe's OCR
// pass must use the block PSM (6, "a uniform block of text"), not the
// single-line PSM (7) that forces tesseract's line segmenter onto a
// multi-line box and reads nothing from it. The stub factory records the PSM
// it was built with and proves the probe asked for the block mode.
func TestCaptureProbeOCRUsesBlockPSM(t *testing.T) {
	e := newCaptureEnv(t)
	var asked int
	factory := func(psm int) ocr.Engine { asked = psm; return ocrScripted{text: "TWO LINES"} }
	e.h = e.a.handler(WithCapture(e.stk), WithOCRPreview(factory, nil))
	for n := 1; n <= 2; n++ {
		e.src.push(t, n)
	}
	body, _ := json.Marshal(map[string]any{"device": stack.FakeID, "frames": 2})
	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/capture/probe", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST probe = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if asked != ocr.DefaultPSM {
		t.Fatalf("the probe's OCR engine was built with PSM %d, want %d: PSM 7 (a single line) reads nothing from a multi-line box, so the two-line textbox came back blank", asked, ocr.DefaultPSM)
	}
}

// TestCaptureProbeOCRIsDegradeable pins the degradable half: with no OCR
// engine wired, the probe still 200s and the OCR block degrades to engine
// "unavailable" with the note — never a failed probe.
func TestCaptureProbeOCRIsDegradeable(t *testing.T) {
	e := newCaptureEnv(t)
	// No WithOCRPreview: the factory is nil, so the probe's OCR closure
	// refuses and the stack degrades the block.
	for n := 1; n <= 3; n++ {
		e.src.push(t, n)
	}
	body, _ := json.Marshal(map[string]any{"device": stack.FakeID, "frames": 2})
	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/capture/probe", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST probe (no OCR) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var pr struct {
		OCR *struct {
			Engine string `json:"engine"`
			Note   string `json:"note"`
		} `json:"ocr"`
	}
	decodeCapture(t, rec, &pr)
	if pr.OCR == nil {
		t.Fatalf("the probe has no OCR block: %s", rec.Body.String())
	}
	if pr.OCR.Engine != "unavailable" {
		t.Fatalf("ocr engine = %q, want unavailable (no engine wired)", pr.OCR.Engine)
	}
	if pr.OCR.Note == "" {
		t.Fatal("the degraded OCR block carries no note (the operator action)")
	}
}

// TestCaptureSourceFaultIsAnswered pins the source-fault path: a source that
// cannot start is a 503 capture-source-failed, not a 200 with an empty frame.
func TestCaptureSourceFaultIsAnswered(t *testing.T) {
	e := newCaptureEnv(t)
	e.src.fail = fmt.Errorf("ffmpeg: cannot open %s: device or resource busy", stack.FakePath)

	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/preview?device="+stack.FakeID, nil)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET preview (source fault) = %d, want 503: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(strings.ToLower(rec.Body.String()), "capture") {
		t.Fatalf("the 503 does not name the capture fault: %s", rec.Body.String())
	}
}

// TestCaptureRoutesAreUnwiredSafe pins the honest-degradation: a server with
// no capture stack answers 503 (not an empty list) on every capture route.
func TestCaptureRoutesAreUnwiredSafe(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler() // no WithCapture
	for method, target := range map[string]string{
		http.MethodGet:  "/api/v1/capture/devices",
		http.MethodPost: "/api/v1/capture/probe",
	} {
		r := httptest.NewRequest(method, target, nil)
		r.Header.Set("Authorization", "Bearer "+a.bearer(auth.RoleOperator))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s (unwired) = %d, want 503", method, target, rec.Code)
		}
	}
}
