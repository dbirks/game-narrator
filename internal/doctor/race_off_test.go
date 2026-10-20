//go:build !race

package doctor

// checkTimeoutScale is 1 in an uninstrumented build: the fake host's check
// and model-call budgets run at the values the tests were written around
// (3 s/5 s). See race_on_test.go for why the race build scales them
// (issue #22).
const checkTimeoutScale = 1
