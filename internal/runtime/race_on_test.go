//go:build race

package runtime_test

// ocrTimeoutScale scales the real-Tesseract engine budgets when the test
// binary is race-instrumented: 5, the same scale the pipeline, ocr and
// vision packages' real-Tesseract gates use (their race_on_test.go files
// document the measurement: under a 2-core runner's race load the same crop
// takes several times longer, and an unscaled budget times out reads the
// engine would answer — a timeout is an inconclusive measurement, not a
// confirmed defeat).
//
// It applies to the session's default engine in newEnv (helpers_test.go)
// and to the live OCR fallback test's pre-check engine (ocr_live_test.go).
// The engine's own per-call deadline is the bound that fires in a
// ModeReplay session: it is tighter than the pipeline's 30 s
// ReplayOCRTimeout, so the replay retry policy's fresh per-attempt budget
// cannot outrun it — a race host on which one crop outlives the engine's
// unscaled 4 s default times out every attempt, the frames degrade
// (Stats.ReplayDegradedFrames) instead of committing, and the session's
// commit count drops. The production default stays tuned for the board;
// only the test's injected engine widens its budget (ADR-0113).
const ocrTimeoutScale = 5

// patienceScale scales the test suite's event-and-state patience ceilings
// when the test binary is race-instrumented (issue #22): the ceilings are
// bounds on "this healthy run eventually does X", not part of the contract
// — a run that misses one has failed, but a run that merely takes 5x longer
// under -race on a saturated host must not. The named tests' ceilings
// (TestCaptureSessionCommitsSyntheticText's commit and terminal bounds,
// TestReplayPauseMutesSpeech's waitEvents/waitState patience) multiply by it;
// 5 is the same factor the pipeline, ocr and this package's real-Tesseract
// gates use.
const patienceScale = 5
