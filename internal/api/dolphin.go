package api

// Developer Dolphin bridge API (docs/spec/11; dev-only, ADR-0007).
//
// Every route here is developer-role-only via the scope table's /api/v1/dev
// prefix grant, and additionally answers 501 unless the process was started
// with the explicit GAME_NARRATOR_DEV_DOLPHIN=1 opt-in (docs/spec/01
// invariant 7: production builds never set it, so a production box cannot
// launch an emulator or inject controller input through this API even if a
// developer-role caller tries). With the opt-in set but no bridge wired into
// this process, the routes answer 503 like every other unwired subsystem —
// the route existing is a fact; the emulator being reachable is not.
//
// The game image path is never accepted in a request body and never appears
// in a response: provenance is the image_sha256 the bridge recorded at
// launch (the path itself comes from the process environment only).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
)

// WithDolphinBridge wires the dev-only Dolphin bridge behind the
// /api/v1/dev/dolphin routes. A nil bridge answers 503 on those routes;
// without the option the routes answer the same 503 sentence (unwired).
func WithDolphinBridge(b dolphin.Bridge) Option {
	return func(s *Server) { s.dolphinBridge = b }
}

// dolphinLaunchBudget caps the setup phase of an async launch (Xvfb boot,
// emulator boot, render-window resolution): the first game frame arrives
// well inside this on the calibrated hardware (~20 s at llvmpipe speed). It
// bounds the Launch call only — the run itself is bound to the process
// lifetime (see handleDolphinLaunch), so a long session is not killed by a
// setup budget.
const dolphinLaunchBudget = 20 * time.Minute

// dolphinGate answers the two honest-degradation pre-checks shared by every
// route: the explicit opt-in (501) and the wired bridge (503). It returns
// the bridge and true when the handler may proceed.
func (s *Server) dolphinGate(w http.ResponseWriter, r *http.Request) (dolphin.Bridge, bool) {
	if !dolphin.DevOptIn(os.LookupEnv) {
		s.writeProblem(w, r, http.StatusNotImplemented, "dolphin-dev-not-enabled",
			"501 Not Implemented",
			"the developer Dolphin bridge is not enabled in this process; set GAME_NARRATOR_DEV_DOLPHIN=1 and restart (dev host only)")
		return nil, false
	}
	if s.dolphinBridge == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dolphin-bridge-unwired",
			"Dev Dolphin Unavailable",
			"no Dolphin bridge is wired into this process; the route exists but there is no emulator to drive")
		return nil, false
	}
	return s.dolphinBridge, true
}

// handleDolphinStatus answers GET /api/v1/dev/dolphin with the bridge's
// self-report. The body is the bridge's Status verbatim: state, pids, the
// resolved render window, the pipe connection fact, and the image hash —
// never the image path.
func (s *Server) handleDolphinStatus(w http.ResponseWriter, r *http.Request) {
	b, ok := s.dolphinGate(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, b.State())
}

