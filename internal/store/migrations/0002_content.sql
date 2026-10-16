-- 0002_content.sql — profile *content* tables from docs/spec/02-data-and-storage.md
-- ("Entity catalog"): messages, message_pages, message_edges, characters,
-- speaker_assertions, scenes, regions and corrections, plus the FTS5 trigram
-- index over message text.
--
-- This is the body of a profile revision: the canonical dialogue graph an
-- adapter produced plus the human labels applied to it. It deliberately keeps
-- the distinctions docs/spec/02 opens with — a *message* (canonical source
-- text) is not an *occurrence* (that text seen on screen at runtime, which
-- lives in the operational tables of a later migration), not a *page*
-- (`message_pages`: one screenful of a message), not an *utterance*
-- (`speech_text`: what should be spoken) and not an audio asset (content
-- addressed on disk). Modelling those as one table is the failure mode the
-- spec warns against.
--
-- Conventions inherited from 0001_init.sql: TEXT public IDs (ULID strings),
-- TEXT RFC 3339 timestamps guarded by a strftime CHECK (SQLite has no date
-- type, so an unparsable string would otherwise sort and compare silently),
-- CHECK-guarded enums, and JSON columns validated with json_valid rather than
-- parsed — SQLite is not the document store, the service round-trips them
-- through its own types.
--
-- Profile scoping. docs/spec/02 makes content part of one profile revision, so
-- every table that owns a row of content (messages, characters, scenes,
-- regions, speaker_assertions, corrections) carries `profile_id` and cascades
-- from `profiles`. `message_pages` and `message_edges` do **not** duplicate it:
-- their only identity is the message they hang off, `messages.profile_id`
-- already fixes which revision they belong to, and a copied column could drift
-- out of agreement with its parent (SQLite cannot CHECK across tables without
-- triggers). They cascade from `messages` instead, so deleting a profile still
-- removes their rows — `TestDeletingProfileCascadesContentAndFTS` asserts that,
-- including that the FTS index comes away empty with them.
--
-- Foreign key ON DELETE behaviour, justified per requirement:
--   * messages.profile_id / characters.profile_id / scenes.profile_id /
--     regions.profile_id / speaker_assertions.profile_id /
--     corrections.profile_id -> profiles(id) CASCADE: this content is curated
--     *within* one revision and is meaningless outside it. Unlike
--     `profiles.edition_id` in 0001 (RESTRICT, because a published revision is
--     the durable artefact), a draft message has no life after its revision.
--   * messages.source_id -> sources(id) SET NULL: `sources` documents where
--     content came from, and revoking that provenance row must not destroy
--     curated text someone has already edited; the message survives with its
--     link cleared and a later import re-attributes it. `sources` itself
--     cascades from `profiles`, so profile deletion is unaffected.
--   * messages.speaker_id -> characters(id) SET NULL: docs/spec/02 "SQLite
--     schema principles" — attribution "is not necessarily a speaker-name
--     printed on screen", so the resolved speaker may legitimately be unknown
--     while `speaker_assertions` holds the candidates. Removing the character
--     must not remove the dialogue line it spoke.
--   * messages.scene_id -> scenes(id) SET NULL: a scene is an editorial
--     grouping; deleting it is a re-organisation, not a deletion of the lines.
--   * characters.portrait_asset_id has no foreign key at all, exactly like
--     `source_blobs` in 0001: assets are content-addressed and shared, so
--     their lifetime is decided by reachability GC, and docs/spec/02 says to
--     "refer by hash, verify on read".
--   * speaker_assertions.message_id / .character_id CASCADE: a row of that
--     table *is* the claim "this message may be spoken by this character"; if
--     either side disappears the claim asserts nothing.
--   * message_edges.from_id / .to_id -> messages(id) CASCADE: an edge is a
--     transition between two lines. A dangling edge would make the graph
--     traversal in docs/spec/04 ("Graph traversal must allow branches, cycles,
--     NPC repeats …") walk into nothing, so it goes with its endpoints rather
--     than blocking their deletion with RESTRICT.
--   * message_pages.message_id CASCADE: a page is a fragment of exactly one
--     message.
--   * corrections.target_id is a TEXT reference with no foreign key, and that
--     is deliberate: docs/spec/04 makes corrections "first-class correction
--     training/test samples rather than ad hoc code patches", so the label has
--     to outlive the row it was about (a message removed by a re-import is
--     still evidence that the parser got it wrong) and a dangling target is
--     data, not corruption. `corrections.message_id` is the one structured
--     link and cascades.
--
-- FTS5 (docs/spec/02 "SQLite schema principles": "FTS5 index on normalized
-- message text … trigram"; ADR-0008 requires the driver to provide it):
-- `messages_fts` is an **external content** table (content='messages'), so the
-- text is stored once and the index is derived. That costs sync bookkeeping —
-- the three triggers below implement the pattern documented in the SQLite FTS5
-- manual, re-feeding 'delete' with the *old* column values and then inserting
-- the new ones on UPDATE, in that order, because the delete command removes
-- index entries by value. Consequence: `messages` must keep an implicit rowid,
-- so it is deliberately **not** declared WITHOUT ROWID, and `content_rowid`
-- maps to it.

CREATE TABLE characters (
	id                  TEXT PRIMARY KEY,
	profile_id          TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	-- Canonical display name for the narrator ("Link", "Zelda"). Not unique:
	-- two editions of a profile revision may legitimately reuse a name, and
	-- docs/spec/04 expects aliases and visual descriptors to disambiguate.
	name                TEXT NOT NULL,
	-- JSON array of alternative labels, including OCR-mangled spellings the
	-- speaker-label region can produce.
	aliases             TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (aliases)),
	-- JSON array of visual descriptors (clothing colour, model id, sprite
	-- hash) used when no label is printed on screen.
	visual_descriptors  TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (visual_descriptors)),
	-- Editorial prose the voice-design and dialogue-continuity agents read.
	personality_summary TEXT NOT NULL DEFAULT '',
	-- Free-form role label ('player_one', 'companion', 'npc', 'narrator').
	-- Deliberately not an enum: adapter vocabularies differ per game and
	-- docs/spec/04's portability rule forbids baking one game's taxonomy into
	-- the core schema.
	role                TEXT NOT NULL DEFAULT '',
	-- JSON array of sources.id values that establish this character exists.
	verified_source_refs TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (verified_source_refs)),
	-- Asset-store hash of the portrait, '' when none. Same 64-hex-or-empty
	-- rule as sources.content_sha256 in 0001.
	portrait_asset_id   TEXT NOT NULL DEFAULT '' CHECK (portrait_asset_id = '' OR length (portrait_asset_id) = 64),
	-- 'proposed' rows come from a parser or agent and must be confirmed before
	-- publication treats them as fact.
	status              TEXT NOT NULL CHECK (status IN ('proposed', 'confirmed', 'retired')),
	created_at          TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL)
);

