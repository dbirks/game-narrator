package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

func enqueueKind(t *testing.T, js *JobsStore, kind string) *Job {
	t.Helper()
	job, created, err := js.Enqueue(testCtx(t), EnqueueParams{Kind: kind, MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue %s: %v", kind, err)
	}
	if !created {
		t.Fatalf("enqueue %s reported not-created without an idempotency key", kind)
	}
	return job
}

func TestJobsEnqueueDefaults(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	job, _, err := js.Enqueue(testCtx(t), EnqueueParams{
		Kind:    "import.parse",
		Payload: json.RawMessage(`{"file":"a.txt"}`),
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if job.State != JobQueued || job.Priority != 0 || job.MaxAttempts != 1 {
		t.Fatalf("defaults not applied: %+v", job)
	}
	if job.TraceID == "" || !games.ValidID(job.TraceID) {
		t.Fatalf("trace_id = %q, want a fresh id", job.TraceID)
	}
	if got := string(job.Payload); got != `{"file":"a.txt"}` {
		t.Fatalf("payload round-trip = %q", got)
	}

	got, err := js.GetJob(testCtx(t), job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != job.ID || got.Kind != "import.parse" {
		t.Fatalf("get returned %+v, want the enqueued row", got)
	}
}

func TestJobsQueueDepth(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	// Empty queue: nothing waiting and nothing in flight.
	if n, err := js.QueueDepth(ctx); err != nil || n != 0 {
		t.Fatalf("QueueDepth(empty) = %d, %v; want 0", n, err)
	}

	// Enqueue two: both are queued, so the depth is two.
	ja, _, err := js.Enqueue(ctx, EnqueueParams{Kind: "a", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue a: %v", err)
	}
	jb, _, err := js.Enqueue(ctx, EnqueueParams{Kind: "b", MaxAttempts: 3})
	if err != nil {
		t.Fatalf("enqueue b: %v", err)
	}
	if n, err := js.QueueDepth(ctx); err != nil || n != 2 {
		t.Fatalf("QueueDepth(two queued) = %d, %v; want 2", n, err)
	}

	// Claim one: it moves queued→running, but a running job still counts as
	// depth (waiting plus in flight).
	claimed, err := js.Claim(ctx, "worker-1", nil, time.Minute)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n, err := js.QueueDepth(ctx); err != nil || n != 2 {
		t.Fatalf("QueueDepth(one running, one queued) = %d, %v; want 2 (running still counts)", n, err)
	}

	// Complete the running one: one queued remains.
	if err := js.Complete(ctx, claimed.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if n, err := js.QueueDepth(ctx); err != nil || n != 1 {
		t.Fatalf("QueueDepth(after complete) = %d, %v; want 1 (the other is still queued)", n, err)
	}

	// Cancel the last queued one: terminal states (succeeded, cancelled) do
	// not count as depth, so the queue is empty.
	remaining := jb.ID
	if claimed.ID == jb.ID {
		remaining = ja.ID
	}
	if _, err := js.Cancel(ctx, remaining); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n, err := js.QueueDepth(ctx); err != nil || n != 0 {
		t.Fatalf("QueueDepth(terminal states) = %d, %v; want 0", n, err)
	}
}

func TestJobsEnqueueIdempotency(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	first, created, err := js.Enqueue(ctx, EnqueueParams{
		Kind: "voice.register", IdempotencyKey: "key-1", MaxAttempts: 3,
	})
	if err != nil || !created {
		t.Fatalf("first enqueue = created %v (%v)", created, err)
	}
	// The retried POST arrives with the same key (possibly replayed bytes):
	// it must see the stored job, not a second one (docs/spec/08).
	second, created, err := js.Enqueue(ctx, EnqueueParams{
		Kind: "voice.register", IdempotencyKey: "key-1", MaxAttempts: 9,
	})
	if err != nil {
		t.Fatalf("retried enqueue: %v", err)
	}
	if created {
		t.Fatal("retried idempotency key created a second job")
	}
	if second.ID != first.ID || second.MaxAttempts != 3 {
		t.Fatalf("retried enqueue returned %+v, want the stored job %s", second, first.ID)
	}

	// Keys NULL never collide (internal jobs have no client key).
	a := enqueueKind(t, js, "system.noop")
	b := enqueueKind(t, js, "system.noop")
	if a.ID == b.ID {
		t.Fatal("two keyless enqueues collided")
	}

	// Concurrent retried POSTs: the BEGIN IMMEDIATE claim means exactly one
	// insert wins.
	var (
		wg  sync.WaitGroup
		mu  sync.Mutex
		ids = map[string]int{}
	)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			j, _, err := js.Enqueue(ctx, EnqueueParams{Kind: "import.parse", IdempotencyKey: "race"})
			if err != nil {
				t.Errorf("racy enqueue: %v", err)
				return
			}
			mu.Lock()
			ids[j.ID]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	// The retried POSTs raced a fresh key ("race"), not the earlier "key-1"
	// job — what must be invariant is that all eight saw ONE job.
	if len(ids) != 1 {
		var n int
		for _, c := range ids {
			n += c
		}
		t.Fatalf("idempotency under concurrency produced %d distinct jobs (%d calls)", len(ids), n)
	}
	for _, c := range ids {
		if c != 8 {
			t.Fatalf("idempotency under concurrency = %v, want all 8 calls on one job", ids)
		}
	}
}

func TestJobsEnqueueValidation(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	if _, _, err := js.Enqueue(testCtx(t), EnqueueParams{Kind: " "}); err == nil {
		t.Fatal("blank kind accepted")
	}
	if _, _, err := js.Enqueue(testCtx(t), EnqueueParams{Kind: "k", Priority: 101}); err == nil {
		t.Fatal("priority 101 accepted (CHECK bounds 0..100)")
	}
	if _, _, err := js.Enqueue(testCtx(t),
		// Well-formed ULID, no such row: the FK is the arbiter here (the
		// shape check below rejects only syntactic junk).
		EnqueueParams{Kind: "k", ParentID: games.NewID()}); !errors.Is(err, ErrConflict) {
		t.Fatalf("bad parent id err = %v, want ErrConflict (FK)", err)
	}
}

func TestJobsClaimPriorityOrder(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	low, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "k", Priority: 1})
	// No sleep between the same-priority pair on purpose: at millisecond
	// resolution they normally share a created_at, so this is the tie the claim
	// order has to resolve by insertion order (`rowid`) rather than by the random
	// ULID suffix in `id`. If the clock does tick over, created_at alone gives the
	// same answer — either way the expectation below is deterministic.
	high, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "k", Priority: 5})
	later, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "k", Priority: 5})

	for _, want := range []*Job{high, later, low} {
		got, err := js.Claim(ctx, "w1", []string{"k"}, time.Minute)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if got == nil {
			t.Fatal("claim found nothing while the queue had work")
		}
		if got.ID != want.ID {
			t.Fatalf("claim order = %s, want %s (priority DESC then oldest)", got.ID, want.ID)
		}
		if got.State != JobRunning || got.Attempts != 1 || got.LeaseOwner != "w1" || got.LeaseExpiresAt.IsZero() {
			t.Fatalf("claimed row not leased+running: %+v", got)
		}
	}
	if none, err := js.Claim(ctx, "w1", []string{"k"}, time.Minute); err != nil || none != nil {
		t.Fatalf("claim on empty kinds = (%v, %v), want (nil, nil)", none, err)
	}
}

