//go:build linux

package generic

import (
	"syscall"
	"time"
)

// cpuTimeNanos returns the process-wide CPU time (user + system, summed over
// every thread) from getrusage(2). It is the load-independent alternative to
// wall clock for the import-speed budget: on a shared host wall clock
// stretches with the load average, CPU time does not (issue #16).
//
// RUSAGE_SELF is the whole test process. The budget is a delta taken
// immediately before and after the Parse call, so work done by other tests in
// the same binary outside that window is excluded.
func cpuTimeNanos() (int64, error) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, err
	}
	ns := (int64(ru.Utime.Sec)+int64(ru.Stime.Sec))*int64(time.Second) +
		(int64(ru.Utime.Usec)+int64(ru.Stime.Usec))*int64(time.Microsecond)
	return ns, nil
}
