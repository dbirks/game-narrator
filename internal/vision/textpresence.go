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
