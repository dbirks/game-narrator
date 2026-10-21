package store

// The durable work queue over migration 0003's `jobs`/`job_attempts` tables
// plus migration 0005's lease columns (docs/spec/02 "Entity catalog": "jobs,
// job_attempts … detailed job state, parent chains, replay/resume tokens";
// docs/spec/08 "durable workflow queue").
//
// Three properties are load-bearing and live here rather than in any caller:
//
//  1. A claim is a compare-and-swap. Claim runs inside BEGIN IMMEDIATE (via
//     inWrite), so two workers polling the same appliance cannot both be handed
//     the same job — docs/spec/12 names "duplicate job execution after restart"
//     as a failure to test for, and "each job runs exactly once" is only
//     provable if the pick, the attempt increment and the lease write are one
//     transaction.
//  2. The retry budget is a schema invariant. `attempts <= max_attempts` is a
//     table CHECK (AGENTS.md rule 11: bounded retries); every mutation here
//     reads that pair before writing, and the claim query additionally refuses
//     an expired-lease row that has already spent its budget.
//  3. Crash evidence is kept, not hidden. An attempt row left `running` is
//     exactly what Claim (marking the predecessor `crashed`) and
//     RecoverOnStartup consume; docs/spec/02 requires interrupted `running`
//     jobs to become `recoverable` at boot rather than silently restart.
//
// Timestamps: run_after and lease_expires_at are stored as same-width UTC
// millisecond stamps (stampMS) so SQLite's TEXT comparison is chronological —
// a lease can be shorter than a second, which the table's second-resolution
// strftime CHECK tolerates but plain RFC 3339 comparisons would not.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// Job states mirror the `jobs.state` CHECK enum in 0003; they are strings in
// the domain because that is what the table enforces and what the API echoes.
const (
	JobQueued        = "queued"
	JobRunning       = "running"
	JobSucceeded     = "succeeded"
	JobFailed        = "failed"
	JobCancelled     = "cancelled"
	JobRecoverable   = "recoverable"
	JobRequiresInput = "requires_input"
)

// Job attempt outcomes from the `job_attempts.outcome` CHECK.
const (
	AttemptRunning       = "running"
	AttemptSucceeded     = "succeeded"
	AttemptFailed        = "failed"
	AttemptCancelled     = "cancelled"
	AttemptTimedOut      = "timed_out"
	AttemptCrashed       = "crashed"
	AttemptRequiresInput = "requires_input"
)

// Backoff shape required by the queue design: base 2 s doubling per attempt,
// capped at 5 minutes, jittered so a fleet of workers restarting onto one
// poisoned job does not stampede in lockstep.
const (
	retryBackoffBase = 2 * time.Second
	retryBackoffCap  = 5 * time.Minute
)