// dolphinLaunchRequest is the launch body. Every field is optional: the
// image comes from the process environment only, the user dir is always a
// fresh isolated dir (a request-supplied path could not name anything but
// the real Dolphin dir, which the bridge refuses), and the defaults are the
// spike's proven config (OGL backend, native resolution, no savestate).
type dolphinLaunchRequest struct {
	Backend   string `json:"backend"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	SaveState string `json:"save_state"`
	Movie     string `json:"movie"`
}

// handleDolphinLaunch answers POST /api/v1/dev/dolphin/launch. Launch is
// long (a private Xvfb plus the emulator's window), so it runs in the
// background and the answer is 202 with the status as known at dispatch:
// the client then polls GET /api/v1/dev/dolphin until state is "running"
// (or "failed", whose last_error names the reason). A second launch while
// one is in flight answers 409, not a second emulator.
func (s *Server) handleDolphinLaunch(w http.ResponseWriter, r *http.Request) {
	b, ok := s.dolphinGate(w, r)
	if !ok {
		return
	}
	var req dolphinLaunchRequest
	if r.Body != nil {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-dolphin-launch",
				"Bad Request", "the launch body is not a valid JSON object")
			return
		}
	}
	if req.Width < 0 || req.Height < 0 {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-dolphin-launch",
			"Bad Request", "width and height must be non-negative")
		return
	}

	st := b.State()
	if st.State == dolphin.StateRunning || st.State == dolphin.StateLaunching {
		s.writeProblem(w, r, http.StatusConflict, "dolphin-already-launching",
			"Conflict", "the bridge is already "+string(st.State)+"; stop it before launching again")
		return
	}

	spec := dolphin.LaunchSpec{
		Backend:   req.Backend,
		Width:     req.Width,
		Height:    req.Height,
		SaveState: req.SaveState,
		Movie:     req.Movie,
		// The run outlives this request: an explicit POST /stop ends it, and
		// the process's own death kills the emulator outright (Pdeathsig on
		// every child the bridge spawns). A request-scoped context would
		// instead kill a live game session the moment this handler returns.
		Lifetime: context.Background(),
	}
	// The budget bounds the setup phase only; the goroutine's deferred
	// cancel releases it once Launch has returned, after which the run is
	// owned by the lifetime context above, not by this timeout.
	setupCtx, cancel := context.WithTimeout(context.Background(), dolphinLaunchBudget)
	go func() {
		defer cancel()
		if err := b.Launch(setupCtx, spec); err != nil && !errors.Is(err, dolphin.ErrAlreadyLaunched) {
			// Launch records its own failure in the state (last_error); the
			// log line keeps the request correlation for the inspector.
			s.log.Warn("dolphin: launch failed", "error", err)
		}
	}()
	s.log.Info("dolphin: launch dispatched", "state", string(b.State().State))
	writeJSON(w, http.StatusAccepted, b.State())
}

// dolphinInputRequest is the input body: button names (a, b, x, y, z,
// start, l, r, up, down, left, right) and axis names (main_x, main_y,
// stick_x, stick_y in -1..1; l, r triggers in 0..1), plus the hold in
// milliseconds. Unknown names are a 400, not a silent drop: a typo that
// does nothing is the input path's worst failure (the operator believes the
// press landed).
type dolphinInputRequest struct {
	Buttons map[string]bool    `json:"buttons"`
	Axes    map[string]float64 `json:"axes"`
	HoldMS  int                `json:"hold_ms"`
}

// handleDolphinInput answers POST /api/v1/dev/dolphin/input. It is
// synchronous: the command is short (PRESS/SET, the hold, RELEASE) and the
// caller wants to know it was written. "Accepted" still means "written to
// the controller device", not "the game reacted" — the scene predicate
// (ProveInput) is the acceptance proof, and the API does not claim more.
func (s *Server) handleDolphinInput(w http.ResponseWriter, r *http.Request) {
	b, ok := s.dolphinGate(w, r)
	if !ok {
		return
	}
	if st := b.State(); st.State != dolphin.StateRunning {
		s.dolphinNotRunning(w, r, st)
		return
	}
	var req dolphinInputRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-dolphin-input",
			"Bad Request", "the input body is not a valid JSON object")
		return
	}
	buttons, err := mapButtons(req.Buttons)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-dolphin-input",
			"Bad Request", err.Error())
		return
	}
	axes, err := mapAxes(req.Axes)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-dolphin-input",
			"Bad Request", err.Error())
		return
	}
	hold := time.Duration(req.HoldMS) * time.Millisecond
	if err := b.Inject(r.Context(), buttons, axes, hold); err != nil {
		switch {
		case errors.Is(err, dolphin.ErrNotRunning), errors.Is(err, dolphin.ErrNotLaunched):
			s.dolphinNotRunning(w, r, b.State())
		case errors.Is(err, dolphin.ErrPipeNotConnected):
			s.writeProblem(w, r, http.StatusServiceUnavailable, "dolphin-pipe-not-connected",
				"Dev Dolphin Unavailable",
				"the controller pipe is not connected (the emulator has not bound it yet); retry once the state is running")
		default:
			s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
				"Internal Error", "injecting the input failed; see the server log for this request_id")
		}
		return
	}
	writeJSON(w, http.StatusOK, b.State())
}

// handleDolphinScreenshot answers POST /api/v1/dev/dolphin/screenshot with
// the render window's current frame as a fresh PNG. It is deliberately not
// stored (docs/spec/11: the dev route serves the pixels; nothing here adds
// them to the asset store, the event log or the disk).
func (s *Server) handleDolphinScreenshot(w http.ResponseWriter, r *http.Request) {
	b, ok := s.dolphinGate(w, r)
	if !ok {
		return
	}
	if st := b.State(); st.State != dolphin.StateRunning {
		s.dolphinNotRunning(w, r, st)
		return
	}
	img, err := b.Screenshot(r.Context())
	if err != nil {
		switch {
		case errors.Is(err, dolphin.ErrNotRunning), errors.Is(err, dolphin.ErrNotLaunched):
			s.dolphinNotRunning(w, r, b.State())
		default:
			s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
				"Internal Error", "the frame grab failed; see the server log for this request_id")
		}
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(http.StatusOK)
	if err := png.Encode(w, img); err != nil {
		s.log.Warn("dolphin: screenshot encoding failed mid-response", "error", err)
	}
}

// handleDolphinStop answers POST /api/v1/dev/dolphin/stop. Stop is
// idempotent in the bridge (a second call is a no-op), so the route says so
// in the body instead of inventing a 409 for stopping something already
// stopped.
func (s *Server) handleDolphinStop(w http.ResponseWriter, r *http.Request) {
	b, ok := s.dolphinGate(w, r)
	if !ok {
		return
	}
	b.Stop()
	writeJSON(w, http.StatusOK, b.State())
}

// dolphinNotRunning is the 409 the input and screenshot routes answer while
// the bridge is not in the running state: the condition is branchable and
// the state field says what it is instead of a bare 409.
func (s *Server) dolphinNotRunning(w http.ResponseWriter, r *http.Request, st dolphin.Status) {
	detail := "the bridge is " + string(st.State)
	if st.LastError != "" {
		detail += " (" + st.LastError + ")"
	}
	s.writeProblem(w, r, http.StatusConflict, "dolphin-not-running",
		"Conflict", detail+"; launch it and wait for the running state first")
}

// mapButtons renders the JSON button names onto the bridge's Buttons. The
// set is closed: an unknown name is an error, so "star" cannot silently do
// nothing while the operator watches for a reaction.
func mapButtons(in map[string]bool) (dolphin.Buttons, error) {
	var out dolphin.Buttons
	fields := map[string]*bool{
		"a": &out.A, "b": &out.B, "x": &out.X, "y": &out.Y, "z": &out.Z,
		"start": &out.Start, "l": &out.L, "r": &out.R,
		"up": &out.Up, "down": &out.Down, "left": &out.Left, "right": &out.Right,
	}
	for name, v := range in {
		f, ok := fields[name]
		if !ok {
			return out, fmt.Errorf("unknown button %q (known: a b x y z start l r up down left right)", name)
		}
		if v {
			*f = true
		}
	}
	return out, nil
}

// mapAxes renders the JSON axis names onto the bridge's Axes with range
// checks: sticks are -1..1, triggers 0..1. Out-of-range values are a 400,
// because a clamped value would hide the operator's typo.
func mapAxes(in map[string]float64) (dolphin.Axes, error) {
	var out dolphin.Axes
	sticks := map[string]*float64{
		"main_x": &out.MainX, "main_y": &out.MainY,
		"stick_x": &out.StickX, "stick_y": &out.StickY,
	}
	triggers := map[string]*float64{"l": &out.L, "r": &out.R}
	for name, v := range in {
		if f, ok := sticks[name]; ok {
			if v < -1 || v > 1 {
				return out, fmt.Errorf("axis %q must be in -1..1, got %v", name, v)
			}
			*f = v
			continue
		}
		if f, ok := triggers[name]; ok {
			if v < 0 || v > 1 {
				return out, fmt.Errorf("trigger %q must be in 0..1, got %v", name, v)
			}
			*f = v
			continue
		}
		return out, fmt.Errorf("unknown axis %q (known: main_x main_y stick_x stick_y l r)", name)
	}
	return out, nil
}
