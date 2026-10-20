//go:build !linux

package generic

import "errors"

// cpuTimeNanos is the non-Linux stand-in for the getrusage-based helper in
// rusage_linux_test.go. The import-speed budget is enforced with CPU time on
// Linux (the dev and release targets); elsewhere the test reports the speed
// check as NOT RUN rather than guessing at a wall-clock bound that issue #16
// showed to be load-sensitive.
func cpuTimeNanos() (int64, error) {
	return 0, errors.New("cpu time (getrusage) unavailable on this platform")
}
