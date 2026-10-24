# ADR-0173: RAM game state — read-only MEM1 reads and RAM-driven goto/wait_state route steps

- Status: accepted
- Date: 2026-10-24
- Owners: dolphin-ram worker
- Depends on: ADR-0162 (play-loop routes), the dolphin bridge (pipe input injection)

## Context and constraints

The play loop navigates by dead reckoning and vision. The owner wants reliable
navigation to NPCs. The running Dolphin exposes the emulated GameCube memory on
Linux as a shared-memory mapping (`/dev/shm/dolphin-emu.<pid>`, created by
Dolphin's `MemArena`); the process's `/proc/<pid>/maps` locates the MEM1 block
(file offset 0x2040000 on this host, inside a 60 MiB contiguous fastmem arena
holding MEM1 + extended RAM). No ptrace, no writes: `/proc/<pid>/mem` reads
against the mapping, or the backing file directly.

Wind Waker (GZSE01/GZLE01) RAM layout, public sources:

- https://github.com/LagoLunatic/WW-Hacking-Docs/blob/master/RAM%20Map.txt
  (player true position `0x803E440C..14`, player entity pointer
  `0x803CA74C`, stage name `0x803C9D3C`, actor queue head/count
  `0x8037202C/30`)
- https://github.com/LagoLunatic/WW-Hacking-Docs/blob/master/Entity%20RAM%20Formats.txt
  (`fopAc_ac_c` entity: actor ID `+0x008`, live-entity word `+0x0C0`, position
  floats `+0x1F8`, rotY `+0x206`, room `+0x20A`)
- Dolphin Memory Engine / MemArena shared-memory design (Dolphin source,
  `MemManager`/`MemArena`): on Linux the emulated arena is a `shm_open`
  mapping visible to same-user processes.

Positions are read as **opaque world units** (big-endian floats). The unit
scale is consistent with mm (the fixture deck reads y≈1650 for a deck roughly
a metre or two above the water) but the public docs do not state the scale, so
tolerances are expressed in world units, not metres.

## Decision

1. `internal/dolphin/ram.go` (with `ram_unix.go`, `ram_bridge.go`):
   - `OpenMem1(pid)` locates the arena from `/proc/<pid>/maps` (falling back to
     the `/dev/shm` backing file) and exposes a read-only arena snapshot.
   - `Mem1.GameState()` → position (x, y, z), facing (entity rotY), stage name,
     room, player entity pointer; `Mem1.Actors()` → bounded entity list
     (actor ID, position, rotY, room) from the actor queue.
   - Typed errors `ErrMem1NoProcess` / `ErrMem1NotFound` / `ErrMem1Access` /
     `ErrMem1Unrecognized` so callers report NOT RUN instead of guessing.
   - Read-only, bounded, safe when the mapping is absent.
2. Route step kinds `{"goto": {x, z, tolerance, timeout_ms}}` and
   `{"wait_state": …}` in the runner (ADR-0162 route vocabulary), closed-loop
   on RAM state: read → steer stick → hold 1 s → re-read, until within
   tolerance or the deadline. The measured `GameState` lands in the step
   trace (route timeline) every iteration.
3. Steering is relative to a **measured** stick-forward axis, calibrated by a
   one-time startup probe (1 s forward hold; its displacement gives the axis
   and the walk speed), and re-estimated only from near-aligned walks
   (|turn| < 0.5 ≈ heading error < 22°) with displacement well above the
   ±2-unit idle-bob floor. The stick is camera-relative in Wind Waker and the
   character's rotY drifts off the stick axis while walking, so the entity
   facing is only a seed.

### Shared files touched (merge note for the npc-atlas worker)

`route.go`, `runner.go`, `script.go` gained only the `goto` / `wait_state`
step dispatch — self-contained branches (grep `goto`/`wait_state`); no
existing step logic restructured. All RAM machinery is in new files
(`ram.go`, `ram_unix.go`, `ram_bridge.go`, `ram_test.go`, `ram_live_test.go`).

## Alternatives considered

- **ptrace/`/proc/<pid>/mem`-only access:** works under
  `ptrace_scope=1` only with CAP_SYS_PTRACE; the maps/shm path needs neither
  and is strictly read-only. Rejected for the default path, kept as
  `ErrMem1Access` diagnostic.
- **Vision-only navigation (status quo):** no closed loop; drifts; cannot
  assert proximity to an NPC. Kept as fallback: the RAM steps are
  additive and report NOT RUN when MEM1 is absent.
- **Writing to RAM (teleport):** would prove the mapping but violates the
  read-only contract and the "no simulation theater" rule; the closed-loop
  goto is the honest proof.

## Evidence (measured, class (d) — running Dolphin, fixture savestate)

- Arena: `/dev/shm/dolphin-emu.<pid>`; MEM1 at file offset 0x2040000;
  60 MiB contiguous fastmem arena.
- `GameState` at fixture boot: x=-195402.0, y=1650.0, z=313668.0,
  facing=-180.0, stage="sea", room=0, player entity 0x80AB86BC (extended RAM,
  ~11 MiB past the MEM1 block — hence `mem1PtrMax` covers MEM1 + 28 MiB).
- Stick hold 2 s forward → position delta 166.9 units, forward-alignment
  0.935 vs the measured facing: the RAM read is live and reflects input.
- `goto` to a target 250 units along the measured stern lane: **arrived in
  10.4 s at dist 91.4 (tol 100)** after a full ~180° closed-loop spin;
  post-release residual drift 52 units over the settle window.
- Failure modes found live and fixed: (a) a ±2-unit displacement estimator
  latched on idle-bob noise and spun the character in place; (b) slide-along
  displacement while turning at a wall contaminated the axis estimate.
  Both fixed by the gated re-estimation above and a pure-turn band
  (|heading error| > 90° → forward stick 0).
- Walkable-space map of the fixture deck (per-direction sustained holds):
  bow ~170, stern ~300–530, port ~120, starboard ~460 world units; the deck
  is small and every existing fixture savestate is on this deck, so a
  multi-metre goto is not provable with current fixtures (tracked as a
  fixture gap, see Consequences).

## Tradeoffs

- One extra 1 s probe per goto step; bounded, and the probe displacement is
  informative, not wasted.
- `Actors()` is bounded and cheap, but actor IDs are numeric; mapping IDs to
  names is the npc-atlas worker's job (this ADR exposes the raw list).
- The axis estimate assumes the camera is not cutscene-locked; a frozen
  player (dialogue) yields zero probe displacement and the step seeds from
  facing, then fails on its own deadline with a typed message.

## Consequences

- The play harness and any RAM consumer must hold the Dolphin process (the
  arena vanishes on stop); `Mem1.Close()` releases the reader.
- `make test-dolphin` (or `GN_GATE=test-dolphin`) runs the live proof;
  elsewhere the steps report NOT RUN.
- Fixture gap: all current savestates are boat-deck scenes (~300–530 world
  units of walkable space). A few-metre goto needs an open-field savestate
  fixture; file an issue (no copyrighted fixture data in the repo per
  AGENTS.md rule 5).

## Reversal plan

Delete `ram.go`, `ram_unix.go`, `ram_bridge.go`, `ram_test.go`,
`ram_live_test.go` and the `goto`/`wait_state` branches in
`route.go`/`runner.go`/`script.go` (self-contained; grep-locatable). Routes
fall back to the ADR-0162 vocabulary. No data format changes.
