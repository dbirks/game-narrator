package pipeline

// The Dolphin probe replay (ADR-0137 §Robustness, the 2026-10-21 catch-up
// run's dev-trace frames): the captured-frame regression for the failure the
// backdrop gate and the switch persistence exist to kill.
//
// The shape, measured on the probe frames (internal/e2e's catch-up run, the
// bundle the test replays): the llvmpipe software renderer — the dev host's
// no-GPU fallback — prints its GL info block into the TOP of the framebuffer
// for the first seconds after a savestate load. The block is a dense strip of
// bright text (text share 0.018–0.027, past the selection's floor) whose
// letters sit in scene rows (on-panel 0.80 — the gate rejects it at 0.9).
// The real dialogue box sits in the BOTTOM of the frame (y ≈ 350–369 of 480),
// a short line riding a panel that fills its band (on-panel 1.00). The old
// picker (pre-robustness) took the top band on the first banner frame — the
// standing band's own faint read scored under the text floor, so it was not
// even a box candidate — and never came back: the box's line scored under the
// floor too, so no band was ever eligible to pull the crop home, and every
// OCR ran against the banner band's empty scene. Nothing was read from the
// box.
//
// This test replays the probe frames through the picker (and, bounded,
// through the OCR) and asserts the regression is gone:
//
//   - the crop NEVER lands on the top band — the banner's phantom text is not
//     a box, on either region's candidate set;
//   - on the box's region (the profile's lower textbox) the crop stays on the
//     profile band — where the box is — for the run, and
//   - the OCR leg (when the engine is available): the standing band's crop
//     reads the box's line — visibly more runes than the top band's crop,
//     which reads the empty scene.
//
// AGENTS.md rule 3: this is class-(b) captured-frame replay — the frames are
// captured output of the real emulator session; the test reads them, it does
// not run the emulator. Rule 5: the frames are private; the test's output is
// aggregate (band counts, rune counts) — never a recognised line. The bundle
// lives under the gitignored artifacts/, so a fresh checkout has no probe and
// the test is NOT RUN (never a pass) when it is absent.

import (
	"context"
	"image"
	"image/draw"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// probeRunDir finds the newest catch-up probe bundle under the gitignored
// artifacts/ (the e2e catch-up run's dev-trace frames), and the PNG frame
// paths it holds. The bundle lives in this checkout's artifacts/ — or, when
// the external fixture root env var is set, in the catch-up-live/ sibling of
// that root (the same artifacts/ tree the fixture root itself sits in, the
// one-var-one-meaning layout of ADR-0182, issue #47). The test is NOT RUN
// when the bundle is absent.
func probeRunDir() (string, []string, bool) {
	rel := filepath.Join("..", "..", "artifacts", "catch-up-live")
	if v := os.Getenv("GAME_NARRATOR_FIXTURE_DIR"); v != "" {
		rel = filepath.Join(filepath.Dir(v), "catch-up-live")
	}
	entries, err := os.ReadDir(rel)
	if err != nil {
		return "", nil, false
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "probe-") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", nil, false
	}
	sort.Strings(names)
	dir := filepath.Join(rel, names[len(names)-1])
	var frames []string
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() && filepath.Ext(p) == ".png" {
			frames = append(frames, p)
		}
		return nil
	})
	sort.Strings(frames)
	if len(frames) == 0 {
		return "", nil, false
	}
	return dir, frames, true
}

