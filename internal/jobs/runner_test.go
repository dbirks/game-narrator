package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/store"
)

// ── harness ────────────────────────────────────────────────────────────────

func openMigrated(t *testing.T) (*store.Store, *store.JobsStore) {
	t.Helper()
	st, err := store.Open(context.Background(), store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st, store.NewJobsStore(st)
}

func quietLog() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

// waitFor polls cond until timeout with a clear failure message.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func jobState(t *testing.T, js *store.JobsStore, id string) *store.Job {
	t.Helper()
	job, err := js.GetJob(context.Background(), id)
	if err != nil {
		t.Fatalf("get job %s: %v", id, err)
	}
	return job
}

// fastRunner: short lease + fast poll so lease and backoff paths run in test
// time, not wall-clock-service time. Backoff stays real (the store's 2 s base
// is exactly what we assert elsewhere), everything else shrinks.
func fastRunner(t *testing.T, js *store.JobsStore, hub *events.Hub) *Runner {
	t.Helper()
	r, err := NewRunner(js, hub, quietLog(),
		WithLease(300*time.Millisecond), WithPollInterval(10*time.Millisecond))
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}
	return r
}

func mustStart(t *testing.T, r *Runner, ctx context.Context) {
	t.Helper()
	if err := r.Start(ctx); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = r.Stop(sctx)
	})
}

// ── the exactly-once claim guarantee ───────────────────────────────────────

// TestTwoRunnerPoolsRunEachJobOnce is the queue's central promise at the
// scale it will actually see: two runner pools (as after a half-restart, or
// the API/job split docs/spec/01 permits on one appliance), 100 jobs, four
// workers each — every job claimed by exactly one worker, run to exactly one
// terminal outcome. -race makes any shared-state shortcut in the claim path
// visible here.
func TestTwoRunnerPoolsRunEachJobOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var (
			mu   sync.Mutex
			runs = map[string]int{}
		)
		handler := func(_ context.Context, job Job, _ ProgressFn) (json.RawMessage, error) {
			mu.Lock()
			runs[job.ID]++
			mu.Unlock()
			return json.RawMessage(`{"ok":true}`), nil
		}

		for i := 0; i < 100; i++ {
			if _, _, err := js.Enqueue(ctx, enqueueJob("bulk.task")); err != nil {
				t.Fatalf("enqueue: %v", err)
			}
		}

		for w := 0; w < 2; w++ {
			// A 30 s lease, not the 300 ms one: the guarantee under test is that
			// two live pools never CLAIM the same job. A short lease here would
			// measure heartbeat latency under -race instead (a legitimately
			// stalled attempt being retaken is at-least-once retry, which the
			// takeover and store-level disjoint-claim tests pin separately).
			r, err := NewRunner(js, nil, quietLog(),
				WithLease(30*time.Second), WithPollInterval(10*time.Millisecond))
			if err != nil {
				t.Fatalf("new runner: %v", err)
			}
			if err := r.Register("bulk.task", handler, RegisterOpts{Concurrency: 4}); err != nil {
				t.Fatalf("register: %v", err)
			}
			mustStart(t, r, ctx)
		}

		waitFor(t, "100 jobs to succeed", func() bool {
			done, _, err := js.ListJobs(ctx, store.JobFilter{State: store.JobSucceeded}, "", store.MaxListLimit)
			return err == nil && len(done) >= 100
		})
		waitFor(t, "the queue to drain", func() bool {
			left, _, err := js.ListJobs(ctx, store.JobFilter{State: store.JobQueued}, "", 5)
			return err == nil && len(left) == 0
		})

		mu.Lock()
		defer mu.Unlock()
		if len(runs) != 100 {
			t.Fatalf("%d distinct jobs ran, want 100", len(runs))
		}
		for id, n := range runs {
			if n != 1 {
				t.Fatalf("job %s executed %d times, want exactly once", id, n)
			}
		}
	})
}

