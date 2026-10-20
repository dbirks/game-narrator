package vision

import (
	"image"
	"image/draw"
)

// LetterboxMode selects whether a crop maps its rect onto the whole frame or
// onto the picture inside the frame after black bars are removed.
type LetterboxMode uint8

const (
	// LetterboxNone maps the rect onto the full frame: correct when the capture
	// already carries a 4:3 picture, or when a profile was calibrated against
	// this exact framebuffer and knows the bars are part of its coordinates.
	LetterboxNone LetterboxMode = iota
	// LetterboxDetect scans the frame for black bars first, so a rect authored
	// against the game's own picture lands on the picture even when the capture
	// device padded it to 16:9 (docs/spec/04: "Coordinates must adapt to 4:3 vs
	// 16:9 stretch, letterboxing, overscan"). With a Tracker set, the window
	// comes from that tracker's hysteresised state instead of a fresh scan per
	// crop — see CropOptions.Tracker.
	LetterboxDetect
)

// CropOptions carries the per-crop switches docs/spec/04 puts on a region's
// detector. Only the letterbox rule is implemented today; masks and overscan
// compensation attach here when the region editor needs them, which is why the
// options are a struct and not a bool.
type CropOptions struct {
	Letterbox LetterboxMode
	// Tracker, when non-nil and Letterbox is LetterboxDetect, supplies the
	// active window from hysteresised, stateful detection (Tracker.Update /
	// Tracker.Box) instead of a fresh stateless ActiveArea scan per crop. The
	// caller owns the update cadence: call Update once per frame on the frame
	// loop; CropNormalized only reads Box, so every region's crop for a frame
	// maps the same window, and a fade or black frame cannot move the crop.
	// With a nil Tracker the historic stateless behaviour applies, and tests
	// that call CropNormalized directly are unaffected.
	Tracker *Tracker
}

// BarLuma is the luma below which a pixel counts as part of a black bar. 16 is
// the bottom of legal video range (16–235 in 8-bit studio range), so a bar that
// is crushed to 0–15 reads as a bar while a dark-but-detailed picture edge does
// not.
const BarLuma = 16

// barCoverage is the share of a scanned line that must be below BarLuma for the
// line to count as bar. 0.98 tolerates a couple of noisy or dithered pixels at a
// bar edge without mistaking a dark letterbox shadow for content.
const barCoverage = 0.98

// minWideAspect is the frame aspect above which a black-bar scan is worth
// running. 4:3 content arrives in a 4:3 frame far more often than a 4:3 frame
// carries bars, and scanning a 4:3 frame risks eating a legitimately dark top
// and bottom of the scene. Above this aspect the frame is wide enough that bars
// are the likely explanation for a black border.
const minWideAspect = 1.4

// maxTrimShare caps how far the detected active area may sit from the frame
// edge. A frame that is 60% black on each side is not a pillarboxed picture; it
// is a black frame, and cropping to its "active area" would amplify noise into
// the OCR input.
const maxTrimShare = 0.35

// scanStride limits how many pixels per scanned line the bar detector reads.
// Detection runs per frame at 5–10 Hz (docs/spec/05 "Timings"), so it samples
// instead of touching every pixel; the 98% coverage rule is a ratio test and a
// few hundred samples estimate it well.
const scanStrideTarget = 512

// CropNormalized returns the sub-image of img covered by r, whose coordinates
// are fractions of the active picture. The result is a copy: the caller may keep
// it after the source frame is recycled (docs/spec/05 "immutable scene
// observations").
//
// It never panics. A nil or empty image, a degenerate rect, or a rect that ends
// up outside the picture yields an empty image — check image.Rectangle.Empty on
// Bounds() — so a mis-calibrated region degrades into "read no text" instead of
// taking the recognition loop down.
// activeWindowFor reports the active window a crop maps against for the
// given options: the tracker's hysteresed window once it has settled one,
// a fresh ActiveArea scan before that, the whole frame when the scan finds
// no bar-framed picture (or letterbox detection is off). It is the single
// spelling CropNormalized and CropRect both map through, so the cut and the
// box a caller draws for it cannot disagree.
func activeWindowFor(img image.Image, bounds image.Rectangle, opts CropOptions) image.Rectangle {
	active := bounds
	if opts.Letterbox == LetterboxDetect {
		if opts.Tracker != nil {
			// The tracker's window is the hysteresised answer; before the first
			// window has settled it degrades to a fresh scan, then to the whole
			// frame, so a black start reads no text rather than a wrong window.
			if a, ok := opts.Tracker.Box(); ok {
				active = a
			} else if a, ok := ActiveArea(img); ok {
				active = a
			}
		} else if a, ok := ActiveArea(img); ok {
			active = a
		}
	}
	return active
}

