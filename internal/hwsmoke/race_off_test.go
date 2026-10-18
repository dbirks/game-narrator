//go:build !race

package hwsmoke

// ocrTimeoutScale is 1 in an uninstrumented build: TestHWOCRControlledImage
// runs the engine's production default budget. See race_on_test.go for why
// the race build scales it (CI run 39090252213, "engine timed out").
const ocrTimeoutScale = 1
