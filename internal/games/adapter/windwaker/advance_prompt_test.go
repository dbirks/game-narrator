package windwaker

// advance_prompt_test.go unit-tests the advance-prompt detector on synthetic
// images drawn here in the test (AGENTS.md rule 5: no real frames or captured
// game pixels are committed). The colours and geometry are invented to sit on
// the right side of the measured pixel rule: a dark scene plus a dark box
// scores 0, a teal disc under the box's bottom edge scores in the presence
// band, and a patch above the bottom edge is outside the ROI.

import (
	"image"
	"image/color"
	"testing"

	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// promptTestFrame draws a 640×480 frame: a dark blue-grey scene, a dark
// dialogue-box panel on the bottom half (the Wind Waker bottom region's
// measured band, y 0.62–0.87, as pixels), and — when disc is non-nil — a
// filled disc centred at (320, 432) ≈14 px below the box's bottom edge, the
// position the swirl was measured at.
func promptTestFrame(t *testing.T, disc color.Color) *image.RGBA {
	t.Helper()
	const (
		w = 640
		h = 480
	)
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	scene := color.RGBA{20, 24, 34, 255} // dark blue-grey, below the green floor
	panel := color.RGBA{12, 16, 22, 255} // the box's dark panel
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, scene)
		}
	}
	box := image.Rect(80, 298, 560, 418)
	for y := box.Min.Y; y < box.Max.Y; y++ {
		for x := box.Min.X; x < box.Max.X; x++ {
			img.Set(x, y, panel)
		}
	}
	if disc != nil {
		drawDisc(img, 320, 432, 14, disc)
	}
	return img
}

// promptRegion is the synthetic box's region rect in frame pixels: the same
// band promptTestFrame draws the panel at, centre x 320, bottom edge 418.
func promptRegion() image.Rectangle {
	return image.Rect(80, 298, 560, 418)
}

// drawDisc fills a disc of radius r centred at (cx, cy) with c.
func drawDisc(img *image.RGBA, cx, cy, r int, c color.Color) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				img.Set(cx+dx, cy+dy, c)
			}
		}
	}
}

// tealPrompt is the saturated half of the swirl's pulse (measured 502–544
// pixels at 640×480); palePrompt the pale white-green half.
var (
	tealPrompt = color.RGBA{51, 217, 191, 255}
	palePrompt = color.RGBA{204, 247, 224, 255}
)

func TestAdvancePromptPresent_TealDiscUnderBox(t *testing.T) {
	img := promptTestFrame(t, tealPrompt)
	present, score := AdvancePromptPresent(img, promptRegion())
	if !present {
		t.Fatalf("present = false, want true (score %d)", score)
	}
	// The disc is ≈14 px radius (≈600 px, the whole disc sits inside the ROI),
	// and the measured presence band is 502–544 — assert the same shape rather
	// than one exact number the anti-aliasing of the drawn disc could move.
	if score < 400 || score > 800 {
		t.Fatalf("score = %d, want in the presence band [400,800]", score)
	}
}

func TestAdvancePromptPresent_PaleDiscUnderBox(t *testing.T) {
	img := promptTestFrame(t, palePrompt)
	present, score := AdvancePromptPresent(img, promptRegion())
	if !present {
		t.Fatalf("present = false, want true for the pale white-green pulse (score %d)", score)
	}
	if score < 400 || score > 800 {
		t.Fatalf("score = %d, want in the presence band [400,800]", score)
	}
}

func TestAdvancePromptPresent_NoDisc(t *testing.T) {
	img := promptTestFrame(t, nil)
	present, score := AdvancePromptPresent(img, promptRegion())
	if present {
		t.Fatalf("present = true, want false with no disc")
	}
	if score != 0 {
		t.Fatalf("score = %d, want 0 (dark scene and panel must not match the rule)", score)
	}
}

func TestAdvancePromptPresent_BlueDiscIsNotThePrompt(t *testing.T) {
	// A saturated blue object under the box: green is not dominant, so the
	// rule must not read it as the prompt.
	img := promptTestFrame(t, color.RGBA{30, 80, 230, 255})
	present, score := AdvancePromptPresent(img, promptRegion())
	if present {
		t.Fatalf("present = true, want false for a blue disc (score %d)", score)
	}
	if score != 0 {
		t.Fatalf("score = %d, want 0", score)
	}
}

func TestAdvancePromptPresent_PatchAboveBoxBottom(t *testing.T) {
	// A big teal patch inside the box, above its bottom edge: the ROI is only
	// −20..+60 px around that edge, so scene colour above it must not count.
	img := promptTestFrame(t, nil)
	patch := image.Rect(100, 120, 540, 396)
	for y := patch.Min.Y; y < patch.Max.Y; y++ {
		for x := patch.Min.X; x < patch.Max.X; x++ {
			img.Set(x, y, tealPrompt)
		}
	}
	present, score := AdvancePromptPresent(img, promptRegion())
	if present {
		t.Fatalf("present = true, want false for a patch above the bottom edge (score %d)", score)
	}
	if score != 0 {
		t.Fatalf("score = %d, want 0 (the patch does not intersect the ROI)", score)
	}
}

