-- 0014_voice_profile_gain.sql — the per-voice output gain (ADR-0074,
-- docs/spec/07 "Audio coexistence and acoustics": "per-voice gain").
--
-- The gain lives on the voice PROFILE ROW (one row per voice revision), not
-- on a session: the spec calls it per-voice, and the revision is the unit
-- the TTS cache keys on — a gain change must not invalidate the cache, so
-- it is applied to the samples at playback, never by re-synthesis.
--
-- The column is added with a constant default of 1.0 (unity): every
-- pre-existing voice revision reads gain 1.0, which is exactly "the
-- samples unchanged", so the migration changes no audible behaviour on its
-- own. Bounds: 0 silences a voice (a calibration act), 10 is well below the
-- clipping danger for a 16-bit device at full master volume.

ALTER TABLE voice_profiles ADD COLUMN gain REAL NOT NULL DEFAULT 1.0
    CHECK (gain >= 0.0 AND gain <= 10.0);
