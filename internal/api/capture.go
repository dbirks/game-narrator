package api

// The capture product surface (ADR-0078): the UVC HDMI capture dongle's
// device list and selection, the throttled preview snapshot and the capture
// probe. The selection and operation logic lives in internal/capture/stack so
// the Settings page, the session runtime and the doctor read one spelling of
// the same device and mode; the handlers map the stack's sentinels onto
// documented problem codes and own the two HTTP concerns the stack has no
// business knowing — the preview's global throttle and the per-device busy
// check against a running session (the UVC bus is single-consumer).
//
// docs/spec/10's "never assume /dev/video0" holds here: a client may name a
// device only by the stable id the device list reports, and every other name
// is refused (400), the enumeration being the allowlist. The synthetic seam
// (GAME_NARRATOR_CAPTURE_FAKE) is the labelled test seam: every answer
// carries the synthetic flag, and nothing here fakes a hardware answer.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// The preview's throttle (ADR-0078 §3, docs/spec/08 "rate-limit preview
// streaming"): one snapshot in flight, a 500 ms minimum interval, global
// (not per-client) because the budget is the decode cores and the USB bus,
// not the caller. The state lives on the Server so every request shares it.
const (
	previewThrottleMinInterval = 500 * time.Millisecond
	previewRetryAfterSeconds   = 1
)

// captureSelectRequest is PUT /api/v1/capture/devices/{id}'s body: the device
// is always the path's id; the mode is the operator's optional pin (omitting
// it records "the policy decides for this device", ADR-0078).
type captureSelectRequest struct {
	Mode *captureModeView `json:"mode,omitempty"`
}

// captureModeView is the API spelling of a capture mode (the four fields the
// store's row and the device's offered formats share).
type captureModeView struct {
	W      int     `json:"w"`
	H      int     `json:"h"`
	FPS    float64 `json:"fps"`
	PixFmt string  `json:"pix_fmt"`
}

// captureSelectResponse is PUT's answer: the saved row.
type captureSelectResponse struct {
	DeviceID string           `json:"device_id"`
	Mode     *captureModeView `json:"mode,omitempty"`
}

// captureProbeRequest is POST /api/v1/capture/probe's body (docs/spec/10's
// capture test): the device (empty = the stack's own resolution), the frame
// count (0 = the default 20, capped 200) and the OCR region (a pixel box;
// absent, the centre third).
type captureProbeRequest struct {
	Device string     `json:"device,omitempty"`
	Frames int        `json:"frames,omitempty"`
	Region *stack.Box `json:"region,omitempty"`
}

// handleCaptureDevices answers GET /api/v1/capture/devices: the enumeration
// with, per device, the stable id, the USB link, the state and the chosen
// mode with its provenance (the operator's pin or the policy's pick). An
// unwired stack answers 503; a lister fault answers 503 with the operator
// action; an empty list is a 200 — the dongle-unplugged appliance is a real
// configuration the UI shows as "input offline", not a fault.
func (s *Server) handleCaptureDevices(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process")
		return
	}
	view, err := s.capture.Enumerate(r.Context())
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-enumeration-failed",
			"CAPTURE_ENUMERATION_FAILED", "Service Unavailable",
			"enumerating the capture devices failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// handleCaptureSelect answers PUT /api/v1/capture/devices/{id}: persist the