// stampMS is the fixed-width UTC form for run_after/lease_expires_at and the
// jobs created_at/updated_at columns. One width, one zone, always: the claim
// query compares these with `<=` on TEXT, and 0003 explains why mixed-offset
// RFC 3339 strings are chronologically un-sortable. The value still satisfies
// every strftime CHECK the columns carry.
func stampMS(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// Job is a row of the `jobs` table. Times are UTC; zero RunAfter /
// LeaseExpiresAt mean the SQL NULL ("no delay", "no lease").
type Job struct {
	ID             string
	Kind           string
	State          string
	Priority       int
	Payload        json.RawMessage
	Result         json.RawMessage
	ParentID       string
	ResumeToken    string
	Attempts       int
	MaxAttempts    int
	RunAfter       time.Time
	TraceID        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	IdempotencyKey string
	LeaseOwner     string
	LeaseExpiresAt time.Time
}

// JobAttempt is a row of `job_attempts` — the audit trail docs/spec/02 asks
// for ("audit trail") and the crash evidence docs/spec/13 consumes.
type JobAttempt struct {
	JobID      string
	Attempt    int
	StartedAt  time.Time
	FinishedAt time.Time // zero = still in flight
	Outcome    string
	Error      string
}

// EnqueueParams is one request to queue work. Zero values take the schema's
// defaults (payload {}, priority 0, max attempts 1, immediate, no parent).
type EnqueueParams struct {
	Kind        string
	Payload     json.RawMessage
	Priority    int
	MaxAttempts int
	// IdempotencyKey implements docs/spec/08's `Idempotency-Key`: re-enqueue
	// with the same key returns the existing job instead of starting a second
	// import.
	IdempotencyKey string
	RunAfter       time.Time
	ParentID       string
	// TraceID correlates the job with its events; defaulted to the job id.
	TraceID     string
	ResumeToken string
}

// JobFilter narrows ListJobs. Empty fields mean "any".
type JobFilter struct {
	State string
	Kind  string
}

// RecoveryReport is what RecoverOnStartup found and did, so `serve` can log an
// honest account of the crash it just recovered from.
type RecoveryReport struct {
	// Marked are jobs found `running` by a previous process and moved to
	// `recoverable` (docs/spec/02 "Transaction workflows").
	Marked []string
	// Requeued are the `recoverable` jobs whose kind is registered resumable
	// and whose retry budget still has room; they are `queued` again.
	Requeued []string
}

// JobsStore is the queue API. Safe for concurrent use; every mutation is a
// BEGIN IMMEDIATE transaction, so many workers may poll one database.
type JobsStore struct {
	s *Store
}

// NewJobsStore wires the queue onto an open, migrated store.
func NewJobsStore(s *Store) *JobsStore { return &JobsStore{s: s} }

// QueueDepth is the durable queue's depth for the telemetry surface
// (docs/spec/13 "Metrics", internal/telemetry): jobs waiting (queued) plus
// in flight (running). It is a read of the database at scrape time, not an
// in-process counter — the database is the authority on job state (the
// package's first rule), and crash-recovery handoff moves jobs between
// states outside the enqueue/complete paths, so a counter kept on those
// paths would drift from the table it exists to describe.
func (j *JobsStore) QueueDepth(ctx context.Context) (int64, error) {
	var n int64
	err := j.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, "queue depth",
			"SELECT COUNT(*) FROM jobs WHERE state IN ('queued', 'running')").Scan(&n)
	})
	return n, err
}

// CountQueued is the count of jobs waiting in the queue (state queued). It is
// the read the runner's visible foreground pause (ADR-0119, issue #27) reports
// beside the pause itself: the database is the authority on job state, so the
// count is a query at read time — never a counter kept on the enqueue/claim
// paths, which crash-recovery handoff would drift from the table.
func (j *JobsStore) CountQueued(ctx context.Context) (int64, error) {
	var n int64
	err := j.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, "queued count",
			"SELECT COUNT(*) FROM jobs WHERE state = 'queued'").Scan(&n)
	})
	return n, err
}

// jobColumns is every SELECT list in this file, so Get, List and Claim cannot
// drift onto different column orders (the classic way a list view and a
// detail view start disagreeing about the same row).
const jobColumns = `id, kind, state, priority, payload_json, result_json, parent_id,
	resume_token, attempts, max_attempts, run_after, trace_id, created_at, updated_at,
	idempotency_key, lease_owner, lease_expires_at`