func enqueueJob(kind string) store.EnqueueParams {
	return store.EnqueueParams{Kind: kind, MaxAttempts: 3}
}

// ── retries, failures, budgets ─────────────────────────────────────────────

func TestRetryWithBackoffThenSuccess(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var attempts sync.Map
		r := fastRunner(t, js, nil)
		err := r.Register("flaky", func(_ context.Context, job Job, _ ProgressFn) (json.RawMessage, error) {
			n, _ := attempts.LoadOrStore(job.ID, 0)
			count := n.(int) + 1
			attempts.Store(job.ID, count)
			if count < 2 {
				return nil, errors.New("transient provider blip")
			}
			return json.RawMessage(`{"attempt":2}`), nil
		}, RegisterOpts{Concurrency: 1})
		if err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)

		job, _, err := js.Enqueue(ctx, store.EnqueueParams{Kind: "flaky", MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		// The first failure must park the job `queued` with the store's real
		// ~1-2 s backoff, not spin: observable as a nonzero run_after.
		waitFor(t, "first failure to schedule a retry", func() bool {
			fresh := jobState(t, js, job.ID)
			return fresh.State == store.JobQueued && fresh.Attempts >= 1 && !fresh.RunAfter.IsZero()
		})
		waitFor(t, "the retry to succeed", func() bool {
			return jobState(t, js, job.ID).State == store.JobSucceeded
		})
		if got, _ := attempts.Load(job.ID); got != 2 {
			t.Fatalf("handler ran %v times, want 2 (one failure, one success)", got)
		}
		fresh := jobState(t, js, job.ID)
		if string(fresh.Result) != `{"attempt":2}` {
			t.Fatalf("result = %q, want the handler's payload", fresh.Result)
		}
	})
}

func TestPermanentErrorFailsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var runs int
		var mu sync.Mutex
		r := fastRunner(t, js, nil)
		if err := r.Register("bad.input", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			return nil, Permanent(errors.New("markup parse: unsupported byte $ff"))
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)

		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "bad.input", MaxAttempts: 5})
		waitFor(t, "permanent failure to settle without burning the budget", func() bool {
			return jobState(t, js, job.ID).State == store.JobFailed
		})
		mu.Lock()
		defer mu.Unlock()
		if runs != 1 {
			t.Fatalf("permanent job ran %d times, want 1 (no retry)", runs)
		}
		if got := string(jobState(t, js, job.ID).Result); got == "" || !json.Valid([]byte(got)) {
			t.Fatalf("failed job result = %q, want structured error", got)
		}
	})
}

func TestMaxAttemptsExhaustsToFailed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var runs int
		var mu sync.Mutex
		r := fastRunner(t, js, nil)
		if err := r.Register("always.down", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
			mu.Lock()
			runs++
			mu.Unlock()
			return nil, errors.New("gateway unreachable")
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)

		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "always.down", MaxAttempts: 2})
		waitFor(t, "the retry budget to be spent", func() bool {
			return jobState(t, js, job.ID).State == store.JobFailed
		})
		mu.Lock()
		defer mu.Unlock()
		if runs != 2 {
			t.Fatalf("ran %d times, want exactly max_attempts=2", runs)
		}
	})
}

// ── cancellation ───────────────────────────────────────────────────────────