-- Character list for one profile, and name lookup while disambiguating a
-- speaker label.
CREATE INDEX characters_profile_id_idx ON characters (profile_id, name);

CREATE TABLE scenes (
	id            TEXT PRIMARY KEY,
	profile_id    TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	name          TEXT NOT NULL,
	-- JSON array of alternative names, including operator-typed ones.
	aliases       TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (aliases)),
	-- Stable fingerprint (e.g. hash of landmark OCR text plus region layout)
	-- the runtime matcher uses to say "we are back in this scene".
	region_fingerprint TEXT NOT NULL DEFAULT '',
	-- JSON array of {region_id, expected_text|expected_hash} anchors.
	visual_anchors TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (visual_anchors)),
	-- Optional progress metadata ({'low':0.4,'high':0.5,'kind':'percent'}).
	-- Data-only: the meaning of a scene's place in the story is adapter
	-- business, so it is validated as JSON, never interpreted by SQLite.
	progress_json  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (progress_json)),
	created_at     TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL)
);

CREATE INDEX scenes_profile_id_idx ON scenes (profile_id, name);

CREATE TABLE regions (
	id            TEXT PRIMARY KEY,
	profile_id    TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	scene_id      TEXT REFERENCES scenes (id) ON DELETE SET NULL,
	label         TEXT NOT NULL DEFAULT '',
	-- Which kind of on-screen text box the region crops. Region calibration
	-- (docs/spec/09 "region calibration UI") needs the class to pick OCR and
	-- speech-rendering defaults, not just geometry.
	region_type   TEXT NOT NULL CHECK (region_type IN ('textbox', 'speaker_label', 'subtitle', 'sign', 'item', 'menu', 'tutorial', 'hud', 'other')),
	-- How the rectangle was obtained, for the reliability weighting in
	-- docs/spec/04 "Visual profiles".
	detect_method TEXT NOT NULL DEFAULT 'manual' CHECK (detect_method IN ('manual', 'auto_discovered', 'imported', 'inferred')),
	-- Normalised rect [0,1] in the *display* frame, as four columns rather
	-- than one JSON blob so the CHECK below can prove the box cannot fall off
	-- the screen, and so an operator edit is a one-column UPDATE. Aspect-ratio
	-- conversion to device pixels stays in the adapter (docs/spec/04).
	rect_x        REAL NOT NULL CHECK (rect_x >= 0 AND rect_x <= 1),
	rect_y        REAL NOT NULL CHECK (rect_y >= 0 AND rect_y <= 1),
	rect_w        REAL NOT NULL CHECK (rect_w >= 0 AND rect_w <= 1),
	rect_h        REAL NOT NULL CHECK (rect_h >= 0 AND rect_h <= 1),
	-- Capture constraints: the region is only valid inside this aspect band
	-- and at or above this source resolution. NULL = unconstrained.
	min_aspect    REAL CHECK (min_aspect IS NULL OR min_aspect > 0),
	max_aspect    REAL CHECK (max_aspect IS NULL OR max_aspect > 0),
	min_width_px  INTEGER CHECK (min_width_px IS NULL OR min_width_px >= 1),
	min_height_px INTEGER CHECK (min_height_px IS NULL OR min_height_px >= 1),
	-- OCR engine settings for this crop (scale, threshold, charset, whitelist)
	-- as data the adapter interprets.
	ocr_settings  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (ocr_settings)),
	-- JSON array of asset hashes holding occlusion/crop masks.
	mask_asset_ids TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (mask_asset_ids)),
	-- Measured reliability on [0,1]; NULL until a fixture run measures it.
	reliability   REAL CHECK (reliability IS NULL OR (reliability >= 0 AND reliability <= 1)),
	-- Region-set revisions: docs/spec/09 "edits yield new region-set
	-- revision", so cached detections can be invalidated precisely.
	revision      INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
	updated_at    TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', updated_at) IS NOT NULL),
	-- Table-level constraints come last: SQLite's CREATE TABLE grammar accepts
	-- column definitions only before them, so a column def after either of
	-- these two lines is a syntax error rather than a missing comma.
	--
	-- Zero-size regions OCR nothing but stay legal (they mark "known layout, not
	-- yet calibrated"), while a box must still sit inside the frame: this guard
	-- spans four columns, which is why it is a table constraint and not one per
	-- column. A 0.6-wide box at x=0.5 is rejected even though every individual
	-- value is a legal [0,1] fraction.
	CHECK (rect_x + rect_w <= 1 AND rect_y + rect_h <= 1),
	-- An inverted aspect band can match no capture mode, so it is rejected here
	-- rather than surfacing later as "no region matched" in recognition. The
	-- disjuncts make the rule conditional: the bounds only have to be ordered
	-- once both of them exist.
	CHECK (min_aspect IS NULL OR max_aspect IS NULL OR min_aspect <= max_aspect)
);

