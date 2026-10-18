# ADR-0054: Fixture factory — the dolphin fixture recorder, the captured-frame replay gate, and golden-file fixture semantics

Date: 2026-10-18
Status: accepted
Owners: agent (M6 fixture factory)
Related requirements/tests: docs/spec/11 "Fixture origins" and "Avoiding
false positives", docs/spec/12 "Frame replay", "Dolphin emulator mode" and
failure injection, docs/spec/13 evidence correlation, docs/spec/16
implementation and acceptance gates, ADR-0007 (headless render + input),
ADR-0034 (supervision + input-proof timing), ADR-0037 (frame replay and the
blocked register), ADR-0043 (single-pipe drive; savestate hotkeys opt-in).
Tests: `TestRealRecordFixture` and the `record_test.go` unit tests
(internal/dolphin), `TestDolphinScenarios` (internal/scenario),
`testdata/scenarios/dolphin/*.yaml` (16 authored scenarios).

## Context and constraints

M6 needs the "fixture factory": a way to turn one real Dolphin drive into a
replayable, verifiable, re-creatable artifact, and to run the authored M6
scenario set against it as a first-class gate (`make test-dolphin`). Four
constraints governed this ADR:

1. **No game material in git** (AGENTS.md rule 5): frames, OCR text and game
   image paths are evidence, not source. They live only under the gitignored
   `artifacts/fixtures/` and are referenced by id + hash.
2. **No simulation theater** (AGENTS.md rule 3): the replay is real
   captured-frame replay (source `dolphin-emulator`), and the inference
   layer is recorded inference replayed through the fake provider
   gateways — the report must say so, never present it as live.
3. **One fixture, many scenarios**: the recorded drive (boot → title →
   memory card → name entry → file select → opening legend) yields one
   fixture; the 14 runnable M6 scenarios are slices of it, so the whole set
   replays from one artifact.
4. **Concurrent workers**: other test sweeps on this host run the dolphin
   package in parallel. The gate must stay honest under that — a re-record
   in flight must never corrupt a passing gate result.

## Decision

- **The recorder is a `Bridge` wrapper** (`internal/dolphin/record.go`,
  `FixtureRecorder`): it records every drive iteration's screenshot (with
  its screen class and side), every injected wire (the input log), and — on
  `Finalize` — the drive's oracle (success screen + subtitle-timing
  record). It writes `manifest.json` (frame SHA-256 + capture ts + screen
  class + side, input log, oracle, the game image SHA-256 only — never the
  path), `truth.json` (per-frame bottom-bar `box_present` + the real
  Tesseract reading of the subtitle crop, with the crop recipe recorded so
  the replay's OCR answers are the recording's own) and `profile.json`
  (the distinct lines as `ww-legend-NN` message ids with a next-edge).
  The frame text itself never leaves the gitignored tree.
- **Fixtures are golden files**: `TestRealRecordFixture` verifies an
  existing self-consistent fixture (frames vs the on-disk manifest) instead
  of re-recording it, and only records when the fixture is missing or
  corrupt. Re-recording is a one-time capture act: the recorder's mechanics
  are unit-tested (fake bridge) on every run, and the real bridge/drive are
  re-proven by `TestRealDolphinBridge` / `TestRealDriveToFirstDialogue` on
  every run. Delete the fixture directory to force a re-record.
- **The manifest SHA-256 is the fixture's version**: every dolphin scenario
  pins `input.manifest_sha256`. A replay verifies the fixture's self-
  consistency and the pin before running; a mismatch (e.g. after a
  re-record) makes the scenarios NOT RUN with the re-record instruction,
  and the recorder's run log carries the new hash to pin.
- **The replay runs the real recognition/runtime path**: scenario slices
  (0-based inclusive `first`/`last`, the loader's `last: 0` = "to the end")
  are materialized to a temp dir and replayed by the existing scenario
  harness in `frame_replay` mode — layer A the pipeline over the frames,
  layer B the session sidecar issuing recorded decision/TTS requests
  against the fake gateways — at the clip's 2.2 s capture cadence.
  `drop_frames` is the one fault a dolphin-origin scenario may carry: it
  withholds frames from the REPLAY stream (a harness knob), the recording
  is untouched.
- **The per-run HTML report** lands at
  `artifacts/scenarios/<run-id>/index.html`: one bundle per scenario with
  the frame slice, the sanitized event timeline (text payloads stripped —
  rule 5), the WAV bytes per spoken occurrence, and the assertion verdicts.
  BLOCKED scenarios appear as register rows (status + reason + unblock
  path) with no run evidence attached.
- **The gate** `make test-dolphin` runs the recorder/drive tests
  (`-run Real ./internal/dolphin/...`, 45-minute budget) and then the
  scenario replay (`-run TestDolphinScenarios ./internal/scenario/...`);
  the combined rc lands in `artifacts/gates/test-dolphin.rc` (FAIL=1,
  NOT RUN=2) for `make test-full`.
- **BLOCKED is first-class**: scenarios that need savestates or later game
  regions (press-paced legend advance; the first gameplay dialogue) are
  authored with `status: blocked` + a reason + an unblock procedure, and
  are registered as BLOCKED — never silently dropped, never run.

## Tradeoffs

- **Verify-don't-rerun (golden files)** trades "the real recorder drive is
  re-proven on every gate run" for a STABLE gate: the pinned manifest hash
  stays valid across runs, concurrent sweeps can't corrupt a pass, and a
  corrupted fixture fails loudly (hash mismatch) instead of being silently
  replaced. The cost is that after the first capture the real record path
  is exercised only on re-record (delete the fixture dir) or on a corrupt
  fixture — accepted because the bridge/drive real tests run every time.
- **One fixture per authored set**: the 16 scenarios share the
  `windwaker-newgame` fixture. A richer set (later game regions) needs
  savestate-based fixtures (ADR-0043 opt-in path) — a follow-up, not a
  change to this shape.
- **Pinned hash + re-record = NOT RUN, not FAIL**: a host that re-records
  gets a NOT RUN with instructions rather than a silent pass of stale pins;
  the operator updates the pins from the run log. Explicit, not clever.

## Validation

- `go test -race ./internal/dolphin/`: recorder unit tests (fake bridge:
  manifest/hashes, input log, tamper detection, bad-id rejection,
  screenshot-failure) plus `TestRealRecordFixture` (real image: drives to
  the legend narration, finalizes, verifies the fixture and the oracle).
- `go test -race ./internal/scenario/ -run TestDolphinScenarios`: 14 PASS,
  2 BLOCKED, 0 FAIL against the recorded fixture; the HTML report bundle
  is written per run.
- `make test-dolphin`: green on this host (Dolphin available); NOT RUN
  (exit 2) with setup instructions where the image/tools are absent.
- Determinism spot-check: the advance scenario's cancel (region_vanished,
  exactly once) and the dropped-frames shape held across repeated trials.

## Consequences

- M6's scenario evidence is real captured-frame replay with a verifiable
  provenance chain: fixture (hash-pinned) → scenario (id + hash) → run
  (report with frames/timeline/audio).
- The dolphin gate is now two phases (record/verify + replay) and the
  45-minute budget covers the real boot-to-legend drive on llvmpipe.

## Reversal plan

Delete `internal/dolphin/record*.go`, `internal/scenario/{dolphin,report}*.go`,
`testdata/scenarios/dolphin/` and revert the `make test-dolphin` recipe and
the scenario loader's dolphin origin. The fixture under `artifacts/fixtures/`
is untracked and simply ignored; no schema or runtime surface is touched.
