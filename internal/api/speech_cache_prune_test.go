package api

// POST /api/v1/speech/cache/prune (ADR-0159, issue #49): the operator command
// to the durable queue that removes the transient cache entries no current
// published profile revision can hit. The route is the prune half of
// docs/spec/13's Disk-full row and the mirror of the pre-synthesis route
// (ADR-0153): a 202 that names the queued job, a job handler that maps the
// runtime's would-be answer onto the queue's retry rules, and the shared
// OpenAPI contract around both.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/jobs"
)

func TestSpeechCachePruneRouteQueuesOneJob(t *testing.T) {
	e := newDiscEnv(t, t.TempDir())
	target := "/api/v1/speech/cache/prune"

	if rec := e.post(auth.RoleViewer, target, `{}`); rec.Code != http.StatusForbidden {
		t.Fatalf("viewer: status %d, want 403", rec.Code)
	}
	rec := e.post(auth.RoleCurator, target, `{"dry_run":true}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("curator: status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var acc speechCachePruneAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &acc); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if acc.JobKind != JobKindSpeechCachePrune || acc.JobID == "" || acc.DryRun != true || acc.State == "" || acc.Detail == "" {
		t.Fatalf("202 body = %+v", acc)
	}
	job, err := e.js.GetJob(context.Background(), acc.JobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != JobKindSpeechCachePrune {
		t.Fatalf("job kind = %q, want %q", job.Kind, JobKindSpeechCachePrune)
	}
	var pl SpeechCachePrunePayload
	if err := json.Unmarshal(job.Payload, &pl); err != nil || pl.DryRun != true {
		t.Fatalf("job payload = %s (%v), want dry_run true", job.Payload, err)
	}

	// Absent dry_run means "prune" — the body's default is the operator's
	// prune, and the accepted body reports the resolved value.
	rec = e.post(auth.RoleCurator, target, `{}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("second request: status %d, want 202: %s", rec.Code, rec.Body.String())
	}
	var acc2 speechCachePruneAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &acc2); err != nil {
		t.Fatalf("decode 202: %v", err)
	}
	if acc2.DryRun {
		t.Fatalf("absent dry_run = true, want the default false")
	}
}

func TestSpeechCachePruneJobHandlerOutcomes(t *testing.T) {
	job := jobs.Job{ID: "j1", Kind: JobKindSpeechCachePrune, Payload: []byte(`{"dry_run":true}`)}

	t.Run("dry run reports the would-be prune", func(t *testing.T) {
		f := &fakeSpeechCachePruner{entries: 3, bytes: 17179869184}
		out, err := SpeechCachePruneJobHandler(f)(context.Background(), job, nil)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		var res SpeechCachePruneResult
		if json.Unmarshal(out, &res) != nil || res.Entries != 3 || res.Bytes != 17179869184 {
			t.Fatalf("result = %s, want entries 3 bytes 17179869184", out)
		}
		if f.dryRun != true {
			t.Fatalf("dry_run = %v, want true", f.dryRun)
		}
	})
	t.Run("prune runs the real thing", func(t *testing.T) {
		f := &fakeSpeechCachePruner{}
		out, err := SpeechCachePruneJobHandler(f)(context.Background(), jobs.Job{ID: "j2", Kind: JobKindSpeechCachePrune, Payload: []byte(`{}`)}, nil)
		if err != nil {
			t.Fatalf("handler: %v", err)
		}
		var res SpeechCachePruneResult
		if json.Unmarshal(out, &res) != nil || res.Entries != 0 || res.Bytes != 0 {
			t.Fatalf("result = %s, want empty zeros", out)
		}
		if f.dryRun {
			t.Fatalf("dry_run = true, want the default false")
		}
	})
	t.Run("a prune error retries and returns the would-be result", func(t *testing.T) {
		f := &fakeSpeechCachePruner{err: errors.New("speech cache prune: store fault")}
		out, err := SpeechCachePruneJobHandler(f)(context.Background(), job, nil)
		if err == nil || jobs.IsPermanent(err) {
			t.Fatalf("err = %v, want a retryable error", err)
		}
		var res SpeechCachePruneResult
		if json.Unmarshal(out, &res) != nil || res.Entries != 0 {
			t.Fatalf("result = %s, want the zero result alongside the retryable error", out)
		}
	})
	t.Run("bad payload is permanent", func(t *testing.T) {
		bad := jobs.Job{ID: "j3", Kind: JobKindSpeechCachePrune, Payload: []byte(`{"dry_run":`)}
		_, err := SpeechCachePruneJobHandler(&fakeSpeechCachePruner{})(context.Background(), bad, nil)
		if !jobs.IsPermanent(err) {
			t.Fatalf("err = %v, want permanent", err)
		}
	})
}

// fakeSpeechCachePruner is the runtime half the prune job drives; the route
// tests queue through it, the handler tests answer through it.
type fakeSpeechCachePruner struct {
	dryRun  bool
	entries int
	bytes   int64
	err     error
}

func (f *fakeSpeechCachePruner) PruneUnreachableSpeechCache(_ context.Context, dryRun bool) (int, int64, error) {
	f.dryRun = dryRun
	return f.entries, f.bytes, f.err
}