// operator's selection (the device always, the mode when pinned) against the
// live enumeration, so the store cannot hold a selection the next resolution
// would silently discard (ADR-0078: the validation the store cannot do).
func (s *Server) handleCaptureSelect(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-device-id",
			"Bad Request", "the device id in the path is empty")
		return
	}
	var req captureSelectRequest
	dec := captureBodyDecoder(w, r)
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		// A missing or empty body is legal (the OpenAPI's requestBody is not
		// required): it records the device with the policy-decided mode.
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-capture-selection",
			"Bad Request", "the capture selection body is not valid JSON")
		return
	}
	var m *mode.Mode
	if req.Mode != nil {
		// The pin is validated by the stack against the device's offered
		// formats (an unknown device or a non-offered mode is a 400, not a
		// saved row the session would refuse later).
		mm := mode.Mode{W: req.Mode.W, H: req.Mode.H, FPS: req.Mode.FPS, PixFmt: req.Mode.PixFmt}
		m = &mm
	}
	sel, err := s.capture.Save(r.Context(), id, m)
	if err != nil {
		s.captureProblem(w, r, err)
		return
	}
	resp := captureSelectResponse{DeviceID: sel.DeviceID}
	if sel.Mode != nil {
		resp.Mode = &captureModeView{W: sel.Mode.W, H: sel.Mode.H, FPS: sel.Mode.FPS, PixFmt: sel.Mode.PixFmt}
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleCapturePreview answers GET /api/v1/capture/preview?device=: the
// newest frame as a downscaled JPEG (≤ 640 px wide, quality 60, the shared
// encoder) (ADR-0110 §2, UX-11). The route is authenticated (the scope
// grant) and throttled globally: one snapshot in flight and a 500 ms minimum
// interval, answered 429 with Retry-After when exceeded. While a running
// v4l2 session holds the resolved device, the JPEG is the session's newest
// in-memory frame — served from memory, never stored, never a second device
// grab (the UVC bus stays with the session, so the old 503 capture-busy
// answer for the same-device preview is replaced by the picture the operator
// asked for). Without a session (or with a different device named), the
// one-shot throwaway path runs: build, start, one frame, encode, close —
// still refused 503 capture-busy in the window where a session start holds
// the device before the hold is visible.
func (s *Server) handleCapturePreview(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process")
		return
	}

	// The global throttle (ADR-0078/ADR-0110): one one-shot in flight, 500 ms
	// apart. The in-flight counter is the single-snapshot bound (a second
	// request that arrives mid-grab is told to retry, not queued), and the
	// interval is the decode-budget spacing. Both fail closed toward the 429.
	// A session-mode answer reads memory only, but it shares the throttle so
	// a polling pair (preview + signal) cannot double the one-shot budget.
	if !atomic.CompareAndSwapInt32(&s.capturePreviewInFlight, 0, 1) {
		s.writePreviewThrottled(w, r, "another preview snapshot is in flight")
		return
	}
	defer func() { atomic.StoreInt32(&s.capturePreviewInFlight, 0) }()

	now := time.Now()
	last := time.Unix(0, atomic.LoadInt64(&s.capturePreviewLastNano))
	if wait := previewThrottleMinInterval - now.Sub(last); wait > 0 {
		s.writePreviewThrottled(w, r, "the preview is rate-limited to one snapshot per 500 ms")
		return
	}
	atomic.StoreInt64(&s.capturePreviewLastNano, now.UnixNano())

	device := strings.TrimSpace(r.URL.Query().Get("device"))

	// The session's picture (ADR-0110 §2): while a v4l2 session holds the
	// resolved device, the preview is the session's newest in-memory frame —
	// no device grab, no UVC-bus race. The id the preview would open is the
	// stack's resolution of the request (the requested id, else the stored
	// selection, else the first usable device), so the comparison is on what
	// will actually be opened.
	if live, ok := s.captureLiveSession(); ok {
		resID, rerr := s.capture.ResolveID(r.Context(), device)
		if rerr != nil {
			s.captureProblem(w, r, rerr)
			return
		}
		if resID == live.Device {
			s.writeSessionPreview(w, r, live)
			return
		}
	}

	// The one-shot path (no session on this device, or none at all): the
	// per-device busy check keeps a throwaway source off a node a session is
	// starting onto (the window where the hold is not yet visible).
	if s.sessions != nil {
		if heldBy, sessionID, active := s.sessions.ActiveCaptureDevice(); active {
			resID, rerr := s.capture.ResolveID(r.Context(), device)
			if rerr == nil && resID == heldBy {
				s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-busy",
					"CAPTURE_BUSY", "Service Unavailable",
					fmt.Sprintf("the capture device %s is held by the running session %s; stop the session before previewing it", resID, sessionID))
				return
			}
		}
	}

	data, _, err := s.capture.Snapshot(r.Context(), device)
	if err != nil {
		s.captureProblem(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// captureLiveSession reports the running v4l2 session's live view (ADR-0110
// §1/§2): nil sessions wiring or no active capture session answers false, so
// the routes fall through to the one-shot path (or its 409/503 answers).
// The runtime's own CaptureLive answers v4l2-source sessions only — a replay
// or test-pattern run is not the card's source, so it never answers the
// HDMI input's question (the one-shot path answers against the card).
func (s *Server) captureLiveSession() (rtCaptureLive, bool) {
	if s.sessions == nil {
		return rtCaptureLive{}, false
	}
	return s.sessions.CaptureLive()
}

// writeSessionPreview serves the session's newest in-memory frame as the
// preview's JPEG (ADR-0110 §2): the shared encoder's ≤ 640 px cap, quality
// 60, no-store — the frame is encoded from memory and dropped at the end of
// the request, never written to disk. A session that has started but not
// delivered a frame yet answers 409 with the reason: "no frame in memory
// yet" is the honest state for a session that is starting, not a 500 and not
// a stale one-shot grab that would race the session for the node.
func (s *Server) writeSessionPreview(w http.ResponseWriter, r *http.Request, live rtCaptureLive) {
	if live.Latest == nil || live.Latest.Image == nil {
		detail := "the running session has not delivered a frame yet; the preview serves the session's newest in-memory frame, so wait for the first delivery"
		if live.Source.LastError != "" {
			detail += " (source: " + live.Source.LastError + ")"
		}
		s.writeCodeProblem(w, r, http.StatusConflict, "capture-preview-unavailable",
			"PREVIEW_UNAVAILABLE", "Conflict", detail)
		return
	}
	data, _, _, err := stack.EncodeJPEGDownscaled(live.Latest.Image, stackPreviewMaxWidth, stackPreviewJPEGQuality)
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-preview-encode-failed",
			"PREVIEW_ENCODE_FAILED", "Service Unavailable", "encoding the preview JPEG failed: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// stackPreviewMaxWidth and stackPreviewJPEGQuality mirror the stack's
// preview constants (ADR-0110 §2): one width bound and one quality recipe
// for both halves of the preview answer — the one-shot snapshot and the
// session's in-memory frame — so the two cannot drift apart.
const (
	stackPreviewMaxWidth    = 640
	stackPreviewJPEGQuality = 60
)

// The signal answer's evidence-class words (ADR-0110 §1): a running v4l2
// session answers source "session", the one-shot throwaway grab answers
// "oneshot". The contract's enum vocabulary is pinned to these in
// openapi_enums_test.go — the writer and the document cannot drift.
const (
	signalSourceSession = "session"
	signalSourceOneShot = "oneshot"
)

// captureSignalView is GET /api/v1/capture/signal's answer (ADR-0110 §1,
// UX-11): the signal state of the HDMI input path, from two sources of
// evidence — a running v4l2 session (the rich answer: measured fps, the
// tracker's window, the last dialogue crop, the newest frame's age) or a
// one-shot frame grab (the no-session answer: the negotiated mode, the
// window settled from the one frame through the ADR-0082 single-frame seam,
// and no measured rate — one frame cannot measure one, so fps_actual is
// null there).
type captureSignalView struct {
	// Device is the stable id of the device the answer describes.
	Device string `json:"device"`
	// Mode is the negotiated format (e.g. "640x480 MJPEG @ 10 fps").
	Mode string `json:"mode"`
	// Source names the evidence: "session" (a running v4l2 session) or
	// "oneshot" (a single-frame grab through the throwaway path).
	Source string `json:"source"`
	// SessionID names the holding session (source "session" only).
	SessionID string `json:"session_id,omitempty"`
	// Signal is the source's aliveness: a running session's source is
	// delivering (state running), a one-shot grab got a frame. A card
	// delivering a black pattern is signal PRESENT (frames arrive); the
	// picture's active flag is the "is the picture there" half.
	Signal bool `json:"signal"`
	// FPSActual is the measured delivery rate; null in the one-shot answer
	// (one frame cannot measure a rate — the JSON is honest about the
	// evidence class rather than inventing a number).
	FPSActual *float64 `json:"fps_actual"`
	// FPSTarget is the rate the negotiated mode asks (0 when the source has
	// no named rate).
	FPSTarget float64 `json:"fps_target"`
	// Picture is the active-picture window: the tracker's accepted window
	// once settled, the whole frame plus settled=false until then (the
	// ADR-0082 fresh-start behaviour), with active the "is the picture
	// there" judgment on the newest frame (a bar-framed, non-black picture
	// was found — a black/static feed answers active=false, the "no active
	// picture" state the UI renders).
	Picture captureSignalPicture `json:"picture"`
	// DialogueCrop is the last crop rectangle the session's ROI stage
	// computed for the dialogue region, in frame coordinates; absent until
	// the run has computed one (the one-shot answer has no profile regions,
	// so it stays absent there).
	DialogueCrop *captureSignalCrop `json:"dialogue_crop,omitempty"`
	// AgeMS is the age of the last frame (session: the source's
	// self-reported last-frame time; one-shot: the grab's own).
	AgeMS int64 `json:"age_ms"`
	// Synthetic labels the test seam (the ADR-0078 stack's flag): the
	// answer is not the hardware of this machine.
	Synthetic bool `json:"synthetic"`
}

type captureSignalPicture struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
	// Settled is true once the tracker has accepted a window (the
	// ADR-0082 settle count has elapsed); false answers carry the whole
	// frame as the mapping basis.
	Settled bool `json:"settled"`
	// Active is the "is the picture there" judgment on the newest frame
	// (the same ActiveArea scan the tracker runs per frame): a black or
	// static-pattern frame answers false — "no active picture" (UX-11).
	Active bool `json:"active"`
}

type captureSignalCrop struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// handleCaptureSignal answers GET /api/v1/capture/signal?device= (ADR-0110
// §1, UX-11): the JSON sibling of the preview. A running v4l2 session on
// the resolved device answers from the session's live state (no device
// grab); without one, the one-shot path grabs a single frame from the
// selected device — cheap (one frame, the preview's own path) and the
// evidence the no-session state owes. When neither is possible (no device,
// the device not delivering, the device held) the route answers 409
// capture-signal-unavailable with the operator action in the detail — the
// "no session is running" state the spec names — while a client-named
// device that is not in the enumeration stays a 400 (the enumeration is
// the allowlist, ADR-0078). The route shares the preview's global throttle
// because the one-shot mode opens the same single-consumer node.
func (s *Server) handleCaptureSignal(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process")
		return
	}

	// The shared throttle (ADR-0110 §1): the one-shot answer opens the
	// single-consumer node and decodes a frame — that is the preview's
	// CPU/USB budget, so a polling signal client cannot poll it away. The
	// session answer reads memory only but shares the counter, which keeps
	// the budget global (one operator, one poll loop).
	if !atomic.CompareAndSwapInt32(&s.capturePreviewInFlight, 0, 1) {
		s.writePreviewThrottled(w, r, "the capture signal is rate-limited: another one-shot capture is in flight")
		return
	}
	defer func() { atomic.StoreInt32(&s.capturePreviewInFlight, 0) }()

	now := time.Now()
	last := time.Unix(0, atomic.LoadInt64(&s.capturePreviewLastNano))
	if wait := previewThrottleMinInterval - now.Sub(last); wait > 0 {
		s.writePreviewThrottled(w, r, "the capture signal is rate-limited to one answer per 500 ms")
		return
	}
	atomic.StoreInt64(&s.capturePreviewLastNano, now.UnixNano())

	device := strings.TrimSpace(r.URL.Query().Get("device"))

	// The session's live answer (ADR-0110 §1), when a v4l2 session holds
	// the resolved device: measured fps, the tracker's window, the last
	// dialogue crop, the newest frame's age — all from the run's own state,
	// no device grab.
	if live, ok := s.captureLiveSession(); ok {
		resID, rerr := s.capture.ResolveID(r.Context(), device)
		if rerr != nil {
			s.captureProblem(w, r, rerr)
			return
		}
		if resID == live.Device {
			writeJSON(w, http.StatusOK, s.sessionSignalView(live))
			return
		}
	}

	// The one-shot answer: one frame from the selected device (the ADR-0078
	// throwaway path, the cheap option the no-session state takes), the
	// window settled from that one frame through the ADR-0082 single-frame
	// seam. The per-device busy check keeps the grab off a node a session
	// start holds in the window where the hold is not yet visible.
	if s.sessions != nil {
		if heldBy, sessionID, active := s.sessions.ActiveCaptureDevice(); active {
			resID, rerr := s.capture.ResolveID(r.Context(), device)
			if rerr == nil && resID == heldBy {
				s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-busy",
					"CAPTURE_BUSY", "Service Unavailable",
					fmt.Sprintf("the capture device %s is held by the running session %s; stop the session before reading its signal", resID, sessionID))
				return
			}
		}
	}

	fv, err := s.capture.Frame(r.Context(), device)
	if err != nil {
		// A client-named device outside the enumeration is a client error
		// (400, the allowlist rule); every other one-shot failure with no
		// session running is the 409 the spec names, with the operator
		// action in the detail.
		if errors.Is(err, stack.ErrDeviceUnknown) {
			s.captureProblem(w, r, err)
			return
		}
		s.writeCodeProblem(w, r, http.StatusConflict, "capture-signal-unavailable",
			"SIGNAL_UNAVAILABLE", "Conflict",
			"no session is running and a one-shot frame could not be grabbed from the selected device: "+oneShotReason(err))
		return
	}

	writeJSON(w, http.StatusOK, s.oneShotSignalView(fv))
}

