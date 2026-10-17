# ADR-0017: Catalog REST surface, row_version ETags, two-scope idempotency, an upload-to-job import, and a hand-written contract-tested OpenAPI document

Date: 2026-10-17
Status: accepted
Owners: project
Related requirements/tests: docs/spec/08 "Conventions" (`Idempotency-Key`, RFC 9457 problem+json, ETag/If-Match for profile revisions, cursor pagination with hard bounds) + "APIs" (games, editions/profiles, import/preview/publish, export/download, messages, characters, regions, jobs, `GET /api/v1/openapi.json`) + "Error contract examples" (`PROFILE_REVISION_CONFLICT`, `CONTENT_RIGHTS_REVIEW_REQUIRED`, `UNKNOWN_GAME_EDITION`, `JOB_REQUIRES_INPUT`) + "API version and user authentication" (roles, CSRF); docs/spec/02 "Entity catalog" + "Transaction workflows" + "Import resume by content hash" + "Profile portability"; docs/spec/09 (generated TypeScript client from OpenAPI); docs/spec/12 "Contract" mode + "Property" mode ("no published edit without valid revision token"); docs/spec/14 "upload sizes", "Filesystem/import", "copyright"; docs/spec/16 Milestone 1 ("JSON-schema and OpenAPI", "comprehensive API tests") and its 10 000-message benchmark; AGENTS.md rules 7, 9, 10; builds on ADR-0008 (SQLite, `row_version`), ADR-0010 (stdlib `ServeMux`, problem+json fallback), ADR-0014 (content-addressed asset store), ADR-0015 (revisions, publication pointer, publish audit), ADR-0016 (sessions, roles, the scope table); verified with `go test -race -count=1 ./internal/api/ ./cmd/game-narrator/`

## Context and constraints

- Milestone 1 has to expose what already exists underneath it: `internal/store`'s
  repositories (with `row_version` optimistic concurrency and an immutable
  published revision), `internal/profileio`'s import/export, `internal/jobs`'
  durable queue, and `internal/auth`'s roles. None of that work may be re-decided
  at the HTTP layer; the HTTP layer's job is to make each fact visible without
  inventing a second copy of it.
- docs/spec/08 says "implementation may rationalize exact forms through ADR" and
  "generated OpenAPI contract". The generated-in-name has to become a decision:
  nothing in this repository generates a schema today, and docs/spec/09's
  TypeScript client is only safe if the document cannot drift from the handlers.
- docs/spec/08's `POST …/import` and `POST …/preview` are one request each in the
  table, but an import's duration is not bounded by a request: `profileio` streams
  a blob through an adapter and writes it in batches, and the benchmark is
  10 000 messages on a 4 GB appliance.
- The appliance is one process on a small machine, with a restart budget measured
  in "whenever the power goes out" and a data directory that is the whole of its
  durable state (ADR-0008, ADR-0014).
- The audience of an error is a review screen, not a log tail: docs/spec/03 wants
  every problem at once, docs/spec/09 wants a stable code to branch on, and
  docs/spec/14 wants a correlation id a user can quote.

## Decision

`internal/api` gains four handler families — `catalog.go` (games, editions),
`profiles.go` (revision lifecycle, publication, characters, regions),
`messages.go` (dialogue reads and the one writable field), `importexport.go`
(upload→job, export, manifest re-import) — plus `openapi.go` and the embedded
document `internal/api/openapi/openapi.json`. Route registration is added to
`Server.Handler` in `api.go`; the role grants live in `internal/auth/policy.go`
with every other grant, so the whole ACL remains readable in one file.

### 1. Resource shapes

