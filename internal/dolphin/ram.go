// RAM state reads from the running Dolphin (ADR-0173): the emulated GameCube
// MEM1 arena, opened READ-ONLY (no ptrace, no writes), big-endian decoded.
//
// How the other workers use this (npc-atlas, play routes):
//
//   provider, ok := b.(Mem1Provider) // b is a *dolphin bridge
//   mem, err := provider.Mem1()      // typed error → report NOT RUN
//   gs, err := mem.GameState()       // position / facing / stage / room
//   actors, err := mem.Actors()      // bounded actor list (id, x, y, z, room)
//   mem.Close()
//
// The route vocabulary (ADR-0162 routes / this package's Runner) adds two
// RAM-driven step kinds, both closed-loop against the stick the bridge
// already owns:
//
//	{"goto": {"x": …, "z": …, "tolerance": …, "timeout_ms": …}}
//	{"wait_state": {"x": …, "z": …, "tolerance": …, "timeout_ms": …}}
//
// Both read GameState() each iteration, steer MainX/MainY (Y +1 = forward,
// X +1 = right, the ADR-0162 measured convention) toward the target, and
// record the measured GameState in the step's trace details. When MEM1 is
// unreachable the steps fail with a typed error and the gate reports NOT
// RUN — they never fall back to a fake position.
//
// Address sources (public):
//
//	https://github.com/LagoLunatic/WW-Hacking-Docs/blob/master/RAM%20Map.txt
//	https://github.com/LagoLunatic/WW-Hacking-Docs/blob/master/Entity%20RAM%20Formats.txt
//
// Dolphin's Linux MEM1 mapping: MemArena mmaps the emulated arena; the
// region is located by the running process's /proc/<pid>/maps (and the
// Dolphin log's fastmem line) — see OpenMem1 and ADR-0173 for what was
// measured on this host.

package dolphin

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

// mem1Size is the GameCube MEM1 block (4 MiB, 0x80000000–0x803FFFFF). The
// address-table entries (player position, stage name, entity head, player
// entity pointer) all sit in MEM1.
const mem1Size = 0x400000

// mem1PtrMax bounds pointers into the emulated RAM that the arena snapshot
// can resolve: MEM1 plus 28 MiB of extended GameCube RAM. Measured on this
// host (ADR-0173): the player entity of the fixture savestate lives at
// 0x80AB86BC — ~11 MiB past the MEM1 block — in the extended RAM that
// Dolphin's fastmem arena carries contiguously after MEM1. The address
// table stays MEM1-only; entity pointers follow the game's own heap.
const mem1PtrMax = 0x80000000 + 0x2000000

// Wind Waker (GZSE01 / GZLE01) MEM1 addresses — RAM Map.txt, cited above.
const (
	// l_debug_keep_pos: "Player's true position (this gets copied to the
	// player entity's position every frame, and copied back too)".
	addrPlayerPosX = 0x803E440C
	addrPlayerPosY = 0x803E4410
	addrPlayerPosZ = 0x803E4414

	// dComIfG_play_c: "Always has a pointer to the player entity in RAM".
	// 803CA410 (dAttention_c) is the same pointer; either works.
	addrPlayerEntity = 0x803CA74C

	// "The current stage name" — a 9-char C string, 8 bytes + NUL.
	addrStageName = 0x803C9D3C

	// g_fopAcTg_Queue: the actor (entity) list head and count.
	addrEntityHead = 0x8037202C
	addrEntityCnt  = 0x80372030
)

// fopAc_ac_c entity offsets — Entity RAM Formats.txt, cited above.
const (
	entActorID  = 0x008 // 2 bytes: the actor ID (key of the DynamicNameTable)
	entTypeWord = 0x0C0 // 4 bytes: 0x09130005 for a live entity
	entCurX     = 0x1F8 // 4 floats: current X / Y / Z position
	entCurRotY  = 0x206 // 2 bytes: Y rotation; 0x4000 units = 90 degrees
	entCurRoom  = 0x20A // 1 byte: current room number
)

// Typed errors: each is a distinct operator condition so callers can report
// NOT RUN instead of guessing.
var (
	// ErrMem1NoProcess: no Dolphin process (pid <= 0 or dead).
	ErrMem1NoProcess = errors.New("dolphin: mem1: no dolphin process")
	// ErrMem1NotFound: the MEM1 mapping is not visible in the process maps.
	ErrMem1NotFound = errors.New("dolphin: mem1: MEM1 mapping not found in /proc maps")
	// ErrMem1Access: the mapping exists but cannot be read (permissions,
	// e.g. YAMA ptrace_scope without CAP_SYS_PTRACE on /proc/<pid>/mem).
	ErrMem1Access = errors.New("dolphin: mem1: mapping not readable (permission denied)")
	// ErrMem1Unrecognized: readable, but the game state markers are not
	// where the address table says — wrong game, wrong region, or a state
	// that has not booted far enough.
	ErrMem1Unrecognized = errors.New("dolphin: mem1: unrecognized game state markers")
)

