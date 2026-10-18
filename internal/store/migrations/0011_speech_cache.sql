-- 0011_speech_cache.sql — persistent TTS speech-cache index (ADR-0039
-- amendment, docs/spec/07 "Caching", AGENTS.md "Restart tests demonstrate
-- persistent games/voices/cache").
--
-- The TTS cache is content-addressed: the WAV bytes live in the asset store,
-- addressed by SHA-256, and this table is the key→metadata index in front of
-- them. Until now the index lived in process memory (speech.MemIndex) and
-- every restart re-synthesised the whole corpus — exactly what docs/spec/07
-- voice lifecycle step 7 forbids ("a restart reuses the asset-backed WAVs
-- instead of re-synthesizing"). The index must therefore survive a restart:
-- it is a table in the same database the catalogue uses, migrated with it
-- and covered by the backup/restore round trip.
--
-- One row per cache key. The key is the 64-character lowercase hex SHA-256 of
-- the canonical KeyInput JSON (internal/speech: cache identity is the
-- normalized text, message id, rules/voice/engine/settings revisions, style,
-- seed and output format — ADR-0023). The key's JSON carries its own
-- `version` field, which guards the key space against future field changes;
-- the key_version column records that version so a future migration can
-- partition rows by key space without re-deriving it from the hash.
--
--   key            PRIMARY KEY — the CacheKey (64-char lowercase hex SHA-256)
--   sha256         content hash of the committed WAV blob in the asset store
--   size_bytes     the blob's byte count, so eviction and diagnostics can
--                  report cache weight without reading the blob
--   duration_ns / sample_rate  recorded at commit time from the validated
--                  WAV (speech.Entry's contract)
--   qa_state       0 unknown / 1 passed / 2 failed — the ASR check loop's
--                  verdict (docs/spec/07 "ASR loop"); QA-failed entries are
--                  skipped by default unless an explicit override is given
--   key_version    the version field of the key's canonical JSON (1 today)
--   created_at / accessed_at   UTC RFC-3339; accessed_at is bumped on every
--                  read so eviction can order by last use
--
-- No column, no default, no data change to any pre-existing table: the new
-- table is empty on upgrade and the previous in-memory index is simply
-- dropped (a fresh process re-synthesizes nothing it has not generated; the
-- blobs it would have indexed are still addressed by the rows the cache
-- commits from now on). Backward compatible by construction.

CREATE TABLE speech_cache (
	key           TEXT PRIMARY KEY CHECK (length(key) = 64),
	sha256        TEXT NOT NULL CHECK (length(sha256) = 64),
	size_bytes    INTEGER NOT NULL CHECK (size_bytes > 0),
	duration_ns   INTEGER NOT NULL CHECK (duration_ns >= 0),
	sample_rate   INTEGER NOT NULL CHECK (sample_rate > 0),
	qa_state      INTEGER NOT NULL CHECK (qa_state IN (0, 1, 2)),
	key_version   INTEGER NOT NULL CHECK (key_version > 0),
	created_at    TEXT NOT NULL CHECK (strftime('%Y-%m-%dT%H:%M:%S', created_at) IS NOT NULL),
	accessed_at   TEXT NOT NULL CHECK (strftime('%Y-%m-%dT%H:%M:%S', accessed_at) IS NOT NULL)
);

-- Eviction walks the index in last-use order (docs/spec/13 "Disk full":
-- evict safe transient cache), so the ordering probe is a point, not a scan.
CREATE INDEX speech_cache_accessed_at_idx ON speech_cache (accessed_at);
