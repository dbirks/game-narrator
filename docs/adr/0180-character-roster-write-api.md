# ADR-0180: Direct HTTP write surface for a profile's character rows

Date: 2026-10-24
Status: accepted
Owners: character-api worker
Related: docs/spec/08 (catalog writes, draft/ETag contract), docs/spec/09 (OpenAPI contract), docs/spec/13 (audit and scope table); ADR-0015 (If-Match on the revision's row version), ADR-0017 (OpenAPI contract tests), ADR-0134 (speaker-link creates a name-only confirmed row), ADR-0046 (analyze_character grounding); GitHub issue #42, slug `character-api`.
Tests: `internal/api/characters_test.go` (create, update, retire, reference refusals, the 428/412/409/404/400/422 ladder, the operator-tier refusal), `internal/api/openapi_contract_test.go` + `openapi_enums_test.go` (the new operations and the status vocabulary), `web/e2e/character-write.spec.ts` (the Cast page form mints a row and the row appears in the roster).

## Context and constraints

Issue #42: a curator working the REST API directly had only two character-surface routes — `GET .../characters` and the speaker-link POST. Speaker-link creates a row with exactly `name` + `status: confirmed`; `role`, `personality_summary` (the voice brief), `aliases`, `visual_descriptors`, `verified_source_refs` and `portrait_asset_id` stayed at their import defaults forever on link-created rows. The other writers of those fields are agent-gated tools reachable only through the free-form agent loops. Consequences the issue records: the Cast page shows no brief to edit, `analyze_character` degrades to lines-only grounding, and the roster's role column reads blank. The downstream cast-audit tool reads the character's gender/age description from `visual_descriptors`, which had no HTTP writer at all.

Constraints:

1. **Reuse the draft/ETag contract.** Profile writes are guarded by the revision's row version as If-Match (ADR-0015); a non-draft revision is frozen for curation (409). The new routes must sit inside that contract, not beside it.
2. **No store changes.** `store.PutCharacters` already owns the character write path with its validation and version move; this is an API-surface gap, not a schema gap (no migration — rule 10 is not triggered).
3. **Scope table honesty.** The scope table (`internal/auth/policy.go`) is the authorization source the contract tests check; the routes declare their tier there, the way every other mutating profile route does.

## Options considered

1. **Three routes on the existing contract**: `POST .../characters` (mint), `PUT .../characters/{cid}` (update in place), `DELETE .../characters/{cid}` (retire when unreferenced). Adopted.
2. **Extend speaker-link's body with the editorial fields.** Rejected: speaker-link is the identification action for an opaque placeholder; loading it with arbitrary roster writes would blur two audited facts into one and force the link's existing 200 shape to carry a second kind of payload.
3. **A new "character edit" endpoint per field.** Rejected: eight endpoints for one row's editorial half, each with its own ETag dance; the schema validates the whole row at once, so per-field routes would validate fragments.
4. **A hard row drop for DELETE.** Rejected: the schema keeps retired rows for history (the voice and correction rows may still name them), and speaker-link already expresses "leaves the roster" as a status move. A physical drop would orphan those rows and break the re-import bind.

## Decision

1. **The three routes** (all on `internal/api/characters.go`):
   - `POST /api/v1/profiles/{id}/characters` — mints a row from the body's editorial half. The id and `created_at` are the service's; `status` defaults to `proposed` (the row is unaudited until a curator confirms it, the same default `stage_character` carries).
   - `PUT /api/v1/profiles/{id}/characters/{cid}` — updates one row in place. Every body field is a pointer: an absent field keeps the stored value, a present one replaces it. At least one field must be set (a nothing-body is a read, not a write: 400).
   - `DELETE /api/v1/profiles/{id}/characters/{cid}` — retires the row when unreferenced. It answers 409 while anything still points at the row: a line that speaks it (store's `ListMessages` over `SpeakerID`), an attribution that claims it (`ListSpeakerAssertions`), or a voice profile keyed by its name (`ListVoiceProfiles` — the voice surface's own identity). An already-retired row is also a 409. The 200 receipt names the row and its new status.
2. **The shared preconditions** (`characterWriteCommon`): draft-only (409 `draft-only` on any other revision state), If-Match on the revision's row version (428 absent, 412 stale) — the same token speaker-link and the message edit enforce.
3. **Validation and errors**: the merged row passes `games.Character.Validate()` (422 with findings on failure); unknown `{cid}` is the roster's own 404; a moved row version is 412; the store's conflict is 409.
4. **Authorization: operator-tier.** All three routes declare `operator` in the scope table (and `x-required-role` in the OpenAPI document the contract tests cross-check): the writes touch every roster editorial field at once, including the voice brief the speech pipeline grounds on. A curator is refused with 403; the GET roster stays at the viewer tier.
5. **Audit**: each successful write records one catalog audit event (`catalog.character.create` / `.update` / `.delete`) with the profile id, the character id, and — for the two writes — the name, the role and whether a voice brief was set.
6. **The cast-audit's field**: the gender/age description lives in `characters.visual_descriptors` (a string array the importer folds, `internal/store/repo_content.go`); both writes name it, and the OpenAPI description says so, so the cast-audit worker reads a field a curator can edit over HTTP.
7. **Web surface**: the Cast page (`web/src/pages/Cast.tsx`) gains a minimal "Add a character" form (name, role, voice brief, aliases, appearance descriptors) visible on a draft to an operator; it POSTs through the client's `catalogApi.createCharacter`, which rides the profile's cached ETag as If-Match like every other profile write.

## Tradeoffs and rejected-alternative consequences

- Pointer-field bodies make the API slightly heavier than a flat object, but they are the only shape in which "clear this field" and "leave it alone" are distinct over HTTP.
- DELETE retiring instead of dropping is the schema's own vocabulary; a curator who wants the name out of the roster gets `retired`, and the reference walk makes the preconditions explicit (the 409's problem body names the lines/attributions/voices that hold the row).
- Operator-tier (not curator) for the writes is deliberate: naming a role and a voice brief is curation-adjacent but the brief feeds the speech pipeline's grounding, and the scope table keeps the boundary in the table.

## Validation

- `go test -race ./internal/api/ ./internal/store/...` (store re-run alone: the two packages' concurrent race runs share host resources and one store test tripped its own 600 s deadline under that load; it passes alone in 516 s).
- OpenAPI contract tests: the three operations match the router, carry the operator grant, and the new `status` enums are accounted for (`CharacterDeleteReceipt.properties.status` pinned to the retired literal; the create/update bodies to `CharacterStatus`).
- `web/e2e/character-write.spec.ts` (Playwright, voices project): the form mints the row, the row appears in the roster, the wire read carries the submitted editorial half, and a PUT over the same surface moves the role while keeping the brief.
- Speaker-link regression: `internal/api/speakerlink_test.go` unchanged and passing.

## Consequences and reversal plan

- Curators over the API can now mint and edit roster rows; the cast-audit worker's input field (`visual_descriptors`) is writable over HTTP.
- Reversal: delete the three route registrations and `internal/api/characters.go`; the store, the schema, and the speaker-link route are untouched, so no migration reversal is needed.
