//go:build unix

package dolphin

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// TestRealDolphinBridge is the real-hardware proof (docs/spec/12: Dolphin
// emulator mode — never labelled as unit coverage). It is skipped as NOT RUN
// with an actionable reason when any prerequisite is missing:
//
//   - GAME_NARRATOR_GAME_IMAGE_PATH names the user-supplied image (the image
//     is never committed or copied; only its SHA-256 is reported);
//   - dolphin-emu, Xvfb, ffmpeg and xwininfo are on PATH;
//   - no Xvfb or Dolphin process is already running (the spike's no-parallel
//     rule: two emulators on one machine fight over the render-window lookup).
//
// When it runs, it asserts the frame-level evidence the spec demands: a
// game frame (first_frame), a proven START press (input), a live frame
// pipeline with changing hashes (frames=), and no process leftovers
// (/proc scan).
func TestRealDolphinBridge(t *testing.T) {
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils): %v", bin, err)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	// Launch: isolated user dir, private display, the proven config. The
	// window-wait budget is raised for llvmpipe hosts (the frame itself is
	// WaitForGameFrame's job, not the window's).
	if err := b.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	// Synchronous teardown on any later failure: the ctx-cancel watcher tears
	// down asynchronously, and the next test's stray-process check would race
	// a graceful Dolphin exit that takes up to 15 s (the measured leak).
	defer b.Stop()
	st := b.State()
	// The image is proven by hash only; the path itself is never reported.
	t.Logf("launched: display=%s dolphin_pid=%d user_dir=%s image_sha256=%s image_size=%d",
		st.Display, st.DolphinPID, st.UserDir, st.ImageSHA, st.ImageSize)

	// Readiness 1: a real game frame (the predicate, not "the process exists").
	ev, err := b.WaitForGameFrame(ctx, 5*time.Minute)
	if err != nil {
		t.Fatalf("WaitForGameFrame (real): %v", err)
	}
	t.Logf("first_frame: hashes=%v distances=[vs_baseline=%.4f vs_prev=%.4f] at=%s",
		ev.Hashes, ev.Distances[0], ev.Distances[1], ev.At.Format(time.RFC3339))

	// Readiness 2: a START press proven by a scene change against a
	// no-input control window.
	pev, err := b.ProveInput(ctx)
	// Debug aid (env-gated): copy the evidence PNGs to a stable path so a
	// failing run can be inspected after the t.TempDir cleanup.
	if out := os.Getenv("GAME_NARRATOR_EVIDENCE_OUT"); out != "" {
		for label, src := range pev.Files {
			if data, rerr := os.ReadFile(src); rerr == nil {
				_ = os.WriteFile(filepath.Join(out, label+".png"), data, 0o644)
			}
		}
	}
	if err != nil {
		t.Fatalf("ProveInput (real): %v", err)
	}
	t.Logf("input: proven accepted; distances=[control_dispersion=%.4f input_max=%.4f] evidence=%v",
		pev.Distances[0], pev.Distances[1], pev.Files)

	// The live pipeline: ≥20 frames in 3 s with changing hashes (the
	// "the emulated GPU is producing frames, not a frozen window" proof).
	src, err := b.FrameSource(10)
	if err != nil {
		t.Fatalf("FrameSource: %v", err)
	}
	frames, changed := drainFrames(t, ctx, src, 3*time.Second)
	t.Logf("frames=%d changed_hashes=%d in 3s (id=%s)", frames, changed, src.ID())
	if frames < 20 {
		t.Fatalf("frame pipeline delivered %d frames in 3s, want >= 20 (llvmpipe budget)", frames)
	}
	if changed < 2 {
		t.Fatalf("only %d distinct frame hashes in 3s — the picture is frozen; not a live capture", changed)
	}

	// A stored snapshot (the dev tool's on-disk evidence).
	if _, err := b.SaveSnapshot("smoke"); err != nil {
		t.Fatalf("SaveSnapshot (real): %v", err)
	}

	// Stop: graceful, idempotent, and nothing left behind.
	b.Stop()
	if err := strayDolphinProcesses(); err != nil {
		t.Errorf("after Stop: %v", err)
	}
	if n, err := countProcEntries(st.Display); err != nil {
		t.Logf("/proc scan note: %v", err)
	} else if n != 0 {
		t.Errorf("%d /proc entries still reference display %s after Stop", n, st.Display)
	}

	// The emulator's own log must exist (the spike used it to read the
	// booted title) and be non-empty.
	logPath := filepath.Join(st.UserDir, "bridge-emu.log")
	if fi, err := os.Stat(logPath); err != nil || fi.Size() == 0 {
		t.Errorf("emulator log %s missing or empty (the booted-title proof)", logPath)
	}
}

// drainFrames pulls frames from the live source for at most dur and counts
// them and their distinct hashes. It does not require a strict
// frame-per-second quota (llvmpipe at 10 fps on this hardware is the
// calibrated budget); it requires the pipeline be alive and the picture
// moving. Caller-owned source: this helper closes it on the way out.
func drainFrames(t *testing.T, ctx context.Context, src capture.Source, dur time.Duration) (int, int) {
	t.Helper()
	ch, err := src.Start(ctx)
	if err != nil {
		t.Fatalf("source.Start: %v", err)
	}
	defer src.Close()
	deadline := time.Now().Add(dur)
	var count int
	seen := map[string]bool{}
	for time.Now().Before(deadline) {
		select {
		case f, ok := <-ch:
			if !ok {
				return count, len(seen)
			}
			count++
			seen[f.Hash] = true
		case <-ctx.Done():
			return count, len(seen)
		}
	}
	return count, len(seen)
}