func TestJobsClaimRespectsRunAfterAndKinds(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	_, _, _ = js.Enqueue(ctx, EnqueueParams{Kind: "later", RunAfter: time.Now().UTC().Add(time.Hour)})
	_, _, _ = js.Enqueue(ctx, EnqueueParams{Kind: "other"})

	if got, err := js.Claim(ctx, "w", []string{"later"}, time.Minute); err != nil || got != nil {
		t.Fatalf("claim of a backoff-pending job = (%v, %v), want nil", got, err)
	}
	got, err := js.Claim(ctx, "w", nil, time.Minute) // nil kinds = any registered kind
	if err != nil || got == nil || got.Kind != "other" {
		t.Fatalf("kindless claim = %+v (%v), want the only runnable job", got, err)
	}
}

func TestJobsClaimConcurrentDisjoint(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)
	const n = 40
	ids := map[string]bool{}
	for i := 0; i < n; i++ {
		j, _, err := js.Enqueue(ctx, EnqueueParams{Kind: "k", MaxAttempts: 2})
		if err != nil {
			t.Fatalf("enqueue: %v", err)
		}
		ids[j.ID] = true
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		seen = map[string]int{}
	)
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for {
				job, err := js.Claim(ctx, fmt.Sprintf("w%d", w), []string{"k"}, time.Minute)
				if err != nil {
					t.Errorf("claim: %v", err)
					return
				}
				if job == nil {
					return
				}
				mu.Lock()
				seen[job.ID]++
				mu.Unlock()
			}
		}(w)
	}
	wg.Wait()

	if len(seen) != n {
		t.Fatalf("%d jobs claimed, want %d — claims are not disjoint or the queue lost work", len(seen), n)
	}
	for id, c := range seen {
		if c != 1 {
			t.Fatalf("job %s claimed %d times, want exactly once (BEGIN IMMEDIATE compare-and-swap)", id, c)
		}
	}
}

