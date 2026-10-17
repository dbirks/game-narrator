//go:build unix

package capture

import (
	"os/exec"
	"syscall"
)

// supervise puts the child in its own process group and makes cancellation kill
// the whole group (ADR-0012 part 1 §2).
func supervise(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid addresses the group; EPERM/ESRCH mean it already went
		// away, which is what we wanted anyway.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = WaitGrace
}

// killGroup signals an existing process group from a goroutine that does not
// own the *exec.Cmd (the stall watchdog). The caller must hold the source's
// lock; the returned error is only interesting for logging, because a dead
// group is the state we are trying to create.
func killGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	return syscall.Kill(-pid, syscall.SIGKILL)
}
