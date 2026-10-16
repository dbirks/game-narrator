# 02 — Canonical data model, SQLite, asset storage and migrations

## Core data distinctions

`GameTitle` is a title family; `GameEdition` binds region/language/platform/revision/hash; `GameProfileRevision` is an immutable published configuration. `DialogueMessage` is canonical source text; `DialogueOccurrence` is observed on screen; `TextPage` is a display page; `Utterance` is the text that should be spoken; `AudioAsset` is synthesis output. **Do not conflate these** — a message can have several pages and one utterance can cross or split pages.

## Entity catalog with mandatory fields

- `games`: id UUID, slug unique, title, description, platform, user_tags, created_at.
- `editions`: id, game_id, region, language, revision, source_identity (optional hash), display_aspect, locale, compatibility_notes.
- `profiles`: id, edition_id, revision_num, parent_revision, status `draft|review|published|archived`, manifest_digest, published_at, preferred_voice_id, publication_author.
- `sources`: id, profile_id, source_type (`user_file|web_page|repository|manual|game_extract`), URI, retrieval timestamp, content SHA256, MIME, rights_status (`user_supplied|licensed|unknown`), trust, parser, coverage_notes.
- `source_blobs`: sha256, size, media_type, owner_namespace, created_at, encryption_policy, storage_locator; reference counts are advisory, GC via reachability.
- `messages`: id UUID, profile_id, source_id, source_message_id nullable, original_text, markup_json, normalized_text, speech_text, language, text_kind (`dialogue|sign|item|menu|tutorial|system|other`), scene_id nullable, speaker_id nullable, attribution_status, attribution_confidence, attribution_evidence, flags, revision.
- `message_pages`: message_id, ordinal, source_page_id, visible_prefix variants, page_text, display_controls, timing metadata.
- `message_edges`: from_id, to_id, edge_type (`next|branch|repeat|contextual|inferred`), condition_expr **data-only**, source/evidence, confidence.
- `characters`: id, profile_id, names/aliases, visual descriptors, personality_summary, role, verified_source_refs, portrait_asset_id nullable, status.
- `voice_profiles`: id, character_id or narrator namespace, revision, state (`draft|auditioning|approved|retired`), design_instruction, canonical_reference_asset_id, ref_transcript, model_id, embedding_mode, language, audition_ids, consent_provenance, approved_by/at, fingerprint.
- `voice_registrations`: voice_profile_id, provider_instance_id, registered_name, remote_digest/version, last_verified_at, state (`pending|ready|missing|invalid`), error.
- `audio_assets`: content_sha, message/utterance key, voice revision, engine config digest, language, duration, sample_rate, ASR_result, quality_status, created_at.
- `scenes`: id, profile_id, aliases, region fingerprint, visual anchors and optional progress metadata.
- `regions`: id, profile_id, region_type, normalized_rect [0,1], detection method, aspect/resolution constraints, OCR settings, mask asset refs, reliability scores.
- `import_jobs`, `agent_runs`, `tool_calls`, `review_items`, `corrections`, `publish_actions`, `jobs`, `job_attempts`: detailed job state, parent chains, replay/resume tokens, reviewer, audit trail.
- `sessions`, `session_events`, `observations`, `message_occurrences`, `speech_attempts`, `provider_requests`, `diagnostics`: bounded operational store separated from profile data.
- `test_fixtures`, `test_runs`, `test_assertions`: scenario manifest hash, emulator version, screenshot/audio artifacts, verdicts.

## SQLite schema principles

- Use foreign keys ON; WAL, busy_timeout, integrity_check; backups through SQLite backup API, not blindly copying live WAL DB.
- UUIDs or ULIDs for public IDs; stable source message IDs alongside generated canonical IDs. Enforce uniqueness of `(source_id, source_message_id, edition)` where appropriate.
- FTS5 index on normalized message text/aliases, trigram or app-level n-gram inverted index for fuzzy retrieval; separate memory-efficient prefix index. Store precomputed token sequence and character ngrams for candidate generation.
- Composite indexes on `(profile_id, scene_id, text_kind)` and `(profile_id, normalized_text)`; adjacency index on `message_edges(from_id)`; event indexes on `(session_id, seq)`.
- Speaker attribution **is not** necessarily a speaker-name printed on screen: source-derived `speaker_id` can remain null with candidates in a separate `speaker_assertions` table, containing method, timestamp, evidence, posterior and reviewer.
- Do not embed large images/WAVs in SQLite; store content-addressed blobs, refer by hash, verify on read.

## Normalization and markup

Store `original_text` byte-faithful; parse control bytes and symbolic tags into AST, with source offsets, instead of regex removal. `normalized_text` handles Unicode normalization, punctuation, case, whitespace, OCR confusions, line wraps, and explicit player-name placeholders; preserve provenance. `speech_text` is an editable rendering that expands button icons/variable names using a mapping (e.g. GameCube A button → “A button”; on-screen icon not uttered if redundant) and honors pause, emphasis and nonspoken elements. A per-game `SpeechRenderingRules` revision must be included in audio cache keys.

## Asset store contract

- Interface: `PutStaged(io.Reader, expectedSHA256)`, `Commit(stagingID)`, `Get(hash)`, `Stat(hash)`, `Export(refs)`, `DeleteUnreachable(dryRun)`, `Verify(hash)`.
- Local backend: under `$XDG_DATA_HOME/game-narrator` (Arch) or `/var/lib/game-narrator` (service). Directory 0700 for sensitive assets; atomic temp+fsync+rename; defend against symlinks/path traversal.
- Optional S3-compatible/network provider behind same contract. Offline reading from bounded local voice/WAV cache; background reconciliation on reconnect.
- Use strong SHA-256 on raw data and canonical JSON manifests. Hash is not a substitute for licenses or approvals.
- Retention classes: ephemeral frame cache (minutes), sampled diagnostic observations (days), labeled test fixtures (indefinite with opt-in), canonical voices (until deleted), generated utterance cache (LRU or pinned), import sources (per user retention choice).

## Profile portability

Export manifest `.gnprofile.json` describing schema version, edition, source pointers/hashes, parsed text/graph, region settings, voice references and approval metadata. Optional user-directed bundle `.gnpack` may contain user-owned/copyrighted bytes — never implicitly share/publish it. Default export excludes copyrighted source blobs and requires reimport or permission-controlled local reference resolution. Verify hashes at import and check edition/language compatibility.

## Transaction workflows

- Draft edit → new revision snapshot → validation (foreign keys, matching index, voice exists, source provenance) → reviewer approval → atomic publication pointer swap. Active sessions pin a published revision until stop.
- Voice reference update → new immutable voice revision → queue registration reconciliation → invalidate **only affected** synth cache namespace → pending calls use old revision until cutover.
- Import resume by content hash and `job_id`; tool outputs and parsed partial drafts checkpoint every stage; no duplicate message IDs/audio writes.
- Crash recovery: interrupted asset staged writes discarded or resumed; `running` jobs moved to `recoverable`, reconciler audits orphaned registrations and playback in `idle` state.

## Migration and backup acceptance

Migrations run transactionally where possible, include backward-compatible readers for one prior minor schema revision, and are tested from blank DB + prior fixture DB. Backup → corrupt DB simulated → restore → verify profile revision, source hashes, voices, published profiles and audit events. Tests must cover interrupted migration, disk-full error and revoked storage permissions.
