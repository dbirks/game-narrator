//go:build unix && !linux

package scenario

// processStartTicks on non-Linux unix (macOS dev hosts): the kernel does
// not expose the process start time the way /proc does, so 0 — the owner
// check falls back to the signal-0 probe, which is the same liveness rule
// the dolphin bridge uses there.
func processStartTicks(pid int) int64 { return 0 }
