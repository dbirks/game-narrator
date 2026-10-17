package dolphin

import (
	"image"
	"math"
)

// The frame predicates, ported from the spike's ImageMagick one-liners into
// pure Go so the readiness gates need no external tool and the thresholds
// stay the measured ones (ADR-0007, docs/spec/11 "Avoiding false positives"):
//
//   - entropy01: normalised Shannon entropy of the luma histogram (ImageMagick
//     "%[entropy]", 0..1). Black/blank window ≈ 0; a rendered scene ≫ 0.1 —
//     the spike measured the bare GUI at 0.169 and a game frame at 0.277.
//   - diffFraction: the fraction of (strided) luma pixels that differ by more
//     than a small tolerance — the Go form of "magick compare -metric AE"
//     over a pixel count. It is the "differs from the baseline" and "two
//     successive frames differ" half of the boot predicate.
//   - rmse01: normalised root-mean-square difference of two luma pictures at
//     a common size (the spike's scene classifier compared 320x240 crops).
//   - letterBand01: the mean luma of the lower of the top/bottom bands. The
//     title screen fills the frame (0.33-0.60 measured); the opening movie is
//     letterboxed (≈0.000) — camera-motion immune and semantically clean.
//
// Every function is total: a nil or mismatched image yields a documented
// zero/NaN rather than a panic, because a predicate that crashes on a torn
// frame would kill the readiness loop instead of reporting it.

// luma extracts 8-bit luma (BT.601) from any image.Image.
func luma(img image.Image) (w, h int, px []uint8) {
	if img == nil {
		return 0, 0, nil
	}
	r := img.Bounds()
	w, h = r.Dx(), r.Dy()
	px = make([]uint8, w*h)
	i := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			rr, gg, bb, _ := img.At(x, y).RGBA()
			// 16-bit premultiplied inputs to 8-bit luma: Y = 0.299R + 0.587G +
			// 0.114B. Integer coefficients 76/150/30 sum to 256, so >>8 maps
			// pure white (255,255,255) to 255 and black to 0 (the full 8-bit
			// range the predicate thresholds were calibrated on).
			px[i] = uint8((76*int(rr>>8) + 150*int(gg>>8) + 30*int(bb>>8)) >> 8)
			i++
		}
	}
	return w, h, px
}

// entropy01 is the normalised (÷8) Shannon entropy of the luma histogram —
// the same quantity ImageMagick's "%[entropy]" reports, so the 0.1 threshold
// is directly comparable to the spike's measured numbers.
func entropy01(img image.Image) float64 {
	_, _, px := luma(img)
	if len(px) == 0 {
		return 0
	}
	var hist [256]uint64
	for _, v := range px {
		hist[v]++
	}
	total := float64(len(px))
	h := 0.0
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		h -= p * math.Log2(p)
	}
	return h / 8.0
}

// diffTol is the luma tolerance below which a pixel "did not change": a small
// nonzero margin keeps JPEG-free PNG grabs from reporting 1/256 dither noise
// as motion, while any real render difference stays far above it.
const diffTol = 16

// diffFraction is the fraction of pixels that differ by more than diffTol,
// sampled on a 2x2 stride (four times cheaper than the full pixel count; a
// scene change moves whole regions, so the stride cannot hide it). Sizes are
// compared by intersection so a window that resized between captures reports
// the overlap, not a false 1.0.
func diffFraction(a, b image.Image) float64 {
	ra, rb := bounds(a), bounds(b)
	min := intersect(ra, rb)
	if min.Dx() <= 0 || min.Dy() <= 0 {
		return 1.0 // no overlap at all is the strongest possible "changed"
	}
	_, _, pa := luma(a)
	_, _, pb := luma(b)
	if pa == nil || pb == nil {
		return 0
	}
	total, diff := 0, 0
	for y := min.Min.Y; y < min.Max.Y; y += 2 {
		ay := (y - ra.Min.Y) * ra.Dx()
		by := (y - rb.Min.Y) * rb.Dx()
		for x := min.Min.X; x < min.Max.X; x += 2 {
			av := pa[ay+(x-ra.Min.X)]
			bv := pb[by+(x-rb.Min.X)]
			total++
			if d := int(av) - int(bv); d > diffTol || d < -diffTol {
				diff++
			}
		}
	}
	if total == 0 {
		return 0
	}
	return float64(diff) / float64(total)
}

