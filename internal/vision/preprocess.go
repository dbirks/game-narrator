package vision

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/color"
)

// Preprocessing is deliberately integer-only and stdlib-only. It runs on every
// relevant frame at 2–5 Hz on a four-core ARM appliance (docs/spec/05
// "Timings"), and its output is what an OCR cache key is derived from, so a
// float round-off that differed between the dev host and the device would be a
// cache-thrashing, benchmark-invalidating bug. Every step here is deterministic
// for a given input, and Steps.Hash makes that choice visible to the caller.

// ScaleFilter picks how the crop is enlarged before thresholding. docs/spec/05
// asks for "scale 2-4× with candidate filters"; both available filters are
// exposed because which one wins is font-dependent, and the answer is a fixture
// measurement rather than an assumption.
type ScaleFilter uint8

const (
	// FilterNearest keeps glyph edges hard. It is the right default for the 2×
	// enlargement of an already-aliased 25px textbox font: no new grey
	// half-tones for the threshold to argue about.
	FilterNearest ScaleFilter = iota
	// FilterBilinear smooths edges. It helps when the source is already hard and
	// jaggy (an integer-stretched 4:3 signal, a bitmap font).
	FilterBilinear
)

// Steps is one preprocessing recipe. It is a value, so a profile can store one
// in regions.ocr_settings, the runtime can log the one it used (docs/spec/05
// "log chosen transform"), and a cache key can name it.
type Steps struct {
	// AutoInvert flips the crop when it is light text on a dark box. The
	// decision comes from the median luma, not a fixed rule, because profiles
	// ship both textbox styles and docs/spec/12 lists "negative contrast" as a
	// perception case; Tesseract reads dark-on-light far more reliably.
	AutoInvert bool
	// RangeNorm normalises the crop's luma range before anything else sees it
	// (see normalizeLumaRange): a converter that hands the card studio-range
	// (16–235) pixels in a stream the demuxer declares full-range arrives with
	// blacks at 16 and whites at 235, and every downstream step — the invert
	// median, Otsu, the threshold — works on a compressed histogram that reads
	// as low contrast. The step is a no-op on a full-range picture, which is
	// the common case (docs/spec/12 "colour range" hardening).
	RangeNorm bool
	// Scale is the integer enlargement, 1 for none. Values above 4 are clamped:
	// OCR cost grows with area and docs/spec/05 caps the useful range.
	Scale int
	// Filter is the enlargement filter; ignored when Scale < 2.
	Filter ScaleFilter
	// Otsu binarises with the threshold Otsu's method picks from the crop's own
	// histogram — the "threshold adaptively" step, and the reason one recipe
	// covers a textbox over a bright sky and one over a dark cave.
	Otsu bool
	// Median3x3 removes isolated speckles after binarisation. It is what keeps a
	// noisy, near-uniform box (docs/spec/12 "black/loading screen", an empty
	// textbox, a thin decorative border) from becoming confetti that a
	// binariser confidently reads as characters.
	Median3x3 bool
	// Pad adds a light border in pixels. Tesseract's page segmentation behaves
	// better with margin; docs/spec/05 lists "pad border" in the pipeline.
	Pad int
}

// DefaultSteps is the recipe the recognition loop starts with, chosen by
// measuring the fixtures in testdata/synthetic/textbox — see
// TestSyntheticFixturesOCR for the numbers. A profile overrides it; nothing here
// overrides silently.
func DefaultSteps() Steps {
	return Steps{AutoInvert: true, RangeNorm: true, Scale: 2, Filter: FilterNearest, Otsu: true, Median3x3: true, Pad: 8}
}

// Clamp bounds the recipe. Scale and Pad become positive and within the ranges
// the cost model assumes, so a mistyped or hostile profile cannot ask the frame
// loop for a 64× enlargement of a 4K crop (docs/spec/13 "backpressure").
func (s Steps) Clamp() Steps {
	if s.Scale < 1 {
		s.Scale = 1
	}
	if s.Scale > 4 {
		s.Scale = 4
	}
	if s.Pad < 0 {
		s.Pad = 0
	}
	if s.Pad > 64 {
		s.Pad = 64
	}
	if s.Filter > FilterBilinear {
		s.Filter = FilterNearest
	}
	return s
}

