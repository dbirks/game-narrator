//go:build unix

package dolphin

import (
	"errors"
	"math"
	"os"
	"syscall"
	"testing"
)

// putBE16/putBE32/putBEF32 write words into a synthetic arena at an
// absolute MEM1 address (invented test values only — no game data).
func putBE16(arena []byte, addr uint32, v uint16) {
	off := int(addr - 0x80000000)
	arena[off] = byte(v >> 8)
	arena[off+1] = byte(v)
}

func putBE32(arena []byte, addr uint32, v uint32) {
	off := int(addr - 0x80000000)
	arena[off] = byte(v >> 24)
	arena[off+1] = byte(v >> 16)
	arena[off+2] = byte(v >> 8)
	arena[off+3] = byte(v)
}

func putBEF32(arena []byte, addr uint32, bits uint32) { putBE32(arena, addr, bits) }

func f32Bits(f float64) uint32 { return uint32(math.Float32bits(float32(f))) }

// synthArena builds a synthetic MEM1 arena with a live player entity, a
// stage name, and a two-actor list — all invented values.
func synthArena() []byte {
	arena := make([]byte, mem1Size)
	// Player entity at 0x80300000.
	const eptr = 0x80300000
	putBE32(arena, eptr+entTypeWord, 0x09130005)
	putBE16(arena, eptr+entActorID, 0x0102)
	putBEF32(arena, eptr+entCurX, f32Bits(10.5))
	putBEF32(arena, eptr+entCurX+4, f32Bits(-2.25))
	putBEF32(arena, eptr+entCurX+8, f32Bits(300.0))
	putBE16(arena, eptr+entCurRotY, 0x2000) // 45 degrees (0x4000 = 90)
	putBE16(arena, eptr+entCurRoom, 0x044)
	putBE32(arena, addrPlayerEntity, eptr)
	// l_debug_keep_pos.
	putBEF32(arena, addrPlayerPosX, f32Bits(10.5))
	putBEF32(arena, addrPlayerPosY, f32Bits(-2.25))
	putBEF32(arena, addrPlayerPosZ, f32Bits(300.0))
	// Stage name "sea" at addrStageName.
	copy(arena[int(addrStageName-0x80000000):], "sea\x00\x00\x00\x00\x00")
	// Actor list: two nodes.
	const (
		n1 = 0x80310000
		n2 = 0x80310008
		a2 = 0x80320000
	)
	putBE32(arena, addrEntityHead, n1)
	putBE32(arena, addrEntityCnt, 2)
	putBE32(arena, n1, eptr)
	putBE32(arena, n1+4, n2)
	putBE32(arena, n2, a2)
	putBE32(arena, n2+4, n1) // cycle back: the walk must stop, not loop
	putBE32(arena, a2+entTypeWord, 0x09130005)
	putBE16(arena, a2+entActorID, 0x0201)
	putBEF32(arena, a2+entCurX, f32Bits(42.0))
	putBEF32(arena, a2+entCurX+4, f32Bits(7.0))
	putBEF32(arena, a2+entCurX+8, f32Bits(123.0))
	putBE16(arena, a2+entCurRoom, 0x044)
	return arena
}

func TestBE16BE32BEF32(t *testing.T) {
	arena := make([]byte, mem1Size)
	putBE16(arena, 0x80000002, 0xABCD)
	putBE32(arena, 0x80000100, 0x12345678)
	putBE32(arena, 0x80000200, f32Bits(-1.5))
	if got := be16(arena, 2); got != 0xABCD {
		t.Fatalf("be16 = %#x, want 0xABCD", got)
	}
	if got := be32(arena, 0x100); got != 0x12345678 {
		t.Fatalf("be32 = %#x, want 0x12345678", got)
	}
	if got := beF32(arena, 0x200); got != -1.5 {
		t.Fatalf("beF32 = %v, want -1.5", got)
	}
	// Out-of-bounds reads are zero, never panics.
	if got := be32(arena, len(arena)-2); got != 0 {
		t.Fatalf("oob be32 = %#x", got)
	}
	if got := be16(arena, -1); got != 0 {
		t.Fatalf("oob be16 = %#x", got)
	}
}

