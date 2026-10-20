# ADR-0094: Per-run e2e port base (GN_E2E_PORT_BASE) and per-run server manifests

Date: 2026-10-20
Status: accepted
Owners: agent worker (task pw-ports); owner (review)
Related: ADR-0004 (portable e2e testing), ADR-0032 (per-project webServer
pattern; its recorded port 18102 stays correct under the default base),
ADR-0066 (disc webServer 18112; the `e2e-runs` manifest lifecycle),
`web/e2e/support/ports.ts`, `web/e2e/support/servers.ts`,
`web/e2e/serve.mjs`, `web/playwright.config.ts`, `internal/e2e`
(`livePlayPortCandidates`, the 18120–18139 band the Go live-play test owns),
`docs/status/ui-coverage.md` (the UI e2e server/port inventory)

## Context

Several agent workers run `pnpm exec playwright test` concurrently in the
same checkout. Every `webServer` entry bound a fixed port
(`web/e2e/support/ports.ts`: 18090–18119 plus the Vite dev port 5173), so a
second run died with Playwright's "already used" error or, worse, raced the
first run for the same ports. Three fixed paths shared the same problem:

1. **Ports** — all 26 server ports were constants. Two runs in one checkout
   could not start the same servers at once.
2. **Server manifests + logs** — `e2e/serve.mjs` wrote
   `web/test-results/e2e-runs/<name>.json` (hard-coded), and Playwright wipes
   `outputDir` (`web/test-results`) at the start of every run. A second run
   therefore deleted the first run's live manifests mid-flight, and two runs
   of the same project clobbered each other's `<name>.json` — the specs then
   asserted against the other run's `listen` address or data dir.
3. **Test artifacts** — `outputDir: "test-results"` meant one run's wipe hit
   the other run's traces and screenshots.

The test data directories were already per-run (`e2e/serve.mjs` mktemp's
under `<os.tmpdir()>/<GN_E2E_RUN_ROOT>`, one root per Playwright process);
only the paths above were shared. The 18120–18139 band belongs to the Go
live-play test and must stay untouched.

## Decision

- **One port base per run.** `GN_E2E_PORT_BASE` (read at config load, in
  `web/e2e/support/ports.ts`):
  - unset — the default base **18090**, which reproduces today's fixed
    ports exactly (18090–18119 and the dev-server port 5173). CI and the
    docs are unchanged.
  - `<number>` — used as-is after validation (an explicit base is an
    operator choice, so it is never probed).
  - `auto` — probe candidate bases `18090 + k*100` (k = 0..9): the first
    whose whole band (base..base+29) **and** whose dev-server port answer no
    loopback connection wins. The resolved base is written back to
    `process.env.GN_E2E_PORT_BASE` (as a number) before the webServer
    commands, the `serve.mjs` launchers and the worker processes start, so
    every process in the run agrees.
- **Stable per-server offsets** — every port is `base + offset`
  (`ports.ts` is the single source of truth): `api` +0 … `fakeReleases` +29
  (contiguous band, 30 ports), dev server `base + (5173 − 18090)` (default
  5173; a moved base gives each run its own Vite instead of sharing or
  failing `strictPort:` on the first run's). Offsets never change, so the
  config ↔ spec ↔ webServer-env wiring is untouched.
- **Band placement** — the default band 18090–18119 stops exactly where the
  Go live-play band 18120–18139 starts; the auto candidates (stride 100)
  never enter it.
- **Per-run manifests and server logs** — `e2e/serve.mjs` writes the
  manifest and the durable server log to
  `<os.tmpdir()>/<GN_E2E_RUN_ROOT>/e2e-runs/` (under the run root it already
  owns for the data dirs), and `e2e/support/servers.ts` reads from the same
  path, deriving it from the same variable with the same fallback. The run
  root is one per Playwright process, so concurrent runs in the same
  checkout never read or delete each other's records; `global-teardown`
  removes the whole root.
- **Per-run `outputDir`** — `playwright.config.ts` sets
  `outputDir: test-results/run-<pid>`, so the "clear output" wipe at run
  start touches only this run's artifacts.

## Alternatives considered

- **Probe-and-pick a random free port per server at serve time.** Rejected:
  the webServer `env` blocks (role URLs, the private-endpoint allowlist,
  `GAME_NARRATOR_UPDATE_BASE_URL`) are composed from the ports *before* the
  servers start, and the specs assert the `listen` address from the
  manifest — ports must be fixed at config load, one run at a time.
- **Keep 5173 fixed and derive only the appliance ports.** Rejected: the
  second concurrent run would then reuse (dev) or conflict on (CI,
  `strictPort:`) the first run's Vite, coupling the chromium project to
  another run's Go API.
- **Lock files per port.** Rejected: stale locks from crashed runs would
  permanently block bands; a connect probe has no such failure mode.

## Validation

- `pnpm --dir web typecheck`, `lint`, `fmt:check` — pass.
- Port resolution unit-checked by direct import: unset → 18090/5173
  (today's ports), explicit `19000` → 19000/6083, `auto` with 18095 held →
  base 18190 (band 2), `auto` with one port held in every candidate band →
  loud config error, invalid values (900, non-numeric) → loud config error.
- Two concurrent `GN_E2E_PORT_BASE=auto` runs in the same checkout
  (`--project catalog` + `--project sse`, and the same project twice) both
  pass, on disjoint bases, with per-run manifests under per-run roots.
- Default run unchanged: without the variable, the suite binds exactly
  today's ports, so `make test-ui` and CI behave as before.

## Consequences and risks

- Ports are no longer constants in the code. Documentation that names a
  port (this doc, ADR-0032/0066, `docs/status/ui-coverage.md`) is correct
  for the default base; for a non-default run the port is
  `GN_E2E_PORT_BASE + offset`, and `ports.ts` stays the single source of
  truth config and specs both read.
- Residual race in `auto`: a port can be taken between probe and bind. The
  consequence is a loud webServer failure ("already used" / bind error),
  never silent cross-run data mixing.
- `serve.mjs` needed a one-line change (the manifest dir's write side) to
  make the manifest per-run; the read side (`servers.ts`) mirrors the path
  with the same environment variable and fallback. The historical note in
  ADR-0066 about `outputDir` wiping the manifests is now obsolete (the
  manifests live under the per-run root in `os.tmpdir()`, outside
  `outputDir`).
- Reversal: restore the four files to their previous contents — fixed ports
  in `ports.ts`, the `web/test-results/e2e-runs` manifest dir, and
  `outputDir: "test-results"` — and drop the `GN_E2E_PORT_BASE` handling.
  No data or schema change is involved; runs were always throwaway.