// Enqueue puts a job on the queue. An IdempotencyKey that already exists
// returns that stored job with created=false — the retried POST must not
// start a second import (docs/spec/08). The check and the insert share one
// BEGIN IMMEDIATE transaction so two concurrent retried POSTs cannot both
// miss the key and both insert.
func (j *JobsStore) Enqueue(ctx context.Context, p EnqueueParams) (*Job, bool, error) {
	if strings.TrimSpace(p.Kind) == "" {
		return nil, false, fmt.Errorf("store: enqueue job: kind is required: %w", ErrConflict)
	}
	if p.Priority < 0 || p.Priority > 100 {
		return nil, false, fmt.Errorf("store: enqueue job: priority %d outside 0..100: %w", p.Priority, ErrConflict)
	}
	if p.MaxAttempts < 0 {
		return nil, false, fmt.Errorf("store: enqueue job: max_attempts %d must be >= 1 (0 means the default): %w", p.MaxAttempts, ErrConflict)
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 1
	}
	payload := orDefault(p.Payload, "{}")
	trace := p.TraceID
	if trace == "" {
		trace = games.NewID()
	}
	if p.ParentID != "" && !games.ValidID(p.ParentID) {
		return nil, false, fmt.Errorf("store: enqueue job: parent_id %q is not a valid id", p.ParentID)
	}

	var (
		out     *Job
		created bool
	)
	err := j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "enqueue job"
		if p.IdempotencyKey != "" {
			var existing string
			err := tx.queryRow(ctx, op,
				"SELECT id FROM jobs WHERE idempotency_key = ?", p.IdempotencyKey).Scan(&existing)
			switch {
			case err == nil:
				stored, err := getJobTx(ctx, tx, existing)
				if err != nil {
					return err
				}
				out, created = stored, false
				return nil
			case err != sql.ErrNoRows:
				return mapReadError(op, err)
			}
		}

		id := games.NewID()
		now := time.Now().UTC()
		var runAfter any
		if !p.RunAfter.IsZero() {
			runAfter = stampMS(p.RunAfter)
		}
		if _, err := tx.exec(ctx, op,
			`INSERT INTO jobs (id, kind, state, priority, payload_json, parent_id,
				resume_token, attempts, max_attempts, run_after, trace_id,
				created_at, updated_at, idempotency_key)
			 VALUES (?, ?, 'queued', ?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?)`,
			id, p.Kind, p.Priority, payload, nullStr(p.ParentID), nullStr(p.ResumeToken),
			maxAttempts, runAfter, trace, stampMS(now), stampMS(now), nullStr(p.IdempotencyKey)); err != nil {
			return err
		}
		stored, gerr := getJobTx(ctx, tx, id)
		if gerr != nil {
			return gerr
		}
		out, created = stored, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

// Claim atomically hands one runnable job to workerID with a lease of the
// given duration, or returns (nil, nil) when the queue has nothing eligible.
// Runnable means `queued` and past its run_after gate, or `running` whose
// lease has expired (the previous owner is gone) and whose retry budget is
// not yet spent. Highest priority first, then oldest; a same-millisecond tie is
// broken by insertion order (`rowid`), never by the random ULID suffix in `id`,
// which would otherwise make two jobs enqueued in one millisecond claim in an
// order nobody chose. `created_at` is millisecond-stamped, so ties are routine.
//
// Each claim also records a `job_attempts` row (docs/spec/02 "audit trail")
// and closes out any attempt still marked `running` for that job as `crashed`
// — that is the takeover evidence, and it is written in the same transaction
// so the audit history cannot show two live attempts.
func (j *JobsStore) Claim(ctx context.Context, workerID string, kinds []string, lease time.Duration) (*Job, error) {
	if workerID == "" {
		return nil, fmt.Errorf("store: claim job: worker id is required")
	}
	if lease <= 0 {
		return nil, fmt.Errorf("store: claim job: lease must be positive")
	}

	var out *Job
	err := j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "claim job"
		now := time.Now().UTC()
		q := `(state = 'queued' AND (run_after IS NULL OR run_after <= ?))
			OR (state = 'running' AND lease_expires_at IS NOT NULL AND lease_expires_at <= ?
				AND attempts < max_attempts)`
		args := []any{stampMS(now), stampMS(now)}
		if len(kinds) > 0 {
			var sb strings.Builder
			for i, k := range kinds {
				if i > 0 {
					sb.WriteString(", ")
				}
				sb.WriteString("?")
				args = append(args, k)
			}
			q = "(" + q + ") AND kind IN (" + sb.String() + ")"
		}

		var id string
		err := tx.queryRow(ctx, op,
			`SELECT id FROM jobs
			 WHERE `+q+`
			 ORDER BY priority DESC, created_at ASC, rowid ASC LIMIT 1`,
			args...).Scan(&id)
		if err == sql.ErrNoRows {
			return nil // queue empty: an answer, not a failure
		}
		if err != nil {
			return mapReadError(op, err)
		}

		// The row the SELECT named cannot change underneath: this transaction
		// has held the write lock since BEGIN IMMEDIATE, so no second claimer
		// is between the pick and the update.
		if _, err := tx.exec(ctx, op,
			`UPDATE jobs SET state = 'running', attempts = attempts + 1,
				lease_owner = ?, lease_expires_at = ?, updated_at = ?
			 WHERE id = ?`,
			workerID, stampMS(now.Add(lease)), stampMS(now), id); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, op,
			`UPDATE job_attempts SET outcome = 'crashed', finished_at = ?,
				error = COALESCE (NULLIF (error, ''), 'lease expired; taken over by another worker')
			 WHERE job_id = ? AND outcome = 'running' AND finished_at IS NULL`,
			stampMS(now), id); err != nil {
			return err
		}
		var attempt int
		if err := tx.queryRow(ctx, op, "SELECT attempts FROM jobs WHERE id = ?", id).Scan(&attempt); err != nil {
			return mapReadError(op, err)
		}
		if _, err := tx.exec(ctx, op,
			`INSERT INTO job_attempts (job_id, attempt, started_at, outcome) VALUES (?, ?, ?, 'running')`,
			id, attempt, stampMS(now)); err != nil {
			return err
		}
		out, err = getJobTx(ctx, tx, id)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Heartbeat extends the lease of a job this worker still owns and reports the
// job's current state. It is how a running worker learns about an out-of-band
// Cancel (docs/spec/08 cancel has to work against a job already in flight):
// state != running means "stop and do not finalize". Losing the lease to a
// takeover is ErrConflict — the caller must abandon its in-flight work
// immediately, because from this instant a second worker owns the job.
func (j *JobsStore) Heartbeat(ctx context.Context, jobID, workerID string, lease time.Duration) (string, error) {
	const op = "heartbeat job"
	var state string
	err := j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var owner sql.NullString
		if err := tx.queryRow(ctx, op,
			"SELECT state, lease_owner FROM jobs WHERE id = ?", jobID).Scan(&state, &owner); err != nil {
			return mapReadError(op, err)
		}
		if state != JobRunning {
			return nil // e.g. cancelled: reported, nothing extended
		}
		if owner.String != workerID {
			return fmt.Errorf("store: %s: %s held by %q, not %q: %w", op, jobID, owner.String, workerID, ErrConflict)
		}
		now := time.Now().UTC()
		_, err := tx.exec(ctx, op,
			"UPDATE jobs SET lease_expires_at = ?, updated_at = ? WHERE id = ? AND lease_owner = ? AND state = 'running'",
			stampMS(now.Add(lease)), stampMS(now), jobID, workerID)
		return err
	})
	if err != nil {
		// On a conflict or a lost race the state we read is unreliable for the
		// caller's decision; return it anyway — it errs toward "stop".
		if state != "" {
			return state, err
		}
		return "", err
	}
	return state, nil
}

