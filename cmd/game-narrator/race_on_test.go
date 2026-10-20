//go:build race

package main

// ocrEngineBudgetScale scales the one-shot ocr command's real-Tesseract
// engine budgets when the test binary is race-instrumented.
//
// The Tesseract child is a native binary the race detector does not
// instrument, but the instrumented test process shares a 2-core runner with
// the rest of `go test ./...`, and under that load the same controlled-crop
// read takes several times longer than on an uninstrumented dev host. CI run
// 39090252213 measured the consequence of not scaling: on the same step the
// 400×100 controlled-image read timed out at the engine's 4 s default
// (TestHWOCRControlledImage), and this package's TestRunOCRGameRanksAgainst
// ThePublishedProfile (CI 39424864849) failed its one region read on the
// same wall — the one-shot command's read budget is ocrEngineBudget, so the
// race build scales it here, the way internal/pipeline and
// internal/vision/ocr scale theirs.
const ocrEngineBudgetScale = 5
