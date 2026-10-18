//go:build race

package vision

// ocrTimeoutScale scales the real-Tesseract fixture gate's OCR budgets when
// the test binary is race-instrumented. See the pipeline package's
// race_on_test.go for the measurement behind the 5×: a -race binary on a
// 2-core runner makes the same crop take several times longer (CI run
// 39090252213), so the gate's fixed 15 s budget is 5× wider there.
const ocrTimeoutScale = 5
