// truthocr.go is the scripted OCR engine the harness feeds the pipeline in
// synthetic mode: the clip's own truth.json answers each read. It exists
// because the M7 scenarios are about the *faults and the fences*, not about
// OCR quality — the quality gate for real Tesseract on this corpus is the
// M2 replay test (pipeline.TestSyntheticFrameReplayRealTesseract), and
// running it here again would make every scenario a flaky OCR test.
//
// The engine identifies a read by the image hash the pipeline computes
// (vision.ImageHash of the preprocessed crop — the exact bytes the engine is
// handed), so a read maps to the truth row of the frame those pixels came
// from regardless of delivery order or dropped frames. Identical frames share
// a hash and therefore a truth row, which is exactly what "unchanged pixels,
// same text" means. This is a synthetic stub — the scenario's
// origin:generated_synthetic label and its S-mode catalog row are the
// provenance record (docs/spec/11 "Fixture origins").

package scenario

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// truthEngine answers Recognize from a clip's truth rows.
type truthEngine struct {
	byHash map[string]ocr.Result
}

// newTruthEngine builds the hash→truth map. It crops and preprocesses every
// frame with the same recipe the pipeline uses (vision.DefaultSteps, the
// zero Crop options), so the hashes match the pipeline's dedupe keys.
//
// It indexes every region the profile declares, not just the first: the
// pipeline analyses all regions and dispatches a read for any region that
// shows presence, so a read for a non-first region (the box, which shows
// presence during the transitions that follow the last subtitle) must resolve
// to a truth row rather than a "no truth row for crop hash" harness defect.
// A truth row carries one text — the frame's primary text, which sits in the
// session's primary region (regions[0]) — so the primary region's reads answer
// with it and the other regions' reads answer empty: that matches the game,
// where a line is on screen in one region, not every one, and stops a
// non-primary region from echoing the line and making the pipeline commit it
// twice. The session asserts only on regions[0], so the other regions' answers
// only need to exist so their reads stop degrading the replay. Regions are
// indexed with regions[0] last so that, if two regions' crops ever hash
// identically, the primary region's answer wins.
func newTruthEngine(framesDir string, profile *Profile, truth []Truth) (*truthEngine, error) {
	regs := profile.ToRegions()
	if len(regs) == 0 {
		return nil, fmt.Errorf("truth engine: the profile declares no region")
	}
	e := &truthEngine{byHash: make(map[string]ocr.Result, len(truth)*len(regs))}
	ordered := make([]games.Region, 0, len(regs))
	if len(regs) > 1 {
		ordered = append(ordered, regs[1:]...)
	}
	ordered = append(ordered, regs[0])
	for _, row := range truth {
		img, err := readPNG(filepath.Join(framesDir, row.File))
		if err != nil {
			return nil, fmt.Errorf("truth engine: read %s: %w", row.File, err)
		}
		for _, reg := range ordered {
			crop := vision.CropNormalized(img, vision.RectOf(reg), vision.CropOptions{})
			if crop == nil || crop.Bounds().Empty() {
				// The region covers nothing at this resolution: the pipeline
				// reads it as present-but-empty, and so do we.
				continue
			}
			in := vision.Preprocess(crop, vision.DefaultSteps())
			// Only the primary region answers with the row's text; the others
			// answer empty so a line is never committed from a region it is not in.
			if reg.ID == regs[0].ID {
				e.byHash[vision.ImageHash(in)] = truthOCR(row.Text, row.Full)
			} else {
				e.byHash[vision.ImageHash(in)] = truthOCR("", false)
			}
		}
	}
	if len(e.byHash) == 0 {
		return nil, fmt.Errorf("truth engine: no frame produced a crop for any region")
	}
	return e, nil
}

// Recognize answers one read by the image's hash. The pipeline hands the
// engine its already-preprocessed crop (roi.analyze preps and hashes the
// same bytes), so the hash runs on the input as-is: preprocessing again
// would run AutoInvert twice and flip the image back to the un-inverted
// namespace, and every read would miss the truth map. A hash no truth row
// produced means the pipeline handed us pixels the clip does not contain —
// a harness defect, not a game state — and is reported as one.
func (e *truthEngine) Recognize(_ context.Context, img image.Image) (ocr.Result, error) {
	h := vision.ImageHash(img)
	if res, ok := e.byHash[h]; ok {
		return res, nil
	}
	return ocr.Result{}, fmt.Errorf("truth engine: no truth row for crop hash %s (harness defect)", h)
}

// Name reports the engine identity for the OCR provenance field.
func (e *truthEngine) Name() string { return "truth" }

// readPNG reads one frame from the clip.
func readPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

// truthOCR shapes one truth row the way a real engine's result arrives:
// text, mean confidence and one line when text is present. Full-line reads
// carry the measured Tesseract confidence the M2 sidecar uses (0.91);
// partial (typewriter) reads read lower (0.85), the way truncated prefixes
// do.
func truthOCR(text string, full bool) ocr.Result {
	conf := 0.0
	if text != "" {
		conf = 0.85
		if full {
			conf = 0.91
		}
	}
	res := ocr.Result{Text: text, Engine: "truth", MeanConf: conf}
	if text != "" {
		res.Lines = []ocr.Line{{Text: text, Conf: conf}}
	}
	return res
}
