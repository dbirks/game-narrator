package api

// The capture signal and the session-mode preview (ADR-0110, UX-11): the
// signal route's two evidence classes — a running v4l2 session (measured fps,
// the ADR-0082 tracker's window, the last dialogue-region crop, the newest
// frame's age, all from the run's own state, no device grab) and the one-shot
// frame grab (the negotiated mode, the window settled from the one frame
// through the ADR-0082 single-frame seam, fps_actual null — one frame cannot
// measure a rate) — plus the preview's session path (the newest in-memory
// frame, ≤ 640 px JPEG, never a second device grab) and their 409/429/400
// answers.
//
// The session fixtures run a real v4l2 session through the runtime manager
// (the same wiring the production host uses, with the push seam standing in
// for the dongle), so the signal's session answer is read off a run that
// really processed the frames the test pushed.

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
)

// sessionCaptureFix runs a v4l2 session through the runtime manager on a
// push source: the session's profile (the synthetic textbox profile, so the
// dialogue-region crop has a region to crop), the manager, the push source
// the session reads, and the session's id.
type sessionCaptureFix struct {
	t       *testing.T
	a       *testAuth
	mgr     *runtime.Manager
	src     *capturePush
	sessID  string
	profile string
}

// startV4L2Session starts the session and waits for running: every assertion
// in these tests reads a session that is really delivering, not a fixture
// that claims to be one.
func startV4L2Session(t *testing.T, wideFrames bool) *sessionCaptureFix {
	t.Helper()
	a := newTestAuth(t)
	profileID := seedSyntheticProfileForAPI(t, a.st, true)

	src := newCapturePush()
	src.wide = wideFrames
	resolve := func(ctx context.Context, requestedID string) (stack.Resolved, error) {
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
			Resolve: resolve,
			Source:  func(ctx context.Context, res stack.Resolved) (capture.Source, error) { return src, nil },
		},
		Log: discardLogger,
	})
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = a.st.Close() })
	t.Cleanup(func() { _ = hub.Close() })
	t.Cleanup(mgr.Close)
	t.Cleanup(func() { _ = src.Close() })

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
	dev, sessID, active := mgr.ActiveCaptureDevice()
	if !active || dev != stack.FakeID || sessID != info.ID {
		t.Fatalf("ActiveCaptureDevice = (%q, %q, %v), want the fake device held by the running session", dev, sessID, active)
	}
	return &sessionCaptureFix{t: t, a: a, mgr: mgr, src: src, sessID: sessID, profile: profileID}
}

// handler builds the capture surface the fix's session runs behind: the
// sessions manager (the session's live view) and a preview stack on the same
// fixed device whose source seam is counted — a session-mode answer must
// never open it (the UVC bus stays with the session).
func (f *sessionCaptureFix) handler() (http.Handler, *int32) {
	previewSrc := newCapturePush()
	f.t.Cleanup(func() { _ = previewSrc.Close() })
	var srcOpens int32
	stk := &stack.Stack{
		List: func(ctx context.Context) ([]devices.Device, error) {
			return []devices.Device{fakeCaptureDevice()}, nil
		},
		Link: func(ctx context.Context, node string) (usb.Info, error) {
			return usb.Info{SpeedMBps: usb.SpeedHighSpeed, MaxPortSpeedMBps: usb.SpeedHighSpeed}, nil
		},
		Store:  &memSelection{},
		Policy: mode.Policy{},
		Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) {
			atomic.AddInt32(&srcOpens, 1)
			return previewSrc, nil
		},
		Synthetic: true,
		Note:      "device list from GAME_NARRATOR_CAPTURE_FAKE (synthetic; no hardware was enumerated)",
	}
	h := f.a.handler(WithStore(f.a.st), WithSessions(f.mgr), WithCapture(stk))
	return h, &srcOpens
}

// send issues a request on the handler and records 2xx bodies against the
// OpenAPI document (the same contract discipline as the env's send).
func (f *sessionCaptureFix) send(h http.Handler, role auth.Role, method, target string, body []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, target, bytes.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+f.a.bearer(role))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code >= 200 && rec.Code < 300 {
		recordContractResponse(f.t, method, target, rec.Code,
			rec.Header().Get("Content-Type"), rec.Body.Bytes())
	}
	return rec
}

