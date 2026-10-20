package api

// The Bluetooth output surface (issues #2/#3, ADR-0087): the adapter state,
// the time-bounded scan, the audio-capable device inventory and the pairing
// lifecycle — pair (with trust), connect, disconnect, forget and the re-pair
// repair for the "connected but no A2DP sink" state. The selection itself is
// the existing audio output selection (ADR-0074, PUT /api/v1/settings/audio):
// a paired, connected headset appears as the PipeWire node
// bluez_output.<MAC>.1, which is exactly what the page persists as the
// narration output.
//
// The product rule (AGENTS.md rule 6): nothing here shells out to
// bluetoothctl — every operation is a BlueZ D-Bus call made by internal/bt
// through the system bus. The Controller seam is what the routes hold: the
// real client on the appliance, the fake (GAME_NARRATOR_BT_FAKE) in the API
// tests and the Playwright walk — the same labelled-synthetic pattern as
// GAME_NARRATOR_AUDIO_FAKE_DEVICES, so a fake answer never reads as hardware
// (the synthetic flag rides in every answer).
//
// The HTTP concerns the controller has no business knowing live here: the
// rate limit (the operations are physical radio work — the budget is the
// inquiry channel, not the caller), the audit rows (every mutation is
// recorded with its actor and address), and the error mapping (a BlueZ
// failure is a gateway failure: 502 with the daemon's own reason, so the
// page can say "this headset needs a PIN" instead of "it failed").

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/dbirks/game-narrator/internal/audio"
	"github.com/dbirks/game-narrator/internal/bt"
)

// The scan and operation bounds. A scan longer than btScanMax holds the
// inquiry channel for no product reason (the pairing-mode headset answers in
// seconds), and the operations carry their own timeout so a hung BlueZ call
// cannot hold a request open past the operator's patience.
const (
	btScanDefault = 8 * time.Second
	btScanMax     = 30 * time.Second
	btScanMin     = 1 * time.Second

	btPairBound    = 60 * time.Second // a pairing exchange with the radio, end to end
	btConnectBound = 30 * time.Second
	btForgetBound  = 15 * time.Second
	btRepairBound  = 90 * time.Second // the repair owns a classic-transport rescan inside

	// The rate limit (docs/spec/08 "rate-limit … preview streaming" extended
	// to the radio): every Bluetooth mutation from one client address draws
	// on the same inquiry channel and pairing machinery, so they share one
	// bucket. The capacity is wide by design — the UI's whole scan→pair→
	// connect flow costs a handful of tokens — and the window long enough
	// that a legitimate retry is never blocked; what it stops is a loop.
	btLimiterCapacity = 20
	btLimiterWindow   = 5 * time.Minute
)

// btStateView is GET /api/v1/audio/bluetooth/state.
type btStateView struct {
	// Available is false when the controller is unwired (this build has no
	// Bluetooth at all) — the page then hides the section rather than
	// showing an error.
	Available bool `json:"available"`
	// Synthetic marks the labelled-synthetic controller (ADR-0074's rule:
	// the flag travels in every answer).
	Synthetic bool `json:"synthetic"`
	// Powered and Discovering are the adapter's own properties (reported,
	// never inferred); a powered-off adapter is the one state the page must
	// say out loud, with the exact remediation.
	Powered     bool   `json:"powered"`
	Discovering bool   `json:"discovering"`
	Alias       string `json:"alias,omitempty"`
	Note        string `json:"note,omitempty"`
}

// btDeviceView is one inventory row: the controller's device plus the two
// page-facing derivations — whether this device's A2DP sink is the selected
// output, and the repair hint (issue #3's "connected but no audio sink").
type btDeviceView struct {
	bt.Device
	// Selected is true when the effective audio selection resolves to this
	// device's A2DP sink (bluez_output.<MAC>.1).
	Selected bool `json:"selected"`
	// NeedsRepair is the issue-#3 state: BlueZ reports the device connected
	// (a GATT connection counts) yet no A2DP unit exists — the LE-only
	// regression the repair exists for. The page offers the re-pair button.
	NeedsRepair bool `json:"needs_repair,omitempty"`
}

// btDevicesView is GET /api/v1/audio/bluetooth/devices and the scan's answer.
type btDevicesView struct {
	Synthetic bool           `json:"synthetic"`
	Devices   []btDeviceView `json:"devices"`
	Note      string         `json:"note,omitempty"`
}

// btScanRequest is POST /api/v1/audio/bluetooth/scan's body: the duration is
// the operator's bound on how long to hold the scan open (absent, the 8 s
// default; capped at btScanMax).
type btScanRequest struct {
	DurationMS *int `json:"duration_ms,omitempty"`
}

// btActionResponse is the answer of every device mutation.
type btActionResponse struct {
	// Address echoes the normalised (upper-case) address the operation ran
	// on — the row the page should refetch.
	Address string `json:"address"`
	// Note carries the product-level result ("pairing succeeded; the device
	// is trusted", "the device was removed from the pairing list").
	Note string `json:"note,omitempty"`
}

