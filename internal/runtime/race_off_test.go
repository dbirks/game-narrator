//go:build !race

package runtime_test

// liveOCRTimeoutScale is 1 in an uninstrumented build: the live OCR fallback
// test's pre-check runs the engine's production default budget there. See
// race_on_test.go for why the race build scales it.
const liveOCRTimeoutScale = 1

// patienceScale is 1 in an uninstrumented build: the event-and-state patience
// ceilings run at the values the tests were written around. See
// race_on_test.go for why the race build scales them (issue #22).
const patienceScale = 1
