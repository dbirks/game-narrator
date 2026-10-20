package dolphin

import (
	"fmt"
	"image"
	"math"
)

// Screen signatures (docs/spec/11 "Avoiding false positives"): a signature
// identifies a SPECIFIC screen by measured features, not by "the scene
// changed". Two uses:
//
//   - the drive's checkpoints gate each step on the EXPECTED screen (the
//     memory-card prompt with the cursor on a measured side, the file-select
//     grid, the name-entry grid, the dialogue box band);
//   - the runner's loop guard (a step that sees the same prompt again after
//     pressing must fail fast, never pass): after an input attempt the
//     runner re-measures the screen and compares its signature to the
//     previous step's final signature — identical keys mean the input had
//     no effect (a Yes/No prompt loop), which fails the step immediately.
//
// Every threshold below was measured on this host's retained drive evidence
// (ADR-0043; the per-probe PNGs in the evidence dir named by the test trace).
// The comments carry the measured values each clause separates; no game text
// is stored or compared.

// sigRegions are the measured windows (fractions of the frame) the signature
// reports. The set covers the discriminating features of every screen on
// the new-game drive: the full-frame stats (scene identity), the lower third
// (title logo vs bare sky), the box band (the dialogue box), the bottom bar
// (the file-select/name-entry hint bar), the option-row halves (the Yes/No
// cursor blob) and the panel text band (ink density).
type sigRegion struct {
	name           string
	x0, y0, x1, y1 float64
}

var sigRegions = []sigRegion{
	{"lower3", 0.0, 2.0 / 3.0, 1.0, 1.0},
	{"boxband", boxBandX0, boxBandY0, boxBandX1, boxBandY1},
	{"topright", 0.60, 0.02, 0.95, 0.20},
	{"botbar", 0.0, 0.90, 1.0, 1.0},
	{"left", 0.15, 0.55, 0.50, 0.85},
	{"right", 0.50, 0.55, 0.85, 0.85},
	{"text", 0.30, 0.35, 0.70, 0.60},
	{"mid", 0.30, 0.30, 0.70, 0.65},
	// The name-entry screen's measured windows (ADR-0043 amendment 5, the
	// 110+ retained frames of the 2026-10-17 drive's name-entry screen —
	// probes 010-118 of that run's opening-skip step, the same screen in
	// both its dim and bright frame-pulse phases):
	{"namegrid", 0.20, 0.40, 0.80, 0.75},  // the keyboard grid body
	{"namefield", 0.25, 0.30, 0.75, 0.45}, // the name field row
	// The opening legend's narration subtitle (amendment 6): the bottom black
	// bar's text window, BELOW the storybook panel's measured bottom edge
	// (0.74) and ABOVE the pure bottom margin, so the panel's art cannot
	// pollute the measurement (the panel's bright rows at 0.70-0.74 sit just
	// above this window; the name entry's hint bar at 0.90-1.0 sits just
	// below it). The subtitle's two measured lines (0.78-0.83, 0.84-0.88) are
	// inside.
	{"subbar", 0.0, 0.76, 1.0, 0.90},
}

// sigRegionStats is one window's measured features.
type sigRegionStats struct {
	mean   float64 // luma mean (0..255)
	bright float64 // fraction of luma >= sigBrightLuma
	dark   float64 // fraction of luma <= sigDarkLuma
	edges  float64 // fraction of horizontal |dluma| >= sigEdgeDelta
	blob   float64 // fraction of luma >= sigBlobLuma (the cursor is a near-white blob)
	blobCX float64 // centroid x (frame fraction) of the blob pixels (0 when none)
}

const (
	sigBrightLuma = 190
	sigDarkLuma   = 90
	sigBlobLuma   = 220 // the Yes/No cursor blob is near-white (measured 220+)
	sigEdgeDelta  = 30  // a glyph stroke edge (luma steps)
)

// ScreenSig is the measured fingerprint of one frame: the full-frame stats
// plus the per-window stats the checkpoints read.
type ScreenSig struct {
	W, H      int
	Mean      float64 // full-frame luma mean
	Std       float64 // full-frame luma stddev
	Entropy   float64 // normalised (÷8) luma entropy
	Letterbox float64 // the lower of the top/bottom bands (0..1)
	Regions   map[string]sigRegionStats
}

// measureRegion measures one signature window.
func measureRegion(px []uint8, w, h int, x0, y0, x1, y1 float64) sigRegionStats {
	X0, Y0 := int(x0*float64(w)), int(y0*float64(h))
	X1, Y1 := int(x1*float64(w)), int(y1*float64(h))
	var s sigRegionStats
	if X1-X0 < 4 || Y1-Y0 < 2 {
		return s
	}
	var (
		sum                        float64
		n, b, d, edge, blob, blobX int
	)
	for y := Y0; y < Y1; y++ {
		row := px[y*w : (y+1)*w]
		for x := X0; x < X1; x++ {
			v := row[x]
			sum += float64(v)
			n++
			if v >= sigBrightLuma {
				b++
			}
			if v <= sigDarkLuma {
				d++
			}
			if v >= sigBlobLuma {
				blob++
				blobX += x
			}
			if x > X0 {
				if dd := int(v) - int(row[x-1]); dd >= sigEdgeDelta || dd <= -sigEdgeDelta {
					edge++
				}
			}
		}
	}
	s.mean = sum / float64(n)
	s.bright = float64(b) / float64(n)
	s.dark = float64(d) / float64(n)
	s.edges = float64(edge) / float64(n)
	s.blob = float64(blob) / float64(n)
	if blob > 0 {
		s.blobCX = float64(blobX) / float64(blob) / float64(w)
	}
	return s
}

