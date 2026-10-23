package pipeline

import (
	"image"
	"image/color"
	"testing"

	"github.com/dbirks/game-narrator/internal/vision"
)

// The synthetic presence gate corpus (ADR-0169): generated images, committed,
// no fixture dependency — the shapes the 2026-10-23 open-sea probe frame
// exposed, distilled without any game content. The gate is replayed exactly
// the way roi.go's analyze runs it: the standard board band (probeBand) cut
// out of a full 640x480 frame, the production defaults.

// sceneFrame paints the 640x480 backdrop the band sits over: a dark
// desaturated scene (luma 62-84, under darkLuma but no pixel reaches
// brightLuma), the shape of the real board capture where the scene around
// the box is dark enough that its pixels do not read as bright-on-dark text.
func sceneFrame() *image.NRGBA {
	f := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			// A slow banding (luma 62-84): structure without any bright
			// stroke, so the scene contributes no off-panel text.
			l := 62 + (x*31+y*7)%23
			f.SetNRGBA(x, y, color.NRGBA{uint8(l), uint8(l - 4), uint8(l - 10), 255})
		}
	}
	return f
}

// boxBandWithoutArrow paints the positive shape on the scene: a uniform dark
// translucent panel across the board band (luma ~45, desaturated — the
// panel's own tint) carrying two lines of text-like strokes (bright vertical
// bars on the panel). NO advance arrow, no other glyph: the 2026-10-23 frame
// that read absent the moment its arrow vanished.
func boxBandWithoutArrow(f *image.NRGBA) {
	// The panel spans the board band's rows (y 0.62-1.0 of 480 = 298-480),
	// inset a little so the crop's panel rows are unambiguous.
	for y := 300; y < 470; y++ {
		for x := 60; x < 580; x++ {
			// A faint per-pixel tint (luma 40-52, desaturated) keeps the
			// panel uniform but not flat — capture noise shape.
			l := uint8(40 + (x+y*3)%13)
			f.SetNRGBA(x, y, color.NRGBA{l, l + 2, l + 4, 255})
		}
	}
	// Two text lines: thin bright strokes (luma 225) on the panel — 3px
	// wide, 16px tall, 9px gaps — letterform edges without letterforms.
	for _, lineY := range []int{330, 395} {
		for x := 80; x < 560; x += 12 {
			for dy := 0; dy < 16; dy++ {
				for dx := 0; dx < 3; dx++ {
					f.SetNRGBA(x+dx, lineY+dy, color.NRGBA{225, 225, 225, 255})
				}
			}
		}
	}
}

