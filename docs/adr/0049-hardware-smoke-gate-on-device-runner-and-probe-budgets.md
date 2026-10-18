# ADR-0049: Hardware smoke out of the test sweep, on-device runner, and honest doctor probe budgets

Date: 2026-10-17
Status: accepted
Owners: agent (m8-device-fixes task)
Related requirements/tests: AGENTS.md (rules 3, 12; "CLI targets"; "Minimum
quality policy"), docs/spec/10 (hardware discovery and preflight),
docs/spec/13 (diagnostics page and CLI), docs/spec/16 (implementation and
acceptance gates), docs/manuals/13-uno-q-hardware-smoke.md (runner
documentation), ADR-0048 (CI gates — the sweep this ADR corrects),
ADR-0021 (System One probe budget — the precedent this ADR generalises).

## Context and constraints

The first real run on the UNO Q appliance (2026-10-17, orchestrator findings)
and the CI failure on `go test -race ./...` (run 39090252213) exposed five
problems in how the M8 hardware smoke and the `doctor` preflight probe real
hardware:

1. **CI failed on `TestHWOCRControlledImage` ("engine timed out")** because
   `internal/hwsmoke` runs inside the unit-test sweep. The hardware smoke is
   the only package in the tree that drives real capture hardware, spawns a
   real Tesseract subprocess, and runs a 60 s serve session — none of which
   belongs in a sweep that also runs on 2-core GitHub runners.
2. **`TestHWResourceBudget` failed on the device with
   "cannot resolve the module root: stat /go.mod"**: the budget session
   assumed the repo checkout. The appliance has no checkout; it must budget
   the installed binary.
3. **`doctor` reported `capture.devices: 2 capture node(s)`** on the UNO Q:
   the Qualcomm venus codec owns `/dev/video0`/`video1` as
   memory-to-memory nodes. Counting M2M nodes as capture cards reports a
   capture card the appliance does not have.
4. **The device's thermal zones were logged as `3590 mC` (= 3.6 °C)**:
   implausible for an idle SoC. The kernel ABI says millidegrees, but this
   platform's zones report 0.01 °C values (raw 3590 = 35.9 °C idle).
5. **`doctor` reported `providers.agent_reasoning: timeout`** while
   `fast_reasoning` answered in 5.3 s on the same model. The probe budget
   (one shared `ModelTimeout` of 60 s for the whole role check) covered less
   than the role's own production allowance: the agent role sends three
   model requests at its 30 s per-request production timeout with its
   configured low reasoning effort over Wi-Fi — up to 90 s of legitimate
   latency. Meanwhile `providers.voice_clone: voice_missing` was reported as
   FAIL although no registered voice on a fresh install is the expected
   state, not a fault.

Constraints: AGENTS.md rule 3 (no simulation theatre — a hardware gate that
passes without hardware is worse than one that says NOT RUN); docs/spec/16
(unavailable hardware is NOT RUN with actionable setup, never green);
doctor's governing rule (docs/spec/13 — a service not probed is never
reported as working); the production role timeouts are operator-tuned for
the runtime loop and must not be loosened to make a diagnostic pass.

## Decision

### 1. Every `TestHW*` is gated behind `GAME_NARRATOR_HW_SMOKE=1`

`internal/hwsmoke` defines `hwGate`, which every `TestHW*` calls as its
first statement. Without `GAME_NARRATOR_HW_SMOKE=1` (exact match, like the
audio-consent switch) the test skips with the gate's NOT RUN vocabulary:
"NOT RUN: hardware smoke runs via make test-hardware /
scripts/hw/run-on-device.sh". The two sanctioned runners — the Makefile
`test-hardware` target and `scripts/hw/run-on-device.sh` — are the only two
places that set the variable, so `go test ./...` and CI's
`go test -race ./...` never drive real capture hardware, real Tesseract or a
60 s serve session. The gate's own proof (`TestSmokeGateSkipsWithoutTheEnvVar`,
`TestSmokeGateRejectsAnyOtherValue`) stays un-gated and runs in the sweep.

Under `-race`, `TestHWOCRControlledImage` scales its real-Tesseract budgets
×5 through the build-tagged `ocrTimeoutScale` constant
(`race_on_test.go` = 5, `race_off_test.go` = 1), the same mechanism and
measurement as `internal/pipeline`'s `race_on_test.go` (commit 43c8b39):
the instrumented build timed out the engine's 4 s default on a 2-core CI
runner while the 15 s-budgeted fixture reads passed. The check proves the
same thing — the OCR stack reads the controlled image within bound — only
the budget moves with the instrumented build.

### 2. The budget session budgets the binary the appliance actually runs

