package vision

import (
	"image"
	"image/color"
	"testing"
)

// lcg is a tiny deterministic PRNG: the noise must be reproducible in a unit
// test, and the global math/rand seed would make a flake non-reproducible.
type lcg struct{ s uint64 }

func newLcg(seed uint64) *lcg { return &lcg{s: seed} }

func (r *lcg) intn(n int) int {
	r.s = r.s*6364136223846793005 + 1442695040888963407
	return int((r.s >> 33) % uint64(n))
}

func jitterPix(v uint8, jitter int, rng *lcg) uint8 {
	d := rng.intn(2*jitter+1) - jitter
	if d < 0 {
		return v - uint8(-d)
	}
	n := int(v) + d
	if n > 255 {
		return 255
	}
	return uint8(n)
}

// textboxCrop builds a textbox-shaped crop the way a captured feed presents
// it: a light 3px border (the box edge OCR reads as noise glyphs), a dark
// panel, and glyph bars. layout picks a *different line*: 0 puts the ink in
// two wide bars across the left, 1 in three narrow bars across the right —
// the way one page of text differs from the next. Every frame of an analog
// feed differs from its neighbour by a few luma levels of jitter, which the
// test injects.
func textboxCrop(layout, jitter int, rng *lcg) *image.NRGBA {
	w, h := 200, 60
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for yy := 0; yy < h; yy++ {
		for xx := 0; xx < w; xx++ {
			v := uint8(30) // the panel
			switch layout {
			case 0:
				// Two wide bars, left-weighted.
				if xx >= 20 && xx < 100 && yy >= 14 && yy < 28 {
					v = 220
				}
				if xx >= 30 && xx < 130 && yy >= 34 && yy < 48 {
					v = 200
				}
			case 1:
				// A different line: bars across the width at different rows —
				// the way one page of text differs from the next, so a page
				// turn moves many cells of the grid at once.
				if xx >= 60 && xx < 170 && yy >= 10 && yy < 22 {
					v = 215
				}
				if xx >= 90 && xx < 150 && yy >= 28 && yy < 42 {
					v = 230
				}
				if xx >= 20 && xx < 110 && yy >= 44 && yy < 54 {
					v = 195
				}
			}
			// The border: a light frame edge the crop includes.
			if xx < 3 || xx >= w-3 || yy < 3 || yy >= h-3 {
				v = 240
			}
			if jitter > 0 {
				v = jitterPix(v, jitter, rng)
			}
			img.Set(xx, yy, color.NRGBA{R: v, G: v, B: v})
		}
	}
	return img
}

func TestPerceptualHashNoiseStable(t *testing.T) {
	// The same line, jittered frame after frame: every frame must stay
	// within the documented tolerance of its neighbour, or the perceptual
	// dedupe re-OCRs a stable box on a noisy feed — the exact live failure
	// ADR-0115 §3 fixes.
	var prev uint64
	maxDist := 0
	for i := 0; i < 40; i++ {
		img := textboxCrop(0, 4, newLcg(uint64(1000+i)))
		h, ok := PerceptualHash(img)
		if !ok {
			t.Fatalf("frame %d: no hash", i)
		}
		if i > 0 {
			d := PerceptualDistance(h, prev)
			if d > maxDist {
				maxDist = d
			}
			if d > DefaultPerceptualTolerance {
				t.Fatalf("frame %d: noise moved the hash %d bits, over the tolerance %d: the dedupe would re-read a stable box", i, d, DefaultPerceptualTolerance)
			}
		}
		prev = h
	}
	// The deadband makes a stable box's hash deterministic, so the whole run
	// should sit far inside the tolerance — the margin is the evidence.
	if maxDist > 2 {
		t.Logf("note: noise moved the hash up to %d bits (tolerance %d); expect ~0 with the deadband", maxDist, DefaultPerceptualTolerance)
	}
}

func TestPerceptualHashNewLineMovesFar(t *testing.T) {
	base, ok0 := PerceptualHash(textboxCrop(0, 0, newLcg(1)))
	if !ok0 {
		t.Fatal("base line: no hash")
	}
	// A new line: different ink. It must land far outside the tolerance, or
	// a page turn rides the old read and the narrator speaks the previous
	// page.
	other, ok1 := PerceptualHash(textboxCrop(1, 0, newLcg(2)))
	if !ok1 {
		t.Fatal("other line: no hash")
	}
	if d := PerceptualDistance(base, other); d <= DefaultPerceptualTolerance {
		t.Fatalf("a new line moved the hash only %d bits, inside the tolerance %d: the dedupe would miss the page turn", d, DefaultPerceptualTolerance)
	}
}

func TestPerceptualHashEmpty(t *testing.T) {
	if _, ok := PerceptualHash(nil); ok {
		t.Fatal("nil image reported a hash")
	}
	if _, ok := PerceptualHash(image.NewNRGBA(image.Rectangle{})); ok {
		t.Fatal("empty image reported a hash")
	}
	// Distance is total and symmetric.
	if PerceptualDistance(0, 0) != 0 || PerceptualDistance(0xFFFF_FFFF_FFFF_FFFF, 0) != 64 {
		t.Fatal("distance endpoints wrong")
	}
	if PerceptualDistance(1, 2) != PerceptualDistance(2, 1) {
		t.Fatal("distance not symmetric")
	}
}
