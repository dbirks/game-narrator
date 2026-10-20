// Readiness is the cached database health behind /health/ready (ADR-0097).
//
// The full PRAGMA integrity_check exceeds the probe's 2 s budget on the A53
// target — issue #9 measured ~2.83 s for the modernc.org/sqlite check on the
// UNO Q against a file the C sqlite3 CLI verified in 0.82 s — so the check no
// longer runs per request. It runs once at startup (before the listener
// opens) and again on a schedule, and /health/ready answers from the cache,
// reporting the verdict's age so a stale "ok" is visible rather than silently
// trusted. The self-repair decision (the one damage class the store can
// rebuild: the derived messages_fts index) moves with the check, so an
// index-only damage class is rebuilt in the background and the next /ready
// answers ok again without any request paying the rebuild.
package store

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// DefaultReadinessInterval is the schedule when GAME_NARRATOR_READINESS_INTERVAL
// is unset. A full check costs ~3 s on the A53; five minutes bounds how stale
// a corruption answer can be while costing ~6 % of one core-second per
// interval on the target.
const DefaultReadinessInterval = 5 * time.Minute

// The fixed failure strings the verdict carries. Store errors name the
// database file path, and the API surface deliberately carries no host paths
// (the same rule the per-request spelling keeps): only these two strings ever
// reach the /health/ready payload.
const (
	PingFailedDetail      = "database ping failed"
	IntegrityFailedDetail = "integrity check failed"
)

// Readiness caches one integrity verdict per check. It is safe for
// concurrent use: the scheduler and the readiness handler touch the cache at
// once, and the mutex keeps a verdict atomic (version, outcome, age).
type Readiness struct {
	st       *Store
	interval time.Duration
	log      *slog.Logger

	mu     sync.RWMutex
	ver    int
	ok     bool
	detail string
	at     time.Time

	// runCheck runs one full probe and records its verdict. Production wires
	// checkStore; a test points it at a slow or failing checker (the ADR's
	// "injected slow checker") without a database.
	runCheck func(ctx context.Context)
}

// NewReadiness builds the production readiness cache over st: the
// schema-version ping, the full integrity check and the one self-repair,
// re-run every interval. An interval <= 0 falls back to
// DefaultReadinessInterval; a nil log drops the check's log lines (the
// verdict is still recorded).
func NewReadiness(st *Store, interval time.Duration, log *slog.Logger) *Readiness {
	r := &Readiness{st: st, interval: normalizeInterval(interval), log: logOrDefault(log)}
	r.runCheck = r.checkStore
	return r
}

// NewReadinessWithChecker is the same cache with an injected probe in place
// of the store's ping + integrity + repair (ADR-0097): the checker stands in
// for the whole probe and records its verdict with Record. The scheduling,
// caching and age semantics are identical, which is what lets a slow or
// failing checker pin the contract — a probe must never block the answer, a
// failing probe records a fixed-string verdict, and a cancelled tick does not
// clobber the last good verdict — without a database or a wedged disk.
func NewReadinessWithChecker(interval time.Duration, log *slog.Logger, check func(r *Readiness, ctx context.Context)) *Readiness {
	r := &Readiness{interval: normalizeInterval(interval), log: logOrDefault(log)}
	r.runCheck = func(ctx context.Context) { check(r, ctx) }
	return r
}

// Check runs one full probe and records the verdict. Startup calls it before
// the listener opens, so the first /health/ready already answers from a
// verdict rather than an empty cache.
func (r *Readiness) Check(ctx context.Context) {
	r.runCheck(ctx)
}

// Run re-checks on the schedule until ctx is done. One goroutine owns the
// ticks, so checks never overlap; a check that overruns its tick only delays
// the next one, it never stacks. The tick's own context is bounded by the
// interval, so a wedged database cannot hold the scheduler hostage: the tick
// fails, the verdict records the failure, and the next tick retries.
func (r *Readiness) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, r.interval)
			r.runCheck(checkCtx)
			cancel()
		}
	}
}

// Snapshot reports the cached verdict: the schema version the last check saw,
// the integrity outcome with its fixed-string detail ("" when ok), and the
// age of the verdict. Callers put the age in the payload so the answer says
// how stale it is (ADR-0097).
func (r *Readiness) Snapshot() (version int, ok bool, detail string, age time.Duration) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.ver, r.ok, r.detail, time.Since(r.at)
}

// Record stores one probe's verdict as the cached one.
func (r *Readiness) Record(version int, ok bool, detail string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ver, r.ok, r.detail, r.at = version, ok, detail, at
}

// checkStore is the production probe. Only fixed strings leave it: the
// verdict's detail is one of the two constants above, never the store's error
// (which names the database file path).
func (r *Readiness) checkStore(ctx context.Context) {
	version, err := r.st.SchemaVersion(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return // the tick was cancelled (shutdown): the last verdict stands
		}
		r.log.Warn("readiness: database ping failed", "error", err)
		r.Record(0, false, PingFailedDetail, time.Now())
		return
	}
	if err := r.st.IntegrityCheck(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}
		// The one self-repairing damage class: the messages_fts index is
		// derived entirely from messages (migration 0002's triggers), so
		// damage confined to it can be rebuilt from the content without
		// touching a user row. Damage that reaches the content is refused
		// and stays down: the operator path there is backup/restore.
		repaired, repairErr := r.st.RecoverDerivedFTS(ctx)
		if !repaired {
			r.log.Warn("readiness: database integrity check failed", "error", err)
			if repairErr != nil {
				r.log.Error("readiness: integrity damage is not confined to the derived messages_fts index (or the rebuild did not verify); no self-repair applied; restore from the last good backup", "error", repairErr)
			}
			r.Record(version, false, IntegrityFailedDetail, time.Now())
			return
		}
		r.log.Error("readiness: database failed integrity check with damage confined to the derived messages_fts index; the index was rebuilt from messages and re-verified clean — no user rows were touched", "repaired", "messages_fts")
		r.Record(version, true, "", time.Now())
		return
	}
	r.Record(version, true, "", time.Now())
}

func normalizeInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return DefaultReadinessInterval
	}
	return d
}

func logOrDefault(log *slog.Logger) *slog.Logger {
	if log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return log
}