CREATE INDEX regions_profile_id_idx ON regions (profile_id, region_type);

CREATE TABLE messages (
	id            TEXT PRIMARY KEY,
	profile_id    TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	source_id     TEXT REFERENCES sources (id) ON DELETE SET NULL,
	-- Identity assigned by the source (row number, BMG index, dump offset).
	-- Kept beside the generated canonical id per docs/spec/02 "SQLite schema
	-- principles" so a re-import reconciles instead of duplicating.
	source_message_id TEXT,
	original_text TEXT NOT NULL,
	-- Parsed control-byte/tag AST with source offsets (docs/spec/02
	-- "Normalization and markup"). Validated, never interpreted here.
	markup_json   TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (markup_json)),
	-- Case/punctuation/Unicode/OCR-confusion folded text: the retrieval key.
	normalized_text TEXT NOT NULL,
	-- Editable spoken rendering; '' means "deliberately not spoken"
	-- (docs/spec/02: an on-screen icon is not uttered if redundant).
	speech_text   TEXT NOT NULL DEFAULT '',
	language      TEXT NOT NULL DEFAULT '',
	text_kind     TEXT NOT NULL CHECK (text_kind IN ('dialogue', 'sign', 'item', 'menu', 'tutorial', 'system', 'other')),
	scene_id      TEXT REFERENCES scenes (id) ON DELETE SET NULL,
	speaker_id    TEXT REFERENCES characters (id) ON DELETE SET NULL,
	-- How the current speaker_id was arrived at. The pair is left open here on
	-- purpose: 'unknown' beside a speaker_id means a human pinned one without
	-- the profile recording evidence, and a NULL speaker_id beside 'confirmed'
	-- means the line was checked and has no speaker. The one combination that
	-- contradicts itself — 'rejected' beside a speaker_id — is refused in
	-- internal/games, because rejecting a claim while keeping the claim is a
	-- service-level contradiction rather than a storage invariant.
	attribution_status TEXT NOT NULL DEFAULT 'unknown' CHECK (attribution_status IN ('unknown', 'proposed', 'confirmed', 'rejected')),
	attribution_confidence REAL NOT NULL DEFAULT 0 CHECK (attribution_confidence >= 0 AND attribution_confidence <= 1),
	-- JSON array of {method, at, evidence, posterior} provenance fragments;
	-- the full per-candidate history lives in speaker_assertions.
	attribution_evidence TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (attribution_evidence)),
	-- JSON object of editorial flags (no-repeat, spoiler, skip-in-session,
	-- pronunciation notes). Data-only by design, same reason as characters.role.
	flags         TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (flags)),
	-- Optimistic-locking counter: docs/spec/08 uses ETag/If-Match on profile
	-- revisions, and the message editor needs the same protection.
	revision      INTEGER NOT NULL DEFAULT 1 CHECK (revision >= 1),
	created_at    TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	updated_at    TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', updated_at) IS NOT NULL)
);

