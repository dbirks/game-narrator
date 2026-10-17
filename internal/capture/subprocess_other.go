//go:build !unix

package capture

import (
	"os"
	"os/exec"
)

// supervise is the non-POSIX fallback: no process groups, so cancellation kills
// only the direct child. The release target is Linux (docs/spec/10) and this
// path exists so the package compiles for a developer on another OS — where it
// must not be described as satisfying ADR-0012 §2's group-kill requirement.
func supervise(cmd *exec.Cmd) { cmd.WaitDelay = WaitGrace }

// killGroup falls back to killing the single process.
func killGroup(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
