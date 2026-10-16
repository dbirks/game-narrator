package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// jobsFixture wires an API server onto a real migrated SQLite queue — not a
// fake: docs/spec/12 requires that state assertions be backed by the storage
// the appliance actually uses, and the 409s here only mean something if the
// state machine refusing them is the same one the workers drive.
func jobsFixture(t *testing.T) (http.Handler, *store.JobsStore) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	js := store.NewJobsStore(st)
	return newHandler(WithJobs(js)), js
}

func seedJob(t *testing.T, js *store.JobsStore, kind string) *store.Job {
	t.Helper()
	job, created, err := js.Enqueue(context.Background(), store.EnqueueParams{
		Kind: kind, MaxAttempts: 3, Priority: 2,
		Payload: json.RawMessage(`{"file":"synthetic.txt"}`),
	})
	if err != nil || !created {
		t.Fatalf("seed %s: %v", kind, err)
	}
	return job
}

func getJSON(t *testing.T, h http.Handler, target string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := do(t, h, http.MethodGet, target)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	return rec, decodeJSON(t, rec)
}

// TestJobsEndpointsWithoutStoreAreUnavailable: a process with no queue says
// 503 on every jobs route — including the wildcard ones — instead of 200
// with an empty list.
func TestJobsEndpointsWithoutStoreAreUnavailable(t *testing.T) {
	h := newHandler() // no WithJobs
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/jobs"},
		{http.MethodGet, "/api/v1/jobs/" + games.NewID()},
		{http.MethodPost, "/api/v1/jobs/" + games.NewID() + "/cancel"},
		{http.MethodPost, "/api/v1/jobs/" + games.NewID() + "/retry"},
	} {
		rec := do(t, h, tc.method, tc.target)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503 (body %s)", tc.method, tc.target, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
		if !strings.Contains(rec.Body.String(), "jobs-unavailable") {
			t.Fatalf("%s %s body = %s", tc.method, tc.target, rec.Body.String())
		}
	}
}

func TestJobsListEmptyIsAnObjectAndPages(t *testing.T) {
	h, js := jobsFixture(t)

	rec, body := getJSON(t, h, "/api/v1/jobs")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	requireContentType(t, rec, ContentTypeJSON)
	jobsValue, ok := body["jobs"].([]any)
	if !ok || len(jobsValue) != 0 {
		t.Fatalf("empty queue = %v, want an empty \"jobs\" array (null would break every client reducer)", body["jobs"])
	}

	for i := 0; i < 7; i++ {
		seedJob(t, js, fmt.Sprintf("kind.%d", i))
	}

	rec, body = getJSON(t, h, "/api/v1/jobs?limit=3")
	page1, _ := body["jobs"].([]any)
	if len(page1) != 3 {
		t.Fatalf("first page has %d jobs, want 3", len(page1))
	}
	cursor, _ := body["next_cursor"].(string)
	if cursor == "" {
		t.Fatal("next_cursor missing on a truncated page")
	}
	seen := map[string]bool{}
	for _, item := range page1 {
		id := item.(map[string]any)["id"].(string)
		seen[id] = true
	}

	rec, body = getJSON(t, h, "/api/v1/jobs?limit=3&cursor="+cursor)
	for _, item := range body["jobs"].([]any) {
		id := item.(map[string]any)["id"].(string)
		if seen[id] {
			t.Fatalf("cursor pagination repeated job %s", id)
		}
	}

	// limit is hard-capped server-side, not negotiable by query.
	rec, body = getJSON(t, h, "/api/v1/jobs?limit=100000")
	if big, _ := body["jobs"].([]any); len(big) > store.MaxListLimit {
		t.Fatalf("limit=100000 returned %d rows, want the hard cap %d", len(big), store.MaxListLimit)
	}
}

func TestJobsListFilters(t *testing.T) {
	h, js := jobsFixture(t)
	a := seedJob(t, js, "import.parse")
	b := seedJob(t, js, "voice.register")

	_, body := getJSON(t, h, "/api/v1/jobs?kind=import.parse")
	list, _ := body["jobs"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["id"] != a.ID {
		t.Fatalf("kind filter = %v, want only %s", list, a.ID)
	}

	_, body = getJSON(t, h, "/api/v1/jobs?state=queued")
	list, _ = body["jobs"].([]any)
	if len(list) != 2 {
		t.Fatalf("state=queued returned %d, want both seeds", len(list))
	}
	_, body = getJSON(t, h, "/api/v1/jobs?state=succeeded")
	list, _ = body["jobs"].([]any)
	if len(list) != 0 {
		t.Fatalf("state=succeeded returned %d, want 0", len(list))
	}

	// A bad state value is the client's fault, answered with the allowed set.
	rec := do(t, h, http.MethodGet, "/api/v1/jobs?state=exploded")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad state = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)

	rec = do(t, h, http.MethodGet, "/api/v1/jobs?cursor=not-a-token")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	rec = do(t, h, http.MethodGet, "/api/v1/jobs?limit=0")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d, want 400", rec.Code)
	}
	_ = b
}