// be16/be32/beF32 decode big-endian words from the MEM1 arena. b indexes the
// arena slice (0 == 0x80000000).
func be16(b []byte, off int) uint16 {
	if off < 0 || off+2 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint16(b[off : off+2])
}

func be32(b []byte, off int) uint32 {
	if off < 0 || off+4 > len(b) {
		return 0
	}
	return binary.BigEndian.Uint32(b[off : off+4])
}

func beF32(b []byte, off int) float64 {
	return float64(math.Float32frombits(be32(b, off)))
}

// mem1Off converts an absolute emulated-RAM address to the arena slice
// offset (0 == 0x80000000). Accepts the full snapshot range (MEM1 plus
// the extended RAM; see mem1PtrMax).
func mem1Off(addr uint32) (int, bool) {
	if addr < 0x80000000 || addr >= mem1PtrMax {
		return 0, false
	}
	return int(addr - 0x80000000), true
}

// mem1PtrOK reports whether p is a plausible pointer into the emulated RAM
// covered by the snapshot: MEM1 (the actor list nodes) plus the extended
// RAM (the game heap, where entity objects live — measured 0x80AB86BC for
// the fixture savestate). Anything past the snapshot bounds means a
// misidentified region or a dead pointer, so the tight check is
// deliberate.
func mem1PtrOK(p uint32) bool {
	return p >= 0x80000000 && p < mem1PtrMax
}

// cstr reads a NUL-terminated string of at most n bytes.
func cstr(b []byte, off, n int) string {
	end := off + n
	if end > len(b) {
		end = len(b)
	}
	for i := off; i < end; i++ {
		if b[i] == 0 {
			return string(b[off:i])
		}
	}
	return string(b[off:end])
}

// GameState is the measured emulated state: the player's position, facing,
// and the stage/room names the address table yields.
type GameState struct {
	X, Y, Z   float64
	FacingDeg float64 // yaw, degrees; 0 = the direction entity rotation 0 is
	Stage     string
	Room      int
	PlayerPtr uint32 // the player entity pointer, as read (0 = not yet valid)
	At        time.Time
}

// Actor is one entry of the emulated actor list.
type Actor struct {
	ID      uint16
	X, Y, Z float64
	Room    int
}

// Mem1 is a read-only handle on the emulated MEM1 arena of a running
// Dolphin. Every read pulls a fresh bounded 4 MiB snapshot of the arena
// from the emulator process, so position/facing readings are live, not
// stale. Close only releases the source file handles.
type Mem1 struct {
	pid    int
	read   func() ([]byte, error) // one fresh arena snapshot; index 0 == 0x80000000
	closer func()
}

// NewMem1FromBytes builds a Mem1 over a caller-owned arena copy (unit
// tests only — production opens via OpenMem1; the "fresh" read returns
// the same bytes each time).
func NewMem1FromBytes(data []byte) *Mem1 {
	snap := data
	return &Mem1{read: func() ([]byte, error) { return snap, nil }}
}

// Close releases the source (file handle). Bounded: never errors out loud.
func (m *Mem1) Close() error {
	if m != nil && m.closer != nil {
		m.closer()
	}
	return nil
}

// valid reports whether the arena carries the live-game markers: the player
// entity pointer at addrPlayerEntity pointing at an entity whose type word
// is the live-entity value. A booted, in-game state passes; a menu/boot
// state or a misidentified region does not.
func valid(data []byte) bool {
	off, ok := mem1Off(addrPlayerEntity)
	if !ok {
		return false
	}
	p := be32(data, off)
	if !mem1PtrOK(p) {
		return false
	}
	eoff, ok := mem1Off(p)
	if !ok {
		return false
	}
	return be32(data, eoff+entTypeWord) == 0x09130005
}

// snapshot reads one fresh bounded arena and verifies it; a dead game or a
// misidentified region is the typed ErrMem1Unrecognized.
func (m *Mem1) snapshot() ([]byte, error) {
	data, err := m.read()
	if err != nil {
		return nil, err
	}
	if !valid(data) {
		return nil, fmt.Errorf("dolphin: mem1: %w", ErrMem1Unrecognized)
	}
	return data, nil
}

