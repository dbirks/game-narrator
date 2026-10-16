-- 0001_init.sql — the first catalog tables from docs/spec/02-data-and-storage.md
-- ("Entity catalog").
--
-- Conventions (docs/spec/02 "SQLite schema principles"):
--   * Public identifiers are TEXT holding UUID or ULID strings; generated
--     canonical IDs never reuse a source-side identity column.
--   * Timestamps are TEXT in RFC 3339 with an explicit offset (UTC preferred).
--     Storing them as text keeps dumps readable and avoids SQLite's loose
--     date typing. Each timestamp column carries the same CHECK: SQLite has no
--     date type, so an unparsable string would otherwise sort and compare
--     silently. The check uses strftime, which returns NULL for anything
--     SQLite cannot read as a date/time; it accepts a bare date (e.g.
--     '2026-10-16') as a convenience for hand-written fixtures.
--   * Large images/WAVs never live in SQLite. Binary content is
--     content-addressed on disk and referenced by source_blobs.storage_locator.
--   * This migration creates only games, editions, profiles, sources and
--     source_blobs. Messages, characters, voices, scenes, regions and the
--     operational/job tables arrive in later numbered migrations so each
--     schema revision stays reviewable and reversible.
--
-- Foreign key ON DELETE behaviour (justified per requirement):
--   * editions.game_id -> games.id ON DELETE CASCADE: an edition is a
--     region/language/platform/revision binding of exactly one title family and
--     has no meaning once that title is removed, so it is deleted with it.
--   * profiles.edition_id -> editions.id ON DELETE RESTRICT: a profile revision
--     is curated, reviewed and published work. It must never vanish as a side
--     effect of deleting an edition, so SQLite refuses the deletion while
--     revisions exist and the caller has to retire revisions deliberately
--     (docs/spec/02 "Transaction workflows"). Because RESTRICT aborts the whole
--     cascade, a game that still has profile revisions cannot be dropped by a
--     cascading delete either — the intended safety property for an appliance
--     holding user-curated profiles.
--   * profiles.parent_revision: self-referential composite key on
--     (edition_id, parent_revision) -> (edition_id, revision_num), so a
--     revision chain can never cross editions, and RESTRICT keeps an
--     already-derived revision from orphaning when its parent is removed.
--   * sources.profile_id -> profiles.id ON DELETE CASCADE: a source row only
--     documents where one profile's content came from. Once the profile is gone
--     the provenance row describes nothing and would dangle.
--   * source_blobs deliberately has no foreign keys: blobs are content-addressed
--     and shared between owners, so their lifetime is decided by reachability
--     GC over all referrers, not by any single row disappearing
--     (docs/spec/02 "Asset store contract": "reference counts are advisory").

CREATE TABLE games (
	id          TEXT PRIMARY KEY,
	slug        TEXT NOT NULL UNIQUE,
	title       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	platform    TEXT NOT NULL DEFAULT '',
	-- JSON array of user-assigned labels, e.g. ["zelda","kid-friendly"].
	-- Validated rather than parsed: SQLite is not the document store, the
	-- service round-trips this through its own JSON type.
	user_tags   TEXT NOT NULL DEFAULT '[]' CHECK (json_valid(user_tags)),
	created_at  TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL)
);

CREATE TABLE editions (
	id                  TEXT PRIMARY KEY,
	game_id             TEXT NOT NULL REFERENCES games (id) ON DELETE CASCADE,
	region              TEXT NOT NULL DEFAULT '',
	language            TEXT NOT NULL DEFAULT '',
	revision            TEXT NOT NULL DEFAULT '',
	-- Optional hash identifying the exact source image/archive this edition was
	-- observed against. Nullable: a manual-only edition has no extractable
	-- source identity yet.
	source_identity     TEXT,
	-- Display aspect ratio (1.7778 for 16:9, 1.3333 for 4:3). Nullable while
	-- the capture target is unknown; must be positive when set.
	display_aspect      REAL CHECK (display_aspect IS NULL OR display_aspect > 0),
	locale              TEXT NOT NULL DEFAULT '',
	compatibility_notes TEXT NOT NULL DEFAULT ''
);

