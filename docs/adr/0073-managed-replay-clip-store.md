# ADR-0073: A managed replay clip store with an API and UI — uploaded clips are replay roots by default

Date: 2026-10-19
Status: accepted
Owners: agent worker (replay-clip-import)
Related requirements/tests: docs/spec/08 (scope table — developer tier for sensitive gameplay evidence), docs/spec/09 (routes and page requirements), docs/spec/10 (env file protected at 0600; resource budgets), docs/spec/16 M8 (sustained play), ADR-0024 (replay allowlist), AGENTS.md rules 3/5/6/9. Tests: `internal/clips/` (extract, id, index, validate), `internal/api/clips_test.go`, `internal/api/authz_test.go` (the scope-table rows), `internal/runtime` (clip-by-id session start), `web/e2e/replay-clips-walk.spec.ts` (the packaged-binary walk on a server with NO replay allowlist).

## Context and constraints

The 30-minute sustained run on the real UNO Q (appliance `61052a1`, 2026-10-19) could only replay the committed synthetic clip by appending `GAME_NARRATOR_REPLAY_ROOTS` to `/etc/game-narrator/env` — the unit's `EnvironmentFile`, protected at 0600, **the same file that holds the gateway token** (docs/spec/10) — and restarting the service. The production default admits no replay directory, and no product surface existed to bring a recorded clip (a replay directory/archive as `internal/capture/replay` reads it) onto an installed device. That is an operator footgun: the routine of "get a clip onto the box" was "open the secrets file and restart the service".

A clip is user content: AGENTS.md rule 5 requires provenance recorded and size limits; rule 6 keeps it out of operator-only surfaces; docs/spec/08 keeps sensitive gameplay evidence (recorded frames are the same class as the dev-trace store) behind the developer tier.

## Options

1. **Managed clip store + API + UI** (adopted): `POST/GET/DELETE /api/v1/replay/clips` at the developer tier; the archive commits to the asset store under its content hash before extraction; extraction is confined to a staging directory with entry/size limits (zip-slip, symlink and absolute-path entries rejected — `internal/clips/extract.go`); the layout validates against the replay manifest contract; the clip installs under a slug id into `<data dir>/replay-clips`, which the runtime always treats as a replay root.
2. Keep the env-file operator step (status quo) — rejected: it is the footgun this ADR exists to remove, and it is a configuration change for what is product content.
3. A generic "import" route reusing the onboarding/disc machinery — rejected: clips are not a game script or a disc; a dedicated route with its own id contract, limits and provenance row keeps the security surface small and the contract explicit.

## Decision

- `POST /api/v1/replay/clips` (multipart, tar or zip, ≤ 512 MiB archive, ≤ 1 GiB extracted, ≤ 50 000 entries, ≤ 64 MiB/file) uploads, commits the archive to the asset store (provenance = the archive's SHA-256, re-verified), extracts into a random staging dir, validates the layout, and installs it under the chosen id — caller `name` field, else the archive's top-level directory, else the filename, else a hash-derived fallback; a collision is a 409, never a silent rename. Every earlier failure leaves no directory and no index row.
- `GET` lists the store (reconciling index rows against directories: orphan directories are adopted only when they validate; orphan rows list as `missing`). `DELETE` removes one clip by id (the id contract — no separators — is the path-traversal boundary; the id is validated before any filesystem access).
- The scope table grants `/api/v1/replay/*` to the **developer** tier (admin always passes; an operator cannot list, upload or delete). A clip is a recording of gameplay frames — the same evidence class as the dev-trace store — not an operator convenience.
- The runtime's managed clips dir is a replay root by default, in addition to any `GAME_NARRATOR_REPLAY_ROOTS`; a session may select a clip with `frame_source {"kind":"replay","clip_id":"<id>"}`, which outranks `dir` and still passes the replay-root allowlist (the resolved path is exactly `<store>/<id>`).
- UI: a developer-tier **Replay clips** page (upload / list with provenance / delete) and the Play page's source picker, which lists installed clips; selecting one disables the directory field. No host path appears in any response or on any page (docs/spec/14).
- `scripts/hw/sustained-on-device.sh` now uploads/deletes its clip through this API; it reads the env file once for the non-secret listen address and never writes it, and never restarts the service.

## Tradeoffs

- A developer (or admin) can now install a replay source on an appliance without an operator: intended — that is the product path — and the scope table is the only gate, so the role is the deliberate boundary, not a gap.
- Archives persist in the asset store for provenance even after a clip is deleted (the delete removes the clip's directory and index row, not the committed hash); that is the documented content model (hash-addressed, verified) and a purge path is a separate decision.
- The 512 MiB upload cap bounds the browser-upload case; a clip too large for it must be recorded at a lower frame rate — the cap is a product decision recorded here, not a server default.

## Validation

- Unit: `internal/clips` (extraction limits, zip-slip/symlink/absolute-path rejection, id contract, index durability), `internal/api/clips_test.go` (upload/list/delete, path-traversal ids, 409 collision, provenance), `internal/api/authz_test.go` (the three rows at the developer tier), `internal/runtime` (clip-by-id resolves only within the managed store; the unwired process answers `ErrClipsUnavailable`).
- Packaged binary, real browser (AGENTS.md rule 9): `web/e2e/replay-clips-walk.spec.ts` on a server that carries **no** `GAME_NARRATOR_REPLAY_ROOTS` at all — the walk uploads the committed synthetic clip through the page, asserts the row's frame count and the archive's own SHA-256, plays it from the Play page's clip picker (two commits), and deletes it; the isolation spec asserts the server's own data dir.

## Reversal plan

Revert the route registration, the `ClipsDir` wiring in `cmd/game-narrator` and the runtime's default root, and remove the UI page; the env-file operator step in docs/manuals/13 returns as the only path. The asset store's hash-addressed model is untouched, so no migration is needed — committed archives simply become orphaned content.
