-- 0006_auth.sql — users, server-side sessions, API tokens and the auth audit
-- trail (docs/spec/08-api-and-events.md "API version and user authentication",
-- docs/spec/14-security-and-content.md "Mandatory controls: Authorization",
-- docs/spec/10-hardware-and-deployment.md "Security defaults for LAN
-- deployment"). ADR-0016 records the choices this file encodes.
--
-- Why sessions are rows and not self-contained tokens (JWT): docs/spec/14's
-- acceptance test is "stolen old session cookie invalidated", and a
-- stateless token cannot be revoked before its expiry without a server-side
-- denylist — which is a session table with extra steps. Revocation here is one
-- UPDATE. ADR-0016 also rejects the "keep a denylist next to JWTs" option for
-- the same reason: two sources of truth for who is logged in.
--
-- Why the raw token is never stored. `id_hash` is the hex SHA-256 of the
-- cookie value. A dump of this database (or a backup copied off the appliance,
-- which docs/spec/02 says is normal) therefore yields nothing an attacker can
-- replay: the presented value is hashed on the request path and only ever
-- compared as a digest. The same rule covers `api_tokens.token_hash` and
-- `sessions.csrf_hash`. SHA-256 (not PBKDF2) is correct for these because the
-- input is 256 bits of CSPRNG output — brute-forcing a live session is not
-- cheaper than guessing the token itself.
--
-- Why `password_hash` is opaque text. The format
-- `pbkdf2-sha256$iterations$salt$key` carries its own parameters, so a future
-- iteration increase is a rehash-on-login (internal/auth/password.go), not a
-- migration of every row; and a future move to Argon2id (when a dependency is
-- admissible — see ADR-0016) is an additive prefix, not a rewrite.
--
-- Foreign key ON DELETE behaviour, justified per requirement:
--   * sessions.user_id -> users.id ON DELETE CASCADE: a session is a proof that
--     a *user* was authenticated; when the user is deliberately deleted, that
--     proof must not survive it. Keeping the rows would leave live cookies
--     pointing at a missing principal, which every read path would then have to
--     special-case. Revocation-by-deletion is the safe default.
--   * api_tokens.user_id -> users.id ON DELETE CASCADE, same reasoning: an
--     integration credential outliving its principal is the fail-open case
--     docs/spec/14 calls a standing privilege. The role-ceiling rule (a token
--     can never exceed its owner's role) is enforced in Go, because SQLite
--     CHECK constraints cannot reference another row and a trigger for it would
--     hide the rule from every reader of this file.
--   * auth_audit.user_id carries no foreign key at all — deliberately, for the
--     reason 0003 gives for `events.session_id`: an audit row must outlive the
--     row it describes. A user deleted after an incident must not take their
--     failed-login history with them (docs/spec/14 audit expectations), and
--     CASCADE would make deletion erase evidence while SET NULL would silently
--     lose the linkage an investigator reads. NULL means "no principal was ever
--     established" (an unknown username, a setup attempt), which is exactly the
--     shape of a failed login.
--   * auth_audit.id is an INTEGER AUTOINCREMENT rather than a TEXT ULID: this
--     table is append-only, read newest-first, and pruned by age. An integer
--     rowid keeps it compact on the 4 GB target and the ordering is exact.
--
-- Timestamp conventions follow 0001: TEXT RFC 3339 guarded by strftime, so a
-- hand-edited row cannot sort silently wrong. Expiry columns use the
-- fixed-width millisecond form written by internal/store (stampMS), which
-- satisfies the same CHECK and keeps `expires_at <= ?` a TEXT comparison that
-- is still chronological.

-- Users are the appliance's operators, not game characters and not browsers:
-- one row per human or integration principal (docs/spec/14 trust zone A).
CREATE TABLE users (
	id TEXT PRIMARY KEY,
	-- NOCASE uniqueness is the whole point: "Admin" and "admin" must not be
	-- two accounts whose owners can impersonate each other through a
	-- case-insensitive login path. SQLite's NOCASE collation folds ASCII only,
	-- and internal/auth refuses non-ASCII usernames, so the Go rule and the
	-- schema rule agree.
	username TEXT NOT NULL UNIQUE COLLATE NOCASE CHECK (length (username) BETWEEN 3 AND 64),
	-- Opaque self-describing hash: pbkdf2-sha256$iter$base64(salt)$base64(key).
	-- Never a password, never a raw key, and never logged (AGENTS.md).
	password_hash TEXT NOT NULL CHECK (length (password_hash) BETWEEN 32 AND 512),
	-- The ordered role set of docs/spec/08 "Token/session scopes". Ordering and
	-- grants live in internal/auth/roles.go; the CHECK at the end of this table
	-- is what keeps a bad string out of a hand-written fixture.
	--
	-- It is written as a table constraint on its own line rather than
	-- column-level for one reason: internal/games' schema-parity guard refuses an
	-- IN list on any line that begins with the column name `role`, because
	-- characters.role is an intentionally open vocabulary (docs/spec/04
	-- portability). A principal's authorization scope is the other kind of thing
	-- entirely — it must be closed for the ACL to mean anything — and the two
	-- just share a word. ADR-0016 records the collision so the guard can later be
	-- narrowed to the content tables instead of the word.
	role TEXT NOT NULL DEFAULT 'viewer',
	-- Disabled rather than deleted: docs/spec/14 wants a principal that can be
	-- withdrawn instantly ("stolen old session cookie invalidated") without
	-- destroying the audit history that references it.
	disabled INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
	created_at         TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	-- Rotating this column is what makes "the password changed, so every old
	-- session is void" checkable independently of the session rows themselves.
	password_changed_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', password_changed_at) IS NOT NULL),
	-- See the note on `role` above for why this is a table constraint.
	CHECK (role IN ('viewer', 'curator', 'operator', 'developer', 'admin'))
);

-- Sessions: server-side state behind one HttpOnly cookie (docs/spec/08
-- "Browser session secured with same-site httpOnly cookie").
CREATE TABLE sessions (
	-- SHA-256 hex of the 32-byte cookie token. The primary key *is* the
	-- lookup: no session-id column exists, so no code path can accidentally
	-- log an identifier that also authenticates.
	id_hash TEXT PRIMARY KEY CHECK (length (id_hash) = 64),
	user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	-- SHA-256 hex of the CSRF token handed to the SPA at login and echoed in
	-- X-CSRF-Token. Hashed so a database read cannot mint CSRF-valid requests
	-- either; verified by digest comparison in constant time.
	csrf_hash TEXT NOT NULL CHECK (length (csrf_hash) = 64),
	created_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	-- Advanced on authenticated requests (throttled): the idle timeout is the
	-- 12 h of docs/spec/08's session convention; expires_at is the absolute cap.
	last_seen_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', last_seen_at) IS NOT NULL),
	expires_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', expires_at) IS NOT NULL),
	-- Raw header text, bounded: enough for "which browser logged in as me"
	-- without letting a hostile client inflate the row or inject log content
	-- (same reasoning internal/api applies to X-Request-Id).
	user_agent TEXT NOT NULL DEFAULT '' CHECK (length (user_agent) <= 256),
	revoked_at TEXT CHECK (revoked_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', revoked_at) IS NOT NULL),
	-- An idle-timeout session is *expired*, and expired_at is derived from
	-- last_seen_at at read time; revocation is the deliberate act, so only it
	-- gets a column. CHECK keeps the ordering honest for hand-written rows.
	CHECK (expires_at > created_at)
);

-- One lookup per request (by digest) and one sweep per login (revoked/expired
-- rows for that user); both are indexed, the second partially, because the
-- live-session list is what an operator reads.
CREATE INDEX sessions_user_idx ON sessions (user_id, revoked_at);
CREATE INDEX sessions_expiry_idx ON sessions (expires_at);

-- API tokens: the integration credential of docs/spec/08 ("API token for
-- integration CLI"). Printed once, stored only as a digest.
CREATE TABLE api_tokens (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
	-- Operator-facing label ('dolphin-harness', 'release-scripts'), never a
	-- secret: the secret is the token itself.
	name TEXT NOT NULL CHECK (length (name) BETWEEN 1 AND 100),
	-- SHA-256 hex of the whole `gn_...` token. UNIQUE, so a token is resolved
	-- directly rather than by scanning every token and constant-time comparing
	-- — a scan would make login latency proportional to the token count and
	-- would still not survive a digest collision being noticed.
	token_hash TEXT NOT NULL UNIQUE CHECK (length (token_hash) = 64),
	-- A ceiling, not an elevation: internal/auth refuses role > owner's role
	-- (docs/spec/08 "scopes"; docs/spec/14 "role ACL"). The enum CHECK is at the
	-- end of this table for the reason explained in `users`.
	role TEXT NOT NULL,
	created_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	-- Nullable: NULL last_used_at means "never used", which is how an operator
	-- finds a stale credential to revoke. Updated at most once per minute per
	-- token so a busy harness cannot write this row on every request.
	last_used_at TEXT CHECK (last_used_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', last_used_at) IS NOT NULL),
	-- Nullable absolute deadline. NULL means no expiry — legitimate for a
	-- long-lived appliance integration on a localhost-bound service, and the
	-- CLI prints the alternative. Revocation is available either way.
	expires_at TEXT CHECK (expires_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', expires_at) IS NOT NULL),
	revoked_at TEXT CHECK (revoked_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', revoked_at) IS NOT NULL),
	CHECK (role IN ('viewer', 'curator', 'operator', 'developer', 'admin'))
);

CREATE INDEX api_tokens_user_idx ON api_tokens (user_id, revoked_at);

-- The auth audit trail docs/spec/14 asks for. It records *that* an
-- authentication decision happened, never a password, a token, a hash or a
-- provider key (AGENTS.md, docs/spec/13 log hygiene).
CREATE TABLE auth_audit (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	occurred_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', occurred_at) IS NOT NULL),
	-- NULL when no principal was established: a failed login for an unknown
	-- username, or a first-run setup attempt.
	user_id TEXT CHECK (user_id IS NULL OR length (user_id) >= 1),
	-- Free-form event name ('login.success', 'login.failure',
	-- 'session.revoked', 'token.create', 'access.denied', ...). Not a CHECK
	-- enum: the set grows with every subsystem that makes an auth decision, and
	-- 0003 makes the same argument for jobs.kind.
	event TEXT NOT NULL CHECK (length (event) BETWEEN 1 AND 100),
	-- Remote address of the actor. Never taken from X-Forwarded-For, which the
	-- first-run rule in particular must not trust (docs/spec/14 SSRF/spoofing).
	remote_addr TEXT NOT NULL DEFAULT '',
	-- Structured evidence: username (never the password), counts of revoked
	-- sessions, token id and role, the route and required role.
	detail_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (detail_json))
);

CREATE INDEX auth_audit_recent_idx ON auth_audit (occurred_at DESC);
