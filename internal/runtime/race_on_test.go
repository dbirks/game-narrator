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