-- Supports "list editions of a game", which also drives the ON DELETE CASCADE
-- from games.
CREATE INDEX editions_game_id_idx ON editions (game_id);

-- One edition is identified by at most one row per region/language/revision
-- combination; re-importing the same disc must reconcile, not duplicate.
CREATE UNIQUE INDEX editions_identity_idx
	ON editions (game_id, region, language, revision);

CREATE TABLE profiles (
	id                 TEXT PRIMARY KEY,
	edition_id         TEXT NOT NULL REFERENCES editions (id) ON DELETE RESTRICT,
	revision_num       INTEGER NOT NULL CHECK (revision_num >= 1),
	parent_revision    INTEGER,
	status             TEXT NOT NULL CHECK (status IN ('draft', 'review', 'published', 'archived')),
	-- Digest of the canonical profile manifest; the publication gate and the
	-- audio cache key both derive from it. Empty until a manifest is built.
	manifest_digest    TEXT NOT NULL DEFAULT '',
	-- NULL until published; an RFC3339 instant afterwards.
	published_at       TEXT CHECK (published_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', published_at) IS NOT NULL),
	preferred_voice_id TEXT,
	publication_author TEXT NOT NULL DEFAULT '',
	FOREIGN KEY (edition_id, parent_revision)
		REFERENCES profiles (edition_id, revision_num) ON DELETE RESTRICT
);

-- UNIQUE, as required by docs/spec/02: revision numbers are dense per edition,
-- and this index is also the parent key for the self-referential
-- parent_revision foreign key above.
CREATE UNIQUE INDEX profiles_edition_revision_idx ON profiles (edition_id, revision_num);

CREATE TABLE sources (
	id             TEXT PRIMARY KEY,
	profile_id     TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	source_type    TEXT NOT NULL CHECK (source_type IN ('user_file', 'web_page', 'repository', 'manual', 'game_extract')),
	uri            TEXT NOT NULL,
	-- Instant the content was retrieved. For manual sources this is the moment
	-- the note was entered, so the field is never NULL and provenance ordering
	-- stays total.
	retrieved_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', retrieved_at) IS NOT NULL),
	-- Lowercase hex SHA-256 of the referenced bytes; '' when a manual source
	-- has no addressable payload.
	content_sha256 TEXT NOT NULL CHECK (content_sha256 = '' OR length(content_sha256) = 64),
	mime_type      TEXT NOT NULL DEFAULT '',
	rights_status  TEXT NOT NULL CHECK (rights_status IN ('user_supplied', 'licensed', 'unknown')),
	-- Confidence in the source on [0,1]: 1 for bytes the user supplied
	-- directly, lower for scraped or inferred material (docs/spec/14 trusts
	-- external content only after review).
	trust          REAL NOT NULL DEFAULT 0 CHECK (trust >= 0 AND trust <= 1),
	parser         TEXT NOT NULL DEFAULT '',
	coverage_notes TEXT NOT NULL DEFAULT ''
);

CREATE INDEX sources_profile_id_idx ON sources (profile_id);

-- Provenance lookups by payload hash are the hot path for import resume and
-- reachability GC.
CREATE INDEX sources_content_sha256_idx ON sources (content_sha256);

CREATE TABLE source_blobs (
	sha256            TEXT PRIMARY KEY CHECK (length(sha256) = 64),
	size              INTEGER NOT NULL CHECK (size >= 0),
	media_type        TEXT NOT NULL DEFAULT '',
	-- Namespace of the owning import (profile id, job id, 'fixture:<hash>', …).
	-- Advisory only: the same blob may legitimately be referenced by several.
	owner_namespace   TEXT NOT NULL,
	created_at        TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	-- Storage protection policy recorded with the blob. Free-form by design:
	-- the encryption backends are chosen by a later ADR, and inventing an enum
	-- here now would lock the vocabulary before that decision.
	encryption_policy TEXT NOT NULL DEFAULT 'none',
	-- Asset-store locator (relative content-addressed path or provider URI).
	-- Never an absolute caller-supplied path (docs/spec/02 "Asset store
	-- contract" forbids path traversal).
	storage_locator   TEXT NOT NULL
);
