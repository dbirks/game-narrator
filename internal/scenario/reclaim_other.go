//go:build !unix

package scenario

// Non-unix stubs: the harness's reclaim path is conservative on platforms
// without the signal-0 liveness probe — the owner is treated as alive, so a
// reclaim pass never removes a dir there. The dirs clean up on normal
// completion (the deferred cleanups) and on the unix release host; this is
// degradation, not a supported dev platform (ADR-0012).

func processStartTicks(pid int) int64 { return 0 }

// pidAlive: unknown. ownerProcessAlive must treat unknown as ALIVE (the
// conservative direction): a wrong "alive" keeps a stale dir, a wrong "dead"
// would remove one a live worker still owns.
func pidAlive(pid int) bool { return true }
