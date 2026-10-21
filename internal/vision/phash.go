package vision

import (
	"image"
	"math/bits"
)

// PerceptualHash is the 64-bit dHash docs/spec/05 asks for as the tolerant
// half of "suppress duplicate OCR calls on unchanged region hash": a crop
// whose pixels moved by capture noise (an analog HDMI feed never delivers
// two identical frames) still hashes within a small Hamming distance, while
// a new line of text flips a majority of the bits. The exact hash
// (ImageHash) is the zero-tolerance half; this one is the tolerant one.
//
// dHash over aHash: the bits are *differences* between neighbouring grid
// pixels, not absolute levels. On a dark textbox — where every pixel is
// near the crop's own mean and an aHash therefore carries almost no
// contrast — the neighbour differences are still the glyph edges, which is
// exactly where a new line changes the picture and a noise jitter is not.
//
// The recipe is deterministic and stdlib-only, on the same footing as
// ImageHash: convert to 601 luma, downsample to a hashGridW×hashGridH grid,
// one bit per horizontal neighbour pair (bit i of row y is set when
// grid[y][x+1] > grid[y][x]). The downsample is a box average — every source
// pixel adds its luma to the grid cell it falls in, and the cell reads as the
// mean of its box. The averaging is what makes analog noise harmless: a
// 200×60 crop down to 9×8 puts ~165 source pixels per cell, so per-pixel
// jitter (±a few luma) moves a cell by a fraction of a level and almost
// never flips the sign of a neighbour difference, while a new line moves the
// ink across whole cells and flips the signs it crosses. Bit order is fixed
// (row-major, the left pixel first), so the value is comparable across hosts
// and builds the way the SHA-256 crop hash is.

const (
	// hashBits is the perceptual hash's bit count: 64, the natural word.
	hashBits = 64
	// hashGridW is the grid's width: one more than the bit count, because a
	// row of hashBits comparisons needs hashBits+1 samples.
	hashGridW = hashBits + 1
	// hashGridH is the grid's height: the rows compared, one bit-row each.
	hashGridH = hashBits / 8
)

// PerceptualHash returns the 64-bit dHash of img. ok is false for a nil or
// empty image, which has no percept to hash: the caller keeps its previous
// hash rather than comparing against zero (zero is a real answer for a
// picture that is uniformly its own left edge, and would otherwise read as
// "perceptually equal to" every other empty crop).
func PerceptualHash(img image.Image) (h uint64, ok bool) {
	if img == nil {
		return 0, false
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return 0, false
	}
	g := ToGray(img)
	w, hgt := g.Rect.Dx(), g.Rect.Dy()
	if w <= 0 || hgt <= 0 {
		return 0, false
	}
	// Box averaging: destination cell (x, y) reads the mean luma of the source
	// box [x*w/hashGridW, (x+1)*w/hashGridW) × [y*hgt/hashGridH,
	// (y+1)*hgt/hashGridH). Every source pixel adds its luma to the cell it
	// falls in — integer arithmetic throughout, so the answer is
	// bit-identical on every host the way ImageHash's is — and a cell with no
	// pixels (an impossible division, guarded by the w/h > 0 checks) reads 0.
	var sum [hashGridH][hashGridW]uint32
	var cnt [hashGridH][hashGridW]uint16
	for y := 0; y < hgt; y++ {
		sy := y * hashGridH / hgt
		row := g.Pix[y*g.Stride : y*g.Stride+w]
		for x := 0; x < w; x++ {
			sx := x * hashGridW / w
			sum[sy][sx] += uint32(row[x])
			cnt[sy][sx]++
		}
	}
	at := func(x, y int) uint8 {
		if cnt[y][x] == 0 {
			return 0
		}
		return uint8(sum[y][x] / uint32(cnt[y][x]))
	}
	var out uint64
	for y := 0; y < hashGridH; y++ {
		row := uint32(0)
		for x := 0; x < hashBits/8; x++ {
			// One row holds 8 comparison pairs, i.e. 9 samples.
			var bits uint32
			for i := 0; i < 8; i++ {
				// The deadband: a near-equal pair of cells (a flat region under
				// noise) reads 0 on every frame; only a real edge sets the bit.
				left := at(x*8+i, y)
				right := at(x*8+i+1, y)
				if int(right)-int(left) > hashDeadband {
					bits |= 1 << uint(i)
				}
			}
			row |= bits << uint(x*8)
		}
		out |= uint64(row) << uint(y*8)
	}
	return out, true
}

// PerceptualDistance is the Hamming distance between two perceptual hashes:
// the number of grid comparisons the two crops disagree on. Zero is
// "perceptually identical" in the dHash sense (the 64 neighbour relations
// agree) — not byte identity, which is ImageHash's job.
func PerceptualDistance(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

// hashDeadband is the comparison's dead zone, in luma levels, after the box
// average. Without it a flat region (the textbox's dark panel) carries
// neighbour differences of ~0 and every bit in it is a coin flip that
// per-pixel noise re-rolls each frame, so the hash would be as noisy as the
// feed. With the deadband a flat region reads a deterministic run of zeros on
// every frame, and only a real edge (a glyph crossing a cell boundary, tens of
// luma after the average) sets a bit; analog jitter would have to move a cell
// mean by the whole deadband to re-roll one. Four levels sits between the
// noise case (a cell mean moves by a fraction of a level under per-pixel
// jitter averaged over the cell's pixels) and the faintest real edge an OCR
// crop is worth keeping.
const hashDeadband = 4

// DefaultPerceptualTolerance is the pipeline's skip tolerance in bits
// (ADR-0115 §3): a crop within this distance of the crop the region last
// OCRed skips its OCR call. Measured on the 2026-10-20 board feed, analog
// HDMI noise shifts a stable textbox's 9×8 grid by single bits (each grid
// pixel averages ~25 source pixels, which absorbs the per-pixel jitter),
// while a new line or a box change flips a majority of the glyph-bearing
// grid — well past the tolerance. The value sits in the gap with a ~4×
// margin to the noise case and ~2× headroom under the line-change case,
// the same evidence-set reasoning fuzzyTextFloor applies in the matcher.
const DefaultPerceptualTolerance = 10