```
GET  /api/v1/games                      cursor page of the library
POST /api/v1/games                      Idempotency-Key required
GET  /api/v1/games/{id}                 id *or* slug
PATCH /api/v1/games/{id}                partial; no token (see §2)
GET  /api/v1/games/{id}/editions        unpaginated: a title has a handful
POST /api/v1/games/{id}/editions        Idempotency-Key required; (game,region,language,revision) is unique → 409
GET  /api/v1/editions/{id}/profiles     every revision of the edition, lowest first
POST /api/v1/editions/{id}/profiles     new draft; the revision number is server-owned
POST /api/v1/editions/{id}/profiles/import-manifest   201 + sources_needing_bytes
GET  /api/v1/editions/{id}/published    404 when nothing has shipped
GET  /api/v1/profiles/{id}              + ETag
POST /api/v1/profiles/{id}/revisions    derive a draft from any revision, no token
POST /api/v1/profiles/{id}/status       If-Match required
POST /api/v1/profiles/{id}/publish      If-Match required; 422 lists every finding
GET  /api/v1/profiles/{id}/publish-actions
GET  /api/v1/profiles/{id}/messages     ?kind=&scene=&speaker=&cursor=&limit=
GET  /api/v1/profiles/{id}/messages/search   ?q=&limit=   (hits carry bm25 unchanged)
GET  /api/v1/profiles/{id}/messages/{mid}     row + display pages
PATCH /api/v1/profiles/{id}/messages/{mid}    If-Match; `speech_text` is the only field
GET  /api/v1/profiles/{id}/characters · /regions
POST /api/v1/profiles/{id}/import       202 + job (§4)
GET  /api/v1/profiles/{id}/export       .gnprofile.json, rights-filtered (§4)
GET  /api/v1/openapi.json               public (§5)
```

Three shape decisions are worth stating rather than inheriting by accident:

- **`/games/{id}` accepts a slug as well as a ULID**, and both misses answer the
  same 404. The library UI addresses titles by slug (`/games/harvest-moon`), and a
  route that distinguished "unknown id" from "unknown slug" would let an
  unauthenticated scanner enumerate which shapes the service issues.
- **A profile is addressed by revision id, not by "edition + rev number".**
  `profiles.id` is already the row identity that `messages.profile_id`,
  `publish_actions` and session pinning hang off; a second addressing scheme would
  be a second way to be wrong about which revision a session is narrating. The
  edition-scoped routes exist to *discover* those ids.
- **`POST …/status` refuses `to: published`.** It is a legal transition in the
  state machine, and the route still refuses it, because publication is the
  validated, digested, pointer-swapped, audited act (`store.Publish`) and a column
  write cannot stand in for it. The refusal names the route to use.

### 2. ETag is `row_version`, quoted and decimal — and nothing else

`ETag: "<row_version>"` and `If-Match: "<same>"`. ADR-0015 already made
`row_version` the concurrency token: it moves in the same statement as every
accepted write, never on a refused one, and cannot come back around. The HTTP
layer adds a rendering, not a mechanism.

Rejected renderings: a digest of the body (no stable byte order across encoders,
and it would make an unrelated column write look like a new token), `updated_at`
(a clock that can be moved backwards by an NTP step is not a lock), and
`manifest_digest` (it names *content*, and only exists once a revision has been
published — so a draft could not carry it).

Which writes need the token is a table, not a judgement, and it lives in the
comment at the top of `profiles.go`:

| write | If-Match | absent | mismatch |
|---|---|---|---|
| `POST /profiles/{id}/status` | required | 428 | 412 |
| `POST /profiles/{id}/publish` | required | 428 | 412 |
| `PATCH /profiles/{id}/messages/{mid}` | required | 428 | 412 |
| `POST /profiles/{id}/revisions` | not required | — | — |
| games, editions | not required | — | — |

- `If-Match: *` is **refused** on a profile write. It is a legitimate validator
  meaning "whatever is there now", which is precisely the assumption that loses an
  edit; the whole point of the token is that the caller saw the revision it is
  changing.
- Weak validators (`W/"3"`) and unquoted tokens are refused rather than coerced
  (RFC 9110 §8.8.3 permits only strong validators in `If-Match`). A list of
  validators is accepted, and any match satisfies.
- A 412 **carries the current ETag** in the response, so an editor recovers in one
  round trip instead of guessing which of its two tabs won.
- Games and editions take no token: docs/spec/08 puts ETag/If-Match on *profile
  revisions*, and `repo_catalog.go` explains the asymmetry — a lost title edit is
  a nuisance, a lost dialogue edit is corruption. Adding a version column to
  `games` would be a schema change buying a guarantee nobody asked for.
