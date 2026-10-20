-- 0018_profile_auto_cast.sql — the per-profile auto-cast dial (ADR-0109,
-- docs/design/plum-peach UX-03): with the dial on, a voice audition run
-- created for one of this profile's characters produces one take that is
-- auto-approved once its ASR QA read-back is not failed (ADR 0061), the
-- consent record attributed to the profile-level consent default
-- ("original_synthetic_design") instead of a per-click prompt.
--
-- The dial lives on the PROFILE ROW, not in app_settings: it is an editorial
-- decision about how this profile's cast is voiced, so it travels with the
-- profile (publication.auto_cast in the export manifest) and a re-import
-- applies it to the draft it creates — the same placement ADR-0101 chose
-- for narrator_fallback.
--
-- The default is 1 (on): auto-cast is the product default for new setups
-- (UX-03), and a pre-existing profile opting in to the default is the
-- documented behaviour — the dial only changes what the voice job does AFTER
-- its fill, never what the fill produced, so the migration changes no
-- already-approved voice on its own.

ALTER TABLE profiles ADD COLUMN auto_cast INTEGER NOT NULL DEFAULT 1
    CHECK (auto_cast IN (0, 1));
