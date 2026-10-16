//go:build !race

package matcher

// timingAsserted is true in the uninstrumented build, where the latency and memory
// gates in docs/spec/16 M2 are meaningful.
const timingAsserted = true