- A list read of a revision's messages also returns `profile_etag`, because the
  dialogue browser edits *from* the list and would otherwise fetch the profile once
  per keystroke. That is a convenience member, not a second source of truth: the
  same integer, from the same row.
- `PATCH …/messages/{mid}` takes the **profile's** row version, not a per-message
  token. The store's one-write-one-bump discipline makes the revision the unit of
  concurrency; a per-message ETag would imply two messages could be edited
  concurrently without disagreeing, which is what the row version exists to prevent.
- A message row's own `revision` counter moves with it and is returned, so a client
  that cached one message can tell its copy is stale.

### 3. Idempotency has two scopes, and the difference is the lifetime of the work

`docs/spec/08`'s `Idempotency-Key` covers "create or expensive requests", which is
two different problems wearing one header:

- **Short creates (`POST /games`, `POST …/editions`)** are deduplicated by an
  in-process LRU: at most `10 000` records, `24 h` TTL, keyed by
  *principal + route + key* with a SHA-256 of the canonical request body.
  Same key + same body ⇒ the original status and body are replayed verbatim,
  flagged `Idempotency-Replayed: true`, with the same `Location`.
  Same key + different body ⇒ 422 `IDEMPOTENCY_KEY_REUSED`: one key describing two
  resources is a client bug, and honouring the second is the silent duplicate the
  header exists to prevent. Missing key ⇒ 400 `IDEMPOTENCY_KEY_REQUIRED`.
  The claim and the reservation happen under one lock, so two concurrent identical
  POSTs cannot both be told "new".
- **Uploads (`POST …/import`)** are deduplicated *durably*, by the queue's own
  unique `jobs.idempotency_key` column, scoped as
  `import.file\0principal\0profile\0key`. A retry after a dropped 202 is the case
  whose response cannot be reconstructed from memory — the job may still be running
  minutes later — so the answer is the job's identity, and the client follows
  `/api/v1/jobs/{id}`. If the stored job's payload names different bytes, the retry
  is refused with the same 422 as the in-memory path.

**The consequence stated plainly:** a process restart or a second appliance
forgets replayed create responses. That is deliberate. The durable queue already
owns a persisted idempotency key, and a second durable table for short-lived POSTs
would be state nobody can explain during a restore — while the response it replays
(`201` plus an id) would then be a claim about a row that a *different* process may
have since deleted. The uniqueness rules that actually prevent a duplicate game
(`games.slug` unique index) and a duplicate edition (the identity index) are in the
schema, so a forgotten cache degrades into a constraint violation, not corruption.
Follow-up, not done here: a `create_responses` table if a multi-process deployment
ever needs replay across restarts, with an explicit retention sweep.

### 4. An upload answers 202 with a job, and the bytes are landed first

`POST /api/v1/profiles/{id}/import` reads the request body **once, streaming** —
either a `multipart/form-data` form (a `file` part plus the mapping fields, each
non-file field capped at 1 KiB) or the raw bytes with a `Content-Type` and the
mapping in the query string, because a browser and a `curl -F` script are the two
clients that matter and they do not agree on anything else.

What happens synchronously is only what must be: stage → hash → verify → commit to
the asset store (ADR-0014), then enqueue `import.file` with the blob's content
address, and answer `202 {job_id}` with `Location: /api/v1/jobs/{id}`. What happens
in the worker is `profileio.ImportFile` against those stored bytes, with progress
and the parse report on the job row (`/jobs/{id}`, plus `job.*` events).

Why this shape rather than a synchronous 200 with a parse report:

- The duration is unbounded by the request. Holding a connection open through a
  10 000-message parse costs a browser timeout and a client that cannot safely
  retry; answering early with a summary the work has not earned is a lie with a
  200 status.
- The upload's content hash travels in the payload, so a worker that resumes after
  a crash re-reads the *same bytes* the API verified — which is what docs/spec/02's
  "import resume by content hash" requires, and what makes the job retryable on a
  different worker.
- Bytes that were never persisted are bytes a client can re-send forever. Storing
  first means provenance exists whether or not a parser ever runs, and the 413 is
  answered from a byte count rather than discovered as a full disk. The staging
  entry is aborted on every failure path, including the size check, and
  `serve` discards leftover staging before it starts listening.
