// Controlled-image OCR check. The image is synthetic — a white 400×100 canvas
// with "NARRATOR 42" in DejaVu Sans 48pt — so it carries no third-party
// copyright and can live in the repository (AGENTS.md rule 5). It is generated
// once with:
//
//	magick -size 400x100 xc:white -font DejaVu-Sans -pointsize 48 -fill black \
//	  -gravity center -annotate +0+0 "NARRATOR 42" internal/doctor/testdata/ocr-check.png
//
// and then used for both `ocr.controlled_image` (does the local OCR stack
// actually read pixels?) and the vision role probe (does the gateway model
// actually accept an image and read it?).
package doctor

import _ "embed"

//go:embed testdata/ocr-check.png
var ocrCheckPNG []byte

// controlledText is the string both the local OCR and the vision model must
// recover from the image.
const controlledText = "NARRATOR 42"

// ocrImage returns the controlled image bytes for a run.
func (r *runner) ocrImage() []byte {
	if len(r.opts.OCRImage) > 0 {
		return r.opts.OCRImage
	}
	return ocrCheckPNG
}
