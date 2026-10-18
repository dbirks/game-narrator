# ADR-0056: The overview's manifest re-import surface

Date: 2026-10-18
Status: accepted
Owners: agent worker
Related requirements/tests: docs/spec/00 ("exports/reimports profile manifest"), docs/spec/02 ("Profile portability", "Verify hashes at import and check edition/language compatibility"), docs/spec/09 ("E2E UI verification requirements"), docs/spec/16 M1 ("create/edit/revise/publish/export/reimport two synthetic game profiles … comprehensive API/UI tests") and M5 ("user can curate a full synthetic game with no CLI"), ADR-0015 (revision lifecycle), ADR-0017 (contract), `web/e2e/curation-walk.spec.ts`

## Context and constraints

docs/spec/16 M5's acceptance names the curation walk "create → import → edit → characters → voice → regions → publish → export, then reimport" as a UI-only flow. Every step already had a UI surface except the last: the re-import endpoint `POST /api/v1/editions/{id}/profiles/import-manifest` (ADR-0015's "a `.gnprofile.json` in, a brand new draft out, never an overwrite") exists, is tested, and is in the OpenAPI contract, but no page offers it. The overview's per-revision import panel feeds `POST /api/v1/profiles/{id}/import` — the script-file job — whose generic adapter would read a manifest's `messages` array as loose rows: the roster would be built from `speaker_id` references instead of the roster the import-manifest path remaps, edges, characters, attribution and regions would be dropped, and the column mapping would paper over exactly the portability guarantees the endpoint exists to enforce (digest re-verification, edition/language compatibility, the missing-blob report).

The constraint is that the uploaded bytes must reach the server unchanged: the importer re-verifies `manifest_digest` against the document's bytes (a re-encoded document 422s), so the transport may not JSON-round-trip the file.

## Decision

1. **One small surface on the edition, not on a revision.** The overview's edition panel (beside "New draft") gains a manifest import: a file input (`.json`), a submit button, and the 201 report rendered as visible state — imported revision number, messages written, the manifest digest the server echoed, and the missing-blob count when sources need re-attachment. A re-import targets an edition because it mints a *new* draft (ADR-0015), so the control sits where drafts are born.
2. **The bytes cross the wire as the file.** `catalogApi.importManifest` is a raw `fetch` with the `File` as the body (content-type `application/json`, the CSRF token header the client already maintains), because the generic client's JSON serialisation would change the bytes and trip the digest check. The 201 body is typed (`ManifestImportResult` in `web/src/api/client.ts`); failures surface as the API's own problem document through `ApiError`, rendered by the page's error state. No new endpoint, no contract change, no Go change: the surface is presentation for an existing, already-authorized route (curator-and-up, `authz_test.go`'s matrix covers it).

## Options considered

- **Feeding the export through the script-file import with a column mapping.** Rejected: it is a degraded re-parse, not a re-import — roster, edges, attribution, regions and the digest proof are lost or wrong, and the "re-import" the spec names (with hash verification and the missing-blob report) would not have happened.
- **A dedicated re-import page.** Rejected: the spec's route table (docs/spec/09) defines no such route; the edition panel already hosts the draft-creation action the re-import parallels, and a whole page for one control would be a route-matrix row of its own.
- **Multipart upload with a file part.** Rejected: the endpoint reads the body as the manifest document (the script import is the multipart one); making this endpoint accept both bodies would be a contract change to solve a transport convenience.

## Consequences and risks

- The overview gains a write the page did not have; the authz boundary is the endpoint's (unchanged), and the ETag/CSRF discipline applies as on every other page write.
- The re-imported draft is content-identical to the export's messages (original and speech text preserved, `internal/profileio/importmanifest.go`), so "export → re-import" round-trips the curated content — the curation walk asserts the edited line survives the round trip.
- `sources_needing_bytes` is displayed, not hidden: an export without its source bytes re-imports as structure with a named gap (docs/spec/02's "imported" vs "imported and re-parseable"), exactly the report the endpoint returns.

## Validation

- `web/e2e/curation-walk.spec.ts` steps 9–10: the exported `.gnprofile.json` (schema_version 1.0, 40 messages, `publication.status: published`) is uploaded through the new control; the report renders the message count and the export's own `manifest_digest`; revision 2 appears as a new draft; its dialogue page shows the edited line's original and speech text.
- Typecheck/lint/unit (`pnpm typecheck && pnpm lint && pnpm test`), then the full Playwright suite against `make build`.

## Reversal

Delete the edition-panel block and `catalogApi.importManifest`; the endpoint and its contract stand alone.
