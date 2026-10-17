# Dolphin profile reset

## Purpose

Reset the emulator state the dev Dolphin bridge
(`internal/dolphin`, ADR 0034) writes for a run — save slots, config drift,
hotkey FIFO state — without ever touching the operator's real Dolphin
installation.

## Preconditions

- The bridge is **stopped** (no `make test-dolphin` / drive run in
  progress; no `dolphin-emu`/`Xvfb` processes left over — the tests'
  no-leftovers scan asserts exactly this).
- For the dev-API launch routes: the developer role **and** the
  process-level opt-in `GAME_NARRATOR_DEV_DOLPHIN=1` (without it the
  routes answer 501). `make test-dolphin` exercises the tests directly.
- You know the bridge's `DataDir` root: the default is
  `$TMPDIR/game-narrator-dolphin` (a per-user temp location, deliberately
  not the real Dolphin dirs).

## How isolation works

Every bridge run creates a **fresh isolated user dir**
(`dolphin-<unixnano>`) under the `DataDir` root and writes its own
`Config/` (`Dolphin.ini`, `GCPadNew.ini`, `Logger.ini`) and `Pipes/`
(controller-input FIFO) into it (ADR 0007 isolation rule; the launch spec
may also load a savestate or input movie, referenced read-only). `isolateUserDir` refuses to create
or use a user dir that is or contains the real Dolphin directories
(`~/.local/share/dolphin-emu` and friends) — pointing the bridge at the
real profile fails with `ErrUserDirNotIsolated`. The real dirs are the one
filesystem this package must never touch.

## Steps

```sh
# 1. stop anything running
#    (tests self-clean; for manual runs: the bridge's Stop kills and reaps
#     the supervised Xvfb and dolphin-emu children)

# 2. delete the isolated roots
rm -rf "${TMPDIR:-/tmp}/game-narrator-dolphin"

# 3. start the run again; a fresh user dir is created on launch
```

If you configured a non-default `DataDir`, delete that root instead. The
next launch re-creates the isolated dir from scratch, so any savestates,
settings or FIFO leftovers from a previous run are gone.

## Verification

- The real profile is untouched: `ls -l ~/.local/share/dolphin-emu` shows no
  new/modified files from the test run.
- A fresh `make test-dolphin` run (with `GAME_NARRATOR_GAME_IMAGE_PATH` and
  `dolphin-emu`/`Xvfb`/`ffmpeg` present; else NOT RUN) boots the isolated
  bridge: the readiness log lines (`entropy=`, `vs_baseline=`, `vs_prev=`)
  are produced and `bridge-emu.log` appears in the run's user dir.

## What NOT to do

- **Never** point the bridge's `DataDir` (or a `LaunchSpec.UserDir`) at
  `~/.local/share/dolphin-emu` or anywhere under it — the guard exists
  because a test must not corrupt the operator's emulator profile.
- Never run `dolphin-emu` by hand against the game image to "help" a
  failing test; only the supervised bridge path (isolated launch, private
  Xvfb display, proven config) is a valid input path (docs/spec/11,
  ADR 0034).
- Never delete the real Dolphin dirs to reset the tests — that is user
  state, not test state.
- Do not lower the readiness thresholds to make a stale-profile symptom go
  away; reset the profile first (09-troubleshooting-black-frames.md).