// TestPresenceAcceptsBoxBandWithoutArrow is the ADR-0169 positive: a dark
// translucent band with text strokes and no advance arrow is a present
// textbox — the arrow is a marker, not the box.
func TestPresenceAcceptsBoxBandWithoutArrow(t *testing.T) {
	cfg := DefaultPresence()
	f := sceneFrame()
	boxBandWithoutArrow(f)

	crop := cropBand(f)
	pp := vision.PanelPresenceOf(crop, cfg.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	if !cfg.Present(pp) {
		t.Fatalf("box band without arrow reads absent: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f}",
			pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
	}
	// The panel reaches the top of the crop, so the open-sea/sky clause's
	// bright-top half must not fire here.
	if pp.TopDark < cfg.MinTopDarkFloor() {
		t.Errorf("topDark=%.3f: the synthetic panel does not reach the top of the crop (want >= %.2f)", pp.TopDark, cfg.MinTopDarkFloor())
	}
	t.Logf("box band without arrow: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f} -> PRESENT",
		pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
}

// TestPresenceRejectsBrightSeaGradient is the ADR-0169 negative the task
// asks for: a bright open-sea/sky gradient — no panel, no line — must read
// absent. Its board band is bright (low dark share), so the dark clause is
// what rejects it; the open-sea/sky clause must not loosen to rescue it.
func TestPresenceRejectsBrightSeaGradient(t *testing.T) {
	cfg := DefaultPresence()
	f := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		// Sky to bright open sea: luma 200 at the top easing to 150 at the
		// bottom, a desaturated blue tint — no dark panel anywhere.
		l := uint8(200 - y*50/480)
		for x := 0; x < 640; x++ {
			f.SetNRGBA(x, y, color.NRGBA{uint8(l - 15), uint8(l - 5), l, 255})
		}
	}

	crop := cropBand(f)
	pp := vision.PanelPresenceOf(crop, cfg.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	if cfg.Present(pp) {
		t.Fatalf("bright sea gradient reads present: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f}",
			pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
	}
	t.Logf("bright sea gradient: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f} -> ABSENT (dark clause)",
		pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
}

// TestPresenceRejectsOpenSeaWithSparkle is the measured 2026-10-23 shape
// distilled: a scene whose board band is fairly dark (dark water under a
// bright sky) with a little saturation in the water and a few bright glints.
// The dark, text, and dark-saturated clauses all pass; only the open-sea/sky
// clause (a weakly saturated scene with a bright top band) separates it from
// a box (the probe frame read SatDark 0.0274 and TopDark 0.062; every box
// read TopDark >= 0.78).
func TestPresenceRejectsOpenSeaWithSparkle(t *testing.T) {
	cfg := DefaultPresence()
	f := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			// Bright sky above (luma ~200) to y=343, dark water below — the
			// board-band crop (y 297-480) then mixes bright sky rows at the
			// top with dark water below, so its top band is bright (under
			// the 0.5 floor) while its overall dark share (~0.71) still
			// clears the 0.30 floor. Every 20th water pixel is a saturated
			// blue (spread >= 80) so the water's saturated-dark share lands
			// around 0.05 — over the 0.02 sea floor but far under the 0.5
			// dark-saturated ceiling.
			if y < 343 {
				f.SetNRGBA(x, y, color.NRGBA{200, 202, 204, 255})
			} else if x%20 == 0 {
				f.SetNRGBA(x, y, color.NRGBA{20, 100, 40, 255}) // saturated dark (spread >= 80)
			} else {
				f.SetNRGBA(x, y, color.NRGBA{55, 58, 61, 255}) // desaturated dark water
			}
		}
	}
	// Wave glints: a sparse run of bright strokes on the water — enough
	// bright-on-dark pixels to clear the text floor.
	for x := 60; x < 560; x += 17 {
		for dy := 0; dy < 3; dy++ {
			f.SetNRGBA(x, 400+dy, color.NRGBA{210, 210, 210, 255})
		}
	}

	crop := cropBand(f)
	pp := vision.PanelPresenceOf(crop, cfg.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	if cfg.Present(pp) {
		t.Fatalf("open sea with sparkle reads present: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f}",
			pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
	}
	// The rejection must be the open-sea/sky clause itself: a weakly
	// saturated scene with a bright top band.
	if pp.TopDark >= cfg.MinTopDarkFloor() || pp.SaturatedDark <= cfg.MinSeaSatFloor() {
		t.Errorf("the synthetic sea no longer pins the open-sea/sky clause (TopDark=%.3f want < %.2f; SaturatedDark=%.4f want > %.2f)",
			pp.TopDark, cfg.MinTopDarkFloor(), pp.SaturatedDark, cfg.MinSeaSatFloor())
	}
	// The other clauses must pass on this image — otherwise the test pins
	// the wrong rejection and a future regression through the open-sea/sky
	// clause would still pass here.
	if pp.Dark < cfg.MinDarkShare || pp.Text < cfg.MinTextShare || pp.SaturatedDark > cfg.SaturatedDarkCeiling() {
		t.Errorf("the negative must fail on the open-sea/sky clause alone, not the dark/text/dark-saturated clauses: Dark=%.4f(>=%.2f) Text=%.6f(>=%.4f) SaturatedDark=%.4f(<=%.2f)",
			pp.Dark, cfg.MinDarkShare, pp.Text, cfg.MinTextShare, pp.SaturatedDark, cfg.SaturatedDarkCeiling())
	}
	t.Logf("open sea with sparkle: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f} -> ABSENT (open-sea/sky clause)",
		pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
}

// TestPresenceKeepsDesaturatedBoxWithBrightTop is the no-tracker-sign shape
// the open-sea/sky clause must NOT swallow: a desaturated band (no
// saturation anywhere) whose top of the crop is bright, dark enough overall,
// carrying a line. The clause requires a little saturation to fire, so a
// genuinely desaturated panel with a bright top stays present — a bright top
// alone is not the sea.
func TestPresenceKeepsDesaturatedBoxWithBrightTop(t *testing.T) {
	cfg := DefaultPresence()
	f := image.NewNRGBA(image.Rect(0, 0, 640, 480))
	for y := 0; y < 480; y++ {
		for x := 0; x < 640; x++ {
			// A bright strip at the top of the board band (the crop's first
			// ~23 rows, y 297-320), desaturated — sky above the sign — and a
			// dark desaturated panel below it, so the crop's top band is
			// bright (TopDark ~ 0) but the crop carries no saturation at all.
			if y >= 297 && y < 320 {
				f.SetNRGBA(x, y, color.NRGBA{200, 202, 204, 255})
			} else {
				l := uint8(48 + (x+y*3)%13)
				f.SetNRGBA(x, y, color.NRGBA{l, l + 2, l + 4, 255})
			}
		}
	}
	// A line of bright strokes on the panel clears the text floor.
	for x := 80; x < 560; x += 12 {
		for dy := 0; dy < 16; dy++ {
			for dx := 0; dx < 3; dx++ {
				f.SetNRGBA(x+dx, 380+dy, color.NRGBA{225, 225, 225, 255})
			}
		}
	}

	crop := cropBand(f)
	pp := vision.PanelPresenceOf(crop, cfg.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	if !cfg.Present(pp) {
		t.Fatalf("desaturated box with bright top reads absent: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f}",
			pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
	}
	// The shape must have a bright top (the clause's bright-top half true)
	// and no saturation (its saturation half false) — that is what keeps it
	// present. If either flips, the test no longer pins the no-tracker-sign
	// contract.
	if pp.TopDark >= cfg.MinTopDarkFloor() {
		t.Errorf("topDark=%.3f: the synthetic sign's top is not bright (want < %.2f)", pp.TopDark, cfg.MinTopDarkFloor())
	}
	if pp.SaturatedDark > cfg.MinSeaSatFloor() {
		t.Errorf("saturatedDark=%.4f: the synthetic sign is saturated (want <= %.2f)", pp.SaturatedDark, cfg.MinSeaSatFloor())
	}
	t.Logf("desaturated box with bright top: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f} -> PRESENT",
		pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
}

// TestPresenceRejectsDarkFlatBandKeeps holds the documented contract the
// open-sea/sky clause must not erode: a dark, desaturated band with NO
// strokes (a night sky, an empty letterbox band) still reads absent — the
// whole crop is one flat dark colour (no bright pixels at all, so TopDark is
// 1 and SaturatedDark is 0), and the text floor is what rejects it, exactly
// as before ADR-0169.
func TestPresenceRejectsDarkFlatBandKeeps(t *testing.T) {
	cfg := DefaultPresence()
	f := sceneFrame()
	// Paint the ENTIRE board-band crop one flat dark desaturated colour —
	// no strokes, and no bright scene edge left inside the crop.
	for y := 290; y < 480; y++ {
		for x := 60; x < 580; x++ {
			f.SetNRGBA(x, y, color.NRGBA{50, 52, 55, 255})
		}
	}
	// The band must read ABSENT (no text) — the open-sea/sky clause added by
	// ADR-0169 must not have loosened the empty-band rejection.
	crop := cropBand(f)
	pp := vision.PanelPresenceOf(crop, cfg.DarkLuma, vision.DefaultTextLuma, vision.DefaultSatFloor)
	if cfg.Present(pp) {
		t.Fatalf("dark flat band with no strokes reads present: PanelPresence{Dark=%.4f Text=%.6f SaturatedDark=%.4f TopDark=%.3f}",
			pp.Dark, pp.Text, pp.SaturatedDark, pp.TopDark)
	}
}