// ScreenSigOf measures the frame. Total: a nil/empty image yields an
// all-zero signature (class "unknown"), never a panic.
func ScreenSigOf(img image.Image) ScreenSig {
	w, h, px := luma(img)
	sig := ScreenSig{W: w, H: h, Regions: make(map[string]sigRegionStats, len(sigRegions))}
	if w == 0 || h == 0 || px == nil {
		return sig
	}
	var (
		sum, sumSq float64
		hist       [256]int
	)
	for _, v := range px {
		f := float64(v)
		sum += f
		sumSq += f * f
		hist[v]++
	}
	n := float64(len(px))
	sig.Mean = sum / n
	sig.Std = math.Sqrt(math.Max(0, sumSq/n-sig.Mean*sig.Mean))
	e := 0.0
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(len(px))
		e -= p * math.Log2(p)
	}
	sig.Entropy = e / 8
	sig.Letterbox = letterBand01(img)
	for _, r := range sigRegions {
		sig.Regions[r.name] = measureRegion(px, w, h, r.x0, r.y0, r.x1, r.y1)
	}
	return sig
}

// SigLine renders the signature as one measured line (the per-step trace and
// the discovery test log it; the checkpoints' details carry the clauses they
// check). It is the human-readable form of what the loop guard compares.
func (s ScreenSig) SigLine() string {
	if s.W == 0 {
		return "sig: no frame"
	}
	get := func(k string) sigRegionStats { return s.Regions[k] }
	l3, bb := get("lower3"), get("boxband")
	tr, bt := get("topright"), get("botbar")
	le, ri, tx, md := get("left"), get("right"), get("text"), get("mid")
	ng, nf := get("namegrid"), get("namefield")
	return fmt.Sprintf("mean=%.1f std=%.1f ent=%.3f lb=%.3f lower3.bright=%.3f boxband[mean=%.0f edges=%.3f] topright.bright=%.3f botbar[mean=%.0f bright=%.3f] mid.mean=%.0f left.blob=%.4f(cx=%.3f) right.blob=%.4f(cx=%.3f) text.edges=%.3f namegrid[mean=%.0f edges=%.3f] namefield[mean=%.0f bright=%.3f]",
		s.Mean, s.Std, s.Entropy, s.Letterbox,
		l3.bright, bb.mean, bb.edges, tr.bright,
		bt.mean, bt.bright, md.mean, le.blob, le.blobCX, ri.blob, ri.blobCX, tx.edges,
		ng.mean, ng.edges, nf.mean, nf.bright)
}

// ScreenClass identifies the screen family by the measured clauses.
type ScreenClass string

const (
	ClassUnknown  ScreenClass = "unknown"
	ClassTitle    ScreenClass = "title"    // orbiting camera + bright logo lower third
	ClassPrompt   ScreenClass = "prompt"   // a bright letterboxed Yes/No panel (synthetic doubles only on this host — the real create-save prompt is the dark ClassContinue family; amendment 4)
	ClassDark     ScreenClass = "dark"     // the overall-dark system screen (the near-black transition/notice)
	ClassDialog   ScreenClass = "dialog"   // the system dialog: a bright centre panel over a dark background (save notices, the reading state)
	ClassContinue ScreenClass = "continue" // the "cannot be saved — continue anyway?" Yes/No dialog: the dialog base CARRYING the near-white cursor blob (cursor side reported separately)
	ClassFileSel  ScreenClass = "filesel"  // the quest-log "File Selection" list: a dark screen with the 3 slot icons (near-white) + dense text
	ClassNameEnt  ScreenClass = "nameent"  // name entry: the keyboard grid + bright bottom hint bar
	ClassLetter   ScreenClass = "letter"   // the bright full-frame opening monologue
	ClassBox      ScreenClass = "box"      // a rendered scene carrying the dialogue box
	ClassScene    ScreenClass = "scene"    // a rendered scene without the box band
	// ClassStorySub (amendment 6): the opening legend's letterboxed storybook
	// panel carrying the white narration subtitle in the bottom black bar —
	// the first in-game narrated text the pipeline must read (the M6 drive's
	// success screen). The class is the LETTERBOXED panel + the measured text
	// clauses; a letterboxed frame WITHOUT the text (a fade, a panel between
	// subtitles) is the cutscene family (ClassDark/ClassScene), never this.
	ClassStorySub ScreenClass = "storysub"
)

// CursorSide is the measured side of the Yes/No cursor blob on a prompt.
type CursorSide string

const (
	CursorNone  CursorSide = "-"
	CursorLeft  CursorSide = "yes" // Yes is the left option (text order Yes/No)
	CursorRight CursorSide = "no"  // No is the right option
)