// GameState reads the player position, facing, stage and room.
// It returns ErrMem1Unrecognized when the markers are not live.
func (m *Mem1) GameState() (GameState, error) {
	var gs GameState
	data, err := m.snapshot()
	if err != nil {
		return gs, err
	}
	gs.X = beF32(data, mem1OffMust(addrPlayerPosX))
	gs.Y = beF32(data, mem1OffMust(addrPlayerPosY))
	gs.Z = beF32(data, mem1OffMust(addrPlayerPosZ))
	gs.PlayerPtr = be32(data, mem1OffMust(addrPlayerEntity))
	eoff, _ := mem1Off(gs.PlayerPtr)
	gs.FacingDeg = float64(int16(be16(data, eoff+entCurRotY))) * 90.0 / 0x4000
	gs.Room = int(be16(data, eoff+entCurRoom)) & 0xFF
	gs.Stage = cstr(data, mem1OffMust(addrStageName), 8)
	gs.At = time.Now()
	return gs, nil
}

// maxActors bounds the actor walk (the queue's count is untrusted; the
// walk stops at the cap or at the first non-MEM1 pointer).
const maxActors = 256

// Actors walks the actor list (g_fopAcTg_Queue): node {actor_p, next_p}.
// Bounded by maxActors and by the first invalid pointer.
func (m *Mem1) Actors() ([]Actor, error) {
	data, err := m.snapshot()
	if err != nil {
		return nil, err
	}
	var out []Actor
	head := be32(data, mem1OffMust(addrEntityHead))
	node := head
	for i := 0; i < maxActors; i++ {
		if node == 0 || !mem1PtrOK(node) {
			break
		}
		noff, _ := mem1Off(node)
		// Node layout: {actor_p @ 0, next_p @ 4} — f_op_actor_mng's queue
		// node; verified against the live emulator in ADR-0173.
		aper := be32(data, noff)
		next := be32(data, noff+4)
		if aper != 0 && mem1PtrOK(aper) {
			aoff, _ := mem1Off(aper)
			a := Actor{
				ID:   be16(data, aoff+entActorID),
				X:    beF32(data, aoff+entCurX),
				Y:    beF32(data, aoff+entCurX+4),
				Z:    beF32(data, aoff+entCurX+8),
				Room: int(be16(data, aoff+entCurRoom)) & 0xFF,
			}
			out = append(out, a)
		}
		if next == head {
			break // the list wrapped: no forward progress
		}
		node = next
	}
	return out, nil
}

func mem1OffMust(addr uint32) int {
	off, ok := mem1Off(addr)
	if !ok {
		panic("dolphin: mem1: bad address table entry")
	}
	return off
}

// --- Route step types (ADR-0173) ------------------------------------------

// GotoTarget is the {"goto": …} route step: steer the stick toward (x, z)
// in world coordinates until within tolerance, closed-loop on RAM state.
type GotoTarget struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
	// Tolerance in world units (m); 0 = defaultGotoTolerance.
	Tolerance float64 `json:"tolerance"`
	// TimeoutMS bounds the step; 0 = defaultGotoTimeout, capped at
	// maxGotoTimeout.
	TimeoutMS int64 `json:"timeout_ms"`
}

// StateTarget is the {"wait_state": …} route step: wait until the player's
// RAM position is within tolerance of (x, z) — e.g. after an event that
// moves the player, or before asserting proximity to an NPC.
type StateTarget struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
	// Tolerance in world units; 0 = defaultGotoTolerance.
	Tolerance float64 `json:"tolerance"`
	// TimeoutMS; 0 = defaultGotoTimeout, capped at maxGotoTimeout.
	TimeoutMS int64 `json:"timeout_ms"`
}

const (
	// World units: the deck fixture's player stands at y=1650 for a deck
	// roughly a metre or two above the water, consistent with mm — but
	// the unit scale is not confirmed in the public docs, so the ADR
	// treats positions as opaque world units (tolerances in the same
	// units; ADR-0173).
	defaultGotoTolerance = 300.0
	defaultGotoTimeout   = 60 * time.Second
	maxGotoTimeout       = 180 * time.Second
)

// clampStick bounds a stick axis to [-1..1] (NaN-safe; the route's own
// clampStickAxis is NaN-unsafe and used where inputs are validated).
func ramClampStick(v float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	return clampStickAxis(v)
}

// normAngle folds d into (-180, 180].
func normAngle(d float64) float64 {
	for d <= -180 {
		d += 360
	}
	for d > 180 {
		d -= 360
	}
	return d
}

