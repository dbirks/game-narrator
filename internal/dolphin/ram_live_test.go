//go:build unix

package dolphin

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRAMLive is the class-(d) proof for ADR-0173: on the running Dolphin
// (fixture savestate) it shows (1) the MEM1 mapping is located and readable
// read-only, (2) GameState reflects the emulated state, (3) a stick hold
// moves the measured position in the measured facing direction, and (4) a
// closed-loop goto reaches a point a few metres away. Skipped as NOT RUN
// when any prerequisite is missing (the image, the binaries, the fixture
// state, or another emulator on the host).
func TestRAMLive(t *testing.T) {
	skipUnlessTestDolphinGate(t)
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set (the user-supplied game image path)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH", bin)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}
	state := filepath.Join(fixtureRootForTest(t), "states", "windwaker-deck-aryll.state")
	if _, err := os.Stat(state); err != nil {
		t.Skipf("NOT RUN: fixture savestate %s is missing: %v", "states/windwaker-deck-aryll.state", err)
	}

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	defer b.Stop()
	if err := b.Launch(ctx, LaunchSpec{SaveState: state}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	st := b.State()
	t.Logf("dolphin: pid=%d display=%s", st.DolphinPID, st.Display)

	// ADR evidence: which mapping the MEM1 region resolved to.
	if raw, rerr := os.ReadFile(fmt.Sprintf("/proc/%d/maps", st.DolphinPID)); rerr == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			f := strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			sp := strings.SplitN(f[0], "-", 2)
			if len(sp) != 2 {
				continue
			}
			var lo, hi uint64
			if _, err := fmt.Sscanf(sp[0], "%x", &lo); err != nil {
				continue
			}
			if _, err := fmt.Sscanf(sp[1], "%x", &hi); err != nil {
				continue
			}
			if hi-lo >= 0x400000 {
				t.Logf("maps: %s", line)
			}
		}
	}

	if _, err := b.WaitForGameFrame(ctx, 5*time.Minute); err != nil {
		t.Fatalf("WaitForGameFrame: %v", err)
	}
	provider, ok := b.(Mem1Provider)
	if !ok {
		t.Fatalf("bridge does not implement Mem1Provider")
	}
	mem, err := provider.Mem1()
	if err != nil {
		t.Fatalf("Mem1: %v", err)
	}
	defer mem.Close()

	// The savestate's emulated markers are not live right after the
	// first game frame (measured 10–20 s after it, run to run): wait
	// for the first valid read, bounded.
	var gs1 GameState
	var lastProbe string
	gsDeadline := time.Now().Add(90 * time.Second)
	for {
		gs1, err = mem.GameState()
		if err == nil {
			break
		}
		if time.Now().After(gsDeadline) {
			t.Fatalf("GameState(1): %v (probe: %s)", err, lastProbe)
		}
		t.Logf("mem1: markers not live yet; probe: %s; waiting", lastProbe)
		time.Sleep(2 * time.Second)
		lastProbe = probeArena(t, st.DolphinPID)
	}
	t.Logf("mem1: live after settle (first valid read at %s)", gs1.At.Format(time.RFC3339))
	t.Logf("gs1: x=%.3f y=%.3f z=%.3f facing=%.1f stage=%s room=%d ptr=%#x",
		gs1.X, gs1.Y, gs1.Z, gs1.FacingDeg, gs1.Stage, gs1.Room, gs1.PlayerPtr)

	// Walk forward on the stick, then read again.
	if err := b.Inject(ctx, Buttons{}, Axes{MainY: 1}, 2*time.Second); err != nil {
		t.Fatalf("Inject forward: %v", err)
	}
	time.Sleep(1 * time.Second)
	gs2, err := mem.GameState()
	if err != nil {
		t.Fatalf("GameState(2): %v", err)
	}
	dx, dy, dz := gs2.X-gs1.X, gs2.Y-gs1.Y, gs2.Z-gs1.Z
	dist := math.Hypot(dx, dz)
	t.Logf("gs2: x=%.3f y=%.3f z=%.3f facing=%.1f | delta=(%.3f, %.3f, %.3f) dist=%.3f",
		gs2.X, gs2.Y, gs2.Z, gs2.FacingDeg, dx, dy, dz, dist)
	if dist < 0.1 {
		t.Fatalf("walk: position did not change (dist=%.3f) — the RAM read is not live", dist)
	}
	// The move must be in the measured facing direction (forward = stick Y+).
	fwd := (dx*math.Sin(gs1.FacingDeg*math.Pi/180) + dz*math.Cos(gs1.FacingDeg*math.Pi/180)) / dist
	t.Logf("walk: forward-alignment=%.3f (cos of angle between delta and facing)", fwd)
	if fwd <= 0.25 {
		t.Fatalf("walk: delta is not in the facing direction (alignment %.2f)", fwd)
	}

	// Actors sanity: the live list must be non-empty and bounded.
	actors, err := mem.Actors()
	if err != nil {
		t.Fatalf("Actors: %v", err)
	}
	t.Logf("actors: %d entries; first few: %v", len(actors), actors[:minLen(len(actors), 4)])

	// Closed-loop goto: a target 250 world units along the STICK-BACK walk
	// direction — the deck fixture's one measured open lane (stern walk;
	// the bow walk into the facing direction stops after ~170 units, the
	// stern lane after ~300–530 depending on the entry point; ADR-0173
	// live runs). A short back-probe hold measures that lane's direction
	// before the target is computed.
	const gotoDist, gotoTol = 250.0, 100.0 // world units
	if err := b.Inject(ctx, Buttons{}, Axes{MainY: -1}, time.Second); err != nil {
		t.Fatalf("Inject back-probe: %v", err)
	}
	gsB1, err := mem.GameState()
	if err != nil {
		t.Fatalf("GameState(back0): %v", err)
	}
	if err := b.Inject(ctx, Buttons{}, Axes{MainY: -1}, time.Second); err != nil {
		t.Fatalf("Inject back-probe: %v", err)
	}
	gsB2, err := mem.GameState()
	if err != nil {
		t.Fatalf("GameState(back1): %v", err)
	}
	bdx, bdz := gsB2.X-gsB1.X, gsB2.Z-gsB1.Z
	if math.Hypot(bdx, bdz) < 10 {
		t.Fatalf("back-probe: no displacement — cannot derive the goto lane")
	}
	laneDir := math.Atan2(bdx, bdz) * 180.0 / math.Pi
	t.Logf("back-probe: delta=(%.1f,%.1f) lane dir=%.1f (from x=%.1f z=%.1f)",
		bdx, bdz, laneDir, gsB2.X, gsB2.Z)
	tx := gsB2.X + gotoDist*math.Sin(laneDir*math.Pi/180)
	tz := gsB2.Z + gotoDist*math.Cos(laneDir*math.Pi/180)
	tr, err := RunScript(ctx, b, Script{Steps: []Step{{
		Name:    "goto-test",
		Goto:    &GotoTarget{X: tx, Z: tz, Tolerance: gotoTol, TimeoutMS: 90000},
		Timeout: 90 * time.Second,
	}}})
	if err != nil {
		for _, d := range tr.Steps[0].Details {
			t.Log("  " + d)
		}
		t.Fatalf("goto: %v", err)
	}
	t.Logf("goto trace tail: %v", tr.Steps[0].Details[len(tr.Steps[0].Details)-3:])
	// The character keeps sliding for a short while after the stick
	// releases (measured residual drift: 60.7 → 112.6 units, i.e. ~52
	// units over the settle window; live11 run). Let it settle, then allow
	// the measured drift in the final bound.
	time.Sleep(2 * time.Second)
	gs3, err := mem.GameState()
	if err != nil {
		t.Fatalf("GameState(3): %v", err)
	}
	final := math.Hypot(tx-gs3.X, tz-gs3.Z)
	const settleDrift = 60.0 // world units, measured live11
	t.Logf("goto: ok=%v finished_in=%s final=(%.3f, %.3f) final_dist=%.3f (tol=%.0f + drift=%.0f)",
		tr.OK, tr.Finished.Sub(tr.Started), gs3.X, gs3.Z, final, gotoTol, settleDrift)
	if final > gotoTol+settleDrift {
		t.Fatalf("goto: final dist %.3f exceeds tolerance+drift %.0f", final, gotoTol+settleDrift)
	}
}

