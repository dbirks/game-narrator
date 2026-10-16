//go:build race

package matcher

// timingAsserted is false under -race: the race runtime multiplies the cost of
// every memory touch, so a latency measured there measures the detector, not the
// matcher (docs/spec/12 wants both gates run, and docs/spec/16 forbids presenting
// a number from the wrong build as a pass). The scale test logs instead of
// asserting, and the definition of done runs it uninstrumented.
const timingAsserted = false