// pushUntilLive pushes frames until the session's live view reports the
// wanted state (a frame in memory, a settled tracker window, a dialogue
// crop), keeping the pipeline fed while it gets there.
func (f *sessionCaptureFix) pushUntilLive(want func(l runtime.CaptureLive) bool) runtime.CaptureLive {
	f.t.Helper()
	by := time.Now().Add(30 * time.Second)
	for n := 0; ; n++ {
		f.src.push(f.t, n+1)
		live, ok := f.mgr.CaptureLive()
		if ok && want(live) {
			return live
		}
		if time.Now().After(by) {
			f.t.Fatalf("the session never reached the wanted live state in 30 s (last: ok=%v settled=%v dialogue=%v frame=%v)",
				ok, live.Picture.Settled, live.Dialogue != nil, live.Latest != nil)
		}
		time.Sleep(15 * time.Millisecond)
	}
}

// TestCaptureSignalSessionMode pins the signal's session answer: a running
// v4l2 session answers from the run's own state (the measured fps against
// the target, the ADR-0082 tracker's window on the synthetic bordered frame,
// the last dialogue-region crop, the newest frame's age) — no device grab.
func TestCaptureSignalSessionMode(t *testing.T) {
	f := startV4L2Session(t, true) // bordered frames: the tracker settles a window
	h, opens := f.handler()

	// Feed the pipeline until the tracker has settled its window (the
	// ADR-0082 settle count), the dialogue region has cropped, and a frame
	// is in memory.
	f.pushUntilLive(func(l runtime.CaptureLive) bool {
		return l.Picture.Settled && l.Dialogue != nil && l.Latest != nil && l.Latest.Image != nil
	})

	rec := f.send(h, auth.RoleOperator, http.MethodGet, "/api/v1/capture/signal?device="+stack.FakeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET signal (session) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var v struct {
		Device    string   `json:"device"`
		Mode      string   `json:"mode"`
		Source    string   `json:"source"`
		SessionID string   `json:"session_id"`
		Signal    bool     `json:"signal"`
		FPSActual *float64 `json:"fps_actual"`
		FPSTarget float64  `json:"fps_target"`
		Picture   struct {
			X       int  `json:"x"`
			Y       int  `json:"y"`
			W       int  `json:"w"`
			H       int  `json:"h"`
			Settled bool `json:"settled"`
			Active  bool `json:"active"`
		} `json:"picture"`
		DialogueCrop *struct {
			X, Y, W, H int
		} `json:"dialogue_crop"`
		AgeMS     int64 `json:"age_ms"`
		Synthetic bool  `json:"synthetic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("signal body is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if v.Device != stack.FakeID || v.Source != "session" || v.SessionID != f.sessID {
		t.Fatalf("device/source/session_id = %q/%q/%q, want the held device, the session evidence and the holding session", v.Device, v.Source, v.SessionID)
	}
	if !v.Signal {
		t.Fatal("signal = false, want true (the running session's source is delivering)")
	}
	if v.FPSActual == nil || *v.FPSActual <= 0 {
		t.Fatalf("fps_actual = %v, want the source's measured rate (the push seam reports 10)", v.FPSActual)
	}
	if v.FPSTarget != 10 {
		t.Fatalf("fps_target = %v, want 10 (the negotiated mode's rate)", v.FPSTarget)
	}
	diff := func(a, b int) int {
		if a > b {
			return a - b
		}
		return b - a
	}
	// The tracker's window on the bordered frame: settled, the bar-trimmed
	// content rectangle, and active — the "is the picture there" half the
	// black-frame test below answers false.
	if !v.Picture.Settled {
		t.Fatalf("picture.settled = false, want the tracker's settled window: %+v", v.Picture)
	}
	if diff(v.Picture.X, 160) > 2 || v.Picture.W != 640 || v.Picture.H != 480 {
		t.Fatalf("picture window = %+v, want the bar-trimmed 640x480 content rectangle at x=160", v.Picture)
	}
	if !v.Picture.Active {
		t.Fatalf("picture.active = false on the synthetic bordered frame, want the bar-framed picture found")
	}
	// The last dialogue-region crop: the textbox region mapped through the
	// tracker's window (the 640x480 content rectangle at x=160 on the
	// 960x480 bordered frame) — the same active window and MapRect the
	// runtime's cut used, in frame coordinates: x 0.096875 of the window
	// (222 on the frame), y 0.75625 (363), w 0.80625 of 640 (516),
	// h 0.158333 of 480 (76).
	if v.DialogueCrop == nil {
		t.Fatalf("dialogue_crop = null, want the textbox region's last crop rectangle")
	}
	if diff(v.DialogueCrop.X, 222) > 2 || diff(v.DialogueCrop.Y, 363) > 2 ||
		diff(v.DialogueCrop.W, 516) > 2 || diff(v.DialogueCrop.H, 76) > 2 {
		t.Fatalf("dialogue_crop = %+v, want ~(222, 363, 516, 76) in frame coordinates", v.DialogueCrop)
	}
	if v.AgeMS < 0 {
		t.Fatalf("age_ms = %d, want non-negative", v.AgeMS)
	}
	if !v.Synthetic {
		t.Fatal("synthetic = false, want the labelled test seam flag")
	}
	if atomic.LoadInt32(opens) != 0 {
		t.Fatalf("the signal's session answer opened the device %d times; it answers from the run's state, no device grab", *opens)
	}
}

// TestCaptureSignalOneShotBorderedFrame pins the signal's one-shot answer on
// the synthetic bordered frame (no session running): the negotiated mode and
// target fps, the window settled from the one frame through the ADR-0082
// single-frame seam, active=true on the bar-framed picture, fps_actual null
// (one frame cannot measure a rate) and no dialogue crop (the one-shot path
// has no profile regions).
func TestCaptureSignalOneShotBorderedFrame(t *testing.T) {
	e := newCaptureEnv(t)
	e.src.wide = true // the bordered frame the tracker settles on

	e.src.push(t, 1)
	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/signal?device="+stack.FakeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET signal (one-shot) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	// fps_actual is the explicit null, not an invented number.
	if !strings.Contains(rec.Body.String(), `"fps_actual":null`) {
		t.Fatalf("the one-shot answer must carry fps_actual null: %s", rec.Body.String())
	}
	var v struct {
		Device    string  `json:"device"`
		Mode      string  `json:"mode"`
		Source    string  `json:"source"`
		Signal    bool    `json:"signal"`
		FPSTarget float64 `json:"fps_target"`
		Picture   struct {
			Settled bool `json:"settled"`
			Active  bool `json:"active"`
			W       int  `json:"w"`
			H       int  `json:"h"`
		} `json:"picture"`
		DialogueCrop *struct{} `json:"dialogue_crop"`
		Synthetic    bool      `json:"synthetic"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("signal body is not valid JSON: %v\n%s", err, rec.Body.String())
	}
	if v.Source != "oneshot" || !v.Signal {
		t.Fatalf("source/signal = %q/%v, want the one-shot evidence with a frame grabbed", v.Source, v.Signal)
	}
	if v.Device != stack.FakeID {
		t.Fatalf("device = %q, want the resolved device", v.Device)
	}
	if v.Mode == "" || v.FPSTarget <= 0 {
		t.Fatalf("mode/fps_target = %q/%v, want the negotiated mode with its rate", v.Mode, v.FPSTarget)
	}
	if !v.Picture.Settled || !v.Picture.Active {
		t.Fatalf("picture = settled %v active %v, want the settled window with the bar-framed picture found", v.Picture.Settled, v.Picture.Active)
	}
	if v.Picture.W <= 0 || v.Picture.H <= 0 {
		t.Fatalf("picture window is empty on the bordered frame")
	}
	if v.DialogueCrop != nil {
		t.Fatal("dialogue_crop present on the one-shot answer; the one-shot path has no profile regions, so it stays absent")
	}
	if !v.Synthetic {
		t.Fatal("synthetic = false, want the labelled test seam flag")
	}
}

// TestCaptureSignalOneShotBlackFrame pins the "no active picture" state: a
// black frame is a frame (signal present — the source delivers), but the
// active-picture scan finds no bar-framed picture, so picture.active is
// false and the UI's "no active picture" text is what renders.
func TestCaptureSignalOneShotBlackFrame(t *testing.T) {
	e := newCaptureEnv(t)
	e.src.blank = true

	e.src.push(t, 1)
	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/signal?device="+stack.FakeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET signal (black frame) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var v struct {
		Signal  bool `json:"signal"`
		Picture struct {
			Settled bool `json:"settled"`
			Active  bool `json:"active"`
		} `json:"picture"`
	}
	decodeCapture(t, rec, &v)
	if !v.Signal {
		t.Fatal("signal = false on a black frame, want true (frames arrive; the card is connected)")
	}
	if v.Picture.Active {
		t.Fatal("picture.active = true on a black frame, want false (the 'no active picture' state)")
	}
}

