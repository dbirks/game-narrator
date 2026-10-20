# ADR-0065: Gates own their test classes — `GN_GATE` marker, real/live tests skip outside the owning gate, the re-OCR recipe leaves the test-dolphin gate, and the drive's dialogue-box success requires the measured dark box form

Date: 2026-10-19
Status: accepted
Owners: agent worker (gate hygiene, ADR-0065 task)
Related requirements/tests: AGENTS.md rule 3 (no simulation theater — label classes (a)–(e); the unit gate is class (a) synthetic-stub territory), AGENTS.md "Minimum quality policy" (each gate asserts its own class), docs/dev-environment.md ("Gates", "Gate marker (`GN_GATE`)"), docs/spec/12 (Dolphin emulator mode is never labelled unit coverage), docs/spec/11 ("avoiding false positives"), ADR-0009/0013/0037 (NOT RUN is a reported missing capability, never a pass; a NOT-RUN gate is never green), ADR-0040 (live vision tests run by default under test-models), ADR-0043 (savestate hotkeys), ADR-0054 (fixture factory; the re-OCR recipe-revision step), ADR-0058 (captured bench holdout), `internal/providers/roles.go` (`EnvGate`), `internal/dolphin` (drive/signature/screen classification), Makefile (`test-unit`, `test-dolphin`, `test-models`), `docs/dev-environment.md`

## Context and constraints

`make test-full` on clean tree 9a99985, with the live gateway env
(gateway roles incl. decision=clef-omni/systemone, ASR, TTS) and
`GAME_NARRATOR_GAME_IMAGE_PATH` set, exposed three coupled defects:

1. **The unit gate ran real and live tests.** `make test-unit` is
   `go test -race ./...`; with the image env set it booted the real Dolphin
   tests (`internal/dolphin` `TestReal*`, class (d)) under `-race` at full
   parallel load, and with the gateway env set it would have called the real
   gateway from the `TestLive*` provider/voices/runtime/e2e/agent/vision
   families. The unit gate then depended on host environment — exactly what
   AGENTS.md rule 3 (label classes) and the gate design (docs/dev-environment.md)
   forbid: the unit gate must be independent of host env, and the emulator and
   the gateway have their own gates (`make test-dolphin`, `make test-models`)
   with their own budgets, `-p 1` and NOT RUN semantics.
2. **Under that load the drive reported a false success.**
   `TestRealDriveToFirstDialogue` closed at 25 s with `reason="dialogue_box"`
   on the **name-entry** screen — its `dialogue_box` success checkpoint matched
   the classifier's BRIGHT structural box-band clause (the recheck frame
   measured `class=nameent lb=0.297 boxband[mean=118 edges=0.096]`, i.e. the
   bright band, not the real box). The post-drive recheck — which already
   required the measured DARK box form (box band mean below the bright floor,
   `bb.mean < boxBandMeanMin=100`) — then never saw the real box in 65 frames
   over its 2-minute window and failed the test. The success checkpoint and
   the recheck disagreed about what a dialogue box is; the success gate was
   the looser one.
3. **The test-dolphin gate could never be green.** Its `-run Real` selection
   included `TestRealReOCRFixture` — the re-OCR recipe-revision step (ADR-0054),
   which is NOT RUN unless `GN_REOCR_FIXTURE` names a fixture id. A
   recipe/maintenance action inside a test gate item makes the gate rc 2
   forever (a gate with a NOT RUN item is never 0, docs/dev-environment.md
   "Gates").

