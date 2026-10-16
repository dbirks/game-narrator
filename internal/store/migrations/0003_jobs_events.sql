-- 0003_jobs_events.sql — the durable work queue and the append-only event log
-- from docs/spec/02-data-and-storage.md ("Entity catalog": "jobs,
-- job_attempts … detailed job state, parent chains, replay/resume tokens,
-- reviewer, audit trail") and docs/spec/08-api-and-events.md ("APIs": jobs and
-- `GET /api/v1/events` SSE; "Event stream reliability": Last-Event-ID replay
-- from a bounded event log).
--
-- Why the queue is a table and not goroutines: docs/spec/01 puts "durable
-- workflows" in the same process domain as the API, and docs/spec/08 exposes
-- `POST /jobs/:id/cancel|retry`, which has to survive a restart of the
-- appliance. An in-memory queue loses an import halfway through every crash;
-- docs/spec/02 "Transaction workflows" requires the opposite — "interrupted
-- `running` jobs moved to `recoverable`".
--
-- Why the log is a table: SSE reconnects must replay from `Last-Event-ID`
-- (docs/spec/08), and the developer inspector (`/api/v1/dev/traces/:id`) has to
-- show the same correlated events a browser saw, after the fact. That needs a
-- server-side global order — hence `seq INTEGER PRIMARY KEY AUTOINCREMENT`,
-- which is monotonic across connections and processes writing this database,
-- unlike a timestamp at RFC 3339 second resolution.
--
-- Conventions inherited from 0001_init.sql: TEXT public IDs, TEXT RFC 3339
-- timestamps guarded by a strftime CHECK, CHECK-guarded enums, JSON payloads
-- validated with json_valid and interpreted only by Go.
--
-- Foreign key ON DELETE behaviour, justified per requirement:
--   * jobs.parent_id -> jobs(id) CASCADE: a parent chain is a decomposition of
--     one piece of work (import → parse → graph build). The children have no
--     independent life — dropping the root of a cancelled pipeline must not
--     leave orphan stages that a reconciler would later resurrect and run.
--     Self-referential, nullable: a root job has no parent.
--   * job_attempts.job_id -> jobs(id) CASCADE: an attempt row is the history of
--     one queued job; once the job is intentionally removed the audit rows for
--     it are noise, and they can never be re-attached. Retention of *live* job
--     history is bounded by the reconciler deleting old terminal jobs, which is
--     a policy decision in docs/spec/13, not by an SQLite constraint.
--   * events has **no** foreign keys at all. session_id and causation_id point
--     at the operational `sessions` table and at other events, neither of which
--     exists before the operational migration, and the log is append-only and
--     pruned by age: a reference constraint would make pruning a parent row
--     fail, which is precisely how an appliance ends up with an unbounded event
--     table on 4 GB of storage. Dangling ids are data, and the API reports a
--     gap as `events.resync_required` instead (docs/spec/08).
--   * events.seq is AUTOINCREMENT rather than a bare INTEGER PRIMARY KEY so a
--     rowid reused after a pruning `DELETE` can never be handed out twice: a
--     browser holding `Last-Event-ID` must not silently skip or repeat an
--     event because its cursor now names a different one. SQLite keeps
--     `sqlite_sequence` monotonic even after the max row is deleted, at the
--     cost of a gap after a rollback — gaps are harmless here, aliasing is not.

CREATE TABLE jobs (
	id              TEXT PRIMARY KEY,
	-- Free-form job class ('import.parse', 'onboarding.run', 'voice.register').
	-- Not an enum: the set grows with every adapter and agent tool, and a
	-- schema edit per new job type would couple the queue to feature work.
	kind            TEXT NOT NULL,
	-- recoverable: a `running` row left behind by a crash, which the reconciler
	--   must decide to resume from resume_token or fail (docs/spec/02
	--   "Transaction workflows").
	-- requires_input: parked on a human answer, e.g. JOB_REQUIRES_INPUT in
	--   docs/spec/08's error contract, and therefore must never be picked up by
	--   a worker.
	state           TEXT NOT NULL DEFAULT 'queued' CHECK (state IN ('queued', 'running', 'succeeded', 'failed', 'cancelled', 'recoverable', 'requires_input')),
	-- 0 = ordinary. Bounded 0..100 so one caller cannot ask for "priority 10^9"
	-- and starve everything else forever; the picker orders by
	-- (priority DESC, run_after ASC, seq of insertion) per queue.
	priority        INTEGER NOT NULL DEFAULT 0 CHECK (priority >= 0 AND priority <= 100),
	-- Inputs the worker needs. Validated as JSON only: every job kind has its
	-- own shape, checked in Go against that kind's schema.
	payload_json    TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (payload_json)),
	-- Output of the successful attempt, or the structured error of the last
	-- failure. NULL until the job reaches a terminal state.
	result_json     TEXT CHECK (result_json IS NULL OR json_valid (result_json)),
	-- Parent chain (see the ON DELETE justification above).
	parent_id       TEXT REFERENCES jobs (id) ON DELETE CASCADE,
	-- Opaque checkpoint the worker uses to resume without redoing completed
	-- stages (docs/spec/02: "Import resume by content hash and job_id";
	-- "tool outputs and parsed partial drafts checkpoint every stage"). NULL
	-- for jobs with nothing to resume from.
	resume_token    TEXT,
	-- Attempts made so far. The CHECK pair is what makes AGENTS.md rule 11
	-- ("Retry only idempotent operations with max attempts") a storage
	-- invariant instead of a code-review hope: a worker physically cannot record
	-- a 40th attempt against a job allowed 3.
	attempts        INTEGER NOT NULL DEFAULT 0 CHECK (attempts >= 0),
	max_attempts    INTEGER NOT NULL DEFAULT 1 CHECK (max_attempts >= 1),
	-- Earliest instant the job may start: exponential backoff for retries and
	-- deliberate scheduling for prewarming. NULL = immediately eligible.
	run_after       TEXT CHECK (run_after IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', run_after) IS NOT NULL),
	-- Correlation id shared with the events log, so a UI can follow a job from
	-- queued to completed without joining on ids.
	trace_id        TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	updated_at      TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', updated_at) IS NOT NULL),
	-- `Idempotency-Key` from docs/spec/08: a retried POST must return the job
	-- it already created rather than starting a second import. NULL is
	-- allowed (internal jobs have no client key) and SQLite treats NULLs as
	-- distinct inside UNIQUE, so unsupervised rows never collide on it.
	idempotency_key TEXT UNIQUE,
	-- Table-level constraints must follow every column definition: SQLite's
	-- CREATE TABLE grammar rejects a column def after a constraint, so this
	-- stays last even though it belongs conceptually to the pair above.
	-- Attempts may catch up to the ceiling but never pass it, which is what
	-- makes the bounded-retry rule unbreakable from the worker side.
	CHECK (attempts <= max_attempts)
);