// Complete marks a running job succeeded and closes its in-flight attempt.
// A job that is no longer `running` (cancelled under it, or already succeeded
// — a replayed completion) is ErrConflict: finalizing a job another worker
// owns would let two workers both report victory.
func (j *JobsStore) Complete(ctx context.Context, jobID string, result json.RawMessage) error {
	return j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "complete job"
		now := stampMS(time.Now().UTC())
		res, err := tx.exec(ctx, op,
			`UPDATE jobs SET state = 'succeeded', result_json = ?, updated_at = ?,
				lease_owner = NULL, lease_expires_at = NULL
			 WHERE id = ? AND state = 'running'`,
			jsonOrNull(result), now, jobID)
		if err != nil {
			return err
		}
		if err := touchedOrConflict(ctx, tx, op, res, jobID); err != nil {
			return err
		}
		// Close the attempt the transition consumed, in the same transaction:
		// an attempt row left `running` under a succeeded job is false crash
		// evidence for the reconciler.
		_, err = tx.exec(ctx, op,
			`UPDATE job_attempts SET outcome = 'succeeded', finished_at = ?
			 WHERE job_id = ? AND outcome = 'running' AND finished_at IS NULL`,
			now, jobID)
		return err
	})
}

// Fail records an attempt failure. While the job is retryable — the caller
// says so, budget remains (attempts < max_attempts) — it goes back to `queued`
// with an exponential backoff (base 2 s, cap 5 min, jittered) on run_after.
// Out of budget, or told the error is permanent, it becomes `failed` with the
// structured error in result_json (docs/spec/08's error contract surfaces it
// to the UI). A job already in a terminal or cancelled state is left alone —
// Cancel wins over a late failure report by a worker that did not heartbeat.
func (j *JobsStore) Fail(ctx context.Context, jobID, reason string, retryable bool) error {
	return j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "fail job"
		var (
			state                 string
			attempts, maxAttempts int
		)
		if err := tx.queryRow(ctx, op,
			"SELECT state, attempts, max_attempts FROM jobs WHERE id = ?", jobID).
			Scan(&state, &attempts, &maxAttempts); err != nil {
			return mapReadError(op, err)
		}
		if state != JobRunning {
			return nil // Cancel/Complete already decided this job's fate
		}

		now := time.Now().UTC()
		next := JobFailed
		var runAfter any
		var result any = errorJSON(reason)
		if retryable && attempts < maxAttempts {
			next = JobQueued
			runAfter = stampMS(now.Add(retryDelay(attempts)))
			result = nil // keep the previous result; the job is not done
		}
		if _, err := tx.exec(ctx, op,
			`UPDATE jobs SET state = ?, run_after = ?, result_json = COALESCE (?, result_json),
				updated_at = ?, lease_owner = NULL, lease_expires_at = NULL
			 WHERE id = ? AND state = 'running'`,
			next, runAfter, result, stampMS(now), jobID); err != nil {
			return err
		}
		_, err := tx.exec(ctx, op,
			`UPDATE job_attempts SET outcome = 'failed', finished_at = ?, error = ?
			 WHERE job_id = ? AND outcome = 'running' AND finished_at IS NULL`,
			stampMS(now), reason, jobID)
		return err
	})
}

