package pipeline

import (
	"context"
	"image"
	"image/draw"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// TestPlacementOCRBeforeAfterFix is the OCR half of ADR-0137's class-(b)
// captured-frame replay: on the real live-hdmi capture it measures what the
// OLD fixed band (the profile's one calibrated rect — the pre-Fix-1 crop) and
// the NEW placement-aware selection (TestPlacementOnLiveCapture's picker)
// each read off the box, and what each OCR call costs.
//
// The two paths run the same single OCR (the fix moves the crop, not the
// engine): the timing leg is the cost discipline (one crop, one OCR, inside
// the ~600 ms frame_to_ocr budget the selection steers) and the quality leg
// is the crop's, measured frame by frame: the fixed band on a top-placed box
// reads a confident fragment (half the line, the rest cut by the band's edge),
// and the placed band reads the full line — the frame's NEW read carrying
// visibly more of the line than its OLD one.
//
// AGENTS.md rule 5: the output is aggregate — durations, confidences, rune
// counts, band counts — never a recognised line. NOT RUN when the external
// fixture is absent (never a pass).
func TestPlacementOCRBeforeAfterFix(t *testing.T) {
	root, frames, ok := liveFixtureDir()
	if !ok {
		t.Skip("NOT RUN: live-hdmi fixture absent (set $GAME_NARRATOR_FIXTURE_DIR to the capture root)")
	}

	// The production crop recipe: the runtime's single source (letterbox
	// detection plus the ADR-0115 §2 inset). It lives in internal/runtime
	// (which this package must not import — the dependency runs the other
	// way), so the two constants are mirrored here: a change to the recipe
	// there must land here.
	crop := vision.CropOptions{Letterbox: vision.LetterboxDetect, Inset: 2}

	// The profile's calibrated textbox band (the rev3 "dialogue box high"
	// rect, ADR-0137): the OLD path's crop and the candidate set's base.
	base := vision.Rect{X: 0.05, Y: 0.25, W: 0.8, H: 0.37}
	cands := textBandCandidates(base)
	const presence = 0.30
	margin := DefaultPlacementSwitchMargin
	textMin := DefaultPlacementTextMin
	onPanel := DefaultPlacementTextOnPanel
	switchFrames := DefaultPlacementSwitchFrames
	const darkLuma = uint8(90)

	// One picker run per capture session (the fixture root holds one
	// directory per dev-trace session, plus the standalone example frame):
	// the picker's sticky band is a per-run state, and mixing two sessions'
	// frames in one run would leak the remembered band across the session
	// boundary the scene change implies.
	groups := map[string][]string{}
	order := []string{}
	for _, f := range frames {
		rel, err := filepath.Rel(root, f)
		if err != nil {
			rel = f
		}
		g := rel
		if i := strings.IndexByte(rel, filepath.Separator); i >= 0 {
			g = rel[:i]
		}
		if _, seen := groups[g]; !seen {
			order = append(order, g)
		}
		groups[g] = append(groups[g], f)
	}
	const perGroup = 60 // the capture is longer than the budget the gate owes (and the gate runs -race alongside the box's other gates)

	eng := ocr.Tesseract{Timeout: 6 * time.Second}
	ctx := context.Background()
	const ocrBudget = 600 * time.Millisecond
	const fullReadGain = 20 // runes: a full line over a cut fragment (the ADR's measured ~2x)

	type leg struct {
		costs []time.Duration
		// runes is the per-frame read length (trimmed), aligned with costs —
		// the frame-level comparison needs the pair, not the sum.
		runes []int
		// good is a read the matcher would take seriously: confident and
		// line-shaped (a few words at least).
		good, total int
		bands       map[int]int
	}
	aggs := map[string]struct{ old, new leg }{}

	decodeOCR := func(img image.Image, rect vision.Rect) (string, float64, time.Duration) {
		rgba := image.NewRGBA(img.Bounds())
		draw.Draw(rgba, rgba.Bounds(), img, img.Bounds().Min, draw.Src)
		c := vision.CropNormalized(rgba, rect, crop)
		if c == nil || c.Bounds().Empty() {
			return "", 0, 0
		}
		p := vision.Preprocess(c, vision.DefaultSteps())
		t0 := time.Now()
		res, err := eng.Recognize(ctx, p)
		cost := time.Since(t0)
		if err != nil {
			return "", 0, cost
		}
		return res.Text, res.MeanConf, cost
	}
	readLen := func(text string) int {
		return len([]rune(strings.TrimSpace(matcher.TrimEdgeNoise(text))))
	}

	for _, g := range order {
		frs := groups[g]
		if len(frs) > perGroup {
			frs = frs[:perGroup]
		}
		agg := aggs[g]
		agg.old.bands = map[int]int{}
		agg.new.bands = map[int]int{}
		cur := profileBandIndex(cands, base) // the remembered band starts where the profile drew it
		var mem placementMemory              // the switch persistence is per-run state, with the band
		for _, p := range frs {
			img, err := decodeJPEG(p)
			if err != nil {
				continue
			}
			// The OLD path: the fixed band, every frame (the pre-Fix-1 shape).
			text, conf, cost := decodeOCR(img, base)
			agg.old.costs = append(agg.old.costs, cost)
			agg.old.total++
			agg.old.runes = append(agg.old.runes, readLen(text))
			if conf >= 0.7 && readLen(text) >= 20 {
				agg.old.good++
			}
			// The NEW path: the picker's selection, then the one OCR.
			scores := scoreTextBands(img, cands, crop, darkLuma)
			cur = pickTextBand(scores, cur, presence, margin, textMin, onPanel, &mem, switchFrames)
			agg.new.bands[cur]++
			text, conf, cost = decodeOCR(img, cands[cur])
			agg.new.costs = append(agg.new.costs, cost)
			agg.new.total++
			agg.new.runes = append(agg.new.runes, readLen(text))
			if conf >= 0.7 && readLen(text) >= 20 {
				agg.new.good++
			}
		}
		aggs[g] = agg
	}

	varP50 := func(costs []time.Duration) time.Duration {
		if len(costs) == 0 {
			return 0
		}
		s := make([]time.Duration, len(costs))
		copy(s, costs)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		return s[len(s)/2]
	}
	report := func(g string, name string, l leg) (p50 time.Duration, frac float64) {
		if len(l.costs) == 0 {
			return 0, 0
		}
		p50 = varP50(l.costs)
		frac = float64(l.good) / float64(l.total)
		avg := 0
		for _, n := range l.runes {
			avg += n
		}
		t.Logf("%s %s OCR: n=%d p50=%v (budget %v); confident line-shaped reads %d/%d (%.0f%%); mean %d runes",
			g, name, len(l.costs), p50, ocrBudget, l.good, l.total, frac*100, avg/len(l.runes))
		return
	}

	improvedGroups := 0
	for _, g := range order {
		agg := aggs[g]
		if agg.old.total == 0 || agg.new.total == 0 {
			continue
		}
		oldP50, _ := report(g, "OLD(fixed) ", agg.old)
		newP50, _ := report(g, "NEW(placed) bands="+bandCounts(agg.new.bands, bandNames(cands)), agg.new)
		// The cost discipline: the placement-aware path is one crop and one
		// OCR inside the frame_to_ocr budget — the selection added no OCR
		// work (the frame the selection steers stays the single read). The
		// budget is enforced only where a median is a claim: the gate runs
		// -race alongside the other packages, and a single-sample group (the
		// standalone example frame) measures that one call's host load, not
		// the path — measured under gate load, the example frame's OLD-path
		// read landed at 582 ms and its NEW-path read at 1.3 s while the
		// 60-frame group's p50 sat at ~234 ms. Small groups report their
		// cost; the budget is enforced on the real medians.
		if len(agg.new.costs) >= 10 {
			if newP50 > ocrBudget {
				t.Errorf("%s NEW p50 %v exceeds the %v frame_to_ocr budget", g, newP50, ocrBudget)
			}
		} else {
			t.Logf("%s NEW p50 %v from %d sample(s) under concurrent gate load (informational: the budget is enforced on groups with a real median)", g, newP50, len(agg.new.costs))
		}
		if oldP50 > ocrBudget {
			t.Logf("%s OLD p50 %v was already over the budget on this host (the live session's measured p50; the crop's content, not the OCR count, is what moved)", g, oldP50)
		}
		// The quality leg, frame by frame: the NEW read carries visibly more
		// of the line than the OLD one (the full line over the cut
		// fragment). The frame pairs are aligned by construction (one OLD
		// and one NEW OCR per frame, in order).
		full, pairs := 0, 0
		for i := range agg.old.runes {
			if i >= len(agg.new.runes) {
				break
			}
			pairs++
			if agg.new.runes[i] >= agg.old.runes[i]+fullReadGain {
				full++
			}
		}
		frac := 0.0
		if pairs > 0 {
			frac = float64(full) / float64(pairs)
		}
		t.Logf("%s frame-level: the placed read carried the fuller line on %d/%d frames (%.0f%%)", g, full, pairs, frac*100)
		if frac >= 0.5 {
			improvedGroups++
		}
	}
	if improvedGroups == 0 {
		t.Fatalf("the placement-aware crop did not read the fuller line on any capture group: the fix is not doing its job on this capture")
	}
	_ = root
}

// bandCounts renders the picker's per-band selection counts (top / profile
// / bottom) for the report line.
func bandCounts(counts map[int]int, names []string) string {
	out := ""
	for i, n := range names {
		if i == 0 {
			out = n + "=" + strconv.Itoa(counts[i])
		} else {
			out += " " + n + "=" + strconv.Itoa(counts[i])
		}
	}
	return out
}

// bandNames names the candidate set's bands the way the selection counts
// report them: the geometry, not the order (top at y=0, bottom at y=1-h,
// the profile's own band whatever its y — a profile already at the top
// reports twice, the report line, not the picker).
func bandNames(cands []vision.Rect) []string {
	out := make([]string, 0, len(cands))
	for _, r := range cands {
		switch r.Y {
		case 0:
			out = append(out, "top")
		case 1 - r.H:
			out = append(out, "bottom")
		default:
			out = append(out, "profile")
		}
	}
	return out
}
