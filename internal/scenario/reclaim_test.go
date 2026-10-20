package scenario

// reclaim_test.go is the regression gate for issue #14: a SIGKILLed harness
// run (go test -timeout panic, worker timeout, OOM, EDQUOT) never reaches
// the deferred cleanup, so its per-run temp dir survives under the system
// temp dir — 233 MB of the fixture's frames each, in the October incident.
// The gate here is the reclaim rule itself, with fake dirs (no Dolphin, no
// fixture, no emulator): a dead owner's dir is reclaimed, a LIVE owner's
// dir (this test's own PID) is kept, an unmarked dir older than the age
// floor is reclaimed, and a dir outside the harness's own prefixes is
// never touched.

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// deadPid starts and waits a trivial process and returns its PID: a
// guaranteed-dead owner for the marker (the test process itself is alive,
// so its PID is the guaranteed-LIVE owner).
func deadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true") // POSIX; present on the dev host (Arch) and the release host (Debian)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dead-owner process: %v", err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("wait dead-owner process: %v", err)
	}
	return cmd.Process.Pid
}

// writeDir makes dir with one content file of n bytes and returns its path.
func writeDir(t *testing.T, base, name string, n int) string {
	t.Helper()
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make %s: %v", name, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "content.bin"), make([]byte, n), 0o644); err != nil {
		t.Fatalf("content in %s: %v", name, err)
	}
	return dir
}

// writeMarker drops a .gn-owner marker naming pid (with this test process's
// start ticks when pid is this process, else 0 = unknown, the honest value
// for a pid whose start time the test did not capture).
func writeMarker(t *testing.T, dir string, pid int) {
	t.Helper()
	var ticks int64
	if pid == os.Getpid() {
		ticks = processStartTicks(pid)
	}
	marker := "{\"pid\": " + strconv.Itoa(pid) + ", \"start_ticks\": " + strconv.FormatInt(ticks, 10) + ", \"written_at\": \"2026-01-01T00:00:00Z\"}\n"
	if err := os.WriteFile(filepath.Join(dir, ".gn-owner"), []byte(marker), 0o644); err != nil {
		t.Fatalf("marker in %s: %v", dir, err)
	}
}

func TestReclaimStaleTempDirs(t *testing.T) {
	base := t.TempDir()
	now := time.Now()
	dead := deadPid(t)
	const contentBytes = 1024

	// 1. The incident shape: a killed run's replay dir, marker names a dead
	// PID → reclaimed, and the bytes are accounted in the report.
	deadDir := writeDir(t, base, "gn-scenario-dolphin-killed-run", contentBytes)
	writeMarker(t, deadDir, dead)

	// 2. The safety rule: a dir whose owner is THIS process (alive) is kept,
	// whatever the age. This is the rule that keeps reclaim from eating a
	// concurrent live worker's in-flight dir.
	liveDir := writeDir(t, base, "gn-scenario-dolphin-live-run", contentBytes)
	writeMarker(t, liveDir, os.Getpid())

	// 3. Pre-marker build: no marker, newest mtime 48 h old (past the 24 h
	// floor) → reclaimed.
	oldDir := writeDir(t, base, "gn-scenario-audio-unmarked-old", contentBytes)
	past := now.Add(-48 * time.Hour)
	for _, p := range []string{oldDir, filepath.Join(oldDir, "content.bin")} {
		if err := os.Chtimes(p, past, past); err != nil {
			t.Fatalf("age %s: %v", p, err)
		}
	}

	// 4. Pre-marker build, still fresh: no marker but newest mtime now →
	// too recent to be sure of; a run may still be filling it. Kept.
	freshDir := writeDir(t, base, "gn-scenario-audio-unmarked-fresh", contentBytes)

	// 5. Foreign names in the shared temp dir: never touched, even with a
	// dead-owner marker and an old mtime.
	foreignDir := writeDir(t, base, "gn-other-harness-dir", contentBytes)
	writeMarker(t, foreignDir, dead)
	_ = os.Chtimes(foreignDir, past, past)
	foreignFile := filepath.Join(base, "gn-scenario-dolphin-a-file")
	if err := os.WriteFile(foreignFile, []byte("a file, not a dir"), 0o644); err != nil {
		t.Fatalf("foreign file: %v", err)
	}

	rep := ReclaimStaleTempDirs(base, now, 24*time.Hour)

	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Errorf("dead-owner dir %s still exists, want reclaimed (stat err=%v)", deadDir, err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("live-owner dir %s was removed, want kept (err=%v)", liveDir, err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("unmarked old dir %s still exists, want reclaimed (stat err=%v)", oldDir, err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("unmarked fresh dir %s was removed, want kept (too recent) (err=%v)", freshDir, err)
	}
	if _, err := os.Stat(foreignDir); err != nil {
		t.Errorf("foreign dir %s was touched, want untouched (err=%v)", foreignDir, err)
	}
	if _, err := os.Stat(foreignFile); err != nil {
		t.Errorf("foreign file %s was touched, want untouched (err=%v)", foreignFile, err)
	}

	var removed = make(map[string]int64)
	for _, d := range rep.Removed {
		removed[d.Name] = d.Bytes
	}
	if _, ok := removed["gn-scenario-dolphin-killed-run"]; !ok {
		t.Errorf("report did not list the dead-owner dir as removed: %v", rep.Removed)
	}
	if _, ok := removed["gn-scenario-audio-unmarked-old"]; !ok {
		t.Errorf("report did not list the unmarked old dir as removed: %v", rep.Removed)
	}
	for _, kept := range []string{"gn-scenario-dolphin-live-run", "gn-scenario-audio-unmarked-fresh"} {
		if _, ok := removed[kept]; ok {
			t.Errorf("report lists %s as removed, want kept", kept)
		}
	}
	// Both reclaimed dirs held contentBytes of content plus the marker
	// (only the dead dir had a marker); account at least the content.
	if rep.Bytes < 2*contentBytes {
		t.Errorf("report bytes %d, want at least %d (two dirs of %d B)", rep.Bytes, 2*contentBytes, contentBytes)
	}
	if rep.String() == "" {
		t.Error("report String() empty, want the gate log line")
	}
}

// TestReclaimMarkerRoundTrip proves the marker the harness writes carries a
// live owner, so a reclaim pass over a dir this process just materialized
// keeps it — the marker is not merely decorative.
func TestReclaimMarkerRoundTrip(t *testing.T) {
	base := t.TempDir()
	dir := writeDir(t, base, "gn-scenario-dolphin-fresh", 16)
	writeTempOwnerMarker(dir)

	rep := ReclaimStaleTempDirs(base, time.Now(), 24*time.Hour)
	for _, d := range rep.Removed {
		if d.Name == "gn-scenario-dolphin-fresh" {
			t.Fatalf("fresh dir with this process's own marker was reclaimed: %v", rep.Removed)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("fresh dir with live marker removed (err=%v)", err)
	}
}

// TestProcessStartTicksSelfOnLinux pins the /proc/<pid>/stat field-22 parse
// on the platform where it loads: the test's own start ticks must be
// positive and stable across two reads (the marker-vs-reclaim comparison
// relies on both).
func TestProcessStartTicksSelfOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("start-ticks probe is a /proc detail; linux only")
	}
	a := processStartTicks(os.Getpid())
	b := processStartTicks(os.Getpid())
	if a <= 0 || a != b {
		t.Fatalf("own start ticks: first %d, second %d, want equal and > 0", a, b)
	}
	if got := processStartTicks(-1); got != 0 {
		t.Fatalf("processStartTicks(-1) = %d, want 0 (unknown)", got)
	}
}
