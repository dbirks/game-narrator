//go:build unix

package ocr

import (
	"os/exec"
	"syscall"
	"time"
)

// KillWaitGrace is how long the child is given between SIGKILL-ing itself and
// the parent giving up on its pipes. It is short because the OCR result is
// worthless once the observation has been superseded (docs/spec/05).
const KillWaitGrace = 250 * time.Millisecond

// supervise puts the child in its own process group and arranges for
// cancellation to SIGKILL that whole group.
//
// Two mechanisms, both needed:
//
//   - Setpgid means the child's own children (a wrapper script, a shell a stub
//     binary spawns) land in the same group, so kill(-pgid) reaches them.
//     Killing only the direct child is the classic orphaned-`sleep` bug: the
//     goroutine returns, the process survives, and a frame loop eventually dies
//     of PID exhaustion rather than of slow OCR.
//   - cmd.Cancel overrides exec's default SIGKILL-of-the-child, and WaitDelay
//     guarantees Wait returns even if an inherited pipe fd keeps a copy goroutine
//     alive.
func supervise(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid addresses the group. AEPERM here means the child already
		// exited, which is success for our purposes; exec reports the error and
		// Wait still reaps it.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = KillWaitGrace
}