Additionally, two transient failures in `make test-unit` runs under the old
recipe: `internal/scenario` `TestDolphinScenarios` hit the default 10-minute
`go test` timeout under `-race` at full parallel load (the 13-scenario
recorded-fixture replay belongs to the test-dolphin gate's 30-minute phase),
and the real-Dolphin package's load-sensitive tests flaked under `-race`
full-parallel load while passing standalone under both environment
conditions (verified in this session: `go test -count=1 -race
./internal/dolphin/` PASS with the real tests gate-skipped; the soak package
PASSes in fresh unit-gate runs, all 13 chaos reports PASS, seed-42
deterministic).

Constraints: `make test-dolphin` must keep running the real tests (it is the
class (d) gate); `make test-models` must keep running the live tests; the
unit gate must pass on a fully-configured dev host without touching the
emulator or the gateway; the drive's success must be the measured shape of
the real WOTW in-game dialogue box (ADR-0054 amendment 1), never a
structural double; no ROMs or fixtures are ever committed (AGENTS.md rule 5).

## Decision

1. **A make-gate marker, `GN_GATE`.** Each go-test gate recipe sets `GN_GATE`
   to the gate's own name (`test-unit`, `test-dolphin`, `test-models`). Every
   real/live test family requires its owning gate's value as its first
   statement and skips — a plain skip ("run via make test-dolphin / test-models"),
   not NOT RUN — when it is absent or different (AGENTS.md rule 3: the
   capability is configured on this host; only the gate differs). The real
   families: `internal/dolphin` `TestReal*` (incl. the drive, the recorders,
   the savestate tests), `internal/e2e` live Dolphin/onboarding e2e, and the
   `TestLive*` tests in `internal/providers/{openai,recorded,systemone,tts,asr}`,
   `internal/voices`, `internal/runtime`, `internal/agent[.../tools]`,
   `internal/vision/ocr` and `internal/api` (the 30-minute
   `TestLiveOnboardingAPI` stays manual: `GN_GATE=test-models` plus the
   package absent from the recipe). The hardware family keeps its separate
   `GAME_NARRATOR_HW_SMOKE=1` gate (only `make test-hardware` and
   `scripts/hw/run-on-device.sh` set it). Two hermetic unit tests whose names
   contain "Live" (`TestHealthLive`, `TestLiveModePassesThroughAndLabels`)
   deliberately stay in the unit gate — they make no gateway calls.
   `make test-unit` is therefore independent of host env: it never boots the
   emulator and never calls the gateway. Documented in docs/dev-environment.md
   ("Gate marker (`GN_GATE`)").
2. **`test-unit` keeps its budget.** The recipe becomes
   `GN_GATE=test-unit go test -race -skip TestDolphinScenarios ./...`: the
   scenario replay is the test-dolphin gate's 30-minute phase (it also writes
   the HTML report), and its 10+ minute run would time out the unit gate's
   default test timeout under parallel load.
3. **The re-OCR recipe leaves the gate.** `make test-dolphin` runs
   `-run Real -skip 'TestRealReOCRFixture$'`; the gate is rc 0 when every
   real test passes. The re-OCR step is an on-demand maintenance action:
   `GN_GATE=test-dolphin GN_REOCR_FIXTURE=<fixture id> go test -count=1
   -timeout 30m -run 'TestRealReOCRFixture$' ./internal/dolphin/` (ADR-0054).