// rtCaptureLive is the runtime's live capture-session view (ADR-0110):
// an alias keeps the two capture routes' signatures readable at the call
// site; the fields are the runtime's (session id, device, the negotiated
// format and target fps, the source's self-report, the tracker's window,
// the last dialogue crop, the newest in-memory frame).
type rtCaptureLive = rt.CaptureLive

// sessionSignalView shapes the session's live view into the signal answer:
// the measured fps against the target, the tracker's window with the
// active-picture judgment on the newest frame, the last dialogue crop and
// the last frame's age. Signal is the source's aliveness (a card delivering
// a black pattern is signal present; picture.active is the other half).
func (s *Server) sessionSignalView(live rtCaptureLive) captureSignalView {
	fv := captureSignalView{
		Device:    live.Device,
		Mode:      live.Format,
		Source:    signalSourceSession,
		SessionID: live.SessionID,
		Signal:    live.Source.State == string(capture.StateRunning),
		FPSTarget: live.FPSTarget,
		Synthetic: s.capture.Synthetic,
	}
	actual := live.Source.FPS
	fv.FPSActual = &actual
	fv.Picture = captureSignalPicture{
		X:       live.Picture.X,
		Y:       live.Picture.Y,
		W:       live.Picture.W,
		H:       live.Picture.H,
		Settled: live.Picture.Settled,
		Active:  activePicture(live.Latest),
	}
	if live.Dialogue != nil {
		c := captureSignalCrop{X: live.Dialogue.X, Y: live.Dialogue.Y, W: live.Dialogue.W, H: live.Dialogue.H}
		fv.DialogueCrop = &c
	}
	if !live.Source.LastFrameAt.IsZero() {
		fv.AgeMS = time.Since(live.Source.LastFrameAt).Round(time.Millisecond).Milliseconds()
	}
	return fv
}

