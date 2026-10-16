-- 0004_publication.sql — the publication facts docs/spec/02 "Entity catalog"
-- promises for the catalogue rather than for the operational log: the optimistic
-- concurrency counter behind every ETag (docs/spec/08 "Conventions" /
-- `PROFILE_REVISION_CONFLICT`), the pointer that says which revision an edition
-- serves (docs/spec/02 "Transaction workflows": "atomic publication pointer swap",
-- docs/spec/01 "Runtime and setup isolation"), and the `publish_actions` audit
-- trail ("publish_actions … reviewer, audit trail").
--
-- Why these are columns and a table, and not event-log rows. An earlier build
-- kept all three in 0003_jobs_events.sql's `events` table. That log is explicitly
-- *bounded and pruned by age* (docs/spec/02 "Entity catalog": "bounded operational
-- store separated from profile data"), so a fact stored only there can disappear
-- while the row it describes survives. For a counter that means a revision's
-- version can restart at 1 after pruning and a months-old `If-Match` token can
-- match again — a fail-*open* concurrency bug, the one direction docs/spec/12's
-- "no published edit without valid revision token" must never allow. ADR-0015
-- records the decision and the rejection.
--
-- Conventions inherited from 0001_init.sql: TEXT public IDs, TEXT RFC 3339
-- timestamps guarded by a strftime CHECK, CHECK-guarded enums, JSON payloads
-- validated with json_valid and interpreted only by Go.
--
-- Foreign key ON DELETE behaviour, justified per requirement:
--   * editions.published_profile_id -> profiles(id) ON DELETE RESTRICT. Nullable:
--     an edition with nothing shipped serves nothing (docs/spec/13's degraded mode
--     makes a session refuse to start rather than improvise). RESTRICT rather than
--     CASCADE or SET NULL, because both alternatives would silently *change what a
--     live appliance narrates* as a side effect of a delete nobody asked for:
--     CASCADE would delete a curated, reviewed revision merely because somebody
--     tidied an edition row, and SET NULL would un-publish the current release
--     without an audit trail — sessions would find their pinned revision gone from
--     under them, which docs/spec/01 forbids. RESTRICT forces the honest order,
--     retirement first (an audited act through Store.Publish/SetProfileStatus),
--     deletion second, and it is the same reasoning 0001 applies to
--     profiles.edition_id. Note the direction: deleting the *edition* row is
--     unrestricted (the pointer is a child), so a game with no revisions can still
--     cascade away as 0001 promises.
--   * publish_actions.profile_id -> profiles(id) ON DELETE RESTRICT. An audit row
--     that outlived its subject would describe nothing and could not be
--     cross-checked against the manifest digest it recorded, so a revision with
--     publication history cannot be deleted out from under its own trail. The
--     audit trail is what docs/spec/16's evidence bundles are built from; deleting
--     either side is a deliberate act, never a cascade.
--   * publish_actions.edition_id carries no foreign key. 0002_content.sql argues
--     the same case for corrections.target_id: the audit record must survive the
--     removal of what it describes in the one direction that SQLite would allow
--     (an edition with no revisions left can cascade away; its decided history
--     stays readable for the compliance review docs/spec/14 requires). The
--     repositories always write a real edition id, checked against the profile
--     they just loaded.

-- The optimistic-locking counter. DEFAULT 1 rather than 0: creating a revision is
-- itself a write, so the first ETag a client receives is 1 and no caller has to
-- special-case "not written yet". CHECK >= 1 keeps a hand-written row from
-- introducing a version no token can ever equal.
--
-- SQLite applies ADD COLUMN defaults to existing rows without rewriting them and
-- does not evaluate the CHECK against them, which is exactly the migration
-- behaviour wanted here: every revision that already exists has been written at
-- least once, so 1 is the true value for all of them, and
-- TestUpgradeFromThePriorSchemaBackfillsThePointer proves the repositories go on
-- working against those rows.
ALTER TABLE profiles ADD COLUMN row_version INTEGER NOT NULL DEFAULT 1
	CHECK (row_version >= 1);

-- Which revision an edition currently serves. Nullable by design (an edition that
-- has never shipped); the status column below is not enough on its own because
-- "what is live" must be one indexed lookup that cannot return two answers.
ALTER TABLE editions ADD COLUMN published_profile_id TEXT
	REFERENCES profiles (id) ON DELETE RESTRICT;

-- The schema half of "a session asks what is live and must get one answer": at
-- most one published revision per edition, enforced even for a write that bypasses
-- the repositories. Partial, so only published rows participate and every draft
-- and archived revision of the edition is untouched. TestPublishingAgainRetiresThePredecessor
-- shows the repositories never violate it (they retire the incumbent before
-- promoting the successor, inside one BEGIN IMMEDIATE transaction); this index is
-- the backstop that makes the invariant true of the *file*, not just of the code
-- that was written today.
--
-- Consequence worth knowing: applying this to a database that already holds two
-- published revisions of one edition fails loudly instead of silently picking one,
-- which is the correct outcome for state the repositories could not have produced.
CREATE UNIQUE INDEX profiles_one_published_idx
	ON profiles (edition_id) WHERE status = 'published';

-- docs/spec/02 "Entity catalog": `publish_actions` … "reviewer, audit trail".
-- One row per publication-lifecycle decision, written in the same transaction as
-- the state it describes — except a gate refusal, which is committed on its own
-- after the failed publication is rolled back, because the refusal *is* the
-- outcome and its evidence must survive the rollback.
CREATE TABLE publish_actions (
	id          TEXT PRIMARY KEY,
	-- The revision the decision was about. For action 'archived' produced by a
	-- successor's publication, that is the revision being retired, which is why
	-- an edition's history reads as a sequence of decisions rather than as a list
	-- of what shipped.
	profile_id  TEXT NOT NULL REFERENCES profiles (id) ON DELETE RESTRICT,
	-- Denormalised on purpose: the query an auditor or the developer UI asks is
	-- "this edition's publication history", and answering it through
	-- profiles.edition_id would join through rows that can be deleted
	-- independently of the audit record.
	edition_id  TEXT NOT NULL,
	-- 'published' = this revision became what the edition serves.
	-- 'archived'  = this revision stopped serving it.
	-- 'rejected'  = the publication gate refused this revision; nothing changed.
	-- There is no 'review' entry: submitting for review is a status transition
	-- (docs/spec/02), not a publication decision, and Store.SetProfileStatus has
	-- no actor to attribute it to.
	action      TEXT NOT NULL CHECK (action IN ('published', 'archived', 'rejected')),
	-- Who decided. NOT NULL with no default: an audit trail whose actor column can
	-- be left empty is an audit trail that silently loses accountability, so the
	-- schema refuses the row and Go's PublishAction.Validate refuses earlier.
	-- Unattributed writes name 'unknown' explicitly.
	author      TEXT NOT NULL CHECK (length(author) BETWEEN 1 AND 200),
	-- The revision's row_version *after* this decision. A rejected publication
	-- changed nothing, so it stores the version the caller presented — the pair
	-- (expected, stored) is how a reader distinguishes "this publish moved the
	-- revision" from "this publish was refused".
	row_version INTEGER NOT NULL CHECK (row_version >= 1),
	-- The remainder of the record, fixed-shape and interpreted only by Go (the
	-- convention every JSON column in this schema follows): the gate's problem
	-- list, the manifest digest that was validated, the caller's expected version,
	-- the superseded revision, and any extra context the writer had.
	detail_json TEXT NOT NULL DEFAULT '{}' CHECK (json_valid(detail_json)),
	occurred_at TEXT NOT NULL
		CHECK (strftime ('%Y-%m-%dT%H:%M:%S', occurred_at) IS NOT NULL)
);

-- "Show this edition's publication history, newest first" — the only audit query
-- the review screen and the developer inspector make.
CREATE INDEX publish_actions_edition_time_idx
	ON publish_actions (edition_id, occurred_at);

-- Backfill for a database written before this migration. Older builds put the
-- fact "this revision is published" in profiles.status alone (and before them, in
-- the event log), so the pointer starts NULL while a published row exists. Without
-- this, upgrading an appliance would silently un-publish its live release:
-- PublishedProfile reads the pointer, and a session that cannot name a revision
-- must refuse to start (docs/spec/13).
--
-- The subquery is `LIMIT 1` + `ORDER BY revision_num DESC` as a defensive
-- tie-break for a hand-repaired database holding two published revisions of one
-- edition; the partial unique index above is created before this statement and
-- refuses that state anyway, so in practice at most one row matches.
UPDATE editions
	SET published_profile_id = (
		SELECT p.id FROM profiles p
		 WHERE p.edition_id = editions.id AND p.status = 'published'
		 ORDER BY p.revision_num DESC LIMIT 1
	)
	WHERE EXISTS (
		SELECT 1 FROM profiles p
		 WHERE p.edition_id = editions.id AND p.status = 'published'
	);
