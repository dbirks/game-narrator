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
