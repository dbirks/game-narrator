-- 0008_agent_approvals.sql — human approval tokens that gate agent
-- publication (docs/spec/03 "Human-in-the-loop boundaries": publishing
-- requires a clear approval in first deployment; ADR-0026).
--
-- A token is issued by a human OUTSIDE the agent loop (the approval button
-- in the UI, or the API) for exactly one profile. The agent may only
-- CONSUME it: publish_profile refuses without a valid, unspent, unexpired
-- token, and no agent tool issues tokens. The table therefore has no
-- agent-facing writer other than the human step's Issue.
--
-- Single-use is a durable predicate, not caller discipline: consumed_at
-- flips exactly once (the UPDATE is guarded by the same predicate the
-- SELECT checks, inside one write transaction), so a crash between the
-- read and the write cannot double-spend a token.
--
-- The raw token is never stored — only its SHA-256. A database leak does
-- not leak publication authority (AGENTS.md rule 5: no credentials in the
-- repo or in data we persist).

CREATE TABLE agent_approvals (
	id TEXT PRIMARY KEY,
	-- The profile this token authorises. One token, one profile.
	profile_id TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	-- SHA-256 of the raw token, lowercase hex. 64 chars exactly.
	token_hash TEXT NOT NULL UNIQUE CHECK (length (token_hash) = 64),
	-- The human who granted the approval (username or "ui"); audit column.
	granted_by TEXT NOT NULL CHECK (length (granted_by) BETWEEN 1 AND 200),
	created_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	expires_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', expires_at) IS NOT NULL),
	-- Set the moment the token is consumed (publish attempted). NULL while
	-- the token is live. The CHECK mirrors 0003's nullable-timestamp
	-- convention.
	consumed_at TEXT CHECK (consumed_at IS NULL
	                       OR strftime ('%Y-%m-%dT%H:%M:%S', consumed_at) IS NOT NULL),
	CHECK (expires_at >= created_at)
);

CREATE INDEX agent_approvals_profile_idx ON agent_approvals (profile_id, created_at);
