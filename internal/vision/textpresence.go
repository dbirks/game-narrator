package vision

import "image"

// TextPresence is the cheap, no-OCR answer to "does this crop carry readable
// text on a panel, and how much?" (ADR-0137). It is the pixel walk the
// pipeline's placement-aware textbox selection scores its candidate bands
// with: one luma pass, the same order of work as DarkShare, against the ~600 ms
// OCR it steers — which is what keeps "do not OCR every band every frame"
// (docs/spec/05) intact.
//
// It reports three shares in [0,1]:
//
//   - dark: the fraction of luma pixels below darkLuma. A dialogue box is a
//     translucent panel darker than the scene, so the dark share is the panel
//     presence the textbox detector has always asked (DarkShare). On a dark
//     scene it is high on every band, so it is the presence gate, never the
//     selector.
//
//   - text: the fraction of bright pixels (luma above brightLuma) that sit
//     next to a dark pixel (luma below darkLuma): a bright stroke on a dark
//     panel, the letterforms themselves. A scene with no text has few bright
//     strokes riding a dark panel, so the text share is low; a band that
//     carries the line has many, so the text share is the selector's floor.
//     It is the signal that separates "there is a line" from "the scene is
//     dark".
//
//   - onPanel: the fraction of the text's bright-on-dark pixels that sit in
//     rows that are panel rows (a row whose own dark share reaches 0.5): the
//     letters ride the panel, not the scene. This is the backdrop signal the
//     band selection's box-likeness gate needs (ADR-0137 §Robustness): a text
//     strip floating over the scene — the llvmpipe GL-info banner Dolphin
//     draws into the top of the frame for the first seconds after a savestate
//     load, or any HUD caption — has its letters in scene rows, and its share
//     is low; the dialogue box's letters sit in the panel's rows, and its
//     share is ~1. Measured on the 2026-10-21 Dolphin probe run (banner band
//     0.80, box band 1.00) and the live-hdmi top-box capture (box band 1.00),
//     the gate sits at 0.9, between the two.
//
// The walk reads Pix directly (a 640×480 band is ~300 k pixels; At() would
// make it a method call each), strides by one pixel, and keeps a per-row dark
// count in the same pass so the on-panel share costs no second walk. A nil or
// empty crop reads as no text at all.
func TextPresence(g *image.Gray, darkLuma, brightLuma uint8) (dark, text, onPanel float64) {
	if g == nil {
		return 0, 0, 0
	}
	b := g.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 || len(g.Pix) == 0 {
		return 0, 0, 0
	}
	w := b.Dx()
	h := b.Dy()
	darkN, textN := 0, 0
	rowDark := make([]int32, h)
	rowText := make([]int32, h)
	for y := 0; y < h; y++ {
		row := g.Pix[y*g.Stride:]
		for x := 0; x < w; x++ {
			l := row[x]
			if l < darkLuma {
				darkN++
				rowDark[y]++
			}
			if l < brightLuma {
				continue
			}
			// A bright pixel is a text stroke only when it rides a dark panel:
			// one of its right or lower neighbours is panel-dark. Right and
			// lower (not left/up) so each stroke is counted once per edge, not
			// once per pixel, the way the cheap gradient walks count.
			darkNb := false
			if x+1 < w && row[x+1] < darkLuma {
				darkNb = true
			} else if y+1 < h && g.Pix[(y+1)*g.Stride+x] < darkLuma {
				darkNb = true
			}
			if darkNb {
				textN++
				rowText[y]++
			}
		}
	}
	n := float64(w * h)
	// The on-panel share: of the text's bright-on-dark pixels, the share that
	// sits in a panel row (a row that is majority panel-dark). A band with no
	// text reads as 1 (vacuously all of nothing is on the panel — the caller's
	// text floor is what keeps such a band from being a box).
	onN, onTot := 0, 0
	for y := 0; y < h; y++ {
		if rowDark[y]*2 < int32(w) { // the row is not a panel row (dark share < 0.5)
			continue
		}
		onN += int(rowText[y])
	}
	for y := 0; y < h; y++ {
		onTot += int(rowText[y])
	}
	op := 1.0
	if onTot > 0 {
		op = float64(onN) / float64(onTot)
	}
	return float64(darkN) / n, float64(textN) / n, op
}