-- The worker's claim query: eligible jobs, most urgent first, respecting
-- backoff. Without this index every poll is a table scan on an appliance that
-- also has to do OCR in real time.
CREATE INDEX jobs_claim_idx ON jobs (state, run_after, priority DESC, created_at);
-- Walk the chain top-down (progress roll-up) and bottom-up (cancel cascade).
CREATE INDEX jobs_parent_id_idx ON jobs (parent_id);
-- "What is this retried POST referring to?" lookups are the idempotency path.
CREATE INDEX jobs_trace_id_idx ON jobs (trace_id);

CREATE TABLE job_attempts (
	job_id      TEXT NOT NULL REFERENCES jobs (id) ON DELETE CASCADE,
	-- 1-based attempt number; UNIQUE with job_id records the whole retry
	-- history without a counter that could double-write on a replay.
	attempt     INTEGER NOT NULL CHECK (attempt >= 1),
	started_at  TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', started_at) IS NOT NULL),
	-- NULL while the attempt is in flight; an attempt that never finished is
	-- the crash-recovery evidence the reconciler needs.
	finished_at TEXT CHECK (finished_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', finished_at) IS NOT NULL),
	-- Terminal class of this one attempt. 'crashed' and 'timed_out' are kept
	-- distinct from 'failed' because the retry policy differs: a deadline is
	-- worth another attempt at the same input, a deterministic validation
	-- failure is not (docs/spec/06 "Retry idempotent inference calls … only if
	-- event still current").
	outcome     TEXT NOT NULL CHECK (outcome IN ('running', 'succeeded', 'failed', 'cancelled', 'timed_out', 'crashed', 'requires_input')),
	-- Human/model-readable failure summary; provider request ids and other
	-- structure belong in the event log, not here, and never an API key
	-- (docs/spec/14 redacts secrets in traces).
	error       TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (job_id, attempt),
	-- Chronology must not run backwards. strftime('%s') compares the two
	-- instants numerically, which the text-form CHECKs alone cannot guarantee:
	-- RFC 3339 strings with different offsets do not sort chronologically.
	CHECK (finished_at IS NULL
		OR CAST (strftime ('%s', finished_at) AS INTEGER) >= CAST (strftime ('%s', started_at) AS INTEGER))
);

CREATE TABLE events (
	seq            INTEGER PRIMARY KEY AUTOINCREMENT,
	-- Public cursor carried as SSE `id:` (docs/spec/08). A ULID so a client can
	-- sort by wall clock too, while `seq` remains the authoritative local
	-- order across concurrent writers.
	event_id       TEXT NOT NULL UNIQUE,
	-- NULL for global events (job state changes, diagnostics, config edits)
	-- that belong to no recognition session.
	session_id     TEXT,
	-- Event name from docs/spec/01 "High-level events" and docs/spec/08
	-- (frame.observed, text.observed, message.candidates, decision.resolved,
	-- speech.started, session.stopped, agent.step, tool.completed, …). Open
	-- vocabulary on purpose: emitting a new event must not need a migration,
	-- and consumers switch on the name plus schema_version.
	name           TEXT NOT NULL,
	trace_id       TEXT NOT NULL DEFAULT '',
	-- id of the event that caused this one (docs/spec/01: "causation"), for the
	-- developer inspector's causal chain. NULL for a root event.
	causation_id   TEXT,
	occurred_at    TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', occurred_at) IS NOT NULL),
	-- Version of data_json's shape, so a reader built against an older client
	-- can still decode a newer log entry or reject it explicitly.
	schema_version INTEGER NOT NULL DEFAULT 1 CHECK (schema_version >= 1),
	-- Event envelope payload. The envelope itself (docs/spec/17 "Event
	-- envelope") is assembled in Go from these columns; only the `data` member
	-- lives here.
	data_json      TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (data_json))
);

-- Session-scoped SSE replay: "give me everything after seq N for this
-- session" is answered by an index-only scan, and the ordering is the index
-- order, so no sort is needed on the hot path.
CREATE INDEX events_session_seq_idx ON events (session_id, seq);
-- GET /api/v1/dev/traces/:id — pull every event of one decision by correlation.
CREATE INDEX events_trace_seq_idx ON events (trace_id, seq);
-- Pruning old entries by age (docs/spec/02 "bounded operational store") has to
-- find the boundary without scanning the log.
CREATE INDEX events_occurred_at_idx ON events (occurred_at);
