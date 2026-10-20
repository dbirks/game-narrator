package dolphin

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The display allocator and the Stop-side artifact cleanup are pure file
// logic once the locations and the PID liveness check are injected — so
// these tests run against a temp dir with no real X server and no real
// /tmp locks. livePID is os.Getpid(): a process that is genuinely alive,
// so the "live PID is never touched" assertions would hold under the real
// procAlive seam as well.
func setDisplaySeams(t *testing.T, dir string, livePID int) {
	t.Helper()
	oldSocket, oldLock, oldAlive := xvfbSocketDir, xvfbLockDir, pidAlive
	xvfbSocketDir = filepath.Join(dir, "unix")
	xvfbLockDir = dir
	pidAlive = func(pid int) bool { return pid == livePID }
	if err := os.MkdirAll(xvfbSocketDir, 0o755); err != nil {
		t.Fatalf("mkdir fake socket dir: %v", err)
	}
	t.Cleanup(func() {
		xvfbSocketDir, xvfbLockDir, pidAlive = oldSocket, oldLock, oldAlive
	})
}

func writeLock(t *testing.T, dir string, n int, content string) {
	t.Helper()
	if err := os.WriteFile(fmt.Sprintf("%s/.X%d-lock", dir, n), []byte(content), 0o644); err != nil {
		t.Fatalf("write lock :%d: %v", n, err)
	}
}

func lockExists(t *testing.T, dir string, n int) bool {
	t.Helper()
	_, err := os.Stat(fmt.Sprintf("%s/.X%d-lock", dir, n))
	return err == nil
}

// Issue #7 case 1: a lock whose recorded owner is dead is stale — the
// allocator reclaims it (lock AND any leftover socket) and hands out the
// number, instead of burning the slot forever.
func TestFreeDisplayReclaimsStaleLock(t *testing.T) {
	live := os.Getpid()
	dir := t.TempDir()
	setDisplaySeams(t, dir, live)
	writeLock(t, dir, 91, "12345\n") // dead owner (the seam: not live)
	writeLock(t, dir, 92, fmt.Sprint(live)+"\n")

	d, err := freeDisplay()
	if err != nil {
		t.Fatalf("freeDisplay: %v", err)
	}
	if d != ":91" {
		t.Fatalf("display = %q, want :91 (the stale slot)", d)
	}
	if lockExists(t, dir, 91) {
		t.Error("stale lock .X91-lock was not reclaimed")
	}
	if _, err := os.Stat(xvfbSocketPath(91)); !os.IsNotExist(err) {
		t.Error("stale socket X91 was not reclaimed")
	}
	if !lockExists(t, dir, 92) {
		t.Error("lock with a live owner was removed")
	}
}

// Issue #7 case 2: a lock with a live PID and a lock with unparseable
// content are both treated as busy — the allocator skips them and never
// touches them. A bare socket with no lock is busy too.
func TestFreeDisplaySkipsLiveAndUnreadable(t *testing.T) {
	live := os.Getpid()
	dir := t.TempDir()
	setDisplaySeams(t, dir, live)
	writeLock(t, dir, 91, fmt.Sprint(live)+"\n")
	writeLock(t, dir, 92, "garbage that is not a pid\n")
	if err := os.WriteFile(xvfbSocketPath(93), nil, 0o644); err != nil {
		t.Fatalf("write socket X93: %v", err)
	}

	d, err := freeDisplay()
	if err != nil {
		t.Fatalf("freeDisplay: %v", err)
	}
	if d != ":94" {
		t.Fatalf("display = %q, want :94 (first slot that is truly free)", d)
	}
	for _, n := range []int{91, 92} {
		if !lockExists(t, dir, n) {
			t.Errorf("lock :%d (live/unreadable) was removed", n)
		}
	}
}

