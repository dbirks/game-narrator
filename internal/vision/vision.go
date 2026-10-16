// Package vision turns one captured frame into text: crop the region a profile
// says holds dialogue (docs/spec/04 "Visual profiles"), preprocess it for OCR,
// and hand the result to an engine (docs/spec/05 "OCR pipeline").
//
// The split is deliberate. Geometry and preprocessing are pure, local and
// stdlib-only in this package, because they are what a fixture replay must
// reproduce exactly (docs/spec/12 "Frame replay") and what a cache key is
// computed over. The engine lives in the ocr subpackage because it is a
// supervised subprocess per ADR-0012 and can fail, time out or be absent on a
// host. Metrics live in metrics because CER/WER is an evaluation concern that
// also runs over text that never came from an image.
//
// docs/spec/05's rules that bind this package:
//
//   - "Process cropped ROIs for CPU efficiency": nothing here touches a whole
//     frame when a region will do, and a crop is a copy, never a window onto a
//     caller-owned buffer that a later frame might overwrite.
//   - "Suppress duplicate OCR calls on unchanged region hash": ImageHash is the
//     key that makes that possible, so it is a content hash of luma pixels plus
//     dimensions, not a hash of a pointer or a timestamp.
//   - "Coordinates must adapt to 4:3 vs 16:9 stretch, letterboxing, overscan and
//     capture resolution" (docs/spec/04): a Rect is always a fraction of the
//     *active picture*, and CropNormalized with LetterboxDetect finds that
//     picture by scanning for the black bars around it.
package vision

import (
	"github.com/dbirks/game-narrator/internal/games"
)

// Rect is a rectangle in normalized [0,1] frame coordinates — the same
// convention as regions.rect_x … rect_y in docs/spec/02 and games.Region, which
// is why the profile inspector can drag a box at any capture resolution and
// store one number set. Fractions, not pixels: a 480i and a 1080p capture of the
// same game share a region.
type Rect struct {
	X, Y, W, H float64
}

// RectOf projects the geometry columns of a stored region. It exists so callers
// never re-derive the four fields at every use site and so the mapping from the
// database row to this package's geometry is one exported line rather than an
// implicit convention.
func RectOf(r games.Region) Rect {
	return Rect{X: r.RectX, Y: r.RectY, W: r.RectW, H: r.RectH}
}

// Empty reports whether the rect covers nothing after normalization, which is
// how a degenerate stored region arrives here rather than as a panic.
func (r Rect) Empty() bool { return !(r.W > 0 && r.H > 0) }

// clamped returns the rect with every coordinate finite, inside [0,1] and the
// box inside the unit square. NaN and negative sizes become empty rather than
// wild, because a region that cannot be interpreted should read no text at all —
// an OCR run over a garbage rect produces a confident hallucination, which is
// the worst possible outcome for the matcher downstream.
func (r Rect) clamped() Rect {
	x, y, w, h := finite01(r.X), finite01(r.Y), finite01(r.W), finite01(r.H)
	if !(w > 0) || !(h > 0) {
		return Rect{X: x, Y: y}
	}
	if x+w > 1 {
		w = 1 - x
	}
	if y+h > 1 {
		h = 1 - y
	}
	if w <= 0 || h <= 0 {
		return Rect{X: x, Y: y}
	}
	return Rect{X: x, Y: y, W: w, H: h}
}

func finite01(v float64) float64 {
	if !(v == v) || v < 0 { // NaN or negative; !(v==v) is NaN without importing math here
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
