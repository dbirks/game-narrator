# ADR-0066: Disc-import API and UI surface (allowlisted image directory, upload, and the synthetic disc fixture factory)

Date: 2026-10-27
Status: accepted
Owners: agent worker (disc-import-ui)
Related requirements/tests: docs/spec/00 FR-04 ("User-owned dump import with format adapters, source hashes, raw bytes stored separately from parsed records"), docs/spec/04 ("Wind Waker reference adapter", adapter capability set), docs/spec/02 ("Import resume by content hash"), docs/spec/14 ("Filesystem/import", "Authorization", "memory/time limits"), docs/spec/09 (E2E UI), docs/spec/16 M5 ("user can curate a full synthetic game with no CLI"), ADR-0016 (scope table), ADR-0017 (contract), ADR-0054 (fixture-factory semantics), AGENTS.md rules 3/5/6. Tests: `internal/api/discimport_test.go`, `internal/gcdisc/synth_test.go`, `internal/games/adapter/windwaker/synth_test.go`, `testdata/disc/` (fixture + factory test), `web/e2e/disc-import-walk.spec.ts`.

## Context and constraints

The disc import exists as a CLI command (`game-narrator import-disc`) and in the live E2E, but the live E2E imports the *extracted BMG* through `POST /api/v1/profiles/{id}/import` — it does not import a disc image. The web UI has no disc-import surface at all: onboarding research produces characters without dialogue, and publish then refuses with "no messages". A user must be able to add game → research → **import the script from their own disc image** → review → publish, entirely in the browser.

Constraints:

