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

## Amendment 2026-10-21: the unset default now probes and claims (issue #43)

### Context

The original decision kept the **fixed base 18090** for the unset default so
that CI and the docs were unchanged; only `GN_E2E_PORT_BASE=auto` probed and
claimed. That default is exactly what issue #43 documents: two concurrent
`playwright test` runs in the same checkout (no variable set) both bind
18090-18119 plus the Vite dev port. The second run's webServer health check
(`/api/v1/health/live`) is answered by the first run's still-live appliance, so
Playwright proceeds and the second run's specs assert against the first run's
claimed appliance — misleading failures that look like spec regressions, with
cross-run state bleed (claims, sessions, profiles). Probe-and-claim existed
but was opt-in; the runs that collide are the ones that do not opt in.

### Decision

The **unset (and empty) value now takes the `auto` path**: probe candidate
bases 18090 + k·100 (k = 0..9) and atomically claim the first band whose
whole port set (band, dev-server port, past-band review ports) answers no
loopback connection and that no other run's lock dir under `os.tmpdir()`
claims. An explicit `<number>` remains as-is — an operator choice, validated
but never probed or claimed (unchanged). Everything else in the ADR stands:
stable offsets, the 18120-18139 live-play band left untouched, per-run
manifests, per-run `outputDir`, claim release on process exit with the
holder-pid liveness + TTL reclamation backstop.

The selection logic moved from `web/e2e/support/ports.ts` to
`web/e2e/support/portband.ts` (pure functions: no environment read, no
module-load work, no network I/O at import). `ports.ts` applies the
resolution once at config load, registers the exit-time release, and writes
the resolved base back to `process.env.GN_E2E_PORT_BASE` exactly as before,
so the webServer shells, `serve.mjs` launchers and workers inherit the
same number. The move exists so the selection is unit-testable in-process:
`web/src/e2e/portband.test.ts` runs several "runs" through the same claim
path (real mkdir locks under `os.tmpdir()`) and asserts the bands stay
disjoint.

### Consequences

- A **lone** run still binds the historical ports: the first candidate is
  18090, so a checkout with no other e2e run active resolves 18090-18119
  plus the dev port 5173, and CI, the docs and `make test-ui` behave
  exactly as before.
- Concurrent runs in the same checkout now each reserve their own band
  (18090, 18190, …), so the appliance each test talks to is the one its own
  run started. The residual probe-then-bind race (a foreign process takes a
  port between probe and bind) is now a loud webServer bind failure,
  never a silent cross-run assertion — same as `auto` already had.
- The band lock under `os.tmpdir()` (`gn-e2e-portband-<base>.lock`) is now
  taken by every default run, released on process exit; a kill -9'd run
  leaves it for the holder-pid liveness reclamation.
- An operator who wants the historical pinned behaviour can still set
  `GN_E2E_PORT_BASE=18090` explicitly.

### Validation

- `web/src/e2e/portband.test.ts` (vitest, six cases): against the legacy
  default it fails with two unset runs resolving to the same base 18090
  (the issue's collision, reproduced); after the change all six pass —
  disjoint bands for two and three concurrent runs, claim-on-disk,
  released claims reclaimable, explicit base as-is (never probed or
  claimed), invalid bases failing loudly, and the loud give-up error when
  every candidate band is held.
- `pnpm --dir web test` (28 files, 264 tests), `typecheck`, `lint` and
  `fmt:check` pass.
- A lone real Playwright run (one spec, no variable set) resolves base
  18090 and passes end-to-end; see the task report for the command and
  result.
