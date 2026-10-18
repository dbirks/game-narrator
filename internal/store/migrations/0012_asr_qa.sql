-- 0012_asr_qa.sql — ASR quality-check evidence (FR-10, docs/spec/07 "ASR
-- loop", ADR 0061). The ASR loop now runs: it reads audition clips and
-- freshly generated lines back through the speech_asr role and records the
-- verdict WITH its evidence (WER/CER on normalized text, the recognised
-- transcript, the ASR model id, and the provider request correlation).
--
-- The two tables the verdicts live on:
--
--   voice_auditions — the approval gate. An audition's asr_state decides
--   whether the UI offers the approve action (passed or not-run) or shows
--   the flag (failed, with the WER and transcript as the review evidence).
--
--   speech_cache — the generated-line verdicts. qa_state already carried
--   the pass/fail/unknown state (migration 0011); the new columns carry the
--   evidence the spec asks to retain (docs/spec/12: "retain diagnostic
--   evidence"): a flagged line survives a restart with its WER and
--   transcript, and the developer UI can show why a line was flagged.
--
-- Every column is additive with a default: zero data movement, no existing
-- row changes meaning (a row without a check reads as "not run" — the
-- documented degraded mode when the ASR role is not configured).
-- Backward compatible by construction (AGENTS.md rule 10); the backup/
-- restore round trip covers the new columns in content_schema_test.go.

ALTER TABLE voice_auditions ADD COLUMN asr_model TEXT NOT NULL DEFAULT '';
ALTER TABLE voice_auditions ADD COLUMN asr_state INTEGER NOT NULL DEFAULT 0 CHECK (asr_state IN (0, 1, 2));
ALTER TABLE voice_auditions ADD COLUMN asr_wer REAL NOT NULL DEFAULT 0;
ALTER TABLE voice_auditions ADD COLUMN asr_cer REAL NOT NULL DEFAULT 0;
ALTER TABLE voice_auditions ADD COLUMN asr_transcript TEXT NOT NULL DEFAULT '';
ALTER TABLE voice_auditions ADD COLUMN asr_request_id TEXT NOT NULL DEFAULT '';
ALTER TABLE voice_auditions ADD COLUMN asr_checked_at TEXT;

ALTER TABLE speech_cache ADD COLUMN qa_wer REAL NOT NULL DEFAULT 0;
ALTER TABLE speech_cache ADD COLUMN qa_cer REAL NOT NULL DEFAULT 0;
ALTER TABLE speech_cache ADD COLUMN qa_transcript TEXT NOT NULL DEFAULT '';
ALTER TABLE speech_cache ADD COLUMN qa_model TEXT NOT NULL DEFAULT '';
ALTER TABLE speech_cache ADD COLUMN qa_checked_at TEXT;