func TestMem1GameState(t *testing.T) {
	m := NewMem1FromBytes(synthArena())
	gs, err := m.GameState()
	if err != nil {
		t.Fatalf("GameState: %v", err)
	}
	if gs.X != 10.5 || gs.Y != -2.25 || gs.Z != 300.0 {
		t.Fatalf("position = (%v %v %v), want (10.5 -2.25 300)", gs.X, gs.Y, gs.Z)
	}
	if math.Abs(gs.FacingDeg-45) > 1e-9 {
		t.Fatalf("facing = %v, want 45", gs.FacingDeg)
	}
	if gs.Stage != "sea" {
		t.Fatalf("stage = %q, want sea", gs.Stage)
	}
	if gs.Room != 0x44 {
		t.Fatalf("room = %#x, want 0x44", gs.Room)
	}
	if gs.PlayerPtr != 0x80300000 {
		t.Fatalf("player ptr = %#x", gs.PlayerPtr)
	}
}

func TestMem1Actors(t *testing.T) {
	m := NewMem1FromBytes(synthArena())
	actors, err := m.Actors()
	if err != nil {
		t.Fatalf("Actors: %v", err)
	}
	if len(actors) != 2 {
		t.Fatalf("len(actors) = %d, want 2 (cyclic list must terminate)", len(actors))
	}
	if actors[0].ID != 0x0102 || actors[0].X != 10.5 || actors[0].Room != 0x44 {
		t.Fatalf("actors[0] = %+v", actors[0])
	}
	if actors[1].ID != 0x0201 || actors[1].X != 42.0 || actors[1].Z != 123.0 {
		t.Fatalf("actors[1] = %+v", actors[1])
	}
}

func TestMem1NotLive(t *testing.T) {
	arena := make([]byte, mem1Size) // booted-nothing: no entity marker
	m := NewMem1FromBytes(arena)
	if _, err := m.GameState(); !errors.Is(err, ErrMem1Unrecognized) {
		t.Fatalf("expected ErrMem1Unrecognized, got %v", err)
	}
	if _, err := m.Actors(); !errors.Is(err, ErrMem1Unrecognized) {
		t.Fatalf("expected ErrMem1Unrecognized, got %v", err)
	}
}

func TestGotoSteer(t *testing.T) {
	gs := GameState{X: 0, Z: 0, FacingDeg: 0} // facing +Z
	// Target straight ahead (far enough to miss the creep band): no turn,
	// full forward.
	turn, fwd := gotoSteer(gs, 0, 1000, 300, 0)
	if turn != 0 || fwd != 1.0 {
		t.Fatalf("steer ahead = (%v %v), want (0 1)", turn, fwd)
	}
	// Target 90° right: full right turn, mostly turning in place.
	turn, fwd = gotoSteer(gs, 1000, 0, 300, 0)
	if turn != 1 || fwd != 0.35 {
		t.Fatalf("steer right = (%v %v), want (1 0.35)", turn, fwd)
	}
	// Target 120° right: full turn, no forward at all (pure spin).
	turn, fwd = gotoSteer(gs, 866, -500, 300, 0)
	if turn != 1 || fwd != 0.0 {
		t.Fatalf("steer 120° = (%v %v), want (1 0)", turn, fwd)
	}
	// Target behind: |err| = 180 → turn clamped to ±1, pure spin.
	turn, fwd = gotoSteer(gs, 0, -1000, 300, 0)
	if turn != 1.0 && turn != -1.0 {
		t.Fatalf("steer behind turn = %v, want ±1", turn)
	}
	if fwd != 0.0 {
		t.Fatalf("steer behind fwd = %v, want 0", fwd)
	}
	// Near target: creep.
	turn, fwd = gotoSteer(gs, 0, 0.5, 300, 0)
	if turn != 0 || fwd != 0.35 {
		t.Fatalf("steer near = (%v %v), want (0 0.35)", turn, fwd)
	}
	// NaN-safe clamp.
	if got := ramClampStick(math.NaN()); got != 0 {
		t.Fatalf("clamp NaN = %v", got)
	}
	if got := ramClampStick(3); got != 1 {
		t.Fatalf("clamp 3 = %v", got)
	}
}

func TestNormAngle(t *testing.T) {
	for in, want := range map[float64]float64{
		200: -160, -200: 160, 180: 180, 540: 180, -540: 180, 90: 90,
	} {
		if got := normAngle(in); got != want {
			t.Fatalf("normAngle(%v) = %v, want %v", in, got, want)
		}
	}
}