- 64 MiB is the documented per-upload ceiling (`maxUploadBytes`), enforced by a
  `LimitReader` around the streaming copy so neither the API nor the asset store
  ever holds the whole upload. The asset store adds its own 512 MiB per-blob cap.
- A failure the same retry would reproduce is marked permanent (`jobs.Permanent`),
  so a malformed file is refused once instead of burning three attempts: JSON
  decoder errors are permanent *because* the bytes are content-addressed and
  verified before the parser sees them, and cannot have changed under a retry.
- `rights=unknown` is refused at the door with 422 `JOB_REQUIRES_INPUT`. Storing
  the question only resurfaces it as a publish refusal months later, at which point
  the person who knew the answer has left the room (docs/spec/01 invariant 6).
  The route's default for an omitted `rights` is `user_supplied`, which is the
  truthful statement about a file the user handed over.

Export is the mirror image, with rights as the default rather than the flag:
`GET …/export` returns the `.gnprofile.json` manifest with text withheld from any
source whose rights position is unresolved (`profileio` owns that filter), and the
`.gnpack` form that carries the bytes needs `bytes=true` **and**
`acknowledge_copyright=true`. The route never supplies the acknowledgement from a
default, and the download filename is derived from the revision's ULID — never from
an uploaded name, so no attacker-controlled string reaches a response header.
`POST /editions/{id}/profiles/import-manifest` always creates a **new draft**
(ADR-0015) and reports `sources_needing_bytes`: the difference between "imported"
and "imported and re-parseable".

### 5. The OpenAPI document is hand-written, embedded, and tested against the live router

`internal/api/openapi/openapi.json` is authored JSON, `go:embed`-ed, served
byte-for-byte at `GET /api/v1/openapi.json`, public.

- **Public** amends one ADR-0016 grant (it was `viewer`). The document is how a
  client learns to authenticate, so it cannot sit behind the credential it
  describes; it carries no game content, no host paths and no secrets. The
  fail-closed default for an *unknown* path is untouched — this is the only route
  ADR-0017 widened, and `TestEveryRegisteredRouteIsCoveredByTheScopeTable` still
  requires every other registered route to appear in the table.
- **Hand-written rather than generated from handler code**, for three reasons.
  Generated schemas describe what a struct happens to marshal to, so the moment a
  handler's DTO is the specification, "the API returns the wrong thing" becomes
  unstatable: a refactor that renames a field, drops a pointer or changes an
  `omitempty` silently *becomes* the contract, and the diff that was supposed to be
  reviewed as an API change arrives as a generated-file noise commit. Half of what
  this contract must say is not in the code at all — which role an operation needs,
  which error codes it can answer, that `If-Match` names the *profile's* token, that
  a `413` means 64 MiB and not "retry later", that `source_id` is absent exactly for
  a manually entered line. And generating it would need a new module dependency,
  which AGENTS.md puts on the table only with an ADR and a measured reason; the
  alternative cost is a 1 200-line JSON file and a contract test, which this ADR
  accepts out loud.
- **Embedded, not read from disk**: a binary always serves the contract it was
  compiled with, which is the property docs/spec/09's generated client depends on.
  A document loaded at request time can be newer, older or absent relative to the
  running handlers.
- **Contract test** (`openapi_contract_test.go`), against the *live router*:
  (a) every route registered by `Handler()` appears with the same method and path
  template, and every documented operation is registered; (b) each operation's
  `x-required-role` equals what the scope table actually grants, so the document
  cannot lie about an access control; (c) every success body produced by the
  package's own handler tests validates against that operation's response schema —
  recorded automatically by the test harness rather than hand-picked, and asserted
  to cover every operation this milestone added.
