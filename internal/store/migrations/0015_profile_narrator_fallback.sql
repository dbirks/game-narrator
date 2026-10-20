-- 0015_profile_narrator_fallback.sql — the per-profile narrator fallback
-- (ADR-0101, issue #11): when a committed line's speaker has no approved
-- voice, the line is spoken by the narrator voice unless this flag says
-- otherwise.
--
-- The flag lives on the PROFILE ROW, not in app_settings: it is an editorial
-- decision about how this profile's narration resolves unknown speakers, so
-- it travels with the profile (the manifest's publication section carries it)
-- and a re-import applies it to the draft it creates.
--
-- The default is 1 (on): every pre-existing profile reads the runtime's
-- historical behaviour — the narrator fallback was unconditional before this
-- column existed — so the migration changes no audible behaviour on its own.

ALTER TABLE profiles ADD COLUMN narrator_fallback INTEGER NOT NULL DEFAULT 1
    CHECK (narrator_fallback IN (0, 1));