// TestMapsLineAndRank locks the /proc maps parsing and the candidate
// ordering used by OpenMem1 (ADR-0173): the fastmem arena mapping at
// file offset 0 must sort before other arena mappings and before
// ordinary shared-library / anonymous mappings.
func TestMapsLineAndRank(t *testing.T) {
	line := "7f3ca5600000-7f3ca7600000 rw-s 00000000 00:1a 589 /dev/shm/dolphin-emu.123 (deleted)"
	r, ok := parseMapsLine(line)
	if !ok {
		t.Fatalf("parseMapsLine: no region for %q", line)
	}
	if r.start != 0x7f3ca5600000 || r.size != 0x2000000 || r.foff != 0 {
		t.Fatalf("region = %+v, want start=0x7f3ca5600000 size=0x2000000 foff=0", r)
	}
	if r.path != "/dev/shm/dolphin-emu.123 (deleted)" {
		t.Fatalf("path = %q", r.path)
	}
	if got := r.rangeString(); got != "[0x7f3ca5600000-0x7f3ca7600000)" {
		t.Fatalf("rangeString = %q", got)
	}

	anon := mapsRegion{start: 0x1000, size: mem1Size}
	otherLib := mapsRegion{start: 0x2000, size: mem1Size, path: "/usr/lib/libQt6Core.so.6.12.0"}
	arenaNonZero := mapsRegion{start: 0x3000, size: mem1Size, foff: 0x2040000, path: "/dev/shm/dolphin-emu.123 (deleted)"}
	arena0 := mapsRegion{start: 0x4000, size: mem1Size, path: "/dev/shm/dolphin-emu.123 (deleted)"}
	if !(arenaRank(arena0) < arenaRank(arenaNonZero) &&
		arenaRank(arenaNonZero) < arenaRank(otherLib) &&
		arenaRank(otherLib) < arenaRank(anon)) {
		t.Fatalf("rank order wrong: mem1=%d nonZero=%d lib=%d anon=%d",
			arenaRank(arena0), arenaRank(arenaNonZero), arenaRank(otherLib), arenaRank(anon))
	}
}

// TestReadAtLoopHole locks the measured /proc/<pid>/mem behavior
// (ADR-0173): the read stops at the first unmapped hole — ReadAt returns
// the mapped prefix plus EIO, a read starting inside a hole returns
// 0 bytes with EIO and cannot be advanced — so a short read always means
// the arena tail is unreadable, never zeros to read past.
func TestReadAtLoopHole(t *testing.T) {
	const page = 4 << 10
	addr, _, err := syscall.Syscall6(syscall.SYS_MMAP, 0, 4*page,
		uintptr(syscall.PROT_READ|syscall.PROT_WRITE),
		uintptr(syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS), 0, 0)
	if err != 0 {
		t.Skipf("mmap: %v", err)
	}
	defer syscall.Syscall(syscall.SYS_MUNMAP, addr, 4*page, 0)
	syscall.Syscall(syscall.SYS_MUNMAP, addr+2*page, page, 0) // guard hole at page 2

	f, oerr := os.OpenFile("/proc/self/mem", os.O_RDONLY, 0)
	if oerr != nil {
		t.Fatalf("open /proc/self/mem: %v", oerr)
	}
	defer f.Close()

	// Fully mapped prefix: a request inside it returns everything, err nil.
	n, rerr := readAtLoop(f, make([]byte, page), int64(addr))
	if n != page || rerr != nil {
		t.Fatalf("mapped read: n=%d err=%v, want n=%d err=nil", n, rerr, page)
	}
	// Request that runs into the hole: the mapped prefix plus EIO.
	buf := make([]byte, 4*page)
	n, rerr = readAtLoop(f, buf, int64(addr))
	if n != 2*page || rerr == nil {
		t.Fatalf("hole read: n=%d err=%v, want n=%d err=EIO-ish (the prefix stops at the hole)", n, rerr, 2*page)
	}
	// Request that starts inside the hole: no bytes, cannot advance.
	n, rerr = readAtLoop(f, buf, int64(addr)+2*page)
	if n != 0 || rerr == nil {
		t.Fatalf("in-hole read: n=%d err=%v, want n=0 err!=nil", n, rerr)
	}
}
