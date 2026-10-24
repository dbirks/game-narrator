package pipeline

// ADR-0172 (issue #53): synthetic-frame tests for the frame-level panel
// finder. Every frame below is invented (uniform tones, procedural strokes —
// no game content, AGENTS.md rule 5), so the tests pin the SCAN RULE, not one
// game's pixels: a dark (and a bright) panel at the top, the middle and the
// bottom of the frame, at odd sizes, over a scene that is not a panel, and
// the two false-positive shapes the threshold pair exists to reject — a busy
// scene and a smooth vertical gradient (the sea).

import "testing"

// lumaOf builds a w×h row-major luma frame from a per-pixel function.
func lumaOf(w, h int, f func(x, y int) byte) []byte {
	out := make([]byte, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out[y*w+x] = f(x, y)
		}
	}
	return out
}

// darkBox draws a dark dialogue panel at [x0,x1)×[y0,y1) with vertical
// bright text strokes on a mid-gray scene. Strokes are 2 px wide every 8,
// so the band's edge density is ~25% — letters, not noise.
func darkBox(w, h, x0, y0, x1, y1 int) []byte {
	return lumaOf(w, h, func(x, y int) byte {
		if y >= y0 && y < y1 && x >= x0 && x < x1 {
			if (x-x0)%8 >= 5 && (x-x0)%8 < 7 { // stroke
				return 240
			}
			return 20 // panel
		}
		return 128 // scene
	})
}

// brightBox is the light-panel twin: a bright band with dark strokes.
func brightBox(w, h, x0, y0, x1, y1 int) []byte {
	return lumaOf(w, h, func(x, y int) byte {
		if y >= y0 && y < y1 && x >= x0 && x < x1 {
			if (x-x0)%8 >= 5 && (x-x0)%8 < 7 {
				return 20
			}
			return 230
		}
		return 128
	})
}

// wantPanel asserts the scan found exactly one candidate centred where the
// frame says the box is.
func wantPanel(t *testing.T, got []PanelRect, w, h int, x0, y0, x1, y1 int, where string) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("%s: got %d candidates %v, want 1", where, len(got), got)
	}
	p := got[0]
	if p.Y < float64(y0)/float64(h)-0.03 || p.Y > float64(y1)/float64(h)+0.03 {
		t.Errorf("%s: panel y=%0.3f, box is y in [%0.3f, %0.3f]",
			where, p.Y, float64(y0)/float64(h), float64(y1)/float64(h))
	}
	if p.X < float64(x0)/float64(w)-0.05 || p.X > float64(x1)/float64(w)+0.05 {
		t.Errorf("%s: panel x=%0.3f, box is x in [%0.3f, %0.3f]",
			where, p.X, float64(x0)/float64(w), float64(x1)/float64(w))
	}
	if p.W < 0.45 {
		t.Errorf("%s: panel width %0.3f too narrow", where, p.W)
	}
}

func TestFindPanelsTopMiddleBottom(t *testing.T) {
	cfg := DefaultPanelFind()
	// 160×120 scan; a 100×20 dark box (~62% wide, ~17% tall) at the three
	// calibrated placements and one between them.
	cases := []struct {
		name string
		y0   int
	}{
		{"top", 10},
		{"upper-middle", 40},
		{"middle", 50},
		{"lower", 80},
		{"bottom", 98},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindPanels(darkBox(160, 120, 30, c.y0, 130, c.y0+20), 160, 120, cfg)
			wantPanel(t, got, 160, 120, 30, c.y0, 130, c.y0+20, c.name)
		})
	}
}

func TestFindPanelsBrightPanel(t *testing.T) {
	// A game that draws a bright panel (the finder's rowBright twin).
	got := FindPanels(brightBox(160, 120, 20, 50, 140, 50+24), 160, 120, DefaultPanelFind())
	wantPanel(t, got, 160, 120, 20, 50, 140, 74, "bright")
}

func TestFindPanelsOddSizes(t *testing.T) {
	// The scan takes its dimensions from its input, not the default
	// 160×120: an odd-height frame with a box at an odd placement.
	w, h := 96, 80
	box := []struct {
		name           string
		x0, y0, x1, y1 int
	}{
		{"narrow-frame wide box", 5, 30, 90, 46}, // ~89% wide, 20% tall
		{"tall-frame short box", 10, 60, 66, 70}, // 58% wide, 12.5% tall
	}
	for _, b := range box {
		t.Run(b.name, func(t *testing.T) {
			got := FindPanels(darkBox(w, h, b.x0, b.y0, b.x1, b.y1), w, h, DefaultPanelFind())
			wantPanel(t, got, w, h, b.x0, b.y0, b.x1, b.y1, b.name)
		})
	}
}

