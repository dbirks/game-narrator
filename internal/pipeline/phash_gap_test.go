package pipeline_test

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/vision"
)

// TestPhashToleranceSitsInTheMeasuredGap pins the measurement the perceptual
// skip's tolerance and settle are tuned against (ADR-0115 §3): on the
// deterministic corpus the perceptual distance separates the three regimes the
// ROI stage has to tell apart — a stable hold (0 bits: the skip's target), a
// one-word typewriter growth (0–7 bits: inside the tolerance, which is why the
// skip is settle-gated and must not eat word-sized changes), and a page change
// (far past the tolerance: a new line always re-reads). If a future
// perceptual-hash change moves these numbers, the tolerance (default 10) and
// the settle (default 3) may need retuning with them — this test is where the
// retuning is measured from.
func TestPhashToleranceSitsInTheMeasuredGap(t *testing.T) {
	dir := corpusDir(t)
	rows := readTruth(t)
	hashes := make([]uint64, len(rows))
	for i, r := range rows {
		if !r.Present {
			continue
		}
		img := decodeFrame(t, dir, r.File)
		crop := vision.CropNormalized(img, vision.Rect{X: r.Rect.X, Y: r.Rect.Y, W: r.Rect.W, H: r.Rect.H}, vision.CropOptions{})
		h, ok := vision.PerceptualHash(crop)
		if !ok {
			t.Fatalf("%s: no perceptual hash", r.File)
		}
		hashes[i] = h
	}

	var (
		growths  []int // text grows by a word between these frames
		holds    []int // identical text between these frames
		pageJump int   // the page-two crop vs the page-one hold
		got      bool
	)
	for i := 1; i < len(rows); i++ {
		if rows[i].Text == "" || rows[i-1].Text == "" || hashes[i] == 0 || hashes[i-1] == 0 {
			continue
		}
		d := vision.PerceptualDistance(hashes[i-1], hashes[i])
		switch {
		case rows[i].Text == rows[i-1].Text:
			holds = append(holds, d)
		case isGrowth(rows[i-1].Text, rows[i].Text):
			growths = append(growths, d)
		default:
			// A non-growth, non-hold text change on the corpus is the page turn.
			if d > pageJump {
				pageJump = d
			}
		}
		got = true
	}
	if !got || len(holds) == 0 || len(growths) == 0 || pageJump == 0 {
		t.Fatalf("measurement incomplete (holds=%d growths=%d page=%d): the corpus must show a hold, word growths and a page turn", len(holds), len(growths), pageJump)
	}
	// The hold is still: every stable frame is perceptually identical.
	for i, d := range holds {
		if d != 0 {
			t.Fatalf("hold frame %d moved %d bits, want 0 (the skip's target is a perceptually frozen crop)", i, d)
		}
	}
	// Word growths are the settle gate's reason to exist: at least one sits
	// inside the tolerance (tolerance alone cannot see it), and the ones that
	// sit past it still re-read — so the corpus proves both halves of the gate's
	// job. The assertion is the lower bound's existence, not an exact value: if
	// the hash recipe changes and every growth clears the tolerance, the settle
	// has nothing left to protect and this test says so.
	tol := vision.DefaultPerceptualTolerance
	minGrowth := growths[0]
	for _, d := range growths {
		if d < minGrowth {
			minGrowth = d
		}
	}
	if minGrowth > tol {
		t.Fatalf("every word growth cleared the tolerance (min %d > %d): the settle gate protects nothing on this corpus — retune the ADR's claim or the hash", minGrowth, tol)
	}
	if pageJump <= tol {
		t.Fatalf("page change moved %d bits, inside the tolerance %d: a new line would ride the old read — the skip's whole premise is gone", pageJump, tol)
	}
	t.Logf("measured: holds %v (want 0), word growths %v (min %d; want ≤ %d), page %d (want > %d)", holds, growths, minGrowth, tol, pageJump, tol)
}

// isGrowth reports whether the later text is the earlier text with more words
// on (a typewriter step), the corpus's one growth shape.
func isGrowth(prev, next string) bool {
	if len(next) <= len(prev) {
		return false
	}
	return next[:len(prev)] == prev
}
