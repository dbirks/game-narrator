package api

// /api/v1/jobs — the durable workflow queue surface (docs/spec/08 "APIs":
// "GET /api/v1/jobs, GET /api/v1/jobs/:id, POST .../cancel, POST .../retry").
//
// Scope note, stated so the gap is a decision and not an accident: there is
// deliberately no POST /api/v1/jobs yet. Which job kinds an unauthenticated
// caller may enqueue is a policy question the auth milestone owns
// (docs/spec/08 "Token/session scopes"), and docs/spec/14 forbids an
// appliance-side DoS surface — enqueuing arrives through the server's own
// subsystems (imports, voice, onboarding) via jobs.Runner/store.JobsStore.
// Everything read/cancel/retry here is likewise unauthenticated so far, on
// the M0 localhost-bind assumption; a later task layers scopes over these
// exact routes.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// jobDTO is the wire shape of a `jobs` row. Timestamps are RFC 3339 UTC
// (docs/spec/08 "Conventions"); payload/result are passed through as JSON,
// never re-encoded strings.
type jobDTO struct {
	ID             string          `json:"id"`
	Kind           string          `json:"kind"`
	State          string          `json:"state"`
	Priority       int             `json:"priority"`
	Attempts       int             `json:"attempts"`
	MaxAttempts    int             `json:"max_attempts"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	ParentID       string          `json:"parent_id,omitempty"`
	TraceID        string          `json:"trace_id"`
	RunAfter       string          `json:"run_after,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	LeaseOwner     string          `json:"lease_owner,omitempty"`
	CreatedAt      string          `json:"created_at"`
	UpdatedAt      string          `json:"updated_at"`
}

