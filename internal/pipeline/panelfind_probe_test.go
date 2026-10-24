package pipeline

// ADR-0172 (issue #53): the panel finder's captured-frame probe.
//
// The finder is tuned on synthetic frames (panelfind_test.go); this test is
// its captured-frame check on the PRIVATE Dolphin frames the catch-up runs
// captured under the gitignored artifacts/ tree (bottom and middle boxes; a
// top-of-screen box frame, if the fixtures' savestate work has produced one,
// is what the finder exists for).
//
// AGENTS.md rule 3/5: the frames are private emulator output — the test is
// gated on the env var GN_PANEL_PROBE_DIR (a directory of PNG frames, e.g.
// artifacts/catch-up-live/*/frames or artifacts/av-harness/*/media) and is
// NOT RUN when it is unset or absent; its output is per-frame RECTANGLES
// only — never recognised text, never a frame. On a fresh checkout nothing
// prints and the test is skipped, not passed.

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func TestFindPanelsCapturedProbe(t *testing.T) {
	dir := os.Getenv("GN_PANEL_PROBE_DIR")
	if dir == "" {
		t.Skip("NOT RUN: GN_PANEL_PROBE_DIR unset (private captured frames are never checked in)")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Skipf("NOT RUN: %v", err)
	}
	var frames []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".png", ".PNG", ".jpg", ".jpeg", ".webp":
			frames = append(frames, filepath.Join(dir, e.Name()))
		}
	}
	if len(frames) == 0 {
		t.Skip("NOT RUN: no frame images under " + dir)
	}
	sort.Strings(frames)

	cfg := DefaultPanelFind()
	cfg.Enabled = true
	hits := 0
	t0 := time.Now()
	for _, p := range frames {
		img, err := loadProbeImage(p)
		if err != nil {
			continue
		}
		start := time.Now()
		luma := panelLuma(img, cfg.SampleW, cfg.SampleH)
		got := FindPanels(luma, cfg.SampleW, cfg.SampleH, cfg)
		elapsed := time.Since(start)
		diagRows(t, p, cfg)
		if len(got) > 0 {
			hits++
		}
		t.Logf("%s: %v in %v", filepath.Base(p), panelRects(got), elapsed)
	}
	t.Logf("%d frames, %d with a panel, %v total", len(frames), hits, time.Since(t0))
}

// diagRows, when GN_PANEL_PROBE_DIAG names a frame file, logs that frame's
// per-row longest panel-tone run over the downsampled luma: the developer
// view for tuning the scan rule against a corpus the finder misses.
func diagRows(t *testing.T, p string, cfg PanelFindConfig) {
	want := os.Getenv("GN_PANEL_PROBE_DIAG")
	if want == "" || filepath.Base(p) != want {
		return
	}
	t.Helper()
	img, err := loadProbeImage(p)
	if err != nil {
		return
	}
	c := cfg.normalize()
	luma := panelLuma(img, c.SampleW, c.SampleH)
	for y := 0; y < c.SampleH; y++ {
		row := luma[y*c.SampleW : (y+1)*c.SampleW]
		bestStart, bestLen, curStart, curLen := 0, 0, -1, 0
		for x := 0; x < c.SampleW; x++ {
			if l := row[x]; l < c.DarkLuma || l > c.BrightLuma {
				if curLen == 0 {
					curStart = x
				}
				curLen++
			} else {
				if curLen > bestLen {
					bestLen, bestStart = curLen, curStart
				}
				curLen = 0
			}
		}
		if curLen > bestLen {
			bestLen, bestStart = curLen, curStart
		}
		if bestLen > 0 {
			t.Logf("  row %3d: run %3d-%3d len %3d (%.2f) %s", y, bestStart, bestStart+bestLen-1, bestLen, float64(bestLen)/float64(c.SampleW), markRowTone(row[bestStart:bestStart+bestLen], c))
		}
	}
}

func markRowTone(run []byte, c PanelFindConfig) string {
	dark, bright := 0, 0
	for _, l := range run {
		if l < c.DarkLuma {
			dark++
		} else if l > c.BrightLuma {
			bright++
		}
	}
	switch {
	case bright > dark:
		return "bright"
	case dark > 0:
		return "dark"
	default:
		return "none"
	}
}

// loadProbeImage loads one probe frame (png; jpg/webp fall through to a
// decode failure the probe skips — the captured bundles are png).
func loadProbeImage(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return png.Decode(f)
}

func panelRects(got []PanelRect) string {
	if len(got) == 0 {
		return "no panel"
	}
	out := ""
	for _, p := range got {
		out += fmt.Sprintf(" [x=%.3f y=%.3f w=%.3f h=%.3f score=%.3f]", p.X, p.Y, p.W, p.H, p.Score)
	}
	return out
}
