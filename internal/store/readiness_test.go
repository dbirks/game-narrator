package store

// Tests for the readiness cache (ADR-0097): the full integrity check runs at
// startup and on a schedule, never per request. The contract pinned here:
// a probe must never block the answer (an injected slow checker), a failing
// probe records a fixed-string verdict (no host paths), a cancelled tick
// does not clobber the last good verdict, and the self-repair verdict is the
// one the next /health/ready answers from.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestReadinessStartupCheckCachesTheVerdict is the production path on a real
// (small) store: Check before the "listener" and the Snapshot the probe
// answers from — ok, the schema version, a fresh age.
func TestReadinessStartupCheckCachesTheVerdict(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)
	r := NewReadiness(s, 0, nil)
	if _, ok, detail, _ := r.Snapshot(); ok || detail != "" {
		t.Fatalf("before any check the cache reported ok=%v detail=%q, want an empty cache (not ok, no error string)", ok, detail)
	}

	r.Check(ctx)
	version, ok, detail, age := r.Snapshot()
	if !ok {
		t.Fatalf("startup check verdict = fail (detail %q), want ok on a fresh store", detail)
	}
	if version <= 0 {
		t.Fatalf("cached schema version = %d, want the migrated version", version)
	}
	if age > time.Minute {
		t.Fatalf("cached verdict age = %v, want fresh (the check ran a moment ago)", age)
	}
	want, err := s.SchemaVersion(ctx)
	if err != nil {
		t.Fatalf("SchemaVersion: %v", err)
	}
	if version != want {
		t.Fatalf("cached version = %d, want the store's %d", version, want)
	}
}

// TestReadinessSlowCheckerNeverBlocksTheAnswer is the ADR's injected slow
// checker: the full check takes far longer than the probe's 2 s budget (the
// ~2.8 s A53 measurement of issue #9), and yet the answer — the Snapshot the
// /health/ready handler reports — comes back immediately with the previous
// verdict, while the slow check runs on and records its own verdict when it
// finishes.
func TestReadinessSlowCheckerNeverBlocksTheAnswer(t *testing.T) {
	r := NewReadinessWithChecker(0, nil, func(r *Readiness, ctx context.Context) {
		time.Sleep(300 * time.Millisecond) // stand-in for the 2.8 s A53 check
		r.Record(7, true, "", time.Now())
	})

	// A stale-but-valid verdict is what a startup check already recorded.
	r.Record(6, true, "", time.Now().Add(-2*time.Minute))

	// The slow check is running in the background; the answer must not wait.
	go r.Check(context.Background())
	start := time.Now()
	version, ok, _, _ := r.Snapshot() // the one the /health/ready handler would report
	elapsed := time.Since(start)
	if !ok || version != 6 {
		t.Fatalf("the in-flight probe clobbered the previous good verdict: (%d, %v), want (6, ok)", version, ok)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("a single Snapshot while the 300 ms probe runs took %v: a cache read must not block on the probe", elapsed)
	}

	// And when the slow check finishes it records its own verdict, refreshing
	// the age the answer reports.
	deadline := time.Now().Add(2 * time.Second) // the probe budget the check exceeds
	for {
		if _, ok, _, age := r.Snapshot(); ok && age < time.Minute {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the slow probe did not finish within the budget: it must run, not the answer that waits")
		}
		time.Sleep(5 * time.Millisecond)
	}
	version, ok, _, age := r.Snapshot()
	if !ok || version != 7 {
		t.Fatalf("after the slow check the cache = version %d ok %v, want 7/ok", version, ok)
	}
	if age > time.Minute {
		t.Fatalf("the finished check did not refresh the verdict age: %v", age)
	}
}

// TestReadinessFailingCheckerRecordsTheFixedString pins the payload rule: a
// failing probe records one of the two fixed strings, never the store's error
// (which names the database file path — host paths do not reach the API).
func TestReadinessFailingCheckerRecordsTheFixedString(t *testing.T) {
	r := NewReadinessWithChecker(0, nil, func(r *Readiness, ctx context.Context) {
		r.Record(3, false, IntegrityFailedDetail, time.Now())
	})
	r.Check(context.Background())
	version, ok, detail, _ := r.Snapshot()
	if ok || version != 3 || detail != IntegrityFailedDetail {
		t.Fatalf("verdict = (%d, %v, %q), want (3, fail, %q)", version, ok, detail, IntegrityFailedDetail)
	}
	if detail == "database ping failed" {
		t.Fatal("the integrity failure recorded the ping detail")
	}
}

