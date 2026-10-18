package hwsmoke

import (
	"fmt"
	"os"
	"testing"
)

// skipT is the *testing.T surface hwGate needs: Helper (so the skip is
// attributed to the calling test) and Skip.
type skipT interface {
	Helper()
	Skip(...any)
}

// hwGate enforces the CI rule that hardware smoke does not run inside a
// `go test ./...` sweep: it skips the calling test as NOT RUN unless
// GAME_NARRATOR_HW_SMOKE=1 is set. The two sanctioned runners — the
// Makefile test-hardware target and scripts/hw/run-on-device.sh — both set
// the variable. The skip text is the gate summariser's NOT RUN vocabulary:
// it becomes the printed setup instruction, so an operator reading a CI log
// sees exactly where the hardware gate runs.
//
// Every TestHW* calls hwGate as its first statement; nothing in a test is
// allowed to touch a device, a Tesseract subprocess or the 60 s serve
// session before the gate has spoken.
func hwGate(t skipT) {
	t.Helper()
	if os.Getenv(EnvSmokeGate) != "1" {
		t.Skip("NOT RUN: hardware smoke runs via make test-hardware / scripts/hw/run-on-device.sh")
	}
}

// TestSmokeGateSkipsWithoutTheEnvVar proves the gate itself: without
// GAME_NARRATOR_HW_SMOKE a hardware test is NOT RUN with the documented
// message, and with the variable set the gate lets the test through. This
// is the test that keeps the CI contract — `go test -race ./...` skipping
// every TestHW* — honest rather than assumed.
func TestSmokeGateSkipsWithoutTheEnvVar(t *testing.T) {
	t.Setenv(EnvSmokeGate, "")
	off := &gateTestT{}
	hwGate(off)
	if !off.skipped {
		t.Fatalf("gate did not skip a hardware test without %s", EnvSmokeGate)
	}
	if off.skipMsg != "NOT RUN: hardware smoke runs via make test-hardware / scripts/hw/run-on-device.sh" {
		t.Fatalf("skip message = %q", off.skipMsg)
	}

	t.Setenv(EnvSmokeGate, "1")
	on := &gateTestT{}
	hwGate(on)
	if on.skipped {
		t.Fatalf("gate skipped a hardware test although %s=1 is set", EnvSmokeGate)
	}
}

// TestSmokeGateRejectsAnyOtherValue pins the strict equality: the gate is a
// consent switch, like GAME_NARRATOR_HW_AUDIO_CONSENT, so "true" or "yes"
// must not open it.
func TestSmokeGateRejectsAnyOtherValue(t *testing.T) {
	for _, v := range []string{"0", "true", "yes"} {
		t.Setenv(EnvSmokeGate, v)
		g := &gateTestT{}
		hwGate(g)
		if !g.skipped {
			t.Fatalf("%s=%q opened the gate; want NOT RUN", EnvSmokeGate, v)
		}
	}
}

// gateTestT records the skip instead of running a nested test, so the gate
// proof does not depend on t.Setenv semantics inside sub-tests.
type gateTestT struct {
	skipped bool
	skipMsg string
}

func (g *gateTestT) Helper() {}

func (g *gateTestT) Skip(msg ...any) {
	g.skipped = true
	if len(msg) > 0 {
		g.skipMsg = fmt.Sprint(msg[0])
	}
}