func TestJobsGetDetail(t *testing.T) {
	h, js := jobsFixture(t)
	job := seedJob(t, js, "import.parse")

	rec, body := getJSON(t, h, "/api/v1/jobs/"+job.ID)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	for field, want := range map[string]any{
		"id": job.ID, "kind": "import.parse", "state": "queued",
		"priority": float64(2), "max_attempts": float64(3), "trace_id": job.TraceID,
	} {
		if got := body[field]; got != want {
			t.Fatalf("%s = %v, want %v", field, got, want)
		}
	}
	payload, ok := body["payload"].(map[string]any)
	if !ok || payload["file"] != "synthetic.txt" {
		t.Fatalf("payload = %v, want the enqueued object", body["payload"])
	}
	// attempts is always an array, and empty until a worker claims the job.
	if att, ok := body["attempts"].([]any); !ok || len(att) != 0 {
		t.Fatalf("attempts = %v, want an empty array", body["attempts"])
	}

	rec = do(t, h, http.MethodGet, "/api/v1/jobs/"+games.NewID())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", rec.Code)
	}
	requireContentType(t, rec, ContentTypeProblem)
	for _, bad := range []string{"/api/v1/jobs/nope", "/api/v1/jobs/", "/api/v1/jobs/" + strings.Repeat("x", 300)} {
		if rec := do(t, h, http.MethodGet, bad); rec.Code != http.StatusNotFound {
			t.Fatalf("%s = %d, want 404 (body %s)", bad, rec.Code, rec.Body.String())
		}
	}
}

