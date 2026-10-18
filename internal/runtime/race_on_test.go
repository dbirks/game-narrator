//go:build race

package runtime_test

// liveOCRTimeoutScale scales the live OCR fallback test's pre-check OCR
// budget when the test binary is race-instrumented: 5, the same scale the
// pipeline and ocr package's real-Tesseract gates use (their
// race_on_test.go documents the measurement: under a 2-core runner's race
// load the same crop takes several times longer, and an unscaled budget
// times out reads the engine would answer — a timeout is an inconclusive
// pre-check measurement, not a confirmed defeat).
const liveOCRTimeoutScale = 5