// Cancel moves a queued, running or recoverable job to `cancelled`. It is
// idempotent on an already-cancelled job and ErrConflict on a terminal
// succeeded/failed one (there is nothing to cancel; the caller wants Retry
// instead). A running worker learns about it at its next Heartbeat rather
// than by signal — cancellation is cooperative, per docs/spec/01's deadline
// contracts.
func (j *JobsStore) Cancel(ctx context.Context, jobID string) (*Job, error) {
	var out *Job
	err := j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "cancel job"
		var state string
		if err := tx.queryRow(ctx, op, "SELECT state FROM jobs WHERE id = ?", jobID).Scan(&state); err != nil {
			return mapReadError(op, err)
		}
		switch state {
		case JobCancelled:
			// Idempotent: the second cancel is not a different outcome.
		case JobQueued, JobRunning, JobRecoverable:
			now := stampMS(time.Now().UTC())
			if _, err := tx.exec(ctx, op,
				`UPDATE jobs SET state = 'cancelled', updated_at = ?, lease_owner = NULL,
					lease_expires_at = NULL
				 WHERE id = ?`, now, jobID); err != nil {
				return err
			}
			if _, err := tx.exec(ctx, op,
				`UPDATE job_attempts SET outcome = 'cancelled', finished_at = ?
				 WHERE job_id = ? AND outcome = 'running' AND finished_at IS NULL`,
				now, jobID); err != nil {
				return err
			}
		default:
			return fmt.Errorf("store: %s: job %s is %s and cannot be cancelled: %w", op, jobID, state, ErrConflict)
		}
		var err error
		out, err = getJobTx(ctx, tx, jobID)
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Retry re-queues a failed or cancelled job from scratch: attempts reset to
// zero (this is a *human*/operator decision, so the old budget was spent, and
// the CHECK `attempts <= max_attempts` forbids anything subtler), backoff and
// lease cleared. Any other state is ErrConflict — docs/spec/08's jobs API
// answers 409 for exactly this.
func (j *JobsStore) Retry(ctx context.Context, jobID string) (*Job, error) {
	var out *Job
	err := j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "retry job"
		res, err := tx.exec(ctx, op,
			`UPDATE jobs SET state = 'queued', attempts = 0, run_after = NULL,
				updated_at = ?, lease_owner = NULL, lease_expires_at = NULL
			 WHERE id = ? AND state IN ('failed', 'cancelled')`,
			stampMS(time.Now().UTC()), jobID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			var state string
			if err := tx.queryRow(ctx, op, "SELECT state FROM jobs WHERE id = ?", jobID).Scan(&state); err != nil {
				return mapReadError(op, err)
			}
			return fmt.Errorf("store: %s: job %s is %s, only failed or cancelled jobs can be retried: %w",
				op, jobID, state, ErrConflict)
		}
		var gerr error
		out, gerr = getJobTx(ctx, tx, jobID)
		return gerr
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecoverOnStartup implements docs/spec/02 "Transaction workflows": jobs a
// previous process left `running` become `recoverable`, their in-flight
// attempt rows become `crashed` (the evidence), and only jobs whose kind is
// registered as resumable are put back on the queue — the rest wait for the
// reconciler/agent decision docs/spec/02 describes, because blindly re-running
// an import that already wrote half its messages is simulation theater.
//
// A resumable job whose retry budget is already spent (it crashed on its last
// permitted attempt) also stays `recoverable`: the schema CHECK forbids
// attempting it again, and pretending otherwise would restart a job the
// bounded-retry rule has retired. Call this before any worker starts: a live
// cluster of pollers would race the recovery by reclaiming expired leases.
func (j *JobsStore) RecoverOnStartup(ctx context.Context, resumableKinds []string) (RecoveryReport, error) {
	var report RecoveryReport
	err := j.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "recover jobs"
		now := stampMS(time.Now().UTC())

		rows, err := tx.query(ctx, op, "SELECT id FROM jobs WHERE state = 'running'")
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return mapReadError(op, err)
			}
			report.Marked = append(report.Marked, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()

		if len(report.Marked) == 0 {
			return nil
		}
		if _, err := tx.exec(ctx, op,
			`UPDATE jobs SET state = 'recoverable', updated_at = ?, lease_owner = NULL,
				lease_expires_at = NULL WHERE state = 'running'`, now); err != nil {
			return err
		}
		if _, err := tx.exec(ctx, op,
			`UPDATE job_attempts SET outcome = 'crashed', finished_at = ?,
				error = COALESCE (NULLIF (error, ''), 'process died while the attempt was running')
			 WHERE outcome = 'running' AND finished_at IS NULL`, now); err != nil {
			return err
		}

		if len(resumableKinds) == 0 {
			return nil
		}
		var (
			sb   strings.Builder
			args = []any{now}
		)
		sb.WriteString(`UPDATE jobs SET state = 'queued', run_after = NULL, updated_at = ?
			 WHERE state = 'recoverable' AND kind IN (`)
		for i, k := range resumableKinds {
			if i > 0 {
				sb.WriteString(", ")
			}
			sb.WriteString("?")
			args = append(args, k)
		}
		sb.WriteString(") AND attempts < max_attempts")
		res, err := tx.exec(ctx, op, sb.String(), args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			rrows, err := tx.query(ctx, op,
				`SELECT id FROM jobs WHERE state = 'queued' AND updated_at = ?`, now)
			if err != nil {
				return err
			}
			defer rrows.Close()
			for rrows.Next() {
				var id string
				if err := rrows.Scan(&id); err != nil {
					return mapReadError(op, err)
				}
				report.Requeued = append(report.Requeued, id)
			}
			return rrows.Err()
		}
		return nil
	})
	if err != nil {
		return RecoveryReport{}, err
	}
	return report, nil
}

