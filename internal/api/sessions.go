package api

// Session lifecycle API (docs/spec/08 "APIs": POST /api/v1/sessions,
// GET /api/v1/sessions/:id, POST .../stop).
//
// The runtime.Manager owns everything this package does not: profile
// resolution against the published revision, frame-source allowlisting, the
// one-active-session invariant, pipeline execution, lifecycle events and the
// per-session dev-trace store. A handler here turns one HTTP request into one
// manager call and one manager error into one documented problem code — the
// mapping table is the contract, so a new sentinel error in the runtime must
// get a row here (and an x-problem type in openapi.json), never a free-text
// interpretation.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// startSessionRequest is the POST /api/v1/sessions body.
type startSessionRequest struct {
	ProfileID   string        `json:"profile_id"`
	FrameSource startFrameSrc `json:"frame_source"`
	Mode        string        `json:"mode"`
	DevTrace    bool          `json:"dev_trace"`
}

// startFrameSrc names the frame input. Dir is meaningful for replay only.
type startFrameSrc struct {
	Kind string `json:"kind"`
	Dir  string `json:"dir,omitempty"`
}

// sessionResponse is the API view of a session (runtime.Info is its shape,
// served as-is so the inspector never drifts from the run's own state).
type sessionResponse = runtime.Info

// handleSessionsStart answers POST /api/v1/sessions. Success is 201 with the
// session in state "starting" and its session.started event cursor, per
// docs/spec/08 "Example session workflow".
func (s *Server) handleSessionsStart(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "sessions-unavailable",
			"Sessions Unavailable",
			"no session runtime is wired in this process; the serve configuration did not start the session manager")
		return
	}
	var req startSessionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)).Decode(&req); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-session-request",
			"Bad Request", "the session request body is not valid JSON: "+err.Error())
		return
	}
	req.ProfileID = strings.TrimSpace(req.ProfileID)
	req.FrameSource.Kind = strings.TrimSpace(req.FrameSource.Kind)
	req.FrameSource.Dir = strings.TrimSpace(req.FrameSource.Dir)
	req.Mode = strings.TrimSpace(req.Mode)

	// dev_trace stores sampled gameplay frames under the session (ADR-0024).
	// This route is operator; the evidence it creates is developer evidence.
	// The extra check is the docs/spec/13 "explicit toggle": an operator who
	// can run a session cannot also switch on frame storage for it.
	if req.DevTrace {
		id := IdentityFromContext(r.Context())
		if id == nil || !id.Role.AtLeast(auth.RoleDeveloper) {
			s.writeProblem(w, r, http.StatusForbidden, "dev-trace-requires-developer",
				"Forbidden", "dev_trace requires the developer role; start the session without it")
			return
		}
	}

	info, err := s.sessions.Start(r.Context(), runtime.StartRequest{
		ProfileID:   req.ProfileID,
		FrameSource: runtime.FrameSource{Kind: req.FrameSource.Kind, Dir: req.FrameSource.Dir},
		Mode:        req.Mode,
		DevTrace:    req.DevTrace,
	})
	if err != nil {
		s.sessionProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, info)
}