4. **The drive's success checkpoint requires the measured real box form.**
   `ScreenSig.RealBoxForm()` = `ClassOf()` is `ClassBox` AND the box band
   measures the DARK form (`Regions["boxband"].mean < boxBandMeanMin`). The
   `dialogue_box` success in `RunDrive` and the post-drive recheck both
   require it; a bright-form `ClassBox` frame (the pre-box animated scene,
   the name-entry screen's pulse) is a scene frame: the loop records the
   transition and keeps observing, and a screen that never resolves to the
   dark form fails the loop detector or the budget with the full transition
   log instead of a false pass (docs/spec/11 "avoiding false positives").
5. **The classifier is unit-tested on the failure shape.** New tests
   (`internal/dolphin/screen_class_realbox_test.go`, named without the
   `Real` pattern so they stay in the unit gate): synthetic shapes (the
   measured dark box passes; the bright structural double does not; a
   name-entry frame whose pulse measures the bright box-band clauses while
   the grid keeps the nameent signature is never the success) and, when the
   recorded fixtures are present, the real retained frames (no recorded
   name-entry frame may measure the real-box form; every first-dialogue
   dialogue-box frame must) — NOT RUN without the fixtures (AGENTS.md rule 5:
   the fixtures are external, never committed).

## Options considered

- **Build tags** (`//go:build real` / `live`) for the real/live families.
  Rejected: build tags keep the tests out of the default build, so `go vet`,
  gopls and coverage accounting silently drop class (d)/(e) code, and every
  gate recipe must remember the tag; the env marker keeps one build, and a
  bare `go test ./...` still compiles and runs each test to an actionable
  skip line.
- **Keep the capability env checks (image/gateway set) as the only gate.**
  Rejected: that is precisely the defect — host operator configuration
  (someone set the image path on this dev box) becoming a test-gate decision.
  The capability checks remain, *inside* the owning gate, where NOT RUN
  (rc 2) is the correct answer for an unconfigured host.
- **Raise the unit gate's timeout** so `TestDolphinScenarios` can ride along.
  Rejected: the replay belongs to the test-dolphin gate (its 30-minute
  budget, its HTML report, its fixture provenance); spending the unit gate's
  wall on it hides the unit gate's real latency budget, and the timeout
  tripped once already under parallel load.
- **A dedicated make target or subcommand for the re-OCR step.** A `make`
  target would still be a gate item reporting NOT RUN (rc 2); on-demand
  invocation with `GN_REOCR_FIXTURE` keeps the gate's selection pure while
  the step stays one documented command away.
- **Exclude `nameent`-like signatures by class name** in the drive's success
  checkpoint. Insufficient: the false-success frame was accepted on the box
  band's BRIGHT structural clauses while the classifier's class label was a
  separate measurement — name-listing the exclusion does not cover a pulse
  frame the classifier labels `box`. Requiring the measured dark form (the
  shape the recheck already demanded) is the root fix and is testable on
  recorded frames.

## Validation

- `make test-unit` with the live gateway env + `GAME_NARRATOR_GAME_IMAGE_PATH`
  set: rc 0, 52 packages (twice); with `env -i HOME=$HOME PATH=$PATH`: rc 0.
  A `GN_GATE=test-unit go test -race -v -run 'TestReal|TestLive'` sweep over
  every gate-owned package: 36 real/live tests SKIP, no PASS/FAIL among them
  (only the hermetic `TestLiveModePassesThroughAndLabels` runs).
- `make test-dolphin`: rc 0 — `artifacts/gates/test-dolphin.rc` = 0 and
  `test-dolphin-scenarios.rc` = 0; 7 real tests PASS (bridge, abrupt-context
  kill, three recorders, drive) plus `TestDolphinScenarios` with all 16
  scenario subtests. The drive now closes on the measured dark form —
  `class=box lb=0.000 boxband[mean=46 edges=0.031]` (band mean 46 < 100),
  recheck accepted at frame 1 — instead of the name-entry false success at
  25 s followed by a 2-minute recheck failure.
- One `TestRealDriveToFirstDialogue` via the gate variable, standalone:
  `GN_GATE=test-dolphin go test -count=1 -p 1 -timeout 30m -run
  'TestRealDriveToFirstDialogue$' ./internal/dolphin/` → ok (421 s).
- Classifier unit tests: `TestDarkBoxFormClauses` PASS on the synthetic
  shapes; `TestScreenClassRecordedFrames` PASS on the recorded fixtures
  (10 recorded name-entry frames, none the real-box form; 72 first-dialogue
  dialogue-box frames, all the real-box form); NOT RUN-skip verified without
  the fixtures.
- `gofmt`, `go vet ./...`, `go build ./...` clean.

## Consequences and reversal

Consequences: `make test-unit` is hermetic with respect to the host's real
capabilities — a bare `go test ./...` on a fully-configured dev host no
longer boots Dolphin or calls the gateway, so the unit gate's result no
longer moves with host load or operator env; `make test-dolphin` is green
(rc 0) when everything real passes, and its NOT RUN semantics apply only to
genuinely unconfigured capabilities; the drive's success is the measured
dark box form — the name-entry screen and the pre-box animated scene are
never a success, and a bright-form `ClassBox` frame now only delays the
drive (detector/budget failure with the transition log instead of a false
pass). The unit gate's skip lines are plain skips, so a developer running
`go test ./...` on a configured host sees "run via make test-dolphin /
test-models", not a capability NOT RUN.

Reversal: delete the `GN_GATE` checks (five lines per family) and the
`-skip` flags / `GN_GATE=` prefixes from the three recipes, and revert the
drive checkpoint from `screen == DriveScreenDialogueBox && sig.RealBoxForm()`
to `screen == DriveScreenDialogueBox`. The recorded cost of the reversion is
the 9a99985 evidence above: real/live tests leaking into the unit gate
under host env, the name-entry false success, and a test-dolphin gate that
can never be rc 0.

## Amendment (2026-10-20): `TestDolphinScenarios` carries the gate check itself

Decision 2 kept `TestDolphinScenarios` out of the unit and replay gates with
the recipes' `-skip TestDolphinScenarios` flags, but the test itself carried
no `GN_GATE` check. On a host with `GAME_NARRATOR_GAME_IMAGE_PATH` and the
fixtures present, a bare `go test -run TestDolphinScenarios ./internal/scenario/`
— or any go-test run outside the owning gate — executed the recorded-fixture
replay, so the test-dolphin gate's ownership was enforced only by the Makefile
and not by the test.

Amendment:

1. **The test carries the check.** `TestDolphinScenarios`
   (`internal/scenario/dolphin_test.go`) now checks
   `os.Getenv(dolphin.EnvGate) != "test-dolphin"` as its first statement and
   plain-skips ("run via make test-dolphin …") otherwise — the same shape and
   the same plain-skip (not NOT RUN) semantics as the `internal/dolphin`
   `TestReal*` tests and the rest of the real/live families.
2. **The scenario phase sets the gate.** The `make test-dolphin` second
   phase line gains the `GN_GATE=test-dolphin` prefix
   (`scripts/gates/run-go-gate.sh test-dolphin-scenarios …`), so the gate
   keeps running the replay and writing the HTML report exactly as before.
   The recipes' `-skip TestDolphinScenarios` flags (test-unit, test-replay)
   stay: they are the budget exclusion (the replay's 30-minute phase does not
   ride the unit gate's timeout), the test-level check is the ownership
   exclusion. Both together: the replay runs only under the test-dolphin
   gate's own recipe line.

Validation: `GN_GATE=test-unit go test -count=1 -run TestDolphinScenarios
-v ./internal/scenario/` SKIPs with the make-test-dolphin instruction (the
pre-amendment run on the same host executed and PASSED the scenario
subtests); `GN_GATE=test-dolphin go test -count=1 -run
'TestDolphinScenarios/dolphin/boot-letterbox' -v ./internal/scenario/`
still runs the replay. No such gate audit test existed in the tree (nothing
greps the test families for the check), so the amendment's first check is
the audit; `gofmt`/`go vet`/`go build` on the touched package clean.

## Amendment 2 (2026-10-20): the dark box form admits the box over a BRIGHT outdoor scene (the G8 telescope)

The G8 item recording (the telescope on the lookout deck, docs/status/
blocked.md row 1) restores to a BRIGHT outdoor scene: the measured deck
frame carries letterbox 0.255 (the bright sky top band ~0.51 over the
mid-luma deck bottom band ~0.255 — the deck is a rendered bright scene, not
a letterboxed cinematic), a scene centre (mid) of ~90, and a box-less
boxband mean of 71. The dark box form's letterbox ceiling
(`s.Letterbox < classBoxDarkLbMax = 0.08`, calibrated on the pure-black
margins of the indoor first-dialogue scene, lb 0.000) rejects a box over
such a scene: the box band's own clauses (dark flat band mean < 68, glyph
edges >= 0.028, band bright <= 0.08, sparse centre text, quiet lower third)
hold on the deck's dialogue box, but the ceiling fails at lb 0.255, so
`ClassOf` returned `dark` and `DriveScreenOf` the `cutscene` family — the
drive's conversation loop (keyed on `ClassBox`) and the fixture recorder's
box line family (the `dialogue_box` screen) would both miss every line of
the deck conversation, and the item box with it. A threshold on the
letterbox axis cannot separate the case: the measured system screens sit at
lb 0.156-0.212, BETWEEN the indoor box scene (0.000) and the deck (0.255),
so raising the ceiling would admit the dialogs instead.

Amendment:

1. **The letterbox ceiling becomes a scene-side guard with a measured
   negative clause.** The dark box form (`internal/dolphin/signature.go`
   `ClassOf`) now reads: the box band's own clauses
   (`l3.bright < promptLowerBrightMax && bb.mean < classBoxDarkMaxMean &&
   bb.edges >= classBoxDarkInkMin && bb.bright <= classBoxDarkBrightMax &&
   tx.edges <= classBoxDarkTextMax`) AND (`s.Letterbox < classBoxDarkLbMax`
   OR `mid.mean < classDarkDialogMinMid`). The centre-panel negative clause
   is the separator the letterbox axis lacks: a system dialog's bright
   centre panel measures mid 115+ (`classDarkDialogMinMid`, the dark-family
   dialog floor), while a bright outdoor scene's centre (the deck measures
   90) does not. A plain bright scene without the box band still fails the
   box band's `bb.mean < classBoxDarkMaxMean` (the deck's box-less band
   measures 71), so the relaxation does not turn every bright scene into a
   box.
