-- 0009_voices.sql — the voice workflow tables (docs/spec/02 entity catalog:
-- voice_profiles, voice_registrations; docs/spec/07 "Voice lifecycle", ADR-0005).
--
-- voice_profiles holds ONE ROW PER VOICE REVISION (docs/spec/02 lists
-- "revision" as a column of the entity): the same character may have several
-- rows, each an immutable snapshot of a design attempt. The state machine is
-- draft -> auditioning -> approved -> retired; a new approval is a new row,
-- and the old approved row is left untouched because old synth cache entries
-- must stay attributed to the revision that produced them (docs/spec/07
-- step 8).
--
-- The canonical reference (clip + exact transcript + model + consent) is the
-- app-owned source of truth (ADR-0005): the clip bytes live in the content-
-- addressed asset store (assets.local root, reached through canonical_
-- reference_asset_id), and everything else below. The provider registry
-- (voice_registrations) is the per-instance copy that reconciliation keeps in
-- sync; losing a registry is recoverable from these rows.

CREATE TABLE voice_profiles (
    id TEXT PRIMARY KEY,
    character TEXT NOT NULL,
    revision INTEGER NOT NULL CHECK (revision >= 1),
    state TEXT NOT NULL DEFAULT 'draft' CHECK (state IN ('draft','auditioning','approved','retired')),
    design_instruction TEXT NOT NULL DEFAULT '',
    canonical_reference_asset_id TEXT,
    ref_transcript TEXT NOT NULL DEFAULT '',
    model_id TEXT NOT NULL DEFAULT '',
    embedding_mode TEXT NOT NULL DEFAULT 'registered' CHECK (embedding_mode IN ('registered','inline')),
    language TEXT NOT NULL DEFAULT '',
    audition_ids TEXT NOT NULL DEFAULT '[]',
    consent_provenance TEXT NOT NULL DEFAULT '',
    approved_by TEXT NOT NULL DEFAULT '',
    approved_at TEXT,
    fingerprint TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    UNIQUE (character, revision)
);

CREATE INDEX idx_voice_profiles_character ON voice_profiles (character, revision DESC);
CREATE INDEX idx_voice_profiles_state ON voice_profiles (state);

-- voice_auditions: one design candidate. The WAV bytes are committed to the
-- asset store before this row exists (the audition is unusable without the
-- bytes, and the row without a blob is GC-able only as unreachable).
-- sample_text/transcript come from the fixed synthetic lines (docs/spec/03:
-- auditions speak synthetic test text, never game text), so the clip's spoken
-- content is replayable without reading the audio.
CREATE TABLE voice_auditions (
    id TEXT PRIMARY KEY,
    voice_profile_id TEXT NOT NULL REFERENCES voice_profiles(id) ON DELETE CASCADE,
    "index" INTEGER NOT NULL CHECK ("index" >= 1),
    brief TEXT NOT NULL DEFAULT '',
    model_id TEXT NOT NULL DEFAULT '',
    sample_text TEXT NOT NULL,
    transcript TEXT NOT NULL,
    asset_sha256 TEXT NOT NULL,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    sample_rate INTEGER NOT NULL DEFAULT 0,
    origin TEXT NOT NULL DEFAULT '',
    request_id TEXT NOT NULL DEFAULT '',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    seed INTEGER,
    created_at TEXT NOT NULL,
    UNIQUE (voice_profile_id, "index")
);

-- voice_registrations: the per-instance registry state (docs/spec/02
-- voice_registrations). Inserted as 'pending' with the precomputed immutable
-- name at approval; Register/Reconcile move it to ready/missing/invalid.
CREATE TABLE voice_registrations (
    voice_profile_id TEXT NOT NULL REFERENCES voice_profiles(id) ON DELETE CASCADE,
    provider_instance_id TEXT NOT NULL,
    registered_name TEXT NOT NULL,
    remote_digest TEXT NOT NULL DEFAULT '',
    last_verified_at TEXT,
    state TEXT NOT NULL DEFAULT 'pending' CHECK (state IN ('pending','ready','missing','invalid')),
    error TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (voice_profile_id, provider_instance_id)
);
