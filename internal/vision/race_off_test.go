//go:build !race

package vision

// ocrTimeoutScale is 1 in an uninstrumented build: the fixture gate runs its
// measured 15 s budget as is. See race_on_test.go for why the race build
// scales it.
const ocrTimeoutScale = 1
