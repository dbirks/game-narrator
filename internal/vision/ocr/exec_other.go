//go:build !unix

package ocr

import (
	"os/exec"
	"time"
)

// KillWaitGrace is how long the child gets to die before Wait gives up on its
// pipes.
const KillWaitGrace = 250 * time.Millisecond

// supervise is the non-POSIX fallback: no process groups, so cancellation kills
// only the direct child. The project's release target is Linux (docs/spec/10),
// and this path exists so the package still compiles for a developer on another
// OS — where it must not be described as satisfying ADR-0012 §2's group-kill
// requirement.
func supervise(cmd *exec.Cmd) { cmd.WaitDelay = KillWaitGrace }

// superviseSlot is the non-POSIX variant: no process groups and no Cancel
// hook (see the unix variant). The slot's deadline kill reaches only the
// direct child on a non-POSIX host — the same limitation the per-process
// path documents there (ADR-0012 §2).
func superviseSlot(cmd *exec.Cmd) { cmd.WaitDelay = KillWaitGrace }

// killSlot is the non-POSIX fallback: no process groups, so it kills only the
// direct child. The release target is Linux (docs/spec/10); on another OS the
// pool's slots keep their one-slot-per-group shape but cannot group-kill a
// wrapper script's children — the same limitation the per-process path
// documents (ADR-0012 §2).
func killSlot(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
