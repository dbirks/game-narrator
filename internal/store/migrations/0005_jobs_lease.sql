-- 0005_jobs_lease.sql — worker lease columns for the durable queue
-- (docs/spec/02 "Transaction workflows": "interrupted `running` jobs moved to
-- `recoverable`"; docs/spec/13 resilience matrix "Power outage": "Recover
-- transactions and jobs").
--
-- Why new columns and not a reinterpretation of 0003's existing ones: a claim
-- is only safe if the database knows *who* holds a `running` job and *until
-- when*. Without them, a worker that dies (crash, OOM kill, power loss) leaves
-- its job `running` forever with no evidence of staleness — nothing distinguishes
-- "still working" from "gone", so no other worker may take the job over and the
-- queue deadlocks exactly when resilience matters. `updated_at` cannot serve:
-- it changes on any bookkeeping write, and a long-running import updates rows
-- for unrelated reasons. `run_after` cannot serve either: it gates *starting* a
-- job (backoff), not the expiry of an in-flight execution, and overloading one
-- column for both makes a retry-scheduled job look claimed and a claimed job
-- look scheduled.
--
--   * lease_owner: identity of the worker goroutine that claimed the row
--     ('<runner-id>/<kind>#<slot>'). Heartbeat and Complete must present it;
--     a stale worker whose lease was taken over cannot finalize the job it no
--     longer owns — that double-finalization is the race docs/spec/12 lists as
--     "duplicate job execution after restart".
--   * lease_expires_at: wall-clock deadline. Claim sets it to now+lease;
--     Heartbeat pushes it forward. A `running` row whose lease_expires_at has
--     passed is, by construction, owned by nobody alive at that instant
--     (barring a > lease-duration pause, which the retry budget bounds), so a
--     concurrent Claim may hand it to another worker, and RecoverOnStartup maps
--     the surviving rows to `recoverable` at boot.
--
-- Timestamp shape matches 0003's strftime CHECK. Lease/run_after comparisons
-- are TEXT comparisons of same-width UTC (YYYY-MM-DDTHH:MM:SS.mmmZ) stamps
-- written by internal/store/jobs_store.go, so lexicographic order is
-- chronological order — millisecond width because a lease shorter than a
-- second must still compare correctly.
--
-- ALTER TABLE ADD COLUMN keeps both nullable, so every pre-existing row is a
-- valid (unleased) job; there is no backfill and no rewrite of 0001–0004.

ALTER TABLE jobs ADD COLUMN lease_owner TEXT;
ALTER TABLE jobs ADD COLUMN lease_expires_at TEXT CHECK (
	lease_expires_at IS NULL
	OR strftime ('%Y-%m-%dT%H:%M:%S', lease_expires_at) IS NOT NULL
);

-- The takeover scan: "which running jobs has nobody renewed?" Left out of
-- 0003's jobs_claim_idx because state+run_after order cannot answer it; on a
-- quiet appliance it is a scan over a handful of running rows, but the queue
-- also holds thousands of terminal ones, and the reconciler polls.
CREATE INDEX jobs_claim_lease_idx ON jobs (state, lease_expires_at);