2. **The classifier is unit-tested on the shape.** New synthetic doubles in
   `internal/dolphin/runner_test.go` (`synthBoxBrightScene`: the deck's
   measured lb 0.255 / mid 90 with the dark glyph-inked box band — must
   classify `ClassBox` and pass `RealBoxForm`; `synthBrightSceneNoBox`: the
   same scene without the box band — must NOT), and
   `TestDarkBoxFormBrightScene` in
   `internal/dolphin/screen_class_realbox_test.go` (unit gate, no `Real`
   pattern) asserting both plus the indoor dark-box regression. The
   recorded-frame half (`TestScreenClassRecordedFrames`) keeps proving the
   existing fixtures: every first-dialogue `dialogue_box` frame is still the
   real box, no name-entry frame is.

Validation: `TestDarkBoxFormBrightScene` FAILS on the pre-amendment
classifier (the bright-scene box double measured `class=dark` at lb 0.255
bb[mean=59 edges=0.037] mid=90 — the exact failure) and PASSES on the
amended one; `TestDarkBoxFormClauses` and `TestScreenClassRecordedFrames`
(including the 72 retained first-dialogue frames and the name-entry
negative check) pass unchanged; `GN_GATE=test-unit go test ./internal/
dolphin/` green (60 s). The measured deck evidence (the retained
`artifacts/runs/dolphin-savestate-2026-10-20-085709/` and
dolphin-telescope-* frames) is the calibration: deck box-less frame
`lb=0.255 bb[mean=71 edges=0.001] mid=90 class=dark`, the start-scene box
frames `lb=0.000 bb[mean=46-64 edges=0.031-0.178] class=box`.

Reversal: restore the single-clause dark box form (`s.Letterbox <
classBoxDarkLbMax && l3.bright < ... && ...`) and delete the two synthetic
doubles and `TestDarkBoxFormBrightScene`. The recorded cost is the G8 item
recording's blocker above: the deck conversation's boxes (and the item box)
classify `cutscene`, the conversation drive never sees a box, and the
fixture records no box lines.
