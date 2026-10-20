// Package stack is the capture device's product layer (ADR-0078): one pure
// selection and operation layer shared by the session runtime (which builds
// the v4l2 source a session runs on) and the API (which lists the devices,
// persists the selection, takes the throttled preview snapshot and runs the
// capture probe). docs/spec/10 "never assume /dev/video0" is enforced here:
// the client may name a device only by the stable id this package reports,
// and every other name is refused — the enumeration is the allowlist, the
// same discipline the replay roots apply to directories.
//
// The hardware seams are closures on Stack, so the whole surface runs on a
// labelled-synthetic fake (EnvFake, the ADR-0074 audio-fake pattern): a fixed
// device, a fixed link and a real FFmpeg testsrc2 pipe standing in for the
// dongle — no v4l2 node, no loopback, real frames through the production
// consumer, every answer flagged synthetic.
package stack

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"log/slog"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/ffmpegpipe"
	"github.com/dbirks/game-narrator/internal/capture/mode"
	"github.com/dbirks/game-narrator/internal/capture/usb"
)

// ErrDeviceAbsent means the enumeration has no usable capture device (the
// dongle is not attached, or nothing in the list looks like one). The API
// answers it 503 capture-device-unavailable (docs/spec/08 DEVICE_UNAVAILABLE);
// the runtime answers it as a session start failure with the same remediation.
var ErrDeviceAbsent = errors.New("capture: no usable capture device is attached")

// ErrDeviceUnknown means a requested device id is not in the current
// enumeration: the stable-id allowlist refused it (a raw /dev/videoN path or
// a stale id is a client error, not a device fault).
var ErrDeviceUnknown = errors.New("capture: the requested device is not in the current enumeration")

// ErrModeUnavailable means no offered mode fits the policy (or the stored
// mode fit nothing): the ADR-0076 selection's refusal, surfaced with the
// doctor's remediation instead of a flapping session.
var ErrModeUnavailable = errors.New("capture: no offered mode fits the selection policy")

// ErrModeNotOffered means a pinned mode the device does not offer (the
// selection route's 400): the operator asked for a mode the card's
// enumeration does not have, so the answer names the offered set rather
// than saving a row the next resolution would re-derive anyway.
var ErrModeNotOffered = errors.New("capture: the pinned mode is not an offered mode of the device")

// ErrSourceUnwired means the Stack has no source builder: preview/probe
// cannot open anything. A production stack is always wired (ffmpegpipe), so
// this is a wiring-bug answer, not a device fault.
var ErrSourceUnwired = errors.New("capture: no source builder is wired into this process")

// ErrNoSelectionStore means the selection seam is absent: a device id may
// still be resolved (it outranks the store), but persisting one is refused
// rather than silently dropped.
var ErrNoSelectionStore = errors.New("capture: no selection store is wired into this process")

// Selection is the persisted capture selection (one app_settings row, the
// store's spelling; ADR-0078): the device the operator chose and, optionally,
// the mode they pinned. An empty Mode means "the policy decides for this
// device" — a pin that the card no longer offers is re-derived, never a
// session failure.
type Selection struct {
	DeviceID string `json:"device_id"`
	// Mode is the operator's pinned mode; the zero value means policy.
	Mode *mode.Mode `json:"mode,omitempty"`
}

// State words for the device list (docs/spec/13: the operator action lives in
// the state, not in a log).
const (
	StateReady          = "ready"
	StateNoFormats      = "no-formats"
	StateNoMode         = "no-mode"
	StateEnumerationErr = "enumeration-error"
	StateNotCapture     = "not-capture"
)

// LinkView is the device's USB link (internal/capture/usb) in its reportable
// form: the negotiated speed, its class, and the ADR-0076 downgrade flag
// (a SuperSpeed-class port that negotiated USB 2 — the "re-seat" fault).
type LinkView struct {
	SpeedMBps  int    `json:"speed_mbps"`
	Class      string `json:"class"`
	Downgraded bool   `json:"downgraded,omitempty"`
}

// ChosenView is the mode the stack will ask for on this device: the operator's
// pinned mode when it is still offered (Source "saved"), otherwise the
// ADR-0076 policy's pick (Source "policy") with the policy's reason and
// rejected-candidate count, so "why 720p and not 1080p" stays answerable.
type ChosenView struct {
	W        int     `json:"w"`
	H        int     `json:"h"`
	FPS      float64 `json:"fps"`
	PixFmt   string  `json:"pix_fmt"`
	Source   string  `json:"source"`
	Reason   string  `json:"reason,omitempty"`
	Rejected int     `json:"rejected"`
}

// DeviceView is one enumerated node in the device list.
type DeviceView struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Path   string      `json:"path"`
	State  string      `json:"state"`
	Link   *LinkView   `json:"link,omitempty"`
	Chosen *ChosenView `json:"chosen,omitempty"`
	Detail string      `json:"detail,omitempty"`
}