1. **The image path is secret (AGENTS.md rule 5).** The disc image is user-supplied, is read in place, is never copied into the repo or the data directory, and its path is never logged or stored. Only the image's SHA-256 (provenance) and the extracted BMG's SHA-256 (the source row's `content_sha256`) are recorded.
2. **No arbitrary paths (docs/spec/14 "Filesystem/import").** The API may only read images the operator deliberately placed in an allowlisted import directory, configured as `GAME_NARRATOR_IMPORT_DIR`. A request names an image by its *basename*; anything path-shaped is refused with 400, and the join is verified to stay inside the directory.
3. **Imports are provenance-aware (AGENTS.md rule 6).** The source row is `game_extract` with a `gcdisc://<game-id>/res/Msg/bmgres.arc` URI, the parser is `windwaker-bmg@<semver>`, and the coverage note carries the image hash, game id, region and the importing user.
4. **Durable, not request-bound.** A real disc takes minutes to hash and parse, so the request does the two bounded things (land/verify the bytes, validate the target) and answers 202 with a job of kind `import.disc`, exactly as `import.file` does (ADR-0017).
5. **The repo's tests must be able to exercise a disc with invented text (AGENTS.md rule 3).** There is no disc-capable fixture in the tree; the synthetic builders live in test files only.

## Decision

### API (new file `internal/api/discimport.go`)

- `GET /api/v1/import/disc/images` (curator, the existing `/api/v1/import/{rest...}` grant): lists the regular files in the allowlisted directory as `{name, size, modified}` — basenames only, never paths. An unconfigured directory is a **state, not a fault**: the route answers 200 with `configured: false` and an empty list. That is deliberate — the overview panel queries the route on every draft revision, on every appliance, and a 503 there logs a browser-console error for a normal configuration (the catalog/voices/curation/a11y walks assert console cleanliness and refused it). The dir *action* of the import (POST) still answers 503 when unconfigured, where an error is what the requesting act asked for. 503 `SUBSYSTEM_UNAVAILABLE` remains for a build whose dependencies are unwired and for a configured directory that cannot be read.
- `POST /api/v1/profiles/{id}/import/disc` (curator, the existing `/api/v1/profiles/{rest...}` grant): two request shapes, one 202 body.
  - **dir shape**: JSON `{"image": "<basename>"}` (or `?image=<name>`). The name must be a plain basename (no `/`, `\`, `..`, NUL); the resolved path must be a regular file inside the directory. The image is *not* copied: the job payload records the basename and the job reads it in place, so a crash resumes against the same on-disk image.
  - **upload shape**: multipart with a `file` part (the raw image, CISO or plain GCM). It streams through `PutStaged`/`Commit` under the existing `maxUploadBytes` (64 MiB) cap with the hash computed while streaming — the same guarantee `import.file` gives. Real GB-scale discs do not fit the cap by design: the directory shape is how a full image is imported; the upload shape exists for small/synthetic images and for parity with the upload surface.
  - The target revision must be a draft (409 otherwise, as on the file import), and the build must link a disc-capable adapter (503, honest "nothing can read a disc").
  - The job payload is `{profile_id, source: "dir"|"upload", image, sha256, actor, request_id}` — no paths. `Idempotency-Key` is honoured durably, scoped to kind + principal + revision (the helper is generalised from the file import).
- **The job handler** (`api.ImportDiscJobHandler`, kind `import.disc`, registered next to `import.file`): opens the image in place (`gcdisc.Open`), lets the disc-capable adapter verify game ownership and extract the message script (capability assertion, see below), computes the image's SHA-256 (the dir shape only; the upload shape already carries it), and then runs `profileio.ImportFile` over the extracted BMG with `format=bmg`. Reusing the file-import machinery is the point: the BMG is staged into the asset store and hashed, a second import of the same BMG reconciles by content hash instead of duplicating (docs/spec/02), speaker labels bind to the roster, and the write is batched in one transaction.
- **Provenance overrides.** `profileio.ImportRequest` gains three optional, additive fields: `SourceURI` (here: `gcdisc://<game-id>/res/Msg/bmgres.arc`), `SourceKind` (here: `game_extract`) and `CoverageSuffix` (here: image sha256, game id, region, adapter, importing user). Zero values keep the file import's current behaviour byte-for-byte.

### Capability, not a special case

`internal/games/adapter` gains the optional capability

```go
type DiscExtractor interface {
    ExtractDiscSource(ctx context.Context, d *gcdisc.Disc) (script []byte, name, gameID string, err error)
}
```

found by assertion over the registry, exactly the pattern the package already uses for `EditionIdentifier`/`MarkupDecoder`. `windwaker.Adapter` implements it: it refuses discs whose game id does not start with `GZL` (the same refusal the CLI makes, in the same place the adapter owns) and returns the first `.bmg` of `res/Msg/bmgres.arc` under the name `bmgres.bmg`. `internal/games/adapter` now imports `internal/gcdisc`; `gcdisc` imports nothing internal, so no cycle. The region mapping moves to `gcdisc.RegionOfGameID` (a fact about the GameCube id format, which the package already parses) and `windwaker.DiscRegion` delegates to it.

### UI (game overview, per draft revision)

Beside the existing script-file import, `GameOverview.tsx` gains a disc-import panel: a select fed by `GET /api/v1/import/disc/images` (with an upload fallback when no directory is configured), a submit that posts the dir shape or the multipart shape, the shared `JobProgress` polling, and the import report (rows, inserted, skipped, speakers, warnings) plus the disc provenance line (image sha256, game id, adapter). The play setup panel additionally lists the selected published revision's messages, so "publish, then the play page lists the messages" is visible state, not an HTTP 200.

### Fixture factory (AGENTS.md rules 3/5)

`internal/gcdisc` gains the synthetic builders the tests used privately: `SynthGCMImage` (boot header + FST + files) and `AssembleRARC` (arbitrary nested tree, uncompressed). `windwaker.SynthDiscImage(gameID, lines)` composes one: a GCM image whose `res/Msg/bmgres.arc` holds a BMG of invented one-letter/short-sentence text — no game material (the strings are the fixture's own, and the factory test asserts exactly that set). The committed Playwright fixture `testdata/disc/synth-windwaker.gcm` is the factory's output for `GZLE01`; a Go test re-derives it and byte-compares (ADR-0054's golden-file semantics for a disc). The Playwright run's server points `GAME_NARRATOR_IMPORT_DIR` at the repo's `testdata/disc` directory itself and reads the fixture in place — no copy, which the in-place rule exists for: the fixture is committed synthetic test data (not a user's disc), so AGENTS.md rule 5's no-copy/no-path-logging guarantee is about the user's image, and the API never answers with any path of the directory it was given. The fixture's SHA-256 is pinned in `testdata/disc/README.md` and in the test; the fixture is labelled a synthetic stub everywhere it is asserted.

### Playwright: a project of the main config, not a second config

The spec runs as the `disc` project of `web/playwright.config.ts`, exactly like the other packaged-binary projects: one `webServer` entry (`serve.mjs disc 127.0.0.1:18112 <binary> serve` with `GAME_NARRATOR_IMPORT_DIR` = `testdata/disc`) and one project pointing at it. A standalone `web/e2e/disc.playwright.config.ts` was rejected while integrating: it cannot own the server (so it needs an operator-started one and a pre-existing manifest), and Playwright wipes `outputDir` at run start — the standalone layout had to point `outputDir` at a sibling directory to stop wiping the manifest `serve.mjs` writes into `web/test-results/e2e-runs/`. The main config needs no such workaround: its `webServer` entries start `serve.mjs` *after* the wipe and re-create the manifest, and the main `globalTeardown` owns the run root those servers live in. One config, one manifest lifecycle, no special case.

## Options considered

- **A new onboarding-agent tool for disc import.** Rejected: the agent's job is research; importing the user's own image is a deliberate, role-gated content act (curator) with a provenance row, and it already has the `import.file` job pattern to follow.
- **Streaming the whole image hash synchronously in the dir request.** Rejected: a 1.4 GB image would hold the request for minutes; the job computes it and reports it in its result, which is where provenance is audited.
- **Copying the allowlisted image into the asset store for durability.** Rejected: that is "copy the disc", which AGENTS.md rule 5 forbids for the image itself (the BMG it yields *is* stored, content-addressed). The job therefore re-reads the on-disk image; a directory entry that vanished after enqueue fails the job loudly with a permanent error rather than guessing.
- **Letting the upload cap grow to real disc sizes.** Rejected: the asset store's 512 MiB ceiling and the 4 GB target appliance both make a 1.4 GB HTTP copy the wrong answer; the directory shape exists for exactly this.

## Consequences and risks

- The serve binary now links the windwaker adapter (it always linked the generic one), which also makes raw `.bmg` *file* imports work through the API — the live E2E's current stopgap becomes what the product offers.
- `GAME_NARRATOR_IMPORT_DIR` is an operator setting like the replay allowlist: `doctor` and the settings view do not render it (it names a path), and the API never returns paths, so a misconfiguration is visible as `configured: false` (the list) / 503 (the dir-shape import) / 400 `invalid-image-name`, never as a path leak.
- The 64 MiB upload cap means real discs must use the directory shape; the 202 detail says so.
- The profileio override is additive; the file import's behaviour and its tests are unchanged (the kind comparison in the reused-source warning now uses the requested kind instead of a hard-coded `user_file`).

## Validation

- Unit/race: `go test -race ./internal/api/... ./internal/gcdisc/... ./internal/games/...` — traversal matrix on the dir shape, the 200 `configured:false` when the directory is unconfigured (and the 503 on the dir-shape import in the same case), the job handler over a synthetic disc (messages written, `game_extract` source, image hash in the coverage note), a non-GZL disc failing permanently, the re-import no-op, and the log capture asserting the image path is never logged.
- Contract: the two operations are in `openapi.json`, `pnpm gen:api` regenerates `schema.d.ts`, and the contract test's route↔document and `x-required-role`↔scope-table checks cover them; the authz matrix gains their rows.
- Playwright: `web/e2e/disc-import-walk.spec.ts` as the `disc` project of `web/playwright.config.ts`, on the packaged binary with the synthetic fixture (labelled as such): claim → login → game → edition → draft → import from the directory → report → dialogue review → publish → the play page lists the messages.
- Live (dev host, not CI): the same import through the API against the user's real image, symlinked into a temp `GAME_NARRATOR_IMPORT_DIR` under /tmp — reported as message count and duration only.

## Reversal

Delete `internal/api/discimport.go` (and its registration, OpenAPI operations, ADR-named test files, the UI panels, the `import.disc` job registration and `testdata/disc/`). The profileio fields and the adapter capability stay — they are additive and the CLI's behaviour is untouched.