// DefaultTextLuma is the bright-pixel threshold for TextPresence's text share:
// a letterform on a dark panel is bright, and 140 sits well above the panel
// (dark, < 90) and the anti-aliased edge while below the white of the glyph,
// the split measured on the live-hdmi capture (ADR-0137).
const DefaultTextLuma = 140

// PanelPresence is the presence gate's read of one raw colour crop (ADR-0137,
// Amendment: the sea false positive): the panel's darkness, the text riding
// it, and the panel's colour. The dark and text numbers are the same walk
// TextPresence runs; SaturatedDark is what the old gate could not see, and
// what let the open sea read as a panel.
type PanelPresence struct {
	// Dark is the panel share — the fraction of luma pixels below darkLuma —
	// the same walk DarkShare runs, the presence the textbox detector has
	// always asked.
	Dark float64
	// Text is the bright-on-dark stroke share (TextPresence's text): the
	// letterforms riding the panel, the signal that separates "there is a
	// line" from "the scene is dark".
	Text float64
	// SaturatedDark is the fraction of the crop's dark pixels that are
	// saturated (channel spread at or above satFloor, measured as
	// max(r,g)−min(r,g,b) by sat8 — the blue channel counts toward the
	// minimum, a pre-existing quirk the ADR-0137 sea measurement stands on,
	// left as-is by ADR-0169): the panel's
	// colour. A dialogue panel is a translucent darkening of the scene, so
	// its dark pixels are the panel's own desaturated tint. A saturated
	// scene — the open sea: dark blue water, measured 0.93–0.97 on the
	// 2026-10-21 board capture — fills the band with dark pixels that are
	// saturated, and a band like that is scene wearing through the band, not
	// a panel.
	SaturatedDark float64
	// TopDark is the mean dark-pixel share of the crop's top band (its top
	// ~1/8 of rows). A textbox region is a dark band that reaches the top
	// edge of the crop, so its top band is dark (near 1) regardless of the
	// text that rides lower down. An open-sea/sky band is bright at the top
	// (the sky) and darkens toward the bottom, so its top band is nearly 0
	// even when its overall dark share clears the floor (ADR-0169: the
	// 2026-10-23 open-sea frame read 0.062 on its board band while every box
	// read >= 0.78). A crop with no dark rows at all reads 0.
	TopDark float64
}

// DefaultSatFloor is the saturation (max(r,g,b)−min(r,g,b)) at or above which
// a dark pixel counts as saturated for PanelPresenceOf's SaturatedDark share:
// the sea's blue is far past it (measured mean dark-pixel saturation 92–96),
// while the panel's tint, the scene's shadows and their capture noise sit
// well under (measured 0–18 on the box fixtures).
const DefaultSatFloor = 80

// PanelPresenceOf measures one raw colour crop for the presence gate: the
// same luma walk TextPresence runs (dark and text, one conversion) plus one
// colour walk counting the dark pixels and the saturated dark ones — two
// DarkShare-order passes, still negligible against the ~600 ms OCR it steers
// (docs/spec/05). The caller keeps its floors in the pipeline's PresenceConfig
// (docs/spec/11 step 4: the detector's shape is measured here, its thresholds
// configured there). A nil or empty crop reads as no panel and no text at all.
func PanelPresenceOf(img image.Image, darkLuma, brightLuma, satFloor uint8) PanelPresence {
	if img == nil {
		return PanelPresence{}
	}
	b := img.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return PanelPresence{}
	}
	g := ToGray(img)
	dark, text, _ := TextPresence(g, darkLuma, brightLuma)
	darkN, satDarkN := saturatedDarkPixels(img, g, darkLuma, satFloor)
	sd := 0.0
	if darkN > 0 {
		sd = float64(satDarkN) / float64(darkN)
	}
	return PanelPresence{Dark: dark, Text: text, SaturatedDark: sd, TopDark: topBandDark(g, darkLuma)}
}

// topBandDark walks the crop's top ~1/8 of rows and reports their mean dark
// share — the darkness of the band's top edge. It reuses the already-
// converted gray g so it costs a partial luma pass, still negligible against
// the OCR the gate steers (docs/spec/05). An empty crop reads 0.
func topBandDark(g *image.Gray, darkLuma uint8) float64 {
	if g == nil {
		return 0
	}
	b := g.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || len(g.Pix) == 0 {
		return 0
	}
	n := h / 8 // top band
	if n < 1 {
		n = 1
	}
	sum := 0.0
	for y := 0; y < n; y++ {
		row := g.Pix[y*g.Stride : (y+1)*g.Stride]
		c := 0
		for x := 0; x < w; x++ {
			if row[x] < darkLuma {
				c++
			}
		}
		sum += float64(c) / float64(w)
	}
	return sum / float64(n)
}