// btControllerIs reports whether the wiring is a labelled-synthetic fake,
// through a type assertion so the real Client (which has no Synthetic
// method) answers false.
func btIsSynthetic(c bt.Controller) bool {
	type syntheticMarker interface{ Synthetic() bool }
	m, ok := c.(syntheticMarker)
	return ok && m.Synthetic()
}

// btSelectedSink resolves the effective audio selection's device (the
// ADR-0074 rule: env > store > default). It is the single place the
// "selected" badge is computed, so the inventory and the devices page cannot
// disagree.
func (s *Server) btSelectedSink() string {
	sel, err := audio.Resolve(audioLookupEnv, s.audioStoredSelection())
	if err != nil {
		return ""
	}
	return sel.Device
}

// btDeviceView renders one controller device with the page's derivations.
func (s *Server) btDeviceView(d bt.Device) btDeviceView {
	sink := bt.SinkID(d.Address)
	return btDeviceView{
		Device:      d,
		Selected:    s.btSelectedSink() == sink,
		NeedsRepair: d.Connected && d.Paired && !d.A2DPSink,
	}
}

// btDevicesView renders the inventory.
func (s *Server) btDevicesView(devs []bt.Device, note string) btDevicesView {
	rows := make([]btDeviceView, 0, len(devs))
	for _, d := range devs {
		rows = append(rows, s.btDeviceView(d))
	}
	return btDevicesView{
		Synthetic: btIsSynthetic(s.btController),
		Devices:   rows,
		Note:      note,
	}
}

// btRequire answers the unwired-controller case: 503, the honest-degradation
// rule every unwired subsystem follows. An empty section is not the same
// fact as no devices, and the page hides itself on available=false.
func (s *Server) btRequired(w http.ResponseWriter, r *http.Request) bool {
	if s.btController == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "bluetooth-unwired",
			"BLUETOOTH_UNAVAILABLE", "Service Unavailable",
			"the Bluetooth controller is not wired into this build (no system bus or no BlueZ daemon)")
		return false
	}
	return true
}

// btRateLimited answers the shared mutation bucket. The key is the transport
// peer address — the one value docs/spec/14's rate-limit rule trusts,
// because X-Forwarded-* is caller-supplied.
func (s *Server) btRateLimited(w http.ResponseWriter, r *http.Request) bool {
	l := s.btLimiter
	if l == nil {
		return true
	}
	ok, retryAfter := l.Allow(s.clientAddr(r))
	if ok {
		return true
	}
	secs := int64(retryAfter.Seconds())
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	w.Header().Set("Cache-Control", "no-store")
	s.auditEvent(r.Context(), "bluetooth.rate_limited", IdentityFromContext(r.Context()), s.clientAddr(r), map[string]any{
		"path": r.URL.Path, "retry_after_s": secs,
	})
	s.writeCodeProblem(w, r, http.StatusTooManyRequests, "bluetooth-rate-limited",
		"RATE_LIMITED", "Too Many Requests",
		"too many Bluetooth operations from this address; the radio is a shared budget — retry in a moment")
	return false
}

// btMAC validates the {mac} path segment (the API's single spelling rule):
// an address that does not pass is a 400, not a D-Bus call with a typo.
func (s *Server) btMAC(w http.ResponseWriter, r *http.Request) (string, bool) {
	mac, err := bt.NormalizeAddress(r.PathValue("mac"))
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-mac",
			"Bad Request", err.Error())
		return "", false
	}
	return mac, true
}

// btError maps a controller error onto the documented problem codes. The
// 502 detail carries the daemon's own reason (BlueZ error names, not
// secrets) because the page's next action depends on it: "Authentication
// Failed" is the headset-needs-a-PIN answer, "Failed: Not authorized" is the
// trust answer, "NotAvailable" is the adapter-off answer.
func (s *Server) btError(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, bt.ErrNotAvailable):
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "bluetooth-unavailable",
			"BLUETOOTH_UNAVAILABLE", "Service Unavailable",
			"the Bluetooth daemon is not reachable: "+err.Error())
	case errors.Is(err, bt.ErrDeviceUnknown):
		s.writeCodeProblem(w, r, http.StatusNotFound, "bluetooth-device-unknown",
			"DEVICE_NOT_FOUND", "Not Found",
			"bluez has no record of that address: "+err.Error())
	default:
		s.log.Warn("bluetooth: operation failed",
			"request_id", RequestIDFromContext(r.Context()), "op", op, "error", err)
		s.writeCodeProblem(w, r, http.StatusBadGateway, "bluetooth-operation-failed",
			"OPERATION_FAILED", "Bad Gateway",
			"the Bluetooth daemon rejected the operation: "+err.Error())
	}
}