-- Required by docs/spec/02 "SQLite schema principles": the dialogue browser
-- filters a profile by scene and content class.
CREATE INDEX messages_profile_scene_kind_idx ON messages (profile_id, scene_id, text_kind);
-- And the exact/prefix retrieval path that ranks OCR candidates.
CREATE INDEX messages_profile_normalized_idx ON messages (profile_id, normalized_text);
-- Speaker roster views and "who says what" audits.
CREATE INDEX messages_profile_speaker_idx ON messages (profile_id, speaker_id);

-- One source row contributes at most one message per stable source id
-- (docs/spec/02: "stable source message IDs alongside generated canonical
-- IDs"). Partial because messages with no source identity — typed by hand, or
-- produced by an OCR observation with no dump — are not duplicates of each
-- other just for sharing NULL.
CREATE UNIQUE INDEX messages_source_identity_idx
	ON messages (profile_id, source_id, source_message_id)
	WHERE source_message_id IS NOT NULL;

CREATE TABLE message_pages (
	message_id   TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
	-- 1-based page position within the message. docs/spec/02: "a message can
	-- have several pages", so pages are ordered, never merged.
	ordinal      INTEGER NOT NULL CHECK (ordinal >= 1),
	-- Source-side page identifier (BMG page index, script block id).
	source_page_id TEXT,
	page_text    TEXT NOT NULL,
	-- JSON array of normalized visible prefixes used for predictive advance
	-- (docs/spec/05 "predictive prefix unique vs two likely continuations").
	visible_prefixes TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (visible_prefixes)),
	-- JSON array of controls that advance this page ('A', 'START', …) so the
	-- runtime knows what to press (docs/spec/11 controller input).
	display_controls TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (display_controls)),
	-- JSON timing metadata: ms the page stays, characters-per-second, whether
	-- the box auto-advances. Data-only; the text state machine interprets it.
	timing_json  TEXT NOT NULL DEFAULT '{}' CHECK (json_valid (timing_json)),
	UNIQUE (message_id, ordinal)
);