- **The validator** (`openapi_validate_test.go`) implements the subset the document
  uses — `$ref` (local pointers), `type` including the 3.1 null-union form,
  `required`, `properties`, `items`, `additionalProperties`, `propertyNames`,
  `enum`, `const`, `allOf` — and nothing more. Length/range/pattern/format
  constraints are enforced at *write* time by `internal/games` and the schema's
  CHECK clauses, and re-checking them on a response would test `fmt` rather than
  the contract. The claim "and nothing more" is itself tested:
  `TestOpenAPISchemaKeywordsAreAllUnderstood` walks every schema position in the
  document and fails on a keyword that is neither enforced nor declared as an
  annotation, and `TestSchemaValidatorRejectsWhatItClaimsTo` gives each enforced
  keyword a failing case, because a validator that accepted everything would turn
  clause (c) into theatre.
- **Enum vocabularies are compared against the domain**, not re-declared from
  memory (`TestOpenAPIEnumVocabulariesMatchTheDomainEnums` over
  `games.WalkEnum`), which is the check that caught four invented members
  (`narration`, `quest`, `hint`, `error` in `text_kind`; `probable`, `contested` in
  `attribution_status`; `fixed_roi`/`edge`/`ocr_anchor`/`adaptive`; `rejected` for a
  character) before this document shipped.

### 6. Error shape

`internal/api`'s existing `problem` stays exactly as ADR-0016/0010 left it —
`type,title,status,detail,instance,request_id` — and the catalog handlers use a
sibling document adding `code` and, where the answer is a list of findings,
`problems`. The publish gate's 422 lists every finding; a validation refusal names
the field; `code` is the member a client branches on instead of prose. The
authorization gate and the router's 404/405 continue to emit the base shape with no
`code`, which is why the contract documents `code` as present-on-this-family rather
than required-on-every-problem — a lie in the contract is worse than the omission.

Sizes and shapes are validated before anything is written: JSON bodies ≤ 1 MiB
(only the import routes are larger), `DisallowUnknownFields` everywhere, exactly one
document per request, `limit` refused outside `1..200` (`1..100` for search) rather
than clamped — a clamped limit makes "give me everything" look successful while the
client silently received a page. Text is returned as JSON text and nothing is ever
rendered to HTML.

## Alternatives considered

- **Generate the OpenAPI document from handler annotations**
  (`swaggo/swag`-style comments, or `oapi-codegen` in reverse). Rejected: adds a
  module and a build step to make a file that would then need a test to prove it
  matches the handlers — which is the contract test anyway, minus the parts a
  generator cannot infer (roles, error codes, header semantics). The annotation
  syntax also moves the contract into 40 Go files.
- **Serve the document from disk.** Rejected: it allows a running process to
  disagree with its own schema, and a deploy that copies the binary without the
  file answers 500 about the API's shape.
- **Content-hash or timestamp ETags.** Rejected in §2. Note the tempting version —
  hashing the profile DTO — would have made *list* reads and *detail* reads
  disagree, because a list response is not the same bytes as the row.
- **Optimistic locking on games/editions too** (a `row_version` column). Rejected:
  no requirement asks for it, it is a schema migration with a backfill, and the
  hazard it removes (two curators editing a title simultaneously) is a nuisance.
  Revisit if the review queue ever performs multi-row edits against a title.
