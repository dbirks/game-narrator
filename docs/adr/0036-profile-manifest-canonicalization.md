# ADR-0036: Profile manifest canonicalization — content-derived digests that survive re-import, canonical JSON on the wire, schema version 1.0
Date: 2026-10-17
Status: accepted (retroactive)
Records decisions made in commits 494a3fc and 1810d02 (`internal/profileio`).
Owners: project
Related requirements/tests: docs/spec/02 "strong SHA-256 on raw data and canonical JSON manifests", "Import resume"; docs/spec/17 "Profile manifest" (field `manifest_digest`); docs/spec/16 M1 acceptance ("create/edit/revise/publish/export/reimport two synthetic game profiles"); docs/spec/12 restart and portability dimensions; docs/spec/14 (provenance per source, rights-aware redaction); builds on ADR-0015 (profile revisions and atomic publication) and ADR-0005 (voice/canonical asset ownership); sibling decision: ADR-0035

## Context and constraints

A published game profile must be exportable as a single `.gnprofile.json` manifest and re-importable into another database, and docs/spec/16 requires the re-import to be **verified**, not just accepted. Three spec statements only fit together if a content fingerprint survives a round trip:

1. docs/spec/02 asks for "strong SHA-256 on raw data and canonical JSON manifests".
2. docs/spec/17 names the field `manifest_digest`.
3. docs/spec/16 requires an exported profile to be re-imported and verified.

But a re-import mints new row IDs by design (docs/spec/02 "Import resume"; ADR-0015's snapshot rule: importing a manifest creates a fresh draft of a fresh game/edition — it does not re-publish anything, and it must not reuse the source database's identifiers). Any digest computed over raw database IDs would disagree with itself after one hop, making the verification requirement unsatisfiable. The store's own `profiles.manifest_digest` (computed at publication over the store's rows) already has that property — it is an audit fingerprint of *this* revision row, not a portability fingerprint.

Additional constraints: a hand-authored manifest (different key order, absent optional fields, out-of-sequence page ordinals) must import and digest the same as the export of the profile it describes; the manifest is the only place two independent databases prove they hold the same content; and the document carries copyrighted game text, so redaction and provenance must stay intact through the round trip (docs/spec/14).

## Decision

`internal/profileio` defines the `.gnprofile.json` wire form (schema version `1.0`, `SchemaVersion` constant; JSON Schema in `schema/` plus Go structural validation) and a canonicalization with the following settled properties:

### Canonical JSON

