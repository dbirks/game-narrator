package api

// Profile pre-synthesis (ADR 0153): POST /api/v1/profiles/{id}/presynthesize
// queues a background job that fills the TTS cache with the profile's lines
// under the live key identity, so play hits the cache. The work itself is
// runtime.Manager.PresynthesizeProfile; this file is the route and the job
// handler around it. Curator role (the /api/v1/profiles wildcard grant): the
// job spends the shared TTS budget on curated content.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/jobs"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKindPresynthProfile is the queue name of the pre-synthesis job.
const JobKindPresynthProfile = "speech.presynth_profile"

// PresynthPayload is the job's payload (and the route's body).
type PresynthPayload struct {
	ProfileID   string `json:"profile_id"`
	Limit       int    `json:"limit,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

// ProfilePresynthesizer is the runtime half the job drives
// (*runtime.Manager implements it).
type ProfilePresynthesizer interface {
	PresynthesizeProfile(ctx context.Context, profileID string, opt rt.PresynthOptions, progress func(rt.PresynthProgress)) (rt.PresynthProgress, error)
}

// PresynthProfileJobHandler runs one pre-synthesis. A gateway outage (the
// consecutive-failure stop) is retryable — the job resumes from the cache —
// while a disk-budget refusal or a bad payload is permanent.
func PresynthProfileJobHandler(p ProfilePresynthesizer, dataDir string) jobs.Handler {
	return func(ctx context.Context, job jobs.Job, progress jobs.ProgressFn) (json.RawMessage, error) {
		var pl PresynthPayload
		if err := json.Unmarshal(job.Payload, &pl); err != nil {
			return nil, jobs.Permanent(fmt.Errorf("%s payload is unreadable: %w", JobKindPresynthProfile, err))
		}
		if !games.ValidID(pl.ProfileID) {
			return nil, jobs.Permanent(fmt.Errorf("%s payload names profile %q, which is not a usable id", JobKindPresynthProfile, pl.ProfileID))
		}
		opt := rt.PresynthOptions{Limit: pl.Limit, Concurrency: pl.Concurrency}
		if dataDir != "" {
			opt.FreeBytes = rt.FreeBytesAt(dataDir)
		}
		res, err := p.PresynthesizeProfile(ctx, pl.ProfileID, opt, func(pp rt.PresynthProgress) {
			if progress == nil || pp.Total == 0 {
				return
			}
			progress(float64(pp.Done)/float64(pp.Total)*100,
				fmt.Sprintf("%d/%d lines: %d cached, %d synthesized, %d skipped (no voice), %d failed",
					pp.Done, pp.Total, pp.CacheHits, pp.Synthesized, pp.Skipped, pp.Failed))
		})
		out, _ := json.Marshal(res)
		switch {
		case err == nil:
			return out, nil
		case errors.Is(err, rt.ErrPresynthDiskBudget), errors.Is(err, store.ErrNotFound):
			return out, jobs.Permanent(err)
		default:
			return out, err
		}
	}
}

// presynthAccepted is the route's 202 body.
type presynthAccepted struct {
	JobID     string `json:"job_id"`
	JobKind   string `json:"job_kind"`
	ProfileID string `json:"profile_id"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}

// handleProfilePresynthesize serves POST /api/v1/profiles/{id}/presynthesize.
func (s *Server) handleProfilePresynthesize(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	if s.jobQueue == nil {
		s.unavailable(w, r, "jobs-unavailable", "the job queue is not wired in this process")
		return
	}
	id := r.PathValue("id")
	if _, _, err := s.store.GetProfile(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeCodeProblem(w, r, http.StatusNotFound, "profile-not-found", codeNotFound,
				"Not Found", fmt.Sprintf("no profile %q", id))
			return
		}
		s.unavailable(w, r, "store-unavailable", "the profile could not be read; retry")
		return
	}
	var body PresynthPayload
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-body", codeInvalidParameter,
				"Bad Request", "the body must be JSON {\"limit\"?: int, \"concurrency\"?: 1..4}")
			return
		}
	}
	if body.Limit < 0 || body.Concurrency < 0 || body.Concurrency > 4 {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-body", codeInvalidParameter,
			"Bad Request", "limit must be >= 0 and concurrency 0..4 (0 takes the default of 1)")
		return
	}
	body.ProfileID = id
	// One run per profile at a time: a second request while one is queued
	// or running gets that job back as a conflict.
	if s.jobStore != nil {
		for _, state := range []string{"queued", "running"} {
			list, _, err := s.jobStore.ListJobs(r.Context(), store.JobFilter{State: state, Kind: JobKindPresynthProfile}, "", 200)
			if err != nil {
				continue
			}
			for i := range list {
				var pl PresynthPayload
				if json.Unmarshal(list[i].Payload, &pl) == nil && pl.ProfileID == id {
					s.writeCodeProblem(w, r, http.StatusConflict, "presynth-in-progress", codeConflict,
						"Conflict", fmt.Sprintf("profile %s already has pre-synthesis job %s (%s); follow /api/v1/jobs/%s", id, list[i].ID, list[i].State, list[i].ID))
					return
				}
			}
		}
	}
	raw, _ := json.Marshal(body)
	job, _, err := s.jobQueue.Enqueue(r.Context(), store.EnqueueParams{
		Kind:        JobKindPresynthProfile,
		Payload:     raw,
		MaxAttempts: 3,
		TraceID:     RequestIDFromContext(r.Context()),
	})
	if err != nil {
		s.unavailable(w, r, "jobs-unavailable", "the pre-synthesis could not be queued; retry")
		return
	}
	s.auditCatalogWrite(r, "catalog.profile.presynthesize", map[string]any{
		"job_id": job.ID, "profile_id": id, "limit": body.Limit, "concurrency": body.Concurrency,
	})
	writeJSON(w, http.StatusAccepted, presynthAccepted{
		JobID: job.ID, JobKind: job.Kind, ProfileID: id, State: job.State,
		Detail: fmt.Sprintf("follow /api/v1/jobs/%s for progress; live sessions pause the queue, so the job runs between play sessions", job.ID),
	})
}
