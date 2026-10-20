# ADR-0101: profile export/import carries approved voice mappings; speaker-key link resolution; configurable narrator fallback

Date: 2026-11-08
Status: accepted
Owners: agent worker (voice-import)
Related requirements/tests: issue #11 (board replay skipped `no_approved_voice` after a PC→board profile import), docs/spec/02 "Profile portability", docs/spec/07 "Voice lifecycle", docs/spec/14 "Voice consent", docs/spec/17 "Profile manifest", ADR-0005 (approved reference is immutable), ADR-0015 (re-import is a new draft), ADR-0017 (catalog API), ADR-0070 (INF1 speaker keys, speaker-link), ADR-0092 (speakable-only fence and idempotent terminals). Tests: `internal/profileio/voices_roundtrip_test.go` (export→import round trip, pointer-only import, fingerprint conflict), `internal/runtime/voice_roundtrip_test.go` (the commit path on the fresh appliance: the linked line, the unattributed line, the fallback, both skip reasons), `internal/runtime/speech_fallback_test.go` (resolveVoice's reason matrix, the link resolver), `internal/store/repo_profiles_test.go` (narrator fallback), `internal/api/profiles_narrator_fallback_test.go` (toggle route), `internal/api/importexport_test.go` (report fields), `internal/store/migrate_test.go` (migration 0015 density).

## Context and constraints

On the UNO Q, a replay of a real HDMI dialogue frame committed `MATCH_EXACT` (p=1.0) but ended `skipped/no_approved_voice`: the profile had been exported from the PC and imported, yet (a) no approved voice mapping travelled with it — the manifest carried no voice rows at all, only `publication.preferred_voice_id`, which import does not apply (the store has no guarded write for that column); (b) the line's speaker attribution — an INF1 speaker key (`ww:spk:<n>`, ADR-0070) whose placeholder had been linked to a real character on the PC — did not resolve to the linked character when the session resolved voices; and (c) there was no per-profile setting for the narrator fallback: the runtime fell back to the narrator unconditionally, and the skip reason `no_approved_voice` conflated "nobody has a voice" with "the setting forbade the fallback".

Constraints that shaped the decision:

- The **shared Base registry** names voices `gn_<profile8>_<character>_v<rev>_<digest8>` — a pure function of the voice profile **id**, the character, the revision number and the reference bytes (internal/voices `RegistryName`). A re-import that re-minted voice ids would register *new* names for the same content and would stop being "a voice id referencing the shared Base registry": the PC and the board would drift apart, and `voice_registrations` rows (keyed by `voice_profile_id` + `provider_instance_id`) would no longer agree between the two appliances on one provider instance.
- **Consent** (docs/spec/14) is a gate on approval, not a nicety: a voice revision without a consent record must not become importable speech material.
- A re-import is **a new draft with re-minted ids** (ADR-0015): every manifest reference is remapped old→new. Voice rows reference the manifest's character ids, so their `character` column must be remapped — except the narrator, whose key is the constant `"narrator"`, not a roster row.
- **Voice reference bytes** (the approved reference WAV) live in the asset store under a SHA-256. A manifest is a pointer document (rule 1 of internal/profileio): the pointer travels by default; the bytes travel only inside an acknowledged `.gnpack`.
- The manifest digest is a **content** fingerprint that must survive a re-import (canonical.go). Voice ids are *not* re-minted, so they are stable content and may appear in the digest literally; the character reference is aliased like every other roster reference.

## Options

1. **Carry approved voice rows in the manifest (`voices` section) with stable ids; resolve speaker links at session start; per-profile narrator-fallback flag** (adopted).
2. Carry voice *registrations* (per-instance names) in the manifest instead of profiles. Rejected: registrations are the per-instance copy that Reconcile exists to rebuild (internal/voices doc); porting them would import one appliance's provider state as fact and could collide with the board's own instance rows. The profile row plus the reference bytes are the source of truth; the board's Reconcile pass rebuilds the registration from them.
3. Re-mint voice ids on import like every other row. Rejected: breaks the shared-registry name (see constraints) and diverges `voice_registrations` between appliances.
4. Store the narrator fallback in `app_settings`. Rejected: that table is the appliance-global runtime settings (ADR-0074 audio); the fallback is an editorial decision about *this profile's* narration and must travel with the profile.

## Decision

### Manifest (schema_version 1.0 → 1.1)

