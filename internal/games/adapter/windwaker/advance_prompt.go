package windwaker

// advance_prompt.go is the visual half of the typewriter settle (ADR-0152):
// Wind Waker draws a teal-green "advance prompt" swirl centred horizontally
// just below the bottom dialogue box once the line has finished typing, and
// this file detects it.
//
// The pixel rule is a measurement, not a guess: on 19 frames of a 640×480
// Dolphin capture the matching count is 502–544 with the prompt present and 0
// on every frame without it (the box mid-reveal, an empty/closing box and a
// frame with no box all score 0), so a 120-pixel threshold sits between the
// two clouds with a four-fold margin. The recognition tracker consumes the
// answer through adapter.RevealCompleteDetector and never sees the pixels.

import (
	"image"
	"image/color"
	"math"
)

// Advance prompt ROI geometry, measured at 640×480 (ADR-0152). The disc's
// centre sits ≈14 px below the region rect's bottom edge, so the window is
// −20..+60 px below the bottom edge, ±40 px around the region's horizontal
// centre: wide enough to ride the swirl's pulse, narrow enough that a teal
// object elsewhere in the frame cannot score.
const (
	promptROIXMin = -40 // left of the region's horizontal centre
	promptROIXMax = 40  // right of it, exclusive
	promptROIYMin = -20 // above the region's bottom edge
	promptROIYMax = 60  // below it, exclusive
)

// promptThreshold640 is the present/absent boundary in matching pixels at the
// reference 640×480 capture: present reads 502–544, absent reads 0.
const promptThreshold640 = 120

// promptRefWidth is the frame width the threshold and the ROI geometry were
// measured at. Other widths scale the threshold by (frameW/640)² so a 2×
// capture — where the same disc covers four times the pixels — keeps the same
// decision, and the ROI is measured on the region's pixel edges, so it scales
// with the frame on its own.
const promptRefWidth = 640

// AdvancePromptPresent reports whether the frame shows the advance prompt under
// the given region. img is the full frame; region is the region rect in frame
// pixels (the profile's calibrated band, before the OCR inset — the swirl sits
// below that edge, not below the inset's).
//
// It returns the present/absent verdict and the score it ran on, so a caller
// (the developer inspector, a test) can see the evidence rather than the
// boolean alone. The scan is a bounded pixel count over an 80×80 window — no
// allocation, no clock, pure over the pixels handed in.
func AdvancePromptPresent(img image.Image, region image.Rectangle) (present bool, score int) {
	if img == nil {
		return false, 0
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 || region.Dx() <= 0 || region.Dy() <= 0 {
		return false, 0
	}
	// The swirl is centred on the region's horizontal centre; the pixel rule
	// is a test on the green channel's dominance, so the centre's x is what
	// the ROI's x window is built from, and the bottom edge its y window.
	cx := region.Min.X + region.Dx()/2
	bottom := region.Max.Y
	// The ROI in frame pixels, clamped to the frame: a region whose bottom
	// edge is near the frame's bottom edge (a full-bleed box) must not scan
	// outside the image, and a region above the frame's top would clamp to
	// nothing rather than panic.
	roi := image.Rect(cx+promptROIXMin, bottom+promptROIYMin, cx+promptROIXMax, bottom+promptROIYMax)
	roi = roi.Intersect(bounds)
	if roi.Empty() {
		return false, 0
	}
	for y := roi.Min.Y; y < roi.Max.Y; y++ {
		for x := roi.Min.X; x < roi.Max.X; x++ {
			if isPromptPixel(img.At(x, y)) {
				score++
			}
		}
	}
	return score >= promptThresholdFor(bounds.Dx()), score
}

// promptThresholdFor scales the reference threshold by the frame's area ratio:
// the same physical disc covers (frameW/640)² times the pixels at another
// width, so the boundary scales with it. Rounded up: a capture that rounds to
// one pixel short of the scaled threshold must not read as absent.
func promptThresholdFor(frameW int) int {
	if frameW <= 0 {
		return promptThreshold640
	}
	scale := float64(frameW) / promptRefWidth
	return int(math.Ceil(float64(promptThreshold640) * scale * scale))
}

// isPromptPixel is the measured rule on one pixel: a green that is clearly
// present (g > 0.40), clearly dominant over red (g−r > 0.12 — the saturated
// teal half of the pulse), and at least as strong as blue (g >= b−0.05 — the
// pale white-green half, where blue rises with the green). A blue or grey
// scene patch fails the red test; a saturated blue fails the green floor.
func isPromptPixel(c color.Color) bool {
	r16, g16, b16, _ := c.RGBA()
	r, g, b := float64(r16)/0xffff, float64(g16)/0xffff, float64(b16)/0xffff
	// Teal, not green: b > 0.35 keeps island grass and foliage under a
	// middle-height box out (measured: 1386–3050 grass pixels in the ROI
	// without it, 0 with it; the prompt keeps 493–523).
	return g > 0.40 && g-r > 0.12 && g >= b-0.05 && b > 0.35
}

// RevealComplete implements adapter.RevealCompleteDetector (ADR-0152). It is
// the capability the runtime finds by assertion at session start; the verdict
// is the score's, so a test and the settle never disagree about the evidence.
func (a Adapter) RevealComplete(img image.Image, region image.Rectangle) bool {
	present, _ := AdvancePromptPresent(img, region)
	return present
}