func TestFindPanelsBusySceneNone(t *testing.T) {
	// A busy scene: deterministic per-pixel noise spanning the full luma
	// range — every row is a mix of dark and bright, so no row is uniform,
	// and no panel may be reported.
	busy := lumaOf(160, 120, func(x, y int) byte {
		return byte((x*x*7 + y*131 + x*17 + y*3) % 256)
	})
	if got := FindPanels(busy, 160, 120, DefaultPanelFind()); len(got) != 0 {
		t.Fatalf("busy scene: got %v, want none", got)
	}
}

func TestFindPanelsSeaGradientNone(t *testing.T) {
	// The sea: a smooth vertical gradient, bright at the top, darker
	// below, no horizontal structure. Its top rows are uniformly bright
	// (a real panel-shape run), so the WIDTH gate would pass — the scan
	// must reject it on EDGE DENSITY: no letters, no edges.
	sea := lumaOf(160, 120, func(x, y int) byte {
		return byte(200 - 110*y/120) // 200 → 90, monotone down
	})
	if got := FindPanels(sea, 160, 120, DefaultPanelFind()); len(got) != 0 {
		t.Fatalf("sea gradient: got %v, want none", got)
	}
}

func TestFindPanelsUniformSceneNone(t *testing.T) {
	// A uniformly dark scene (a night) is one giant dark run: it must be
	// rejected by the height bound, not read as a box, and the same for a
	// uniformly bright one.
	dark := lumaOf(160, 120, func(x, y int) byte { return 15 })
	bright := lumaOf(160, 120, func(x, y int) byte { return 245 })
	for _, c := range []struct {
		name string
		l    []byte
	}{{"uniform dark", dark}, {"uniform bright", bright}} {
		if got := FindPanels(c.l, 160, 120, DefaultPanelFind()); len(got) != 0 {
			t.Fatalf("%s: got %v, want none", c.name, got)
		}
	}
}

func TestFindPanelsTooNarrowNone(t *testing.T) {
	// A panel whose tone holds below MinToneShare of a row is a HUD chip, not
	// a dialogue box: no candidate.
	w, h := 160, 120
	got := FindPanels(darkBox(w, h, 60, 50, 100, 70), w, h, DefaultPanelFind())
	if len(got) != 0 {
		t.Fatalf("narrow panel: got %v, want none", got)
	}
}

func TestFindPanelsTextRowsSplitBand(t *testing.T) {
	// The full-line box the gap rule exists for: a panel whose text rows
	// carry anti-aliased scene-luma fringes that BREAK the row's panel-tone
	// run, so the strict contiguous rule reads the box as two half-bands
	// (one too short, one without edges) and finds nothing. The finder
	// must merge across the letters' neutral rows and return the whole box.
	w, h := 160, 120
	scene := byte(128)
	frame := lumaOf(w, h, func(x, y int) byte {
		if x >= 30 && x < 130 && y >= 48 && y < 72 {
			switch {
			case x == 30 || x == 129: // panel border
				return 240
			case y >= 57 && y < 64 && x%4 == 1: // text-row fringe (scene luma)
				return scene
			case y >= 57 && y < 64 && x%8 >= 5 && x%8 < 7: // letters
				return 240
			default: // panel
				return 20
			}
		}
		return scene
	})
	got := FindPanels(frame, w, h, DefaultPanelFind())
	wantPanel(t, got, w, h, 30, 48, 130, 72, "split-band")
}

func TestFindPanelsConfigNormalize(t *testing.T) {
	c := PanelFindConfig{}.normalize()
	d := DefaultPanelFind()
	if c != d {
		t.Fatalf("zero config normalized to %+v, want default %+v", c, d)
	}
	// Explicit values survive normalize; unset ones fall back.
	e := PanelFindConfig{Enabled: true, DarkLuma: 50}.normalize()
	if !e.Enabled || e.DarkLuma != 50 || e.SampleW != d.SampleW {
		t.Fatalf("normalize mangled explicit values: %+v", e)
	}
}
