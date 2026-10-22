package runtime

// The adjudication's screenshot (ADR-0154): the full frame rides on every
// Clef request, so the model decides from what the player sees — a box at a
// placement no region covers, or a crop the OCR misread, is still read from
// the picture. Measured 2026-10-22 against clef-omni through the gateway: with
// NO OCR text, the full frame alone picked the right line out of 15 options
// (6 of them sharing words with the truth) on a capture-card top box and a
// Dolphin bottom box, and answered "no box" on a sailing frame — 6/6 across
// two runs, ~0.6–0.95 s warm.

import (
	"bytes"
	"image"
	"image/jpeg"
)

// decisionFrameMaxWidth bounds the screenshot's width: a 640×480 capture is
// sent as-is (~30–40 KB at the quality below); a larger source is
// downscaled, keeping the request well under the 4 MB image cap and the
// upload off the decision budget's critical path.
const decisionFrameMaxWidth = 960

// decisionFrameQuality is the JPEG quality: dialogue glyphs stay legible.
const decisionFrameQuality = 85

// encodeDecisionFrame renders the frame as the request's JPEG, nil when there
// is no frame or it cannot be encoded (the request then goes text-only, the
// historic shape).
func encodeDecisionFrame(img image.Image) []byte {
	if img == nil || img.Bounds().Empty() {
		return nil
	}
	if w := img.Bounds().Dx(); w > decisionFrameMaxWidth {
		img = downscaleNearest(img, decisionFrameMaxWidth, img.Bounds().Dy()*decisionFrameMaxWidth/w)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: decisionFrameQuality}); err != nil {
		return nil
	}
	return buf.Bytes()
}

// downscaleNearest is a nearest-neighbour resize: cheap, and enough for a
// model that reads the box at a fraction of the source resolution.
func downscaleNearest(src image.Image, w, h int) image.Image {
	if w <= 0 || h <= 0 {
		return src
	}
	b := src.Bounds()
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			dst.Set(x, y, src.At(b.Min.X+x*b.Dx()/w, sy))
		}
	}
	return dst
}