// TestPlacementOnDolphinProbe is the captured-frame regression replay: the
// banner's band never wins the crop, the box's region keeps its calibrated
// band, and the standing band's crop OCRs the box's line.
func TestPlacementOnDolphinProbe(t *testing.T) {
	dir, frames, ok := probeRunDir()
	if !ok {
		t.Skip("NOT RUN: no Dolphin probe bundle under artifacts/catch-up-live/ (the e2e catch-up run's dev-trace frames; private, gitignored)")
	}
	t.Logf("probe replay: %d frames from %s", len(frames), dir)

	// The catch-up profile's two textbox regions (internal/e2e's scene
	// constants, mirrored here: the dependency runs the other way), each with
	// its derived candidate set.
	regions := []struct {
		name string
		base vision.Rect
	}{
		{"dialogue box", vision.Rect{X: 0.05, Y: 0.62, W: 0.8, H: 0.25}},
		{"dialogue box high", vision.Rect{X: 0.05, Y: 0.25, W: 0.8, H: 0.37}},
	}
	const presence = 0.30
	const darkLuma = uint8(90)
	crop := vision.CropOptions{}

	type leg struct {
		win    map[int]int
		ocrNew int // trimmed runes read off the standing band's crop
		ocrTop int // trimmed runes read off the top (banner) band's crop
	}
	legs := map[string]leg{}

	for _, reg := range regions {
		cands := textBandCandidates(reg.base)
		cur := profileBandIndex(cands, reg.base)
		var mem placementMemory
		win := map[int]int{}
		decode := newProbeDecoder(t)
		newRunes, topRunes, ocrCalls := 0, 0, 0
		for i, p := range frames {
			img, err := loadProbeFrame(p)
			if err != nil {
				continue
			}
			scores := scoreTextBands(img, cands, crop, darkLuma)
			cur = pickTextBand(scores, cur, presence, DefaultPlacementSwitchMargin, DefaultPlacementTextMin,
				DefaultPlacementTextOnPanel, &mem, DefaultPlacementSwitchFrames)
			win[cur]++
			// The OCR leg, bounded: a strided sample of the run's back half
			// (the banner is long gone; the box's line is on screen). The
			// standing band's crop is what the session would OCR; the top
			// band's crop is what the old picker was stuck on.
			if i >= len(frames)/2 && i%11 == 0 && ocrCalls < 8 {
				newRunes += decode(img, cands[cur])
				topRunes += decode(img, cands[0])
				ocrCalls++
			}
		}
		if len(win) == 0 {
			t.Fatalf("%s: no decodable probe frames in %s", reg.name, dir)
		}
		if win[0] > 0 {
			t.Errorf("%s: the crop landed on the top band %d frame(s) — the GL-info banner's phantom text must never win the crop (counts %v)",
				reg.name, win[0], win)
		}
		// The box's region (the profile's lower textbox) holds the box's line
		// at y ≈ 350–369 of 480 — inside its profile band. The crop must stay
		// there for the run: the box is on the calibrated band, and the banner
		// (not a box) is no reason to leave it.
		if reg.name == "dialogue box" {
			base := profileBandIndex(cands, reg.base)
			if win[base] < int(0.9*float64(len(frames))) {
				t.Errorf("%s: the crop left the calibrated band for %d of %d frames (counts %v) — the box's line sits on that band, and the banner is not a box",
					reg.name, len(frames)-win[base], len(frames), win)
			}
		}
		legs[reg.name] = leg{win: win, ocrNew: newRunes, ocrTop: topRunes}
	}

	// The OCR leg: the standing band's crop reads the box's line — visibly
	// more than the top band's crop of the empty scene. (A read of the empty
	// scene is usually nothing, or a one-glyph blip; the line is ~20 runes.)
	for _, reg := range regions {
		l := legs[reg.name]
		if reg.name == "dialogue box" && l.ocrNew < 40 {
			t.Errorf("dialogue box: the standing band's crop read only %d trimmed runes over the sample (want >= 40: the box's line is on that band) — the crop is not where the box is", l.ocrNew)
		}
		if reg.name == "dialogue box" && l.ocrTop >= l.ocrNew {
			t.Errorf("dialogue box: the top band's crop read %d runes, >= the standing band's %d — the old failure shape (the banner band read, the box band starved)", l.ocrTop, l.ocrNew)
		}
		t.Logf("%s: standing band by selection %v; sample OCR standing %d runes vs top band %d runes", reg.name, l.win, l.ocrNew, l.ocrTop)
	}
}

// newProbeDecoder builds the OCR leg's reader: the production crop recipe
// (the runtime's single source, mirrored the way the timing test mirrors it)
// and Tesseract, reading the crop's trimmed rune count. A call whose engine
// errors counts as zero runes; if EVERY call errors the leg is an engine
// absence, not a crop failure, and the caller's rune assertions are the
// honest read of that.
func newProbeDecoder(t *testing.T) func(image.Image, vision.Rect) int {
	t.Helper()
	crop := vision.CropOptions{Letterbox: vision.LetterboxDetect, Inset: 2}
	eng := ocr.Tesseract{Timeout: 6 * time.Second}
	ctx := context.Background()
	return func(img image.Image, rect vision.Rect) int {
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		c := vision.CropNormalized(rgba, rect, crop)
		if c == nil || c.Bounds().Empty() {
			return 0
		}
		p := vision.Preprocess(c, vision.DefaultSteps())
		res, err := eng.Recognize(ctx, p)
		if err != nil {
			return 0
		}
		return len([]rune(strings.TrimSpace(matcher.TrimEdgeNoise(res.Text))))
	}
}

func loadProbeFrame(p string) (image.Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}
