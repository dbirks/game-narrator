//go:build unix

package ocr

import (
	"errors"
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
		return killSlot(cmd)
	}
	cmd.WaitDelay = KillWaitGrace
}

// superviseSlot is the pool's variant: the same process group and WaitDelay,
// but NO Cancel hook — the slot is created with exec.Command (no context:
// the slot outlives any single read, and Go refuses a Cancel on a
// non-CommandContext command), and the read's deadline killer kills the
// group directly through killSlot.
func superviseSlot(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = KillWaitGrace
}

// killSlot SIGKILLs the child's whole process group. The pool's slots are
// supervised the same way the per-process path is (ADR-0012 §2): a slot whose
// read hit its deadline must not survive the read, and its kill must not reach
// the pool's other slots (each slot is its own group). ESRCH means the group
// already exited, which is success for our purposes.
func killSlot(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
			return nil
		}
		return err
	}
	return nil
}