CREATE TABLE message_edges (
	from_id     TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
	to_id       TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
	-- 'next' is the ordinary continuation, 'branch' a player-selectable
	-- alternative, 'repeat' an NPC saying it again, 'contextual' a transition
	-- that depends on game state, 'inferred' one nobody observed and the
	-- graph builder guessed from source order (docs/spec/04 "Dialogue graph").
	edge_type   TEXT NOT NULL CHECK (edge_type IN ('next', 'branch', 'repeat', 'contextual', 'inferred')),
	-- **Data-only** per docs/spec/02: a serialized condition expression
	-- ('flags.has_herosuit && scene == "great_fairy"'). SQLite never
	-- evaluates it and nothing may execute it; the policy engine in
	-- docs/spec/05 interprets it under the allowlist rules of AGENTS.md
	-- ("Research is autonomous but tools are allowlisted").
	condition_expr TEXT NOT NULL DEFAULT '',
	-- Provenance: which source row established the transition, plus a JSON
	-- array of observations/screenshots supporting it.
	source_id   TEXT REFERENCES sources (id) ON DELETE SET NULL,
	evidence    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (evidence)),
	-- Explicit BMG 'next' metadata scores 1; order- or scene-derived edges
	-- score lower, and docs/spec/04 forbids letting them override strong
	-- contrary OCR text.
	confidence  REAL NOT NULL DEFAULT 0 CHECK (confidence >= 0 AND confidence <= 1),
	created_at  TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	-- Re-running an import must not pile up duplicate transitions
	-- (docs/spec/02 "Transaction workflows": "no duplicate message IDs").
	UNIQUE (from_id, to_id, edge_type)
);

-- Adjacency index, required by docs/spec/02: traversal expands successors of
-- one message constantly and is the hot path of candidate ranking.
CREATE INDEX message_edges_from_id_idx ON message_edges (from_id);
-- Reverse lookups for the graph viewer and for "what leads here" audits.
CREATE INDEX message_edges_to_id_idx ON message_edges (to_id);

-- Candidate speakers that are *not* the resolved messages.speaker_id
-- (docs/spec/02 "SQLite schema principles").
CREATE TABLE speaker_assertions (
	id          TEXT PRIMARY KEY,
	profile_id  TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	message_id  TEXT NOT NULL REFERENCES messages (id) ON DELETE CASCADE,
	character_id TEXT NOT NULL REFERENCES characters (id) ON DELETE CASCADE,
	-- How the claim was produced. 'on_screen_label' is a name printed beside
	-- the box; 'script_order' and 'dump_metadata' come from a supplied script;
	-- 'visual_anchor' from a portrait/spread signal; 'model' from an inference
	-- call; 'manual' from an operator.
	method      TEXT NOT NULL CHECK (method IN ('on_screen_label', 'script_order', 'dump_metadata', 'visual_anchor', 'model', 'manual', 'manual_correction')),
	-- Instant the assertion was made (not the row's insertion time), so a
	-- re-derivation can be compared with the evidence it replaced.
	asserted_at TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', asserted_at) IS NOT NULL),
	-- JSON array of {kind, ref, excerpt} evidence: source rows, screenshot
	-- asset hashes, provider request ids.
	evidence    TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (evidence)),
	-- P(speaker = character_id) on [0,1]; combined across methods by the
	-- strategy in docs/spec/17 "Confidence combination strategy".
	posterior   REAL NOT NULL DEFAULT 0 CHECK (posterior >= 0 AND posterior <= 1),
	-- Reviewer identity for 'manual'/'manual_correction', '' otherwise. Kept
	-- because docs/spec/02 requires the audit trail and docs/spec/08 requires
	-- a role to touch these at all.
	reviewer    TEXT NOT NULL DEFAULT '',
	UNIQUE (message_id, character_id, method)
);

CREATE INDEX speaker_assertions_message_idx ON speaker_assertions (message_id, posterior DESC);

