//go:build !race

package runtime_test

// ocrTimeoutScale is 1 in an uninstrumented build: the real-Tesseract
// sessions run the engine's production default budgets there (newEnv's
// default engine, the live OCR fallback test's pre-check). See
// race_on_test.go for why the race build scales them.
const ocrTimeoutScale = 1

// patienceScale is 1 in an uninstrumented build: the event-and-state patience
// ceilings run at the values the tests were written around. See
// race_on_test.go for why the race build scales them (issue #22).
const patienceScale = 1
