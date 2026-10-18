//go:build race

package hwsmoke

// ocrTimeoutScale scales TestHWOCRControlledImage's real-Tesseract budgets
// when the test binary is race-instrumented. See internal/pipeline's
// race_on_test.go for the measurement behind the 5×: CI run 39090252213 saw
// this very test time out at the engine's 4 s default ("engine timed out")
// on a 2-core -race runner, while the 15 s-budgeted fixture reads in the
// sweep passed. What the test proves is unchanged — the OCR stack on this
// host reads the controlled image within bound — only the budget moves with
// the instrumented build.
const ocrTimeoutScale = 5
