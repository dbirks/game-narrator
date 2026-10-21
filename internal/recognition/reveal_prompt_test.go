package recognition

// ADR-0152: a game's reveal-complete marker (Wind Waker's advance prompt)
// settles a line as soon as it shows, and — once the marker is proven on the
// source — a typewriter pause without it is not mistaken for the end.

import (
	"fmt"
	"testing"
	"time"
)

// promptFrame is driver.frame with the reveal detector's verdict attached.
func (d *driver) promptFrame(text string, conf float64, prompt bool) []Transition {
	d.seq++
	d.ts += int64(d.FrameInterval)
	return d.tr.Step(Observation{
		FrameSeq:           d.seq,
		CaptureTimestampNS: d.ts,
		SourceClock:        d.Clock,
		RegionID:           "bottom",
		RegionPresent:      true,
		RegionHash:         fmt.Sprintf("h-%d", d.seq),
		OCR:                ocrResult(text, conf),
		RevealDetector:     true,
		RevealComplete:     prompt,
	})
}

func revealCfg() Config {
	return Config{
		AppearFrames:     2,
		StableWindow:     600 * time.Millisecond,
		MinConfidence:    0.5,
		VoteWindow:       3,
		AdvanceRunFrames: 2,
	}
}

func readyIn(trs []Transition) bool {
	for _, tr := range trs {
		if tr.To == StateReady {
			return true
		}
	}
	return false
}

// TestRevealPromptSettlesWithoutTheWindow: with the marker on screen, text
// unchanged since the previous read stabilizes at once and is READY one
// frame later, instead of waiting out a 600 ms window (6 frames at 100 ms).
func TestRevealPromptSettlesWithoutTheWindow(t *testing.T) {
	d := newDriver(t, revealCfg())
	d.FrameInterval = 100 * time.Millisecond
	line := "The lantern keeper waved from the pier."
	d.promptFrame("The lantern", 0.9, false)
	d.promptFrame("The lantern keeper waved", 0.9, false)
	d.promptFrame(line, 0.9, false)
	// The marker appears with the full line, unchanged from the previous read.
	ready := 0
	for i := 1; i <= 3 && ready == 0; i++ {
		if readyIn(d.promptFrame(line, 0.9, true)) {
			ready = i
		}
	}
	if ready == 0 || ready > 2 {
		t.Fatalf("READY on prompt frame %d, want within 2 frames of the marker (the window alone needs 6); state %s", ready, d.tr.State())
	}
}

// TestRevealPromptHoldsAMidLinePause: once the marker has been seen on this
// source, a stable read WITHOUT it (the typewriter paused mid-line) does not
// stabilize at the plain window; the full line, shown with the marker, does.
func TestRevealPromptHoldsAMidLinePause(t *testing.T) {
	d := newDriver(t, revealCfg())
	d.FrameInterval = 100 * time.Millisecond
	// Prove the detector on an earlier box.
	first := "A gull circled the mast."
	d.promptFrame("A gull", 0.9, false)
	d.promptFrame(first, 0.9, false)
	d.promptFrame(first, 0.9, true)
	d.promptFrame(first, 0.9, true)

	// Next box: the reveal pauses on a fragment for 1 s (> the 600 ms window).
	frag := "Well..."
	for i := 0; i < 10; i++ {
		trs := d.promptFrame(frag, 0.9, false)
		for _, tr := range trs {
			if tr.To == StateStabilizing || tr.To == StateReady {
				t.Fatalf("the paused fragment reached %s at pause frame %d: a mid-line pause was taken for the end", tr.To, i)
			}
		}
	}
	full := "Well... I suppose the tide will wait for no one."
	d.promptFrame(full, 0.9, false)
	got := false
	for i := 0; i < 3 && !got; i++ {
		got = readyIn(d.promptFrame(full, 0.9, true))
	}
	if !got {
		t.Fatalf("the full line never reached READY with the marker; state %s", d.tr.State())
	}
}

// TestRevealPromptFallbackAndUnprovenSource: a source that never shows the
// marker keeps the plain stability window (an unproven detector must not
// delay anything), and a proven source still settles a marker-less box after
// revealPromptFallback.
func TestRevealPromptFallbackAndUnprovenSource(t *testing.T) {
	t.Run("unproven", func(t *testing.T) {
		d := newDriver(t, revealCfg())
		d.FrameInterval = 100 * time.Millisecond
		line := "No marker ever shows on this capture."
		got := false
		for i := 0; i < 12 && !got; i++ {
			got = readyIn(d.promptFrame(line, 0.9, false))
		}
		if !got {
			t.Fatalf("an unproven detector delayed READY past the plain window; state %s", d.tr.State())
		}
	})
	t.Run("fallback", func(t *testing.T) {
		d := newDriver(t, revealCfg())
		d.FrameInterval = 100 * time.Millisecond
		d.promptFrame("Proven.", 0.9, false)
		d.promptFrame("Proven.", 0.9, true)
		d.promptFrame("Proven.", 0.9, true)
		line := "This box hides its marker."
		frames := 0
		got := false
		for frames < 40 && !got {
			frames++
			got = readyIn(d.promptFrame(line, 0.9, false))
		}
		if !got {
			t.Fatalf("a proven source never settled a marker-less box; state %s", d.tr.State())
		}
		if waited := time.Duration(frames) * d.FrameInterval; waited < revealPromptFallback {
			t.Fatalf("settled after %v, before the %v fallback", waited, revealPromptFallback)
		}
	})
}