// DevicesView is GET /api/v1/capture/devices: the enumeration, the current
// selection, and the synthetic flag (true on the labelled test seam).
type DevicesView struct {
	Synthetic    bool         `json:"synthetic"`
	Note         string       `json:"note,omitempty"`
	SelectedID   string       `json:"selected_id,omitempty"`
	SelectedMode *ChosenView  `json:"selected_mode,omitempty"`
	Devices      []DeviceView `json:"devices"`
}

// Box is a pixel region of the current frame (the probe's OCR region; docs/
// spec/10 "user-selected region"). Negative or zero fields mean "not given".
type Box struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

// Valid reports whether the box is a non-empty rectangle.
func (b Box) Valid() bool { return b.W > 0 && b.H > 0 && b.X >= 0 && b.Y >= 0 }

// SnapshotMeta describes the one frame the preview captured.
type SnapshotMeta struct {
	Device  string `json:"device"`
	Mode    string `json:"mode"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FreshMS int64  `json:"freshness_ms"`
}

// ProbeOCR is the probe's OCR block: the text read from the region, the box
// it was read from, and the engine that read it (or "unavailable" with the
// operator action when the engine is not installed — the degradable half).
type ProbeOCR struct {
	Text   string `json:"text"`
	Region Box    `json:"region"`
	Engine string `json:"engine"`
	Note   string `json:"note,omitempty"`
}

// ProbeResult is POST /api/v1/capture/probe: the docs/spec/10 capture test —
// grab N frames from the resolved device, report freshness and drop stats,
// and OCR the requested region of the last frame.
type ProbeResult struct {
	Device      string    `json:"device"`
	Mode        string    `json:"mode"`
	ModeSource  string    `json:"mode_source"`
	Requested   int       `json:"requested"`
	Grabbed     int       `json:"grabbed"`
	Dropped     int64     `json:"dropped"`
	FPS         float64   `json:"fps"`
	FreshnessMS int64     `json:"freshness_ms"`
	State       string    `json:"state"`
	LastError   string    `json:"last_error,omitempty"`
	OCR         *ProbeOCR `json:"ocr,omitempty"`
}

// SelectionStore is the persistence seam (internal/store in production):
// Get returns ok=false when the user never chose a selection; Set is the
// upsert. A Stack with a nil store resolves device ids (they outrank the
// store) but answers ErrNoSelectionStore on a save.
type SelectionStore interface {
	GetSelection(ctx context.Context) (Selection, bool, error)
	SetSelection(ctx context.Context, sel Selection) error
}

// Stack is the wired capture layer. The zero value is not useful; use
// New with the seams (production wiring, or the labelled fake).
type Stack struct {
	// List enumerates the capture nodes (devices.List in production; the
	// fake seam's fixed device in tests). nil runs the real enumerator.
	List func(ctx context.Context) ([]devices.Device, error)
	// Link reports a node's USB link (usb.LinkSpeedFor in production).
	// nil reads the real sysfs.
	Link func(ctx context.Context, node string) (usb.Info, error)
	// Store persists the operator's selection. nil keeps resolution working
	// (an explicit device id outranks the store) and saves refused.
	Store SelectionStore
	// Policy is the ADR-0076 selection policy; the zero value is the
	// documented preflight policy (GameCube canvas, 10 Hz, the measured
	// decode budget).
	Policy mode.Policy
	// Source builds the live source for a resolved (node path, mode).
	// Production is the ffmpegpipe.V4L2 constructor; the fake seam is a
	// testsrc2 pipe; tests inject a push-based source. nil refuses
	// preview/probe with ErrSourceUnwired.
	Source func(ctx context.Context, path string, m mode.Mode) (capture.Source, error)
	// Synthetic labels the whole stack as the test seam (ADR-0078): every
	// answer carries it, and the UI says the list is not the hardware.
	Synthetic bool
	// Note is the one-line provenance a synthetic list reports.
	Note string
	log  *slog.Logger
}

// New builds a Stack with the real hardware seams where the options left
// them nil: the v4l2-ctl enumeration, the sysfs link and the ffmpegpipe
// source. The store seam must be set by the caller (it needs the store).
func New(policy mode.Policy, store SelectionStore, log *slog.Logger) *Stack {
	return &Stack{
		List: devices.List,
		Link: func(ctx context.Context, node string) (usb.Info, error) {
			return usb.LinkSpeedFor(node, usb.DefaultSysfsRoot)
		},
		Store:  store,
		Policy: policy,
		Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) {
			return v4l2Source(path, m)
		},
		log: log,
	}
}

// v4l2Source is the production source builder: the ffmpegpipe.V4L2
// constructor (ADR-0076's low-latency defaults), which opens the node the
// enumeration reported — never a client-named path.
func v4l2Source(path string, m mode.Mode) (capture.Source, error) {
	return ffmpegpipe.V4L2(path, m.W, m.H, m.FPS, m.PixFmt)
}

// DefaultOptions is the process-level stack (ADR-0078 §4): the real hardware
// seams unless the labelled-synthetic test seam is requested. The env name is
// the ADR-0074 audio-fake pattern applied to capture: a fake dongle is a test
// property, never a silent default, and the fake's answers are all flagged
// synthetic.
const (
	// EnvFake names the environment variable that switches the stack to the
	// labelled-synthetic fake device (1 or true; any other value is ignored,
	// which is the fail-toward-hardware direction).
	EnvFake = "GAME_NARRATOR_CAPTURE_FAKE"
)

// FakeID and FakePath are the synthetic device's identity (ADR-0078): a
// stable id that cannot collide with a udev by-id name, and a node path that
// cannot collide with a real /dev/videoN.
const (
	FakeID   = "usb-GameNarrator_SyntheticHDMI_0001"
	FakePath = "/dev/video-fake0"
)

// DefaultOptions returns the process stack for its environment (ADR-0078 §4).
func DefaultOptions(lookupEnv func(string) (string, bool), store SelectionStore, log *slog.Logger) *Stack {
	if lookupEnv == nil {
		lookupEnv = func(string) (string, bool) { return "", false }
	}
	if v, ok := lookupEnv(EnvFake); ok && (v == "1" || strings.EqualFold(v, "true")) {
		st := fakeStack()
		st.Store = store
		st.log = log
		return st
	}
	return New(mode.Policy{}, store, log)
}

// fakeStack is the labelled-synthetic seam: one fixed device with the
// documented MS2130-class format set (synthetic — the ADR-0076 fixtures'
// shape, not the V316B's real enumeration), a fixed HighSpeed link, and a
// real FFmpeg testsrc2 pipe as the source (loopback-free: no X11, no v4l2
// node), with the source ID prefixed v4l2: so the no-signal wrapper and every
// v4l2 code path runs on the fake exactly as on the card.
func fakeStack() *Stack {
	fakeFormats := []devices.Format{
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
	fakeDevice := devices.Device{
		Path:     FakePath,
		Name:     "Synthetic HDMI Capture (test seam)",
		BusInfo:  "usb-0000:00:14.0-fake1",
		StableID: FakeID,
		ByPath:   "usb-0000:00:14.0-fake1-video:00",
		Formats:  fakeFormats,
		Kind:     "Capture devices",
	}
	return &Stack{
		List: func(ctx context.Context) ([]devices.Device, error) {
			return []devices.Device{fakeDevice}, nil
		},
		Link: func(ctx context.Context, node string) (usb.Info, error) {
			return usb.Info{
				DevicePath:       "fake",
				VendorID:         "1234",
				ProductID:        "5678",
				SpeedMBps:        usb.SpeedHighSpeed,
				MaxPortSpeedMBps: usb.SpeedHighSpeed,
			}, nil
		},
		Store: nil,
		Source: func(ctx context.Context, path string, m mode.Mode) (capture.Source, error) {
			if path != FakePath {
				return nil, fmt.Errorf("capture: the fake seam only feeds %s (got %s)", FakePath, path)
			}
			return testsrcSource(m), nil
		},
		Synthetic: true,
		Note:      "device list from " + EnvFake + " (synthetic; no hardware was enumerated)",
	}
}

// ── enumeration and the device list ─────────────────────────────────────────

// Enumerate answers the device list: every node with its identity, USB link,
// state and the mode the stack would ask for on it. A lister error is
// returned (503 territory); an empty list is NOT an error — the appliance
// with the dongle unplugged is a real configuration the UI shows as "input
// offline" (docs/spec/13).
func (s *Stack) Enumerate(ctx context.Context) (DevicesView, error) {
	var list []devices.Device
	var err error
	if s.List != nil {
		list, err = s.List(ctx)
	} else {
		list, err = devices.List(ctx)
	}
	if err != nil {
		return DevicesView{}, err
	}
	sel, selOK, selErr := s.storedSelection(ctx)
	view := DevicesView{
		Synthetic: s.Synthetic,
		Note:      s.Note,
		Devices:   make([]DeviceView, 0, len(list)),
	}
	for _, d := range list {
		v := DeviceView{ID: s.deviceID(d), Name: d.Name, Path: d.Path}
		switch {
		case !strings.Contains(d.Kind, "Capture"):
			v.State = StateNotCapture
		case d.FormatsError != "":
			v.State = StateEnumerationErr
			v.Detail = d.FormatsError
		case len(d.Formats) == 0:
			v.State = StateNoFormats
		default:
			v.State = StateReady
			if info, lerr := s.linkInfo(ctx, d.Path); lerr == nil {
				lv := LinkView{SpeedMBps: info.SpeedMBps, Class: info.Class(), Downgraded: info.Downgraded()}
				v.Link = &lv
			}
			// An unreadable link is a degraded report, not a list failure: the
			// mode policy degrades conservatively (LinkUnknown), and the
			// doctor's row carries the same remedy.
			useSaved := selOK && selErr == nil && sel.DeviceID == v.ID
			chosen, cerr := s.chooseMode(d, sel, useSaved)
			if cerr == nil {
				c := ChosenView{
					W: chosen.Mode.W, H: chosen.Mode.H, FPS: chosen.Mode.FPS,
					PixFmt: chosen.Mode.PixFmt, Source: chosen.ModeSource,
					Reason: chosen.Reason, Rejected: chosen.Rejected,
				}
				v.Chosen = &c
			} else if !errors.Is(cerr, ErrModeUnavailable) {
				// A link/enumeration fault mid-selection: keep the state
				// ready and say so, rather than dropping the device.
				v.Detail = cerr.Error()
			} else if !useSaved {
				// The policy found no fit for this device: it is enumerated
				// but not selectable — the detail carries the policy's
				// refusal (the doctor's remedy).
				v.State = StateNoMode
				v.Detail = "no offered mode fits the policy: " + cerr.Error()
			}
		}
		view.Devices = append(view.Devices, v)
	}
	if selOK && selErr == nil && sel.DeviceID != "" {
		view.SelectedID = sel.DeviceID
		for i := range view.Devices {
			if view.Devices[i].ID == sel.DeviceID {
				view.SelectedMode = view.Devices[i].Chosen
				break
			}
		}
	}
	return view, nil
}

// deviceID is the device's stable identity for the API (the deviceIDOf
// spelling): the same order Resolve accepts, so the id the UI selected is
// the id the session runs with.
func (s *Stack) deviceID(d devices.Device) string { return deviceIDOf(d) }

// linkInfo reads the node's USB link through the seam (nil seam = real sysfs).
func (s *Stack) linkInfo(ctx context.Context, node string) (usb.Info, error) {
	if s.Link != nil {
		return s.Link(ctx, node)
	}
	return usb.LinkSpeedFor(node, usb.DefaultSysfsRoot)
}

// storedSelection reads the persisted selection; a store error degrades to
// "no selection" (the device list must not 503 because the settings row is
// corrupt — the save route reports that fault on its own).
func (s *Stack) storedSelection(ctx context.Context) (Selection, bool, error) {
	if s.Store == nil {
		return Selection{}, false, nil
	}
	sel, ok, err := s.Store.GetSelection(ctx)
	return sel, ok, err
}

// chooseMode is the per-device mode half of Enumerate: the pinned mode for
// the pinned device (when it is still offered), the policy elsewhere.
func (s *Stack) chooseMode(d devices.Device, sel Selection, useSaved bool) (Resolved, error) {
	if useSaved && sel.Mode != nil {
		if m, ok := matchOffered(d, *sel.Mode); ok {
			return Resolved{Device: d, Mode: m, ModeSource: ModeSourceSaved}, nil
		}
		// The pinned mode is no longer offered: fall through to the policy
		// (the pin is re-derived, never a list failure) — and the saved
		// selection is left to Resolve to refresh (see Resolve's note).
	}
	return s.policyMode(d)
}

// ── selection persistence ───────────────────────────────────────────────────

// ModeSource words for the chosen mode's provenance (ADR-0078): the operator's
// pin or the policy's pick.
const (
	ModeSourceSaved  = "saved"
	ModeSourcePolicy = "policy"
)

// Save persists the operator's selection for one enumerated device (the PUT
// route): the device must be in the current enumeration and usable, and a
// pinned mode must be one the device offers — the validation the store
// cannot do (it does not see the enumeration), so the store cannot hold a
// selection the next Resolve would silently discard.
func (s *Stack) Save(ctx context.Context, id string, m *mode.Mode) (Selection, error) {
	if s.Store == nil {
		return Selection{}, ErrNoSelectionStore
	}
	list, err := s.enumerate(ctx)
	if err != nil {
		return Selection{}, err
	}
	var found *devices.Device
	for i := range list {
		if s.deviceID(list[i]) == id {
			found = &list[i]
			break
		}
	}
	if found == nil || !found.Usable() {
		return Selection{}, fmt.Errorf("%w: %q is not a usable capture device in the current enumeration", ErrDeviceUnknown, id)
	}
	sel := Selection{DeviceID: id}
	if m != nil {
		match, ok := matchOffered(*found, *m)
		if !ok {
			return Selection{}, fmt.Errorf("%w: %dx%d %s @ %g fps is not an offered mode of %s",
				ErrModeNotOffered, m.W, m.H, m.PixFmt, m.FPS, id)
		}
		sel.Mode = &match
	}
	if err := s.Store.SetSelection(ctx, sel); err != nil {
		return Selection{}, err
	}
	return sel, nil
}

// Load reports the persisted selection (the API's "selected" block).
func (s *Stack) Load(ctx context.Context) (Selection, bool, error) {
	return s.storedSelection(ctx)
}

// ── resolution (the session's half) ─────────────────────────────────────────

// Resolved is Resolve's answer: the device the session opens, the mode it
// asks for, and which half decided the mode (the operator's pin or the
// policy) with the policy's reason — the fields the session snapshot and the
// session.started event carry.
type Resolved struct {
	Device     devices.Device
	Mode       mode.Mode
	ModeSource string
	Reason     string
	Rejected   int
}

// NodePath is the node the source builder opens.
func (r Resolved) NodePath() string { return r.Device.Path }

// ID is the device's API identity in the device list's spelling (the by-id
// name when udev made one, the by-path name next, the node path last):
// the id the device list reports, a session snapshot carries and the
// preview's per-device busy check compares.
func (r Resolved) ID() string { return deviceIDOf(r.Device) }

// deviceIDOf is the identity spelling shared by the device list, the
// resolution and the snapshot: one order, one answer.
func deviceIDOf(d devices.Device) string {
	switch {
	case d.StableID != "":
		return d.StableID
	case d.ByPath != "":
		return d.ByPath
	default:
		return d.Path
	}
}

// Resolve is the one function the runtime calls at session start
// (ADR-0078 §1): the requested device id (the session's frame_source.dir)
// against the live enumeration, then the mode — the operator's pin when it
// still fits, the ADR-0076 policy otherwise. Empty requestedID falls to the
// persisted selection, then to the first usable device; the answer is what
// the session actually runs, so a fallback is visible in the snapshot.
func (s *Stack) Resolve(ctx context.Context, requestedID string) (Resolved, error) {
	list, err := s.enumerate(ctx)
	if err != nil {
		return Resolved{}, err
	}
	var usable []devices.Device
	for i := range list {
		if list[i].Usable() {
			usable = append(usable, list[i])
		}
	}
	if len(usable) == 0 {
		return Resolved{}, fmt.Errorf("%w (attach the UVC HDMI capture dongle, docs/spec/10, and re-run)", ErrDeviceAbsent)
	}
	requestedID = strings.TrimSpace(requestedID)
	var target *devices.Device
	if requestedID != "" {
		for i := range usable {
			d := &usable[i]
			if d.StableID == requestedID || d.ByPath == requestedID || d.Path == requestedID {
				target = d
				break
			}
		}
		if target == nil {
			return Resolved{}, fmt.Errorf("%w: %q does not match any enumerated device (the enumeration is the allowlist; see GET /api/v1/capture/devices)", ErrDeviceUnknown, requestedID)
		}
	} else {
		sel, selOK, selErr := s.storedSelection(ctx)
		if selOK && selErr == nil {
			for i := range usable {
				if s.deviceID(usable[i]) == sel.DeviceID {
					target = &usable[i]
					break
				}
			}
		}
		if target == nil {
			// No usable stored selection (or none at all): the first usable
			// device. A single-dongle appliance starts a session without a
			// second round-trip, and the snapshot names the device used.
			target = &usable[0]
		}
	}

	sel, selOK, _ := s.storedSelection(ctx)
	if selOK && sel.Mode != nil && sel.DeviceID == s.deviceID(*target) {
		if m, ok := matchOffered(*target, *sel.Mode); ok {
			return Resolved{Device: *target, Mode: m, ModeSource: ModeSourceSaved,
				Reason: "the operator's pinned mode (still offered by the device)"}, nil
		}
		// The pin no longer fits (replugged into a different card, a kernel
		// change): the policy re-derives it. The stale pin is refreshed on
		// the store best-effort so the next answer shows the policy's pick
		// rather than a pin the card cannot offer.
		s.refreshPin(ctx, sel, *target)
	}
	return s.policyModeFor(*target)
}

// refreshPin best-effort rewrites the store's pin for the device to the
// policy's pick when the saved pin stopped fitting. A store failure is
// logged, never returned: the session's mode decision is made, and the
// store is the display, not the decision.

// ResolveID answers "which device id would the session open" for the
// preview's per-device busy check (ADR-0078 §3): the same resolution as
// Resolve (the requested id, else the stored selection, else the first
// usable device) but only the identity, without a mode decision or a
// source. The preview refuses to open a device a running session holds,
// and the answer it compares is the device id in the device list's
// spelling.
func (s *Stack) ResolveID(ctx context.Context, requestedID string) (string, error) {
	res, err := s.Resolve(ctx, requestedID)
	if err != nil {
		return "", err
	}
	return res.ID(), nil
}
func (s *Stack) refreshPin(ctx context.Context, sel Selection, d devices.Device) {
	if s.Store == nil {
		return
	}
	r, err := s.policyModeFor(d)
	if err != nil {
		return
	}
	fresh := Selection{DeviceID: s.deviceID(d), Mode: &r.Mode}
	if err := s.Store.SetSelection(ctx, fresh); err != nil {
		if s.log != nil {
			s.log.Warn("refreshing the stale capture pin failed; the session runs the policy mode", "error", err)
		}
	}
}

// policyMode is Enumerate's per-device policy pick (the link feeds the
// policy, so a downgraded link is the conservative one).
func (s *Stack) policyMode(d devices.Device) (Resolved, error) {
	return s.policyModeFor(d)
}

func (s *Stack) policyModeFor(d devices.Device) (Resolved, error) {
	p := s.Policy
	info, err := s.linkInfo(context.Background(), d.Path)
	if err == nil {
		p.Link = mode.LinkFromSpeed(info.SpeedMBps)
	}
	// LinkUnknown on a fault: the policy degrades conservatively (assumed
	// HighSpeed), the same reading the doctor applies.
	sel, err := mode.Select(d.Formats, p)
	if err != nil {
		return Resolved{}, fmt.Errorf("%w: %s", ErrModeUnavailable, err)
	}
	return Resolved{Device: d, Mode: sel.Mode, ModeSource: ModeSourcePolicy, Reason: sel.Reason, Rejected: sel.Rejected}, nil
}

// enumerate is the List seam with the nil-seam default (the real v4l2-ctl).
func (s *Stack) enumerate(ctx context.Context) ([]devices.Device, error) {
	if s.List != nil {
		return s.List(ctx)
	}
	return devices.List(ctx)
}

// matchOffered reports whether m is exactly an offered mode of d (same size,
// a rate the size enumerates — or its stepwise interval, or an unenumerated
// size at the requested rate — and a supported pixel format), returning the
// normalized mode. The strictness is the point: a pin the card cannot offer
// is re-derived, never requested.
func matchOffered(d devices.Device, m mode.Mode) (mode.Mode, bool) {
	if m.W <= 0 || m.H <= 0 || m.FPS <= 0 {
		return mode.Mode{}, false
	}
	switch m.PixFmt {
	case "mjpeg", "yuyv422":
	default:
		return mode.Mode{}, false
	}
	for _, f := range d.Formats {
		if f.PixFmt != m.PixFmt {
			continue
		}
		for _, sz := range f.Sizes {
			if sz.W != m.W || sz.H != m.H {
				continue
			}
			if rateOffered(sz, m.FPS) {
				return mode.Mode{W: m.W, H: m.H, FPS: m.FPS, PixFmt: m.PixFmt}, true
			}
		}
	}
	return mode.Mode{}, false
}

// rateOffered is the pickRate half of mode.Select that matchOffered needs:
// a discrete rate in the list, a rate inside the stepwise interval, or — a
// size that enumerates no rates at all — any positive rate (the driver takes
// whatever is asked; mode.Select accepts such sizes the same way).
func rateOffered(sz devices.Size, fps float64) bool {
	if sz.Stepwise && sz.Max >= fps && (sz.Min <= 0 || sz.Min <= fps) && fps <= mode.MaxFPS {
		return true
	}
	if len(sz.FPS) == 0 {
		return fps > 0 && fps <= mode.MaxFPS
	}
	for _, r := range sz.FPS {
		if r == fps {
			return true
		}
	}
	return false
}

// ── operations (the API's half) ─────────────────────────────────────────────

const (
	// grabTimeout bounds one frame grab (the preview and the probe): a
	// device that cannot deliver a frame in 10 s reports the fault, it does
	// not hold the request.
	grabTimeout = 10 * time.Second
	// probeHardTimeout bounds the whole probe: frames stop at the requested
	// count or this deadline, whichever comes first.
	probeHardTimeout = 30 * time.Second
	// ocrTimeout bounds the probe's OCR call: a hung engine degrades the
	// OCR block, it does not hold the probe.
	ocrTimeout = 15 * time.Second
	// previewMaxWidth caps the preview JPEG's width (ADR-0110, UX-11): the
	// Health tile and the Play frame are small, and a 720p-class frame in the
	// preview is decode budget the OCR and speech pipelines would rather have
	// on the 4 GB box — the cap moved from 1280 to the 480p class the tile
	// renders.
	previewMaxWidth = 640
	// previewJPEGQuality is the preview's encoding quality: recognisably the
	// picture, not a lossless blob.
	previewJPEGQuality = 60
)

// testsrcSource is the fake seam's source: a real FFmpeg testsrc2 pipe at the
// resolved mode's geometry (loopback-free — no X11, no v4l2 node), with the
// v4l2: ID prefix so the no-signal wrapper and every v4l2 code path run on
// the fake exactly as on the card.
func testsrcSource(m mode.Mode) capture.Source {
	rate := m.FPS
	if rate <= 0 {
		rate = 10
	}
	s := ffmpegpipe.New(ffmpegpipe.Input{
		Format:   "lavfi",
		Location: fmt.Sprintf("testsrc2=size=%dx%d:rate=%d", m.W, m.H, int(rate)),
		Paced:    true,
	}, m.W, m.H, rate)
	s.IDName = "v4l2:" + FakePath
	return s
}

// Snapshot takes one frame from the resolved device and encodes it as JPEG
// (the GET /api/v1/capture/preview body): build the source, start it, read
// the first frame, encode, close. The device id is the API's (stable) id;
// absent means the stack's own resolution (stored selection, then first
// usable device) — the same fallback the session uses.
func (s *Stack) Snapshot(ctx context.Context, id string) ([]byte, SnapshotMeta, error) {
	fv, err := s.Frame(ctx, id)
	if err != nil {
		return nil, SnapshotMeta{}, err
	}
	data, w, h, err := EncodeJPEGDownscaled(fv.Frame.Image, previewMaxWidth, previewJPEGQuality)
	if err != nil {
		return nil, SnapshotMeta{}, err
	}
	meta := SnapshotMeta{
		Device: fv.Device,
		Mode:   fv.Mode.String(),
		Width:  w, Height: h,
		FreshMS: time.Since(fv.Stats.LastFrameAt).Round(time.Millisecond).Milliseconds(),
	}
	return data, meta, nil
}

// FrameView is the one-shot frame answer (ADR-0110): the grabbed frame
// (in memory, never stored), the resolved mode (the negotiated format and
// target rate), the source's self-report and the device's stable id.
type FrameView struct {
	// Frame is the grabbed frame: the API serves its pixels (the preview's
	// JPEG, the signal's active-picture judgment) and drops them at the end
	// of the request.
	Frame capture.Frame
	// Device is the stable id of the device the frame came from.
	Device string
	// Mode is the negotiated mode (the format and target fps the signal
	// reports).
	Mode mode.Mode
	// ModeSource names the mode's provenance ("saved" or "policy").
	ModeSource string
	// Stats is the source's self-report at the grab (the measured rate —
	// meaningful over the grab's frames, the last-frame time, the state,
	// the last failure text).
	Stats capture.Stats
}

// Frame grabs one frame from the resolved device and returns it in memory
// (the one-shot half of ADR-0110: the signal's no-session answer and the
// preview's one-shot path): build the source, start it, read the first
// frame, close. Unlike Snapshot it returns the frame itself — the caller
// downscales or analyses it — with the resolved mode and the source's
// stats. The device id is the API's (stable) id; absent means the stack's
// own resolution (stored selection, then first usable device).
func (s *Stack) Frame(ctx context.Context, id string) (FrameView, error) {
	res, err := s.Resolve(ctx, id)
	if err != nil {
		return FrameView{}, err
	}
	src, err := s.buildSource(ctx, res)
	if err != nil {
		return FrameView{}, err
	}
	defer func() { _ = src.Close() }()
	frames, err := src.Start(ctx)
	if err != nil {
		return FrameView{}, fmt.Errorf("capture: opening the one-shot frame source: %w", err)
	}
	grabCtx, cancel := context.WithTimeout(ctx, grabTimeout)
	defer cancel()
	select {
	case f, ok := <-frames:
		if !ok {
			st := src.Stats()
			return FrameView{}, fmt.Errorf("capture: the one-shot frame source ended before a frame (state %s: %s)", st.State, orEmpty(st.LastError))
		}
		return FrameView{
			Frame:      f,
			Device:     s.deviceID(res.Device),
			Mode:       res.Mode,
			ModeSource: res.ModeSource,
			Stats:      src.Stats(),
		}, nil
	case <-grabCtx.Done():
		st := src.Stats()
		return FrameView{}, fmt.Errorf("capture: no frame within %s (state %s: %s)", grabTimeout, st.State, orEmpty(st.LastError))
	}
}

// EncodeJPEGDownscaled encodes img as a JPEG capped to maxWidth wide
// (quality q, aspect preserved): the preview's shared encoder (ADR-0110 §2),
// used by the one-shot snapshot and by the session's in-memory frame, so
// both halves of the preview answer the same width bound with the same
// recipe. The image is downscaled in memory (nearest-neighbour, the
// existing downscale helper) — the preview is a recognisable snapshot, not
// a quality product, and a decode-heavy scaler on the A53 is CPU the OCR
// budget would rather have — and never written to disk. The answer is the
// bytes and the encoded size.
func EncodeJPEGDownscaled(img image.Image, maxWidth, q int) ([]byte, int, int, error) {
	if img == nil {
		return nil, 0, 0, errors.New("capture: no frame to encode")
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > maxWidth {
		img = downscale(img, maxWidth)
		h = int(float64(h) * float64(maxWidth) / float64(w))
		w = maxWidth
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		return nil, 0, 0, fmt.Errorf("capture: encoding the preview JPEG: %w", err)
	}
	return buf.Bytes(), w, h, nil
}

// Probe runs the docs/spec/10 capture test on the resolved device: grab up
// to `frames` frames (0 = the default 20; the cap is 200), collect the
// freshness/drop stats the spec asks for, and OCR the region (the default:
// the centre third) of the last frame. ocrFn is the caller's engine closure
// (the API passes the region editor's Tesseract factory); a nil closure
// skips the OCR block. The probe's source is a throwaway: the session's
// source, if any, is a different consumer, and the route's busy check keeps
// them off one device at once.
func (s *Stack) Probe(ctx context.Context, id string, frames int, region Box, ocrFn func(ctx context.Context, img image.Image) (string, error)) (ProbeResult, error) {
	if frames <= 0 {
		frames = 20
	}
	if frames > 200 {
		frames = 200
	}
	res, err := s.Resolve(ctx, id)
	if err != nil {
		return ProbeResult{}, err
	}
	src, err := s.buildSource(ctx, res)
	if err != nil {
		return ProbeResult{}, err
	}
	defer func() { _ = src.Close() }()
	framesCh, err := src.Start(ctx)
	if err != nil {
		return ProbeResult{}, fmt.Errorf("capture: opening the probe source: %w", err)
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeHardTimeout)
	defer cancel()

	var last capture.Frame
	grabbed := 0
	var dropped int64
	for grabbed < frames {
		select {
		case f, ok := <-framesCh:
			if !ok {
				// End of stream mid-probe (the source's child died): report
				// what came, with the source's own state and error — the
				// operator action, not a panic.
				return s.probeAnswer(res, frames, grabbed, dropped, src.Stats(), last, region, ocrFn), nil
			}
			last = f
			grabbed++
			// Frame.Dropped is the slot's overwrites between the previous
			// delivery and this one (the pump stamps it); summing across
			// the probe is the consumer-side drop count the spec's
			// "dropped-frame rate" asks for.
			dropped += f.Dropped
		case <-probeCtx.Done():
			// The deadline: the device is slower than the budget. The answer
			// carries the measured rate and freshness, which is the
			// diagnostic the probe exists to produce.
			return s.probeAnswer(res, frames, grabbed, dropped, src.Stats(), last, region, ocrFn), nil
		}
	}
	return s.probeAnswer(res, frames, grabbed, dropped, src.Stats(), last, region, ocrFn), nil
}

// probeAnswer assembles the probe's report from the source's own stats
// (the measured rate, the freshness, the state and the last error are the
// source's self-report; the drop count is the probe's own, above).
func (s *Stack) probeAnswer(res Resolved, requested, grabbed int, dropped int64, st capture.Stats, last capture.Frame, region Box, ocrFn func(ctx context.Context, img image.Image) (string, error)) ProbeResult {
	out := ProbeResult{
		Device:     s.deviceID(res.Device),
		Mode:       res.Mode.String(),
		ModeSource: res.ModeSource,
		Requested:  requested,
		Grabbed:    grabbed,
		Dropped:    dropped,
		FPS:        st.FPS,
		State:      string(st.State),
		LastError:  st.LastError,
	}
	if !st.LastFrameAt.IsZero() {
		out.FreshnessMS = time.Since(st.LastFrameAt).Round(time.Millisecond).Milliseconds()
	}
	if ocrFn != nil && grabbed > 0 {
		box := region
		if !box.Valid() {
			// The default region: the centre third — the dialogue-box class
			// the profiles' textboxes sit in (docs/spec/05), not the whole
			// frame (the spec's test OCR is a region read, not a full-frame
			// dump).
			w, h := last.Bounds()
			box = Box{X: w / 4, Y: h * 2 / 5, W: w / 2, H: h / 5}
		}
		crop := cropBox(last.Image, last.Image.Bounds(), box)
		ocrCtx, cancel := context.WithTimeout(context.Background(), ocrTimeout)
		text, err := ocrFn(ocrCtx, crop)
		cancel()
		if err != nil {
			out.OCR = &ProbeOCR{
				Region: box,
				Engine: "unavailable",
				Note:   "the OCR engine could not run (" + err.Error() + "); the frame stats above are still valid",
			}
		} else {
			out.OCR = &ProbeOCR{Text: text, Region: box, Engine: "tesseract"}
		}
	}
	return out
}

// downscale resizes img to the target width, preserving the aspect ratio
// (the preview's cap: the browser and the 4 GB box never hold a 1080p
// frame). A pixel-loop nearest-neighbour is deliberately simple: the preview
// is a recognisable snapshot, not a quality product, and a decode-heavy
// scaler on the A53 is CPU the OCR budget would rather have.
func downscale(img image.Image, targetW int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= targetW {
		return img
	}
	targetH := int(float64(h) * float64(targetW) / float64(w))
	if targetH < 1 {
		targetH = 1
	}
	out := image.NewRGBA(image.Rect(0, 0, targetW, targetH))
	for y := 0; y < targetH; y++ {
		sy := (y * h) / targetH
		for x := 0; x < targetW; x++ {
			xx := (x * w) / targetW
			out.Set(x, y, img.At(b.Min.X+xx, b.Min.Y+sy))
		}
	}
	return out
}

// cropBox cuts box out of img (clamped to the bounds); a degenerate crop is
// the whole frame rather than an error — the OCR block degrades, the probe
// does not.
func cropBox(img image.Image, b image.Rectangle, box Box) image.Image {
	x0 := box.X
	if x0 < b.Min.X {
		x0 = b.Min.X
	}
	y0 := box.Y
	if y0 < b.Min.Y {
		y0 = b.Min.Y
	}
	x1 := box.X + box.W
	if x1 > b.Max.X {
		x1 = b.Max.X
	}
	y1 := box.Y + box.H
	if y1 > b.Max.Y {
		y1 = b.Max.Y
	}
	if x1 <= x0 || y1 <= y0 {
		return img
	}
	r := image.Rect(x0, y0, x1, y1)
	out := image.NewRGBA(r)
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

// buildSource opens the resolved device's live source through the seam
// (nil seam = a wiring fault, answered, not faked).
func (s *Stack) buildSource(ctx context.Context, res Resolved) (capture.Source, error) {
	if s.Source == nil {
		return nil, ErrSourceUnwired
	}
	return s.Source(ctx, res.NodePath(), res.Mode)
}

// orEmpty is the error-text half of the grab answers.
func orEmpty(s string) string {
	if s == "" {
		return "no error reported"
	}
	return s
}