// saturatedDarkPixels walks the colour source counting the crop's dark pixels
// — judged by the shared gray g, so the count matches TextPresence's dark
// exactly — and the saturated dark ones. NRGBA and YCbCr (the capture's two
// live shapes) are walked directly; any other source goes through At().
func saturatedDarkPixels(img image.Image, g *image.Gray, darkLuma, satFloor uint8) (int, int) {
	b := img.Bounds()
	var darkN, satDarkN int
	switch src := img.(type) {
	case *image.NRGBA:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			y0 := y - b.Min.Y
			ys := y0 * src.Stride
			gs := y0 * g.Stride
			for x := b.Min.X; x < b.Max.X; x++ {
				x0 := x - b.Min.X
				if g.Pix[gs+x0] >= darkLuma {
					continue
				}
				darkN++
				xs := x0 * 4
				if sat8(src.Pix[ys+xs], src.Pix[ys+xs+1], src.Pix[ys+xs+2]) >= satFloor {
					satDarkN++
				}
			}
		}
	case *image.YCbCr:
		// The Y plane is the luma g already took; Cb/Cr are subsampled at the
		// chroma stride, so a chroma address divides the pixel offset by the
		// plane ratio (CStride/YStride horizontally, chroma rows / luma rows
		// vertically: 4:4:4, 4:2:2 and 4:2:0 all fall out of the same math).
		crRows := len(src.Cb) / src.CStride
		for y := b.Min.Y; y < b.Max.Y; y++ {
			y0 := y - b.Min.Y
			yc := y0 * crRows / b.Dy()
			ycs := yc * src.CStride
			ys := y0 * src.YStride
			gs := y0 * g.Stride
			for x := b.Min.X; x < b.Max.X; x++ {
				x0 := x - b.Min.X
				if g.Pix[gs+x0] >= darkLuma {
					continue
				}
				darkN++
				xc := x0 * src.CStride / src.YStride
				cb := int(src.Cb[ycs+xc]) - 128
				crr := int(src.Cr[ycs+xc]) - 128
				yi := int(src.Y[ys+x0])
				// BT.601 reconstruction, JPEG's ×256 integer coefficients.
				r := yi + (359*crr+128)>>8         // 1.402
				gch := yi - (88*cb+183*crr+128)>>8 // 0.344, 0.714
				bl := yi + (454*cb+128)>>8         // 1.772
				if sat8(clamp8(r), clamp8(gch), clamp8(bl)) >= satFloor {
					satDarkN++
				}
			}
		}
	default:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			y0 := y - b.Min.Y
			gs := y0 * g.Stride
			for x := b.Min.X; x < b.Max.X; x++ {
				if g.Pix[gs+x-b.Min.X] >= darkLuma {
					continue
				}
				darkN++
				r16, g16, b16, a16 := src.At(x, y).RGBA()
				if a16 == 0 {
					continue // transparent: no colour to judge
				}
				if a16 != 0xffff {
					// Un-premultiply with lumaOf's arithmetic, so the colour and
					// the luma that judged the pixel dark are one number.
					r16 = clamp16(int(uint64(r16) * 0xffff / uint64(a16)))
					g16 = clamp16(int(uint64(g16) * 0xffff / uint64(a16)))
					b16 = clamp16(int(uint64(b16) * 0xffff / uint64(a16)))
				}
				if sat8(uint8(r16>>8), uint8(g16>>8), uint8(b16>>8)) >= satFloor {
					satDarkN++
				}
			}
		}
	}
	return darkN, satDarkN
}

// sat8 is the 8-bit pixel's saturation, max(r,g,b)−min(r,g,b): the cheap
// chroma distance that needs no colour space, the same rule the probe that
// measured the sea's false positive ran.
func sat8(r, g, b uint8) uint8 {
	mx, mn := r, b
	if g > mx {
		mx = g
	}
	if r < mn {
		mn = r
	}
	if g < mn {
		mn = g
	}
	if b < mn {
		mn = b
	}
	return mx - mn
}

// clamp8 bounds the BT.601 YCbCr→RGB reconstruction to [0,255].
func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}