// oneShotSignalView shapes the one-shot frame grab into the signal answer:
// the negotiated mode and target fps, the window from the tracker's
// single-frame Settle (the ADR-0082 amendment-1 seam — a one-frame input
// has no stream to hysteresise over), the active-picture judgment on the
// frame, and no measured rate (one frame cannot measure one: fps_actual
// stays null).
func (s *Server) oneShotSignalView(fv stack.FrameView) captureSignalView {
	view := captureSignalView{
		Device:    fv.Device,
		Mode:      fv.Mode.String(),
		Source:    signalSourceOneShot,
		Signal:    true,
		FPSTarget: fv.Mode.FPS,
		Synthetic: s.capture.Synthetic,
	}
	tr := vision.NewTracker(vision.TrackerConfig{})
	tr.Settle(fv.Frame.Image)
	if b, ok := tr.Box(); ok {
		view.Picture = captureSignalPicture{
			X: b.Min.X, Y: b.Min.Y, W: b.Dx(), H: b.Dy(),
			Settled: true,
			Active:  activePictureFrame(fv.Frame.Image),
		}
	} else {
		w, h := fv.Frame.Bounds()
		view.Picture = captureSignalPicture{
			W: w, H: h,
			Settled: false,
			Active:  activePictureFrame(fv.Frame.Image),
		}
	}
	if !fv.Stats.LastFrameAt.IsZero() {
		view.AgeMS = time.Since(fv.Stats.LastFrameAt).Round(time.Millisecond).Milliseconds()
	}
	return view
}

