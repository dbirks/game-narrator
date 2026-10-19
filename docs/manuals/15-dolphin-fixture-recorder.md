# Dolphin fixture recorder and the test-dolphin gate

## Purpose

Record a **replayable fixture** from a real Dolphin drive against the
user-supplied game image, replay the committed dolphin scenarios through
the real recognition/runtime path, and read the HTML report that links
each scenario's frames, OCR, candidates, decisions and audio. This is the
M6 fixture factory (docs/spec/11 "Fixture origins"; ADR 0054) and the
`make test-dolphin` gate (docs/spec/12 "Dolphin emulator mode").

## Preconditions

- `GAME_NARRATOR_GAME_IMAGE_PATH` set to the user's authorized game image
  (dev-only; the image path is never logged and never recorded in Git).
  `doctor --dev` verifies the image's SHA-256 against the fixture manifests'
  `game_image_sha256` (it prints the 12-char prefix only, never the path),
  and reports `NOT CONFIGURED` with the exact expectation — env var, file
  kind (CISO/ISO), verification prefix, the gates that need the image — when
  it is unset or the configured file is missing (ADR-0067).
- Dev-host tools on PATH: `dolphin-emu`, `Xvfb` (xorg-server-xvfb),
  `ffmpeg`, `x11-utils`, `tesseract` — and `xdotool` for the savestate
  hotkey tests (16-dolphin-savestates.md). `scripts/bootstrap-arch.sh`
  installs them.
- The external fixture root: `$GAME_NARRATOR_FIXTURE_DIR` (default: the
  gitignored `artifacts/fixtures/`). A captured fixture of a commercial
  game is **never committed** (AGENTS.md rule 5).
- `make test-dolphin` drives the tests directly (no server, no role). The
  dev-API launch routes (`/api/v1/dev/dolphin/...`) additionally need a
  `developer` account and `GAME_NARRATOR_DEV_DOLPHIN=1` (08-dolphin-profile-reset.md).

## Steps

1. **Run the gate:**

   ```sh
   make test-dolphin
   ```

   It has two phases (one emulator at a time, `-p 1`):
   - **Phase 1 — real Dolphin tests** (`-run Real ./internal/dolphin/...`,
     45-minute budget): `TestRealDolphinBridge` (isolated boot, proven
     config, input over the pipe), `TestRealDriveToFirstDialogue` (cold
     boot to the opening legend through real Pipes input),
     `TestRealRecordFixture` (the new-game drive through the **fixture
     recorder**), `TestRealRecordLegendDense` (the dense ≥10 Hz legend
     recording, ADR 0058 amendment 1), `TestRealAbruptContextKill` and
     `TestLaunchRejectsRealUserDir`.
   - **Phase 2 — scenario replay** (`-run TestDolphinScenarios
     ./internal/scenario/...`, 30-minute budget): replays every committed
     scenario under `testdata/scenarios/dolphin/` (16 today) through the
     real recognition/runtime path at the recorded capture cadence, with
     the **recorded** inference (decision + TTS replayed through the fake
     provider gateways — captured-frame replay, never live).

   Every missing capability reports `NOT RUN: <setup instruction>` (exit
   2) — the gate is never green without the tools and the image.

2. **Understand the recorder's golden-file semantics.**
   `TestRealRecordFixture` records frames, capture timestamps, the input
   log, screen-class labels and the drive's oracle into
   `$GAME_NARRATOR_FIXTURE_DIR/<id>/` with a **manifest of per-frame
   SHA-256 hashes**. If the fixture is present **and verifies against its
   manifest**, it is NOT re-recorded; re-running the test re-records (the
   drive's frames are what they are on this host), the NEW manifest hash
   is logged, and the committed scenarios — which pin the **old** hash —
   report NOT RUN until they are re-authorized against the new recording.
   Delete the fixture directory to force a re-record.

3. **Read the replay report.** Phase 2 writes
   `artifacts/scenarios/<run-id>/index.html` (the gitignored evidence
   bundle): one section per scenario linking its frames, OCR, ranked
   candidates, decisions and audio. The clean-tree replay passes 14 of the
   16 scenarios; the two blocked rows (`first-gameplay-dialogue`,
   `legend-press-skip`) are reported BLOCKED with their unblock steps —
   both need a fixture re-recorded from a savestate (16-dolphin-savestates.md).

4. **Benchmark the captured holdout.** With the fixture and the image
   present, `make bench` also runs the fixture's frames through the real
   pipeline (class (b) captured-frame replay), each fixture reported
   separately from the synthetic holdout, then the calibration fit over
   the tuning split plus the fixtures' real-frame observations and their
   deterministic degraded-render variants:

   ```sh
   go run ./cmd/game-narrator bench captured \
     --fixture "$GAME_NARRATOR_FIXTURE_DIR/windwaker-newgame" \
     --image "$GAME_NARRATOR_GAME_IMAGE_PATH" \
     --out artifacts/bench-captured/windwaker-newgame
   ```

   The frames are verified against the manifest, truth comes from the
   fixture's oracle, and the script is imported **at run time** from the
   image (the image is never stored; its SHA-256 must match the fixture
   manifest's). Absent fixture or image: `NOT RUN` with setup steps —
   never a silent pass, never a failure of the synthetic part.

## Verification

- Per-test `PASS` lines plus the JUnit summary (`junit: …`) and the
  recorded exit code in `artifacts/gates/test-dolphin.rc` (0 = all ran and
  passed, 1 = any FAIL, 2 = nothing failed but at least one NOT RUN —
  read the `.rc`, not make's own exit status).
- The fixture manifest hash in the gate log equals the manifest hash the
  committed scenarios pin (or NOT RUN with the re-authorization note).
- The HTML report exists under `artifacts/scenarios/<run-id>/` and its
  links resolve to the run's frames, OCR, candidates, decisions and audio.

## What NOT to do

- **Never commit a captured fixture** (frames, input log, oracle or
  manifest) — external fixture dir only, referenced by id + manifest
  hash; game text and game frames never enter the repo (AGENTS.md rule 5).
- Do not run `dolphin-emu` by hand against the image to "help" a failing
  test — only the supervised bridge path (isolated launch, private Xvfb
  display, proven config) is a valid input path (docs/spec/11, ADR 0034).
- Do not edit a scenario's `manifest_sha256` or cadence to make a replay
  pass: a missing or altered fixture is NOT RUN, and the timing oracle is
  the measurement (09-troubleshooting-black-frames.md for the frame side).
- Do not label the scenario replay as live end-to-end: it is captured-frame
  replay with recorded inference (AGENTS.md rule 3). The live play path is
  a different gate (`internal/e2e`, docs/status/acceptance.md).