// The measured class thresholds. The confusable pairs get their separating
// clauses here (the negative checks the drive requires):
//
//   - create-save prompt vs title dim phase: the prompt is the DARK dialog
//     family (ClassContinue: bright centre panel, mid mean 160) where the
//     title's dim "press START" phase is letterboxed (0.28-0.56) with a
//     mid-luma centre (mid 100-132) — the mid-mean and letterbox clauses
//     separate them (amendment 4; the 2026-10-17 false pass read the dim
//     title as the prompt);
//   - prompt (bright panel) vs letter: the letter is bright FULL-frame
//     (boxband mean 133-143, smooth edges) where the prompt's box band
//     sits on the sky;
//   - filesel vs dialog: the quest-log list's dense slot text (entropy
//     0.74-0.84) + bright slot panel in the box band (mean 140-143) vs the
//     blobless info dialog's sparse text (0.52-0.68) + dark box band (65);
//     the slot icons' near-white (0.024-0.092) appears only on transition
//     frames and is NOT a usable clause (0.000 on settled frames); the
//     letterbox clause (filesel 0.15-0.22 vs the title dim phase 0.28-0.56,
//     amendment 4) keeps the title flicker out;
//   - nameent vs the rest: the bright bottom hint bar (0.941) + the keyboard
//     grid's dense ink (text.edges 0.104-0.134);
//   - box vs letter: the box band carries glyph ink (edges >= boxBandEdgesMin)
//     where the letter band is smooth (measured edges 0.000);
//   - box (dark) vs the dark family (ADR-0054 amendment 1, the 2026-10-18
//     dense legend recording): the first in-game dialogue box (the Outset
//     Island opening NPC) is a DARK translucent band with white text over a
//     rendered scene — the dark family below swallows it (measured mean
//     75-83, l3.dark > 0.85, mid 126-137) and its near-white character in
//     the side band reads as the cursor blob (measured 0.0256, cx 0.256),
//     so 576 settled frames of the box measured "continue:yes" and the
//     legend watch's dialogue-box boundary never fired. Every measured
//     system screen carries the dark-gray frame bands (lb 0.156-0.212),
//     where the rendered scene measures lb 0.000 — and the box band is
//     measured STABLE across the box's whole life (35 settled samples:
//     mean 46-57, edges 0.031, bright 0.014), where the legend's
//     between-subtitle interludes vary (mean 68-137, edges <= 0.026, the
//     one 0.038 frame carrying bright 0.133).
const (
	classDarkMaxMean      = 110.0 // dark system screen family: full-frame mean (measured 32-90; the continue dialog measured 98.4, so the cap sits above its flicker band and 45+ below the bright screens' 155-158)
	classDarkMinDark      = 0.85  // ...and a near-black lower third (measured dark 0.88-1.00)
	classDialogMinMid     = 130.0 // the dialog's bright centre panel (measured mid mean 160 vs the overall-dark screen's 28-52); the title-dim clause (below) keeps this floor to exclude the bright-panel dark dialogs from the title class
	classDarkDialogMinMid = 115.0 // the DARK-FAMILY dialog branch's centre-panel floor (lowered from 130 for the 2026-10-17 re-run): the blobless save-creation / save-fail dialog measured mid 123-129 across its 44 retained frames — just below the old 130 floor, so the drive read it as the overall-dark class (wait) and sat on it 44 iterations (~88 s) before a single frame's mid drifted to 130 and let it through as the dialog (A). The true dark screens (the near-black boot/transition) measure mid 0-70, so 115 sits 45+ above them and 8 below the dialog's measured floor. The title-dim clause above keeps the 130 floor (its measured mid band 100-132 and letterbox 0.28-0.56 keep it out of the dark family regardless)
	classTitleLowerBright = 0.20  // the title's logo lower third (measured 0.233-0.337)
	classPromptPanelMin   = 110.0 // the parchment panel mean in the text window (measured ~121-134)
	classLetterMeanMin    = 130.0 // the bright full-frame letter (measured 155-159)
	classLetterBandMin    = 115.0 // its box band (measured 133-143, smooth)
	classHintBarBrightMin = 0.80  // the bright bottom hint bar (measured 0.941)
	classLetterTopBright  = 0.30  // the letter's bright top-right (measured 0.40-0.60; title 0.000)
	classGridInkMin       = 0.10  // the name-entry keyboard grid's ink (vs the slot screen's)
	classPromptBlobMin    = 0.004 // the near-white cursor blob minimum (the Yes/No cursor, measured 0.023+ on the cursor side)
	// The Yes/No prompt's cursor BLINKS (on/off at ~1 s, measured: the blob is
	// 0.027-0.040 on the cursor side when on, 0.000-0.002 when off). On a
	// blink-OFF frame the prompt is a blobless dark-family dialog — and an A
	// sent to it while the cursor is on NO selects No (the 2026-10-17 run's
	// exact failure: the drive pressed A on a blink-off create-save prompt).
	// The blink-off frame is still recognisable: the prompt's text panel is
	// dense (measured centre-panel mean 155-164, text-window edges 0.095-0.134
	// on 10+ retained frames of both prompt screens) where the blobless save
	// notices are a small panel (measured centre-panel mean 130-131, text-window
	// edges 0.039 — one or two sparse lines). Both clauses carry margin
	// (155-164 vs 145 vs 130-131; 0.095-0.134 vs 0.07 vs 0.039).
	classPromptBlinkOffMidMin = 145.0 // the prompt's text panel mean (the notices measure 130-131)
	classPromptBlinkOffInkMin = 0.07  // the prompt's dense text (the notices measure 0.039)
	// The name-entry screen (ADR-0043 amendment 5), measured on the 110+ retained
	// frames of the 2026-10-17 run's name-entry screen (the opening-skip step's
	// probes 010-118: the SAME screen in both its dim frame-pulse phase — full-
	// frame mean 104-110 — and its bright phase — mean 142.8, the frame the old
	// clause let fall through to the title class): the keyboard grid's window
	// (x 0.20-0.80, y 0.40-0.75) measures STABLE across the pulse — edges
	// 0.066-0.079, mean 120-130 — where its confusables differ: the blobless
	// save-notice dialog measures namegrid edges 0.021 (a single text line over
	// the dark base) and the create-save prompt's dense panel measures 0.084-
	// 0.086 at grid mean 148-150 (the bright text panel, not the grid). The
	// clause sits between the two with margin on both sides (edges 0.055;
	// grid mean 110-140).
	nameEntryGridInkMin  = 0.055 // the keyboard grid's dense letter strokes (vs the notice's 0.021, below; vs the prompt panel's 0.084+, the mean clause separates)
	nameEntryGridMeanMin = 110.0 // the grid panel's mean floor (measured 123-130)
	nameEntryGridMeanMax = 140.0 // its ceiling (the prompt's text panel measures 148-150)
	// The name entry carries DARK-GRAY top/bottom bands (the letterBand01 of the
	// darker band, measured 0.156-0.173 in both pulse phases across the 110+
	// retained frames), whereas the in-game opening it is followed by is a
	// LETTERBOXED cinematic (PURE-BLACK top/bottom bands, measured lb 0.000,
	// a dense centre text panel, no bottom hint bar, and DOUBLE the grid ink —
	// namegrid edges 0.16-0.22 vs the name entry's 0.079). That opening
	// otherwise passes the ink/mean clauses (its grid-window mean oscillates
	// 110-147, crossing the [110,140] band) and is misread as the name entry —
	// the 2026-10-17 re-run's drive then sent the name-entry action to the
	// wrong screen for 20+ iterations and burned the budget. The measured
	// letterbox FLOOR (0.08, well above the opening's 0.000 and well below the
	// name entry's 0.156) is the separator: it keeps the letterboxed opening
	// out of the name-entry class while every retained name-entry frame passes.
	nameEntryLetterboxMin = 0.08 // the name entry's dark-gray band (measured 0.156-0.173) vs the letterboxed in-game opening's 0.000 (pure-black bars)
	nameEntryLetterboxMax = 0.30 // the ceiling: the title->prompt FADE frame's 0.428 (a half-faded logo over the incoming dialog) is rejected — the fade's grid window otherwise passes the ink/mean clauses (edges 0.085, mean 133), and the name entry's action is A+START+A — an A landing on the incoming create-save prompt (cursor on No) would select NO, the no-save path
	// The quest-log "File Selection" list (measured on this host's retained
	// frames, the no-save path: probe walk w00-w15 at 2 s cadence): a DARK
	// screen (mean 95-106, lower third near-black, bright 0.00-0.034) with
	// the dense slot text (entropy 0.74-0.84) and a BRIGHT box-band window
	// (mean 140-143, edges 0.04-0.05 — the slot panel). Its confusable is
	// the plain info dialog (save notice / no-save screen), which is the
	// same dark base but with sparse 3-line text (entropy 0.52-0.68) and a
	// DARK box-band window (mean 65, edges 0.000, measured) — the entropy
	// clause and the box-band-mean clause are the two measured separators
	// (both sides measured on this host; the box-band edge clause 0.04-0.05
	// vs 0.000 corroborates but the two big-margin clauses carry the split).
	// The bright hint bar it does NOT have (botbar bright 0.000) and its
	// mean (below the dark-family cap) keep it out of nameent (155-159).
	// NOTE: the slot icons' near-white appears only on the first 2-3
	// transition frames (left+right blob 0.024-0.092) and is 0.000 on the
	// settled frames — the blob is NOT a usable class clause.
	classFileSelEntMin  = 0.71  // the quest-log list's dense text (measured 0.74-0.84; the info dialog 0.52-0.68)
	classFileSelBandMin = 100.0 // its box-band window mean (measured 140-143; the info dialog 65, the overall-dark screen 28-52)
	// The opening legend's narration subtitle (amendment 6), measured on the
	// 26 retained legend frames of the 2026-10-17 run (its iters 064-089):
	// the letterboxed storybook panel (PURE-BLACK top/bottom bars, measured
	// lb 0.000) with white narration text in the bottom black bar — two lines
	// measured at y 0.78-0.83 and 0.84-0.88, the bright (>=190) text pixels
	// spanning x 0.155-0.841 across the frames. In the subbar window (y
	// 0.76-0.90, below the panel's measured bottom edge at 0.74): WITH text —
	// bright 0.032-0.046, edges 0.078-0.118; WITHOUT text (a fade, or between
	// subtitles) — bright 0.000, edges <= 0.039. The confusables all measure
	// at the no-text end: the name entry (bright 0.000, edges 0.003), the
	// file selection (0.000, 0.019-0.024), the dialog over the title (0.000,
	// 0.004). The name entry and file selection additionally carry DARK-GRAY
	// bands (measured lb 0.156-0.173) where the legend's bars are PURE BLACK
	// (0.000) — the letterbox clause is the class separator, the bright/edge
	// clauses the text-presence separator (the text fades in and out, so the
	// class is only claimed on a frame that MEASURES the text; a faded frame
	// is the cutscene family and the drive waits on it).
	classStorySubLbMax     = 0.05 // the legend's pure-black bar (measured 0.000; the name entry / file selection 0.156-0.173, the title 0.28-0.60, the prompt 0.167-0.212)
	classStorySubBrightMin = 0.02 // the subbar bright floor (measured 0.032-0.046 with text; 0.000 without)
	classStorySubEdgesMin  = 0.05 // the subbar glyph-edge floor (measured 0.078-0.118 with text; <= 0.039 without, the file selection 0.024)

	// The in-game dialogue box's DARK form (ADR-0054 amendment 1): the box is
	// a dark translucent band (measured boxband mean 46-57 settled) carrying
	// white glyph text (measured edges 0.031 settled) over a rendered scene
	// (measured lb 0.000). The clauses, in separation order:
	//   - lb < 0.08 (the letterbox clause the name entry / dialog family use):
	//     every measured system screen (the create-save / continue-anyway
	//     prompts, the save notices, the name entry) carries the dark-gray
	//     frame bands (measured 0.156-0.212), so a pure-black-banded frame is
	//     a rendered scene or a cinematic, never a system screen — the box's
	//     "blob" is a character in the scene, not a cursor;
	//   - lower-third bright < 0.10 (the prompt clause's sky cap): a settled
	//     box over the dark village scene measures 0.004-0.005, where a
	//     bright lower third would be scene, not box;
	//   - boxband mean < 68: the dark box (measured 46-57 settled single-line;
	//     48.7-64.3 settled multi-line, the 2026-10-18 17:03 first-dialogue
	//     recording's frames 19-29 — the band's luma rises as the scene
	//     behind the translucent overlay changes); the legend's
	//     between-subtitle interludes measure 68-137 in the same window;
	//   - boxband edges >= 0.028: the box's white text ink floor (measured
	//     0.031 settled single-line, 0.040-0.178 multi-line — the ink GROWS
	//     with the line count, so the former 0.05 ceiling retired in
	//     amendment 7; the legend's between-subtitle interludes measure
	//     <= 0.026, the one 0.038 frame also failing the bright clause);
	//   - boxband bright <= 0.08: the flat dark band (measured 0.014 settled
	//     single-line; 0.017-0.075 settled multi-line — the inked frames of
	//     a longer line) where a scene's bright content (the legend's
	//     measured 0.245 frame, the one inked interlude frame's 0.133)
	//     would sit inside the window;
	//   - centre-band text ink <= 0.06 (UNCHANGED — amendment 7's load-
	//     bearing separator): the box's scene above the band is a rendered
	//     scene with sparse glyph ink (measured 0.006-0.043 settled); the
	//     storybook panel's pages carry dense manuscript ink in the centre
	//     band (measured 0.141-0.177) — with the edges ceiling gone, the
	//     storybook pages' boxband edges (measured 0.070+) OVERLAP the
	//     multi-line box's (0.040-0.178), so the pages are now separated by
	//     the centre-band ink (0.141+ vs 0.06, a 2.4x margin) and the band
	//     bright (0.245 vs 0.08, a 3x margin), not the edges range.
	classBoxDarkMaxMean   = 68.0  // the dark box band's luma cap (measured 46-57 single-line, 48.7-64.3 multi-line settled; the legend interludes 68-137)
	classBoxDarkInkMin    = 0.028 // the box text's glyph-edge floor (measured 0.031 single-line, 0.040-0.178 multi-line settled; the legend interludes <= 0.026)
	classBoxDarkBrightMax = 0.08  // the flat dark band's bright-content cap (measured 0.014 single-line, 0.075 multi-line settled; the legend's 0.245 frame, the inked interlude 0.133)
	classBoxDarkTextMax   = 0.06  // the scene's sparse centre-band ink (measured 0.006-0.043 settled; the storybook pages 0.141+)
	classBoxDarkLbMax     = 0.08  // the letterbox ceiling: system screens 0.156-0.212, rendered scenes 0.000 (shares the name entry's band floor)
)