func CropNormalized(img image.Image, r Rect, opts CropOptions) image.Image {
	if img == nil {
		return empty()
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return empty()
	}
	active := activeWindowFor(img, bounds, opts)
	box := MapRect(active, r.clamped())
	if box.Empty() {
		return empty()
	}
	return crop(img, box)
}

// CropRect reports the frame-coordinate box CropNormalized would cut for
// (img, r, opts) — the same active window and MapRect as the cut itself, so
// a caller that draws the region's box (the developer inspector, the signal
// route's last dialogue-region crop) draws exactly what the runtime crops.
// The crop image CropNormalized returns is rebased to its own origin, so
// its bounds alone cannot carry the position: this is the position half.
// It reports an empty box for a nil/empty frame or a region that clamps to
// nothing — "no cut" stays "no box", never a whole-frame guess.
func CropRect(img image.Image, r Rect, opts CropOptions) image.Rectangle {
	if img == nil {
		return image.Rectangle{}
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return image.Rectangle{}
	}
	box := MapRect(activeWindowFor(img, bounds, opts), r.clamped())
	if box.Empty() {
		return image.Rectangle{}
	}
	return box
}

// MapRect projects a normalized rect onto pixel coordinates of an active area.
// Exported because the developer inspector has to draw the same box the runtime
// crops (docs/spec/09 "region calibration") and because the letterbox math is
// the part of this package worth testing in isolation.
func MapRect(active image.Rectangle, r Rect) image.Rectangle {
	if active.Dx() <= 0 || active.Dy() <= 0 {
		return image.Rectangle{}
	}
	rc := r.clamped()
	x0 := rc.X * float64(active.Dx())
	y0 := rc.Y * float64(active.Dy())
	x1 := (rc.X + rc.W) * float64(active.Dx())
	y1 := (rc.Y + rc.H) * float64(active.Dy())
	// Round both edges rather than floor-and-ceil a size: the rounded edges keep
	// the box centred, while floor/ceil would grow a 1px sliver of neighbouring
	// background on two sides at every resolution.
	min := image.Point{
		X: active.Min.X + roundInt(x0),
		Y: active.Min.Y + roundInt(y0),
	}
	max := image.Point{
		X: active.Min.X + roundInt(x1),
		Y: active.Min.Y + roundInt(y1),
	}
	box := image.Rectangle{Min: min, Max: max}.Intersect(active)
	if box.Dx() < 1 || box.Dy() < 1 {
		return image.Rectangle{}
	}
	return box
}

// ActiveArea finds the picture inside a frame by scanning the outermost rows and
// columns for black bars. ok is false when the frame is not wide enough to be a
// letterboxed/pillarboxed picture or when no bar was found, in which case the
// caller uses the whole frame.
//
// The result is the bar-free box, not a forced 4:3: a capture whose active
// picture is 4:3 comes out 4:3, and one that is not does not get silently
// stretched. Forcing an aspect here would invent pixels the game never drew.
func ActiveArea(img image.Image) (area image.Rectangle, ok bool) {
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return image.Rectangle{}, false
	}
	if float64(b.Dx())/float64(b.Dy()) <= minWideAspect {
		return b, false
	}
	cx := stride(b.Dx())
	cy := stride(b.Dy())
	left, right := trimHorizontal(img, b, cx, cy)
	top, bottom := trimVertical(img, b, cx, cy)
	if left == 0 && right == 0 && top == 0 && bottom == 0 {
		return b, false
	}
	area = image.Rectangle{
		Min: image.Point{X: b.Min.X + left, Y: b.Min.Y + top},
		Max: image.Point{X: b.Max.X - right, Y: b.Max.Y - bottom},
	}
	if area.Dx() < 1 || area.Dy() < 1 {
		return b, false
	}
	// Reject a "detection" that ate most of the frame — see maxTrimShare.
	if float64(area.Dx()) < float64(b.Dx())*(1-2*maxTrimShare) ||
		float64(area.Dy()) < float64(b.Dy())*(1-2*maxTrimShare) {
		return b, false
	}
	return area, true
}