- New required top-level section `voices`: one row per approved voice revision the exported revision can actually speak with — `state = approved`, a fingerprint, and a reference (the set `ListApprovedVoiceRevisions` returns), filtered to this revision's roster plus the narrator. Fields: `id` (the **unmodified** voice profile id), `character` (a manifest character id, or `"narrator"`), `revision`, `state`, `reference_sha256`, `ref_transcript`, `model_id`, `embedding_mode`, `language`, `fingerprint`, `gain`, `design_instruction`, `consent_provenance`, `approved_by`, `approved_at`.
- `publication.narrator_fallback` (boolean) carries the per-profile flag.
- `characters[].voice_profile_id` — the field that has been "always null" since the schema's inception — is now filled with the exported voice id when the character has one.
- The digest projection gains the `voices` section. Voice ids are **not** aliased (they are stable across re-imports; aliasing would defeat the shared-registry property by pretending they are local). `approved_at`/`approved_by` are dropped from the projection as lifecycle, the same rule that drops `uri`/`retrieved_at` from sources and the whole `publication` section.
- A reader meeting `schema_version` 1.1 refuses 1.0 documents (const check) and a reader meeting 1.0 refuses 1.1 (top-level `additionalProperties: false`): version skew is a diagnosable refusal in both directions, the rule profileio has always applied. Already-exported 1.0 files are re-exported from the PC with the new build; the manifest is a derived artifact, not state.
- The `.gnpack` bundle additionally carries each voice's reference blob as `blobs/<sha256>` (Unpack's existing, hash-verifying blob path). The reference audio is synthetic audition material (docs/spec/03: never game text), so it needs no separate copyright acknowledgement beyond the pack's existing gate; its consent travels in the row.
- A default (non-pack) export carries the voice *pointers* only.

### Import