`TestHWResourceBudget` resolves the serve binary in the documented
precedence: `GAME_NARRATOR_BINARY` (explicit path; a set-but-missing path is
a NOT RUN with the reason, not a crash), then `game-narrator` on `PATH` —
on the appliance the installed `.deb`'s `/usr/bin/game-narrator`, so the
smoke budgets the binary the appliance actually runs — then a fresh build of
`./cmd/game-narrator` from the module root (the dev host, where the repo's
fixture replay roots exist). When none is available the session is NOT RUN
with the exact missing thing. The resolution precedence has un-gated unit
tests (host-agnostic: a fabricated PATH entry, a pinned path).

### 3. `scripts/hw/run-on-device.sh` runs the gate on the appliance

The appliance has no repo checkout, so the gate runs from the dev host: the
script cross-compiles the hwsmoke test binary for linux/arm64
(`CGO_ENABLED=0` — standalone, no module checkout needed on the device),
copies it to the device's `/tmp` (the only place it writes), runs it there
with `-test.v`, `GAME_NARRATOR_HW_SMOKE=1`, the service environment loaded
under `sudo -n` from `/etc/game-narrator/env` (the token never leaves the
process environment and is never printed) and the data dir pinned to a fresh
`/tmp` dir (the device is read-only outside `/tmp`; the run never touches
service state). It never touches the device's sshd, sshd_config,
authorized_keys or network configuration, and never reinstalls the service.
The pulled-back output is translated to the `go test -json` event stream by
`scripts/hw/verbose-to-gotest-json.awk` (subtest terminal lines are
indented under the parent's terminal line in the `-test.v` stream; the
translator must match them, dedent, and strip the trailing ` (N.NNNs)`
including the space or the summariser double-counts) and summarised with the
same `scripts/gates/junit` tool the local gate uses:
`artifacts/gates/test-hardware-device.{xml,rc}` follow the documented 0/1/2
contract. A run that produces no test events (dead connection, the binary
could not start) is a failure (1), never a 0.

### 4. `doctor` counts only capture-capable nodes