// handleBtState answers GET /api/v1/audio/bluetooth/state.
func (s *Server) handleBtState(w http.ResponseWriter, r *http.Request) {
	if !s.btRequired(w, r) {
		return
	}
	st, err := s.btController.State(r.Context())
	if err != nil {
		s.btError(w, r, "state", err)
		return
	}
	view := btStateView{
		Available:   true,
		Synthetic:   btIsSynthetic(s.btController),
		Powered:     st.Powered,
		Discovering: st.Discovering,
		Alias:       st.Alias,
		Note:        st.Note,
	}
	if !st.Powered {
		view.Note = "the Bluetooth adapter is powered off: enable it in the system's Bluetooth settings (the appliance has no keyboard), then retry"
	}
	writeJSON(w, http.StatusOK, view)
}

// handleBtDevices answers GET /api/v1/audio/bluetooth/devices: every device
// BlueZ has a record of (paired or merely discovered), with the pairing
// state, the A2DP evidence and the selection badge.
func (s *Server) handleBtDevices(w http.ResponseWriter, r *http.Request) {
	if !s.btRequired(w, r) {
		return
	}
	devs, err := s.btController.Devices(r.Context())
	if err != nil {
		s.btError(w, r, "devices", err)
		return
	}
	writeJSON(w, http.StatusOK, s.btDevicesView(devs, ""))
}

// handleBtScan answers POST /api/v1/audio/bluetooth/scan: the time-bounded
// discovery (the bound is the request's, capped), and the inventory it left
// behind. A scan that finds nothing is a 200 with the pairing-mode hint in
// note — an empty result is an answer, not a failure.
func (s *Server) handleBtScan(w http.ResponseWriter, r *http.Request) {
	if !s.btRequired(w, r) {
		return
	}
	if !s.btRateLimited(w, r) {
		return
	}
	var req btScanRequest
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := dec.Decode(&req); err != nil && err.Error() != "EOF" {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-scan-request",
				"Bad Request", "the scan body is not valid JSON")
			return
		}
	}
	dur := btScanDefault
	if req.DurationMS != nil {
		dur = time.Duration(*req.DurationMS) * time.Millisecond
		if dur < btScanMin {
			dur = btScanMin
		}
		if dur > btScanMax {
			dur = btScanMax
		}
	}

	ctx := r.Context()
	start := time.Now()
	err := s.btController.Scan(ctx, dur)
	elapsed := time.Since(start)
	// Even a failed scan (the adapter off, the context gone) leaves the
	// inventory to report — the page reads the state, not only the error.
	var view btDevicesView
	if devs, derr := s.btController.Devices(ctx); derr == nil {
		view = s.btDevicesView(devs, "")
	} else {
		view = s.btDevicesView(nil, "")
	}
	if err != nil {
		// The audit row is the operator record of the attempt either way.
		s.auditEvent(ctx, "bluetooth.scan", IdentityFromContext(ctx), s.clientAddr(r), map[string]any{
			"duration_ms": int64(dur.Milliseconds()), "ok": false, "error": err.Error(),
		})
		view.Note = "the scan could not run: " + err.Error()
		s.btError(w, r, "scan", err)
		return
	}
	s.auditEvent(ctx, "bluetooth.scan", IdentityFromContext(ctx), s.clientAddr(r), map[string]any{
		"duration_ms": int64(dur.Milliseconds()), "ok": true, "found": len(view.Devices),
	})
	if len(view.Devices) == 0 {
		view.Note = "the scan found nothing: put the headset in pairing mode (hold its Bluetooth button until the LED flashes) and scan again"
	}
	writeJSON(w, http.StatusOK, scanResponse{
		Synthetic: view.Synthetic,
		ElapsedMS: elapsed.Milliseconds(),
		Devices:   view.Devices,
		Note:      view.Note,
	})
}

// scanResponse is the scan's 200 body (the inventory plus the timing the
// page's progress state reports).
type scanResponse struct {
	Synthetic bool           `json:"synthetic"`
	ElapsedMS int64          `json:"elapsed_ms"`
	Devices   []btDeviceView `json:"devices"`
	Note      string         `json:"note,omitempty"`
}

// btDeviceAction builds one of the {mac} mutation handlers. The factory keeps
// the seven operations (pair, trust, connect, disconnect, forget, repair —
// the issue's list) one code path: same MAC validation, same rate limit,
// same audit row, same error mapping; only the controller call and the
// timeout bound differ.
func (s *Server) btDeviceAction(name string, bound time.Duration, note string,
	call func(bt.Controller, context.Context, string) error,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.btRequired(w, r) {
			return
		}
		mac, ok := s.btMAC(w, r)
		if !ok {
			return
		}
		if !s.btRateLimited(w, r) {
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), bound)
		defer cancel()
		err := call(s.btController, ctx, mac)
		id := IdentityFromContext(ctx)
		s.auditEvent(ctx, "bluetooth."+name, id, s.clientAddr(r), map[string]any{
			"address": mac, "ok": err == nil,
			"error": errString(err),
		})
		if err != nil {
			s.btError(w, r, name+" "+mac, err)
			return
		}
		writeJSON(w, http.StatusOK, btActionResponse{Address: mac, Note: note})
	}
}

// errString renders an error for the audit row ("" for none) — the row is
// the operator's evidence of what the daemon answered, so the daemon's own
// words go in it.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
