//go:build !race

package pipeline_test

// ocrTimeoutScale is 1 in an uninstrumented build: the real-Tesseract gate
// runs the production default budgets there. See race_on_test.go for why the
// race build scales them.
const ocrTimeoutScale = 1
