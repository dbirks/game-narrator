package recognition

import (
	"testing"
	"time"
)

// TestTypewriterGrowthHoldsTheStabilityWindow is the regression test for the
// typewriter freeze: while the raw OCR keeps growing one word per frame, the
// stability clock must keep restarting, so no READY transition can happen
// until the full text has been on screen for the whole window. A vote that
// keeps returning the shorter prefix must not let the tracker declare READY
// on a three-word fragment.
func TestTypewriterGrowthHoldsTheStabilityWindow(t *testing.T) {
	d := newDriver(t, Config{
		AppearFrames:     2,
		StableWindow:     100 * time.Millisecond,
		MinConfidence:    0.5,
		VoteWindow:       3,
		AdvanceRunFrames: 2,
	})
	d.FrameInterval = 100 * time.Millisecond

	// One word per frame. driver.frame supplies a fresh region hash every
	// call, so every growth step is a real OCR pass, not a hash reuse.
	growth := []string{
		"The",
		"The harbour",
		"The harbour bell",
		"The harbour bell rang",
		"The harbour bell rang twice",
		"The harbour bell rang twice before",
		"The harbour bell rang twice before anyone looked up.",
	}
	full := growth[len(growth)-1]

	var readyFrame int64 // FrameSeq of the first transition to READY, 0 if none
	observe := func(trs []Transition) {
		if readyFrame != 0 {
			return
		}
		for _, tr := range trs {
			if tr.To == StateReady {
				readyFrame = d.seq
				return
			}
		}
	}

	for _, text := range growth {
		observe(d.frame(text, 0.9))
	}
	fullFrame := d.seq // frame on which the full text first appears
	for i := 0; i < 4; i++ {
		observe(d.frame(full, 0.9))
	}

	// Boundary: the first frame on which the full text has been on screen for
	// the whole window (window is exactly one frame at this interval).
	boundary := fullFrame + int64(d.tr.Config().StableWindow/d.FrameInterval)
	if readyFrame < boundary {
		t.Fatalf("READY at frame %d, want no READY before frame %d (full text first shown at frame %d, window %v)",
			readyFrame, boundary, fullFrame, d.tr.Config().StableWindow)
	}
	if readyFrame == 0 {
		t.Fatalf("no transition to READY through frame %d; final state %s", d.seq, d.tr.State())
	}
	if got := d.tr.State(); got != StateReady {
		t.Fatalf("final state = %s, want ready", got)
	}
	if got := d.tr.Stats().RevealGrowthFrames; got == 0 {
		t.Fatalf("RevealGrowthFrames = 0, want > 0 (the raw text grew across the reveal)")
	}
	if got, want := d.tr.Text(), "the harbour bell rang twice before anyone looked up."; got != want {
		t.Fatalf("accepted text = %q, want %q", got, want)
	}
}
