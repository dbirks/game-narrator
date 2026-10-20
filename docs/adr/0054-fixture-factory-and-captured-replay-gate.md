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

## Amendment (2026-10-18): the adaptive box-text crop and the recipe-revision re-OCR

Context (measured on the `windwaker-first-dialogue` fixture, real frames,
2026-10-18): the dialogue box's record-time reading crop was the fixed
`boxTextCrop` region (top 0.62). The box carries ONE line at measured y
0.74-0.84 and, for a multi-line box, its FIRST line at y 0.64-0.70. A fixed
0.62 top therefore drags the box's top border and empty interior into every
single-line box's reading: on this fixture's first box, Tesseract read the
fixed crop at conf 0.34-0.67 with unstable garbage variants ("l(newtl'd be
here!") and the reading never settled, while the same box read at conf
0.81-0.85 with the top anchored to the frame's text ink ("i knew you'd be
here!"), and the multi-line box kept its three lines at conf 0.89-0.92.
The first gameplay dialogue therefore could not commit its first message,
and `dolphin/first-gameplay-dialogue` sat in the BLOCKED register.

Decision:

- **The record-time reading crop for a dialogue-box frame is adaptive to the
  frame's pixels** (`adaptiveBoxTextCrop`, `internal/dolphin/record.go`):
  the base `boxTextCrop` with its top replaced by the first row (x 0.05-0.85,
  y 0.60-0.90) whose glyph-edge ink — the fraction of the row with
  horizontal luma step >= 30 — reaches 0.08, minus a 0.02 margin (floor at
  0.55). A text line measures 0.11-0.43 edge-ink; the box border and its
  flat interior measure <= 0.06. No strong-ink row (empty box, mid-fade)
  falls back to the base region: the reading is empty anyway, so the
  fallback cannot invent a line. The crop is a deterministic function of the
  frame's pixels.
- **The base region stays the session's region**: the profile's
  `textbox` region and the presence region keep the fixed `boxTextCrop`;
  only the record-time READING crop moves. The replay's region-crop dedupe
  (identical pixels, identical answer) is untouched.
- **The recipe records the revision**: the manifest's `ocr_box` recipe now
  carries the adaptive parameters (`ink_floor`, `margin`, `scan_y0/1`) next
  to the base region, so the reading is reproducible on the fixture's
  pixels.
- **Recipe revision is a re-finalise, not a re-record**
  (`TestRealReOCRFixture`, env-gated on `GN_REOCR_FIXTURE`): the frames,
  inputs and savestate are the recorded evidence and are not touched (their
  hashes re-verify before anything is written); only the OCR truth, the
  profile and the manifest are rewritten by the SAME code path the live
  recording uses (`ocrFrames` — the record-time OCR + canonicalisation, now
  shared by `Finalize` and the re-OCR — plus `writeTruth`, `writeProfile`,
  `buildManifest`). The fixture re-verifies afterwards, the savestates
  hash-verify, and the operator pins the new manifest SHA-256 into the
  scenarios (the pin-mismatch NOT-RUN discipline of this ADR unchanged).
  The run's backup of the pre-revision fixture is kept under
  `artifacts/fixtures/.backup-first-dialogue-pre-reocr/` (gitignored).

Validation (measured, this host): re-OCR of the 98-frame fixture (19 s):
the first box now reads one stable line (frames 6-10 conf 0.91; the
profile's line is the reading's own mode fold, so the matcher scores
MATCH_EXACT) and the three later boxes keep their prior readings; the
`dolphin/first-gameplay-dialogue` scenario runs PASS (all four messages
committed — `ww-first-01` MATCH_EXACT gen 1, `ww-first-02` gen 2,
`ww-first-03` gen 3, `ww-first-04` gen 4 — each decision.requested →
decision.resolved → speech.ready → speech.started, each in-flight speech
cancelled `region_vanished` at the box's close, 17 assertions). The
scenario's pin is the re-finalised manifest SHA-256
(`3132a9e11403ec53d1b4b9cf74d671254554e1308986c058174432ceecbfd1a1`).

Consequences: single-line dialogue boxes are read through the text, not the
box furniture; the truth remains the engine's record-time reading under a
recorded recipe (rule 3), and a crop-recipe revision no longer requires a
second boot-to-dialogue drive.

## Amendment 2026-10-20: per-run temp dirs carry an owner marker and are reclaimed at harness start (issue #14)

Context: the "materialized to a temp dir" decision above was safe for runs
that finish — the deferred cleanup removes the replay dir on success,
failure and panic — but not for runs that are KILLED. On 2026-10-20 five
SIGKILLed workers (go test -timeout panic, worker timeout) left five
`/tmp/gn-scenario-dolphin-*` dirs of 233 MB each (1.2 GB total) plus the
`gn-scenario-audio-*` sink dirs the harness never removed on the normal
path either, filling the /tmp quota.

Decision:

- **Every per-run temp dir the harness creates carries a `.gn-owner`
  marker**: the creating process's PID plus its start time in clock ticks
  since boot (the kernel field, read from `/proc/<pid>/stat` on Linux; 0
  where the OS does not expose it) and the wall-clock write time. The
  start ticks defeat PID reuse: a live PID whose start time differs from
  the marker's is a newer process, not the owner.
- **At harness start the harness reclaims its own stale dirs**
  (`ReclaimStaleTempDirs`, called from `RunScenario` and
  `runDolphinScenario`, so every entry point — the `make test-dolphin`
  replay phase, the dev-UI test worker — gets it): a prefixed dir
  (`gn-scenario-dolphin-*`, `gn-scenario-audio-*`) is removed only when
  its marker's owner PID is dead (signal 0, plus the start-ticks
  comparison on Linux); a prefixed dir with no readable marker (pre-marker
  builds) is removed only when its NEWEST mtime is older than a 24 h
  floor; a dir outside the harness's own prefixes — or a file — is never
  touched; and where liveness cannot be established the dir is kept. The
  failure direction is always conservative: a wrong "alive" costs one
  stale dir, a wrong "dead" would destroy a live worker's in-flight run.
- **The reclaimed bytes are logged** (`slog` INFO, visible in the
  `make test-dolphin` gate output): "reclaimed N stale per-run temp
  dir(s), M bytes".
- **The replay materialization stops copying the fixture's bytes**: the
  frame files are HARDLINKED into the replay dir (the fixture is
  hash-verified and never mutated, so the links ARE the content),
  falling back to a byte copy on cross-device temp dirs. A leaked replay
  dir then holds a link table, not another full copy of the fixture's
  frames; normal runs pay zero copy cost. The sliced `timestamps.json`
  and `truth.json` remain newly written files, and the "harness reads the
  slice exactly as recorded, fixture never mutated" boundary is
  unchanged.

Validation: `TestReclaimStaleTempDirs` (dead-owner dir reclaimed with its
bytes accounted; live-owner dir — the test's own PID — kept; unmarked dir
past the 24 h floor reclaimed; unmarked fresh dir kept; foreign-prefix
dir and foreign file untouched), `TestReclaimMarkerRoundTrip` (a dir this
process just materialized, with its own marker, survives a reclaim pass),
`TestProcessStartTicksSelfOnLinux` (the /proc field-22 parse is positive
and stable). Measured on the dev host on 2026-10-20: the first reclaim
pass freed 248 stale `gn-scenario-audio-*` dirs (1.49 MB) left by earlier
test runs.

Consequences and reversal: normal-completion cleanup (the deferred
cleanups) is unchanged; this amendment adds the killed-run path. Reversal
is deleting the marker writes and the two `reclaimOurTempDirs` calls —
the pre-#14 behavior (leaked dirs until manual cleanup) returns; the
hardlink materialization is independently reversible by switching
`linkOrCopyFile` back to `copyFile`.