// Mem1Provider is the subset of the live bridge the RAM step kinds need.
// The concrete bridge (*bridge) implements it; test fakes do not, so the
// steps fail with a typed, NOT RUN-reportable message instead of panicking.
type Mem1Provider interface {
	// Mem1 opens (once, cached) the read-only MEM1 mapping for the running
	// Dolphin. Any error is a typed ErrMem1*.
	Mem1() (*Mem1, error)
}

// gotoSteer maps the measured state to one stick hold toward the target.
// forward = stick MainY (+1 = forward), turn = stick MainX (+1 = right):
// the ADR-0162 measured convention the approach step already uses.
// fwd is the estimated world heading of the stick's forward axis (the
// camera direction, not necessarily the character's facing); tol is the
// step's effective tolerance (world units) the creep band derives from.
func gotoSteer(gs GameState, tx, tz, tol, fwd float64) (turn, forward float64) {
	dx, dz := tx-gs.X, tz-gs.Z
	dist := math.Hypot(dx, dz)
	desired := math.Atan2(dx, dz) * 180.0 / math.Pi // heading whose forward points at the target
	err := normAngle(desired - fwd)
	if math.Abs(err) < 15 {
		turn = 0
	} else {
		turn = ramClampStick(err / 45.0) // full stick at a 45°+ heading error
	}
	forward = 1.0
	if math.Abs(err) > 90 {
		forward = 0.0 // pure turn — walking while spinning pushes into walls and
		// contaminates the axis estimate (ADR-0173 live run)
	} else if math.Abs(err) > 45 {
		forward = 0.35 // mostly turning in place
	} else if dist < tol*2 {
		forward = 0.35 // arriving: creep in so the tolerance is hit, not overshot
	}
	return turn, forward
}

// runGotoStep runs one closed-loop goto: read state → steer → hold → repeat
// until within tolerance or the deadline. Every iteration's measured state
// lands in the step trace (the route timeline).
//
// Steering is relative to the MEASURED stick-forward axis, not the
// entity's facing: the stick is camera-relative in Wind Waker, and the
// character's rotY (FacingDeg) drifts off the camera while walking
// (measured on this host: a pure forward hold rotated the facing -180° →
// -156.6°). The loop starts with a one-time stick probe — a 1 s pure-
// forward hold whose displacement calibrates the in-world stick axis and
// the walking speed — then re-estimates the axis from each subsequent
// displacement. Two measured failure modes drive the thresholds
// (ADR-0173, live7/live8 runs): idle-bob noise is ±2 world units per read (an
// estimator that trusted it spun the character in place), and the
// character can walk away from the target for a while before turning, so
// the axis estimate is only updated from displacements well above the
// noise floor.
func runGotoStep(ctx context.Context, b Bridge, mem *Mem1, t GotoTarget, st *StepTrace, deadline time.Time) (bool, string) {
	tol := t.Tolerance
	if tol <= 0 {
		tol = defaultGotoTolerance
	}
	const hold = 1000 * time.Millisecond
	// forwardDeg: the world heading the stick's +Y axis actually points at,
	// calibrated by the startup probe, re-estimated from real displacements.
	forwardDeg := math.NaN()
	probeSpeed := 0.0 // measured world units per 1 s forward hold
	probed := false
	for {
		gs, err := mem.GameState()
		if err != nil {
			// A deferred arena (OpenMem1 found the mapping before the
			// emulated markers were live) reads as not-yet-booted:
			// keep polling on the step's own deadline, bounded.
			if errors.Is(err, ErrMem1Unrecognized) {
				if time.Now().After(deadline) {
					return false, "goto: " + err.Error()
				}
				if err := sleepCtx(ctx, hold); err != nil {
					return false, "goto: " + err.Error()
				}
				continue
			}
			return false, "goto: " + err.Error()
		}
		dist := math.Hypot(t.X-gs.X, t.Z-gs.Z)
		st.Details = append(st.Details, fmt.Sprintf(
			"goto: pos=(%.2f,%.2f,%.2f) facing=%.1f fwd≈%.1f stage=%s room=%d dist=%.2f",
			gs.X, gs.Y, gs.Z, gs.FacingDeg, orNaN(forwardDeg, gs.FacingDeg), gs.Stage, gs.Room, dist))
		if dist <= tol {
			return true, fmt.Sprintf("goto: arrived at (%.2f, %.2f) dist=%.2f tol=%.2f", t.X, t.Z, dist, tol)
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("goto: deadline exceeded, dist=%.2f (pos=%.2f,%.2f target=%.2f,%.2f)", dist, gs.X, gs.Z, t.X, t.Z)
		}
		if !probed {
			// One-time stick probe: hold forward, measure the displacement →
			// the in-world stick-forward axis and the walk speed.
			if err := b.Inject(ctx, Buttons{}, Axes{MainY: 1}, hold); err != nil {
				return false, "goto: probe inject: " + err.Error()
			}
			probed = true
			if gs2, err := mem.GameState(); err == nil {
				dx, dz := gs2.X-gs.X, gs2.Z-gs.Z
				if d := math.Hypot(dx, dz); d > probeMinMove {
					forwardDeg = math.Atan2(dx, dz) * 180.0 / math.Pi
					probeSpeed = d / hold.Seconds()
				}
			}
			continue
		}
		fwd := orNaN(forwardDeg, gs.FacingDeg) // probe failed (stuck?); seed: facing
		turn, forward := gotoSteer(gs, t.X, t.Z, tol, fwd)
		if err := b.Inject(ctx, Buttons{}, Axes{MainX: turn, MainY: forward}, hold); err != nil {
			return false, "goto: inject: " + err.Error()
		}
		// Re-estimate the stick's forward axis from the displacement this
		// hold produced — only when the hold was a near-aligned walk
		// (|turn| < 0.5 ≈ heading error < 22°) and the displacement itself
		// agrees with the estimate. Two measured failure modes drive this
		// (ADR-0173 live runs): idle-bob noise of ±2 world units per read latched
		// the estimate at the wrong heading (a 1.5-unit jitter spun the
		// character in place), and while turning at a wall the
		// slide-along displacement is not the stick axis, so the estimate
		// must be frozen until the heading error is small again.
		if gs2, err := mem.GameState(); err == nil {
			dx, dz := gs2.X-gs.X, gs2.Z-gs.Z
			if math.Abs(turn) < 0.5 {
				dirDeg := math.Atan2(dx, dz) * 180.0 / math.Pi
				if d := math.Hypot(dx, dz); d > axisUpdateMin(probeSpeed) &&
					math.Abs(normAngle(dirDeg-fwd)) < 45 {
					forwardDeg = dirDeg
				}
			}
		}
	}
}