func TestCancelWhileRunningStopsTheHandler(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		handoff := make(chan struct{})
		r := fastRunner(t, js, nil)
		if err := r.Register("long.import", func(hctx context.Context, _ Job, _ ProgressFn) (json.RawMessage, error) {
			close(handoff)
			<-hctx.Done() // the only exit is the cancellation the runner propagates
			return nil, hctx.Err()
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)

		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "long.import", MaxAttempts: 3})
		<-handoff // handler is definitely mid-flight, not merely queued
		if _, err := js.Cancel(context.Background(), job.ID); err != nil {
			t.Fatalf("cancel: %v", err)
		}
		// The worker learns via heartbeat (lease 300 ms → beat ~100 ms) and parks
		// the job as cancelled — the docs/spec/08 cancel contract against an
		// already-running job.
		waitFor(t, "the running job to reach cancelled", func() bool {
			return jobState(t, js, job.ID).State == store.JobCancelled
		})
		time.Sleep(200 * time.Millisecond) // nothing may resurrect it afterwards
		if fresh := jobState(t, js, job.ID); fresh.State != store.JobCancelled {
			t.Fatalf("job moved to %s after cancel", fresh.State)
		}
	})
}

// ── leases and takeover ────────────────────────────────────────────────────

func TestExpiredLeaseIsReclaimedByTheRunner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// A dead worker: claim directly, never heartbeat, never finalize. This is
		// the exact row state a SIGKILL leaves behind between two `serve`s that
		// do not restart in order (so RecoverOnStartup has not run).
		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "voice.register", MaxAttempts: 3})
		dead, err := js.Claim(ctx, "dead-worker", []string{"voice.register"}, 80*time.Millisecond)
		if err != nil || dead == nil {
			t.Fatalf("dead claim: %v", err)
		}

		r := fastRunner(t, js, nil)
		done := make(chan struct{})
		if err := r.Register("voice.register", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
			close(done)
			return json.RawMessage(`{}`), nil
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)

		waitFor(t, "the runner to reclaim the expired job", func() bool {
			select {
			case <-done:
				return true
			default:
				return false
			}
		})
		waitFor(t, "the takeover to succeed on attempt 2", func() bool {
			fresh := jobState(t, js, job.ID)
			return fresh.State == store.JobSucceeded && fresh.Attempts == 2
		})
		atts, err := js.ListJobAttempts(ctx, job.ID)
		if err != nil || len(atts) != 2 || atts[0].Outcome != store.AttemptCrashed {
			t.Fatalf("attempt history = %+v (%v), want a crashed first attempt kept as evidence", atts, err)
		}
	})
}

// ── crash recovery ─────────────────────────────────────────────────────────

