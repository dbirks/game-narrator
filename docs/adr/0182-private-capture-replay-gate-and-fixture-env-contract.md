# ADR 0182: The private-capture replay family is gated, and the fixture env vars keep one meaning each

- Status: accepted (implemented)
- Date: 2026-10-24
- Task: placement-gate (issue #47)

## Context and constraints

The private-capture replay family — `TestPlacementOnLiveCapture`,
`TestPlacementOnDolphinProbe`, `TestPlacementOCRBeforeAfterFix` (ADR-0137) and
`TestPresenceRejectsSeaAcceptsBoxes` (ADR-0137 amendment, the sea false
positive) — is class-(b) captured-frame replay: it asserts the placement fix
and the presence gate on the private, gitignored frames they were measured on
(AGENTS.md rules 3 and 5). Issue #47 found them in no gate:
`make test-replay` kept them `-skip`ped (a fixture-less CI host saw a clean 0)
and `make test-dolphin` ran none of them (the captures phase only ran the
presence test; the Makefile never picked up the ADR-0137 amendment's intended
`-run` pattern).

The second half of the gap was the env var: `$GAME_NARRATOR_FIXTURE_DIR`
meant two things — a *root of named fixtures* (`board-sailing-20261021`,
`windwaker-*`, `states/`) for the presence/bench gates, and a *live-capture
JPEG bundle* for the placement replays (whose `liveFixtureDir` walked the root
and treated every JPEG as a dialogue capture). Pointing the variable at the
named-fixture root made a placement replay consume an unrelated no-box capture
and fail on the wrong assertion ("picker never selected the top band").

ADR-0137's amendment (2026-10-22) fixed half of this: the bundle split — the
live-capture bundle is `$GAME_NARRATOR_LIVE_CAPTURE_DIR` when set, else the
fixture root's `live-capture/` subdirectory, **never** the whole fixture root
(already in `internal/pipeline/placement_captured_test.go`'s
`liveFixtureDir`). This ADR finishes the other half — the gate wiring — and
supersedes the amendment's placement of the family in `make test-dolphin`'s
captures phase as the family's *home* gate.

## Options

1. **The family stays in no gate.** Rejected: the measured behaviour the
   ADRs exist to prove (the picker lands on the box's band, the crop reads the
   full line, the sea is rejected) would have no gate of its own — the exact
   gap #47 reports.
2. **The family lives in `make test-dolphin` only (the amendment's plan).**
   Rejected as the sole gate: `test-dolphin` is the emulator gate — its phases
   need the disc image, `dolphin-emu`, `Xvfb` and the emulator's one-at-a-time
   host lock. A fixture-only host that has the private bundles but no emulator
   would report the replays NOT RUN for the *wrong reason* (the emulator
   prerequisites), and the replay family would inherit the emulator gate's
   scheduling cost. The replay family is class-(b) replay — `make test-replay`
   (the docs/spec/12 replay gate) is its home, and it already hosts the
   sibling replay tests (scenario suite, the deterministic pipeline replays).
3. **`make test-replay` runs the family, each test NOT RUN (never a pass,
   never a silent skip) when its bundle is absent.** Chosen. The gate script's
   documented 0/1/2 contract makes this safe on a fixture-less host: a
   `t.Skip("NOT RUN: …")` is summarised as NOT RUN with the setup line and the
   gate exits 2 — not 0 (no vacuous pass) and not 1 (no false failure);
   consumers read `artifacts/gates/<gate>.rc` (docs/dev-environment.md,
   "Gates"). CI stays green-red-green by contract, not by `-skip`.

## Decision

1. **One variable, one meaning (documented in docs/dev-environment.md,
   "Fixture and live-capture bundle layout"):**
   - `$GAME_NARRATOR_FIXTURE_DIR` — the root of the *named* fixtures, read by
     id (`board-sailing-20261021`, `windwaker-*`, `states/`,
     `trace-fixtures/`). Default: this checkout's gitignored
     `artifacts/fixtures/`.
   - `$GAME_NARRATOR_LIVE_CAPTURE_DIR` — the *live-capture bundle* root (the
     captured frames the placement fix was proven on). When unset, the fixture
     root's `live-capture/` subdirectory. The bundle is **never** the whole
     fixture root.
   - The Dolphin probe bundle (`TestPlacementOnDolphinProbe`) lives under
     `artifacts/catch-up-live/probe-*/` (the e2e catch-up run's dev-trace PNG
     frames); `probeRunDir` honours `$GAME_NARRATOR_FIXTURE_DIR` by looking in
     the `catch-up-live/` sibling of that root, so an external fixture root
     points at one artifacts tree, the same way the named fixtures do.
2. **Gate wiring (Makefile):**
   - `make test-replay` drops the family from its `-skip` list:
     `TestPlacementOnLiveCapture`, `TestPlacementOnDolphinProbe`,
     `TestPlacementOCRBeforeAfterFix` and `TestPresenceRejectsSeaAcceptsBoxes`
     run with the package's other replays. Absent bundle → NOT RUN (gate
     exit 2) with the setup line; present bundle → the assertions run.
   - `make test-dolphin`'s captures phase keeps `TestPresenceRejectsSeaAcceptsBoxes`
     and gains the three placement replays in its `-run` pattern, making the
     committed ADR-0137 amendment and docs/dev-environment.md true (they
     claimed the phase ran them; the Makefile had never wired it). The
     family therefore has a fixture-replay home (test-replay) and a
     same-day-of-Dolphin-work rerun point (test-dolphin captures) — replay
     tests are cheap against the gate they gate, and both gates report the
     bundle's absence as NOT RUN.
   - `TestDolphinScenarios` stays `-skip`ped in test-replay (its own gate: the
     test-dolphin scenarios phase) — unchanged.
3. **The bundle the placement fix was proven on** (issue #47's ask to record
   it): the ADR-0137 live-hdmi rev3 capture (session 01M5E8CG; 511 frames,
   640×480, top-box session 211 + mid-box session 300) and the
   `probe-20261021T134132Z` Dolphin probe bundle (90 frames). To re-gather: a
   live HDMI session on a Wind Waker capture where the box sits in the top
   band (record the dev-trace JPEGs into the bundle root), or rerun the
   catch-up probe.

## Consequences

- A fixture-less host (CI) running `make test-replay` sees the four replays
  as NOT RUN and gate exit 2 — an honest "capability not configured", not a
  silent skip and not a pass; `make test-full` already propagates the 0/1/2
  rc files (AGENTS.md "CLI targets").
- Pointing `$GAME_NARRATOR_FIXTURE_DIR` at the named-fixture root can no
  longer make a placement replay consume the named fixtures' frames and fail
  on a wrong capture: the replays look only in `live-capture/` (or the
  dedicated var) and `catch-up-live/probe-*/`.

## Validation (measured, 2026-10-24, dev host)

- `GAME_NARRATOR_FIXTURE_DIR=<repo>/artifacts/fixtures
  make test-replay`: `TestPlacementOnDolphinProbe` PASS (30.9 s, 90 frames,
  the box's region keeps its calibrated band for the run and the standing
  band's crop reads the box's line against the top band's empty read);
  `TestPresenceRejectsSeaAcceptsBoxes` PASS (board sailing crops/frames all
  NOT present; the three windwaker fixtures' box rows read PRESENT);
  `TestPlacementOnLiveCapture` and `TestPlacementOCRBeforeAfterFix` NOT RUN —
  the private live-hdmi rev3 bundle is not on this host (set
  `$GAME_NARRATOR_LIVE_CAPTURE_DIR` or the fixture root's `live-capture/`
  subdirectory to re-run them).
- Same gate with the env var unset: all four replays NOT RUN with their setup
  lines; gate exit 2, no pass without running.

## Reversal

Re-add the `-skip` pattern to the test-replay recipe and trim the
test-dolphin captures `-run` pattern back to the presence test; the
`liveFixtureDir`/`probeRunDir` bundle split (the load-bearing half) is
independent and stays — reverting only the gate wiring leaves the replays in
no gate again, which is exactly the state #47 reports.

## Amendment (2026-10-24, orchestrator review before merge)

Decision 3 (run the family in `make test-replay`) was **not merged**. CI runs `make test-replay` on a host that can never have the private bundles. Every run there would report the family NOT RUN, `make test-replay` would exit 2, and the CI step fails on exit 2. So the family stays `-skip`ped in `test-replay`.

Its home gate is the `test-dolphin` captures phase, which this ADR's change extended from the presence test to the whole family. `make test-full` runs that phase on a host with the private fixtures. The env-var contract, the probe-bundle lookup fix, the `-timeout 30m` on test-replay and the measured results above all stand. The captures-phase run measured 2 PASS (`TestPlacementOnDolphinProbe`, `TestPresenceRejectsSeaAcceptsBoxes`) and 2 NOT RUN (the live-hdmi bundle is absent on the dev host).
