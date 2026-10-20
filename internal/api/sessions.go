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

// startFrameSrc names the frame input. Dir is the replay clip's directory
// for kind replay, and the capture device's stable id for kind v4l2 (ADR-
// 0078: the enumeration is the allowlist, an empty id resolves the persisted
// selection then the first usable device). ClipID (ADR-0073) selects a clip
// from the managed clip store by id; the runtime resolves it inside the store
// and re-checks the result against the replay-root allowlist, so the id alone
// cannot name a path.
type startFrameSrc struct {
	Kind   string `json:"kind"`
	Dir    string `json:"dir,omitempty"`
	ClipID string `json:"clip_id,omitempty"`
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
	req.FrameSource.ClipID = strings.TrimSpace(req.FrameSource.ClipID)
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
		FrameSource: runtime.FrameSource{Kind: req.FrameSource.Kind, Dir: req.FrameSource.Dir, ClipID: req.FrameSource.ClipID},
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

// handleSessionsAudio answers POST /api/v1/sessions/{id}/audio: the live
// audio controls of docs/spec/07 ("Apply volume, short fade, optional delay
// calibration") — mute, volume and the speech delay. The body is partial:
// a nil field keeps its current value, so the operator never has to read
// the settings back to change one of them. The answer is the session's fresh
// snapshot with the applied state, so the caller sees the result, not just
// an acknowledgement. A session the process could not give a voice pipeline
// answers 503 audio-unavailable: the control was not applied to nothing.
func (s *Server) handleSessionsAudio(w http.ResponseWriter, r *http.Request) {
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
	// runtime.AudioSettings is the wire shape: each control is optional and
	// the manager validates the ranges (volume 0..1, delay 0..2000 ms).
	var req runtime.AudioSettings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)).Decode(&req); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-audio-settings",
			"Bad Request", "the audio settings body is not valid JSON: "+err.Error())
		return
	}
	info, err := s.sessions.SetAudio(id, req)
	if err != nil {
		s.audioProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// audioProblem maps the SetAudio sentinels onto the documented answers:
// 404 for a missing session, 400 for an out-of-range control, 503 when the
// process has no voice workflow or the session's sink could not be built —
// the session runs on, it simply cannot be heard (docs/spec/13: the degraded
// status is reported, not hidden).
func (s *Server) audioProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
			"Session Not Found", fmt.Sprintf("no session with id %q exists in this process", r.PathValue("id")))
	case errors.Is(err, runtime.ErrInvalidAudioSettings):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-audio-settings",
			"Bad Request", err.Error())
	case errors.Is(err, runtime.ErrAudioUnavailable):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "audio-unavailable",
			"Audio Unavailable",
			"this session has no voice pipeline: the process has no voice workflow configured, or the session's audio sink could not be built")
	case errors.Is(err, runtime.ErrManagerClosed):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "runtime-closed",
			"Runtime Closed", "the session runtime is shutting down; try again once the process is up")
	default:
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "applying the audio settings failed; see the server log for this request_id")
	}
}

// sessionProblem maps a runtime sentinel to its documented problem+json
// answer (docs/spec/08 "Error contract examples"). The stable code travels in
// the problem's type, so a client can branch on machine-readable identity.
func (s *Server) sessionProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runtime.ErrProfileNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "profile-not-found",
			"Profile Not Found", "the profile_id in the request names a profile that does not exist")
	case errors.Is(err, runtime.ErrProfileDeleted):
		// A soft-deleted revision is a 404 for Play (ADR-0107): the mark is
		// the delete, and starting a session on it would resurrect what the
		// operator removed.
		s.writeCodeProblem(w, r, http.StatusNotFound, "profile-deleted", codeNotFound, "Not Found",
			"the profile was deleted (its receipt's purge_after is the retention deadline); a session cannot start on a deleted revision")
	case errors.Is(err, runtime.ErrGameDisabled):
		// The game's disable switch (ADR-0107): a 409, not a 404 — the game
		// and its profiles exist and stay editable; the operator re-enables
		// it to make it playable again.
		s.writeCodeProblem(w, r, http.StatusConflict, "game-disabled", "GAME_DISABLED", "Conflict",
			"the game is disabled: it is hidden from Play and narration while its profiles stay editable — re-enable it before starting a session")
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
		// The live capture kinds that are not wired in this process: v4l2
		// without a capture wiring (an unwired process refuses rather than
		// fakes a device, ADR-0078) and the dev-only dolphin bridge without
		// its opt-in/closure. The answer says exactly that instead of
		// pretending the device failed.
		s.writeProblem(w, r, http.StatusNotImplemented, "unsupported-capability",
			"Unsupported Capability", "that frame source kind is not available in this build")
	case errors.Is(err, runtime.ErrCaptureUnavailable):
		// The v4l2 kind with no capture wiring: the capture subsystem is not
		// configured in this process, which is a 503 (unavailable) rather
		// than a 501 — the kind is implemented, the process just has no
		// dongle layer wired into it (ADR-0078).
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-unavailable",
			"CAPTURE_UNAVAILABLE", "Service Unavailable",
			"no capture device layer is wired into this process; the v4l2 frame source cannot run")
	case errors.Is(err, runtime.ErrCaptureDeviceAbsent):
		// No dongle attached: the operator action is the physical connection
		// (docs/spec/08's DEVICE_UNAVAILABLE class).
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "capture-device-unavailable",
			"DEVICE_UNAVAILABLE", "Service Unavailable",
			"no usable capture device is attached: connect the UVC HDMI capture dongle and retry")
	case errors.Is(err, runtime.ErrCaptureDeviceUnknown):
		// The client named a device the enumeration does not have: a client
		// error, and the remedy is the device list route.
		s.writeProblem(w, r, http.StatusBadRequest, "capture-device-unknown",
			"Bad Request", "the capture device in frame_source.dir is not in the current enumeration: see GET /api/v1/capture/devices for the ids a session may name")
	case errors.Is(err, runtime.ErrCaptureModeUnavailable):
		// No offered mode fits the policy: the device is attached but offers
		// nothing the card's link budget admits (the doctor's remedy).
		s.writeProblem(w, r, http.StatusServiceUnavailable, "capture-mode-unavailable",
			"Service Unavailable", "no offered capture mode fits the selection policy: the device offers no format the card's link budget admits")
	case errors.Is(err, runtime.ErrSourceNotAllowed):
		s.writeProblem(w, r, http.StatusBadRequest, "source-not-allowed",
			"Bad Request", "the replay directory is outside the configured allowlist")
	case errors.Is(err, runtime.ErrSourceMissing):
		s.writeProblem(w, r, http.StatusBadRequest, "source-missing",
			"Bad Request", "the replay directory does not exist")
	case errors.Is(err, runtime.ErrInvalidClipID):
		// The clip id is the session's selection key for the managed store
		// (ADR-0073); an id that is not a clip id is a client error, and it is
		// answered 400 before the runtime touches the filesystem.
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-clip-id",
			"Bad Request", "frame_source.clip_id is not a valid clip id (a lowercase slug; see POST /api/v1/replay/clips)")
	case errors.Is(err, runtime.ErrClipsUnavailable):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "clips-unavailable",
			"Clips Unavailable", "this process has no managed clip store; upload a clip with POST /api/v1/replay/clips on a wired appliance, or start the session with a replay directory")
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

