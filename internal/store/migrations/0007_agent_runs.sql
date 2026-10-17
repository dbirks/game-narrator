-- 0007_agent_runs.sql — durable agent run log: runs, per-step messages and
-- tool calls (docs/spec/03 "durable tool/job log and resumable plan",
-- docs/spec/17 "Agent tool result"). ADR-0025 records the choices this file
-- encodes.
--
-- Why rows and not a single blob: docs/spec/03 scenario 7 requires that an
-- interrupted run (e.g. a 3,000-line import killed mid-flight) resumes without
-- duplicates. That is only expressible if every tool call is its own row whose
-- finished state is a durable predicate: a resume rebuilds the conversation
-- from agent_messages and re-executes *only* calls that have no persisted
-- result. A single JSON blob would force the resume to trust the blob.
--
-- State vocabulary is the closed set from docs/spec/03: queued, running,
-- blocked, review, failed, succeeded, cancelled. `blocked` is a normal
-- outcome, not an error: budget exhaustion or the repetition guard leave the
-- run resumable once a human raises the ceiling or the model is asked again.
--
-- Message and tool-call ordering is the integer `seq` per run, not a
-- timestamp: two steps in one process tick must still order deterministically,
-- and the resume path replays exactly in this order. `created_at` remains for
-- audit.
--
-- JSON columns are guarded by json_valid so a hand-edited or crashed row can
-- never silently break the JSON contract the agent loop and the developer UI
-- both rely on (same convention as 0003/0006).

CREATE TABLE agent_runs (
	id TEXT PRIMARY KEY,
	-- Profile being onboarded. NULL while the run is still in game
	-- identification: the run must exist before the profile does, so the FK
	 -- cannot be NOT NULL. The FK enforces the reverse direction (a run
	-- pointing at a deleted profile) only when a profile is set.
	profile_id TEXT REFERENCES profiles (id) ON DELETE SET NULL,
	-- Closed set from docs/spec/03 "checkpoint".
	state TEXT NOT NULL DEFAULT 'queued'
		CHECK (state IN ('queued', 'running', 'blocked', 'review',
		                'failed', 'succeeded', 'cancelled')),
	-- The resumable plan (explicit step list, DAG node ids) as validated JSON.
	plan_json TEXT NOT NULL CHECK (json_valid (plan_json)),
	-- Budget ceilings actually in force for this run (per-tool call caps,
	-- step cap, deadline) frozen at start: a later config change must not
	-- silently raise the ceiling mid-run (docs/spec/14 "Abuse and resource
	-- controls").
	budget_json TEXT NOT NULL CHECK (json_valid (budget_json)),
	-- Which version of the system prompt template rendered the run
	-- (docs/agents/system-prompt-onboarding.md is versioned). The developer
	-- UI uses this to show the prompt a "weird" run saw.
	prompt_version TEXT NOT NULL CHECK (length (prompt_version) BETWEEN 1 AND 32),
	-- Model id the run used (or, on the first attempt, was assigned), for the
	-- evidence bundle. Never a URL or token (AGENTS.md).
	model TEXT NOT NULL DEFAULT '',
	-- Free-form machine-readable stop reason for blocked/failed/review
	-- ('budget_exhausted', 'repetition', 'review_required:<tool>', ...).
	stop_reason TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	updated_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', updated_at) IS NOT NULL),
	CHECK (updated_at >= created_at)
);

CREATE INDEX agent_runs_state_idx ON agent_runs (state, updated_at);

-- Every conversational step of the run: the model's requests are replayed on
-- resume exactly as stored, so role is the closed chat-protocol set
-- (system/user/assistant/tool). Non-message checkpoints belong on the run
-- row's state/stop_reason, not in the conversation.
CREATE TABLE agent_messages (
	run_id TEXT NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
	-- Per-run sequence, starts at 0. UNIQUE (run_id, seq) makes a concurrent
	-- writer impossible: the second INSERT loses the race and gets a
	-- constraint error instead of a duplicate row.
	seq INTEGER NOT NULL CHECK (seq >= 0),
	-- Closed chat-protocol vocabulary (system/user/assistant/tool), not the
	-- open per-game role vocabulary of docs/spec/04. Written as a table-level
	-- CHECK so the line does not start with "role … IN (":
	-- internal/games.TestOpenVocabularyStaysOpen scans for that shape and is
	-- there to guard the game-data columns.
	role TEXT NOT NULL,
	-- Full message payload in the provider-neutral wire shape the loop built
	-- (content, tool_calls, tool_call_id). json_valid keeps the replay
	-- trustworthy.
	content_json TEXT NOT NULL CHECK (json_valid (content_json)),
	created_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	PRIMARY KEY (run_id, seq),
	CHECK (role IN ('system', 'user', 'assistant', 'tool'))
);

-- One row per tool invocation. The (idempotence-critical) contract:
--   * status 'pending' means started but no persisted result yet: on resume
--     this is the call that may be executed;
--   * any other status means a durable outcome exists and the call is
--     NEVER re-executed (docs/spec/03 scenario 7, "resumes without
--     duplicates"). The loop encodes every outcome — including refusals —
--     into result_json before finishing the row.
CREATE TABLE tool_calls (
	id TEXT PRIMARY KEY,
	run_id TEXT NOT NULL REFERENCES agent_runs (id) ON DELETE CASCADE,
	-- Per-run sequence of the call within the whole step stream (a step may
	-- contain several calls; their seq values are consecutive).
	seq INTEGER NOT NULL CHECK (seq >= 0),
	name TEXT NOT NULL CHECK (length (name) BETWEEN 1 AND 100),
	args_json TEXT NOT NULL CHECK (json_valid (args_json)),
	-- Persisted outcome: the wrapped result envelope
	-- {schema_version, tool_call_id, name, status, results|error, source_ids}.
	-- NULL exactly while status is 'pending' (enforced as a table CHECK,
	-- below).
	result_json TEXT CHECK (result_json IS NULL OR json_valid (result_json)),
	status TEXT NOT NULL DEFAULT 'pending'
		CHECK (status IN ('pending', 'succeeded', 'failed', 'refused',
		                  'timeout', 'cancelled')),
	started_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', started_at) IS NOT NULL),
	finished_at  TEXT CHECK (finished_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', finished_at) IS NOT NULL),
	-- Short human-readable failure line; never a stack, never a credential.
	error TEXT NOT NULL DEFAULT '',
	CHECK (
		(status = 'pending' AND finished_at IS NULL AND result_json IS NULL)
		OR (status <> 'pending' AND finished_at IS NOT NULL AND result_json IS NOT NULL)
	)
);

-- Resume lookup: "which calls of this run still lack a durable outcome?" and
-- developer-UI listing "the Nth step ran these tools".
CREATE INDEX tool_calls_run_idx ON tool_calls (run_id, seq);
CREATE INDEX tool_calls_pending_idx ON tool_calls (run_id) WHERE status = 'pending';