// Hash is a stable identifier for this recipe, for cache keys and for the
// decision log. It hashes a canonical rendering of the fields rather than Go
// struct memory layout or a JSON encoding, so the value survives a field
// reorder and does not depend on reflection: a recipe that means the same thing
// hashes the same on every host and every build.
func (s Steps) Hash() string {
	s = s.Clamp()
	name := "nearest"
	if s.Filter == FilterBilinear {
		name = "bilinear"
	}
	canonical := "gn-pre-v1|" + boolByte(s.AutoInvert) + "|range=" + boolByte(s.RangeNorm) +
		"|scale=" + itoa(s.Scale) + "|" + name + "|" + boolByte(s.Otsu) + "|" + boolByte(s.Median3x3) +
		"|pad=" + itoa(s.Pad)
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])
}

func boolByte(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var d [20]byte
	i := len(d)
	for v > 0 {
		i--
		d[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		d[i] = '-'
	}
	return string(d[i:])
}

// Preprocess converts to luma and then applies the recipe in the order
// docs/spec/05 lists (scale, threshold, cleanup, pad). The invert decision is
// taken on the gray crop *before* enlargement or thresholding, because both
// change the histogram the decision depends on.
//
// Any color model and any Rectangle origin may be passed in. The result is
// always a *image.Gray with origin (0,0), so the bytes that reach a hash and the
// PNG encoder are unambiguous.
func Preprocess(img image.Image, s Steps) *image.Gray {
	s = s.Clamp()
	g := ToGray(img)
	if g.Rect.Empty() {
		return g
	}
	if s.RangeNorm {
		normalizeLumaRange(g)
	}
	if s.AutoInvert && ShouldInvert(g) {
		invert(g)
	}
	if s.Scale > 1 {
		switch s.Filter {
		case FilterBilinear:
			g = upscaleBilinear(g, s.Scale)
		default:
			g = upscaleNearest(g, s.Scale)
		}
	}
	if s.Otsu {
		// The guard is not decoration. Otsu always returns *a* threshold, and on a
		// crop with no text there is nothing for it to separate — it splits the
		// background's own noise histogram in half and hands the engine confetti.
		// The committed empty-textbox fixture proved it: binarised, that frame read
		// as "!" at conf 0.71, a fabricated line in a game that said nothing.
		if th := Otsu(g); th.Trustworthy() {
			applyThreshold(g, th.Value)
		}
	}
	if s.Median3x3 {
		median3x3(g)
	}
	if s.Pad > 0 {
		g = pad(g, s.Pad)
	}
	return g
}

// ImageHash is the region hash docs/spec/05 wants for "suppress duplicate OCR
// calls on unchanged region hash", and the dedupe key for the OCR result cache.
// It covers the dimensions and the raw Gray pixel bytes in raster order: not the
// color model, not the stride, not metadata. Two frames that look identical to
// the OCR input therefore share a key even when one arrived as YCbCr from the
// capture card and the other as RGBA from a recorded fixture — which is the
// property a cache needs, and the reason the conversion happens here rather than
// in the caller.
func ImageHash(img image.Image) string {
	g := ToGray(img)
	h := sha256.New()
	var head [8]byte
	binary.LittleEndian.PutUint32(head[0:4], uint32(g.Rect.Dx()))
	binary.LittleEndian.PutUint32(head[4:8], uint32(g.Rect.Dy()))
	h.Write(head[:])
	for y := 0; y < g.Rect.Dy(); y++ {
		h.Write(rowPix(g, y))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// rowPix returns one row's bytes. Stride-aware, because a SubImage of a wider
// buffer must hash as the same picture as a freshly allocated copy of it.
func rowPix(g *image.Gray, y int) []byte {
	off := (y - g.Rect.Min.Y) * g.Stride
	return g.Pix[off : off+g.Rect.Dx()]
}

// lumaAt samples one pixel's luma in the image's own coordinate space, returning
// 0 (black) for a coordinate outside its bounds — the safe answer for bar
// detection, which is asking "is this edge dark?".
//
// The Gray, NRGBA and RGBA fast paths matter because ActiveArea runs on every
// frame at 5–10 Hz (docs/spec/05 "Timings"), where the generic At() path costs
// an interface call and a color conversion per sample.
func lumaAt(img image.Image, x, y int) uint8 {
	switch src := img.(type) {
	case *image.Gray:
		if x < src.Rect.Min.X || x >= src.Rect.Max.X || y < src.Rect.Min.Y || y >= src.Rect.Max.Y {
			return 0
		}
		return src.Pix[(y-src.Rect.Min.Y)*src.Stride+(x-src.Rect.Min.X)]
	case *image.NRGBA:
		if x < src.Rect.Min.X || x >= src.Rect.Max.X || y < src.Rect.Min.Y || y >= src.Rect.Max.Y {
			return 0
		}
		i := (y-src.Rect.Min.Y)*src.Stride + (x-src.Rect.Min.X)*4
		return luma8(src.Pix[i], src.Pix[i+1], src.Pix[i+2])
	case *image.RGBA:
		// Premultiplied, so a translucent pixel needs un-premultiplying before
		// weighting. Bar detection samples a few hundred pixels per line; the
		// general path is correct and the cost is not the frame budget.
		return lumaOf(img.At(x, y))
	default:
		return lumaOf(img.At(x, y))
	}
}

// Histogram returns the 256-bin luma distribution of a gray image. It backs both
// OtsuThreshold and ShouldInvert, and is exported because the inspector shows the
// histogram of a crop when a region is mis-calibrated (docs/spec/09).
func Histogram(g *image.Gray) (bins [256]int) {
	for y := 0; y < g.Rect.Dy(); y++ {
		for _, v := range rowPix(g, y) {
			bins[v]++
		}
	}
	return bins
}

// MedianLuma reports the 50th percentile of the luma distribution, normalized to
// [0,1]. Median, not mean: a textbox is a large flat area with a few bright
// glyph pixels, so the mean drifts with text density while the median stays on
// the box — which is exactly what the invert decision should key on. An empty
// image reports 1 (light): no pixels is not evidence of a dark box.
func MedianLuma(g *image.Gray) float64 {
	bins := Histogram(g)
	total := g.Rect.Dx() * g.Rect.Dy()
	if total == 0 {
		return 1
	}
	half, seen := total/2, 0
	for v := 0; v < 256; v++ {
		seen += bins[v]
		if seen > half {
			return float64(v) / 255
		}
	}
	return 1
}

// InvertThreshold is the median luma below which AutoInvert flips the crop. 0.5
// is the honest midpoint: a dark box with light text sits near 0.1, a light
// parchment box near 0.9, and a box with no box at all (bare subtitles over a
// scene) is decided by the scene, which is as close to intent as a fixed rule
// gets.
const InvertThreshold = 0.5

// ShouldInvert reports whether AutoInvert would flip this gray image. Exported
// so a test, a log line and a profile-validation screen can state the decision
// rather than reverse-engineer it from output.
func ShouldInvert(g *image.Gray) bool { return MedianLuma(g) < InvertThreshold }

// Studio luma range (BT.601 8-bit "limited"): 16..235. A picture that lives
// only in this window has the studio offset baked in: if the capture chain
// declared it full-range, its blacks sit at 16 and whites at 235, and OCR
// sees a 19.5% contrast loss it cannot recover. The normalisation maps the
// canonical studio window onto the full range (16→0, 235→255, linear inside)
// and clips outside it.
const (
	studioLow  = 16
	studioHigh = 235
)

// rangeNormLo/Hi bound the "this looks studio-range" test, in terms of the
// first/last histogram bin carrying at least 1% of the crop's pixels. A
// full-range picture has (nearly) something at 0 or 255 — a true black bar,
// a white text edge — and so does not fire; a studio-range picture has
// nothing below ~14 and nothing above ~238, and does.
const (
	rangeNormLo  = 14
	rangeNormHi  = 238
	rangeNormPct = 100 // per-cent: a bin must hold ≥ 1% of the pixels to count
)

// normalizeLumaRange stretches the crop from the studio window to the full
// range when the crop's luma content says it is studio-range. It is a no-op
// otherwise, on purpose: stretching a full-range picture (a dark cave scene
// whose lowest bin is 8, a bright sky whose highest is 250) would invent
// contrast the game never drew, and a no-op is the answer the pipeline can
// defend. It runs in place before scaling and thresholding, so every later
// step works on the same histogram OCR will see.
func normalizeLumaRange(g *image.Gray) {
	var bins [256]int
	total := 0
	for y := g.Rect.Min.Y; y < g.Rect.Max.Y; y++ {
		p := rowPix(g, y)
		for i := range p {
			bins[p[i]]++
			total++
		}
	}
	if total == 0 {
		return
	}
	lo := 0
	for lo < 256 && bins[lo]*rangeNormPct < total {
		lo++
	}
	hi := 255
	for hi > 0 && bins[hi]*rangeNormPct < total {
		hi--
	}
	if lo < rangeNormLo || hi > rangeNormHi {
		return // full-range signature: leave the picture alone
	}
	// Linear map of [16,235] onto [0,255]: out = (v-16)*255/219.
	// Built once as a table; the per-pixel pass is one table read.
	var lut [256]uint8
	for v := range lut {
		m := (v - studioLow) * 255 / (studioHigh - studioLow)
		switch {
		case m < 0:
		case m > 255:
			m = 255
		default:
			lut[v] = uint8(m)
		}
	}
	for y := g.Rect.Min.Y; y < g.Rect.Max.Y; y++ {
		p := rowPix(g, y)
		for i := range p {
			p[i] = lut[p[i]]
		}
	}
}

// Threshold is the outcome of Otsu's method plus the two statistics that say
// whether the split means anything.
type Threshold struct {
	// Value is the selected level: pixels above it become white, at or below it
	// black.
	Value uint8
	// Separation is the distance between the two class means in luma levels. Real
	// glyph-on-box text measures 100+; the same engine on a textless crop measures
	// single digits, because there the "classes" are halves of one noise
	// distribution rather than ink and paper.
	Separation float64
	// Minority is the fraction of pixels in the smaller of the two classes. Text
	// is sparse — a textbox page is a few percent ink — so a split whose smaller
	// class is half the crop has found texture, not writing. It is reported as the
	// minority rather than "dark" because on a dark textbox the dark class *is*
	// the paper.
	Minority float64
}

// MinSeparation is the class-mean distance below which a binarisation is
// discarded. 32 luma levels is an eighth of the range: well below any
// dark-box/light-text or light-box/dark-text pair (150–230 on the committed
// fixtures) and well above a quantised gradient's own spread (3–15).
const MinSeparation = 32

// MaxMinorityShare is the smaller-class fraction above which a binarisation is
// discarded. docs/spec/12's "outlined text over animated background" is the case
// that pushes real text toward this bound; measured fixture values are 3–9 %.
const MaxMinorityShare = 0.45

// Trustworthy reports whether the split should be applied at all.
func (th Threshold) Trustworthy() bool {
	return th.Separation >= MinSeparation && th.Minority <= MaxMinorityShare
}

// Otsu runs Otsu's method — threshold selection from a histogram by maximizing
// between-class variance — over a gray image, and reports the split together with
// its separation and ink share. Ties go to the lower threshold, which favours
// keeping ink; the loop is a fixed 0..255 sweep, so the answer never depends on
// map iteration order or on summation order.
func Otsu(g *image.Gray) Threshold {
	bins := Histogram(g)
	total := g.Rect.Dx() * g.Rect.Dy()
	if total == 0 {
		return Threshold{Value: 128}
	}
	var sum, sumB, wB, wF float64
	for v := 0; v < 256; v++ {
		sum += float64(v) * float64(bins[v])
	}
	bestVar, best, bestSep, bestMinor := -1.0, 128.0, 0.0, 0.0
	for t := 0; t < 256; t++ {
		wB += float64(bins[t])
		if wB == 0 {
			continue
		}
		wF = float64(total) - wB
		if wF == 0 {
			break
		}
		sumB += float64(t) * float64(bins[t])
		mB := sumB / wB
		mF := (sum - sumB) / wF
		d := mB - mF
		if between := wB * wF * d * d; between > bestVar {
			minor := wB
			if wF < minor {
				minor = wF
			}
			bestVar, best, bestSep, bestMinor = between, float64(t), mF-mB, minor/float64(total)
		}
	}
	return Threshold{Value: uint8(best), Separation: bestSep, Minority: bestMinor}
}

// OtsuThreshold reports just the selected level, for callers that want the number
// without the judgement — the inspector shows it next to the histogram.
func OtsuThreshold(g *image.Gray) uint8 { return Otsu(g).Value }

// ToGray converts any image to luma with BT.601 weights (0.299 R, 0.587 G,
// 0.114 B) in fixed point. 601 rather than 709 because the product reads a
// definition-standard game signal (docs/spec/10: 480i/576i capture), whose
// chroma-to-luma weighting is 601. A grayscale source is a copy, a YCbCr source
// is a direct read of its luma plane, and the output origin is always (0,0).
//
// Alpha is un-premultiplied before weighting, so a translucent overlay converts
// to the luma a viewer would see rather than to a darkened version of it.
func ToGray(img image.Image) *image.Gray {
	if img == nil {
		return image.NewGray(image.Rectangle{})
	}
	b := img.Bounds()
	out := image.NewGray(image.Rectangle{Max: image.Point{X: b.Dx(), Y: b.Dy()}})
	switch src := img.(type) {
	case *image.Gray:
		for y := 0; y < out.Rect.Dy(); y++ {
			copy(rowPix(out, y), rowPix(src, src.Rect.Min.Y+y))
		}
		return out
	case *image.YCbCr:
		// The Y plane is full resolution at YStride for every subsample ratio;
		// only Cb/Cr are subsampled, and luma does not read them.
		for y := b.Min.Y; y < b.Max.Y; y++ {
			dst := out.Pix[y*out.Stride : y*out.Stride+out.Rect.Dx()]
			for x := b.Min.X; x < b.Max.X; x++ {
				dst[x-b.Min.X] = src.Y[(y-b.Min.Y)*src.YStride+(x-b.Min.X)]
			}
		}
		return out
	case *image.NRGBA:
		// NRGBA is NOT alpha-premultiplied, so the weighting is direct.
		for y := b.Min.Y; y < b.Max.Y; y++ {
			ys := (y - b.Min.Y) * src.Stride
			dst := out.Pix[y*out.Stride : y*out.Stride+out.Rect.Dx()]
			for x := b.Min.X; x < b.Max.X; x++ {
				xs := (x - b.Min.X) * 4
				dst[x-b.Min.X] = luma8(src.Pix[ys+xs], src.Pix[ys+xs+1], src.Pix[ys+xs+2])
			}
		}
		return out
	default:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			dst := out.Pix[y*out.Stride : y*out.Stride+out.Rect.Dx()]
			for x := b.Min.X; x < b.Max.X; x++ {
				dst[x-b.Min.X] = lumaOf(img.At(x, y))
			}
		}
		return out
	}
}

// DarkShare is the fraction of the luma pixels below luma. It is the pixel
// walk behind the pipeline's textbox-presence detector (a dialogue box is a
// translucent panel darker than the scene, docs/spec/05): the pipeline's ROI
// stage and the one-shot reader both ask the same question of a crop, so the
// walk lives here, where ToGray already lives, and both call it rather than
// each carrying a copy. It walks Pix directly — a 640×480 textbox crop is
// ~300 k pixels and At() would make that a method call each.
func DarkShare(g *image.Gray, luma uint8) float64 {
	if g == nil || len(g.Pix) == 0 {
		return 0
	}
	dark := 0
	for _, v := range g.Pix {
		if v < luma {
			dark++
		}
	}
	return float64(dark) / float64(len(g.Pix))
}

// lumaOf weights 16-bit channels after un-premultiplying alpha, which is what
// color.Color.RGBA() delivers (premultiplied, 16-bit).
func lumaOf(c color.Color) uint8 {
	r, g, b, a := c.RGBA()
	if a == 0 {
		return 0
	}
	if a != 0xffff {
		// Un-premultiply. a>0 here, so the divide is safe; uint64 because r*0xffff
		// overflows a 32-bit int, and the ARM build must not differ from x86.
		r = clamp16(int(uint64(r) * 0xffff / uint64(a)))
		g = clamp16(int(uint64(g) * 0xffff / uint64(a)))
		b = clamp16(int(uint64(b) * 0xffff / uint64(a)))
	}
	return luma16(r, g, b)
}

func clamp16(v int) uint32 {
	if v < 0 {
		return 0
	}
	if v > 0xffff {
		return 0xffff
	}
	return uint32(v)
}

// luma16 maps 16-bit channels to 8-bit luma with the 601 coefficients scaled by
// 65536: 0.299·65536=19595, 0.587·65536=38470, 0.114·65536=7471 — which sum to
// 65536 exactly, so white stays white.
func luma16(r, g, b uint32) uint8 {
	y := (19595*r + 38470*g + 7471*b + 32768) >> 16
	if y += 128; y > 0xffff { // pure white rounds up to 256 in 8-bit terms
		return 255
	}
	return uint8(y >> 8)
}

// luma8 is the same weighting on 8-bit channels, widened to 16-bit intermediates
// rather than approximated with 8-bit coefficients.
func luma8(r, g, b uint8) uint8 {
	return luma16(uint32(r)*257, uint32(g)*257, uint32(b)*257)
}

func invert(g *image.Gray) {
	for y := 0; y < g.Rect.Dy(); y++ {
		row := rowPix(g, y)
		for i := range row {
			row[i] = 255 - row[i]
		}
	}
}

func applyThreshold(g *image.Gray, t uint8) {
	for y := 0; y < g.Rect.Dy(); y++ {
		row := rowPix(g, y)
		for i := range row {
			if row[i] > t {
				row[i] = 255
			} else {
				row[i] = 0
			}
		}
	}
}

func upscaleNearest(g *image.Gray, scale int) *image.Gray {
	sw, sh := g.Rect.Dx(), g.Rect.Dy()
	out := image.NewGray(image.Rectangle{Max: image.Point{X: sw * scale, Y: sh * scale}})
	for y := 0; y < sh; y++ {
		row := rowPix(g, y)
		for dy := 0; dy < scale; dy++ {
			orow := out.Pix[(y*scale+dy)*out.Stride : (y*scale+dy)*out.Stride+out.Rect.Dx()]
			j := 0
			for _, v := range row {
				for k := 0; k < scale; k++ {
					orow[j] = v
					j++
				}
			}
		}
	}
	return out
}

// upscaleBilinear interpolates in 16.16 fixed point, separably
// (horizontal pass, then vertical). Fixed point is the point: the result is
// exact and bit-identical on every architecture, which float interpolation is
// not, and the hash of a preprocessed region must be stable across the dev host
// and the ARM appliance (docs/spec/10 "framebuffer size change").
func upscaleBilinear(g *image.Gray, scale int) *image.Gray {
	sw, sh := g.Rect.Dx(), g.Rect.Dy()
	dw, dh := sw*scale, sh*scale
	tmp := image.NewGray(image.Rectangle{Max: image.Point{X: dw, Y: sh}})
	for y := 0; y < sh; y++ {
		row := rowPix(g, y)
		orow := tmp.Pix[y*tmp.Stride : y*tmp.Stride+dw]
		for x := 0; x < dw; x++ {
			a, b, f := lerpIndex(x, sw, scale)
			orow[x] = uint8((uint32(row[a])*(0xffff-f) + uint32(row[b])*f + 0x8000) >> 16)
		}
	}
	out := image.NewGray(image.Rectangle{Max: image.Point{X: dw, Y: dh}})
	for y := 0; y < dh; y++ {
		a, b, f := lerpIndex(y, sh, scale)
		rowA := tmp.Pix[a*tmp.Stride : a*tmp.Stride+dw]
		rowB := tmp.Pix[b*tmp.Stride : b*tmp.Stride+dw]
		orow := out.Pix[y*out.Stride : y*out.Stride+dw]
		for x := 0; x < dw; x++ {
			orow[x] = uint8((uint32(rowA[x])*(0xffff-f) + uint32(rowB[x])*f + 0x8000) >> 16)
		}
	}
	return out
}

// lerpIndex maps a destination index to its two source neighbours and the 16.16
// interpolation weight between them. The source coordinate of a destination pixel
// centre is (i+0.5)/scale - 0.5: the two half-pixel offsets are not the same
// quantity, and subtracting a whole pixel instead of 0.5 shifts the whole
// enlargement by half a source pixel, which reads as a soft, undersized crop.
func lerpIndex(i, srcN, scale int) (i0, i1 int, f uint32) {
	pos := ((2*i+1)<<16)/(2*scale) - (1 << 15)
	if pos < 0 {
		pos = 0
	}
	i0 = pos >> 16
	f = uint32(pos) & 0xffff
	if i0 >= srcN-1 {
		return srcN - 1, srcN - 1, 0
	}
	return i0, i0 + 1, f
}

// median3x3 is the "morphological cleanup" step of docs/spec/05 in its cheapest
// useful form: a 3×3 median suppresses salt-and-pepper speckles and thin
// decorative borders while keeping glyph stems, and on a binary image it is a
// majority vote. Borders clamp (replicate) rather than assume white, so a stem
// at the crop edge is not shaved off. Input must be zero-origin, which is what
// Preprocess guarantees after ToGray.
func median3x3(g *image.Gray) {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	src := make([]uint8, len(g.Pix))
	copy(src, g.Pix)
	at := func(x, y int) uint8 {
		if x < 0 {
			x = 0
		} else if x >= w {
			x = w - 1
		}
		if y < 0 {
			y = 0
		} else if y >= h {
			y = h - 1
		}
		return src[y*g.Stride+x]
	}
	var win [9]uint8
	for y := 0; y < h; y++ {
		orow := rowPix(g, y)
		for x := 0; x < w; x++ {
			i := 0
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					win[i] = at(x+dx, y+dy)
					i++
				}
			}
			insertionSort9(&win)
			orow[x] = win[4]
		}
	}
}

// insertionSort9 sorts the fixed 9-value window; sorting nine numbers is cheaper
// than selecting the median and keeps the function obviously correct.
func insertionSort9(a *[9]uint8) {
	for i := 1; i < 9; i++ {
		v := a[i]
		j := i - 1
		for j >= 0 && a[j] > v {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = v
	}
}

// pad surrounds the image with white, the background Tesseract expects.
// docs/spec/05 lists "pad border" because page segmentation on a glyph touching
// the image edge is measurably worse than on one with margin.
func pad(g *image.Gray, p int) *image.Gray {
	w, h := g.Rect.Dx(), g.Rect.Dy()
	out := image.NewGray(image.Rectangle{Max: image.Point{X: w + 2*p, Y: h + 2*p}})
	for i := range out.Pix {
		out.Pix[i] = 255
	}
	for y := 0; y < h; y++ {
		copy(rowPix(out, y+p)[p:p+w], rowPix(g, y))
	}
	return out
}
