//go:build !race

package ocr

// ocrTimeoutScale is 1 in an uninstrumented build: the live test runs the
// engine's production default budget. See race_on_test.go for why the race
// build scales it.
const ocrTimeoutScale = 1
