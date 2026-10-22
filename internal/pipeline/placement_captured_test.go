package pipeline

import (
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/vision"
)

// liveFixtureDir resolves the external live-capture fixture bundle and
// reports whether it holds any JPEG frame. The bundle is the dedicated
// live-capture root — $GAME_NARRATOR_LIVE_CAPTURE_DIR when set, else the
// $GAME_NARRATOR_FIXTURE_DIR fixture root's live-capture/ subdirectory
// (ADR-0054's fixture-root convention) — and it is NEVER the whole fixture
// root: that root also holds the named fixtures (windwaker-*, board-sailing-*,
// states/) the other gates read, and walking it would let a placement replay
// pick up frames it does not own. It is the class-(b) captured-frame replay
// root (AGENTS.md rule 5: the fixture is external and private, referenced by
// the env vars, never committed); the test is NOT RUN — never a pass — when
// the bundle is absent.
func liveFixtureDir() (string, []string, bool) {
	root := os.Getenv("GAME_NARRATOR_LIVE_CAPTURE_DIR")
	if root == "" {
		if v := os.Getenv("GAME_NARRATOR_FIXTURE_DIR"); v != "" {
			root = filepath.Join(v, "live-capture")
		}
	}
	if root == "" {
		return "", nil, false
	}
	if _, err := os.Stat(root); err != nil {
		return "", nil, false
	}
	var frames []string
	filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		f, e := os.Open(p)
		if e != nil {
			return nil
		}
		h := make([]byte, 3)
		if _, e = f.Read(h); e != nil {
			f.Close()
			return nil
		}
		f.Close()
		// JPEG magic: 0xFF 0xD8 0xFF. The dev-trace blobs are content-addressed
		// (no extension), so the magic byte is the only honest shape test.
		if h[0] == 0xff && h[1] == 0xd8 && h[2] == 0xff {
			frames = append(frames, p)
		}
		return nil
	})
	sort.Strings(frames)
	if len(frames) == 0 {
		return "", nil, false
	}
	return root, frames, true
}

// TestPlacementOnLiveCapture is the class-(b) captured-frame replay the ADR
// exists to prove (ADR-0137): on the real live-hdmi capture — where the
// dialogue box sat in the TOP band and the fixed profile band cut its text in
// half — the placement-aware picker selects the top band. It asserts the
// selection and measures the picker's per-frame cost (three luma passes),
// reporting it against the ~600 ms frame_to_ocr budget the selection steers. It
// quotes no game text (AGENTS.md rule 5): the assertion is on the selected band
// and the measured cost, never on the recognised line. NOT RUN when the
// bundle is absent.
func TestPlacementOnLiveCapture(t *testing.T) {
	root, frames, ok := liveFixtureDir()
	if !ok {
		t.Skip("NOT RUN: live-capture bundle absent (set $GAME_NARRATOR_LIVE_CAPTURE_DIR to the capture bundle root, or $GAME_NARRATOR_FIXTURE_DIR to the fixture root holding a live-capture/ subdirectory)")
	}

	// The profile's calibrated textbox band (the "dialogue box high" rect
	// measured on the capture): the base the candidate set derives from.
	base := vision.Rect{X: 0.05, Y: 0.25, W: 0.8, H: 0.37}
	cands := textBandCandidates(base)
	crop := vision.CropOptions{}
	const darkLuma = uint8(90)

	const presence = 0.30
	margin := DefaultPlacementSwitchMargin
	textMin := DefaultPlacementTextMin
	onPanel := DefaultPlacementTextOnPanel
	switchFrames := DefaultPlacementSwitchFrames
	cur := profileBandIndex(cands, base)
	var mem placementMemory // the switch persistence (ADR-0137 §Robustness)

	win := map[int]int{}
	var costs []time.Duration
	ocrBudget := 600 * time.Millisecond // frame_to_ocr p50, ADR-0115
	for i, p := range frames {
		img, err := decodeJPEG(p)
		if err != nil {
			continue
		}
		t0 := time.Now()
		scores := scoreTextBands(img, cands, crop, darkLuma)
		costs = append(costs, time.Since(t0))
		cur = pickTextBand(scores, cur, presence, margin, textMin, onPanel, &mem, switchFrames)
		win[cur]++
		_ = i
	}
	if len(costs) == 0 {
		t.Skip("NOT RUN: no decodable JPEG frames in the bundle")
	}

	// The box sat in the top band across this capture: the picker's standing
	// band must be the top one for the run. If it had stayed on the profile band
	// (the historic fixed crop), the top box's text would have been cut in half —
	// the exact failure this ADR fixes.
	if win[0] == 0 {
		t.Fatalf("picker never selected the top band (counts %v): the top-placed box would stay unread", win)
	}

	// The picker's cost is three luma passes against the ~600 ms OCR it steers:
	// it must be a small fraction of the budget, or "do not OCR every band every
	// frame" is violated. Report the median and fail if it is not well under.
	// The bar is budget/4 (150 ms), not a tighter constant: the gate runs -race
	// with the dev box's other gates concurrently, and the measured median has
	// spanned 7 ms (quiescent) to 81 ms (loaded) on this host. The invariant
	// protected is "the selection is far cheaper than the single OCR it
	// steers" (measured OCR p50 ~200–300 ms on the capture), and a 150 ms
	// median still is; the violation the assertion must catch (OCR-ing all
	// three bands, ~3x the single read) lands far above it.
	median := medianDuration(costs)
	if median > ocrBudget/4 {
		t.Errorf("picker cost p50 %v is more than 25%% of the %v OCR budget: the selection must stay cheap", median, ocrBudget)
	}
	t.Logf("live capture %q: %d frames; standing band by selection %v; picker p50 %v (OCR budget %v)",
		root, len(costs), win, median, ocrBudget)
}

func decodeJPEG(p string) (image.Image, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return jpeg.Decode(f)
}

func medianDuration(d []time.Duration) time.Duration {
	if len(d) == 0 {
		return 0
	}
	s := make([]time.Duration, len(d))
	copy(s, d)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	return s[len(s)/2]
}
