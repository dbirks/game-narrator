//go:build unix

package dolphin

// savestate_fixture_unit_test.go is the unit coverage for the M6 press-pace
// oracle (the typewriter-settle press policy and the press-response watch)
// — the two policies the savestate fixture gate rides on. Pure synthetic
// inputs: no Dolphin, no host tools, runs in the unit gate.

import (
	"image"
	"testing"
)

// TestPressPacePressesOnSettledTypewriter: the typewriter's growing ink never
// presses; the settle (two quiet samples) does, once.
func TestPressPacePressesOnSettledTypewriter(t *testing.T) {
	p := &pressPace{cooldown: 0}
	// The typewriter: growing ink — no press.
	for _, ink := range []float64{0.010, 0.025, 0.040, 0.055} {
		if p.onInk(ink) {
			t.Fatalf("press during the typewriter's growth at ink %g", ink)
		}
	}
	// The settle: the first quiet sample (the typewriter stopped) presses —
	// the window holds the last two samples, so the growth→quiet pair passes
	// the "no growth inside the window" check.
	if !p.onInk(0.056) {
		t.Fatal("no press on the settled typewriter")
	}
	if p.presses != 1 {
		t.Fatalf("presses = %d, want 1", p.presses)
	}
	// A fresh typewriter after the press: growth again — no re-press on the
	// growth samples (the press reset the window).
	if p.onInk(0.012) {
		t.Fatal("press on the fresh typewriter's first sample")
	}
	if p.onInk(0.030) {
		t.Fatal("press on the fresh typewriter's growth")
	}
	// And the fresh typewriter's settle presses again (the policy repeats).
	if !p.onInk(0.031) {
		t.Fatal("no press on the fresh typewriter's settle")
	}
	if p.presses != 2 {
		t.Fatalf("presses = %d, want 2", p.presses)
	}
}

// TestPressPaceSkipCountsAFade: a text that fades (onFade) without a settle
// press is a skip; a pressed text is not.
func TestPressPaceSkipCountsAFade(t *testing.T) {
	p := &pressPace{cooldown: 0}
	p.onInk(0.03) // the typewriter running
	p.onFade()    // it faded before the settle
	if p.skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the text faded without a press)", p.skipped)
	}
	p.onInk(0.05)
	p.onInk(0.05) // settled — the press
	if p.presses != 1 {
		t.Fatalf("presses = %d, want 1", p.presses)
	}
	p.onFade() // the pressed text's region vanishes: the advance, not a skip
	if p.skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (a pressed text's vanish is not a skip)", p.skipped)
	}
}

// TestNoRespWatchStopsOnUnresponsiveScreen: three post-press samples inside
// the RMSE floor of the pre-press frame declare the screen unresponsive; a
// sample that moves clears the watch.
func TestNoRespWatchStopsOnUnresponsiveScreen(t *testing.T) {
	base := solidImage(64, 48, 90)
	// Same-screen re-renders: tiny per-pixel jitter (render noise, the
	// double-restore's measured 0.025 RMSE is far above jitter this small).
	jittered := func() image.Image {
		j := solidImage(64, 48, 90).(*image.RGBA)
		for i := 0; i < 400; i++ { // ~1% of the pixels, +1 luma
			x, y := i%64, i%48
			i4 := (y*64 + x) * 4
			j.Pix[i4]++
		}
		return j
	}
	n := &noRespWatch{}
	if n.observe(base) {
		t.Fatal("observe without a press: no anchor, no stop")
	}
	n.anchorForPress(base)
	if n.observe(jittered()) {
		t.Fatal("stop on the first post-press sample")
	}
	if n.observe(jittered()) {
		t.Fatal("stop on the second post-press sample")
	}
	if !n.observe(jittered()) {
		t.Fatal("no stop after three unresponsive post-press samples")
	}
	if !n.waiting() {
		t.Fatal("waiting must be true while the anchor is set")
	}
	// A moved screen clears the watch (the advance happened).
	moved := solidImage(64, 48, 160)
	n2 := &noRespWatch{}
	n2.anchorForPress(base)
	if n2.observe(moved) {
		t.Fatal("stop on a moved screen")
	}
	if n2.waiting() {
		t.Fatal("a moved screen must clear the watch")
	}
}
