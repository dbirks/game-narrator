# ADR-0178 — Display-free dolphin-emu CLI probe and the audio.pipewire doctor row

- **Status**: accepted
- **Date**: 2026-11 (issue #61, issue #62)
- **Deciders**: doctor-audio-dolphin work item (ADR number 0178 reserved by tasking)

## Context

Two diagnostics were missing from `game-narrator doctor`:

1. **PipeWire clock / Bluetooth codec visibility (issue #61).** On the UNO Q
   (Debian 13 ARM64, PipeWire 1.4 / wireplumber 0.5) the Bluetooth speaker
   output can crackle or add latency, and nothing in the product reported the
   two numbers that decide whether that happens: the daemon clock floor
   (`default.clock.min-quantum`, stock 1024) and the negotiated BT codec
   (SBC fallback being the usual culprit). Upstream guidance (pipewire
   configuration docs, wireplumber bluetooth docs) says verify both rather
   than assume.

2. **CLI probes of dolphin-emu abort without a display (issue #62).**
   `dolphin-emu --version` / `--help` core-dump (abort, exit 134, no output)
   on the dev host, build 2609, because the binary initialises the Qt
   windowing stack before CLI argument handling. Any tooling that probes the
   binary outside the bridge (which already starts Xvfb) would report a
   false failure.

## Options considered

**Probe env (issue #62):**
- (a) `QT_QPA_PLATFORM=offscreen` — no display server required; measured on
  the dev host: bare `dolphin-emu --version` aborts (exit 134),
  `QT_QPA_PLATFORM=offscreen dolphin-emu --version` exits 0 printing
  `Dolphin 2609`.
- (b) `xvfb-run -a` / a private Xvfb — works (verified via the issue report
  and the shared-lock wrapper), but spawns a display server just to read a
  banner and competes for X resources on a host where Dolphin is
  one-at-a-time.
- (c) reuse the bridge's Xvfb for probes — couples diagnostics to a running
  session; the doctor must run with no session at all.

**Clock source (issue #61):**
- `pw-metadata -n settings` — the daemon's own settings; the source of truth
  for rate/quantum/min-quantum/max-quantum. Key naming varies by version
  (`default.clock.*` vs bare `clock.*`); the parser accepts both.
- `pactl` only (pipewire-pulse without the pw-* CLI) — can report the rate
  from the default sink's sample spec and the BT profile/codec from sink
  properties, but cannot read the quantum floor; used as a degraded
  fallback, never as the primary verdict.

## Decision

- **internal/dolphin**: `ProbeArgs()` (exactly `[--version]`) and
  `ProbeEnv()` (exactly `[QT_QPA_PLATFORM=offscreen]`) define the
  display-free probe contract; `HeadlessVersion` runs the probe and returns
  the first non-empty output line. `headless_test.go` pins the argv/env
  contract so a change that drops the offscreen platform fails the suite.
  Option (a) is chosen: it is the only option that needs no display server
  and was measured working on the exact installed build. `xvfb-run -a`
  remains the documented fallback remedy when the probe fails.
- **internal/doctor**: new `ExecEnv` seam (Exec + explicit env additions,
  default `defaultExecEnv` appends to `os.Environ()`); the `--dev`
  `emulator.dolphin` row now probes the version display-free and reports the
  banner ("Dolphin 2609"), degrading with a concrete remedy when the probe
  fails instead of launching the binary bare.
- **internal/doctor** (pipewire.go): new read-only `audio.pipewire` row,
  registered with the other audio checks. Statuses:
  - no `pw-metadata` and no `pactl` → NOT CONFIGURED ("PipeWire is not
    installed"), never a fault on a host that uses a different stack;
  - `pw-metadata -n settings` unreadable → DEGRADED (daemon not running);
  - default sink is a Bluetooth transport (name from `pactl info`,
    profile/codec from that sink's block in `pactl list sinks`, accepting
    both `bluez5.codec` and `api.bluez5.codec` property names) at the stock
    min-quantum 1024 → DEGRADED, with a remedy naming
    `default.clock.min-quantum` and codec selection via
    `bluez5.codec_config`;
  - BT transport active but the quantum floor unreadable (pactl-only host)
    → DEGRADED "unverifiable", because the stock target host runs 1024;
  - otherwise OK with the measured numbers in the detail.
  The check is strictly read-only: no daemon restart, no reconfiguration,
  no audio playback (docs/spec/13). The BT verdict is about the *default*
  sink only; a connected-but-not-selected BT sink does not warn.

## Tradeoffs

- Offscreen Qt may change with a future Dolphin build; the contract test
  plus the DEGRADED-with-remedy path keep the row honest either way. If a
  build ever refuses the offscreen platform for `--version`, the remedy
  text names `xvfb-run -a` as the recovery.
- The min-quantum warning only fires for the stock value, not for any
  quantum a BT transport "might" dislike; upstream guidance names 1024 as
  the standard crackle case and a larger-than-stock floor is what operators
  are told to move to, so warning on the stock value is the actionable
  signal without turning every host DEGRADED.
- pactl-only hosts cannot get the full verdict; the row says exactly that
  instead of guessing (a guess here would be simulation theater, AGENTS.md
  rule 3).

## Validation

- `go test -race ./internal/doctor/...` and the dolphin headless tests pass
  on the dev host (Arch, build 2609); `make lint` clean.
- Measured on the dev host: bare `dolphin-emu --version` → abort, exit 134,
  no output; under `QT_QPA_PLATFORM=offscreen` → exit 0, `Dolphin 2609`;
  `pw-metadata -n settings` → rate 48000 Hz, quantum 1024, min-quantum 32,
  max-quantum 2048 (bare `clock.*` key form on this build).
- `go run ./cmd/game-narrator doctor --dev` shows `audio.pipewire` (OK with
  the measured numbers) and `emulator.dolphin` → `Dolphin 2609
  (rendering proven by scripts/spikes/dolphin)`.
- Bluetooth crackle presence was **not measured** on any host in this
  change (no BT transport was connected on the dev host): the unit tests
  use recorded-shape synthetic fixtures with invented names, and the first
  real BT session's numbers should be documented in the setup manual per
  issue #61 step 2 — no device names or addresses in committed files
  (AGENTS.md rule 5).

## Consequences

- Doctor rows: `emulator.dolphin` (dev) and `audio.pipewire` added; the
  doctor test suite's check-list and group tests were updated accordingly.
- The ExecEnv seam is the only place doctor commands get explicit env
  additions; tests inject it like Exec.

## Reversal plan

- Remove `audio.pipewire` from `Run` (doctor.go) and delete
  `internal/doctor/pipewire.go` and its test.
- Revert `checkEmulator` to presence-only probing (keep or drop the
  ExecEnv seam with it); remove `internal/dolphin/headless.go` and its test.
  No persisted state, schema, or protocol is touched; no data migration.