func TestJobsHeartbeatCancelAndComplete(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)
	job := enqueueKind(t, js, "agent.run")

	claimed, err := js.Claim(ctx, "w1", []string{"agent.run"}, time.Minute)
	if err != nil || claimed.ID != job.ID {
		t.Fatalf("claim = %v (%v)", claimed, err)
	}
	state, err := js.Heartbeat(ctx, job.ID, "w1", time.Minute)
	if err != nil || state != JobRunning {
		t.Fatalf("heartbeat = %q (%v), want running", state, err)
	}

	// Out-of-band cancel (docs/spec/08 POST /jobs/:id/cancel against a
	// running job): the worker learns about it at its next heartbeat.
	if _, err := js.Cancel(ctx, job.ID); err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	state, err = js.Heartbeat(ctx, job.ID, "w1", time.Minute)
	if err != nil {
		t.Fatalf("heartbeat after cancel: %v", err)
	}
	if state != JobCancelled {
		t.Fatalf("heartbeat after cancel = %q, want cancelled", state)
	}

	// Cancel is idempotent; Complete after cancel conflicts — the job was
	// cancelled, not succeeded.
	again, err := js.Cancel(ctx, job.ID)
	if err != nil || again.State != JobCancelled {
		t.Fatalf("second cancel = %v (%v)", again, err)
	}
	if err := js.Complete(ctx, job.ID, json.RawMessage(`{"ok":true}`)); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete-after-cancel err = %v, want ErrConflict", err)
	}
	if err := js.Complete(ctx, job.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("complete-twice err = %v, want ErrConflict", err)
	}

	// A heartbeat against a settled job reports its state and extends
	// nothing — even from a stranger owner, because the answer is the same
	// "stop" either way.
	state, err = js.Heartbeat(ctx, job.ID, "not-the-owner", time.Minute)
	if err != nil || state != JobCancelled {
		t.Fatalf("wrong-owner heartbeat on cancelled job = %q (%v), want cancelled", state, err)
	}
}