- Object keys emitted in byte order, array order preserved, no whitespace, no HTML escaping (`<`, `>`, `&` survive as themselves), UTF-8 pass-through for non-ASCII, numbers kept exactly as decoded (`json.Number`, no float reformatting).
- The exported file (`Encode`) is canonical JSON in canonical section order, so the file itself is hashable and two exports of equal content diff as content differences, not key reorderings. Empty sections serialize as `[]`, never `null` (0002_content.sql's JSON columns and docs/spec/17 both say `[]`).
- Normalization before hashing: every JSON column that arrives absent, `null`, or empty is replaced by its table default (`[]` or `{}`), so `null`, `[]`, and an absent key all produce the same digest — they all mean "none".

### The digest projection (`ContentDigest` / `Stamp` / `Verify`)

`manifest_digest` is the hex SHA-256 of a **canonical projection** of the manifest:

- `manifest_digest`, `profile_id`, and the whole `publication` section are removed: they name a revision row and its lifecycle, which a re-import resets on purpose. (`publication.store_digest` separately carries the store's row-level digest for audit; naming both differently is the honest option, naming them the same would be a lie.)
- Every row ID is replaced by a **content-derived positional alias** (`src#0`, `chr#1`, `scn#…`, `reg#…`, `msg#…`, `asr#…`) instead of the database ID. Alias assignment runs in dependency order (sources → characters → scenes → regions → messages → speaker_assertions), because a row's content includes the references it makes; within a section, rows are sorted by their canonical bytes (references already aliased) and then numbered. A string that is a document ID but not yet aliased becomes `@unresolved`, so a raw database ID never reaches the digest input. `message_edges` carry no ID of their own (identity is the (from, to, type) triple) and are canonicalized but never aliased.
- Each section's rows are ordered by their own canonical bytes, so the result is independent of database row order.
- A message's `pages` array is ordered by its `index` (the 1-based ordinal *is* the order; the store returns pages sorted by it), with content bytes as the tie-break — two documents listing the same pages in different sequence denote the same content.
- A source row's `uri` and `retrieved_at` are dropped from the projection: they describe *this* store's copy of the bytes (the URI defaults to `asset://<sha256>` of the named content), not the content. The exported document keeps stating them; only the fingerprint ignores them.
- `schema_version` and `edition` (the portability form: slug, title, region, language, revision, …) are included in the projection: a re-import of a manifest for a different edition is a different document.

Consequences the code makes explicit: `Verify` treats an absent digest as a mismatch, not a skip — a manifest that claims nothing cannot be evidence that two revisions agree. `ImportManifest` verifies the carried digest before writing a single row, re-mints IDs, creates the draft (or lands on a compatible target edition, refusing a region/language mismatch with `ErrIncompatibleEdition` unless `Force` is set), and reports the digest it stored. `Canonical` exposes the projection bytes so tests compare two revisions by comparing exactly this, instead of re-implementing the canonicalizer.

### Known, recorded limitation

This is a **canonical labelling, not full graph canonization**: two rows that differ only by a forward reference (an ID inside a JSON payload pointing at a section not yet refined) share a sort key and are ordered by their raw IDs, so such a pair could be numbered differently after a re-import and the digests would disagree. Manifests this package exports have no forward references (adapters put labels and offsets in payloads, not row IDs), and the failure mode is conservative — a digest mismatch **refuses the import** rather than admitting different content.

### Validation layers

1. JSON Schema (raw structure: types, enums, shape) — `ValidateDocument` reports by JSON pointer; the schema's vocabularies are asserted to match the domain enums.
2. Go structural validation (two-part knowledge): the document's own references resolve to rows of the right kind, IDs are unique, page ordinals start at 1, and a redacted message must state a `redacted_reason` — a message with text withheld but no stated rule is refused, never guessed at (docs/spec/14). This semantic layer is deliberately Go, not schema: "text absent without a stated rule" is a meaning question the schema cannot ask.

## Alternatives considered

- **Digest over raw database IDs.** This is what the store's publication-time `profiles.manifest_digest` does. Rejected for the manifest: it cannot survive a re-import, so docs/spec/16's "re-import and verify" would have nothing to verify against. Kept as the store-level audit digest and carried (under a different name) in the manifest's `publication` section.
- **Digest over a raw byte-for-byte export (canonical JSON without aliasing).** Simpler, but every re-import mints new IDs, so the bytes — and therefore any digest of them — change on the first hop. The whole portability contract dies.
- **Full graph canonization (Weisfeiler–Lehman-style refinement until a fixed point).** Would remove the forward-reference limitation. Rejected: exported manifests are flat in their reference structure (references point only at earlier sections, payloads carry labels not IDs), so the refinement never fires on this package's own output; buying a graph-canonicalization algorithm for an unreachable input class is cost with no payoff. The limitation is stated in the projection's documentation instead, and the failure direction is safe.
- **A stable UUID per row chosen at import time.** Would let the digest include "real" IDs, but the ID must then be reproducible from content alone (a deterministic function of content), at which point it is an alias with extra steps — and a hand-authored document would have to compute the same function or fail verification.
- **Excluding `edition` from the projection (content-only hash).** Rejected: a JP script imported onto a US edition is a *different* document for the purposes of verification; the compatibility gate (`ErrIncompatibleEdition`) and the digest must agree on what "same" means.

## Consequences and risks

- A re-import is verifiable end-to-end: export → (network) → re-import → re-export, and the digest is checked at every hop (import verifies the carried digest; the tests assert the re-export's digest and a second-hop re-import's digest both equal the original). The file is stable, not drifting one step per hop.
- Hand-authored manifests are first-class inputs: the reader's contract is pinned by `testdata/handwritten.gnprofile.json`, a fixture this code did not write (keys out of order, optional provenance fields absent, references backwards, page ordinals out of sequence). Its `manifest_digest` line is the documented algorithm's output for that document, regenerated by the same arithmetic `Verify` performs when the rules change — `Verify` is never loosened to match a stale file.
- `""` and `null` remain semantically distinct where the schema says so (an empty `speech_text` is a rendering decision; a null one is a redaction) — normalization applies to JSON *columns*, not to nullable text fields.
- The `publication` section and `profile_id` are excluded from the fingerprint, so a published revision and its imported draft are content-equal; lifecycle state is compared separately, by the store.
- Risk: the forward-reference limitation means a *future* manifest writer that starts putting raw IDs inside JSON payloads could produce digests that disagree after re-import for content-identical documents. The importer's refusal (not acceptance) is the safe direction; the fix when it becomes necessary is to extend the alias refinement, with the handwritten-fixture regeneration path as the test.
- Risk: `SchemaVersion` is `1.0` and `Validate` hard-refuses other versions — readers are strict. A writer that evolves the shape must bump the version and add a migration/compatibility test (AGENTS.md rule 10), not silently widen the reader.

## Evidence and test plan

All in `internal/profileio`:

- **Digest properties (`canonical_test.go`):** `TestDigestTamperRejected` (any mutation of a digested field fails `Verify`); `TestDigestSurvivesRelabelling` (re-minting every row ID leaves the digest unchanged); `TestDigestIgnoresExportOrder` (row order within sections is irrelevant); `TestDigestIgnoresPageListingOrderAndLocalProvenance` (page sequence and `uri`/`retrieved_at` do not move the digest); `TestCanonicalProjectionContainsNoRowIDs` (no raw database ID reaches the digest input); `TestCanonicalJSONIsSortedAndWhitespaceFree`.
- **Round trip / portability (`portability_test.go`):** `TestTwoGamePortability` — docs/spec/16 M1's acceptance line run against two deliberately different synthetic scripts (CSV and JSON adapters): import → curate (roster, scenes, region, edges, assertion, speech-text edit) → publish revision 1 → revise and publish revision 2 (first archived) → export (schema-valid) → re-import into a fresh database (new IDs, draft status, missing-blob report, digest equal) → field-by-field content comparison keyed by stable identities (source message IDs, names, (from,to,type) edge triples) → second export reproduces its own digest → two-hop re-import changes nothing → import onto an existing compatible edition mints no second catalogue row. `TestRestartKeepsImportedProfile` (close both stores, reopen the same files: provenance, messages and digest are what was written, and a re-import resolves to the same source row). `TestUnpackMakesBlobsPresent` (a `.gnpack` with its bytes leaves the recipient reporting zero missing blobs and an unchanged digest). `TestImportManifestChecksEditionCompatibility` (region/language mismatch refused with `ErrIncompatibleEdition`, honored only with `Force`; matching edition needs none).
- **Hand-written input (`golden_test.go`):** `TestHandWrittenManifestImports` — the reader against a document this package did not write: schema-valid, correct counts, incomplete provenance filled with what can be verified (`asset://<sha256>`), the fixture's carried digest re-imported unchanged, a re-export digesting the same, and a redacted-without-reason mutation (kept schema-valid on purpose) refused with the error naming `redacted_reason`.
- **Wire form (`export_test.go`, `schema_test.go`):** `TestRightsRedactionInDefaultExport` (unpublishable rights withhold the text and state `redacted_reason`), `TestPackRequiresAcknowledgement` (source bytes only with explicit copyright acknowledgement), `TestWriteManifestIsCanonicalAndSelfVerifying` (the written file is canonical JSON and verifies against itself); `TestSchemaAcceptsEveryExportedManifest`, `TestSchemaRejectsInvalidDocuments` (invalid documents rejected by pointer), `TestSchemaVocabulariesMatchTheDomainEnums`, `TestValidatorRefusesUnknownKeywords`.
- Fixture regeneration is explicit and gated: `GN_UPDATE_FIXTURES=1 go test -run TestHandWrittenManifestImports` rewrites `testdata/handwritten.gnprofile.json` — expected only when the wire shape itself changes.

## Migration / rollback

The manifest format is new in this build; nothing pre-existed to migrate. Reverting removes the package and the export/import call sites; stored profiles are untouched (the manifest is rendered from the store on demand and is never a source of truth). For the wire format: readers refuse any `schema_version` other than `1.0`, so a future shape change is a version bump plus a forward migration and a backup/restore test (AGENTS.md rule 10); documents at older versions keep importing with the older reader. There is no in-place migration of already-exported files — they are content, and their digests let a recipient detect that a new reader canonizes them differently (refusal, not silent re-acceptance).

## Follow-ups

- When payloads start carrying richer structures, revisit the forward-reference limitation: extend the alias refinement (or switch to a fixed-point canonization) before any writer puts raw IDs in JSON payloads, with the handwritten-fixture regeneration as the regression test.
- `voice_profile_id` is a null placeholder in this build (voice definitions land in Milestone 4); when the voices table arrives, the field carries the reference without a schema_version change — confirm the alias order still covers it (characters precede voices by definition).
- Consider publishing the canonical JSON spec (this ADR plus `canonical.go`) as a standalone document for external tooling that must produce digests without Go.
- ADR-0035 (matcher) is the sibling decision in the same package family; its forward-reference note is the same class of stated limitation.
