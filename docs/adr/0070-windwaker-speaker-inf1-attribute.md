# ADR-0070: Wind Waker speaker attribution from the BMG INF1 per-message attribute byte

Date: 2026-10-19
Status: accepted
Owners: agent worker (speaker-attribution)
Related requirements/tests: docs/spec/01 FR-08 (per-character voices), docs/spec/04 (adapter capability "decode markup", "never invent a speaker"), docs/spec/02 ("Speaker attribution is not necessarily a speaker-name", speaker_assertions), docs/spec/09 (curator workflows), docs/spec/12 (named-speaker metric), ADR-0017 (contract), ADR-0029 (BMG layout evidence; opaque sections), ADR-0066 (disc import), AGENTS.md rules 3/5/6. Tests: `internal/games/adapter/windwaker/speakers_test.go`, `internal/games/adapter/windwaker/real_test.go` (TestRealDiscSpeakerCoverage), `internal/profileio/speakers_test.go`, `internal/api/speakerlink_test.go`, `internal/bench/captured_test.go`, `web/e2e/speaker-link.spec.ts`.

## Context and constraints

The disc import (ADR-0066) lands Wind Waker's 4,411 messages, but every line plays in the narrator voice: the benchmark reports `named_speaker_accuracy` n/a with "BMG has no labels", because the windwaker adapter reports no speakers and `BuildCapturedIndex`/`CapturedProfile` leave every line unattributed.

The game's own message archive carries per-message attribution. The BMG's INF1 section holds, per message, a 24-byte entry: a u32 text offset followed by 20 attribute bytes. The bmg package keeps those bytes opaque (ADR-0029: never interpreted), so this decision records the structural evidence that identifies one of them as the speaker field, and the adapter (game-specific code, DoD 6) acts on it.

**Measurement (real disc, run time, counts and field indices only — no game text recorded):**