func TestJobsLeaseExpiryTakeover(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)
	job := enqueueKind(t, js, "import.parse")

	first, err := js.Claim(ctx, "w1", []string{"import.parse"}, 60*time.Millisecond)
	if err != nil || first == nil {
		t.Fatalf("claim: %v", err)
	}
	time.Sleep(80 * time.Millisecond)

	second, err := js.Claim(ctx, "w2", []string{"import.parse"}, time.Minute)
	if err != nil || second == nil || second.ID != job.ID {
		t.Fatalf("takeover claim = %+v (%v), want the expired job", second, err)
	}
	if second.Attempts != 2 || second.LeaseOwner != "w2" {
		t.Fatalf("takeover row = %+v, want attempt 2 owned by w2", second)
	}

	// The crashed attempt is kept as evidence. Finalization is state-gated:
	// see the Complete replay guard below.
	atts, err := js.ListJobAttempts(ctx, job.ID)
	if err != nil || len(atts) != 2 {
		t.Fatalf("attempts = %v (%v), want 2 rows", atts, err)
	}
	if atts[0].Outcome != AttemptCrashed || atts[1].Outcome != AttemptRunning {
		t.Fatalf("attempt outcomes = %v/%v, want crashed then running", atts[0].Outcome, atts[1].Outcome)
	}
	if _, err := js.Heartbeat(ctx, job.ID, "w1", time.Minute); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale heartbeat = %v, want ErrConflict (lease lost)", err)
	}
	// The state machine, not the caller, decides: exactly one Complete can
	// ever transition a running job; a replayed finalize is ErrConflict —
	// that is what stops two workers whose leases overlapped from both
	// reporting victory.
	if err := js.Complete(ctx, job.ID, json.RawMessage(`{}`)); err != nil {
		t.Fatalf("complete by the current state-holder: %v", err)
	}
	if err := js.Complete(ctx, job.ID, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("second complete = %v, want ErrConflict", err)
	}
}

func TestJobsFailRetryBackoffThenSuccess(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)
	job := enqueueKind(t, js, "k") // MaxAttempts 3

	if _, err := js.Claim(ctx, "w", []string{"k"}, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := js.Fail(ctx, job.ID, "provider 502", true); err != nil {
		t.Fatalf("fail: %v", err)
	}
	fresh, err := js.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if fresh.State != JobQueued || fresh.Attempts != 1 {
		t.Fatalf("after retryable fail = %s/%d, want queued/1", fresh.State, fresh.Attempts)
	}
	// First backoff step: base 2 s with half jitter → [1 s, 2 s].
	delta := time.Until(fresh.RunAfter)
	if delta < 900*time.Millisecond || delta > 2100*time.Millisecond {
		t.Fatalf("backoff run_after = %v, want ~1-2 s", delta)
	}
	if now, err := js.Claim(ctx, "w", []string{"k"}, time.Minute); err != nil || now != nil {
		t.Fatalf("claim inside the backoff window = (%v, %v), want nil", now, err)
	}

	// Let the backoff expire by pushing run_after into the past, then succeed.
	if _, err := js.s.db.ExecContext(ctx,
		"UPDATE jobs SET run_after = ? WHERE id = ?", stampMS(time.Now().UTC().Add(-time.Second)), job.ID); err != nil {
		t.Fatalf("age run_after: %v", err)
	}
	claimed, err := js.Claim(ctx, "w", []string{"k"}, time.Minute)
	if err != nil || claimed == nil || claimed.Attempts != 2 {
		t.Fatalf("re-claim = %+v (%v), want attempt 2", claimed, err)
	}
	if err := js.Complete(ctx, job.ID, json.RawMessage(`{"done":true}`)); err != nil {
		t.Fatalf("complete: %v", err)
	}
	fresh, _ = js.GetJob(ctx, job.ID)
	if fresh.State != JobSucceeded || string(fresh.Result) != `{"done":true}` {
		t.Fatalf("final row = %+v, want succeeded with result", fresh)
	}
	atts, _ := js.ListJobAttempts(ctx, job.ID)
	if len(atts) != 2 || atts[0].Outcome != AttemptFailed || atts[0].Error != "provider 502" ||
		atts[1].Outcome != AttemptSucceeded {
		t.Fatalf("attempt audit trail wrong: %+v", atts)
	}
}

