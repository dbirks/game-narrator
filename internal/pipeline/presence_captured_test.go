package pipeline

import (
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/vision"
)

// The 2026-10-21 board sailing capture (negative: no dialogue box on screen in
// any frame) and the three Dolphin Wind Waker fixtures (positive: real boxes),
// under the external fixture root. All private (AGENTS.md rule 5).
const (
	fixtureBoardSailing  = "board-sailing-20261021"
	fixtureWWFirst       = "windwaker-first-dialogue"
	fixtureWWLegendDense = "windwaker-legend-dense"
	fixtureWWLegendPress = "windwaker-legend-press"
)

// presenceFixtureRoot resolves the external fixture root the way the dolphin
// gates do (internal/dolphin): the env var wins, otherwise the gitignored
// artifacts/fixtures/ under the repository root — the test's CWD is
// internal/pipeline, so the CWD-relative default would land in the wrong tree.
func presenceFixtureRoot(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("GAME_NARRATOR_FIXTURE_DIR"); v != "" {
		return v
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "artifacts", "fixtures")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s: cannot locate the repository root", cwd)
		}
	}
}

// bandBoardSailing is the bottom band the gate is replayed on for the board's
// full frames: a superset of the two crop shapes the session OCR'd (508x174
// and 508x116, bottom-anchored on the 640x480 capture — the fixture README
// records the sizes; the calibrated profile rects live on the board, not in
// the fixture, so the superset band is the reproducible choice). Any sub-band
// of the scene is a negative: no box is anywhere on the screen.
var bandBoardSailing = vision.Rect{X: 0.10, Y: 0.62, W: 0.80, H: 0.38}