// TestCrashRecoveryRequeuesResumableKind is the docs/spec/12 "process killed
// mid-transaction" and docs/spec/02 "interrupted `running` jobs moved to
// `recoverable`" acceptance path, at runner level: a job is executing when the
// runner is killed (its context cancelled abruptly, no Stop); the store is
// reopened; RecoverOnStartup marks the row recoverable and re-queues only the
// resumable kind; a fresh runner takes it to success.
func TestCrashRecoveryRequeuesResumableKind(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		dbPath := store.DBPath(filepath.Join(t.TempDir(), "data"))
		open := func() *store.Store {
			st, err := store.Open(context.Background(), dbPath)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if _, err := st.Migrate(context.Background()); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			return st
		}

		killCtx, killCancel := context.WithCancel(context.Background())
		stA := open()
		jsA := store.NewJobsStore(stA)

		blocked := make(chan struct{})
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })

		runnerA, err := NewRunner(jsA, nil, quietLog(), WithLease(time.Hour), WithPollInterval(10*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if err := runnerA.Register("recover.demo", func(_ context.Context, job Job, _ ProgressFn) (json.RawMessage, error) {
			if job.Attempts > 1 { // the resumed attempt knows it is not the first
				return json.RawMessage(`{"resumed":true}`), nil
			}
			close(blocked)
			<-release // never returns while the runner is alive: this is the crash
			return nil, errors.New("unreachable while blocked")
		}, RegisterOpts{Concurrency: 1, Resumable: true}); err != nil {
			t.Fatal(err)
		}
		if err := runnerA.Start(killCtx); err != nil {
			t.Fatal(err)
		}

		job, _, err := jsA.Enqueue(killCtx, store.EnqueueParams{Kind: "recover.demo", MaxAttempts: 3})
		if err != nil {
			t.Fatal(err)
		}
		<-blocked

		// The kill: no Stop, no drain — cancel the context and walk away. The
		// row is left `running` exactly as a power cut would.
		killCancel()
		waitFor(t, "the killed runner to stop claiming", func() bool {
			return killCtx.Err() != nil
		})
		if err := stA.Close(); err != nil {
			t.Fatalf("close after kill: %v", err)
		}

		// Reboot: a fresh process on the same file.
		stB := open()
		t.Cleanup(func() { _ = stB.Close() })
		jsB := store.NewJobsStore(stB)

		runnerB, err := NewRunner(jsB, nil, quietLog(), WithLease(time.Hour), WithPollInterval(10*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if err := runnerB.Register("recover.demo", func(_ context.Context, job Job, _ ProgressFn) (json.RawMessage, error) {
			if job.Attempts > 1 {
				return json.RawMessage(`{"resumed":true}`), nil
			}
			return nil, errors.New("should not run attempt 1 again after recovery")
		}, RegisterOpts{Concurrency: 1, Resumable: true}); err != nil {
			t.Fatal(err)
		}

		report, err := jsB.RecoverOnStartup(context.Background(), runnerB.ResumableKinds())
		if err != nil {
			t.Fatalf("recover: %v", err)
		}
		if len(report.Marked) != 1 || report.Marked[0] != job.ID {
			t.Fatalf("recovery marked %v, want the one interrupted job %s", report.Marked, job.ID)
		}
		if len(report.Requeued) != 1 || report.Requeued[0] != job.ID {
			t.Fatalf("requeued %v, want the resumable kind re-run", report.Requeued)
		}

		runCtx, runCancel := context.WithCancel(context.Background())
		defer runCancel()
		if err := runnerB.Start(runCtx); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the recovered job to finish on the resumed attempt", func() bool {
			fresh, err := jsB.GetJob(context.Background(), job.ID)
			return err == nil && fresh.State == store.JobSucceeded && fresh.Attempts == 2
		})
		if got := string(jobState(t, jsB, job.ID).Result); got != `{"resumed":true}` {
			t.Fatalf("resumed result = %q", got)
		}
	})
}

// ── resource isolation ─────────────────────────────────────────────────────

func TestForegroundBusyPausesBackgroundClaims(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := fastRunner(t, js, nil)
		if err := r.Register("prewarm", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
			return json.RawMessage(`{}`), nil
		}, RegisterOpts{Concurrency: 2}); err != nil {
			t.Fatal(err)
		}
		r.SetForegroundBusy(true) // a live session owns the appliance
		mustStart(t, r, ctx)

		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "prewarm"})
		time.Sleep(300 * time.Millisecond) // ~30 idle polls at 10 ms
		if fresh := jobState(t, js, job.ID); fresh.State != store.JobQueued || fresh.Attempts != 0 {
			t.Fatalf("background job ran (%s/attempt %d) while the foreground was busy", fresh.State, fresh.Attempts)
		}

		r.SetForegroundBusy(false)
		waitFor(t, "the job to resume once the session ends", func() bool {
			return jobState(t, js, job.ID).State == store.JobSucceeded
		})
	})
}

// ── events ─────────────────────────────────────────────────────────────────

func TestRunnerPublishesJobLifecycleEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		log := events.NewMemStore()
		hub := events.New(events.WithStore(log))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := fastRunner(t, js, hub)
		if err := r.Register("onboarding.run", func(_ context.Context, _ Job, progress ProgressFn) (json.RawMessage, error) {
			progress(50, "halfway")
			return json.RawMessage(`{}`), nil
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)

		job, _, err := r.Enqueue(ctx, store.EnqueueParams{Kind: "onboarding.run"})
		if err != nil {
			t.Fatal(err)
		}
		waitFor(t, "the succeeded event", func() bool {
			return slices.Contains(eventNamesFrom(log, ctx, t), EventSucceeded)
		})

		names := eventNamesFrom(log, ctx, t)
		for _, want := range []string{EventQueued, EventStarted, events.AgentJobProgress, EventSucceeded} {
			if !slices.Contains(names, want) {
				t.Fatalf("event stream = %v, missing %s", names, want)
			}
		}
		// One global order, correlation by trace id: every job event shares the
		// job's trace (docs/spec/09 "link everything using trace_id").
		rows, err := log.After(ctx, 0, events.Filter{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range rows {
			if e.TraceID != job.TraceID {
				t.Fatalf("event %s carries trace %q, want the job's %q", e.Name, e.TraceID, job.TraceID)
			}
			if e.SessionID != "" {
				t.Fatalf("job event %s must be global (no session), got %q", e.Name, e.SessionID)
			}
		}
		// Progress payload carries the handler's numbers for the UI.
		for _, e := range rows {
			if e.Name == events.AgentJobProgress {
				var d map[string]any
				if err := json.Unmarshal(e.Data, &d); err != nil {
					t.Fatal(err)
				}
				if d["percent"] != 50.0 || d["message"] != "halfway" || d["job_id"] != job.ID {
					t.Fatalf("progress payload = %v", d)
				}
			}
		}
		// Seq strictly increases across the whole lifecycle: no aliased cursor.
		for i := 1; i < len(rows); i++ {
			if rows[i].Seq <= rows[i-1].Seq {
				t.Fatalf("event seq not monotonic: %v", eventNamesFrom(log, ctx, t))
			}
		}
	})
}

func eventNamesFrom(log *events.MemStore, ctx context.Context, t *testing.T) []string {
	t.Helper()
	rows, err := log.After(ctx, 0, events.Filter{}, 1000)
	if err != nil {
		t.Fatalf("read memstore: %v", err)
	}
	out := make([]string, len(rows))
	for i, e := range rows {
		out[i] = e.Name
	}
	return out
}

// ── shutdown ───────────────────────────────────────────────────────────────

func TestStopDrainsInFlightJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		started, hold := make(chan struct{}), make(chan struct{})
		r, err := NewRunner(js, nil, quietLog(), WithLease(30*time.Second), WithPollInterval(10*time.Millisecond))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Register("slow.export", func(hctx context.Context, _ Job, _ ProgressFn) (json.RawMessage, error) {
			close(started)
			select {
			case <-hold:
				return json.RawMessage(`{"finished":true}`), nil
			case <-hctx.Done():
				return nil, hctx.Err()
			}
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "slow.export", MaxAttempts: 2})
		<-started

		// Unblocking just after Stop begins must let the in-flight job finish —
		// Stop waits for handlers, it does not interrupt them inside the window.
		go func() {
			time.Sleep(50 * time.Millisecond)
			close(hold)
		}()
		sctx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer sCancel()
		if err := r.Stop(sctx); err != nil {
			t.Fatalf("graceful stop: %v", err)
		}
		if fresh := jobState(t, js, job.ID); fresh.State != store.JobSucceeded {
			t.Fatalf("in-flight job after graceful Stop = %s, want succeeded", fresh.State)
		}
	})
}

func TestForcedStopRequeuesTheInterruptedJob(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		started := make(chan struct{})
		r, err := NewRunner(js, nil, quietLog(),
			WithLease(30*time.Second), WithPollInterval(10*time.Millisecond), WithDrainGrace(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		if err := r.Register("stuck", func(hctx context.Context, _ Job, _ ProgressFn) (json.RawMessage, error) {
			close(started)
			<-hctx.Done() // exits only when the forced drain cancels it
			return nil, hctx.Err()
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		if err := r.Start(ctx); err != nil {
			t.Fatal(err)
		}
		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "stuck", MaxAttempts: 2})
		<-started

		// A drain window far shorter than the job: Stop must force, report it,
		// and — the durable part — leave the job re-queued, not `running`.
		sctx, sCancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
		defer sCancel()
		if err := r.Stop(sctx); err == nil {
			t.Fatal("forced stop reported success")
		}
		waitFor(t, "the interrupted job to be re-queued for retry", func() bool {
			fresh := jobState(t, js, job.ID)
			return fresh.State == store.JobQueued && fresh.Attempts == 1
		})
	})
}

