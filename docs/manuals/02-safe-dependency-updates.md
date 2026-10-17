# Safe dependency updates

## Purpose

Update Go modules, the web (`pnpm`) dependencies, or host tools
(tesseract, ImageMagick, ffmpeg, Xvfb, GoReleaser) without silently changing
what the verification gates measure, and leave an auditable before/after
record. "Firmware" in the spec/13 sense does not apply to this project's
code: the UNO Q's own OS/firmware is out of scope here and is **NOT
VERIFIED** territory (docs/status/blocked.md).

## Preconditions

- A green baseline: `make test-full` (or at least `make test-unit`,
  `make lint`) on the current versions, with the gate table recorded.
- The versions you are moving **to**, pinned explicitly — no floating
  `latest`.

## Steps

1. Record the baseline versions, the same commands the host inventory uses
   (docs/dev-environment.md "Reproduce this inventory"): `go version`,
   `node --version`, `pnpm --version`, `tesseract --version | head -1`,
   `magick --version | head -1`, `goreleaser --version`,
   `dolphin-emu --version` (under `xvfb-run -a`), and `go list -m` for the
   modules you touch.
2. Update one dependency set at a time:
   - Go: `go get <module>@<version>` (or `go mod tidy`), then
     `go build ./...`.
   - Web: `pnpm --dir web update <pkg>`; the lockfile
     (`pnpm-lock.yaml`) is the source of truth — `make build` and
     `make test-ui` install with `--frozen-lockfile` behaviour
     (`pnpm install --frozen-lockfile`).
   - Host tools: `sudo pacman -Sy <pkg>`; re-run the inventory command for
     that tool.
3. Verify per surface: `make lint`, `make test-unit`. If the dependency feeds
   a gate, run that gate too: tesseract/magick → `make bench`,
   `make test-replay`; Playwright's Chromium → `make test-ui`; GoReleaser →
   `make release-snapshot`. Finish with `make test-full`.
4. Record the new versions in the run's evidence manifest
   (`artifacts/<run-id>/manifest`, AGENTS.md) and, for host tools, update
   the table in docs/dev-environment.md.

## Verification

- The `make test-full` summary table shows the same gates with the same
  results as the baseline (a gate may newly PASS; a gate may not newly FAIL
  or lose a capability for dependency reasons).
- `bin/game-narrator version` still reports the expected build.

## What NOT to do

- Never hand-edit `go.sum` or `pnpm-lock.yaml`; let the tool manage them and
  commit the resulting diff with the update.
- Do not upgrade a locked engineering choice (Go, React/TypeScript/Vite,
  SQLite driver, GoReleaser flow) without an ADR — AGENTS.md lists these as
  choices not to reopen casually.
- Do not treat model/gateway version changes as dependency updates: the
  model ids are deployment configuration (env vars, manual
  04-inference-gateway-connection), not code dependencies.
- Do not claim the update is safe because `go build` passed; the gates above
  are the evidence (AGENTS.md rule 3).