// gateOne runs the presence gate exactly the way roi.go's analyze does
// (vision.PanelPresenceOf + the PresenceConfig predicate, the production
// defaults) and reports the read and the decision.
func gateOne(img image.Image, cfg PresenceConfig) (vision.PanelPresence, bool) {
	pp := vision.PanelPresenceOf(img, cfg.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	return pp, cfg.Present(pp)
}

// TestPresenceRejectsSeaAcceptsBoxes is the class-(b) captured-frame replay
// (AGENTS.md rule 3, ADR-0137 Amendment: the sea false positive) the amendment
// exists to prove: the presence gate's yes/no on the two real captures that
// bracket it. The board sailing capture reports the region present over the
// open sea and OCR'd the junk that was spoken — every crop and frame is a
// negative the gate must say no to. The three Dolphin fixtures carry real
// boxes — every truth row with box_present is a yes the gate must keep.
// No-box rows in those fixtures are logged, not asserted: a dark scene with
// no box reads present-but-empty (ADR-0137 consequences — the tracker handles
// the empty read and the matcher refuses to commit it), and the amendment does
// not claim to change that. It quotes no game text (AGENTS.md rule 5): the
// assertions are on the gate's decision and the measured shares, never on a
// recognised line. NOT RUN when the external fixture is absent — never a pass.
func TestPresenceRejectsSeaAcceptsBoxes(t *testing.T) {
	root := presenceFixtureRoot(t)
	board := filepath.Join(root, fixtureBoardSailing)
	if _, err := os.Stat(board); err != nil {
		t.Skip("NOT RUN: the board-sailing fixture is absent (set $GAME_NARRATOR_FIXTURE_DIR to the fixture root that holds " + fixtureBoardSailing + ")")
	}
	cfg := DefaultPresence()

	// The negative crops first: these are the exact region crops the pipeline
	// OCR'd on the 2026-10-21 sailing session — the false positives themselves.
	crops, err := filepath.Glob(filepath.Join(board, "crops", "*.png"))
	if err != nil || len(crops) == 0 {
		t.Fatalf("board-sailing crops: %v (none found under %s)", err, filepath.Join(board, "crops"))
	}
	sort.Strings(crops)
	nn, minText, maxText, minSD, maxSD := 0, 1.0, 0.0, 1.0, 0.0
	for _, p := range crops {
		img, err := decodePresenceImage(p, ".png")
		if err != nil {
			t.Fatalf("decode crop %s: %v", filepath.Base(p), err)
		}
		pp, present := gateOne(img, cfg)
		nn++
		minText, maxText = mathMinF(minText, pp.Text), mathMaxF(maxText, pp.Text)
		minSD, maxSD = mathMinF(minSD, pp.SaturatedDark), mathMaxF(maxSD, pp.SaturatedDark)
		if present {
			t.Errorf("crop %s: the gate says PRESENT over the open sea (dark=%.3f text=%.4f satDark=%.3f)",
				filepath.Base(p), pp.Dark, pp.Text, pp.SaturatedDark)
		}
	}
	t.Logf("board sailing crops: n=%d all NOT present; text share %.4f–%.4f, saturated-dark %.3f–%.3f (floors: text>=%.3f, satDark<=%.1f)",
		nn, minText, maxText, minSD, maxSD, cfg.MinTextShare, cfg.MaxSaturatedDarkShare)

	// The negative frames: the full 640x480 frames the crops came out of, cut
	// on the superset bottom band.
	frames, err := filepath.Glob(filepath.Join(board, "frames", "*.jpg"))
	if err != nil || len(frames) == 0 {
		t.Fatalf("board-sailing frames: %v (none found under %s)", err, filepath.Join(board, "frames"))
	}
	sort.Strings(frames)
	nf, frameMinText := 0, 1.0
	for _, p := range frames {
		img, err := decodePresenceImage(p, ".jpg")
		if err != nil {
			t.Fatalf("decode frame %s: %v", filepath.Base(p), err)
		}
		crop := vision.CropNormalized(img, bandBoardSailing, vision.CropOptions{})
		pp, present := gateOne(crop, cfg)
		nf++
		frameMinText = mathMinF(frameMinText, pp.Text)
		if present {
			t.Errorf("frame %s: the gate says PRESENT on the bottom band (dark=%.3f text=%.4f satDark=%.3f)",
				filepath.Base(p), pp.Dark, pp.Text, pp.SaturatedDark)
		}
	}
	t.Logf("board sailing frames: n=%d all NOT present on the bottom band; min text share %.4f", nf, frameMinText)

	// The positives: every box_present truth row must read present on the
	// region that carries the box (a fixture with two regions — the subtitle
	// band and the dialogue band — keeps only one lit at a time, so the
	// assertion is per-row, not per-region). The rects come from each
	// fixture's profile.json: the same geometry the pipeline would crop.
	positives := map[string][]vision.Rect{}
	havePositive := false
	for _, name := range []string{fixtureWWFirst, fixtureWWLegendDense, fixtureWWLegendPress} {
		d := filepath.Join(root, name)
		_, err := os.Stat(filepath.Join(d, "frames"))
		if err != nil {
			t.Logf("NOT RUN: %s is absent; its box rows are unverified", name)
			continue
		}
		havePositive = true
		positives[name], err = profileTextRegions(t, filepath.Join(d, "profile.json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		rows, err := loadTruthRows(t, filepath.Join(d, "truth.json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		nPres, nAbsent, nAbsentLit, minT, maxT, maxSDPos := 0, 0, 0, 1.0, 0.0, 0.0
		for _, row := range rows {
			img, err := decodePresenceImage(filepath.Join(d, "frames", row.File), ".png")
			if err != nil {
				t.Fatalf("%s %s: %v", name, row.File, err)
			}
			anyPresent, rowText, rowSD := false, 1.0, 0.0
			for _, r := range positives[name] {
				crop := vision.CropNormalized(img, r, vision.CropOptions{})
				pp, present := gateOne(crop, cfg)
				rowText, rowSD = mathMinF(rowText, pp.Text), mathMaxF(rowSD, pp.SaturatedDark)
				anyPresent = anyPresent || present
			}
			if row.BoxPresent {
				nPres++
				if !anyPresent {
					t.Errorf("%s %s (box present in truth): the gate says NOT PRESENT on every region (min text share %.4f)",
						name, row.File, rowText)
				} else {
					minT, maxT = mathMinF(minT, rowText), mathMaxF(maxT, rowText)
					maxSDPos = mathMaxF(maxSDPos, rowSD)
				}
			} else {
				// No box in the truth: the verdict is logged, not asserted. These
				// rows are the present-but-empty shape (a dark scene, a little
				// bright noise) the tracker already handles — the amendment
				// claims the sea shape is rejected, not that every dark frame is
				// read absent.
				nAbsent++
				if anyPresent {
					nAbsentLit++
				}
			}
		}
		if nPres > 0 {
			t.Logf("%s: %d/%d box rows read PRESENT; row min text %.4f–%.4f, max saturated-dark %.3f",
				name, nPres, nPres+nAbsent, minT, maxT, maxSDPos)
		}
		if nAbsentLit > 0 {
			t.Logf("%s: %d of %d no-box rows read present (the present-but-empty shape; logged, not asserted)", name, nAbsentLit, nAbsent)
		} else {
			t.Logf("%s: %d no-box rows all NOT present", name, nAbsent)
		}
	}
	if !havePositive {
		t.Skip("NOT RUN: no positive Wind Waker fixture is present (the box side of the gate is unverified)")
	}
}

// profileTextRegions loads a fixture profile.json and returns the normalized
// rects of its textbox regions, in order.
func profileTextRegions(t *testing.T, path string) ([]vision.Rect, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p struct {
		Regions []struct {
			ID   string  `json:"id"`
			Type string  `json:"type"`
			X    float64 `json:"x"`
			Y    float64 `json:"y"`
			W    float64 `json:"w"`
			H    float64 `json:"h"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, err
	}
	var out []vision.Rect
	for _, r := range p.Regions {
		if !strings.EqualFold(r.Type, "textbox") {
			continue
		}
		out = append(out, vision.Rect{X: r.X, Y: r.Y, W: r.W, H: r.H})
	}
	if len(out) == 0 {
		return nil, errNoTextRegion
	}
	return out, nil
}

// errNoTextRegion is profileTextRegions' answer when a fixture profile
// carries no textbox region: the gate has nothing to replay on.
var errNoTextRegion = errors.New("profile has no textbox region")

// truthRows is one fixture truth.json row: the frame file and whether the
// ground truth carries a dialogue box on it.
type truthRows struct {
	File       string `json:"file"`
	BoxPresent bool   `json:"box_present"`
}

func loadTruthRows(t *testing.T, path string) ([]truthRows, error) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rows []truthRows
	if err := json.Unmarshal(b, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

// decodePresenceImage reads a fixture frame by its extension; the magic-free
// split keeps the two fixture shapes (the board's JPEG capture, Dolphin's PNG)
// on one walk.
func decodePresenceImage(path, ext string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if ext == ".jpg" || ext == ".jpeg" {
		return jpeg.Decode(f)
	}
	return png.Decode(f)
}

func mathMinF(a, b float64) float64 {
	if b < a {
		return b
	}
	return a
}

func mathMaxF(a, b float64) float64 {
	if b > a {
		return b
	}
	return a
}

// TestZeroPresenceConfigTakesTheDefaults is the 2026-10-22 board regression:
// the runtime builds the pipeline with a zero PresenceConfig, and a ceiling
// of 0 read as "no saturated dark allowed" rejected the real Wind Waker box
// (saturated-dark share 0.010 on the capture card) for a whole session.
func TestZeroPresenceConfigTakesTheDefaults(t *testing.T) {
	cfg := PresenceConfig{}.normalize()
	if cfg != DefaultPresence() {
		t.Fatalf("zero config normalizes to %+v, want the defaults %+v", cfg, DefaultPresence())
	}
	board := vision.PanelPresence{Dark: 0.870, Text: 0.0208, SaturatedDark: 0.010}
	if !cfg.Present(board) || !(PresenceConfig{}).Present(board) {
		t.Fatal("the board's measured box crop reads absent under a zero (default) config")
	}
	sea := vision.PanelPresence{Dark: 0.84, Text: 0.0003, SaturatedDark: 0.95}
	if cfg.Present(sea) {
		t.Fatal("the sea reads present under the defaults")
	}
}
