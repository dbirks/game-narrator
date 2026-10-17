// subprocess.go implements ADR-0012 part 1 §2 for long-lived capture processes:
// cancellation reaches the whole process group, not just the direct child.
//
// internal/vision/ocr carries its own private copy of this for one-shot OCR
// calls. It is duplicated rather than shared only because that package is
// outside this task's boundary; unifying the two into one supervisor is a
// small, obvious follow-up (the two copies have the same contract and the same
// tests must hold for both).

package capture

import (
	"os/exec"
	"time"
)

// WaitGrace is how long Wait waits for the child's pipes to close after it has
// been killed. FFmpeg inherits nothing that should outlive it, but a wrapper
// script or a shell-spawned grandchild holding an fd is exactly the case
// WaitDelay exists for (ADR-0012's "orphaned process groups" risk).
const WaitGrace = 2 * time.Second

// Supervise puts cmd in its own process group and arranges for cancellation to
// SIGKILL that entire group. Call it before Start.
//
// Both mechanisms are needed: Setpgid means FFmpeg's own children land in the
// group so a group kill reaches them, and Cancel overrides exec's default
// "SIGKILL the direct child only". Killing just the direct child of a capture
// process is the classic orphan bug — the goroutine returns, the grandchild
// keeps the device open, and the next restart fails on EBUSY with a message
// that points nowhere near the cause.
func Supervise(cmd *exec.Cmd) { supervise(cmd) }

// KillGroup SIGKILLs the process group led by pid, for the stall watchdog: a
// stalled FFmpeg is still "running" as far as exec.CommandContext is
// concerned, because nothing has cancelled its context yet. A dead group is the
// goal, so the error is only worth logging.
func KillGroup(pid int) error { return killGroup(pid) }