// activePicture reports the "is the picture there" judgment (UX-11) for a
// possibly-absent frame: the same ActiveArea scan the tracker runs per
// frame finds a bar-framed, non-black picture. nil answers false — a
// session that has not delivered a frame has no picture, and the UI's
// "no active picture" text is the honest render.
func activePicture(f *capture.Frame) bool {
	if f == nil || f.Image == nil {
		return false
	}
	return activePictureFrame(f.Image)
}

func activePictureFrame(img image.Image) bool {
	if img == nil {
		return false
	}
	_, ok := vision.ActiveArea(img)
	return ok
}

// oneShotReason turns a one-shot grab failure into the 409's detail: the
// sentinel's operator action (attach the dongle, re-seat it, name a device
// the enumeration has), not a raw driver error.
func oneShotReason(err error) string {
	switch {
	case errors.Is(err, stack.ErrDeviceAbsent):
		return "no usable capture device is attached: connect the UVC HDMI capture dongle and retry (docs/spec/10)"
	case errors.Is(err, stack.ErrSourceUnwired):
		return "no capture source builder is wired into this process"
	case errors.Is(err, stack.ErrModeUnavailable):
		return "no offered capture mode fits the selection policy: the device offers no format the card's link budget admits"
	case errors.Is(err, stack.ErrNoSelectionStore):
		return "the capture selection cannot be persisted: the settings store is not wired into this process"
	default:
		return "the capture device is not delivering frames: " + err.Error()
	}
}