- Each voice row is written with its **original id** through `CreateVoiceRevision`, its `character` remapped to this revision's roster (the narrator key passes through), and its `revision` number preserved — the (id, revision, character, reference) tuple is what the registry name and the speech cache key derive from.
- A voice is written **only when its reference blob is present** in the asset store (the pack committed it, or this store already holds it). A pointer whose bytes are absent is reported in the report (`voice_refs_missing`) and not written: writing a row whose reference cannot be loaded would turn every line of that character into `voice_reference_missing` on the board, which is a worse failure than the narrator fallback the row was imported to enable.
- **Idempotence and re-pointing.** Re-importing the same manifest finds the same voice ids. If the row exists with the same fingerprint, the import **re-points** `character` at the new draft's character (or leaves the narrator alone) so the freshly imported revision speaks with the same voices — a re-import that stranded the new draft's characters without their voices would fail the very property (round-trip) the flow exists to prove. The re-point is a new store write, `SetVoiceCharacter`, documented here as a lifecycle-neutral move of the character column (the content — reference, transcript, model, fingerprint, consent — is frozen at approval, ADR-0005, and untouched). If the row exists with a *different* fingerprint, the import is refused (`ErrInvalid`): an id naming two contents is a tampered or buggy manifest, and the conservative answer is the same refusal a digest mismatch earns.
- A (character, revision) slot already occupied by a *different* voice id (e.g. two different PCs' narrator voices on one board) is skipped and reported (`voices_conflicts`) rather than failing the import: the profile content is the point, the voice set is additive.
- `narrator_fallback` is applied to the new draft's profile row (absent → default on, the column default).
- Import reports gain the voice half of the answer: `voices_written`, `voices_skipped` (already present, re-pointed), `voice_refs_missing`, `voices_conflicts`.

### Store and API

- `profiles.narrator_fallback` (migration `0015`, `INTEGER NOT NULL DEFAULT 1 CHECK IN (0,1)`): every existing profile reads ON, matching the runtime's historical behaviour, so the migration changes no audible behaviour on its own. `games.Profile` gains `NarratorFallback`; `NewRevision` carries it from the parent, and a draft's default is ON.
- `POST /api/v1/profiles/{id}/narrator-fallback` with `{"enabled": bool}` and If-Match (428 absent, 412 stale, the profile-write contract). The store write (`SetProfileNarratorFallback`) does **not** bump `row_version`: the flag is an editorial setting, not content — the same reasoning the import report gives for the absent guarded write of `preferred_voice_id` (moving the token would 412 a curator mid-edit for a dial nobody is racing on). The DTO answers with the new value; the route is documented in OpenAPI.
- The profile export route and the import-manifest report surface the new fields.

### Runtime

- **Speaker-link resolution (the board gap).** When the session builds the per-message speaker map from the published content, it follows the curator's link: a line whose speaker character is *identified by* another live character — the placeholder's name (the INF1 key `ww:spk:<n>`, or any folded alias) appears in the target's aliases — resolves to the target. That is exactly the state a speaker-link (ADR-0070) leaves: the placeholder retired, its name folded into the target, its lines re-pointed. The remap is applied where the map is built, so the speech pipeline and the Clef decision wiring (whose speaker options are the same distinct ids) agree on what a speaker is. A line with no speaker, or a speaker that matches no character, stays the narrator's line as before — nothing is invented.
- **Configurable narrator fallback.** `speechWiring` carries the flag (default ON, read from the profile row at session start). `resolveVoice`'s skip reasons:
  - speaker has a usable voice → speak (unchanged);
  - speaker fault on the primary voice → the specific fault, reported not papered over (unchanged, ADR-0092's rule);
  - speaker has no voice and the fallback is ON: narrator usable → speak as narrator; narrator fault → the narrator's fault; narrator has no voice → `no_approved_voice`;
  - speaker has no voice and the fallback is OFF: narrator *could* speak → `narrator_fallback_disabled`; narrator could not → `no_approved_voice`.
  - `no_approved_voice` therefore means exactly what issue #11 expected it to mean: even the narrator has no voice.

## Tradeoffs

- Re-pointing a voice's character on re-import moves the row off the superseded draft's character. The historical revision's pointer is not preserved: a voice follows its manifest content, and the superseded revision is not served.
- A default export's voice rows are inert on a board that never received the pack (reported, not written). The operator sees `voice_refs_missing` in the import report and re-sends the acknowledged bundle — the same "imported vs imported-and-usable" split the source blobs already make.
- Two appliances sharing one Base instance register the same voice under one name when their ids agree (the intended shared-registry behaviour) and under separate names when the voices were designed separately (content-addressed names, no collision by construction).
- The 1.0 → 1.1 refusal both ways means an old binary cannot import a new file; the refusal names the version, and the fix is to re-export from the PC.

## Validation

- `internal/profileio/voices_roundtrip_test.go`: a PC store with a speaker-linked roster (placeholder `ww:spk:1` → target with the folded alias, confirmed lines, manual assertions) and approved voices (narrator + the linked character, consent records, reference assets) exports a 1.1 manifest and a `.gnpack`; a fresh store unpacks the pack, imports, and the report shows the voices written; the imported revision's message resolves its speaker to the re-minted target character, the voice row keeps its original id, its character is the new target id, and its consent/provenance/fingerprint survive; the narrator voice's character is the constant; the digest is stable across the round trip; a default (pointer-only) import reports `voice_refs_missing` and writes no row; a second import of the same manifest re-points rather than duplicates; a fingerprint conflict is refused.
- `internal/runtime/voice_roundtrip_test.go`: the end-to-end half — build the PC store, export, import into a fresh store, then drive the speech wiring's commit path with a committed line: the attributed line resolves to the imported character voice (same id, same fingerprint) and the unattributed line to the imported narrator voice; with `narrator_fallback` off and the narrator voiced, an attributed-but-voiceless line ends `skipped/narrator_fallback_disabled`; with the narrator unvoiced it ends `skipped/no_approved_voice`.
- `internal/store`: migration 0015 (dense, named, applied; default reads ON; `SetProfileNarratorFallback` flips and persists; `NewRevision` carries the flag), covered by the existing migrate suite's density checks.
- `internal/api`: the toggle route (200 with the DTO's new value and the unchanged ETag, 428/412, 400 on a missing or wrong-typed body) and the import-manifest report's voice fields, contract-tested against OpenAPI.
- `make test-replay` and the existing portability/golden suites (the golden fixture's digest is regenerated for the new projection, its `schema_version` now 1.1).

## Consequences

- A board that imports the PC's acknowledged bundle narrates: committed lines resolve to the imported per-character voices or the imported narrator voice, and an all-skip session is no longer the default degraded shape after an import.
- The board's voices.Reconcile pass registers the imported voices on its Base instance from the imported rows and blobs (docs/spec/06 "voice missing on replica") — no re-audition, no re-consent (the consent record travels and is re-checked at registration via the existing `Ensure` flow).
- FR-08's per-character voices become a property of the portable profile, not of the machine that approved them.

## Reversal plan

Bump the manifest schema back to 1.0 (drop the `voices` section and `publication.narrator_fallback` from the writer and the embedded schema), stop the pack's voice blobs, and delete the import's voice half (the rows already written are inert without a reader that carries them, and a later import simply does not touch them). `SetVoiceCharacter` and the toggle route become dead ends and are removed; migration 0015 is left in place (a forward-only schema is not un-migrated; the column reads its default). The runtime's link resolution and the skip-reason split are independent of the manifest and can be kept or reverted separately.
