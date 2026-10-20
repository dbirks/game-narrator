package dolphin

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Display selection follows the spike's proven range: :91 through :110. A
// display number is free when neither the X socket nor the lock file is
// present — and a lock whose recorded owner PID is dead counts as absent
// (issue #7: Xvfb reclaims its own stale lock on the next start, so an
// allocator that does not is stricter than the server it launches, and
// every SIGKILL/OOM/test-timeout permanently burned one of the 20 slots).
// Checking the socket matters for the opposite direction: reusing a number
// whose socket survives would make two bridges share one display — the
// isolation this whole package exists to guarantee.
const (
	displayLowNum   = 91
	displayHighNum  = 110
	xvfbDefaultGeom = "1280x1024x24"

	xvfbSocketDirDef = "/tmp/.X11-unix"
	xvfbLockDirDef   = "/tmp"
)

// Seams. Production uses the fixed X11 locations above; unit tests point
// these at a temp dir and a fake PID table so the allocation, reclamation
// and cleanup logic is testable without a real X server. pidAlive is the
// liveness probe (signal 0 on unix; the !unix stub answers false, which is
// safe here because it only ever disables reclamation, never the
// "lock present" guard).
var (
	xvfbSocketDir = xvfbSocketDirDef
	xvfbLockDir   = xvfbLockDirDef
	pidAlive      = alive
)

func xvfbSocketPath(n int) string { return xvfbSocketDir + "/X" + strconv.Itoa(n) }
func xvfbLockPath(n int) string   { return fmt.Sprintf("%s/.X%d-lock", xvfbLockDir, n) }

// freeDisplay returns the next free private display (":91" form).
func freeDisplay() (string, error) {
	var live, unreadable int
	for n := displayLowNum; n <= displayHighNum; n++ {
		if _, err := os.Stat(xvfbSocketPath(n)); err == nil {
			live++
			continue // socket present: a live server owns it
		}
		lock := xvfbLockPath(n)
		if _, err := os.Stat(lock); err != nil {
			return ":" + strconv.Itoa(n), nil // neither socket nor lock: free
		}
		owner := lockPID(lock)
		if owner <= 0 {
			unreadable++
			continue // unparseable owner: the safe default is BUSY
		}
		if pidAlive(owner) {
			live++
			continue // live owner: genuinely taken
		}
		// Stale lock: the owner is gone (SIGKILL, OOM, a test that
		// timed out). Reclaim it and hand out the number.
		if reclaimStaleLock(n) {
			return ":" + strconv.Itoa(n), nil
		}
		live++ // removal failed (permissions?): count the slot as busy
	}
	return "", fmt.Errorf("dolphin: no free X display in :%d-:%d: all %d slots busy (%d held by live owners, %d with unreadable lock files)",
		displayLowNum, displayHighNum, displayHighNum-displayLowNum+1, live, unreadable)
}

// lockPID reads the owner PID from an X11 lock file. X servers write the
// PID as plain text, optionally with a trailing newline; anything else —
// empty, garbage, out of range — is reported as -1 and the caller treats
// the slot as busy (never steal a lock whose owner cannot be read).
func lockPID(path string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return -1
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 || pid > 1<<30 {
		return -1
	}
	return pid
}

// reclaimStaleLock removes the lock (and the leftover socket, if any) of a
// display whose recorded owner is dead. It is the single place the "never
// delete a lock whose PID is alive" rule lives: a parseable live PID
// aborts the removal, and so does a lock that vanished under us. Only ever
// called on displays inside the private range.
func reclaimStaleLock(n int) bool {
	lock := xvfbLockPath(n)
	if pid := lockPID(lock); pid > 0 && pidAlive(pid) {
		return false
	}
	if err := os.Remove(lock); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false
	}
	_ = os.Remove(xvfbSocketPath(n)) // the socket of a killed server may linger
	return true
}

// removeOwnedXvfbArtifacts is the Stop-side cleanup for a display this
// bridge started and has just stopped: Xvfb removes its own lock on a
// clean exit, but the SIGKILL escalation and an abrupt parent death leave
// it behind. Best-effort — the re-Launch's freeDisplay reclaims whatever
// this misses, and the liveness check inside reclaimStaleLock keeps it
// from ever deleting a lock a live process owns.
func removeOwnedXvfbArtifacts(display string) {
	n := displayNum(display)
	if n < displayLowNum || n > displayHighNum {
		return // never touch displays outside the private range
	}
	// A live PID still recorded (or a failed removal) aborts inside
	// reclaimStaleLock; a re-Launch's freeDisplay then reports the slot as
	// busy rather than stealing it.
	reclaimStaleLock(n)
}

// displayIsUp reports whether the socket for the display exists (the
// readiness check the spike polled with `test -S`).
func displayIsUp(display string) bool {
	n := displayNum(display)
	if n <= 0 {
		return false
	}
	_, err := os.Stat(xvfbSocketPath(n))
	return err == nil
}

func displayNum(display string) int {
	// ":91" or ":91.0"
	s := display
	if len(s) > 0 && s[0] == ':' {
		s = s[1:]
	}
	if i := indexDot(s); i > 0 {
		s = s[:i]
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1
	}
	return n
}

func indexDot(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			return i
		}
	}
	return -1
}