// handleSessionsPause answers POST /api/v1/sessions/{id}/pause:
// docs/spec/08 "…/pause" — pause stops new speech and fences pending
// utterances. The call is idempotent (pausing a paused session returns the
// same snapshot) and the answer is the session's fresh snapshot, whose
// paused flag the UI's pause button reads. A terminal session answers 409
// session-not-running: a pause the session cannot take is not a pause.
func (s *Server) handleSessionsPause(w http.ResponseWriter, r *http.Request) {
	s.sessionControl(w, r, s.sessions.Pause)
}

// handleSessionsResume answers POST /api/v1/sessions/{id}/resume:
// docs/spec/08 "…/resume" — resume continues from the next commit. The
// session's text machines kept advancing through the pause (the observations
// were real), so the resume changes no belief and the next ready box commits
// exactly once: a resume can never burst the paused window's lines, because
// those decisions were made and are idempotent. Idempotent and snapshot-
// answered like pause.
func (s *Server) handleSessionsResume(w http.ResponseWriter, r *http.Request) {
	s.sessionControl(w, r, s.sessions.Resume)
}

// handleSessionsSettings answers PUT /api/v1/sessions/{id}/settings:
// docs/spec/08 "…/settings" and docs/spec/09 "/play" — the operator's
// mid-session policy, the narrated classes (dialogue / +signs / +items) and
// the conservative/predictive decision mode. The body is partial: a nil field
// keeps the session's current value, so a client that only flips the mode
// never has to re-send the classes it never touched. The answer is the
// session's fresh snapshot carrying the applied mode and the normalized
// class list (dialogue always present, first) — what the UI's toggles read.
func (s *Server) handleSessionsSettings(w http.ResponseWriter, r *http.Request) {
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
	var req runtime.SessionSettings
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSessionBodyBytes)).Decode(&req); err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-session-settings",
			"Bad Request", "the settings body is not valid JSON: "+err.Error())
		return
	}
	if req.Mode != nil {
		m := *req.Mode
		m = strings.TrimSpace(m)
		req.Mode = &m
	}
	info, err := s.sessions.SetSettings(id, req)
	if err != nil {
		s.controlProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// sessionControl is the shared path of the binary live controls (pause,
// resume): the 503 unwired-runtime answer, the id/path validation, the
// manager call, the documented problem mapping and the snapshot answer.
func (s *Server) sessionControl(w http.ResponseWriter, r *http.Request, call func(id string) (runtime.Info, error)) {
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
	info, err := call(id)
	if err != nil {
		s.controlProblem(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

// controlProblem maps the live-control sentinels onto their documented
// answers: 404 for a missing session, 400 for a settings value outside the
// documented vocabulary, 409 for a session that has already reached a
// terminal state (the control is refused, not dropped), 503 for the unwired
// or closing runtime.
func (s *Server) controlProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, runtime.ErrSessionNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "session-not-found",
			"Session Not Found", fmt.Sprintf("no session with id %q exists in this process", r.PathValue("id")))
	case errors.Is(err, runtime.ErrInvalidSessionSettings):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-session-settings",
			"Bad Request", err.Error())
	case errors.Is(err, runtime.ErrSessionNotRunning):
		s.writeProblem(w, r, http.StatusConflict, "session-not-running",
			"Session Not Running", "the session has reached a terminal state; the control was refused, not dropped")
	case errors.Is(err, runtime.ErrManagerClosed):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "runtime-closed",
			"Runtime Closed", "the session runtime is shutting down; try again once the process is up")
	default:
		s.writeProblem(w, r, http.StatusInternalServerError, "internal-error",
			"Internal Error", "applying the session control failed; see the server log for this request_id")
	}
}