// TestReadinessScheduleRechecksOnTheInterval pins the schedule: the check runs
// at startup (the caller's job) and then once per tick, until the context is
// done.
func TestReadinessScheduleRechecksOnTheInterval(t *testing.T) {
	var ticks atomic.Int32
	r := NewReadinessWithChecker(40*time.Millisecond, nil, func(r *Readiness, ctx context.Context) {
		ticks.Add(1)
		r.Record(1, true, "", time.Now())
	})
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the context was done")
	}
	if n := ticks.Load(); n < 3 {
		t.Fatalf("the scheduler ran the check %d times in 400 ms at a 40 ms interval, want >= 3", n)
	}
}

// TestReadinessCancelledTickKeepsTheLastVerdict pins the shutdown rule: a tick
// cancelled by the process context records nothing — a cancelled probe's
// errors are not damage, and the last good verdict must stand for the next
// boot's startup check (which runs before the listener, ADR-0097).
func TestReadinessCancelledTickKeepsTheLastVerdict(t *testing.T) {
	r := NewReadinessWithChecker(0, nil, func(r *Readiness, ctx context.Context) {
		// checkStore's own cancelled-tick rule, spelt out.
		if ctx.Err() != nil {
			return
		}
		r.Record(4, true, "", time.Now())
	})
	r.Record(3, true, "", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Check(ctx)
	version, ok, _, _ := r.Snapshot()
	if version != 3 || !ok {
		t.Fatalf("a cancelled tick recorded a verdict: (%d, %v), want the previous (3, ok)", version, ok)
	}
}

// TestReadinessCheckSurvivesAClosedStore pins the production probe against a
// store whose connection pool is gone: the ping fails, the verdict records
// the fixed ping string, and the cache is left in a state the next scheduled
// check can replace (the pool is reopened, the verdict heals — no process
// restart).
func TestReadinessCheckSurvivesAUnreadableStore(t *testing.T) {
	s := migratedStore(t)
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	r := NewReadiness(s, 0, nil)
	r.Check(testCtx(t))
	_, ok, detail, _ := r.Snapshot()
	if ok || detail != PingFailedDetail {
		t.Fatalf("verdict = (%v, %q), want (fail, %q)", ok, detail, PingFailedDetail)
	}
}

// TestReadinessRepairsIndexOnlyDamageAtCheckTime pins the move of the
// self-repair from the per-request path into the scheduled check: the verdict
// the next /health/ready answers from is the repaired one, and the search the
// repair was made to keep still works.
func TestReadinessRepairsIndexOnlyDamageAtCheckTime(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	seed := make([]games.Message, 0, 120)
	for i := 0; i < 120; i++ {
		seed = append(seed, ftsLine(f, i))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, seed, nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Settle the index into data pages so there is a page body to damage.
	mustExec(t, f.s.db, `INSERT INTO messages_fts(messages_fts) VALUES('merge')`)

	s := corruptFTSIndexPage(t, f.s)
	if err := s.IntegrityCheck(ctx); err == nil {
		t.Fatal("corrupted index passed PRAGMA integrity_check: the guard would never fire")
	}

	r := NewReadiness(s, 0, nil)
	r.Check(ctx) // the startup check finds the damage and self-repairs it
	version, ok, detail, _ := r.Snapshot()
	if !ok || detail != "" {
		t.Fatalf("verdict after the self-repairing check = (%d, %v, %q), want ok with no error", version, ok, detail)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Fatalf("integrity after the check's repair: %v", err)
	}
	if hits, err := s.SearchMessages(ctx, f.profile.ID, "keeper log", 5); err != nil || len(hits) == 0 {
		t.Fatalf("search after repair: hits=%d err=%v, want the seeded lines", len(hits), err)
	}
}