// trimHorizontal returns the black-bar widths at the left and right edges.
func trimHorizontal(img image.Image, b image.Rectangle, cx, cy int) (left, right int) {
	limit := b.Dx() / 2
	for x := 0; x < limit; x += cx {
		if !blackColumn(img, b, x, cy) {
			break
		}
		left = x + cx
	}
	for x := b.Dx() - 1; x > b.Dx()-1-limit; x -= cx {
		if !blackColumn(img, b, x, cy) {
			break
		}
		right = b.Dx() - x
	}
	return left, right
}

// trimVertical returns the black-bar heights at the top and bottom edges.
func trimVertical(img image.Image, b image.Rectangle, cx, cy int) (top, bottom int) {
	limit := b.Dy() / 2
	for y := 0; y < limit; y += cy {
		if !blackRow(img, b, y, cx) {
			break
		}
		top = y + cy
	}
	for y := b.Dy() - 1; y > b.Dy()-1-limit; y -= cy {
		if !blackRow(img, b, y, cx) {
			break
		}
		bottom = b.Dy() - y
	}
	return top, bottom
}

func blackColumn(img image.Image, b image.Rectangle, x, step int) bool {
	seen, black := 0, 0
	for y := b.Min.Y; y < b.Max.Y; y += step {
		seen++
		if lumaAt(img, b.Min.X+x, y) < BarLuma {
			black++
		}
	}
	return seen > 0 && float64(black)/float64(seen) >= barCoverage
}

func blackRow(img image.Image, b image.Rectangle, y, step int) bool {
	seen, black := 0, 0
	for x := b.Min.X; x < b.Max.X; x += step {
		seen++
		if lumaAt(img, x, b.Min.Y+y) < BarLuma {
			black++
		}
	}
	return seen > 0 && float64(black)/float64(seen) >= barCoverage
}

// stride picks the sample step for one axis so a line costs at most about
// scanStrideTarget samples.
func stride(n int) int {
	s := n / scanStrideTarget
	if s < 1 {
		return 1
	}
	return s
}

func roundInt(v float64) int {
	if v < 0 {
		return -int(-v + 0.5)
	}
	return int(v + 0.5)
}

// crop copies box out of img into a fresh image that always starts at (0,0),
// keeping the source color model where the stdlib has one. The copy is the
// point: image.SubImage would alias the caller's frame buffer, and a scene
// observation that changes when the next frame is decoded breaks both
// docs/spec/05's "immutable scene observations" and every hash taken over it
// (docs/spec/12 "repeated identical frames").
//
// Colour survives the crop on purpose. The OCR path wants luma, but the same box
// is what a VLM fallback and the developer inspector display (docs/spec/05 "can
// send screenshot crop plus recent context").
func crop(img image.Image, box image.Rectangle) image.Image {
	box = box.Intersect(img.Bounds())
	if box.Empty() {
		return empty()
	}
	size := image.Point{X: box.Dx(), Y: box.Dy()}
	zr := image.Rectangle{Max: size}
	var dst draw.Image
	switch src := img.(type) {
	case *image.Gray:
		dst = image.NewGray(zr)
	case *image.Gray16:
		dst = image.NewGray16(zr)
	case *image.NRGBA:
		dst = image.NewNRGBA(zr)
	case *image.RGBA:
		dst = image.NewRGBA(zr)
	case *image.RGBA64:
		dst = image.NewRGBA64(zr)
	case *image.CMYK:
		dst = image.NewCMYK(zr)
	case *image.Paletted:
		dst = image.NewPaletted(zr, src.Palette)
	default:
		dst = image.NewRGBA(zr)
	}
	draw.Draw(dst, zr, img, box.Min, draw.Src)
	return dst
}

// empty returns the value every degenerate path yields.
func empty() image.Image {
	return image.NewGray(image.Rectangle{})
}
