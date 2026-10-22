package api

// Speech-cache prune (ADR-0159, issue #49): POST /api/v1/speech/cache/prune
// queues a background job that removes the transient cache entries no current
// published profile revision can hit, through the runtime's reachable-key walk
// and the cache's reference-checked deletion. The work itself is
// runtime.Manager.PruneUnreachableSpeechCache; this file is the route and the
// job handler around it — the mirror of presynth.go (ADR-0153) for the prune
// half of docs/spec/13's Disk-full row.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKindSpeechCachePrune is the queue name of the speech-cache prune job.
const JobKindSpeechCachePrune = "speech.cache_prune"

// SpeechCachePrunePayload is the job's payload (and the route's body).
type SpeechCachePrunePayload struct {
	// DryRun reports the would-be prune: the entries and bytes it would free,
	// without deleting anything.
	DryRun bool `json:"dry_run"`
}

// SpeechCachePruneResult is the prune job's result: the entries removed and
// the bytes actually freed (the dry-run answer is the would-be numbers).
type SpeechCachePruneResult struct {
	Entries int64 `json:"entries"`
	Bytes   int64 `json:"bytes"`
}

// SpeechCachePruner is the runtime half the prune job drives
// (*runtime.Manager implements it).
type SpeechCachePruner interface {
	PruneUnreachableSpeechCache(ctx context.Context, dryRun bool) (int, int64, error)
}

// SpeechCachePruneJobHandler runs one prune. A prune is deterministic and
// safe to retry: a rerun recomputes the reachable set and finds the first
// run's entries gone, so a retry after a crash cannot prune a live entry.
// Payload errors are permanent (a malformed body is a caller bug); a store or
// cache fault is retryable like any other job error.
func SpeechCachePruneJobHandler(p SpeechCachePruner) jobs.Handler {
	return func(ctx context.Context, job jobs.Job, progress jobs.ProgressFn) (json.RawMessage, error) {
		var pl SpeechCachePrunePayload
		if err := json.Unmarshal(job.Payload, &pl); err != nil {
			return nil, jobs.Permanent(fmt.Errorf("%s payload is unreadable: %w", JobKindSpeechCachePrune, err))
		}
		entries, bytes, err := p.PruneUnreachableSpeechCache(ctx, pl.DryRun)
		out, _ := json.Marshal(SpeechCachePruneResult{Entries: int64(entries), Bytes: bytes})
		if err != nil {
			return out, err
		}
		return out, nil
	}
}

// speechCachePruneAccepted is the route's 202 body.
type speechCachePruneAccepted struct {
	JobID   string `json:"job_id"`
	JobKind string `json:"job_kind"`
	DryRun  bool   `json:"dry_run"`
	State   string `json:"state"`
	Detail  string `json:"detail"`
}

// handleSpeechCachePrune serves POST /api/v1/speech/cache/prune. The body is
// the prune's own input: {"dry_run":bool} (absent or false prunes). The prune
// runs as a job like pre-synthesis (ADR-0153): live sessions pause the queue,
// so the job runs between play sessions.
func (s *Server) handleSpeechCachePrune(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	if s.jobQueue == nil {
		s.unavailable(w, r, "jobs-unavailable", "the job queue is not wired in this process")
		return
	}
	var body SpeechCachePrunePayload
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-body", codeInvalidParameter,
				"Bad Request", "the body must be JSON {\"dry_run\":bool}")
			return
		}
	}
	raw, _ := json.Marshal(body)
	job, _, err := s.jobQueue.Enqueue(r.Context(), store.EnqueueParams{
		Kind:        JobKindSpeechCachePrune,
		Payload:     raw,
		MaxAttempts: 3,
		TraceID:     RequestIDFromContext(r.Context()),
	})
	if err != nil {
		s.unavailable(w, r, "jobs-unavailable", "the speech-cache prune could not be queued; retry")
		return
	}
	s.auditCatalogWrite(r, "catalog.speech.cache.prune", map[string]any{
		"job_id": job.ID, "dry_run": body.DryRun,
	})
	writeJSON(w, http.StatusAccepted, speechCachePruneAccepted{
		JobID:   job.ID,
		JobKind: job.Kind,
		DryRun:  body.DryRun,
		State:   job.State,
		Detail:  fmt.Sprintf("follow /api/v1/jobs/%s for progress; live sessions pause the queue, so the job runs between play sessions", job.ID),
	})
}
