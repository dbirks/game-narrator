package pipeline_test

// ADR-0172 (issue #53): the dynamic panel region end-to-end on the synthetic
// corpus. The corpus's box sits in the profile's calibrated band; the tests
// prove the finder's contract by moving the configured region OUT of the
// box's reach (a fixed non-textbox band at the top of the frame — the
// ADR-0137 candidate bands cannot cover the box then either) and asserting:
//
//   1. with the finder ON, both invented lines still commit, and the
//      observations that carried them came from the dynamic region
//      (RegionID "panel-anywhere" — the dynamic-placement mark);
//   2. with the finder OFF, nothing commits — the shifted region alone
//      reads nothing, which is the bug the finder fixes;
//   3. with the configured region in place (the box's own band) and the
//      finder ON, the fixed path is untouched: the lines commit through the
//      configured region, and the dynamic region never reports a present box
//      (the finder is gated off by the configured region's presence).
//
// The fake OCR is scripted on the finder's OWN rect: for each corpus frame
// with a box, the panel finder's rect crops that frame exactly the way the
// pipeline's dynamic region will (same downsample, same rect, same
// CropOptions zero, same recipe), so the scripted hash is the hash the
// pipeline computes. The fake thus contributes recognition of the dynamic
// region's crop only — all of the placement, presence and tracker logic is
// the pipeline's own.

import (
	"image"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/vision"
)

// panelRegionScript extends an OCR script with the finder-rect crops: for
// every corpus frame that shows a box, FindPanelsImage's rect is cropped
// with the pipeline's own (zero) CropOptions and the recipe's hash is
// mapped to that frame's truth text — the text the dynamic region must
// recite for the tracker to settle and commit.
func panelRegionScript(t *testing.T, base map[string]string) map[string]string {
	t.Helper()
	dir := corpusDir(t)
	cfg := pipeline.DefaultPanelFind()
	script := map[string]string{}
	for k, v := range base {
		script[k] = v
	}
	added := 0
	for _, r := range readTruth(t) {
		if !r.Present || r.Text == "" {
			continue
		}
		img := decodeFrame(t, dir, r.File)
		got := pipeline.FindPanelsImage(img, cfg)
		if len(got) == 0 {
			t.Fatalf("%s: the finder saw no panel on a frame whose box is present; the dynamic region could never read the corpus", r.File)
		}
		p := got[0]
		var crop image.Image
		crop = vision.CropNormalized(img, vision.Rect{X: p.X, Y: p.Y, W: p.W, H: p.H}, vision.CropOptions{})
		if crop == nil || crop.Bounds().Empty() {
			t.Fatalf("%s: finder rect %+v crops nothing", r.File, p)
		}
		script[vision.ImageHash(vision.Preprocess(crop, steps()))] = r.Text
		added++
	}
	if added == 0 {
		t.Fatalf("script added no panel crops")
	}
	return script
}

// shiftedTopRegion is the configured region moved out of the box's reach:
// a fixed band at the top of the frame with a non-placement-aware role, so
// neither its stored band nor ADR-0137 candidate bands can cover the box.
func shiftedTopRegion() games.Region {
	return games.Region{
		ID:         "topother",
		RegionType: games.RegionOther,
		RectX:      0.1,
		RectY:      0.02,
		RectW:      0.8,
		RectH:      0.1,
	}
}

// dynamicTraceIDs reports the region ids with at least one present box in the
// run's trace, and whether each id also delivered OCR text.
func dynamicTraceIDs(t *testing.T, h *harness) (present map[string]bool, read map[string]bool) {
	t.Helper()
	present = map[string]bool{}
	read = map[string]bool{}
	for _, tr := range h.runner.Trace() {
		if tr.RegionPresent {
			present[tr.RegionID] = true
		}
		if tr.OCRText != "" {
			read[tr.RegionID] = true
		}
	}
	return present, read
}

func TestDynamicPanelRegionReadsUncoveredBox(t *testing.T) {
	script := panelRegionScript(t, ocrScript(t))
	h := newHarness(t, &fakeOCR{script: script, delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.Regions = []games.Region{shiftedTopRegion()}
		c.PanelFind = pipeline.DefaultPanelFind()
		c.PanelFind.Enabled = true
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	commits := committed(t, evs)
	if len(commits) == 0 {
		for _, tr := range h.runner.Trace() {
			if tr.RegionID == "panel-anywhere" && (tr.RegionPresent || tr.OCRText != "" || tr.Reason != "") {
				t.Logf("DBG %s seq=%d present=%v hash=%s ocr=%q reason=%s state=%s",
					tr.RegionID, tr.FrameSeq, tr.RegionPresent, tr.RegionHash, tr.OCRText, tr.Reason, tr.State)
			}
		}
		t.Fatalf("no line committed with the finder on (stats %+v): the dynamic region must have read the uncovered box", st)
	}
	present, read := dynamicTraceIDs(t, h)
	if !present["panel-anywhere"] {
		t.Errorf("no present-box observation from the dynamic region (present regions %v): the box was read through the fixed path, not the finder", present)
	}
	if !read["panel-anywhere"] {
		t.Errorf("the dynamic region never delivered OCR text (read regions %v), yet %v committed", read, commits)
	}
	if present["topother"] {
		t.Errorf("the shifted top region reported a present box: it crops empty scene")
	}
}

func TestDynamicPanelOffReadsNothing(t *testing.T) {
	// The counterfactual: the same shifted region, finder disabled — the
	// pre-fix behaviour. Nothing may commit.
	script := ocrScript(t)
	h := newHarness(t, &fakeOCR{script: script, delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.Regions = []games.Region{shiftedTopRegion()}
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if commits := committed(t, evs); len(commits) != 0 {
		t.Fatalf("finder off, region shifted: %v committed (stats %+v); the shifted fixed band must read nothing", commits, st)
	}
}

func TestDynamicPanelYieldsToConfiguredRegion(t *testing.T) {
	// With the configured region in place (the box's own band) the finder
	// must not shadow it: the lines commit through the configured region,
	// and the dynamic region never reports a present box.
	script := ocrScript(t)
	h := newHarness(t, &fakeOCR{script: script, delay: fastScript, respectCtx: true}, func(c *pipeline.Config) {
		c.PanelFind = pipeline.DefaultPanelFind()
		c.PanelFind.Enabled = true
	})
	st, evs, err := h.run(t, h.source(0, false))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if commits := committed(t, evs); len(commits) == 0 {
		t.Fatalf("no line committed with the finder enabled over a working configured region (stats %+v)", st)
	}
	present, read := dynamicTraceIDs(t, h)
	if present["panel-anywhere"] {
		t.Errorf("the dynamic region reported a present box while the configured region owns the box (present regions %v): the finder's gate must keep it out", present)
	}
	if !read["bottom"] {
		t.Errorf("the configured region did not deliver OCR text (read regions %v): the fixed path must be unchanged", read)
	}
}
