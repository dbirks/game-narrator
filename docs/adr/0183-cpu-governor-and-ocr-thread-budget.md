# ADR 0183: Make the UNO Q's CPU clock and the Tesseract child's OpenMP budget measurable and tunable

- Status: accepted (implemented; board measurement pending — the appliance is offline)
- Date: 2026-10-24
- Task: board-perf (issue #58)

## Context and constraints

The release target — an Arduino UNO Q, QRB2210 SoC, 4× Cortex-A53, max
2.016 GHz, Debian 13 arm64 — captures around 7.5 fps against the 10 fps
capture/OCR target. Community characterization of the SoC (Arduino forum
performance thread cited in issue #58) explains the gap as clock regime,
not compute:

- a light periodic workload (our ~10 fps tick with short OCR bursts) leaves
  the frequency policy modal at its ~614 MHz floor;
- free-running back-to-back work holds 2.016 GHz; the step-time difference
  between the regimes is ~6x, mostly clock;
- a 10-minute combined CPU+GPU soak peaked at 80.2 C with no frequency cap
  reduction and no cooling-device state — the board has thermal headroom;
- 4-thread workloads outperform 1–2 threads for throughput.

The deployment env pins `OMP_THREAD_LIMIT=1`/`OMP_NUM_THREADS=1`, which also
bounds Tesseract 5.x's OpenMP parallelism when the build has it — a thread
pin nobody measured, applied to the single most latency-visible child we run
(docs/status/acceptance-2026-10-22.md hardware notes).

Two facts shape the design: the wrapper **execs the tesseract binary
directly** (ADR-0012: argv-only, supervised, no shell), so a per-process
environment is the whole mechanism for a per-child thread budget; and the
clock fix is a **write to sysfs**, so the product must separate the read-only
measurement (doctor) from the write (an explicit operator script) — a doctor
that mutates the host would violate its read-only contract.

## Options

- **A. Read-only doctor row + operator tuning script + env knob** (chosen).
  The doctor reports the DVFS state it can act on; the operator decides the
  write; the OCR child gets an isolated env knob. Each piece is independently
  testable off the board (fake sysfs tree, stub engine argv/env contract,
  `bash -n`/shellcheck), so the offline appliance is not a blocker.
- **B. Doctor writes the governor when DEGRADED.** Violates the doctor's
  read-only contract, and a diagnostic command silently changing the clock
  policy of a live board is the kind of surprise the manual warns operators
  away from.
- **C. A runtime config file / web-UI toggle for governor + threads.**
  The change is a one-time per-board decision made while measuring, not a
  per-session product feature; a runtime control surface would add a write
  path, a permissions story and a state model for a knob the operator will
  set once. Revisit only if per-session tuning is actually wanted.
- **D. Do nothing; accept 7.5 fps.** The evidence says the deficit is mostly
  clock, the fix is a few lines of sysfs, and the community data predicts a
  ~6x step-time improvement — cheap to verify, cheap to revert.

## Decision

1. **`system.cpu_governor` doctor check** (`internal/doctor/cpufreq.go`).
   Read-only. Per cpufreq policy (`/sys/devices/system/cpu/cpufreq/policy*`
   under the existing `SysfsRoot` seam) it reports governor, min/max/cur
   frequency, plus a thermal sample from
   `/sys/class/thermal/thermal_zone*/temp`. Status vocabulary:
   - `NOT CONFIGURED` when no cpufreq policies are visible (sysfs absent,
     no cpufreq driver, non-Linux) — absence is not a fault;
   - `DEGRADED` with a remedy when a non-`performance` governor can park
     the clock at the policy's floor: `scaling_min_freq` equals the lowest
     `scaling_available_frequencies` entry. That is exactly the regime the
     community measurement found; the two accepted tunings (performance
     governor, or schedutil with a raised floor) are both OK by construction;
   - `OK` otherwise. Temperature is context in the detail, not a gate,
     because the measured headroom (80.2 C peak, no throttling) says thermal
     state does not cap the board.
2. **`GAME_NARRATOR_OCR_THREADS` env knob** (`internal/vision/ocr`).
   When set to a positive integer, the wrapper sets
   `OMP_NUM_THREADS`/`OMP_THREAD_LIMIT` to that value **on the tesseract
   child only** (all three exec sites: per-call `Recognize`,
   `probeSlotProtocol`, pool slot spawn). Unset or unparseable leaves
   `cmd.Env` nil — the child inherits the parent environment, which is
   exactly the pre-knob behaviour, including the board's `=1` pin.
   The knob overrides the inherited pin without touching the service env,
   so it is trivially reversible (remove one line).
3. **`scripts/board/cpu-tune.sh`** — idempotent, **dry-run by default**
   (prints before-state, target and unit path, writes nothing; `--apply`
   writes). Sets the governor (`schedutil` default, `--performance` to pin
   the max clock) and optionally a raised `scaling_min_freq`, persisted via
   the `game-narrator-cpu.service` oneshot unit (re-applied at boot,
   `RemainAfterExit=yes`). It prints before/after state and points at the
   measurement protocol.
4. **Manual + protocol** (docs/manuals/19-install-on-uno-q.md, "CPU
   frequency tuning"): the exact commands (placeholders
   `<board-user>@<uno-q-host>` only), and a measurement protocol —
   sustained fps, per-call OCR latency (min/median/p95), thermal peak —
   run **before and after each single change**, one variable at a time,
   recorded as before/after rows in issue #58.

## Tradeoffs

- The doctor can only *see* the slow regime, not *cause* it: a DEGRADED row
  on a board that is currently running is an inference from the policy
  configuration (governor + floor), not a captured 614 MHz sample. That is
  the right trade: the measurement that confirms it is the fps/latency
  protocol, and the script's before/after output supplies the raw numbers.
- The knob sets both `OMP_NUM_THREADS` and `OMP_THREAD_LIMIT` because
  Tesseract/OpenMP builds read either; a build that honours only one still
  gets the budget. The downside (an env pair a build ignores) is nil.
- The oneshot unit is a small parallel state to the service's own units;
  it is idempotent, its removal is a single `disable`+delete, and the
  manual documents the revert command.

## Validation

- `go test -race ./internal/doctor/` — fake sysfs trees under
  `t.TempDir()`: absent tree → NOT CONFIGURED; performance governor → OK;
  powersave/schedutil with the floor at the lowest available frequency →
  DEGRADED naming the script; schedutil with a raised floor → OK; multiple
  policies; thermal sample in the detail.
- `go test -race ./internal/vision/ocr/` — stub engine records the OMP
  variables from its own environment: knob unset → inherited parent values
  (default = current behaviour); knob=3 → 3/3 in the child, parent env
  untouched (no leak); invalid values (`0`, `-2`, `abc`, `1.5`) →
  inheritance.
- `bash -n` + shellcheck (clean) on the script; dry run executed on the dev
  host (correct before-state printout, no writes, exit 0); bad-flag and
  bad-`--min-freq` paths exit 1 with a message.
- **NOT RUN** (appliance offline): the on-board measurement itself —
  fps/OCR-latency/temperature before/after each single change. The
  orchestrator runs the protocol in the manual; until then the board
  performance claim stays unverified (AGENTS.md rule 12).

## Consequences

- The doctor report on the board now names the slow-clock regime and the
  remedy in one row; the doctor's check set grows by one, so the
  "every component exactly once" expectation list updated with it.
- Any tesseract exec site that appears in the future must call
  `applyOCRThreadEnv` to honour the knob; the contract test covers the
  three current sites' engine, not the call sites, so a new site silently
  falls back to inheritance (safe, but the knob would not apply there).

## Reversal plan

Remove the doctor row (one `r.add` line + `cpufreq.go`), delete the knob
(restore nil-`cmd.Env` inheritance at the three exec sites), and on the
board: `systemctl disable --now game-narrator-cpu.service && rm
/etc/systemd/system/game-narrator-cpu.service`. No data, schema, API or
fixture changes, so nothing else is affected.