// GetJob fetches one job by id. A missing id is ErrNotFound.
func (j *JobsStore) GetJob(ctx context.Context, id string) (*Job, error) {
	if !games.ValidID(id) {
		return nil, fmt.Errorf("store: get job: id %q is not a valid identifier: %w", id, ErrNotFound)
	}
	var out *Job
	err := j.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		row := q.queryRow(ctx, "get job",
			"SELECT "+jobColumns+" FROM jobs WHERE id = ?", id)
		job, err := scanJob(row)
		if err != nil {
			return err
		}
		out = job
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListJobs pages jobs newest-id-first? No — in id (ULID, hence creation-time)
// order like every other list here, so the opaque keyset cursor is stable
// against concurrent state changes: ordering by updated_at would let a job
// move between pages as workers heartbeat it, which is how pagination skips
// or repeats rows.
func (j *JobsStore) ListJobs(ctx context.Context, f JobFilter, cursor string, limit int) ([]Job, string, error) {
	limit = clampLimit(limit, MaxListLimit)
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}
	if f.State != "" && !validJobState(f.State) {
		return nil, "", fmt.Errorf("store: list jobs: unknown state %q: %w", f.State, ErrConflict)
	}

	var (
		out  []Job
		next string
	)
	err = j.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		sb := &strings.Builder{}
		args := []any{after}
		sb.WriteString("SELECT " + jobColumns + " FROM jobs WHERE id > ?")
		if f.State != "" {
			sb.WriteString(" AND state = ?")
			args = append(args, f.State)
		}
		if f.Kind != "" {
			sb.WriteString(" AND kind = ?")
			args = append(args, f.Kind)
		}
		fmt.Fprintf(sb, " ORDER BY id LIMIT %d", limit+1) // limit clamped above
		rows, err := q.query(ctx, "list jobs", sb.String(), args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for len(out) <= limit && rows.Next() {
			job, err := scanJob(rows)
			if err != nil {
				return err
			}
			out = append(out, *job)
		}
		if len(out) > limit {
			next = encodeCursor(out[limit-1].ID)
			out = out[:limit]
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

// ListJobAttempts returns a job's attempt history in order — the crash
// evidence the developer inspector and the reconciler read.
func (j *JobsStore) ListJobAttempts(ctx context.Context, jobID string) ([]JobAttempt, error) {
	var out []JobAttempt
	err := j.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, "list job attempts",
			`SELECT job_id, attempt, started_at, finished_at, outcome, error
			 FROM job_attempts WHERE job_id = ? ORDER BY attempt`, jobID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				a        JobAttempt
				started  string
				finished sql.NullString
			)
			if err := rows.Scan(&a.JobID, &a.Attempt, &started, &finished, &a.Outcome, &a.Error); err != nil {
				return mapReadError("list job attempts", err)
			}
			if a.StartedAt, err = parseStamp(started); err != nil {
				return err
			}
			if s := nullStringText(finished); s != "" {
				if a.FinishedAt, err = parseStamp(s); err != nil {
					return err
				}
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// getJobTx reads one row inside an open transaction, joining the caller's
// write lock instead of asking the pool for a second connection (which would
// deadlock against the lock this goroutine holds).
func getJobTx(ctx context.Context, tx *Tx, id string) (*Job, error) {
	row := tx.queryRow(ctx, "get job", "SELECT "+jobColumns+" FROM jobs WHERE id = ?", id)
	return scanJob(row)
}

// scanJob is the one Job projection, shared by GetJob, ListJobs and Claim.
func scanJob(s scanner) (*Job, error) {
	const op = "scan job"
	var (
		id, kind, state, traceID, createdAt, updatedAt string
		priority, attempts, maxAttempts                int
		payload, result, parentID, resumeToken         []byte
		runAfter, leaseOwner, leaseExpires             sql.NullString
		idempotency                                    sql.NullString
	)
	if err := s.Scan(&id, &kind, &state, &priority, &payload, &result,
		&parentID, &resumeToken, &attempts, &maxAttempts,
		&runAfter, &traceID, &createdAt, &updatedAt, &idempotency,
		&leaseOwner, &leaseExpires); err != nil {
		return nil, mapReadError(op, err)
	}
	job := &Job{
		ID:             id,
		Kind:           kind,
		State:          state,
		Priority:       priority,
		Payload:        payload,
		Attempts:       attempts,
		MaxAttempts:    maxAttempts,
		TraceID:        traceID,
		IdempotencyKey: nullStringText(idempotency),
		ParentID:       optText(parentID),
		ResumeToken:    optText(resumeToken),
		LeaseOwner:     nullStringText(leaseOwner),
	}
	if trimmed := strings.TrimSpace(string(result)); trimmed != "" {
		job.Result = json.RawMessage(trimmed)
	}
	if len(payload) > 0 {
		job.Payload = json.RawMessage(append([]byte(nil), payload...))
	}
	for _, c := range []struct {
		name  string
		raw   string
		into  *time.Time
		allow bool
	}{{"created_at", createdAt, &job.CreatedAt, false},
		{"updated_at", updatedAt, &job.UpdatedAt, false},
		{"run_after", nullStringText(runAfter), &job.RunAfter, true},
		{"lease_expires_at", nullStringText(leaseExpires), &job.LeaseExpiresAt, true}} {
		if c.raw == "" {
			if !c.allow {
				return nil, fmt.Errorf("store: %s: %s is empty", op, c.name)
			}
			continue
		}
		t, err := parseStamp(c.raw)
		if err != nil {
			return nil, fmt.Errorf("store: %s: %s: %w", op, c.name, err)
		}
		*c.into = t
	}
	return job, nil
}

// nullStringText unwraps a scanned sql.NullString (NULL → ""). optText
// handles driver-level any values, not the typed struct — fmt.Sprint of a
// NullString is the Go struct text, which then fails every timestamp parse
// with a value like "{2026… true}".
func nullStringText(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

// parseStamp reads back any RFC 3339 shape (the fixed-width millisecond form
// this file writes, and the second-resolution form older rows or other
// repository helpers may hold).
func parseStamp(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("store: timestamp %q is not RFC 3339: %w", s, err)
	}
	return t.UTC(), nil
}

// touchedOrConflict verifies an UPDATE that required a specific prior state
// actually moved the row: zero rows means either the id is wrong (ErrNotFound)
// or the row was not in the state the transition demanded (ErrConflict) —
// reporting either as success is how a UI shows a completion that never
// happened.
func touchedOrConflict(ctx context.Context, tx *Tx, op string, res sql.Result, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return mapReadError(op, err)
	}
	if n > 0 {
		return nil
	}
	var state string
	if err := tx.queryRow(ctx, op, "SELECT state FROM jobs WHERE id = ?", id).Scan(&state); err != nil {
		return mapReadError(op, err)
	}
	return fmt.Errorf("store: %s: job %s is %s, expected running: %w", op, id, state, ErrConflict)
}

// retryDelay is the backoff curve: base · 2^(attempt-1), capped, with half
// jitter (always wait at least half the computed delay, at most the full
// delay). Half jitter — rather than uniform 0..d — keeps a fleet's retries
// spread out while preserving the ordering guarantee that the second failure
// never waits less than the first by bad luck.
func retryDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	shift := min(attempt-1, 16)
	d := retryBackoffBase << shift
	if d > retryBackoffCap || d <= 0 { // d <= 0 guards a shift overflow on 32-bit time.Duration paths
		d = retryBackoffCap
	}
	half := d / 2
	return half + time.Duration(rand.Int63n(int64(half)+1))
}

// errorJSON wraps a failure reason into the structured result_json the failed
// job carries, so GET /api/v1/jobs/:id shows why without a second query.
func errorJSON(reason string) string {
	b, err := json.Marshal(map[string]string{"error": reason})
	if err != nil { // a Go string always marshals; keep the shape explicit rather than panic
		return `{"error":"failed to encode the failure reason"}`
	}
	return string(b)
}

// jsonOrNull stores NULL (not "null") for an absent JSON column, matching the
// result_json convention: NULL means "nothing reported yet".
func jsonOrNull(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || !json.Valid(raw) {
		return nil
	}
	return trimmed
}

// validJobState checks a list filter against the schema's enum without
// importing the table grammar twice.
func validJobState(s string) bool {
	switch s {
	case JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled, JobRecoverable, JobRequiresInput:
		return true
	}
	return false
}