func TestAdvancePromptPresent_SubThresholdDisc(t *testing.T) {
	// A small teal disc the size of scene noise: below the 120-pixel threshold
	// the verdict is absent, which is the margin the measurement's two clouds
	// (0 vs 502–544) buy.
	img := promptTestFrame(t, nil)
	drawDisc(img, 320, 432, 4, tealPrompt)
	present, score := AdvancePromptPresent(img, promptRegion())
	if present {
		t.Fatalf("present = true, want false for a sub-threshold disc")
	}
	if score < 1 || score >= promptThreshold640 {
		t.Fatalf("score = %d, want a small positive count below %d", score, promptThreshold640)
	}
}

func TestAdvancePromptPresent_ScaledFrameWidth(t *testing.T) {
	// At 1280×960 the same disc covers four times the pixels, so the threshold
	// scales by (frameW/640)²: 120 → 480.
	if got := promptThresholdFor(1280); got != 480 {
		t.Fatalf("promptThresholdFor(1280) = %d, want 480", got)
	}
	if got := promptThresholdFor(640); got != 120 {
		t.Fatalf("promptThresholdFor(640) = %d, want 120", got)
	}
	img := image.NewRGBA(image.Rect(0, 0, 1280, 960))
	for y := 0; y < 960; y++ {
		for x := 0; x < 1280; x++ {
			img.Set(x, y, color.RGBA{20, 24, 34, 255})
		}
	}
	drawDisc(img, 640, 864, 28, tealPrompt) // the 2× disc at the 2× position
	region := image.Rect(160, 596, 1120, 836)
	if present, score := AdvancePromptPresent(img, region); !present {
		t.Fatalf("present = false at 2× scale, want true (score %d)", score)
	}
	// A disc that stays small at 2× is still under the scaled threshold.
	small := image.NewRGBA(image.Rect(0, 0, 1280, 960))
	for y := 0; y < 960; y++ {
		for x := 0; x < 1280; x++ {
			small.Set(x, y, color.RGBA{20, 24, 34, 255})
		}
	}
	drawDisc(small, 640, 864, 8, tealPrompt)
	if present, score := AdvancePromptPresent(small, region); present {
		t.Fatalf("present = true for a small disc at 2× scale, want false (score %d)", score)
	}
}

func TestAdvancePromptPresent_ClampedAtFrameBottom(t *testing.T) {
	// A box whose bottom edge sits near the frame's bottom edge: the ROI's
	// +60 px window runs past the frame and must clamp rather than panic.
	img := promptTestFrame(t, nil)
	drawDisc(img, 320, 466, 10, tealPrompt)
	region := image.Rect(80, 360, 560, 470)
	if present, score := AdvancePromptPresent(img, region); !present {
		t.Fatalf("present = false with the ROI clamped at the frame edge (score %d)", score)
	}
	// A region entirely above the frame: the clamped ROI is empty, no panic.
	above := image.Rect(80, -400, 560, -300)
	if present, score := AdvancePromptPresent(img, above); present || score != 0 {
		t.Fatalf("present = %v score = %d for a region above the frame, want false 0", present, score)
	}
}

func TestAdvancePromptPresent_DegenerateInputs(t *testing.T) {
	cases := []struct {
		name   string
		img    image.Image
		region image.Rectangle
	}{
		{"nil image", nil, promptRegion()},
		{"empty region", promptTestFrame(t, tealPrompt), image.Rect(0, 0, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if present, score := AdvancePromptPresent(tc.img, tc.region); present || score != 0 {
				t.Fatalf("present = %v score = %d, want false 0", present, score)
			}
		})
	}
}

func TestRevealCompleteCapability(t *testing.T) {
	// The capability is how the runtime finds the detector: a compile-time
	// assertion keeps an accidental method change a build error, not a
	// session that quietly loses the settle.
	var _ adapter.RevealCompleteDetector = Adapter{}

	a := New()
	detector, ok := a.(adapter.RevealCompleteDetector)
	if !ok {
		t.Fatal("Adapter does not implement adapter.RevealCompleteDetector")
	}
	withDisc := promptTestFrame(t, tealPrompt)
	if !detector.RevealComplete(withDisc, promptRegion()) {
		t.Fatal("RevealComplete = false with the teal disc, want true")
	}
	without := promptTestFrame(t, nil)
	if detector.RevealComplete(without, promptRegion()) {
		t.Fatal("RevealComplete = true with no disc, want false")
	}
}

// TestAdvancePromptPresent_GrassIsNotThePrompt pins the blue floor: island
// grass and foliage under a middle-height box are green, not teal. Without
// the floor the measured island scene put 1386–3050 grass pixels in the ROI
// (a false "reveal complete"); the prompt itself keeps ~500.
func TestAdvancePromptPresent_GrassIsNotThePrompt(t *testing.T) {
	img := promptTestFrame(t, color.RGBA{86, 168, 52, 255}) // sunlit grass green
	if present, score := AdvancePromptPresent(img, promptRegion()); present {
		t.Fatalf("grass-green disc read as the prompt (score %d)", score)
	}
}
