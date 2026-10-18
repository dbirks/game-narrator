//go:build race

package pipeline_test

// ocrTimeoutScale scales the real-Tesseract gate's OCR budgets when the test
// binary is race-instrumented.
//
// The Tesseract child is a native binary the race detector does not
// instrument, but the instrumented test process shares a 2-core runner with
// the rest of `go test ./...`, and under that load the same 550×110 crop
// takes several times longer than on an uninstrumented dev host. CI run
// 39090252213 measured the consequence of not scaling: on the same step the
// 400×100 controlled-image read timed out at the engine's 4 s default
// (TestHWOCRControlledImage), the 15 s-budgeted fixture reads passed, and
// TestSyntheticFrameReplayRealTesseract's unscaled 4 s budget timed out reads
// whose retry policy changed what run 1 and run 2 of the same clip read —
// the run-to-run determinism assertion the gate exists to make.
//
// The gate therefore runs its OCR budgets at 5× the production defaults under
// -race: wide enough for the measured slowness, bounded enough that a wedged
// engine still costs at most ReplayOCRMaxAttempts × the scaled per-call
// budget per distinct read, after which the frame degrades and the scenario's
// replay_not_degraded assertion fails the run.
const ocrTimeoutScale = 5
