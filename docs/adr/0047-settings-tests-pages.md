# ADR-0047: /settings and /tests pages — read-only configuration, scenario runs as durable jobs
Date: 2026-10-17
Status: accepted
Owners: m5-settings-tests worker
Related requirements/tests: docs/spec/09 line 21/23 (`/tests`, `/settings`), docs/spec/14 (secrets never reach the browser, role ACL), docs/spec/16 gate list, ADR-0016 (auth), ADR-0017 (OpenAPI contract), ADR-0037 (blocked ≠ pass)

## Context and constraints

docs/spec/09 names two pages the appliance did not have:

- **`/settings`** — model endpoints, storage limits, security, profiles/import policies, audio sinks, backup/export, software updates;
- **`/tests`** — the scenario catalog with run controls, Arch Dolphin readiness, frame-replay mode, live-provider/hardware test status, and an HTML report viewer/download.

Constraints from the codebase, not preferences:

1. **Every setting of this build is environment- or build-constant.** The model roles come from `providers.LoadRoles` (env), the listen address, data dir, audio sink and web policy from `GAME_NARRATOR_*` vars, and the agent budget from build constants in `cmd/game-narrator`. Nothing is runtime-mutable, so the settings API has no honest write path (AGENTS.md "no simulation theater": a save button that saves nothing is worse than its absence).
2. **Secrets never reach the browser (docs/spec/14).** Gateway hosts/paths and bearer tokens are operational detail: an unauthenticated-adjacent admin read must still not leak them.
3. **The job runner cannot be extended after `Start`.** `jobs.Runner.Register` refuses a kind once the runner is started, and `cmd/game-narrator` starts the runner before the HTTP process exists — a kind this API enqueues therefore cannot be registered on the process runner without editing the process wiring (out of this task's boundary: `cmd/game-narrator/main.go` is owned by another worker).
4. **A packaged appliance has no checkout.** The binary's working directory is not the repository, so the harness must resolve its root explicitly (env) or not at all, and say so instead of serving an empty catalog that reads as "no scenarios".

## Decision

### Routes (scope table: `/api/v1/settings` and `/api/v1/admin/backup` admin; `/api/v1/dev/tests*` developer via the existing dev prefix)

| Route | Contract |
|---|---|
| `GET /api/v1/settings` | Effective configuration, read-only. Model roles (model id, protocol, base URL as the literal `"<configured>"`, reasoning effort, `capabilities: null` — the API layer holds no live client, and a capability finding nobody probed would be the docs/spec/13 bug), tokens as presence booleans only; storage (data-dir **base name**, free space, asset-store bytes, speech cache as "not distinguishable by size" with the reason); audio sink/device; security (listen address, auth mode, whether an admin exists); import policies (web search registered + untrusted, private-allowlist **count only**, agent budget as a named build constant); version/commit; the update policy (no auto-update, `docs/release.md`); and the mutable notice (`count: 0` with the controlling variables named). |
| `PUT /api/v1/settings` | `405 Method Not Allowed`, `Allow: GET`, problem+json whose detail names the environment variables a change would have to go through. The route exists so the UI can learn the answer; it mutates nothing. |
| `POST /api/v1/admin/backup` | A consistent snapshot through the store's online-backup API (read-only use of `internal/store`), streamed as an `application/x-sqlite3` download with a timestamped name. Restore is CLI-only (`game-narrator db restore`) and documented as such in the UI: a restore is a process-lifecycle act (close store, replace file, reopen). |
| `GET /api/v1/dev/tests` | The scenario catalog (walks `testdata/scenarios/**/*.yaml` through `scenario.Load`: id, catalog id, mode, origin, inference, fault count, `runnable`/`blocked` with the YAML reason / `load_error` with the loader message), the gate rows (`artifacts/gates/*.rc` + `*.xml` for the known gate set: PASS/FAIL from the recorded exit code, NOT RUN with the make instruction, JUnit counts when the xml exists), Dolphin readiness (dev flag, `dolphin-emu` and `Xvfb` on PATH, `GAME_NARRATOR_GAME_IMAGE_PATH` set — **presence booleans and variable names, never paths**), and live-provider readiness (configured/missing role names). |
| `POST /api/v1/dev/tests/scenarios/{rest...}` | Enqueues a durable `scenario.run` job (payload: the scenario id) and answers 202 with the job id. The trailing-wildcard pattern exists because a scenario id carries its directory (`synthetic/dialogue-advance-001`), which a single-segment `{id}` cannot hold; the handler requires the final segment to be `run`. A blocked scenario **runs**: the harness reports it BLOCKED with its reason (ADR-0037: blocked ≠ pass, but a run that says so is the page's point). |
| `GET /api/v1/dev/tests/runs/{job_id}` | The job's state (the durable row is the source of truth the UI polls) plus, once succeeded, the report's two links. |
| `GET /api/v1/dev/tests/reports/{sha256}` | One stored report blob by content hash. The HTML report is script-free and carries a `Content-Security-Policy` for the viewer's `sandbox=""` iframe; the JSON report is served as `application/json`. |

### The scenario worker

The run is a real job on the durable queue (kind `scenario.run`), claimed by an **in-process worker inside the API package** that speaks the store's own claim/lease/heartbeat/complete protocol for exactly this kind:

- It starts lazily on the first run POST (`sync.Once`) and stops of its own accord when the store stops answering (process shutdown closes the database; ten consecutive claim errors are its notion of "closed").
- A crash mid-run leaves the row `running`; the lease expires; the next claim takes it over (`attempts < max_attempts`); a restart parks it `recoverable` for the operator's existing retry route. The queue's crash story is unchanged — the API owns a second consumer of the same rows, not a second queue.
- The worker renders the verdict as JSON **and** a static, style-only HTML page (no scripts: the same bytes serve the sandboxed iframe and a standalone download), commits both to the asset store, and completes the job with the content hashes.
- **Verdict vocabulary is one string, end to end:** the harness reports `"passed" | "failed" | "blocked"` (`internal/scenario.RunResult`), the job-failure path reports `"failed"`, the report page's badge CSS classes are named after exactly those values, and the `/tests` page's PASS/FAIL/BLOCKED pills key on exactly those values. A drift between the API's spelling and the page's comparison is invisible until a run finishes (the pill simply never renders and the UI polls to its full wait) — `TestDevTestRunDialogueAdvanceReachesTerminal` pins the API side against the real synthetic fixture within a time bound, and the Playwright spec pins the page side.

### Harness root resolution

`WithTestHarness(repoRoot)` option (production wiring to add in `cmd/game-narrator`, listed for the owner), falling back to `GAME_NARRATOR_REPO_ROOT`, then the working directory. A candidate lacking `testdata/scenarios` is rejected, so the answer is a usable root or an honest "not wired" (`repo_root_known: false`, 503 with the variable named on runs). This is how the e2e binary — started from the checkout — finds its fixtures with no extra wiring.

### Settings redaction

The response never carries: a token (presence boolean only), a gateway host/port/path (the literal `"<configured>"`), the data directory's absolute path (base name only), or the private-endpoint allowlist entries (count only). The model id and role/protocol/timeout values are displayable configuration, per docs/spec/09's "model endpoints" section.

## Alternatives considered

- **Register the `scenario.run` handler on the process runner (main.go).** Rejected without the process owner: `Register` is start-gated, so the kind could only be added by editing `cmd/game-narrator`, which this task may not touch. The Option/handler split above means the process owner can later move the worker into the runner without changing the API contract.
- **A dedicated worker process / sidecar for scenario runs.** Rejected: a second consumer of the queue's rows already exists in one process; a sidecar would duplicate the lease protocol for no isolation the run needs (frame replay is a pure function of the checkout).
- **`PUT /api/v1/settings` returning 200 with a no-op body.** Rejected: a 200 on a write that wrote nothing is the simulation theater the 405-with-explanation refuses to be.
- **Serving the HTML report from a separate static path.** Rejected: the report is developer-gated evidence; the `/api/v1/dev` prefix grant and the content-hash route keep it behind the same role gate as everything else on the page.
- **Documenting the run route as `/api/v1/dev/tests/scenarios/{id}/run`.** Rejected: the OpenAPI contract test compares registered patterns against the document verbatim, and ServeMux cannot register the multi-segment id in a single-segment template; the documented pattern is the registered one (`{rest...}`), and the handler enforces the `/run` suffix.

## Consequences and risks

- **Two claimers for different kinds share one store.** The API worker and the process runner never claim the same kind, so there is no contention on rows; the worker's lease (30 s, 10 s heartbeats) is sized so a slow box cannot lose its own lease.
- **Report blobs accumulate in the asset store.** They are small (a verdict page) and content-addressed (identical verdicts dedupe); the asset store's GC policy is the owner of retention, as with every other blob.
- **The worker is per-process.** Two API processes (never the case in this build: one binary, one queue writer) would split the work fairly by claim; nothing depends on single-process behavior.
- **`GAME_NARRATOR_REPO_ROOT` pointing at a partial checkout** yields per-run 404s with the id named — visible, not silent.
- **Secret surface of the admin page.** Mitigated by the redaction rules above; the handler tests assert the token and host strings are absent from the JSON (a regression test for docs/spec/14).

## Evidence and test plan

- `internal/api/settings_test.go`: admin gate on all three settings/backup routes; token/host absence in the JSON with a configured gateway; `NOT CONFIGURED` with the variable named; the 405 PUT contract (Allow header, problem+json, the env list in the detail); the backup download's `SQLite format 3` header.
- `internal/api/tests_test.go`: the developer gate; the catalog with the blocked scenario's reason; the wildcard path parsing; the 404/503 contracts; and the **worker end-to-end** on a blocked scenario (202 → durable job claimed by the in-process worker → `succeeded` with `status: blocked` → both report routes serving the script-free HTML with the reason).
- `web/e2e/m5-settings-tests.spec.ts` (Playwright, chromium project): the settings page shows NOT CONFIGURED roles with no secrets in the DOM and the backup download is a valid SQLite file; the tests page lists the synthetic scenarios including the BLOCKED one with its reason, runs `dialogue-advance-001`, polls to PASS, and opens the report in the sandboxed iframe.
- Contract: `openapi.json` carries the seven operations (hand-authored, drift-checked by `TestOpenAPIDocMatchesTheRouter` / `TestOpenAPIRoleGrantsMatchTheScopeTable`); `pnpm gen:api` regenerates `web/src/api/schema.d.ts`.
- Gates: `gofmt -l internal`, `go vet ./internal/api/...`, `go test -race ./internal/api/...`, `pnpm typecheck/lint/fmt:check/test`, `make build` + the spec above.

## Migration / rollback

No schema, migration or data change: the new routes are additive, the scope-table grants are additive, and the `scenario.run` job kind is new (an old binary never enqueues it). Reverting deletes `internal/api/settings*.go`, `internal/api/tests*.go`, the route registrations, the policy grants, the OpenAPI operations, the two pages and the ADR; rows of the kind left behind are inert (no worker on an old binary, visible in the job list, retryable by hand or deleted).

## Follow-ups

- Wire `api.WithTestHarness(repoRoot)` and `api.WithListen(listen)` in `cmd/game-narrator` (process owner) so the packaged binary reports its bound address and an explicit harness root instead of the env/cwd fallback.
- Expose `openai.Capabilities()` findings on the settings page once the API layer holds a live client (the field is already `capabilities` and currently null with the reason).
- A streamed/cancellable TTS endpoint would unblock GN-T047 (the blocked scenario the page now renders with its reason). — **Unblocked 2026-10-18 by ADR-0057**: the TTS contract carries the streamed/cancellable synthesis endpoint, the scenario runs over the real streaming wire (the fault drops the SSE connection mid-stream), and the catalog lists the row as runnable; `web/e2e/m5-settings-tests.spec.ts` asserts both states (a still-blocked row with its reason, and GN-T047 runnable).
- The spec-08 `GET/PUT /api/v1/config` contract row ("redacted settings and validated changes") has never been implemented: the route pair is `/settings`, and the PUT answers 405 by design per this ADR. — **Decision recorded 2026-10-21 by ADR-0133: the deviation is kept** (single-operator LAN appliance; env plus restart; the audio setting of ADR-0074 remains the one runtime-mutable exception). docs/spec/08 now points at `/api/v1/settings` with the ADR-0133 note, and the gap-audit row G18 is closed with deviation in `docs/status/acceptance-2026-10-20.md`. This ADR's decision stands; ADR-0133 is the deciding record for the spec row, not a supersession of ADR-0047.
