//go:build unix

package scenario

import "syscall"

// pidAlive probes a pid with signal 0 (the same liveness rule the dolphin
// bridge uses, internal/dolphin procAlive): EPERM means the pid exists but
// belongs to someone else, which for our purposes is "alive" — never
// reclaim a dir whose owner we cannot signal.
func pidAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
