package vision

import (
	"image"
	"image/color"
	"testing"
)

// studioCrop builds a gray crop whose content lives entirely in the BT.601
// studio window: a 16 (limited black) box with a 235 (limited white) bar —
// the exact pixels a converter delivers when it sends studio-range YUV into a
// stream the demuxer declares full-range.
func studioCrop() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			g.SetGray(x, y, color.Gray{Y: 16})
		}
	}
	for y := 20; y < 28; y++ {
		for x := 8; x < 56; x++ {
			g.SetGray(x, y, color.Gray{Y: 235})
		}
	}
	return g
}

// fullCrop is the same picture delivered in full range: black 0, white 255.
func fullCrop() *image.Gray {
	g := image.NewGray(image.Rect(0, 0, 64, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			g.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	for y := 20; y < 28; y++ {
		for x := 8; x < 56; x++ {
			g.SetGray(x, y, color.Gray{Y: 255})
		}
	}
	return g
}

func TestNormalizeLumaRangeStretchesStudioWindow(t *testing.T) {
	// 32x8: 40% studio-black (16), 40% studio-white (235), 20% mid (125).
	g := image.NewGray(image.Rect(0, 0, 32, 8))
	for x := 0; x < 32; x++ {
		for y := 0; y < 8; y++ {
			var v uint8
			switch {
			case y < 3:
				v = 16
			case y < 6:
				v = 235
			default:
				v = 125
			}
			g.SetGray(x, y, color.Gray{Y: v})
		}
	}
	normalizeLumaRange(g)
	// 16→0, 125→(125-16)*255/219=126, 235→255 — the canonical studio mapping,
	// not an auto-contrast fit.
	for row, want := range map[int]uint8{1: 0, 4: 255, 7: 126} {
		if got := g.GrayAt(5, row).Y; got != want {
			t.Errorf("row %d = %d, want %d", row, got, want)
		}
	}
}

func TestNormalizeLumaRangeLeavesFullRangeAlone(t *testing.T) {
	// A full-range picture has real pixels at 0 and 255, so the studio test
	// must not fire: stretching it would invent contrast.
	g := fullCrop()
	before := copyGray(g)
	normalizeLumaRange(g)
	if !equalGray(before, g) {
		t.Error("a full-range crop was modified; the step must be a no-op")
	}
}

func TestNormalizeLumaRangeLeavesDarkSceneAlone(t *testing.T) {
	// A dark scene that carries a true-black edge: the bin at 0 holds more
	// than 1% of the pixels, so the lowest counting bin is 0, the full-range
	// signature, and the step must stay quiet.
	g := image.NewGray(image.Rect(0, 0, 32, 16)) // 512 pixels
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			g.SetGray(x, y, color.Gray{Y: 60})
		}
	}
	for i := 0; i < 10; i++ { // 10/512 = 2% true-black pixels
		g.SetGray(i, 15, color.Gray{Y: 0})
	}
	before := copyGray(g)
	normalizeLumaRange(g)
	if !equalGray(before, g) {
		t.Error("a dark scene with a true-black edge was stretched; the step must be a no-op")
	}
}

func TestNormalizeLumaRangeStretchesFlatMidGrey(t *testing.T) {
	// A flat mid-grey with no full-range signature anywhere: the step cannot
	// tell it from a studio-range flat, so it takes the documented studio
	// stretch — 60 → (60-16)*255/219 = 51.
	g := image.NewGray(image.Rect(0, 0, 32, 16))
	for y := 0; y < 16; y++ {
		for x := 0; x < 32; x++ {
			g.SetGray(x, y, color.Gray{Y: 60})
		}
	}
	normalizeLumaRange(g)
	if got := g.GrayAt(5, 5).Y; got != 51 {
		t.Errorf("mid-grey 60 = %d, want 51 after the studio stretch", got)
	}
}

func TestPreprocessRangeNormMakesStudioMatchFull(t *testing.T) {
	// The behavioural claim: whatever range the converter delivers, OCR sees
	// the same binarised picture. A studio-range crop stretched first is
	// pixel-identical, after the full recipe, to its full-range twin.
	studio := Preprocess(studioCrop(), DefaultSteps())
	full := Preprocess(fullCrop(), DefaultSteps())
	if studio.Bounds() != full.Bounds() {
		t.Fatalf("bounds %v != %v", studio.Bounds(), full.Bounds())
	}
	if !equalGray(studio, full) {
		t.Errorf("the recipe's output differs between a studio-range and a full-range delivery of the same picture:\nstudio %v\nfull   %v",
			hist(studio), hist(full))
	}
	// And the stretch is what made them equal: without the step the studio
	// crop's 16/235 content inverts to 239/20, not 255/0 — compared without
	// Otsu, because binarisation would wash the range difference away.
	noNorm := Preprocess(studioCrop(), Steps{AutoInvert: true, Scale: 2, Filter: FilterNearest})
	fullNN := Preprocess(fullCrop(), Steps{AutoInvert: true, Scale: 2, Filter: FilterNearest})
	if equalGray(noNorm, fullNN) {
		t.Error("without RangeNorm the studio and full deliveries agree — the test cannot distinguish the step")
	}
}

func TestStepsHashCoversRangeNorm(t *testing.T) {
	a := DefaultSteps()
	b := DefaultSteps()
	b.RangeNorm = false
	if a.Hash() == b.Hash() {
		t.Error("Steps.Hash does not distinguish RangeNorm on/off")
	}
}

func copyGray(g *image.Gray) *image.Gray {
	out := image.NewGray(g.Bounds())
	for y := g.Rect.Min.Y; y < g.Rect.Max.Y; y++ {
		for x := g.Rect.Min.X; x < g.Rect.Max.X; x++ {
			out.SetGray(x, y, g.GrayAt(x, y))
		}
	}
	return out
}

func equalGray(a, b *image.Gray) bool {
	if a.Bounds() != b.Bounds() {
		return false
	}
	for y := a.Rect.Min.Y; y < a.Rect.Max.Y; y++ {
		for x := a.Rect.Min.X; x < a.Rect.Max.X; x++ {
			if a.GrayAt(x, y) != b.GrayAt(x, y) {
				return false
			}
		}
	}
	return true
}

func hist(g *image.Gray) [256]int {
	var h [256]int
	for y := g.Rect.Min.Y; y < g.Rect.Max.Y; y++ {
		for x := g.Rect.Min.X; x < g.Rect.Max.X; x++ {
			h[g.GrayAt(x, y).Y]++
		}
	}
	return h
}
