package dolphin

import (
	"context"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"
)

// oliveTestFill is the measured olive talk-arrow fill (the isOliveArrowPixel
// clauses: green at or above red, bright, the yellow family, red in
// [130, 225)) — the restored-deck calibration's arrow rgb ~ (165, 185, 95).
var (
	oliveTestFill = color.RGBA{R: 165, G: 185, B: 95, A: 255}
	// deckTestFill clears no olive clause (the dark wooden deck): the background
	// must never read as the arrow.
	deckTestFill = color.RGBA{R: 60, G: 80, B: 50, A: 255}
)

// oliveFrame paints one solid olive blob bbox (x0..x1, y0..y1) over the deck.
func oliveFrame(w, h, x0, x1, y0, y1 int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, deckTestFill)
		}
	}
	for y := y0; y <= y1; y++ {
		for x := x0; x <= x1; x++ {
			img.SetRGBA(x, y, oliveTestFill)
		}
	}
	return img
}

// approachStep renders one route-style approach step onto the runner's Step
// (the {"approach": true, "max_steps": N} of ADR-0166).
func approachStep(maxSteps int) Step {
	return Step{Name: "approach", Approach: true, MaxSteps: maxSteps, Timeout: 10 * time.Second}
}

// TestApproachStepHoldsTowardTheOliveArrow drives the ADR-0166 approach step
// through the fake bridge over synthetic olive-blob frames and asserts the
// stick direction each frame chose: the blob LEFT of centre holds LEFT, RIGHT
// of centre holds RIGHT, at centre holds FORWARD. The hold is the measured
// lateral (300 ms) / forward (400 ms) pair; the step is pixel-only (no vision
// model — the arrow's own pixels are the signal).
func TestApproachStepHoldsTowardTheOliveArrow(t *testing.T) {
	cases := []struct {
		name     string
		x0, x1   int // the blob's bbox; the centroid lands at cx = (x0+x1)/2 / w
		wantX    float64
		wantHold time.Duration
	}{
		{"left of centre", 102, 121, -1, 300 * time.Millisecond}, // cx ≈ 0.35
		{"right of centre", 198, 217, 1, 300 * time.Millisecond}, // cx ≈ 0.65
		{"at centre", 150, 169, 0, 400 * time.Millisecond},       // cx ≈ 0.5
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := &fakeBridge{shots: []image.Image{oliveFrame(320, 240, tc.x0, tc.x1, 40, 59)}}
			tr, err := RunScript(context.Background(), fb, Script{Steps: []Step{approachStep(1)}})
			if err != nil {
				t.Fatalf("RunScript: %v (trace: %+v)", err, tr)
			}
			if !tr.OK {
				t.Fatalf("the approach step failed: %s", tr.LastError)
			}
			if len(tr.Steps) != 1 {
				t.Fatalf("trace = %d steps, want 1", len(tr.Steps))
			}
			if len(fb.injects) != 1 {
				t.Fatalf("the fake saw %d injects, want 1 (the one hold)", len(fb.injects))
			}
			in := fb.injects[0]
			wantY := 1.0 // forward (away from the camera)
			if tc.wantX != 0 {
				wantY = 0.5 // a forward diagonal toward the arrow
			}
			if in.axes.MainX != tc.wantX || in.axes.MainY != wantY {
				t.Errorf("inject axes = %+v, want main (x=%v, y=%v) — the stick toward the arrow", in.axes, tc.wantX, wantY)
			}
			if in.hold != tc.wantHold {
				t.Errorf("inject hold = %v, want %v (the lateral hold is shorter than the forward hold)", in.hold, tc.wantHold)
			}
		})
	}
}

// TestApproachStepStopsWhenAreaStopsGrowing: a static olive blob (the arrow's
// area never grows — the step has converged) ends the approach on the third
// consecutive stalled sample, well before the max_steps budget.
func TestApproachStepStopsWhenAreaStopsGrowing(t *testing.T) {
	fb := &fakeBridge{shots: []image.Image{oliveFrame(320, 240, 150, 169, 40, 59)}} // cx ≈ 0.5, static
	tr, err := RunScript(context.Background(), fb, Script{Steps: []Step{approachStep(5)}})
	if err != nil {
		t.Fatalf("RunScript: %v (trace: %+v)", err, tr)
	}
	if !tr.OK {
		t.Fatalf("the approach step should stop on the area stall, not fail: %s", tr.LastError)
	}
	// The stall fires on sample 4 (grew, stalled, stalled, stalled) — three
	// holds, not the full max_steps.
	if len(fb.injects) != 3 {
		t.Fatalf("the fake saw %d injects, want 3 (one per growing sample before the stall)", len(fb.injects))
	}
	last := tr.Steps[0].Details[len(tr.Steps[0].Details)-1]
	if !strings.Contains(last, "stopped growing") {
		t.Errorf("the step's last detail = %q, want the area-stall stop reason", last)
	}
}