// TestCaptureSignalOneShotThrottlesAndRefuses pins the one-shot path's 429
// (the shared global throttle: the second request inside the 500 ms window
// is told to retry) and the 400 for a client-named device outside the
// enumeration (the allowlist rule).
func TestCaptureSignalOneShotThrottlesAndRefuses(t *testing.T) {
	e := newCaptureEnv(t)
	e.src.push(t, 1)

	rec := e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/signal?device="+stack.FakeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET signal = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// The second request inside the 500 ms window: throttled, with the
	// Retry-After the poller needs.
	rec = e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/signal?device="+stack.FakeID, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second GET signal = %d, want 429 (the shared throttle): %s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("the 429 carries no Retry-After; the poller needs the spacing")
	}

	// A client-named device outside the enumeration is a client error.
	time.Sleep(previewThrottleMinInterval + 50*time.Millisecond)
	rec = e.send(auth.RoleOperator, http.MethodGet, "/api/v1/capture/signal?device=usb-unknown-9999", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET signal (unknown device) = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capture-device-unknown") {
		t.Fatalf("the 400 does not name the unknown device: %s", rec.Body.String())
	}
}

// TestCaptureSignalNoDevice409 pins the 409: no session running and no
// device to grab from — the "no session is running" state the spec names,
// with the operator action in the detail.
func TestCaptureSignalNoDevice409(t *testing.T) {
	a := newTestAuth(t)
	stk := &stack.Stack{
		List:   func(ctx context.Context) ([]devices.Device, error) { return nil, nil },
		Link:   func(ctx context.Context, node string) (usb.Info, error) { return usb.Info{}, nil },
		Store:  &memSelection{},
		Policy: mode.Policy{},
		Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) {
			t.Fatalf("no device is enumerated; the source seam must never run")
			return nil, nil
		},
		Synthetic: true,
		Note:      "no capture devices attached",
	}
	h := a.handler(WithStore(a.st), WithCapture(stk))

	rec := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/capture/signal", nil)
		r.Header.Set("Authorization", "Bearer "+a.bearer(auth.RoleOperator))
		out := httptest.NewRecorder()
		h.ServeHTTP(out, r)
		return out
	}()
	if rec.Code != http.StatusConflict {
		t.Fatalf("GET signal (no device) = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capture-signal-unavailable") {
		t.Fatalf("the 409 does not name the unavailable state: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "connect the UVC HDMI capture dongle") {
		t.Fatalf("the 409's detail must carry the operator action: %s", rec.Body.String())
	}
}

// TestCapturePreviewServesTheRunningSessionsFrame pins the preview's session
// path (ADR-0110 §2): while a v4l2 session holds the device, the preview is
// the session's newest in-memory frame — the ≤ 640 px JPEG, served from
// memory, never a second device grab — and the 409 the session answers while
// it has not delivered a frame yet. The probe on the same device is still
// refused 503 capture-busy: it grabs.
func TestCapturePreviewServesTheRunningSessionsFrame(t *testing.T) {
	f := startV4L2Session(t, true)
	h, opens := f.handler()

	// Before the first delivery: the session holds the device and has no
	// frame in memory — the preview is 409, not a 503 and not a second grab.
	rec := f.send(h, auth.RoleOperator, http.MethodGet, "/api/v1/capture/preview?device="+stack.FakeID, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("GET preview (session, no frame yet) = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capture-preview-unavailable") {
		t.Fatalf("the 409 does not name the unavailable frame: %s", rec.Body.String())
	}
	if n := atomic.LoadInt32(opens); n != 0 {
		t.Fatalf("the 409 path opened the device %d times; it must not race the session for the node", n)
	}

	// Feed the session until a frame is in memory, then the preview serves it.
	f.pushUntilLive(func(l runtime.CaptureLive) bool { return l.Latest != nil && l.Latest.Image != nil })
	time.Sleep(previewThrottleMinInterval + 50*time.Millisecond) // the shared throttle
	rec = f.send(h, auth.RoleOperator, http.MethodGet, "/api/v1/capture/preview?device="+stack.FakeID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET preview (session, frame in memory) = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("content-type = %q, want image/jpeg", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("cache-control = %q, want no-store (the frame is never stored)", cc)
	}
	img, err := jpeg.Decode(bytes.NewReader(rec.Body.Bytes()))
	if err != nil {
		t.Fatalf("the preview body is not a decodable JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() > 640 {
		t.Fatalf("preview width = %d, want the ≤ 640 px bound (ADR-0110 §2)", b.Dx())
	}
	if b.Dy() <= 0 {
		t.Fatalf("preview height = %d, want positive", b.Dy())
	}
	if n := atomic.LoadInt32(opens); n != 0 {
		t.Fatalf("the session-mode preview opened the device %d times; the JPEG is the session's in-memory frame", n)
	}

	// The probe on the session's device is still refused: it grabs, and the
	// UVC bus stays with the session.
	body, _ := json.Marshal(map[string]any{"device": stack.FakeID, "frames": 2})
	rec = f.send(h, auth.RoleOperator, http.MethodPost, "/api/v1/capture/probe", body)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("POST probe (device held) = %d, want 503 capture-busy: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "capture-busy") {
		t.Fatalf("the 503 does not name the busy condition: %s", rec.Body.String())
	}
}