// TestJobAttemptsHistoryIsVisible proves the developer-facing evidence chain:
// claim → one running attempt; complete → succeeded attempt, in one GET.
func TestJobAttemptsHistoryIsVisible(t *testing.T) {
	h, js := jobsFixture(t)
	ctx := context.Background()
	job := seedJob(t, js, "import.parse")

	claimed, err := js.Claim(ctx, "w1", []string{"import.parse"}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim: %v", err)
	}
	_, body := getJSON(t, h, "/api/v1/jobs/"+job.ID)
	att, _ := body["attempts"].([]any)
	if len(att) != 1 {
		t.Fatalf("attempts = %v, want one running row", att)
	}
	first := att[0].(map[string]any)
	if first["attempt"] != float64(1) || first["outcome"] != "running" {
		t.Fatalf("attempt row = %v", first)
	}
	if _, busy := first["finished_at"]; busy {
		t.Fatalf("in-flight attempt must not carry finished_at: %v", first)
	}
	if body["lease_owner"] != "w1" {
		t.Fatalf("lease_owner = %v, want w1", body["lease_owner"])
	}

	if err := js.Complete(ctx, job.ID, json.RawMessage(`{"messages":7}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	_, body = getJSON(t, h, "/api/v1/jobs/"+job.ID)
	att, _ = body["attempts"].([]any)
	done := att[0].(map[string]any)
	if done["outcome"] != "succeeded" || done["finished_at"] == "" {
		t.Fatalf("finished attempt = %v", done)
	}
	if result, ok := body["result"].(map[string]any); !ok || result["messages"] != float64(7) {
		t.Fatalf("result = %v", body["result"])
	}
	if body["lease_owner"] != "" && body["lease_owner"] != nil {
		t.Fatalf("lease must clear on completion, got %v", body["lease_owner"])
	}
}

func TestJobCancelEndpoint(t *testing.T) {
	h, js := jobsFixture(t)
	job := seedJob(t, js, "import.parse")

	rec := do(t, h, http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel")
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["state"] != "cancelled" || body["id"] != job.ID {
		t.Fatalf("body = %v, want the cancelled job", body)
	}

	// Idempotent repeat.
	if rec := do(t, h, http.MethodPost, "/api/v1/jobs/"+job.ID+"/cancel"); rec.Code != http.StatusOK {
		t.Fatalf("second cancel = %d, want 200", rec.Code)
	}

	// A succeeded job cannot be cancelled: 409 problem+json.
	done, _, _ := js.Enqueue(context.Background(), store.EnqueueParams{Kind: "k"})
	if _, err := js.Claim(context.Background(), "w", []string{"k"}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if err := js.Complete(context.Background(), done.ID, nil); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodPost, "/api/v1/jobs/"+done.ID+"/cancel")
	if rec.Code != http.StatusConflict {
		t.Fatalf("cancel of succeeded = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	if !strings.Contains(rec.Body.String(), "succeeded") {
		t.Fatalf("conflict must name the current state, got %s", rec.Body.String())
	}

	if rec := do(t, h, http.MethodPost, "/api/v1/jobs/"+games.NewID()+"/cancel"); rec.Code != http.StatusNotFound {
		t.Fatalf("cancel unknown = %d, want 404", rec.Code)
	}
}

// claim takes the specific job by id, failing if the queue handed back
// something else — a shared kind across seeds would otherwise let these
// assertions race against whichever row the claimer happened to pick.
func claim(ctx context.Context, t *testing.T, js *store.JobsStore, kind, wantID string) {
	t.Helper()
	got, err := js.Claim(ctx, "w", []string{kind}, time.Minute)
	if err != nil || got == nil || got.ID != wantID {
		t.Fatalf("claim %s: got %+v (%v), want job %s", kind, got, err, wantID)
	}
}

// TestJobRetryOnlyFromFailedOrCancelled is the docs/spec/08 state rule: retry
// moves failed|cancelled to queued and answers 409 problem+json for every
// other state, so a UI cannot double-run work that is merely waiting.
func TestJobRetryOnlyFromFailedOrCancelled(t *testing.T) {
	h, js := jobsFixture(t)
	ctx := context.Background()

	queued := seedJob(t, js, "k")
	rec := do(t, h, http.MethodPost, "/api/v1/jobs/"+queued.ID+"/retry")
	if rec.Code != http.StatusConflict {
		t.Fatalf("retry of queued = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)

	running, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "k0", MaxAttempts: 3})
	claim(ctx, t, js, "k0", running.ID)
	if rec := do(t, h, http.MethodPost, "/api/v1/jobs/"+running.ID+"/retry"); rec.Code != http.StatusConflict {
		t.Fatalf("retry of running = %d, want 409", rec.Code)
	}

	succeeded, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "k1", MaxAttempts: 3})
	claim(ctx, t, js, "k1", succeeded.ID)
	if err := js.Complete(ctx, succeeded.ID, nil); err != nil {
		t.Fatal(err)
	}
	if rec := do(t, h, http.MethodPost, "/api/v1/jobs/"+succeeded.ID+"/retry"); rec.Code != http.StatusConflict {
		t.Fatalf("retry of succeeded = %d, want 409", rec.Code)
	}

	failed, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "k2", MaxAttempts: 1})
	// Claim the *known* id: kinds are shared with the jobs above, and the
	// claimer must not be handed one of theirs instead.
	if claim(ctx, t, js, "k2", failed.ID); false {
		return
	}
	if err := js.Fail(ctx, failed.ID, "boom", false); err != nil {
		t.Fatal(err)
	}
	rec = do(t, h, http.MethodPost, "/api/v1/jobs/"+failed.ID+"/retry")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry of failed = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	if body["state"] != "queued" || body["attempts"] != float64(0) {
		t.Fatalf("retried job = %v, want a fresh queued row", body)
	}

	cancelled, _ := js.Cancel(ctx, running.ID)
	rec = do(t, h, http.MethodPost, "/api/v1/jobs/"+cancelled.ID+"/retry")
	if rec.Code != http.StatusOK {
		t.Fatalf("retry of cancelled = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestJobsWritePathsRejectGet proves the route table treats jobs as
// POST-only for mutations: a GET on /cancel is a method error, not a cancel.
func TestJobsWritePathsRejectGet(t *testing.T) {
	h, js := jobsFixture(t)
	job := seedJob(t, js, "k")

	rec := do(t, h, http.MethodGet, "/api/v1/jobs/"+job.ID+"/cancel")
	if rec.Code == http.StatusOK {
		t.Fatal("GET on the cancel path must not cancel anything")
	}
	if body := jobStateForTest(t, js, job.ID); body != "queued" {
		t.Fatalf("state after rejected GET = %s, want still queued", body)
	}
}

func jobStateForTest(t *testing.T, js *store.JobsStore, id string) string {
	t.Helper()
	job, err := js.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return job.State
}