// Issue #7 case 3: when every slot in the range is genuinely taken the
// error says how — live owners versus unreadable locks — so an operator
// knows whether to wait or to investigate the lock files.
func TestFreeDisplayFullRange(t *testing.T) {
	live := os.Getpid()
	dir := t.TempDir()
	setDisplaySeams(t, dir, live)
	for n := displayLowNum; n <= displayHighNum; n++ {
		writeLock(t, dir, n, fmt.Sprint(live)+"\n")
	}
	_, err := freeDisplay()
	if err == nil {
		t.Fatal("freeDisplay: want an error with the range full, got none")
	}
	msg := err.Error()
	for _, want := range []string{":91-:110", "all 20 slots busy", "20 held by live owners", "0 with unreadable lock files"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

// The Stop-side cleanup: after the bridge stops its own Xvfb it removes
// the artifacts of the display it owned. A dead-owner lock and its
// leftover socket go; a lock whose owner is still alive never goes;
// displays outside :91-:110 are never touched.
func TestRemoveOwnedXvfbArtifacts(t *testing.T) {
	live := os.Getpid()
	dir := t.TempDir()
	setDisplaySeams(t, dir, live)

	// Dead owner: the lock and the socket are removed.
	writeLock(t, dir, 95, "4321\n")
	if err := os.WriteFile(xvfbSocketPath(95), nil, 0o644); err != nil {
		t.Fatalf("write socket X95: %v", err)
	}
	removeOwnedXvfbArtifacts(":95")
	if lockExists(t, dir, 95) {
		t.Error("dead-owner lock .X95-lock was not removed")
	}
	if _, err := os.Stat(xvfbSocketPath(95)); !os.IsNotExist(err) {
		t.Error("socket X95 was not removed")
	}

	// Live owner: the lock is NEVER removed.
	writeLock(t, dir, 96, fmt.Sprint(live)+"\n")
	removeOwnedXvfbArtifacts(":96")
	if !lockExists(t, dir, 96) {
		t.Error("lock with a live owner was removed")
	}

	// Outside the private range: untouched even though the owner is dead.
	writeLock(t, dir, 90, "4321\n")
	removeOwnedXvfbArtifacts(":90")
	if !lockExists(t, dir, 90) {
		t.Error("lock outside :91-:110 was removed")
	}
}

// The Stop-side funnel: an explicit Stop, a lifetime ctx cancel and a
// failed launch all end in bridge.Stop, which must remove the artifacts of
// the display it owns — but only after the Xvfb stop actually succeeded.
func TestStopRemovesArtifactsOfOwnedDisplay(t *testing.T) {
	dir := t.TempDir()
	setDisplaySeams(t, dir, os.Getpid())
	writeLock(t, dir, 97, "4321\n") // the killed Xvfb left its lock behind
	if err := os.WriteFile(xvfbSocketPath(97), nil, 0o644); err != nil {
		t.Fatalf("write socket X97: %v", err)
	}

	stopped := 0
	b := &bridge{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		st:        Status{Display: ":97"},
		stopXvfb:  func() error { stopped++; return nil },
		xvfbOwned: true,
		launched:  true,
	}
	b.Stop()
	if stopped != 1 {
		t.Fatalf("stopXvfb called %d times, want 1", stopped)
	}
	if lockExists(t, dir, 97) {
		t.Error("Stop left our own stale lock behind")
	}
	if _, err := os.Stat(xvfbSocketPath(97)); !os.IsNotExist(err) {
		t.Error("Stop left our own stale socket behind")
	}

	// A FAILED Xvfb stop must not burn the lock: the server may still be up
	// and the allocator must still see the slot as busy.
	writeLock(t, dir, 98, fmt.Sprint(os.Getpid())+"\n")
	b2 := &bridge{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		st:        Status{Display: ":98"},
		stopXvfb:  func() error { return fmt.Errorf("still alive") },
		xvfbOwned: true,
		launched:  true,
	}
	b2.Stop()
	if !lockExists(t, dir, 98) {
		t.Error("Stop removed a lock although the Xvfb stop failed")
	}
}
