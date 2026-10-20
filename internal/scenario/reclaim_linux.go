//go:build linux

package scenario

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// processStartTicks returns the process's start time in clock ticks since
// boot, straight from the kernel's own field 22 of /proc/<pid>/stat
// (utime+stime before it: the field never wraps within a boot, unlike a
// wall-clock start time, and it is what makes the PID-reuse check in
// ownerProcessAlive sound). It returns 0 when the pid has no /proc entry
// (dead) or the entry cannot be parsed — 0 means "unknown", which
// ownerProcessAlive falls back to the signal-0 probe for.
func processStartTicks(pid int) int64 {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0
	}
	s := string(raw)
	// Field 2 (the comm) may contain spaces and parentheses; the numeric
	// fields start after the LAST ')'.
	i := strings.LastIndexByte(s, ')')
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	// After the comm: fields 3.. of the whole line. starttime is field 22
	// overall, i.e. index 19 of the post-comm slice (fields[0] is field 3,
	// the state).
	fields := strings.Fields(s[i+1:])
	if len(fields) <= 19 {
		return 0
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return ticks
}