// handleCaptureProbe answers POST /api/v1/capture/probe (docs/spec/10's
// capture test): grab `frames` from a throwaway source, report the freshness
// and drop stats the spec asks for, and OCR the requested region of the last
// frame with the region editor's Tesseract factory. OCR is degradable: a
// missing engine degrades the ocr block, never the probe.
func (s *Server) handleCaptureProbe(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process")
		return
	}
	var req captureProbeRequest
	dec := captureBodyDecoder(w, r)
	if err := dec.Decode(&req); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-capture-probe",
			"Bad Request", "the capture probe body is not valid JSON")
		return
	}

	// The per-device busy check: a probe that opens the device a session is
	// running would race the session's source (single-consumer bus).
	if s.sessions != nil {
		if heldBy, sessionID, active := s.sessions.ActiveCaptureDevice(); active {
			// Only the same device conflicts; a two-device bench's other
			// picture is legitimate (ADR-0078 consequence).
			resID, rerr := s.capture.ResolveID(r.Context(), req.Device)
			if rerr == nil && resID == heldBy {
				s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-busy",
					"CAPTURE_BUSY", "Service Unavailable",
					fmt.Sprintf("the capture device %s is held by the running session %s; stop the session before probing it", resID, sessionID))
				return
			}
		}
	}

	var region stack.Box
	if req.Region != nil {
		region = *req.Region
	}
	result, err := s.capture.Probe(r.Context(), req.Device, req.Frames, region, s.captureProbeOCR())
	if err != nil {
		s.captureProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// captureProbeOCR builds the probe's engine closure from the region editor's
// Tesseract factory (one engine spelling, ADR-0078 option 5). The PSM is the
// block default (PSM 6, "assume a single uniform block of text"): a textbox
// is one block of one or more lines, and the single-line PSM 7 forces
// tesseract's line segmenter onto a multi-line box and returns nothing for
// it — issue #10's field observation ("no OCR results on a two-line box")
// is exactly that. A nil factory (an unwired OCR engine) is a closure that
// refuses, which the stack turns into the degradable ocr block rather than a
// failed probe.
func (s *Server) captureProbeOCR() func(ctx context.Context, img image.Image) (string, error) {
	return func(ctx context.Context, img image.Image) (string, error) {
		if s.ocrPreview.Factory == nil {
			return "", errors.New("no OCR engine is wired into this process")
		}
		res, err := s.ocrPreview.Factory(ocr.DefaultPSM).Recognize(ctx, img)
		if err != nil {
			return "", err
		}
		return res.Text, nil
	}
}

// writePreviewThrottled answers the preview's 429 with the Retry-After the
// client's poller needs (docs/spec/08: the preview streaming is rate-limited,
// and the answer says when to try again).
func (s *Server) writePreviewThrottled(w http.ResponseWriter, r *http.Request, detail string) {
	w.Header().Set("Retry-After", strconv.Itoa(previewRetryAfterSeconds))
	s.writeCodeProblem(w, r, http.StatusTooManyRequests, "capture-preview-throttled",
		"RATE_LIMITED", "Too Many Requests", detail)
}

// captureProblem maps a capture/stack sentinel to its documented problem code
// (ADR-0078 §2): the operator sees the remedy (attach the dongle, name a
// device the enumeration has), not a generic failure.
func (s *Server) captureProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, stack.ErrDeviceAbsent):
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-device-unavailable",
			"DEVICE_UNAVAILABLE", "Service Unavailable",
			"no usable capture device is attached: connect the UVC HDMI capture dongle and retry (docs/spec/10)")
	case errors.Is(err, stack.ErrDeviceUnknown):
		s.writeProblem(w, r, http.StatusBadRequest, "capture-device-unknown",
			"Bad Request", "the requested device is not in the current enumeration: see GET /api/v1/capture/devices for the ids the session may name")
	case errors.Is(err, stack.ErrModeUnavailable):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "capture-mode-unavailable",
			"Service Unavailable", "no offered capture mode fits the selection policy: the device offers no format the card's link budget admits")
	case errors.Is(err, stack.ErrModeNotOffered):
		s.writeProblem(w, r, http.StatusBadRequest, "capture-mode-not-offered",
			"Bad Request", "the pinned mode is not one the device offers: see the device's offered modes in GET /api/v1/capture/devices, or omit the mode and let the policy decide")
	case errors.Is(err, stack.ErrNoSelectionStore):
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-selection-unavailable",
			"CAPTURE_SELECTION_UNAVAILABLE", "Service Unavailable",
			"the capture selection cannot be persisted: the settings store is not wired into this process")
	case errors.Is(err, stack.ErrSourceUnwired):
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-source-unavailable",
			"CAPTURE_SOURCE_UNAVAILABLE", "Service Unavailable",
			"no capture source builder is wired into this process")
	default:
		// A source fault (FFmpeg could not open the node, no frame within
		// the grab timeout): the device is attached but not delivering. The
		// detail carries the source's own error so the operator action is
		// findable (re-seat the dongle, try another link/port).
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-source-failed",
			"CAPTURE_SOURCE_FAILED", "Service Unavailable", "the capture device is not delivering frames: "+err.Error())
	}
}

// captureBodyDecoder is the request-body decoder with the same 1 MiB bound
// the other JSON routes use: a selection or probe body that needs more is an
// attack on the appliance's memory before it is a request.
func captureBodyDecoder(w http.ResponseWriter, r *http.Request) *json.Decoder {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
}
