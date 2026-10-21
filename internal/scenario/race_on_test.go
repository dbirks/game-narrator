//go:build race

package scenario

import "time"

// budgetScale scales the harness's wall-clock budgets when the test binary
// is race-instrumented (issue #22): the race detector slows every goroutine
// handoff in the instrumented process, and the gate runs under a saturated
// host (parallel gates and workers, load 12-20), so the same scenario work
// takes several times longer on the wall clock than on an uninstrumented
// dev host. The scenarios themselves are correctness tests — their clocks
// are the clip's capture clock — so the budgets that keep them honest are
// scaled, not the scenarios: the suite's shared context bound
// (scenarios_test.go) and the per-provider-call bound (providerTimeout) get
// the 5x the pipeline and ocr packages' real-Tesseract gates use (their
// race_on_test.go files document the measurement behind the same factor).
const budgetScale = 5

func init() {
	// providerTimeout is a var in run.go (a package file, which cannot see
	// this test-file const); the race build widens the per-provider-call
	// bound here: 10 s x budgetScale.
	providerTimeout = 10 * time.Second * budgetScale
}