// TestRealAbruptContextKill is the /proc half of the "every child dies with
// its parent" contract. It launches the real emulator, waits for the
// running state, then cancels the run's context abruptly — WITHOUT calling
// Stop — and asserts that no dolphin-emu or Xvfb process survives (checked
// against /proc, the same scan the stray-process guard uses). This is the
// regression test for the leaked background real test: a worker killed
// mid-run must not orphan its emulator. The OS-level half (parent process
// dying outright, covered by Pdeathsig on every spawned child) is asserted
// by the orchestrator's own worker-kill behaviour, not from inside the
// process that would die.
func TestRealAbruptContextKill(t *testing.T) {
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils): %v", bin, err)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	launchCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := b.Launch(launchCtx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	// Synchronous teardown if the test fails before the abrupt cancel (the
	// cancel itself triggers the watcher's async teardown, and the /proc wait
	// loop below is what proves it completed).
	defer b.Stop()
	st := b.State()
	t.Logf("launched for the abrupt-kill test: display=%s dolphin_pid=%d xvfb_pid=%d image_sha256=%s",
		st.Display, st.DolphinPID, st.XvfbPID, st.ImageSHA)

	// Wait for the running state (the render window is resolved; the game
	// frame itself is not needed for this test).
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		if b.State().State == StateRunning {
			break
		}
		if b.State().State == StateFailed {
			t.Fatalf("bridge failed before the abrupt cancel: %s", b.State().LastError)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if st := b.State(); st.State != StateRunning {
		t.Fatalf("bridge never reached running (state %s: %s)", st.State, st.LastError)
	}

	// The abrupt kill: the parent context is cancelled; Stop is NOT called.
	// The bridge's lifetime watcher must signal both process groups itself.
	t.Log("input: cancelling the run's context abruptly (no Stop call)")
	cancel()

	// Give the graceful path its budget (SIGTERM, then bounded SIGKILL),
	// then assert on /proc that nothing of this run survives.
	cleanDeadline := time.Now().Add(60 * time.Second)
	for {
		left, err := liveDolphinOrXvfbPIDs()
		if err == nil && len(left) == 0 {
			t.Log("leftovers: none — no dolphin-emu or Xvfb process after the abrupt cancel")
			break
		}
		if time.Now().After(cleanDeadline) {
			t.Fatalf("after the abrupt context cancel: %d dolphin-emu/Xvfb process(es) still alive (pids %v); the children must die with their parent", len(left), left)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if n, err := countProcEntries(st.Display); err == nil && n != 0 {
		t.Fatalf("%d /proc entries still reference display %s after the abrupt cancel", n, st.Display)
	}
	if b.State().State != StateStopped {
		t.Errorf("bridge state = %s after the abrupt cancel, want stopped", b.State().State)
	}
}

// procComm reads /proc/<pid>/comm — the kernel's name of the executable
// (truncated to 15 chars; "dolphin-emu" and "Xvfb" both fit) — with the
// argv[0] basename as fallback. These are EXECUTABLE terms, which is what
// the no-leftovers and no-parallel assertions need: a cmdline-substring
// match (the old shape) trips on any process whose command line merely
// mentions the name, such as a shell running `pgrep dolphin-emu` or the
// developer's own grep.
func procComm(pid string) string {
	b, err := os.ReadFile(filepath.Join("/proc", pid, "comm"))
	if err == nil {
		if c := strings.TrimSpace(string(b)); c != "" {
			return c
		}
	}
	cmdline, err := os.ReadFile(filepath.Join("/proc", pid, "cmdline"))
	if err != nil {
		return ""
	}
	fields := strings.Split(string(cmdline), "\x00")
	if len(fields) == 0 || fields[0] == "" {
		return ""
	}
	return filepath.Base(fields[0])
}

// isSupervisedProc reports whether pid is a live dolphin-emu or Xvfb
// process, in executable-basename terms.
func isSupervisedProc(pid string) bool {
	c := procComm(pid)
	return c == "dolphin-emu" || c == "Xvfb"
}

// liveDolphinOrXvfbPIDs scans /proc for any live dolphin-emu or Xvfb process
// and returns their pids (the no-leftovers assertion the spec demands, in
// process-table terms rather than `pgrep`).
func liveDolphinOrXvfbPIDs() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("cannot scan /proc (run on a host with /proc): %w", err)
	}
	var out []string
	for _, e := range entries {
		if !isPIDDir(e.Name()) {
			continue
		}
		if isSupervisedProc(e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// strayDolphinProcesses reports an error when another Xvfb or Dolphin
// process is already running (the no-parallel rule), via /proc.
func strayDolphinProcesses() error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("cannot scan /proc (run on a host with /proc): %w", err)
	}
	for _, e := range entries {
		if !isPIDDir(e.Name()) {
			continue
		}
		if isSupervisedProc(e.Name()) {
			return fmt.Errorf("another emulator/Xvfb process is already running (pid %s, comm %s); the spike's no-parallel rule forbids two bridges at once", e.Name(), procComm(e.Name()))
		}
	}
	return nil
}

// countProcEntries counts /proc entries whose environ or cmdline references
// the display — the "no leftover Xvfb" check in /proc terms.
func countProcEntries(display string) (int, error) {
	if display == "" {
		return 0, nil
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, e := range entries {
		if !isPIDDir(e.Name()) {
			continue
		}
		if cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline")); err == nil &&
			strings.Contains(string(cmdline), display) {
			n++
		}
	}
	return n, nil
}

func isPIDDir(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