// probeArena reads the arena markers straight from /proc/<pid>/mem at the
// file-offset-0 /dev/shm/dolphin-emu VMA — the same bytes OpenMem1's
// deferred reader uses — and reports them for a not-live poll.
func probeArena(t *testing.T, pid int) string {
	f, err := os.Open(fmt.Sprintf("/proc/%d/maps", pid))
	if err != nil {
		return "maps: " + err.Error()
	}
	defer f.Close()
	lines, _ := io.ReadAll(f)
	for _, l := range strings.Split(string(lines), "\n") {
		r, ok := parseMapsLine(l)
		if !ok || r.size < mem1Size || !isArenaMapping(&r) {
			continue
		}
		mem, err := os.OpenFile(fmt.Sprintf("/proc/%d/mem", pid), os.O_RDONLY, 0)
		if err != nil {
			return "mem: " + err.Error()
		}
		defer mem.Close()
		buf := make([]byte, mem1Size)
		if _, err := mem.Seek(int64(r.start), 0); err != nil {
			return "seek: " + err.Error()
		}
		if _, err := mem.Read(buf); err != nil {
			return "read: " + err.Error()
		}
		ep := be32(buf, 0x3CA74C)
		typ := uint32(0)
		if ep >= 0x80000000 && int64(ep-0x80000000)+4 < int64(len(buf)) {
			eo := int(ep - 0x80000000)
			typ = be32(buf, eo+0xC0)
			var words []string
			for off := 0; off <= 0x20; off += 0x10 {
				words = append(words, fmt.Sprintf("+%03x=%08x", off, be32(buf, eo+off)))
			}
			t.Logf("probe entity words: %v", strings.Join(words, " "))
			t.Logf("probe pos: %v %v %v", beF32(buf, eo+0x1F8), beF32(buf, eo+0x1FC), beF32(buf, eo+0x200))
		}
		return fmt.Sprintf("vma=0x%x entityPtr=%08x type=%08x stage=%q",
			r.start, ep, typ, cstr(buf, 0x3C9D3C, 8))
	}
	return "no arena mapping"
}

func minLen(a, b int) int {
	if a < b {
		return a
	}
	return b
}
