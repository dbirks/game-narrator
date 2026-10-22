-- 0019_abandoned_run_state.sql — the operator-initiated run stop (issue #45,
-- ADR-0028): an onboarding run that is waiting (blocked on a provider fault or
-- a human, stopped by a pause) and has no attempt in flight moves to the
-- terminal `abandoned` state through POST /api/v1/onboarding/runs/{id}/abandon.
--
-- The state vocabulary is the closed set from docs/spec/03 "checkpoint",
-- widened by one: abandoned joins queued, running, blocked, review, failed,
-- succeeded and cancelled. SQLite cannot ALTER TABLE ... DROP CONSTRAINT, so
-- the widening is done by table rebuild: create the next shape carrying the
-- extended CHECK, copy the rows, drop the previous table and rename. The copy
-- reads the OLD CHECK because the rows predate the new state; the CHECKs on the
-- source data are satisfied by the old vocabulary by construction. The index
-- rides along with the new table and is recreated after the rename.

CREATE TABLE agent_runs_abandoned (
	id TEXT PRIMARY KEY,
	-- Profile being onboarded. NULL while the run is still in game
	-- identification: the run must exist before the profile does, so the FK
	-- cannot be NOT NULL. The FK enforces the reverse direction (a run
	-- pointing at a deleted profile) only when a profile is set.
	profile_id TEXT REFERENCES profiles (id) ON DELETE SET NULL,
	-- Closed set from docs/spec/03 "checkpoint", now including abandoned.
	state TEXT NOT NULL DEFAULT 'queued'
		CHECK (state IN ('queued', 'running', 'blocked', 'review',
		                'failed', 'succeeded', 'cancelled', 'abandoned')),
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
	-- ("budget_exhausted", "repetition", "review_required:<tool>", ...) and
	-- for an operator stop ("abandoned").
	stop_reason TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	updated_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', updated_at) IS NOT NULL),
	CHECK (updated_at >= created_at)
);

INSERT INTO agent_runs_abandoned (id, profile_id, state, plan_json, budget_json, prompt_version, model, stop_reason, created_at, updated_at)
	SELECT id, profile_id, state, plan_json, budget_json, prompt_version, model, stop_reason, created_at, updated_at
	FROM agent_runs;

DROP TABLE agent_runs;
ALTER TABLE agent_runs_abandoned RENAME TO agent_runs;

CREATE INDEX agent_runs_state_idx ON agent_runs (state, updated_at);