func TestJobsFailPermanentAndBudget(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	perm, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "k", MaxAttempts: 5})
	_, _ = js.Claim(ctx, "w", []string{"k"}, time.Minute)
	if err := js.Fail(ctx, perm.ID, "unparseable source", false); err != nil {
		t.Fatalf("permanent fail: %v", err)
	}
	fresh, _ := js.GetJob(ctx, perm.ID)
	if fresh.State != JobFailed {
		t.Fatalf("permanent failure state = %s, want failed despite remaining budget", fresh.State)
	}
	if len(fresh.Result) == 0 || !json.Valid(fresh.Result) {
		t.Fatalf("failed job lost its structured error: %q", fresh.Result)
	}

	// Budget exhaustion: three allowed attempts, all retryable.
	budget, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "k", MaxAttempts: 3})
	for i := 1; i <= 3; i++ {
		if _, err := js.s.db.ExecContext(ctx, "UPDATE jobs SET run_after = NULL WHERE id = ?", budget.ID); err != nil {
			t.Fatalf("clear backoff: %v", err)
		}
		job, err := js.Claim(ctx, "w", []string{"k"}, time.Minute)
		if err != nil || job == nil || job.ID != budget.ID {
			t.Fatalf("attempt %d claim = %+v (%v)", i, job, err)
		}
		if err := js.Fail(ctx, budget.ID, fmt.Sprintf("boom %d", i), true); err != nil {
			t.Fatalf("fail %d: %v", i, err)
		}
	}
	fresh, _ = js.GetJob(ctx, budget.ID)
	if fresh.State != JobFailed || fresh.Attempts != 3 {
		t.Fatalf("exhausted job = %s/%d, want failed/3", fresh.State, fresh.Attempts)
	}
}

func TestJobsCancelQueuedAndRetry(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)
	job := enqueueKind(t, js, "k")

	if got, err := js.Cancel(ctx, job.ID); err != nil || got.State != JobCancelled {
		t.Fatalf("cancel queued = %+v (%v)", got, err)
	}
	if _, err := js.Cancel(ctx, job.ID); err != nil {
		t.Fatalf("idempotent cancel failed: %v", err)
	}
	if _, err := js.Retry(ctx, job.ID); err != nil {
		t.Fatalf("retry cancelled job: %v", err)
	}
	fresh, _ := js.GetJob(ctx, job.ID)
	if fresh.State != JobQueued || fresh.Attempts != 0 || !fresh.RunAfter.IsZero() {
		t.Fatalf("after retry = %+v, want fresh queued", fresh)
	}

	// Retry only applies to failed|cancelled; a running job answers 409 at
	// the API because of this ErrConflict.
	_, _ = js.Claim(ctx, "w", []string{"k"}, time.Minute)
	if _, err := js.Retry(ctx, job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("retry of running job = %v, want ErrConflict", err)
	}
	if err := js.Complete(ctx, job.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := js.Cancel(ctx, job.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("cancel of succeeded job = %v, want ErrConflict", err)
	}
}

