# ADR-0091: Bluetooth audio output as a first-class appliance surface (D-Bus controller, the lingering session, the selection integration)

Date: 2026-10-21
Status: accepted
Owners: agent worker (issues #2/#3); owner (review)
Related: issues #2 (pair/connect from the device UI) and #3 (the "connected but
no A2DP sink" state, the LE-only re-pair), ADR 0074 (audio device selection and
gain — the `bluez_output.<MAC>.1` sink identity), ADR 0081 (boot enablement),
ADR 0084 (the skill that recorded the hand-verified recipe),
`skills/uno-q-setup/references/audio.md` (the verified headless override and
the LE-only repair), `internal/bt`, `internal/api/bt.go`,
`packaging/deb/postinst.sh`, `docs/spec/10` (UNO Q software deployment)

## Context

The hand-verified bring-up (ADR 0084, the issue #2/#3 comments, 2026-10-19 on
the real board) established three facts this ADR builds on:

1. **The radio recipe is known and works** — scan (bounded), pair (a
   `NoInputNoOutput` agent), trust, connect, and the LE-only re-pair for the
   issue-#3 state. It was proven with `bluetoothctl` driven by a human over
   SSH. The appliance itself does not ship that tool as a product
   dependency, and AGENTS.md rule 6 (tools are allowlisted; the operator UI
   is the product surface) means the appliance must perform the same
   operations itself, from the process, against the daemon the human was
   talking to.
2. **The A2DP sink is a user-instance node.** WirePlumber's `bluez5` module
   exposes the paired headset as the PipeWire sink
   `bluez_output.<MAC>.1` (a2dp-sink profile, SBC) inside the *user's*
   PipeWire/WirePlumber instance. A headless appliance needs (a) a lingering
   user session for the audio-owning user, and (b) the verified headless
   override `monitor.bluez.seat-monitoring = disabled` — without it the
   device pairs and connects but no sink node appears (the exact issue-#3
   symptom that is *not* the LE-only state).
3. **A2DP SBC crackle is open** (issue #3, link level: Wi-Fi/BT
   coexistence on the shared Qualcomm antenna). USB audio remains the
   dependable output. This ADR changes none of that; it makes Bluetooth a
   reviewable operator option instead of a set of shell steps.

The selection model (ADR 0074) already needs a stable identity per output:
the Bluetooth sink's identity is the MAC, embedded in the sink ID
(`bluez_output.<MAC>.1`), and the ADR-0074 `Resolve` precedence (environment
override → stored row → defaults) must treat that ID like any other sink
name — with the addition that a sink can be *held open* (auto-reconnect) in
a way a USB device does not need.

## Decision

### 1. Radio operations: BlueZ over D-Bus, system bus, from the service

`internal/bt` talks to `org.bluez` on the **system bus** (BlueZ runs as a
system service; the D-Bus interface is the daemon's stable API and is what
`bluetoothctl` itself calls):

- **Agent**: the process registers a `NoInputNoOutput` agent with the
  AgentManager (`/org/bluez`) at startup and names itself
  `org.game_narrator.bt` on the bus. Headsets that want no PIN pair against
  it; a PIN-capable device makes `Pair` fail with the daemon's exact reason
  (surfaced as a 502 detail), which is the honest answer — the product does
  not invent a PIN.
- **Operations** (all bounded by deadline, all idempotent where the daemon
  is): `State` (adapter powered/discovering), time-bounded `Scan` (default
  8 s, clamped 1–30 s), `Devices` (the ObjectManager inventory with
  Paired/Trusted/Connected/UUIDs), `Pair` (pair + trust), `Trust`,
  `Connect`, `Disconnect`, `Forget` (remove the record), and `Repair` — the
  issue-#3 recipe: forget, a bounded rescan, re-pair (the operator puts the
  headset in pairing mode; the API says so when the rescan finds nothing).
- **Identity**: the colon-form MAC is the stable identity
  (`NormalizeAddress`: case-insensitive in, canonical out; malformed →
  400, never sent to the radio). Sink ID ↔ MAC is
  `bluez_output.<MAC>.1` (`bt.SinkID`/`bt.ParseSinkID`).
