//go:build race

package ocr

// ocrTimeoutScale scales the real-Tesseract live test's OCR budgets when the
// test binary is race-instrumented. See the pipeline package's
// race_on_test.go for the measurement behind the 5×: a -race binary on a
// 2-core runner makes the same crop take several times longer (CI run
// 39090252213), so a comparison read that would answer inside the 4 s
// default can need five times that under runner load.
const ocrTimeoutScale = 5