CREATE TABLE corrections (
	id           TEXT PRIMARY KEY,
	profile_id   TEXT NOT NULL REFERENCES profiles (id) ON DELETE CASCADE,
	-- What kind of mistake the operator fixed.
	kind         TEXT NOT NULL CHECK (kind IN ('speaker', 'text', 'speech_text', 'scene', 'region', 'classification', 'edge', 'other')),
	-- Loose pointer to the corrected row, with the no-foreign-key rationale in
	-- this file's header: the label is training data and outlives the row.
	target_type  TEXT NOT NULL CHECK (target_type IN ('message', 'message_page', 'message_edge', 'character', 'scene', 'region', 'other')),
	target_id    TEXT NOT NULL,
	-- Populated when the target is (or was) a message, so the common query
	-- "everything known wrong about this line" is an index seek.
	message_id   TEXT REFERENCES messages (id) ON DELETE CASCADE,
	before_value TEXT NOT NULL DEFAULT '',
	after_value  TEXT NOT NULL DEFAULT '',
	reason       TEXT NOT NULL DEFAULT '',
	-- JSON array of evidence: frame/observation ids, asset hashes.
	evidence     TEXT NOT NULL DEFAULT '[]' CHECK (json_valid (evidence)),
	-- Who or what made the correction (user id, or 'agent:<run id>').
	actor        TEXT NOT NULL,
	-- A correction is a proposal until a reviewer with the right scope accepts
	-- it (docs/spec/08 ACLs; docs/spec/03 "produce preview diffs").
	status       TEXT NOT NULL DEFAULT 'proposed' CHECK (status IN ('proposed', 'accepted', 'rejected', 'superseded')),
	reviewer     TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL CHECK (strftime ('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	reviewed_at  TEXT CHECK (reviewed_at IS NULL OR strftime ('%Y-%m-%dT%H:%M:%S', reviewed_at) IS NOT NULL)
);

CREATE INDEX corrections_profile_status_idx ON corrections (profile_id, status, created_at);
CREATE INDEX corrections_message_idx ON corrections (message_id);

-- FTS5 trigram index over the two texts a session actually searches: the
-- normalized form (OCR candidate matching, docs/spec/05) and the spoken form
-- (the message editor's search box). External content: rows are *not* stored
-- twice, the index points back at messages.rowid.
--
-- 'trigram' is chosen over unicode61 because the runtime query is a noisy OCR
-- substring, not a word: docs/spec/02 asks for "trigram or app-level n-gram
-- inverted index for fuzzy retrieval". Substring queries need at least three
-- characters, which the app enforces, not the schema.
CREATE VIRTUAL TABLE messages_fts USING fts5 (
	normalized_text,
	speech_text,
	content='messages',
	content_rowid='rowid',
	tokenize='trigram'
);

-- External-content FTS5 has no automatic sync: these triggers are the pattern
-- from the SQLite FTS5 documentation. Each body is a compound statement
-- containing its own semicolons, which is exactly the case ADR-0008 flagged for
-- splitSQL; TestSplitSQLUnderstandsTriggerBodies proves the splitter keeps them
-- intact, and TestMigrationAppliesContentFileToBlankDatabase proves SQLite
-- accepts them.
CREATE TRIGGER messages_fts_after_insert AFTER INSERT ON messages BEGIN
	INSERT INTO messages_fts (rowid, normalized_text, speech_text)
		VALUES (new.rowid, new.normalized_text, new.speech_text);
END;

CREATE TRIGGER messages_fts_after_delete AFTER DELETE ON messages BEGIN
	-- The 'delete' command must be given the values that are *in* the index,
	-- i.e. the pre-delete row, or the entries can never be found again.
	INSERT INTO messages_fts (messages_fts, rowid, normalized_text, speech_text)
		VALUES ('delete', old.rowid, old.normalized_text, old.speech_text);
END;

CREATE TRIGGER messages_fts_after_update AFTER UPDATE ON messages BEGIN
	-- Remove the old text first, then add the new: both operations address
	-- index entries by token string, so inverting the order would let the
	-- delete strip tokens the update had just re-added.
	INSERT INTO messages_fts (messages_fts, rowid, normalized_text, speech_text)
		VALUES ('delete', old.rowid, old.normalized_text, old.speech_text);
	INSERT INTO messages_fts (rowid, normalized_text, speech_text)
		VALUES (new.rowid, new.normalized_text, new.speech_text);
END;