// TestJobsRecoverOnStartup is the docs/spec/02 "Transaction workflows" rule —
// interrupted `running` jobs move to `recoverable` and only *resumable* kinds
// are re-queued — expressed as a test rather than a wish.
func TestJobsRecoverOnStartup(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	resumable, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "import.parse", MaxAttempts: 3})
	if _, err := js.Claim(ctx, "dead-worker", []string{"import.parse"}, time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	untrusted, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "onboarding.run", MaxAttempts: 3})
	if _, err := js.Claim(ctx, "dead-worker", []string{"onboarding.run"}, time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}

	report, err := js.RecoverOnStartup(ctx, []string{"import.parse"})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(report.Marked) != 2 {
		t.Fatalf("marked = %v, want both running jobs", report.Marked)
	}
	if len(report.Requeued) != 1 || report.Requeued[0] != resumable.ID {
		t.Fatalf("requeued = %v, want only the resumable kind", report.Requeued)
	}

	rJob, _ := js.GetJob(ctx, resumable.ID)
	if rJob.State != JobQueued || rJob.Attempts != 1 {
		t.Fatalf("requeued job = %+v, want queued keeping the spent attempt (budget still has 2)", rJob)
	}
	uJob, _ := js.GetJob(ctx, untrusted.ID)
	if uJob.State != JobRecoverable {
		t.Fatalf("non-resumable job = %s, want recoverable for the reconciler", uJob.State)
	}
	atts, _ := js.ListJobAttempts(ctx, resumable.ID)
	if len(atts) != 1 || atts[0].Outcome != AttemptCrashed {
		t.Fatalf("attempt evidence = %+v, want one crashed row", atts)
	}
	// The reclaim is not a free pass: a *second* crash still ends within the
	// budget.
	if _, err := js.Claim(ctx, "w", []string{"import.parse"}, time.Hour); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	if err := js.Fail(ctx, resumable.ID, "again", true); err != nil {
		t.Fatalf("fail: %v", err)
	}
	again, _ := js.GetJob(ctx, resumable.ID)
	if again.State != JobQueued || again.Attempts != 2 {
		t.Fatalf("second fail = %+v, want queued at attempt 2", again)
	}
}

func TestJobsRecoverRespectsRetryBudget(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)

	// A job that crashed on its only permitted attempt must not be requeued:
	// the CHECK `attempts <= max_attempts` forbids the claim that would
	// follow, so recovery leaves it recoverable instead of handing a worker
	// an unclaimable row.
	job, _, _ := js.Enqueue(ctx, EnqueueParams{Kind: "import.parse", MaxAttempts: 1})
	if _, err := js.Claim(ctx, "dead", []string{"import.parse"}, time.Hour); err != nil {
		t.Fatalf("claim: %v", err)
	}
	report, err := js.RecoverOnStartup(ctx, []string{"import.parse"})
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(report.Requeued) != 0 {
		t.Fatalf("requeued %v, want none — the budget is spent", report.Requeued)
	}
	fresh, _ := js.GetJob(ctx, job.ID)
	if fresh.State != JobRecoverable {
		t.Fatalf("state = %s, want recoverable (a human decides next)", fresh.State)
	}
}

func TestJobsListPaginationAndFilters(t *testing.T) {
	js := NewJobsStore(migratedStore(t))
	ctx := testCtx(t)
	for i := 0; i < 12; i++ {
		kind := "k.a"
		if i%3 == 0 {
			kind = "k.b"
		}
		_, _, _ = js.Enqueue(ctx, EnqueueParams{Kind: kind})
	}

	var (
		seen   []Job
		cursor string
	)
	for page := 0; page < 10; page++ {
		batch, next, err := js.ListJobs(ctx, JobFilter{}, cursor, 5)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		seen = append(seen, batch...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 12 {
		t.Fatalf("walked %d jobs, want 12 exactly once", len(seen))
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].ID <= seen[i-1].ID {
			t.Fatal("list not ordered by id")
		}
	}

	onlyB, _, err := js.ListJobs(ctx, JobFilter{Kind: "k.b"}, "", 100)
	if err != nil || len(onlyB) != 4 {
		t.Fatalf("kind filter = %d (%v), want 4", len(onlyB), err)
	}
	running, _, err := js.ListJobs(ctx, JobFilter{State: JobRunning}, "", 100)
	if err != nil || len(running) != 0 {
		t.Fatalf("state filter = %d (%v), want 0", len(running), err)
	}
	if _, _, err := js.ListJobs(ctx, JobFilter{State: "exploded"}, "", 10); err == nil {
		t.Fatal("unknown state filter accepted")
	}
}