`checkCaptureDevices` queries each `/dev/video*` node's capabilities
(`v4l2-ctl -d <node> --info`, the VIDIOC_QUERYCAP `device_caps` the kernel
exposes) and counts a node only when its capabilities include
VIDEO_CAPTURE and no memory-to-memory flag (`parseV4L2Capabilities`,
fixture-tested against recordings of the UNO Q's venus nodes and a UVC
card). The ignored nodes are listed with their driver identity
("video0 (Qualcomm Venus video decoder; memory-to-memory codec node)") so
the operator sees why a node is present but unusable. A node whose
capabilities cannot be read is kept in the count — it cannot be proven
non-capture — and recorded in the detail. With v4l2-ctl present but no
capture-capable node the check is NOT CONFIGURED ("no UVC capture device
attached"), not a failure: a missing capture card is the expected state for
an appliance whose dongle is not attached (docs/spec/13's four states
distinguish "not set up yet" from "set up and broken").

### 5. Thermal readings are unit-converted before they are printed

`internal/hwsmoke`'s `thermalCelsius` converts a raw
`/sys/class/thermal/thermal_zone*/temp` value by plausibility: the raw value
as-is when it is already 0–120 (some zones report °C directly), else
millidegrees (the documented unit) when the result is plausible (≥ 5 °C —
a powered SoC sensor sits at or above ambient), else 0.01 °C (the observed
UNO Q unit) when plausible, else the reading is reported as **unit
unknown** with the raw value instead of an invented number. A diagnostic
that prints 3.6 °C for a 35.9 °C SoC is worse than one that says the unit
is unknown. The conversion is unit-tested against the recorded device
values (raw 3590 → 35.9 °C; raw 33700 → 33.7 °C millidegrees).

### 6. Doctor's role probes budget each request at the role's production allowance

`checkRoles` gives each model-role check the budget `roleProbeBudget`:
the larger of `Options.ModelTimeout` (the floor, unchanged as a default)
and the probe's total production allowance — the number of model requests
the probe sends for that protocol (chat roles: the pong completion plus the
two capability-probe completions; vision adds the image probe; System One:
the single decision probe; the voice roles: at most two TTS calls) times the
role's configured per-request `Timeout` — capped at `maxRoleProbeBudget`
(5 min) so a pathological role timeout cannot hang a diagnostic the user
runs deliberately. This is the generalisation of ADR-0021's decision-role
probe budget to every role, and it bounds the diagnostic only: the
production role timeouts are untouched, and a role configured for 30 s of
Wi-Fi latency at low reasoning effort is not killed by a 60 s shared budget
that covers three requests' worth of 20 s each. The probe's contract is
tested, not assumed: it sends the role's configured reasoning effort
(agent_reasoning's `low`, never the model's default, which on reasoning
deployments burns the output budget thinking) and the short fixed pong
prompt on every probe request (`TestProbeSendsTheRolesReasoningEffortAndShortPrompt`
against a recorded gateway; the flip side — an empty configured effort sends
no `reasoning_effort` field — is tested too).

### 7. A fresh install's missing voice is NOT CONFIGURED, not FAIL

`checkVoiceClone` distinguishes the fresh-install state from a broken
voice: with a voice registry the instance listing zero voices, or with no
registry the Base checkpoint's `voice_missing` 400 for the probe voice, is
the expected "nothing is configured for this component" state and is
reported NOT CONFIGURED — "no registered voice yet; register one from the
Voices page" — with the probe's own (redacted) evidence in the detail. A
`voice_missing` for a voice the registry actually listed, or any other
synthesis failure for an existing registered voice, stays FAIL.

## Tradeoffs

- **The gate variable duplicates the Makefile/script knowledge.** A test
  that can run only because two files know about it is a contract; the
  un-gated gate tests and the NOT RUN text (which names both runners) are
  the enforcement. The alternative — a build tag or a `TestMain` sniff —
  would hide the contract inside the compiler or the test harness.
- **The on-device runner pulls text back through awk.** A compiled test
  binary's `-test.v` is a lossy re-render of `-json`; the translator covers
  the fields the summariser reads (action, name, elapsed) and is
  exercised end-to-end on every device run, including the subtest-indent
  regression. The alternative (a Go test harness that speaks `-json` over
  the network) is a new dependency surface on the appliance for a problem
  the summariser already solves.
- **The budget formula can make one role row take up to 5 minutes.** A
  deliberately-run diagnostic tolerates that; the cap keeps the worst case
  bounded and the production loop sees none of it.
- **Capability-filtered capture enumeration changes the summary text**
  ("2 capture node(s)" → "no UVC capture device attached" on the UNO Q).
  That is the point: the old text was a false positive. The ignored-node
  list preserves the evidence the old text pretended to show.

## Validation

- `go test -race ./internal/hwsmoke/ ./internal/doctor/` (all PASS; every
  `TestHW*` skips as NOT RUN without the gate variable, including under
  `-race` — the CI failure class is gone from the sweep).
- Un-gated unit tests: the gate itself (skip message and strict equality),
  the serve-binary precedence (installed PATH binary wins; a pinned
  missing path explains the NOT RUN), the thermal conversion table (the
  recorded device values), the v4l2 capability parsing (venus and UVC
  recordings), the probe budget table (agent 30 s role → 90 s check budget;
  fast 10 s role unchanged; the 5 min cap), and the probe's effort/prompt
  contract against a recorded gateway.
- Real device (2026-10-17, UNO Q at the appliance): `scripts/hw/run-on-device.sh
  arduino@10.0.0.139` → `artifacts/gates/test-hardware-device.xml`: 20 PASS,
  0 FAIL, 2 NOT RUN (exit 2) — the NOT RUNs are the honest "dongle and DAC
  not attached" states; `TestHWResourceBudget` ran the installed
  `/usr/bin/game-narrator` (peak RSS 28 MiB over the 60 s session, budget
  512 MiB, UNO Q profile assertion met) and logged all ten thermal zones at
  35–42 °C (previously `3590 mC`).
- Real device, cross-compiled doctor (repo build to `/tmp`, service
  environment loaded, never printed): `capture.devices` NOT CONFIGURED with
  both venus nodes listed as ignored M2M codec nodes with their driver
  names; `providers.agent_reasoning` OK (chat probe answered in 345 ms —
  previously `timeout`); `providers.voice_clone` NOT CONFIGURED with the
  register-a-voice remedy (previously FAIL `voice_missing`); 0 FAIL overall.

## Consequences and reversal

- `make test-hardware` and `scripts/hw/run-on-device.sh` are the only
  sanctioned hardware-smoke runners; anything else that wants to run the
  smoke must set `GAME_NARRATOR_HW_SMOKE=1` and take the NOT RUN contract.
- The appliance's 0/1/2 evidence now lives at
  `artifacts/gates/test-hardware-device.{xml,rc}` beside the local gate's
  `test-hardware.{xml,rc}`; the release evidence bundle (AGENTS.md
  "Expected work loop") reads the device file for the hardware gate.
- Reversal: delete `hwGate` and its calls to return the smoke to the sweep
  (and re-accept the CI timeout); delete the `roleProbeBudget` call and
  revert to the flat `ModelTimeout` (and re-accept the agent-role timeout
  on slow Wi-Fi); neither reversal touches production timeouts.