- **No `bluetoothctl`, no root, no extra groups**: the service user acquires
  a free D-Bus name; BlueZ's D-Bus accepts any local client. The
  skill/manual `bluetoothctl` recipe stays as the *human* fallback (the
  script's read-only rows still use it) and the two paths operate the same
  daemon, so state is not split.

### 2. The session model: a lingering user session for the service user

The A2DP sink node and the `pw-play` target both live in the service user's
**user** PipeWire instance. The appliance therefore runs that instance
headlessly:

- `loginctl enable-linger game-narrator` — the user manager (and with it
  the user PipeWire/WirePlumber) starts at boot with no login. Applied by
  the package **postinst** (idempotent, `|| true` on non-systemd hosts) and
  reversed by **postrm** on purge. (The skill's `bluetooth-audio.sh
  --apply` performs the same step for pre-package boards; both agree.)
- **Headless override**: postinst installs the verified file
  (ADR 0084's step 3, byte-for-byte the recipe that made the sink appear on
  the real board) into the service user's WirePlumber config directory:
  `monitor.bluez.seat-monitoring = disabled`. The tmpfiles unit gains the
  `.config` directory line so the path exists with 0700 ownership.
- **`XDG_RUNTIME_DIR`**: the service unit does not set it (the uid is not
  static — sysusers assigns it). The binary resolves the fallback
  `/run/user/<uid>` when the variable is unset, so the `pw-play`
  subprocess targets the user instance without an env-file entry. The
  env file may still pin it (operator override wins, as with every other
  env value).
- Linger is **package state, not service state**: the unit file is
  unchanged (system service, `multi-user.target`); the linger is what makes
  the *user*-side audio stack exist. `doctor` reports the linger state as a
  row (degraded when off: "the Bluetooth sink node cannot appear").

### 3. Selection and auto-reconnect

- `bluez_output.<MAC>.1` is a normal ADR-0074 selection value: `KindBluetooth`
  is inferred from the sink naming (checked before the other kinds), and the
  selection is persisted through the existing admin `PUT
  /api/v1/settings/audio` — no new persistence surface.
- **Auto-reconnect**: at boot, if the resolved selection (environment
  override, then stored row — the ADR-0074 precedence) is a
  `bluez_output.<MAC>.1` sink and the controller is wired, the process
  holds that device connected: three attempts, five seconds apart, logged
  honestly, then done (a narration run is never held up on a speaker; the
  settings page's repair hint is the operator's way in). BlueZ itself
  reconnects trusted devices; this covers the states it does not — the
  LE-only connection and a device still waking from pairing mode.
- The device list view carries a `selected` badge (the stored selection's
  MAC) and a `needs_repair` badge (connected but no A2DP unit — the
  issue-#3 state), so the page shows exactly the two facts an operator
  acts on.

### 4. API surface (operator tier)

`/api/v1/audio/bluetooth/…`: `GET state`, `POST scan` (`duration_ms`,
clamped), `GET devices`, and per-device `POST pair | trust | connect |
disconnect | forget | repair`. It inherits the existing
`/api/v1/audio/{rest…}` **operator** grant (the surface acts on the
physical radio; viewers may not). Mutation budget: 20 per 5 minutes per
client *address* (the one unspoofable value), 429 + `Retry-After`; every
mutation is audit-logged (address + operation, redacted nothing — a MAC is
not a secret). Degradation is honest: no controller (bus unreachable, or a
build without it) → **503 `BLUETOOTH_UNAVAILABLE`** and the page hides the
section (an empty 200 would read as "no devices"); adapter off → 503 with
the remediation note; daemon failure → **502** with the daemon's exact
reason (e.g. "needs a PIN"); malformed MAC → 400; unknown device → 404.

### 5. Labelled-synthetic seam and tests

`GAME_NARRATOR_BT_FAKE` wires the deterministic fake (the ADR-0074/0078
pattern): fixed inventory at obviously-fake addresses, every answer labelled
`synthetic=true` in the API. The private-bus integration test runs the
**real** D-Bus client against a **fake BlueZ service** on a private
`dbus-launch` bus — the godbus plumbing, the agent registration and the PIN
probe round-trip are exercised against a real bus, not a stubbed one.
Playwright walks the Settings → Audio page against the fake (labeled
synthetic throughout the UI); the hardware gate (hwsmoke + `doctor`) is the
only place a real radio is asserted, and is reported NOT RUN until it runs.

## Alternatives considered

- **`bluetoothctl` as a subprocess of the service.** Rejected: it is not a
  stable API (human-oriented, no machine-readable errors), it is not
  installed by the product package, and rule 6 already excludes agent-driven
  CLI tools where a daemon API exists. The D-Bus interface *is*
  bluetoothctl's own backend.
- **A user-manager service (a user unit) for the radio operations.**
  Rejected: BlueZ is on the system bus and the operations need no user
  context; a user unit would couple the radio's availability to the
  linger's health and add a second unit whose enablement would need its own
  ADR-0081-style treatment.
- **A PIN dialog in the web UI.** Rejected for now: the NoInputNoOutput
  agent covers the common case (headphones without a PIN); a PIN-capable
  device gets the honest 502 "the daemon says it needs a PIN" and the
  human fallback (the skill's bluetoothctl recipe). Revisit with a
  `DisplayPinCode` agent if the field says otherwise.
- **Storing the selection as the MAC instead of the sink ID.** Rejected:
  the ADR-0074 model stores *sink names* (the thing the audio pipeline
  actually targets); the MAC is derived, the sink ID is the persisted
  value, and `Resolve` stays sink-agnostic.

## Validation

- `go test ./internal/bt/` — unit tests on the fake (state, bounded scan,
  pair/connect/disconnect/forget, repair, the LE-only state, address
  validation, the powered-off 503) and the private-bus integration test
  (real client ↔ fake BlueZ on `dbus-launch`, including the
  `RequestPinCode` probe to the registered agent).
- `go test ./internal/api/ -run TestBt` — the route grants (viewer 403,
  operator 200), the unwired 503, the state/scan/inventory/lifecycle/repair
  views, the MAC spelling rule, the shared 429 bucket, the selected badge.
- Contract tests: the OpenAPI doc ↔ router, the `x-required-role` ↔ scope
  table, the enum vocabularies (the `kind` vocabulary gains `bluetooth`).
- Playwright (fake): Settings → Audio shows the Bluetooth section, the scan
  finds the synthetic device, the badges (selected / needs_repair) render,
  and the repair button clears the issue-#3 state.
- `doctor`: a `bluetooth` row (adapter state, linger, the headless
  override) — degraded, not failed, when the radio or the session is
  absent.
- Hardware gate (hwsmoke, real board + headset): the UI walk with a real
  MAC (kept out of the tree: reported in the evidence bundle, fixtures use
  the fake addresses). NOT RUN until the hardware is available.

## Consequences and risks

- The package now owns the linger and the headless override: an upgrade
  re-applies both (idempotent), a purge removes the linger. A board
  installed before this ADR needs the one-time `loginctl enable-linger`
  (the skill's script still does it; `doctor` says so when it is missing).
- The system bus is the seam: a host without D-Bus (some dev laptops,
  containers) runs with the Bluetooth surface unwired (503, section
  hidden) — a visible degraded state, not a crash.
- A2DP quality is unchanged (issue #3's crackle stays open); the UI labels
  the surface best-effort next to the USB baseline, per the skill.
- The `selected` badge and the stored selection read the same ADR-0074 row
  through `audio.Resolve`, so the page and the pipeline cannot disagree
  about which sink is live.
- Reversal: drop the `WithBluetoothController` wiring (the routes 503 as
  unwired), remove the postinst linger/override blocks, delete
  `internal/bt` and the page section. No schema change is involved — the
  selection row's `device` value is just a string.
