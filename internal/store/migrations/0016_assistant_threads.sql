-- 0016_assistant_threads.sql — the context-aware assistant agent (ADR-0099,
-- docs/design/plum-peach UX-01/UX-04/UX-05): per-user threads and the
-- questions/confirmations a thread's run raises.
--
-- A thread is one durable agent run (agent_runs, migration 0007): the run
-- log IS the thread's conversation, so this migration adds only the
-- ownership mapping and the question rows the run's SSE cards need. The
-- conversation itself (and the tool calls behind the dock's tool cards)
-- lives in agent_messages / tool_calls, untouched.
--
-- Questions never block the run (UX-04, ADR-0099): a row here is a card the
-- user may answer later. `kind` distinguishes the model's free-form question
-- (ask_user) from a mutating tool's confirmation card, and `applied` +
-- `answer_seq` are the post-run follow-up predicate: the job handler
-- enqueues one follow-up attempt when an answer's durable message (seq in
-- agent_messages) postdates the attempt's start.
--
-- JSON columns are json_valid-checked at the schema, the same guard the
-- agent run log uses (0007) and every other JSON column in this schema.

CREATE TABLE assistant_threads (
	id TEXT PRIMARY KEY,
	-- The owning user (auth users.id): threads are per-user (ADR-0099), and
	-- the API's ownership check is this column, never the run id alone.
	user_id TEXT NOT NULL,
	-- The thread's durable conversation log (agent_runs.id). UNIQUE: one
	-- thread owns exactly one run; ON DELETE CASCADE so a run deleted by
	-- retention takes its thread mapping, not the reverse.
	run_id TEXT NOT NULL UNIQUE REFERENCES agent_runs (id) ON DELETE CASCADE,
	-- Display title (the first message, truncated at write time). Bounded the
	-- way every user-titled column is: 200 chars, the games.title bound.
	title TEXT NOT NULL CHECK (length (title) BETWEEN 1 AND 200),
	created_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	updated_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', updated_at) IS NOT NULL),
	CHECK (updated_at >= created_at)
);

-- Listing is per-user, newest first: the index is the list's shape.
CREATE INDEX assistant_threads_user_idx ON assistant_threads (user_id, updated_at DESC);

-- One row per NEEDS-YOU card or confirmation a thread's run raised. The
-- run_id is denormalised (the thread owns it) so the question can be read
-- against the run's log without the join, and is CHECK-tied to the thread's
-- run by the writer (assistant_store.go), the same way tool_calls ties its
-- rows to the run it belongs to.
CREATE TABLE assistant_questions (
	id TEXT PRIMARY KEY,
	thread_id TEXT NOT NULL REFERENCES assistant_threads (id) ON DELETE CASCADE,
	-- The run that raised the card (the thread's run; the writer verifies
	-- the match, the schema cannot express it without a subquery join).
	run_id TEXT NOT NULL,
	-- 'question': the model asked the user (ask_user); 'confirmation': a
	-- mutating tool staged an action and the card is its confirm/cancel.
	kind TEXT NOT NULL CHECK (kind IN ('question', 'confirmation')),
	-- The card's prompt (the model's question, or the staged action summary).
	prompt TEXT NOT NULL CHECK (length (prompt) BETWEEN 1 AND 2000),
	-- The offered options as a JSON array of strings (['confirm','cancel']
	-- for confirmations; the model's own options, or [] for open questions).
	-- json_valid keeps the card renderable from a hand-edited row.
	options_json TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (options_json)),
	-- The user's answer (the chosen option or free text). NULL while open;
	-- the CHECK below makes answered and open a closed pair.
	answer TEXT,
	-- The seq of the durable user message that carries the answer into the
	-- run's conversation (agent_messages.seq). -1 while the answer has not
	-- been appended yet (an open card, or a card answered before the append
	-- lands); the post-run follow-up predicate compares it against the
	-- attempt's start seq.
	answer_seq INTEGER NOT NULL DEFAULT -1 CHECK (answer_seq >= -1),
	-- The screen context at ask time ({route, game_id, profile_id,
	-- selection, visible_ids}) and, for a confirmation, the staged action
	-- ({tool, args}) the confirmation token binds to. NULL = not captured.
	context_json TEXT CHECK (context_json IS NULL OR json_valid (context_json)),
	-- 1 once the answer is durable in the run's conversation (the user
	-- message above was appended). The writer owns the order: mark applied
	-- only after the append succeeds, so a crash leaves the card answerable
	-- again (the handler appends + marks in one transaction).
	applied INTEGER NOT NULL DEFAULT 0 CHECK (applied IN (0, 1)),
	created_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	-- When the user answered (NULL while open).
	answered_at TEXT CHECK (answered_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', answered_at) IS NOT NULL),
	CHECK (
		(answer IS NULL AND applied = 0)
		OR
		(answer IS NOT NULL AND applied = 1 AND answered_at IS NOT NULL AND answer_seq >= 0)
	)
);

-- The dock lists a thread's open cards newest-first, and the follow-up hook
-- scans one thread's answered rows: the index is both shapes.
CREATE INDEX assistant_questions_thread_idx ON assistant_questions (thread_id, created_at DESC);