// probeMinMove: a displacement at least this large (world units) after
// a 1 s forward hold is real walking, not idle bobbing. Measured: the
// fixture's walk speed is ~80 units/s (166 units over a 2 s hold) while the
// position readout jitters ±2 world units.
const probeMinMove = 20.0

// axisUpdateMin: update the stick-axis estimate only when the displacement
// since the last read is well above the noise floor and a plausible
// fraction of the calibrated speed (a partial turn hold legitimately
// produces less displacement than pure forward).
func axisUpdateMin(probeSpeed float64) float64 {
	if probeSpeed > 0 {
		if m := 0.25 * probeSpeed; m > 30.0 {
			return m
		}
	}
	return 30.0
}

func orNaN(v, def float64) float64 {
	if math.IsNaN(v) {
		return def
	}
	return v
}

// runWaitStateStep waits (no stick) until the player's RAM position is
// within tolerance of (x, z), or the deadline.
func runWaitStateStep(ctx context.Context, mem *Mem1, t StateTarget, st *StepTrace, deadline time.Time) (bool, string) {
	tol := t.Tolerance
	if tol <= 0 {
		tol = defaultGotoTolerance
	}
	const poll = 500 * time.Millisecond
	for {
		gs, err := mem.GameState()
		if err != nil {
			// A deferred arena reads as not-yet-booted: keep polling on
			// the step's own deadline, bounded.
			if errors.Is(err, ErrMem1Unrecognized) {
				if time.Now().After(deadline) {
					return false, "wait_state: " + err.Error()
				}
				if err := sleepCtx(ctx, poll); err != nil {
					return false, "wait_state: " + err.Error()
				}
				continue
			}
			return false, "wait_state: " + err.Error()
		}
		dist := math.Hypot(t.X-gs.X, t.Z-gs.Z)
		st.Details = append(st.Details, fmt.Sprintf(
			"wait_state: pos=(%.2f,%.2f,%.2f) stage=%s room=%d dist=%.2f",
			gs.X, gs.Y, gs.Z, gs.Stage, gs.Room, dist))
		if dist <= tol {
			return true, fmt.Sprintf("wait_state: within %.2f of (%.2f, %.2f)", tol, t.X, t.Z)
		}
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("wait_state: deadline exceeded, dist=%.2f", dist)
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return false, "wait_state: " + err.Error()
		}
	}
}
