package api

// POST /api/v1/profiles/{id}/presynthesize (ADR 0153): the route queues one
// speech.presynth_profile job per profile, and the job handler maps the
// runtime's outcomes onto the queue's retry rules.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/jobs"
	rt "github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

func TestPresynthesizeRouteQueuesOnePerProfile(t *testing.T) {
	// The disc env's runner does not register the presynth kind, so a queued
	// job stays queued — exactly the window the 409 guards.
	e := newDiscEnv(t, t.TempDir())
	profileID, _, _ := newDiscDraft(t, e)
	target := "/api/v1/profiles/" + profileID + "/presynthesize"

	if rec := e.post(auth.RoleViewer, target, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: status %d, want 403", rec.Code)
	}
	if rec := e.post(auth.RoleCurator, target, `{"concurrency":9}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("concurrency 9: status %d, want 400", rec.Code)
	}
	if rec := e.post(auth.RoleCurator, "/api/v1/profiles/01M5NOPE00000000000000000Z/presynthesize", `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown profile: status %d, want 404 (%s)", rec.Code, rec.Body.String())
	}

	rec := e.post(auth.RoleCurator, target, `{"limit":50,"concurrency":2}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var acc presynthAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if acc.JobKind != JobKindPresynthProfile || acc.ProfileID != profileID || acc.JobID == "" {
		t.Fatalf("202 body = %+v", acc)
	}
	job, err := e.js.GetJob(context.Background(), acc.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	var pl PresynthPayload
	if err := json.Unmarshal(job.Payload, &pl); err != nil || pl.ProfileID != profileID || pl.Limit != 50 || pl.Concurrency != 2 {
		t.Fatalf("job payload = %s (%v), want the profile, limit 50, concurrency 2", job.Payload, err)
	}

	again := e.post(auth.RoleCurator, target, `{}`)
	if again.Code != http.StatusConflict || !strings.Contains(again.Body.String(), acc.JobID) {
		t.Fatalf("second request: status %d, want 409 naming %s: %s", again.Code, acc.JobID, again.Body.String())
	}
}

type fakePresynth struct {
	err   error
	steps []rt.PresynthProgress
	opt   rt.PresynthOptions
}

func (f *fakePresynth) PresynthesizeProfile(_ context.Context, _ string, opt rt.PresynthOptions, progress func(rt.PresynthProgress)) (rt.PresynthProgress, error) {
	f.opt = opt
	var last rt.PresynthProgress
	for _, s := range f.steps {
		progress(s)
		last = s
	}
	return last, f.err
}

func TestPresynthJobHandlerOutcomes(t *testing.T) {
	payload, _ := json.Marshal(PresynthPayload{ProfileID: "01M5PRESYNTH000000000000AB", Concurrency: 2})
	job := jobs.Job{ID: "j1", Kind: JobKindPresynthProfile, Payload: payload}

	t.Run("progress and result", func(t *testing.T) {
		f := &fakePresynth{steps: []rt.PresynthProgress{{Total: 4, Done: 2, CacheHits: 1, Synthesized: 1}, {Total: 4, Done: 4, CacheHits: 1, Synthesized: 3}}}
		var pcts []float64
		out, err := PresynthProfileJobHandler(f, t.TempDir())(context.Background(), job, func(pct float64, _ string) { pcts = append(pcts, pct) })
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		if len(pcts) != 2 || pcts[0] != 50 || pcts[1] != 100 {
			t.Fatalf("progress = %v, want [50 100]", pcts)
		}
		var res rt.PresynthProgress
		if json.Unmarshal(out, &res) != nil || res.Synthesized != 3 {
			t.Fatalf("result = %s", out)
		}
		if f.opt.Concurrency != 2 || f.opt.FreeBytes == nil {
			t.Fatalf("options = %+v, want concurrency 2 and a disk probe", f.opt)
		}
	})
	t.Run("disk budget is permanent", func(t *testing.T) {
		f := &fakePresynth{err: rt.ErrPresynthDiskBudget}
		_, err := PresynthProfileJobHandler(f, "")(context.Background(), job, nil)
		if !jobs.IsPermanent(err) {
			t.Fatalf("err = %v, want permanent", err)
		}
	})
	t.Run("gateway outage retries", func(t *testing.T) {
		f := &fakePresynth{err: errors.New("presynth: 5 consecutive provider failures")}
		_, err := PresynthProfileJobHandler(f, "")(context.Background(), job, nil)
		if err == nil || jobs.IsPermanent(err) {
			t.Fatalf("err = %v, want a retryable error", err)
		}
	})
	t.Run("unknown profile is permanent", func(t *testing.T) {
		f := &fakePresynth{err: store.ErrNotFound}
		_, err := PresynthProfileJobHandler(f, "")(context.Background(), job, nil)
		if !jobs.IsPermanent(err) {
			t.Fatalf("err = %v, want permanent", err)
		}
	})
	t.Run("bad payload is permanent", func(t *testing.T) {
		bad := jobs.Job{ID: "j2", Kind: JobKindPresynthProfile, Payload: []byte(`{"profile_id":"nope"}`)}
		_, err := PresynthProfileJobHandler(&fakePresynth{}, "")(context.Background(), bad, nil)
		if !jobs.IsPermanent(err) {
			t.Fatalf("err = %v, want permanent", err)
		}
	})
}
