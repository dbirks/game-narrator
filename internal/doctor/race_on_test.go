//go:build race

package doctor

// checkTimeoutScale scales the fake host's check and model-call budgets when
// the test binary is race-instrumented (issue #22): the doctor report runs
// the real checks against the fake host, and the storage.database check is a
// real SQLite migration — a wall clock the race detector slows down. At
// host load 12-20 the unscaled 3 s/5 s budgets timed out checks the fake
// host answers, so the two named report tests failed only under load. The
// scaled budgets (15 s/25 s) stay small against the production defaults
// (15 s/60 s, doctor.go) so a wedged check still fails the test; they only
// buy the headroom a load-inflated migration needs.
const checkTimeoutScale = 5
