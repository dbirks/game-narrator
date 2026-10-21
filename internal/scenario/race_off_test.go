//go:build !race

package scenario

// budgetScale is 1 in an uninstrumented build: the harness's wall-clock
// budgets run at their production values there. See race_on_test.go for why
// the race build scales them (issue #22).
const budgetScale = 1