// titleDimMeanMin: the title's dim ("press START") phase's full-frame mean
// floor (amendment 4; measured phase-B frames 90-126 — the flicker band
// dips to 90.4; the create-save prompt 97-102 is separated on the mid-mean
// clause, not the mean, so the floor only has to reach the title's own
// flicker band).
const titleDimMeanMin = 90.0

// cursorSideOf measures the Yes/No cursor side from the left/right option
// windows (the near-white blob, the 2x-ratio test shared by the prompt
// families). The measured WOTW Yes/No screens are the dark dialog family
// (the create-save prompt and the save-failure continue-anyway dialog,
// both ClassContinue — bright centre panel, measured mid mean 160, the
// near-white cursor blob 0.021-0.030 on the cursor side, 0.000 on the
// other); the bright parchment prompt the class order keeps separate
// matches no retained real screen on this host (amendment 4).
func cursorSideOf(le, ri sigRegionStats) CursorSide {
	if le.blob >= classPromptBlobMin && le.blob >= 2*ri.blob {
		return CursorLeft
	}
	if ri.blob >= classPromptBlobMin && ri.blob >= 2*le.blob {
		return CursorRight
	}
	return CursorNone
}

// ClassOf returns the screen family + the prompt cursor side (measured;
// CursorNone on non-prompt screens).
func (s ScreenSig) ClassOf() (ScreenClass, CursorSide) {
	if s.W == 0 {
		return ClassUnknown, CursorNone
	}
	l3, bb, bt, tr := s.Regions["lower3"], s.Regions["boxband"], s.Regions["botbar"], s.Regions["topright"]
	le, ri, tx, md := s.Regions["left"], s.Regions["right"], s.Regions["text"], s.Regions["mid"]
	ng := s.Regions["namegrid"]
	// The opening legend's narration subtitle (amendment 6): CHECKED FIRST
	// because the dark family below would otherwise swallow these frames
	// (measured mean 65-83, l3.dark > 0.85 — the black bar dominates the
	// lower third) and read them as the blobless dialog or the overall-dark
	// cutscene — exactly the misclass the 2026-10-17 re-run's iters 064-089
	// show (the nameentry clause's letterbox FLOOR, amendment 5, already
	// rejects them from the name entry; without this class they fell through
	// to unknown/dialog). The pure-black letterbox clause (measured lb 0.000;
	// every non-letterboxed screen measures 0.15+) reaches only the cinematic
	// frames, and of those only the ones carrying the measured text in the
	// bottom bar pass the bright/edge clauses: a fade or a between-subtitles
	// frame (measured bright 0.000) stays the cutscene family, and the
	// full-frame box scene the drive's other success class is measured at
	// lb 0.25+ (it cannot reach this clause).
	if sub := s.Regions["subbar"]; s.Letterbox < classStorySubLbMax && sub.bright >= classStorySubBrightMin && sub.edges >= classStorySubEdgesMin {
		return ClassStorySub, CursorNone
	}

	// The in-game dialogue box's DARK form (ADR-0054 amendment 1): CHECKED
	// BEFORE the dark family because the family would otherwise swallow these
	// frames (measured mean 75-83, l3.dark > 0.85, mid 126-137) and read the
	// near-white character in the side band (measured blob 0.0256, cx 0.256)
	// as the cursor — the 2026-10-18 dense recording's 576 settled box frames
	// measured "continue:yes" exactly this way, and the legend watch's
	// dialogue-box boundary never fired. The measured clauses above separate
	// the box from the legend's between-subtitle interludes (the same lb
	// 0.000 rendered scenes: the box band is dark, flat and glyph-inked —
	// mean 46-57, edges 0.031, bright 0.014 across the box's whole life —
	// where the interludes vary, mean 68-137, edges <= 0.026, and their one
	// inked frame carries bright 0.133) and from every system screen (the
	// letterbox clause: the prompts and notices measure 0.156-0.212, the
	// scene 0.000).
	// The box band's own clauses are the box's identity (the dark flat band
	// with glyph ink, the sparse centre text, the quiet lower third); the
	// letterbox ceiling below is the scene-side guard.
	boxBandDark := l3.bright < promptLowerBrightMax && bb.mean < classBoxDarkMaxMean && bb.edges >= classBoxDarkInkMin && bb.bright <= classBoxDarkBrightMax && tx.edges <= classBoxDarkTextMax
	// Amendment 2 (2026-10-20, the G8 telescope): the letterbox ceiling keeps
	// the system screens out on dark scenes (measured lb 0.156-0.212), but a
	// BRIGHT outdoor scene (the lookout deck, measured lb 0.255 — the bright
	// sky top band over the mid-luma deck bottom band) also carries the box,
	// and the ceiling alone would reject it. Admit the bright-scene box when
	// the centre panel is NOT a system dialog's bright panel (mid.mean <
	// classDarkDialogMinMid, the dark-family dialog floor; the deck's scene
	// centre measures 90, the dialogs 115+). The box-band clauses above stay
	// the box's identity: a plain bright scene without the dark box band
	// fails bb.mean < classBoxDarkMaxMean and never enters (measured deck
	// boxband mean 71), and a bright-centred system screen with the band
	// inked still fails the mid clause.
	if boxBandDark && (s.Letterbox < classBoxDarkLbMax || s.Regions["mid"].mean < classDarkDialogMinMid) {
		return ClassBox, CursorNone
	}

	// The dark system screens: a near-black lower third and a low full-frame
	// mean. They split on the centre panel: the system DIALOG carries a bright
	// centre panel (the save notices, the reading state — measured mid mean
	// 160) where the overall-dark screen (the near-black transition/notice)
	// has a dark centre (measured mid mean 28-52). The dialog family splits
	// again on the cursor blob (the measured negative check, ADR-0043
	// amendment 2): the "cannot be saved — continue anyway?" Yes/No dialog
	// carries the near-white cursor blob (measured 0.023 on the cursor side,
	// 0.000 on the other) where the plain save-notice / no-save info dialog
	// carries none (0.000 both sides, measured).
	if s.Mean < classDarkMaxMean && l3.dark > classDarkMinDark {
		if s.Regions["mid"].mean >= classDarkDialogMinMid {
			side := cursorSideOf(le, ri)
			if le.blob >= classPromptBlobMin || ri.blob >= classPromptBlobMin {
				return ClassContinue, side
			}
			// The cursor is blinked OFF (the measured prompt blinks ~1 s). The
			// dense text panel keeps the frame on the prompt family with the
			// side UNKNOWN (the drive waits for the measured side — it never
			// guesses A onto a side it has not measured): without this clause
			// the blink-off create-save prompt reads as a save notice and the
			// drive's notice action (A) lands on the prompt with the cursor on
			// No, selecting the no-save path. The LETTERBOX clause keeps the
			// in-game opening out of the prompt family: the opening is a
			// letterboxed cinematic (measured lb 0.000, pure-black bars) whose
			// dense centre text panel (tx.edges 0.14-0.22) and mid band (110-151)
			// otherwise satisfy the blink-off ink/mid clauses on its bright
			// frames — the 2026-10-17 re-run read those frames as a blinked-off
			// prompt and sent the prompt action (wait / main-stick) to the
			// opening instead of advancing it. Every measured Yes/No prompt is
			// letterboxed (measured lb 0.167-0.212), so the floor (0.08, shared
			// with the name entry's band) keeps them in and the opening out.
			if md.mean >= classPromptBlinkOffMidMin && tx.edges >= classPromptBlinkOffInkMin && s.Letterbox >= nameEntryLetterboxMin {
				return ClassContinue, CursorNone
			}
			// Every measured system dialog carries the DARK-GRAY frame bands
			// (measured lb 0.156-0.173). A LETTERBOXED frame (pure-black bars,
			// measured lb 0.000) with a bright centre and no cursor blob is a
			// CINEMATIC, not a dialog: the opening legend's storybook panel
			// (measured mid 110-151 over its art) otherwise reads as the
			// blobless save notice on its fade frames — the 2026-10-17 re-run's
			// iter-069 measured mid 131, lb 0.000, and the old branch labelled
			// it the save notice, whose drive action is A — an A landing on the
			// legend SKIPS the narration the pipeline must record. The same
			// letterbox floor (0.08) that keeps the letterboxed opening out of
			// the prompt family keeps it out of the dialog family: the frame
			// falls through to unknown/cutscene and the drive waits on it
			// (the legend advances on its own — nothing is pressed on it).
			if s.Letterbox >= nameEntryLetterboxMin {
				return ClassDialog, CursorNone
			}
		}
		return ClassDark, CursorNone
	}
	// The name-entry screen (amendment 5): the keyboard grid's dense letter
	// strokes, measured STABLE across the screen's frame pulse (the old
	// bright-hint-bar clause matched only the bright phase — the 2026-10-17
	// run read the dim phase as filesel and the bright phase as title, and
	// the linear drive then sat on the screen for 166 s). The grid window's
	// measured clauses (110+ retained frames: edges 0.066-0.079, mean
	// 120-130) separate the screen from the blobless save notice (edges
	// 0.021, a single text line) and the create-save prompt (edges 0.084+
	// at panel mean 148-150 — the mean clause keeps the prompt's bright text
	// panel out); the dark family above keeps the prompts that DO enter it
	// (measured mid 160 + l3.dark > 0.85) on their own classes; the letterbox
	// clause rejects the title->prompt FADE frame (measured lb 0.428; the
	// screen itself measures 0.166-0.173 in both pulse phases). CHECKED
	// BEFORE filesel: the quest-log clause's measured frames were, it turns
	// out, this screen mislabelled (amendment 5), so the grid must win the
	// overlap.
	if ng.edges >= nameEntryGridInkMin && ng.mean >= nameEntryGridMeanMin && ng.mean <= nameEntryGridMeanMax && s.Letterbox >= nameEntryLetterboxMin && s.Letterbox < nameEntryLetterboxMax {
		return ClassNameEnt, CursorNone
	}
	// The quest-log "File Selection" list (measured on this host): a DARK
	// screen (mean 95-106) whose near-black lower third is diluted by the
	// mid-luma slot panel (l3.dark ~0.80 — it does NOT enter the dark family
	// above) carrying the dense slot text (entropy 0.74-0.84) and the bright
	// slot panel in the box-band window (mean 140-143). Checked AFTER the
	// dark family so the dialog / continue screens (which DO enter the dark
	// family) keep their classes; its two big-margin clauses (entropy,
	// box-band mean) split it from the blobless info dialog (0.52-0.68, 65);
	// the letterbox clause (amendment 4, measured 0.15-0.22 here vs the
	// title's dim phase 0.28-0.56) keeps the title's dim flicker out — the
	// title flicker is what the 2026-10-17 run's probe frames leaked into
	// this class (mean 97-107, entropy 0.74-0.81, box-band 102-115, letterbox
	// 0.28-0.32).
	if s.Mean < classDarkMaxMean && s.Entropy >= classFileSelEntMin && bb.mean >= classFileSelBandMin && l3.bright < promptLowerBrightMax && bt.bright < classHintBarBrightMin && s.Letterbox < letterboxTitleMin {
		return ClassFileSel, CursorNone
	}
	// The bright full-frame letter (the opening monologue): bright mean, a
	// smooth bright box band, no glyph ink in the panel. The title carries a
	// smooth bright box band too (measured boxband mean 170, edges 0.000) but
	// no bright bottom hint bar (measured 0.260) and no bright top-right
	// (0.000), so those clauses keep it out (measured letter: botbar bright
	// 0.941, topright bright 0.40-0.60).
	if s.Mean >= classLetterMeanMin && bb.mean >= classLetterBandMin && bb.edges < 0.02 && tx.edges < 0.05 && bt.bright >= classHintBarBrightMin && tr.bright >= classLetterTopBright {
		return ClassLetter, CursorNone
	}
	// The title's DIM phase (the "press START" phase) and its flicker
	// (ADR-0043 amendment 4): the 2026-10-17 real run proved this phase is
	// what the old prompt clause misread as the memory-card prompt — the
	// START step "passed" on the dim title, the START press landed on the
	// bright logo phase (measured ignored there) and the screen stayed on
	// the title for the whole drive. Measured phase-B frames: mean
	// 90-126, letterbox 0.28-0.56, lower-third bright 0.00-0.075,
	// centre-panel (mid) mean 100-132, bottom bar mean 112-118 (the hint
	// text) — where the dark create-save prompt (the ClassContinue family
	// below) carries the bright centre panel (measured mid 160) and the
	// quest-log list is NOT letterboxed (measured 0.15-0.22). The mid-mean
	// and letterbox clauses are the two measured separators; the mean cap
	// (< 130) keeps the bright full-frame screens (letter/nameent 155-159)
	// out, and the lower-third clause keeps the box frames out (the box band
	// lights the lower third, measured bright 0.12+).
	if s.Letterbox >= letterboxTitleMin && s.Mean >= titleDimMeanMin && s.Mean < classLetterMeanMin && l3.bright < promptLowerBrightMax && md.mean < classDialogMinMid {
		return ClassTitle, CursorNone
	}
	// The Yes/No parchment prompt: a bright panel over a dark-sky lower
	// third. NOTE (amendment 4): the real create-save prompt measured on
	// this host is the DARK dialog family (the ClassContinue class — bright
	// centre panel, mid mean 160, the cursor blob); this clause matches no
	// retained real screen, only the synthetic doubles and the letterboxed
	// bright frames the title clauses above must reject first.
	if s.Letterbox > letterboxTitleMin && l3.bright < promptLowerBrightMax && tx.mean >= classPromptPanelMin {
		return ClassPrompt, cursorSideOf(le, ri)
	}
	// A rendered scene carrying the dialogue box band (the structural
	// BoxPresent clauses).
	if bb.mean >= boxBandMeanMin && bb.edges >= boxBandEdgesMin && s.Entropy < boxBandEntropyMax {
		return ClassBox, CursorNone
	}
	// The name-entry screen: the bright bottom hint bar carrying the
	// keyboard grid's dense letter strokes (the quest-log list does NOT
	// carry the hint bar — measured 0.000 — and its dark mean keeps it out
	// of this branch).
	if bt.bright >= classHintBarBrightMin && tx.edges >= classGridInkMin {
		return ClassNameEnt, CursorNone // the keyboard grid's dense letter strokes
	}
	// The title: a full-frame scene with the bright logo lower third.
	if s.Letterbox > letterboxTitleMin && l3.bright >= classTitleLowerBright {
		return ClassTitle, CursorNone
	}
	if s.Letterbox > letterboxTitleMin {
		return ClassScene, CursorNone
	}
	return ClassUnknown, CursorNone
}