type jobAttemptDTO struct {
	Attempt    int    `json:"attempt"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Outcome    string `json:"outcome"`
	Error      string `json:"error,omitempty"`
}

type jobListResponse struct {
	Jobs       []jobDTO `json:"jobs"`
	NextCursor string   `json:"next_cursor,omitempty"`
}

// jobDetailResponse adds the attempt history: the crash evidence
// docs/spec/13's reconciler and the developer inspector read belongs with the
// job it explains, in one round trip.
type jobDetailResponse struct {
	jobDTO
	Attempts []jobAttemptDTO `json:"attempts"`
}

func toJobDTO(j *store.Job) jobDTO {
	dto := jobDTO{
		ID:             j.ID,
		Kind:           j.Kind,
		State:          j.State,
		Priority:       j.Priority,
		Attempts:       j.Attempts,
		MaxAttempts:    j.MaxAttempts,
		Payload:        j.Payload,
		Result:         j.Result,
		ParentID:       j.ParentID,
		TraceID:        j.TraceID,
		IdempotencyKey: j.IdempotencyKey,
		LeaseOwner:     j.LeaseOwner,
		CreatedAt:      fmtTime(j.CreatedAt),
		UpdatedAt:      fmtTime(j.UpdatedAt),
	}
	if !j.RunAfter.IsZero() {
		dto.RunAfter = fmtTime(j.RunAfter)
	}
	return dto
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// jobsUnavailable answers 503 when no queue is wired: an endpoint that
// returned an empty list would read as "the appliance has no background
// work" rather than "this process has no queue" (docs/spec/13 "no generic
// success on an unprobed service").
func (s *Server) jobsUnavailable(w http.ResponseWriter, r *http.Request) {
	s.writeProblem(w, r, http.StatusServiceUnavailable, "jobs-unavailable",
		"Jobs Unavailable", "no durable job queue is configured in this process")
}

// handleJobsList serves GET /api/v1/jobs?state=&kind=&cursor=&limit=.
func (s *Server) handleJobsList(w http.ResponseWriter, r *http.Request) {
	if s.jobStore == nil {
		s.jobsUnavailable(w, r)
		return
	}
	q := r.URL.Query()
	f := store.JobFilter{State: q.Get("state"), Kind: q.Get("kind")}
	if f.State != "" && !validJobStateParam(f.State) {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-job-state", "Bad Request",
			"state must be one of queued, running, succeeded, failed, cancelled, recoverable, requires_input")
		return
	}
	limit := 0
	if raw := q.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-limit", "Bad Request",
				"limit must be a positive integer")
			return
		}
		limit = n
	}

	jobs, next, err := s.jobStore.ListJobs(r.Context(), f, q.Get("cursor"), limit)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidCursor):
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-cursor", "Bad Request",
				"cursor is not a pagination token issued by this service")
		default:
			s.log.Warn("job list failed", "request_id", RequestIDFromContext(r.Context()), "error", err)
			s.writeProblem(w, r, http.StatusServiceUnavailable, "job-store-unavailable",
				"Jobs Unavailable", "the job queue could not answer this query")
		}
		return
	}
	out := jobListResponse{Jobs: make([]jobDTO, 0, len(jobs)), NextCursor: next}
	for i := range jobs {
		out.Jobs = append(out.Jobs, toJobDTO(&jobs[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// jobIDFromPath resolves the {id} wildcard, refusing malformed ids as 404
// (same answer as "no such job", so the endpoint does not oracle which ids
// exist for a scanner).
func (s *Server) jobIDFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !games.ValidID(id) {
		s.writeProblem(w, r, http.StatusNotFound, "job-not-found", "Not Found",
			"no job has that identifier")
		return "", false
	}
	return id, true
}

// handleJobGet serves GET /api/v1/jobs/{id} including the attempt history.
func (s *Server) handleJobGet(w http.ResponseWriter, r *http.Request) {
	if s.jobStore == nil {
		s.jobsUnavailable(w, r)
		return
	}
	id, ok := s.jobIDFromPath(w, r)
	if !ok {
		return
	}
	job, err := s.jobStore.GetJob(r.Context(), id)
	if err != nil {
		s.jobError(w, r, id, err, "get job")
		return
	}
	attempts, err := s.jobStore.ListJobAttempts(r.Context(), id)
	if err != nil {
		s.jobError(w, r, id, err, "list job attempts")
		return
	}
	detail := jobDetailResponse{jobDTO: toJobDTO(job), Attempts: make([]jobAttemptDTO, 0, len(attempts))}
	for _, a := range attempts {
		detail.Attempts = append(detail.Attempts, jobAttemptDTO{
			Attempt:    a.Attempt,
			StartedAt:  fmtTime(a.StartedAt),
			FinishedAt: fmtTime(a.FinishedAt),
			Outcome:    a.Outcome,
			Error:      a.Error,
		})
	}
	writeJSON(w, http.StatusOK, detail)
}

// handleJobCancel serves POST /api/v1/jobs/{id}/cancel. Idempotent for a
// queued/running job already cancelled; 409 once the job succeeded or failed
// (there is nothing to cancel — retry instead).
func (s *Server) handleJobCancel(w http.ResponseWriter, r *http.Request) {
	if s.jobStore == nil {
		s.jobsUnavailable(w, r)
		return
	}
	id, ok := s.jobIDFromPath(w, r)
	if !ok {
		return
	}
	job, err := s.jobStore.Cancel(r.Context(), id)
	if err != nil {
		s.jobError(w, r, id, err, "cancel job")
		return
	}
	writeJSON(w, http.StatusOK, toJobDTO(job))
}

// handleJobRetry serves POST /api/v1/jobs/{id}/retry. Only `failed` and
// `cancelled` jobs may be re-queued (docs/spec/08's jobs contract); anything
// else answers 409 problem+json naming the current state.
func (s *Server) handleJobRetry(w http.ResponseWriter, r *http.Request) {
	if s.jobStore == nil {
		s.jobsUnavailable(w, r)
		return
	}
	id, ok := s.jobIDFromPath(w, r)
	if !ok {
		return
	}
	job, err := s.jobStore.Retry(r.Context(), id)
	if err != nil {
		s.jobError(w, r, id, err, "retry job")
		return
	}
	writeJSON(w, http.StatusOK, toJobDTO(job))
}

// jobError maps the store's typed errors onto the docs/spec/08 error
// contract: 404 RESOURCE_NOT_FOUND for a missing job, 409 for a
// state-machine conflict, 503 problem+json for everything else (the queue
// itself is still there — a locked database is retryable, and saying 500
// would tell the UI to give up when refreshing is the fix).
func (s *Server) jobError(w http.ResponseWriter, r *http.Request, id string, err error, op string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "job-not-found", "Not Found",
			"no job has that identifier")
	case errors.Is(err, store.ErrConflict):
		s.writeProblem(w, r, http.StatusConflict, "job-state-conflict", "Conflict", err.Error())
	default:
		s.log.Warn("job operation failed",
			"request_id", RequestIDFromContext(r.Context()), "op", op, "job_id", id, "error", err)
		s.writeProblem(w, r, http.StatusServiceUnavailable, "job-store-unavailable",
			"Jobs Unavailable", "the job queue could not perform this operation")
	}
}

// validJobStateParam mirrors the `jobs.state` CHECK enum. The store refuses
// a bad filter too, but the 400 here is the API's own contract: a client
// typo is answered with the allowed set, not an opaque internal error.
func validJobStateParam(s string) bool {
	switch s {
	case "queued", "running", "succeeded", "failed", "cancelled", "recoverable", "requires_input":
		return true
	}
	return false
}
