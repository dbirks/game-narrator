-- 0017_library_lifecycle.sql — the profile library lifecycle of FR-02 /
-- ADR-0103 (gap G14): duplicate, clone, disable and delete.
--
-- Four additive changes, one per operation, no row rewrite and no change to
-- any existing key space:
--
--   * games.disabled — the disable half of G14. A disabled game is hidden
--     from Play and narration (a session start refuses it, docs/spec/01's
--     "refuse to start rather than improvise") while its profiles stay
--     editable, which is why the flag is a column rather than a status: the
--     profile lifecycle is untouched, and the library keeps showing the game
--     so it can be re-enabled. DEFAULT 0 keeps every existing game enabled —
--     SQLite applies the default to existing rows without rewriting them and
--     does not evaluate the CHECK against them (the same upgrade behaviour
--     0004 relies on for profiles.row_version).
--
--   * profiles.deleted_at / profiles.purge_after — the soft-delete half.
--     A delete is a mark, not a removal: the row (and its content) survives
--     a retention window during which the revision is hidden from the
--     library, from direct reads and from session start, and support can
--     still see what was deleted. purge_after is deleted_at + 72h, written
--     in the same statement (ADR-0103); the pair is always set together,
--     which the repositories assert, so a half-written mark is not a state
--     the schema admits. Both columns are nullable with the same
--     strftime-CHECK discipline every timestamp column in this schema has:
--     NULL means "not deleted", and an unparsable string would otherwise
--     sort and compare silently.
--
--     Deliberately NOT a status: 'deleted' is not a lifecycle state the
--     revision transitions through (docs/spec/02's workflow is
--     draft → review → published → archived, and archived revisions must
--     stay addressable forever — the publication trail outlives its
--     subject, 0004_publication.sql), it is a retention marker on the row.
--     Adding a fifth status would widen every transition table, enum list
--     and UI state machine for a property that only deletion has.
--
--   * clone_links — the clone half. A clone copies a revision into a new
--     game, a matching edition and a fresh draft revision; this table is
--     the provenance link ADR-0103 promises. It is a table rather than a
--     sources row because a clone is a catalogue relationship between two
--     revisions, not documentation of where one revision's bytes came from:
--     a sources row would need a URI the clone has none and would hide the
--     link from the "what was cloned from where" query in both directions.
--     The rows are immutable once written (no UPDATE path exists anywhere
--     in the repositories), and the table deliberately carries no foreign
--     keys: the audit record must survive the removal of what it describes
--     — the same case 0002_content.sql makes for corrections.target_id and
--     0004 for publish_actions.edition_id — and a cascade from a deleted
--     clone target would be exactly the history loss this table exists to
--     prevent. The repositories write real ids, checked against the rows
--     they just created, inside the clone's own transaction.
--
--   * speech_cache.original_message_id — the purge half's addressability.
--     The speech-cache key is the SHA-256 of the key's canonical JSON
--     (0011_speech_cache.sql, ADR-0039 amendment), and that JSON already
--     carries original_message_id (internal/speech KeyInput): the column
--     here is the denormalised provenance a purge needs to address "the
--     cache entries of this profile's messages" without inverting a hash.
--     Nullable on purpose: pre-0017 rows and voice-preview entries (whose
--     identity is "voice-preview:<id>", never a message id) match no
--     profile's purge and age out through eviction instead. Purely
--     additive — the key space, the key_version partitioning and the
--     cache's LRU behaviour are untouched.

ALTER TABLE games ADD COLUMN disabled INTEGER NOT NULL DEFAULT 0
    CHECK (disabled IN (0, 1));

ALTER TABLE profiles ADD COLUMN deleted_at TEXT
    CHECK (deleted_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', deleted_at) IS NOT NULL);
ALTER TABLE profiles ADD COLUMN purge_after TEXT
    CHECK (purge_after IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', purge_after) IS NOT NULL);

CREATE TABLE clone_links (
	id          TEXT PRIMARY KEY,
	-- The revision the clone was taken from.
	source_profile_id TEXT NOT NULL,
	-- The catalogue relationship, in both directions: "what did this game
	-- come from" (target) and "what was ever cloned out of this revision"
	-- (source). Edition ids travel with the game ids because a clone copies
	-- one edition of the source revision into the new game.
	source_edition_id TEXT NOT NULL,
	target_edition_id TEXT NOT NULL,
	source_game_id  TEXT NOT NULL,
	target_game_id  TEXT NOT NULL,
	target_profile_id TEXT NOT NULL,
	-- Who cloned. NOT NULL with no default, on the same accountability
	-- reasoning as publish_actions.author (0004): an unattributed clone is
	-- a provenance record that silently loses its author. Unattributed
	-- writes name 'unknown' explicitly.
	created_by    TEXT NOT NULL CHECK (length(created_by) BETWEEN 1 AND 200),
	created_at    TEXT NOT NULL
		CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL)
);

-- "What was ever cloned out of this revision" (the source direction of the
-- audit query) and "where did this game come from" (the target direction).
CREATE INDEX clone_links_source_profile_idx ON clone_links (source_profile_id);
CREATE INDEX clone_links_target_game_idx ON clone_links (target_game_id);

ALTER TABLE speech_cache ADD COLUMN original_message_id TEXT;
CREATE INDEX speech_cache_original_message_id_idx ON speech_cache (original_message_id);