// handleSessionsGet answers GET /api/v1/sessions/{id}: the current snapshot,
// active or finished. A viewer may read it (session state is not gameplay
// text); only an operator may create or stop one (docs/spec/08 scopes).
func (s *Server) handleSessionsGet(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "sessions-unavailable",
			"Sessions Unavailable",
			"no session runtime is wired in this process; the serve configuration did not start the session manager")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-session-id",
			"Bad Request", "the session id in the path is empty")
		return
	}
	info, ok := s.sessions.Get(id)
	if !ok {
		s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
			"Session Not Found", fmt.Sprintf("no session with id %q exists in this process", id))
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// handleSessionsStop answers POST /api/v1/sessions/{id}/stop. The call is
// idempotent (docs/spec/08: stopping a stopping or stopped session returns the
// current state) and the response is the terminal snapshot when the run has
// settled, so a client never has to distinguish "accepted" from "done".
func (s *Server) handleSessionsStop(w http.ResponseWriter, r *http.Request) {
	if s.sessions == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "sessions-unavailable",
			"Sessions Unavailable",
			"no session runtime is wired in this process; the serve configuration did not start the session manager")
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-session-id",
			"Bad Request", "the session id in the path is empty")
		return
	}
	info, err := s.sessions.Stop(id)
	if err != nil {
		if errors.Is(err, runtime.ErrSessionNotFound) {
			s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
				"Session Not Found", fmt.Sprintf("no session with id %q exists in this process", id))
			return
		}
		if errors.Is(err, runtime.ErrManagerClosed) {
			s.writeProblem(w, r, http.StatusServiceUnavailable, "runtime-closed",
				"Runtime Closed", "the session runtime is shutting down; try again once the process is up")
			return
		}
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "stopping the session failed; see the server log for this request_id")
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// sessionProblem maps a runtime sentinel to its documented problem+json
// answer (docs/spec/08 "Error contract examples"). The stable code travels in
// the problem's type, so a client can branch on machine-readable identity.
func (s *Server) sessionProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runtime.ErrProfileNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "profile-not-found",
			"Profile Not Found", "the profile_id in the request names a profile that does not exist")
	case errors.Is(err, runtime.ErrProfileNotPublished):
		s.writeProblem(w, r, http.StatusConflict, "profile-not-published",
			"Profile Not Published", "the profile is not the edition's published revision; publish it before running it")
	case errors.Is(err, runtime.ErrProfileNotRunnable):
		s.writeProblem(w, r, http.StatusBadRequest, "profile-not-runnable",
			"Profile Not Runnable", "the published profile has no observable regions")
	case errors.Is(err, runtime.ErrInvalidMode):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-mode",
			"Bad Request", "mode must be \"conservative\" or \"predictive\"")
	case errors.Is(err, runtime.ErrUnknownSourceKind):
		s.writeProblem(w, r, http.StatusBadRequest, "unknown-source-kind",
			"Bad Request", "frame_source.kind is not a known frame source kind")
	case errors.Is(err, runtime.ErrSourceUnsupported):
		// v4l2 and dolphin capture are not implemented in this build; the
		// answer says exactly that instead of pretending the device failed.
		s.writeProblem(w, r, http.StatusNotImplemented, "unsupported-capability",
			"Unsupported Capability", "that frame source kind is not implemented in this build")
	case errors.Is(err, runtime.ErrSourceNotAllowed):
		s.writeProblem(w, r, http.StatusBadRequest, "source-not-allowed",
			"Bad Request", "the replay directory is outside the configured allowlist")
	case errors.Is(err, runtime.ErrSourceMissing):
		s.writeProblem(w, r, http.StatusBadRequest, "source-missing",
			"Bad Request", "the replay directory does not exist")
	case errors.Is(err, runtime.ErrAnotherSessionRunning):
		s.writeProblem(w, r, http.StatusConflict, "session-already-running",
			"Session Already Running", "one active session at a time is a key invariant; stop the running session first")
	case errors.Is(err, runtime.ErrDevTraceUnavailable):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-trace-unavailable",
			"Dev Trace Unavailable", "developer-trace storage is not configured in this process; start a session without dev_trace")
	case errors.Is(err, runtime.ErrManagerClosed):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "runtime-closed",
			"Runtime Closed", "the session runtime is shutting down; try again once the process is up")
	default:
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "starting the session failed; see the server log for this request_id")
	}
}

// maxSessionBodyBytes bounds the start body: a profile id, a kind, a dir and a
// mode do not need anywhere near 64 KiB, and a request that does is an attack
// on the appliance's memory before it is a request at all.
const maxSessionBodyBytes = 64 << 10