- Entry size 24 (offset word + 20 attribute bytes), uniform across all 4,411 entries; INF1 order equals pool order (0 inversions).
- Attribute byte 0 (offset+4): **50 distinct values**, present on every message (the most common value covers 284 messages; the distribution is a long tail of ~120–190 messages per value — the shape of a character roster, not of a per-message property).
- Adjacent-pair equality in pool order: **92.4%** for byte 0 versus a 2.6% random baseline computed from its own value distribution (35× the baseline). Pool runs of a constant byte-0 value: 62 runs over 4,411 messages — the pool is grouped by speaker in long blocks.
- The low-cardinality side fields (bytes 7, 11, 12, 18) are near-functions of byte 0 (constant for 38–46 of the 50 values, each collapsing to a small value set: byte 7 → 7 values, byte 18 → 6). They are per-speaker presentation properties (box/sound/camera class), not the speaker itself.
- No in-text speaker marker: all 145 control-tag families have a constant first argument (distinct = 1 per family); the sparse "voice/emotion" tag groups (1–3) occur on 1.4% of messages only, with single occurrences per (group,type).
- Independent fixture cross-check (four Dolphin-captured fixtures, their truth.json line readings matched to the imported script with the bench's own fold): each recorded scene's matched lines carry **1–2 distinct byte-0 values** (e.g. 19 lines → 1 value; 20 lines → 2 values). Random per-message values would yield ~19.
- The disc holds a second message archive (`bmgresh.arc`, a further .bmg) that the adapter does not import (pre-existing scope gap, noted here, not changed by this ADR); character *names* are not available as message data in any readable archive, so no verified name↔speaker join is possible at import time.

**Decision question:** does a reliable speaker signal exist, and where does the mapping live?

## Options

1. **Use INF1 attribute byte 0 as the speaker slot** (adopted).
2. Guess an in-text control code as a speaker tag (rejected: the measurement shows every tag family's argument is constant and the candidate groups are near-absent; a guess would violate ADR-0029's never-guess rule).
3. No attribution; defer to a curator-assignment UI only (rejected: a verified source signal exists and discarding it would leave FR-08 blocked on manual work for 4,411 lines).
4. Put the byte-0 interpretation in the bmg core package (rejected: DoD 6 — game-specific facts live in `internal/games/adapter/windwaker`; the bmg package stays lossless and layout-agnostic).

## Decision

### Adapter (`internal/games/adapter/windwaker`, new `speakers.go`)

- The speaker slot is **INF1 attribute byte 0**. Per line the adapter emits the opaque speaker key `ww:spk:<n>` (n = the byte value, decimal). Stable across re-imports; game-scoped prefix; no name is invented.
- The game data provides no display name at import time, so `SpeakerRef.DisplayName` **is** the opaque key. No name is ever recorded from the game; the roster row the importer creates is named by the key itself.
- `ParseResult.Speakers` lists the distinct keys in first-occurrence order over the rows actually appended; `SpeakerKeys[i]` is parallel for every appended message (100% coverage: the byte is present on every entry); `SpeakerColumn = "inf1:attribute_0"` carries the provenance.
- Each attributed row leaves the parse with `attribution_status = proposed`, confidence 1.0 (the dump's own linkage cannot be misaligned between a line and its field), and `attribution_evidence` `{method: "source_field", source_id, field: "inf1:attribute_0", key: "ww:spk:<n>"}`.
- A deterministic provenance marker rides the result warnings (the rules-revision pattern): speaker count, coverage and field name — counts only.
- Adapter version bumps 1.0.0 → 1.1.0; `SchemaVersion` 1 → 2 (the result now fills the v2 speaker sidecar). A re-import against a stored `windwaker-bmg@1.0.0` source is stale-by-version and re-parses.

### Importer (`internal/profileio/speakers.go`)

- `adapter.SpeakerRef` gains `SourceNamed bool` (zero value = synthetic label). The generic adapter's labels are source-supplied and set it true; the windwaker keys set it false.
- **SourceNamed (existing path, unchanged):** character `confirmed`, message `confirmed` at 1.0, `script_order` assertion at posterior 1.0.
- **Synthetic label (new path):** the placeholder character is created with `status = proposed` (a parser-emitted row, unaudited — the label is an identifier, not a name the source vouches for); the message keeps its `proposed` status and the parser's confidence, gains the placeholder's `speaker_id`, and receives one `speaker_assertions` row with **method `dump_metadata`** ("a data dump's own linkage"), posterior 1.0, no reviewer (automated claims must not carry one).
- `resolveSpeakers` skips `retired` characters when building the roster key map, so a linked speaker keeps binding to its target after the placeholder is retired on re-import.

### Curator link (new API + UI)

- `POST /api/v1/profiles/{id}/characters/{cid}/speaker-link` with `{"target_character_id": "…"}` (curator grant, draft revisions only, If-Match on the revision). Semantics: the source-field speaker identified by `{cid}` (the placeholder row the import created, found via its linked messages' `source_field` evidence) **is** the target character. The handler, in one transaction:
  1. re-points every message of `{cid}` at the target (`speaker_id`, status `confirmed`, confidence 1.0, evidence = the source_field entry plus a `reviewer` entry naming the actor);
  2. writes one `manual` `speaker_assertions` row per re-pointed message (reviewer = the actor);
  3. folds the placeholder's name (the opaque key) into the target's aliases, so a re-import binds the key straight to the target through the existing roster-key match;
  4. retires the placeholder (`status = retired`).
- The UI (Characters page) lists the speaker placeholders — proposed characters whose linked messages carry `source_field` evidence (data-driven; no game-specific pattern in core or UI) — with their line counts, and offers the link action with a target picker. Name-match suggestions are computed when a placeholder carries a real display name (the game supplies one); with the Wind Waker keys there are none, so the suggestion list is empty rather than invented.

### Bench (`internal/bench/captured.go`)

- `CapturedProfile` fills each line's speaker from the adapter's sidecar (the imported message's speaker key, `unknown` when absent), and `Segments` grades the sample's truth speaker from the same sidecar for its resolved message. The scripted decision answers from the same map, so `named_speaker_accuracy` grades whether commits preserve the script's speaker attribution on the captured holdout (class b). It measures commit consistency against the script, not identity against canon — the curator's link is what settles identity.

## Tradeoffs

- The 50 placeholder roster rows ("ww:spk:n") are noisy for a curator who knows the cast; the link action retires them on merge, and the roster shows line counts so the merge order is obvious.
- Opaque keys mean the live session can offer per-speaker voices only after at least one voice exists per placeholder/linked character; until then the narrator voice is the honest degraded mode, not a silent fallback.
- The second archive (`bmgresh.arc`) stays unimported; its messages (if they extend the pool) would get the same speaker treatment in a later unit once the adapter's single-archive assumption is revisited.
- Confidence 1.0 on `proposed` rows is a deliberate split: the slot linkage is certain, the identity is not; the status — not the confidence — gates the "verified" badge and any confirmed-only consumers.

## Validation

- Synthetic: a BMG with invented speaker bytes (no game text) asserts the key set, first-occurrence order, sidecar lockstep, provenance warning and the proposed evidence payload (`speakers_test.go`); the committed Playwright disc fixture (`testdata/disc/synth-windwaker.gcm`) is regenerated to the measured INF1 layout — 24-byte entries (u32 offset + 20 attribute bytes) with one distinct speaker byte per line — so the UI walk has four linkable placeholders; its SHA-256 pin lives in `testdata/disc/README.md`. profileio asserts placeholder creation (proposed), message linkage (proposed, dump_metadata assertion) and re-import idempotence plus the retired-placeholder rebind (`speakers_test.go`).
- Real disc: `TestRealDiscSpeakerCoverage` (NOT RUN without `GAME_NARRATOR_GAME_IMAGE_PATH`) asserts 100% sidecar coverage, a non-degenerate distinct-speaker count, and logs counts only.
- Bench wiring: `TestCapturedProfileCarriesTheSpeakerSidecar` (`internal/bench/captured_test.go`) locks the sidecar→script-line flow in memory (and the no-sidecar unknown default); the captured holdout reports `named_speaker_accuracy` with n > 0 instead of n/a (class b evidence — `TestCapturedEndToEnd` + `game-narrator bench captured` runs, which grade it at 100% on the recorded fixtures where named utterances occur).
- API: `speakerlink_test.go` covers the happy path (message re-point, one manual assertion per line, alias fold, source retirement), the self-link 400, the missing-field 400, the unknown-target 404, the retired-source 409, the draft-only 409, the revision-conflict 412 and the viewer forbidden 403. UI: `web/e2e/speaker-link.spec.ts` (project `disc`, packaged binary) exercises the placeholder list, the link action, the post-link roster state and a clean console.

## Consequences

- FR-08 unblocks: the pipeline's speaker options come from the distinct `speaker_id` values, so per-character voices become reachable per line without further import changes.
- The audit trail separates the dump's claim (`dump_metadata`) from the curator's identification (`manual`), which docs/spec/02 asks for.
- Any future Wind Waker edition whose INF1 layout differs (entry size, byte position) fails the adapter's bounds and must re-run the measurement, not silently reuse this mapping.

## Reversal plan

Bump the adapter version, set the speaker mapping back to "none" (drop the sidecar fill and the marker warning), and keep the importer's synthetic-label path (it is inert without an adapter that reports synthetic speakers). Rows already linked by a curator are plain attribution rows and need no migration; the placeholders become ordinary proposed characters a curator can retire. No schema change is introduced by this ADR, so there is no forward migration to reverse.
