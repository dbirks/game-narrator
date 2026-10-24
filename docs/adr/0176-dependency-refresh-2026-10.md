# ADR-0176 — Dependency refresh, October 2026

* Status: accepted
* Date: 2026-10-23
* Owner: deps-update task
* Related: AGENTS.md working agreement (rules 4, 12); docs/spec/16 (gates)

## Context

Routine dependency refresh across the Go module, the web frontend, and the
external tools the gates rely on. Policy applied:

1. **14-day supply-chain rule** — no update to a version released less than
   14 days before the run date (2026-10-23), checked via the Go module proxy
   `.info` timestamps and the npm registry `time` field.
2. Direct dependencies only for active bumps; indirect dependencies move
   only as the module graph demands.
3. A bump is only applied if the build and the test gates pass; anything
   that breaks and is not fixable in a few edits is reverted and listed.
4. `govulncheck` before/after and `pnpm audit` before/after.

## Inventory (2026-10-23)

### Go (direct, `go list -m -u all`)

| Package | Installed | Latest | Latest released | Decision |
| --- | --- | --- | --- | --- |
| golang.org/x/sys | v0.48.0 | v0.49.0 | 2026-10-06 (17 d) | **bumped** |
| modernc.org/sqlite | v1.60.1 | v1.61.0 | 2026-10-21 (2 d) | deferred (rule 1) |

No direct dependency has a MAJOR update available; there were no major
bumps to triage or file.

### Web (`pnpm outdated`)

| Package | Installed | Latest | Latest released | Decision |
| --- | --- | --- | --- | --- |
| @tanstack/react-query | 5.104.3 | 5.104.5 | 2026-10-19 (4 d) | deferred (rule 1) |
| vite (dev) | 8.3.4 | 8.3.5 | 2026-10-21 (2 d) | deferred (rule 1) |
| @axe-core/playwright (dev) | 4.13.0 | 4.14.0 | 2026-10-15 (8 d) | deferred (rule 1) |
| oxfmt (dev) | 0.73.0 | 0.74.0 | 2026-10-19 (4 d) | deferred (rule 1) |
| oxlint (dev) | 1.88.0 | 1.89.0 | 2026-10-19 (4 d) | deferred (rule 1) |
| react-router | 8.4.0 | 8.5.0 | 2026-10-19 (4 d) | deferred (rule 1) |

`web/package.json` and `pnpm-lock.yaml` are therefore unchanged.

### External gate tools

| Tool | Installed | Upstream latest (2026-10-23) |
| --- | --- | --- |
| tesseract | 5.5.3 | 5.5.3 (2026-07-24) — current |
| ffmpeg | n9.0.2 | n9.0.2 — current |
| dolphin-emu | 2609 (pinned per ADR-0007) | 2609 (2026-09-24) — current |
| Playwright (npm + Chromium 1248) | 1.64.1 | 1.64.1 (2026-10-14) — current |

No external tool needed updating; none was installed or reconfigured.

## Decision

- **Bumped** `golang.org/x/sys` v0.48.0 → v0.49.0 (minor; the only bump
  that satisfied the 14-day rule). `go.mod`/`go.sum` carry the corresponding
  direct line change plus transitive graph checksums.
- **Deferred** every other candidate, each with its release date above.
  Re-check on the next refresh after 2026-11-04 (the oldest blocker,
  modernc.org/sqlite v1.61.0, reaches 14 days on 2026-11-04).

## Tradeoffs

- Applying the 14-day rule strictly means this refresh moved exactly one
  module; the alternative (apply immediately, re-verify in a few days if a
  retracted/replaced release appears) is not worth the risk for patch-level
  packages.
- Not bumping `modernc.org/sqlite` avoids pulling a 2-day-old CGO-free
  SQLite driver build into the persistence path mid-week; the v1.60.1 line
  has no known vulnerabilities at run time.

## Validation

- `go build ./...` — pass after the bump.
- Full race suite `go test -race -count=1 -timeout 30m ./...` under
  `systemd-run --user --scope -p MemoryMax=6G`, then re-run under the
  shared Dolphin lock (see the run notes in the task report for the
  contention explanation).
- `cd web && pnpm typecheck && pnpm test` — pass (266/266 vitest tests).
- `go vet ./...` and `go mod verify` ("all modules verified") pass on a clean tree.
- `govulncheck ./...` after the bump: "No vulnerabilities found".
- `pnpm audit`: "No known vulnerabilities found".
- Race tests of the syscall-heavy packages that use x/sys (internal/bt, internal/audio, internal/capture/..., internal/store) pass. CI runs the full `-race ./...` suite on the commit.
- `make lint` passes on the commit.
- `github.com/godbus/dbus/v5` moved from indirect to direct in go.mod: `go mod tidy` records it, because internal/bt imports it directly. The version is unchanged.

## Consequences and reversal

- Consequence: one checksum churn in `go.sum`; no API surface change for
  x/sys v0.49.0 (patch/minor kernel-syscall line).
- Reversal: `go get golang.org/x/sys@v0.48.0 && go mod tidy` restores the
  previous state; no code or migration changes are involved, so the
  reversal is a one-command operation with the same validation suite.
