//go:build !race

package profileio

// raceEnabled is false in an uninstrumented build; see race_on_test.go for why
// the scale test distinguishes the two.
const raceEnabled = false