// Key is the loop-guard comparison: the class plus the prompt cursor side
// (the two Yes/No prompt screens share their respective classes with the
// plain dialog, and the cursor side is what keeps "the same prompt again"
// detectable — the create-save prompt and the continue-anyway dialog each
// loop with their own key; two frames of the same screen at different times
// still share the key — cloud drift changes the mean, not the class).
func (s ScreenSig) Key() string {
	c, side := s.ClassOf()
	if c == ClassPrompt || c == ClassContinue {
		return string(c) + ":" + string(side)
	}
	return string(c)
}

// RealBoxForm reports whether the frame measures the classifier's DARK box
// form — the measured shape of the real WOTW in-game dialogue box: the dark
// translucent band (settled mean 46-64, ADR-0054 amendment 1) carrying white
// glyph text over a rendered scene (letterbox 0.000). It is ClassBox PLUS
// the box band below the bright clause's floor (bb.mean < boxBandMeanMin):
// ClassOf reaches ClassBox through the structural BRIGHT clause as well
// (bb.mean >= boxBandMeanMin, edges >= boxBandEdgesMin, entropy <
// boxBandEntropyMax), and that bright form matches more than the real box —
// the pre-box animated scene (measured band mean 76-152; the run-2 stop
// frame 130.5/0.066) and the name-entry screen (measured band mean 118 /
// edges 0.096 at letterbox 0.297, whose dim-pulse frames also pass the grid
// clauses' neighbourhood — the 2026-10-19 test-full drive closed on such a
// frame as the dialogue box at 25 s, the false success this predicate exists
// to reject: under -race + full parallel load the name-entry screen's pulse
// produced a frame the bright clause swallowed). The drive's dialogue_box
// SUCCESS and the post-drive recheck therefore accept only the dark form;
// a bright-form ClassBox frame is a scene frame the loop keeps observing.
func (s ScreenSig) RealBoxForm() bool {
	if c, _ := s.ClassOf(); c != ClassBox {
		return false
	}
	return s.Regions["boxband"].mean < boxBandMeanMin
}