- **Synchronous import with a preview step** (`POST …/preview` then `POST …/import`,
  as docs/spec/08's table hints). Deferred rather than rejected: the job's result
  *is* the preview (adapter, format, counts, warnings, source id), and adding a
  second route that parses the same bytes twice would need its own lifetime story.
  A `202`-with-job plus `GET /jobs/{id}` already gives the reviewer a page they can
  reload; the missing piece is a "reject this queued import" affordance, which is
  `POST /jobs/{id}/cancel` — already implemented. Milestone 2 should decide whether
  it needs a preview that writes nothing.
- **Buffer the upload to disk, then parse in the request.** Rejected: it is the same
  unbounded duration with two failure modes (disk-full during the request, and a
  client that cannot retry safely) and no job row to point at.
- **Put the idempotency store in SQLite for all routes.** Rejected in §3 — the
  durable queue is where it pays for itself, and short creates already have unique
  indexes doing the enforcing.
- **Let `POST /games` create an edition too (a nested create).** Rejected: the
  edition's identity index is per-row, and a create that could fail after writing
  the game would need either a transaction spanning two POSTs or an orphan. Two
  calls, each idempotent, is the same UI flow with visible state.

## Consequences and risks

- **Positive.** The library, the revision lifecycle, the dialogue editor and the
  import/export portability path are usable through one authenticated surface, and
  three separate tests (`TestEveryRegisteredRouteIsCoveredByTheScopeTable`,
  `TestOpenAPIDocMatchesTheRouter`, `TestOpenAPIRoleGrantsMatchTheScopeTable`) fail
  if a later route is added without a grant or without a contract entry.
  The upload path cannot claim success it has not earned, and its progress is
  observable by the same endpoint the UI watches.
- **Costs.** The contract is now a hand-maintained file: adding a route means
  writing its entry and its response schema, and the tests make the omission loud
  rather than quiet. Every response body is validated in every flow test, which adds
  a few milliseconds per request and, more importantly, will fail a PR that changes a
  DTO — which is the intent, and will occasionally be annoying.
- **Risks accepted, named.** (1) Idempotent-create replays do not survive a restart
  (§3): a client retrying across a restart gets a `409` from a unique index instead
  of a replayed `201`. (2) The LRU keeps ≤ 10 000 recorded bodies (~10 MB worst
  case) in process memory. (3) `GET /openapi.json` is public, so an unauthenticated
  local or LAN caller can read the API's shape; it could read it in the source too,
  and docs/spec/09's client has to fetch it before it can authenticate. (4) The
  `GET …/export` grant is `viewer` because the default is rights-filtered; a
  reviewer who decides export is a curator action should change the grant in
  `policy.go` and the `x-required-role` in the document, which
  `TestOpenAPIRoleGrantsMatchTheScopeTable` forces to happen together. (5) Two of
  this family's DTO shapes are documented as they *are* rather than as one would
  want: `GET /api/v1/jobs/{id}` returns `attempts` as the attempt *history*, because
  `internal/api/jobs.go`'s detail DTO embeds the list DTO and re-declares the field,
  so the integer retry counter is shadowed and only the list gives it. ADR-0017
  records the finding against that file (owned by the queue milestone) instead of
  renaming someone else's field from here; the JobDetail schema says so explicitly.
- **Security.** Nothing new reaches the process: the routes are behind ADR-0016's
  gate (verified by `TestUnauthenticatedCatalogCallsAreRefused`,
  `TestSessionCookieAndCSRFGateACatalogWrite` and the role matrix), uploads are
  size-bounded and streamed, an uploaded filename is reduced to a base name and is
  never opened or echoed into a header, a downloaded filename is built from a ULID,
  and every SQL path in this family goes through `internal/store`'s bound
  parameters. `X-Profile-Sha256` is a digest, not a path.

## Evidence and test plan

HTTP flows (`internal/api/catalog_test.go`, over `httptest` against a migrated
SQLite database, a real asset store, the generic adapter and a live job runner):
`TestCatalogFlowEndToEnd` (create → idempotent replay → edition → draft → multipart
import → poll the job to `succeeded` → paginate → filter by kind → search → read one
message with pages → `PATCH` under `If-Match` → stale `If-Match` 412 → missing 428 →
review → publish → `PATCH` on published 409 → derive a revision → publish again and
see `publish`+`archive` in the audit → export → import-manifest into a second
edition, 409 without `force` and 201 with it → nine viewer writes 403),
`TestPaginationIsBoundedAndCursorStable`,
`TestRequestBodyLimitsAndUnknownFields`, `TestCatalogRoutesSaySoWhenNothingIsWired`,
`TestGamePatchIsPartialAndValidated`, `TestEditionIdentityIsUniqueAndRejectedTwice`,
`TestUnauthenticatedCatalogCallsAreRefused`, `TestSessionCookieAndCSRFGateACatalogWrite`,
`TestGamesListRefusesAForeignQueryParameter`.

Lifecycle and concurrency (`profiles_test.go`): `TestETagTokenIsTheRowVersion`,
`TestIfMatchAcceptanceAndRefusal` (absent 428, wildcard/weak/unquoted/non-numeric 400,
stale 412 carrying the live token, a matching validator inside a list),
`TestLifecycleMovesAndTheOnesThatAreNotMoves`,
`TestPublicationGateListsEveryProblem` (draft 422 lists findings; an unresolved
rights position answers 422 `CONTENT_RIGHTS_REVIEW_REQUIRED`, survives a text edit
that changes nothing about the decision, is recorded as a rejected audit row, and
clears only when the provenance is corrected),
`TestPublishedRevisionIsImmutableButDerivable`, `TestProfileAndEditionReads`,
`TestEveryCatalogOperationWasExercised`.

Messages (`messages_test.go`): `TestPatchSpeechTextNeverTouchesTheSourceText`
(`original_text` refused *by name*, the row re-read from storage, empty speech as a
legitimate rendering, the 8 000-character bound as a finding, 1 MiB as a 413, and a
sibling revision cannot address another's row),
`TestMessageListFiltersAndSearchBound`.

Idempotency (`catalog_idempotency.go`, `catalog_idempotency_test.go`):
`TestIdempotencyStoreReplaysConfirmsAndScopes`,
`TestIdempotencyStoreExpiresAndEvictsWithinItsBounds` (24 h TTL, the 10 000-record
LRU, the key-length ceiling),
`TestConcurrentIdenticalCreatesClaimTheKeyOnce`,
`TestIdempotencyKeysAreScopedPerPrincipalAndRouteOnTheWire` (byte-identical replay,
same `Location`, `Idempotency-Replayed: true`, a released reservation after a 422).

Import/export (`importexport_test.go`):
`TestImportRunsAsAJobAndReconcilesOnReUpload`,
`TestImportAcceptsARawBodyWithQueryParameters`, `TestImportRefusals` (415, missing
file part → `JOB_REQUIRES_INPUT`, `rights=unknown` → `JOB_REQUIRES_INPUT`, unknown
format naming the known ones, two file parts, an over-long form field, and a
malformed script failing as *one* permanent job attempt),
`TestUploadOverTheSizeLimitIsRefusedWith413` (64 MiB+1 streamed, staging left
clean, a sub-limit upload still accepted), `TestImportHonoursADurableIdempotencyKey`,
`TestImportIntoAPublishedRevisionIsRefused`,
`TestExportWithholdsTextWhoseRightsAreUnresolved`,
`TestExportPackNeedsAnAcknowledgementAndCarriesTheBlobs`,
`TestImportManifestOntoAFreshAppliance` (a second appliance, `sources_needing_bytes`
non-empty, no row id carried across machines), `TestImportManifestRefusals`, and
`TestImportProgressIsVisibleOnTheEventStream` (the import's `job.queued`,
`job.started` and `job.succeeded` frames arrive on `GET /api/v1/events` replayed
from the durable event log, naming the job the browser was handed).

Contract (`openapi.go`, `openapi_contract_test.go`, `openapi_validate_test.go`):
`TestOpenAPIDocMatchesTheRouter`, `TestOpenAPIRoleGrantsMatchTheScopeTable`,
`TestOpenAPIOperationsHaveResponsesAndIDs`,
`TestOpenAPIErrorResponsesAreProblemJSON`,
`TestTemplateResolutionPrefersALiteralSegment`,
`TestOpenAPIEnumVocabulariesMatchTheDomainEnums`,
`TestOpenAPIDescribesItsSecuritySchemes`, `TestOpenAPIProblemDocumentShape`,
`TestOpenAPIIsServedPubliclyAndByteStably`,
`TestSchemaValidatorRejectsWhatItClaimsTo`,
`TestOpenAPISchemaKeywordsAreAllUnderstood`,
`TestOpenAPIRequestSchemasMatchTheHandlers`.

Wiring (`cmd/game-narrator`): `TestServeCatalogAndImportOverTheWire` runs the
`serve` command against a temporary data directory and drives the whole path over
HTTP — anonymous read refused, keyed create and its replay, edition, draft, the
multipart upload, the job to `succeeded` through `GET /jobs/{id}`, a stale-token
412 followed by the 200 it names, the export download, and a blob present under
`<data-dir>/assets/blobs/` afterwards.

Commands:

```
go vet ./...
go test -race -count=1 -timeout 600s $(go list ./... | grep -v capture)
go test -race -count=1 -run 'TestCatalogFlowEndToEnd|TestOpenAPI|TestIdempotency' ./internal/api/
go test -race -count=1 -run TestServeCatalogAndImportOverTheWire ./cmd/game-narrator/
make build && bin/game-narrator serve --listen 127.0.0.1:18701 --data-dir "$(mktemp -d)"
```

Measured on the development host (Arch Linux, x86-64, Go 1.27.x, race detector):
`internal/api` 70.8 s and `cmd/game-narrator` 33.8 s (the repository's whole race
suite runs in ~7 minutes, unchanged from before this milestone);
`TestUploadOverTheSizeLimitIsRefusedWith413` costs 0.8 s alone and 3.4 s under the
race detector, which is the streaming 64 MiB+1 assertion itself being written and
hashed — not anything the API adds.

## Migration / rollback

No schema change, no migration, no new dependency. The surface is additive:
`internal/store`, `internal/profileio` and `internal/jobs` are unchanged, so
rollback is (1) delete the route registrations from `Server.Handler` and the
catalog grants from `internal/auth/policy.go`, which returns the API to the
Milestone 0 surface (the fail-closed default then refuses any path left behind),
and (2) drop the `api.WithStore` / `api.WithImport` options from `serve`, which
makes every catalog route answer 503 rather than half-work. Data written through
this API is ordinary catalogue rows and asset blobs — an appliance that stops
serving the routes still holds them, and `game-narrator migrate` / the CLI remain
able to read them. The one behaviour a client could depend on that is *not*
reversible in place is the widened `GET /api/v1/openapi.json` grant; restoring
`Require(RoleViewer)` there means docs/spec/09's client must be given the document
another way, and that change belongs in the scope table plus the document in the
same commit that the contract test forces anyway.

## Follow-ups

1. A durable idempotency table for keyed creates, if a second process ever shares
   a data directory (or a replica appears).
2. `POST /profiles/{id}/preview` as a first-class route, with its lifetime decided
   against the job result it would duplicate (§4).
3. Rename or re-shape `GET /jobs/{id}`'s `attempts` so the retry counter and the
   history are both present; the JobDetail schema documents the shadowing today.
4. The `docs/adr/README.md` index has no per-ADR list to update, so nothing to do
   there; if one is added, ADR-0017 belongs in it.
5. **Import rate limiting is not wired, and docs/spec/08 asks for it.** The current
   controls on the upload path are the `curator` role, the 64 MiB per-upload ceiling
   enforced while streaming, the asset store's per-blob cap, and one `import.file`
   worker of concurrency 1 — which bound the blast radius but not the *request rate*.
   ADR-0016's `auth.Limiter` is attached to the credential endpoints only; giving the
   import route (and, when they arrive, audition and agent-web-search) a per-principal
   bucket means adding a limiter to `Server` as a wired dependency, which is the
   middleware's shape rather than this family's. It should be done there, once, for
   every expensive route, rather than bolted onto import alone.
6. Region and character *writes* (`PUT …/regions`, `POST …/characters`) are the
   agent and review-queue milestones' work; this milestone exposes the reads only,
   and the `characters`/`regions` schemas are written for the shapes those routes
   will produce.

## Links/references

docs/spec/08-api-and-events.md, docs/spec/02-data-model-and-storage.md,
docs/spec/03-developer-and-admin-ui.md, docs/spec/09-frontend-web-ui.md,
docs/spec/12-testing-strategy.md, docs/spec/14-security-privacy.md,
docs/spec/16-implementation-and-acceptance.md, ADR-0008, ADR-0010, ADR-0014,
ADR-0015, ADR-0016, RFC 9110 §8.8.3 (`If-Match`), RFC 9457 (problem+json),
RFC 6266 (`Content-Disposition`), OpenAPI 3.1 (JSON Schema 2020-12 types).

## Supersedes / superseded by

Supersedes nothing. Amends ADR-0016's scope table in one row
(`GET /api/v1/openapi.json`: `viewer` → `public`); ADR-0016 is not rewritten, and
the change is recorded here and in `internal/auth/policy.go` where the grant lives.