// rmse01 resizes both pictures to size (nearest neighbour) and returns the
// normalised (÷255) luma RMSE — 0.0 identical, 1.0 maximally different. The
// spike's scene classifier compared 320x240 crops with exactly this metric.
func rmse01(a, b image.Image, w, h int) float64 {
	_, _, pa := luma(a)
	_, _, pb := luma(b)
	if pa == nil || pb == nil || w <= 0 || h <= 0 {
		return 0
	}
	ra, rb := a.Bounds(), b.Bounds()
	if ra.Dx() <= 0 || rb.Dx() <= 0 {
		return 0
	}
	var sum float64
	n := 0
	for y := 0; y < h; y++ {
		sy := y * ra.Dy() / h
		sy2 := y * rb.Dy() / h
		for x := 0; x < w; x++ {
			ax := x * ra.Dx() / w
			bx := x * rb.Dx() / w
			av := int(pa[sy*ra.Dx()+ax])
			bv := int(pb[sy2*rb.Dx()+bx])
			d := av - bv
			sum += float64(d * d)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(sum/float64(n)) / 255.0
}

// letterBand01 is the mean luma of the lower of the top and bottom lbRows
// bands of the image, normalised 0..1. (A scene that is letterboxed shows
// black there; a full-frame scene shows sky/sea — the spike's measured
// 0.33-0.60 vs 0.000 split.)
const lbRows = 30

func letterBand01(img image.Image) float64 {
	w, h, px := luma(img)
	if px == nil || h <= 0 {
		return 0
	}
	rows := lbRows
	if rows > h/2 {
		rows = h / 2
	}
	if rows <= 0 {
		return 0
	}
	var top, bottom uint64
	for y := 0; y < rows; y++ {
		base := y * w
		for x := 0; x < w; x++ {
			top += uint64(px[base+x])
		}
		base = (h - 1 - y) * w
		for x := 0; x < w; x++ {
			bottom += uint64(px[base+x])
		}
	}
	mean := func(v uint64) float64 {
		return float64(v) / (float64(rows) * float64(w) * 255.0)
	}
	t, b := mean(top), mean(bottom)
	if t < b {
		return t
	}
	return b
}

type rect = image.Rectangle

// cropRegion returns img's subimage at window region r (the spike's CROP: the
// render-window rect inside the root shot), or img unchanged when the region
// does not intersect the shot (no window yet). The result keeps the region's
// bounds, so the diffFraction intersection logic compares the same area on
// both sides.
func cropRegion(img image.Image, r Region) image.Image {
	if img == nil {
		return nil
	}
	cr := img.Bounds().Intersect(image.Rect(r.X, r.Y, r.X+r.W, r.Y+r.H))
	if cr.Empty() {
		return img
	}
	// SubImagers (PNG-decoded images, *image.RGBA) keep one bitmap, so the
	// subimage is a zero-copy view with the region's bounds; anything else
	// gets a direct pixel copy.
	if si, ok := img.(interface {
		SubImage(image.Rectangle) image.Image
	}); ok {
		if sub := si.SubImage(cr); sub != nil {
			return sub
		}
	}
	out := image.NewRGBA(cr)
	for y := cr.Min.Y; y < cr.Max.Y; y++ {
		for x := cr.Min.X; x < cr.Max.X; x++ {
			out.Set(x, y, img.At(x, y))
		}
	}
	return out
}

func bounds(img image.Image) rect {
	if img == nil {
		return rect{}
	}
	return img.Bounds()
}

func intersect(a, b rect) rect {
	return a.Intersect(b)
}