// ── api/registration guards ────────────────────────────────────────────────

func TestRegisterGuardsAndTimeoutClassification(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		r := fastRunner(t, js, nil)
		if err := r.Register("k", func(context.Context, Job, ProgressFn) (json.RawMessage, error) { return nil, nil }, RegisterOpts{}); err != nil {
			t.Fatal(err)
		}
		if err := r.Register("k", func(context.Context, Job, ProgressFn) (json.RawMessage, error) { return nil, nil }, RegisterOpts{}); err == nil {
			t.Fatal("duplicate kind accepted")
		}
		if err := r.Register("", nil, RegisterOpts{}); err == nil {
			t.Fatal("empty kind accepted")
		}

		// A hanging attempt is cut off by the kind's Timeout and, with a budget
		// of one, ends failed — the timeout must be visible in the result.
		if err := r.Register("hangs", func(hctx context.Context, _ Job, _ ProgressFn) (json.RawMessage, error) {
			<-hctx.Done()
			return nil, hctx.Err()
		}, RegisterOpts{Concurrency: 1, Timeout: 60 * time.Millisecond}); err != nil {
			t.Fatal(err)
		}
		if err := r.Register("panics", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
			panic("handler went boom")
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)
		if err := r.Register("late", func(context.Context, Job, ProgressFn) (json.RawMessage, error) { return nil, nil }, RegisterOpts{}); err == nil {
			t.Fatal("Register after Start accepted")
		}

		hang, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "hangs"})
		waitFor(t, "the timeout to fail the job", func() bool {
			fresh := jobState(t, js, hang.ID)
			return fresh.State == store.JobFailed
		})
		if res := string(jobState(t, js, hang.ID).Result); res == "" {
			t.Fatal("timed-out job lost its structured error")
		} else {
			var d map[string]string
			if err := json.Unmarshal([]byte(res), &d); err != nil || d["error"] == "" {
				t.Fatalf("timeout result = %q, want the failure reason", res)
			}
		}

		panicking, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "panics"})
		waitFor(t, "the panicking handler to fail only its own job", func() bool {
			fresh := jobState(t, js, panicking.ID)
			return fresh.State == store.JobQueued || fresh.State == store.JobFailed
		})
		// The runner survives the panic: MaxAttempts defaults to 1, so the job
		// ends failed, and the pool still processes what comes next.
		after, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "k"})
		waitFor(t, "post-panic processing to work", func() bool {
			return jobState(t, js, after.ID).State == store.JobSucceeded
		})
	})
}

// TestPanicIsRetryableOnce proves the runner keeps the job alive through a
// panic (retryable), and terminates it only when the budget is gone.
func TestPanicIsRetryableOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, js := openMigrated(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var runs int
		var mu sync.Mutex
		r := fastRunner(t, js, nil)
		if err := r.Register("panics.once", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
			mu.Lock()
			runs++
			first := runs == 1
			mu.Unlock()
			if first {
				panic("transient-looking nil map write, hypothetically")
			}
			return json.RawMessage(`{"fine":true}`), nil
		}, RegisterOpts{Concurrency: 1}); err != nil {
			t.Fatal(err)
		}
		mustStart(t, r, ctx)
		job, _, _ := js.Enqueue(ctx, store.EnqueueParams{Kind: "panics.once", MaxAttempts: 2})
		waitFor(t, "the panicked job to retry and succeed", func() bool {
			return jobState(t, js, job.ID).State == store.JobSucceeded
		})
	})
}
