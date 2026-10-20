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

	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/stack"
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

// handleCapturePreview answers GET /api/v1/capture/preview?device=: one JPEG
// snapshot through a throwaway source (build, start, one frame, encode at
// quality 60 capped to 1280 px wide, close — ADR-0078 §3). The route is
// authenticated (the scope grant) and throttled globally: one snapshot in
// flight and a 500 ms minimum interval, 429 with Retry-After when exceeded.
// A preview is also refused 503 capture-busy while a running session holds
// the same device — the UVC bus is single-consumer, and the answer names the
// holding session instead of a raw FFmpeg "device or resource busy".
func (s *Server) handleCapturePreview(w http.ResponseWriter, r *http.Request) {
	if s.capture == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process")
		return
	}

	// The global throttle (ADR-0078): one in flight, 500 ms apart. The
	// in-flight counter is the single-snapshot bound (a second request that
	// arrives mid-grab is told to retry, not queued), and the interval is the
	// decode-budget spacing. Both fail closed toward the 429.
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

	// The per-device busy check (ADR-0078): a running session on the same
	// device holds the UVC bus, and the preview would race it for the node.
	// The id the preview would open is the stack's resolution of the request
	// (the requested id, else the stored selection, else the first usable
	// device), so the comparison is on what will actually be opened.
	resID, err := s.capture.ResolveID(r.Context(), device)
	if err != nil {
		s.captureProblem(w, r, err)
		return
	}
	if s.sessions != nil {
		if heldBy, sessionID, active := s.sessions.ActiveCaptureDevice(); active && heldBy == resID {
			s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-busy",
				"CAPTURE_BUSY", "Service Unavailable",
				fmt.Sprintf("the capture device %s is held by the running session %s; stop the session before previewing it", resID, sessionID))
			return
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
