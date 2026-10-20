-- 0013_app_settings.sql — the appliance's runtime-mutable settings
-- (ADR-0074, gap audit G5): a key/value table for the values the Settings
-- page persists. The audio output selection and master volume are the first
-- rows; the key stays a plain string so later settings (the spec-08 config
-- surface) grow into the same table without a second migration.
--
-- The environment remains the override layer (ADR-0039): a row here is the
-- DEFAULT the process resolves to when the environment does not name a value.
-- That precedence is enforced by internal/audio.Resolve, which both the API
-- and the serve wiring call, so the page and the process cannot disagree.

CREATE TABLE app_settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
