//go:build unix

package dolphin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// startGroupChild spawns cmd under exactly the supervision the bridge uses
// (supervisedAttr: own process group + Pdeathsig, spawned and reaped on one
// locked thread — the production shape, so a test that passes here exercises
// the real stop path) and returns the pid plus a stop built on
// stopProcessGroup — the exact body the Xvfb and Dolphin stop closures run.
//
// Two invariants are asserted at spawn time, because a silent violation of
// either turns a later Stop into a swallowed ESRCH and a leaked process the
// orchestrator only finds minutes later:
//   - the child IS its group leader (Getpgid(pid) == pid), so KillGroup(pid)
//     signals it and everything it spawned;
//   - a t.Cleanup SIGKILLs the group and the leader, so no test leaves a
//     supervised child behind even when it fails before calling stop.
func startGroupChild(t *testing.T, args ...string) (int, func(grace time.Duration) error) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = supervisedAttr()
	pid, done, err := spawnSupervised(cmd)
	if err != nil {
		t.Fatalf("start child %v: %v", args, err)
	}
	if pgid, gerr := syscall.Getpgid(pid); gerr != nil || pgid != pid {
		t.Fatalf("child %d is not its own group leader (pgid=%d, err=%v); the group kill would miss it", pid, pgid, gerr)
	}
	t.Cleanup(func() {
		// Hard-kill any child the test did not stop: the group first
		// (co-processes), then the leader directly, since a child that is
		// not a group leader cannot be reached through the group id.
		_ = capture.KillGroup(pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	})
	return pid, func(grace time.Duration) error { return stopProcessGroup(pid, grace, done) }
}

// procGone asserts /proc/<pid> no longer exists: the observable "the child
// is really gone" half of the Stop contract (a buffered reap with no
// /proc entry, not just a returned error value).
func procGone(t *testing.T, pid int) {
	t.Helper()
	_, err := os.Stat(fmt.Sprintf("/proc/%d", pid))
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("/proc/%d still exists after Stop (err=%v): the child was not reaped", pid, err)
	}
}

// TestStopProcessGroupChildExitsOnSIGTERM pins the regression where Stop
// hung forever on the NORMAL path: the child dies promptly on SIGTERM, the
// select consumes the one buffered reap, and the old trailing `<-done` then
// waited on a value that never comes. Stop must return quickly, and the
// child must be gone from /proc when it does.
func TestStopProcessGroupChildExitsOnSIGTERM(t *testing.T) {
	pid, stop := startGroupChild(t, "sleep", "30")
	start := time.Now()
	if err := stop(2 * time.Second); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("stop took %s for a SIGTERM-obedient child; want well under the grace", d)
	}
	procGone(t, pid)
}

// TestStopProcessGroupChildIgnoresSIGTERM covers the escalation path: the
// group leader traps SIGTERM and keeps running, so the grace must elapse,
// the group must be SIGKILLed, and Stop must still return — bounded, not
// wedged — and the child must be gone from /proc afterwards. The leader
// loops a short sleep rather than one `sleep 30`: the group SIGTERM kills
// each sleep co-process and a plain `trap; sleep 30` shell would exit with
// its script, defeating the point.
func TestStopProcessGroupChildIgnoresSIGTERM(t *testing.T) {
	pid, stop := startGroupChild(t, "sh", "-c", `trap "" TERM; while :; do sleep 0.2; done`)
	// Let the shell actually run `trap` before the group TERM arrives: sent
	// within microseconds of execve the signal would land before the trap is
	// installed, which is not the steady state Stop ever faces.
	time.Sleep(300 * time.Millisecond)
	grace := 2 * time.Second
	start := time.Now()
	if err := stop(grace); err != nil {
		t.Fatalf("stop: %v", err)
	}
	d := time.Since(start)
	if d < grace-500*time.Millisecond {
		t.Fatalf("stop returned after %s, before the %s grace: the escalation path was not exercised (the child died on SIGTERM)", d, grace)
	}
	if d > grace+reapTimeout+time.Second {
		t.Fatalf("stop took %s after the SIGKILL escalation; want grace plus the bounded reap", d)
	}
	procGone(t, pid)
}
