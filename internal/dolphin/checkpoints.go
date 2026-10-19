package dolphin

import (
	"context"
	"fmt"
	"image"
	"math"
	"time"
)

// Checkpoint factories for RunScript (docs/spec/11 step 3: "apply
// DTM/controller sequence and checkpoint screenshots"). Each factory
// returns a fresh Checkpoint bound to one run (the per-run state closures
// — last samples, first timestamps — never leak across steps; the script
// builders allocate one instance per step). The predicates are ports of
// the readiness helpers (frame.go, readiness.go) so the calibrated
// thresholds (letterboxTitleMin, sceneEntropyMin, crossStepMax, ...) stay
// one set of numbers for the whole package.
//
// Thresholds marked "calibrated" were measured on screenshots captured by
// this harness (see ADR-0043 for the measurements and the evidence paths).

// textbox thresholds for the lower-third dialogue-box predicate. The box is
// a bright rounded band (parchment/white) carrying dark glyph-like text, so
// the three measured clauses are: a bright band is present, dark glyphs are
// present in the lower third, and the lower-third luma contrast is high (a
// sky/sea band has brightness but not the bright-plus-dark mixture).
const (
	textboxBrightMin   = 0.12 // fraction of lower-third luma >= 190 (the box band)
	textboxDarkMin     = 0.03 // fraction of lower-third luma <= 90  (the glyphs)
	textboxContrastMin = 35.0 // luma stddev of the lower third (0..255 scale)
	textboxBrightLuma  = 190
	textboxDarkLuma    = 90

	// promptLowerBrightMax: the memory-card prompt's lower third is bare sky
	// (measured bright fraction 0.031); the title's lower third is its logo
	// text (measured 0.233-0.337). 0.10 sits between the two measured sets
	// with margin on both sides.
	promptLowerBrightMax = 0.10

	// Box-band window: the in-game dialogue box sits in the lower-middle of
	// the frame (measured on this host's captured first-dialogue screen,
	// ADR-0043): x in [0.31, 0.75], y in [0.69, 0.84] of the frame.
	boxBandX0, boxBandX1 = 0.31, 0.75
	boxBandY0, boxBandY1 = 0.69, 0.84

	// Box-band thresholds, calibrated on this host's captured evidence
	// (ADR-0043): box frames show band mean 128-137 with horizontal-edge
	// fraction 0.083-0.14; title/prompt screens have the same brightness
	// band but 0.000 edges; letter-scroll frames have edges 0.084-0.122
	// (indistinguishable from the box on the band alone) but full-frame
	// luma entropy 0.84-0.87 vs 0.44-0.50 on box scenes, so the entropy
	// clause is what rejects the scroll.
	boxBandMeanMin    = 100.0 // luma, box-band window mean
	boxBandEdgesMin   = 0.06  // fraction of horizontal |dluma| >= 30
	boxBandEdgeDelta  = 30    // luma steps counted as a glyph edge
	boxBandEntropyMax = 0.70  // full-frame 256-bin luma entropy (bits/px)
)

// lowerThirdLuma extracts the luma of the bottom third of the image.
func lowerThirdLuma(img image.Image) []uint8 {
	w, h, px := luma(img)
	if px == nil || h < 3 {
		return nil
	}
	third := h / 3
	if third < 1 {
		third = 1
	}
	out := make([]uint8, 0, w*third)
	for y := h - third; y < h; y++ {
		out = append(out, px[y*w:]...)
	}
	return out
}

// textboxStats measures the lower-third band: bright fraction (>= 190),
// dark fraction (<= 90) and luma standard deviation.
func textboxStats(img image.Image) (bright, dark, contrast float64) {
	px := lowerThirdLuma(img)
	if len(px) == 0 {
		return 0, 0, 0
	}
	var (
		b, d  int
		sum   float64
		sumSq float64
	)
	for _, v := range px {
		f := float64(v)
		sum += f
		sumSq += f * f
		if v >= textboxBrightLuma {
			b++
		}
		if v <= textboxDarkLuma {
			d++
		}
	}
	n := float64(len(px))
	mean := sum / n
	contrast = math.Sqrt(math.Max(0, sumSq/n-mean*mean))
	return float64(b) / n, float64(d) / n, contrast
}

// TextboxPresent returns a Checkpoint that holds when the lower third of
// the screen shows a dialogue box: a bright band with high-contrast dark
// glyphs (the measured clauses above). Stateless — safe to evaluate on any
// sample.
func TextboxPresent() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		bright, dark, contrast := textboxStats(cur)
		ok := bright >= textboxBrightMin && dark >= textboxDarkMin && contrast >= textboxContrastMin
		return ok, fmt.Sprintf("lower3 bright=%.3f dark=%.3f contrast=%.1f (want >= %.2f/%.2f/%.0f)",
			bright, dark, contrast, textboxBrightMin, textboxDarkMin, textboxContrastMin)
	}
}

// PromptCard holds when the screen is the memory-card save-file prompt:
// a bright full-frame panel over a moving sky background. Measured on this
// host's captured prompt (2026-10-17, ADR-0043) against the title it
// follows: letterbox band 0.317 (spike: 0.345) vs the letterboxed movie's
// 0.000, and the lower third is bare sky (bright 0.031, dark 0.896, luma
// contrast 35.8) where the title's lower third carries the bright logo text
// (bright 0.233-0.337). The prompt NEVER settles (its clouds drift 0.115
// between 2 s samples, just inside the title-orbit band), so the predicate
// is bright-full-frame + dark-lower-third — not a stability test. It cannot
// fire on the title (bright clause) or the movie (lb clause).
func PromptCard() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		lb := letterBand01(cur)
		bright, dark, contrast := textboxStats(cur)
		ok := lb > letterboxTitleMin && bright < promptLowerBrightMax
		return ok, fmt.Sprintf("lb=%.3f want > %.2f; lower3 bright=%.3f want < %.2f (dark=%.3f contrast=%.1f)",
			lb, letterboxTitleMin, bright, promptLowerBrightMax, dark, contrast)
	}
}

// boxBandStats measures the dialogue-box band: the mean luma and the
// fraction of horizontal edges (|dluma| >= boxBandEdgeDelta) inside the
// box-band window. The box is a bright parchment carrying dark glyph
// strokes, so a box frame shows a bright band with a dense high-contrast
// horizontal-edge signature; smooth gradients (title sky, movie sky, the
// memory-card panel) show near-zero edge fraction at similar brightness.
func boxBandStats(img image.Image) (mean, edges float64) {
	w, h, px := luma(img)
	if w == 0 || h == 0 {
		return 0, 0
	}
	x0, x1 := int(boxBandX0*float64(w)), int(boxBandX1*float64(w))
	y0, y1 := int(boxBandY0*float64(h)), int(boxBandY1*float64(h))
	if x1-x0 < 4 || y1-y0 < 2 {
		return 0, 0
	}
	var sum float64
	var n, edge int
	for y := y0; y < y1; y++ {
		for x := x0 + 1; x < x1; x++ {
			a, c := px[y*w+x], px[y*w+x-1]
			sum += float64(a)
			n++
			if d := int(a) - int(c); d >= boxBandEdgeDelta || d <= -boxBandEdgeDelta {
				edge++
			}
		}
	}
	if n == 0 {
		return 0, 0
	}
	return sum / float64(n), float64(edge) / float64(n)
}

// lumaEntropy computes the normalised (÷8) 256-bin Shannon entropy of the
// luma surface — the same 0..1 scale as entropy01. Measured on this host:
// letter-scroll frames 0.84-0.87 (textured parchment plus dense text), box
// scenes 0.44-0.50, title ~0.57-0.62.
func lumaEntropy(img image.Image) float64 {
	w, h, px := luma(img)
	if w == 0 || h == 0 {
		return 0
	}
	hist := make([]int, 256)
	for _, v := range px {
		hist[v]++
	}
	total := len(px)
	e := 0.0
	for _, c := range hist {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(total)
		e -= p * math.Log2(p)
	}
	return e / 8
}

// BoxPresent holds when the structural dialogue-box signature is on screen
// in EITHER measured form (ADR-0054 amendment 1): the bright parchment band
// (the synthetic doubles and the letter's box: box-band mean >=
// boxBandMeanMin, horizontal edges >= boxBandEdgesMin, full-frame entropy <
// boxBandEntropyMax — the scroll separator) or the real WOTW dark
// translucent box (the measured dark-form clauses below). Both forms are
// exactly the classifier's ClassBox (signature.go carries both measured
// shapes), so the checkpoint and the drive's screen family cannot diverge:
// a frame the checkpoint calls a box is the frame the drive's loop
// terminates on. The legacy bright clauses are kept as the fallback form
// (the synthetic doubles' calibration, ADR-0043) because the dark form's
// flat-band shape could one day read a bright box's fade frame and vice
// versa; the OR of the two measured shapes is what "a dialogue box is on
// screen" means here.
func BoxPresent() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, _ := sig.ClassOf()
		if c == ClassBox {
			return true, "screen class=box (" + sig.SigLine() + ")"
		}
		mean, edges := boxBandStats(cur)
		ent := lumaEntropy(cur)
		ok := mean >= boxBandMeanMin && edges >= boxBandEdgesMin && ent < boxBandEntropyMax
		return ok, fmt.Sprintf("screen class=%s; boxband mean=%.0f want >= %.0f; edges=%.3f want >= %.2f; entropy=%.3f want < %.2f",
			c, mean, boxBandMeanMin, edges, boxBandEdgesMin, ent, boxBandEntropyMax)
	}
}

// Screen-specific checkpoints (the drive's per-step gates). Each holds when
// the measured screen signature (ADR-0043) identifies a SPECIFIC screen —
// not merely "the scene changed" — so a step cannot pass on the wrong screen
// (the false positive the two-prompt loop produced). The class and the
// prompt cursor side are measured, not assumed; the detail line reports the
// measured class so a failing trace shows exactly which screen was on
// screen.

// ScreenIsClass holds when the screen signature's class is target. It is the
// screen-specific gate the drive steps on (the memory-card prompt, the
// continue-anyway dialog, the file-select grid, the name-entry grid, the
// save notice, the letter, the box). The detail reports the measured class
// so a failing trace names the screen that was actually on screen.
func ScreenIsClass(target ScreenClass) Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, side := sig.ClassOf()
		ok := c == target
		detail := fmt.Sprintf("screen class=%s want %s (key=%s)", c, target, sig.Key())
		if c == ClassPrompt || c == ClassContinue {
			detail = fmt.Sprintf("screen class=%s cursor=%s want %s (key=%s)", c, side, target, sig.Key())
		}
		return ok, detail
	}
}

// PromptCursor holds when the screen is the Yes/No memory-card prompt with
// the cursor measured on the wanted side. CursorLeft = the left option
// (Yes), CursorRight = the right option (No). This is the gate the drive
// uses before confirming an answer: it proves the cursor moved to the option
// the press will select, so the following A cannot silently pick the other.
func PromptCursor(side CursorSide) Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, got := sig.ClassOf()
		ok := c == ClassPrompt && got == side
		return ok, fmt.Sprintf("prompt cursor=%s want %s (class=%s; key=%s)", got, side, c, sig.Key())
	}
}

// ContinuePrompt holds on the dark Yes/No dialog family: the dialog base
// (dark lower third + bright centre panel, measured mid mean 160) carrying
// the near-white cursor blob on either side — the measured split from the
// plain save-notice / no-save info dialog, which carries no blob (ADR-0043
// amendment 2). This family carries BOTH measured WOTW Yes/No screens
// (amendment 4): the post-START create-save prompt (probe8 frames p02-p04)
// and the save-failure continue-anyway dialog. It is the screen-specific
// step the task requires: a save failure is handled on THIS screen, never
// confused with the info dialog (blobless) or the title's dim phase (which
// is the title class now, not a prompt).
func ContinuePrompt() Checkpoint { return ScreenIsClass(ClassContinue) }

// ContinuePromptYes holds on the continue-anyway dialog with the cursor
// measured on the LEFT option (Yes) — the strict gate that must hold before
// the confirming A press (the task's "move the cursor to Yes, verify
// cursor-on-Yes"): unlike CursorNotNo's leniency (the near-white parchment
// panel makes the side test flaky), the dark dialog's cursor blob is clean,
// so the strict side is required here.
func ContinuePromptYes() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, got := sig.ClassOf()
		ok := c == ClassContinue && got == CursorLeft
		return ok, fmt.Sprintf("continue dialog cursor=%s want yes (class=%s; key=%s)", got, c, sig.Key())
	}
}

// MemoryCardPrompt holds on the post-START "create a save file?" Yes/No
// prompt with the cursor measured on the RIGHT option (No) — the measured
// entry state (ADR-0043 amendment 4, probe8 retained frames p02/p03:
// mean 99.7-101.6, centre-panel mid mean 160, right.blob 0.0212-0.0238
// cx 0.559-0.560, left.blob 0.000). The prompt is the DARK dialog family
// (the ClassContinue class — bright centre panel over a dark base, the
// near-white cursor blob) — the same measured shape as the save-failure
// continue-anyway dialog. The old bright-parchment gate (ClassPrompt) is
// gone: it matched the title's dim "press START" phase instead (the false
// pass of the 2026-10-17 real run).
func MemoryCardPrompt() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, got := sig.ClassOf()
		ok := c == ClassContinue && got == CursorRight
		return ok, fmt.Sprintf("create-save prompt: class=%s cursor=%s want continue:no (key=%s)", c, got, sig.Key())
	}
}

// MemoryCardPromptYes holds on the create-save prompt with the cursor
// measured on the LEFT option (Yes) — the strict gate that must hold before
// the confirming A press (ADR-0043 amendment 4, probe8 retained frame
// p04: left.blob 0.0296 cx 0.445, right.blob 0.000, cursor side = yes,
// after the main-stick-left 900 ms input). The dark prompt's cursor blob is
// clean (0.021-0.030 on the cursor side, 0.000 on the other), so the
// strict side is the gate, not a leniency.
func MemoryCardPromptYes() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, got := sig.ClassOf()
		ok := c == ClassContinue && got == CursorLeft
		return ok, fmt.Sprintf("create-save prompt cursor=%s want yes (class=%s; key=%s)", got, c, sig.Key())
	}
}

// SaveNotice holds on the system DIALOG the save-file operation shows (the
// "creating/created" notice, the "no save file" info, the reading state): a
// bright centre panel over a dark background, carrying NO cursor blob
// (ADR-0043 measured the notices at full-frame mean 85-90, lower-third dark
// 0.84-0.89, centre-panel mean 160). It is deliberately NOT the prompt
// (bright), NOT the overall-dark screen (dark centre), NOT the continue
// dialog (which carries the cursor blob) and NOT the quest-log file
// selection (dark screen with the bright slot panel in the box band, mean
// 140-143 — the dialog's box band is dark, measured 65), so a step cannot
// pass on any of them.
func SaveNotice() Checkpoint { return ScreenIsClass(ClassDialog) }

// CursorNotNo holds on the Yes/No prompt with the cursor measured OFF the
// right (No) option — that is, on the left (Yes) or ambiguous. The drive no
// longer uses it: the measured failure it caused (ADR-0043 amendment 3) was
// the ambiguous cursor=- frames — the d-pad input leaves the cursor where it
// was, the side test reads "-" on the flaky parchment frames, and the gate
// passed while the cursor was still on No. The move-cursor steps gate on the
// strict cursor-on-Yes predicates instead (MemoryCardPromptYes /
// ContinuePromptYes), with the runner's loop guard as the backstop.
func CursorNotNo() Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		sig := ScreenSigOf(cur)
		c, side := sig.ClassOf()
		ok := c == ClassPrompt && side != CursorRight
		return ok, fmt.Sprintf("prompt cursor=%s (want off No; class=%s; key=%s)", side, c, sig.Key())
	}
}

// FileSelect holds on the quest-log "File Selection" screen: the dark screen
// with the dense slot text (entropy 0.74-0.84) and the bright slot panel in
// the box-band window (mean 140-143) — the measured class (ADR-0043
// amendment 3). It is split from the confusable blobless info dialog on the
// two big-margin clauses (entropy, box-band mean — the dialog measures
// 0.52-0.68 and 65); the bright hint bar it does NOT have (botbar bright
// 0.000) keeps it out of nameent (155-159).
func FileSelect() Checkpoint { return ScreenIsClass(ClassFileSel) }

// NameEntry holds on the character-name-entry screen: the keyboard grid's
// dense letter strokes (panel ink above the measured grid threshold) plus the
// bright bottom hint bar (ADR-0043). It is the dense-ink class the
// file-select screen does not satisfy.
func NameEntry() Checkpoint { return ScreenIsClass(ClassNameEnt) }

// OpeningLetter holds on the bright full-frame opening monologue (the
// letter): bright mean, a smooth bright box band, a bright bottom hint bar
// and a bright top-right header (ADR-0043; the title carries the same smooth
// bright band but no hint bar and no top-right, so it cannot fire on the
// title).
func OpeningLetter() Checkpoint { return ScreenIsClass(ClassLetter) }

// SceneChanged returns a Checkpoint holding when the current sample differs
// from the step-start reference by more than min RMSE (320x240 luma, the
// spike's working size). The reference is the ref the runner passes (the
// screenshot taken before the step's first input).
func SceneChanged(min float64) Checkpoint {
	return func(_ context.Context, cur, ref image.Image) (bool, string) {
		d := rmse01(cur, ref, normW, normH)
		return d > min, fmt.Sprintf("rmse=%.3f want > %.2f", d, min)
	}
}

// SceneChangedFrom holds when the current sample differs from the fixed ref
// image by more than min RMSE.
func SceneChangedFrom(ref image.Image, min float64) Checkpoint {
	return func(_ context.Context, cur, _ image.Image) (bool, string) {
		d := rmse01(cur, ref, normW, normH)
		return d > min, fmt.Sprintf("rmse=%.3f want > %.2f (fixed ref)", d, min)
	}
}

// Letterbox holds while the screen is letterboxed: the lower of the top and
// bottom bands is dark (the readiness helper's camera-motion-immune band
// test; the opening movie measured 0.000, the full-frame title 0.33-0.60).
func Letterbox(maxBand float64) Checkpoint {
	return func(_ context.Context, cur image.Image, _ image.Image) (bool, string) {
		band := letterBand01(cur)
		return band < maxBand, fmt.Sprintf("band=%.3f want < %.2f", band, maxBand)
	}
}

// StableFor holds once the screen has shown the same scene (RMSE <= tol at
// the 320x240 working size) for at least d of the run's polling. It keeps
// the first sample it sees; later evaluations compare against it. A
// mid-transition sample resets the anchor, so the duration is measured on
// one settled stretch, not on a fade. tol <= 0 uses the readiness settled
// threshold (crossStepMax).
func StableFor(dur time.Duration, tol float64) Checkpoint {
	if tol <= 0 {
		tol = crossStepMax
	}
	var (
		anchorLuma []uint8
		anchorAt   time.Time
	)
	return func(_ context.Context, cur, _ image.Image) (bool, string) {
		l := normalise(cur, normW, normH)
		if len(l) == 0 {
			return false, "no luma"
		}
		if anchorLuma == nil {
			anchorLuma, anchorAt = l, time.Now()
			return false, "anchor set at t=0"
		}
		d := rmseSlice(l, anchorLuma)
		elapsed := time.Since(anchorAt)
		if d > tol {
			// The screen left the anchored scene: re-anchor (a fade or a
			// new scene starts a fresh stability window).
			anchorLuma, anchorAt = l, time.Now()
			return false, fmt.Sprintf("rmse=%.3f > %.2f; re-anchored", d, tol)
		}
		ok := elapsed >= dur
		return ok, fmt.Sprintf("dt=%.1fs want >= %s; rmse=%.3f want <= %.2f",
			elapsed.Seconds(), dur, d, tol)
	}
}

// Settled holds when two CONSECUTIVE run samples show a stable full-frame
// scene (RMSE <= crossStepMax between them, rendered, not letterboxed):
// the shape of a new settled screen (a prompt, a menu). The title's camera
// orbit never settles (measured: it moves >= crossStepMax between 1 s
// samples), so a settled stretch after input is a new scene.
func Settled() Checkpoint {
	var (
		prevLuma []uint8
		prevOK   bool
	)
	return func(_ context.Context, cur, _ image.Image) (bool, string) {
		l := normalise(cur, normW, normH)
		rendered := letterBand01(cur) > letterboxTitleMin && entropy01(cur) > sceneEntropyMin
		ok := false
		detail := fmt.Sprintf("rendered=%v", rendered)
		if len(l) > 0 && prevOK && rendered {
			d := rmseSlice(l, prevLuma)
			ok = d < crossStepMax
			detail = fmt.Sprintf("step=%.3f want < %.2f", d, crossStepMax)
		}
		prevLuma, prevOK = l, rendered && len(l) > 0
		return ok, detail
	}
}

// FullFrameScene holds when two consecutive run samples both show a
// rendered full-frame scene (the seekScene rule: letterbox band bright AND
// entropy high) — the boot-logo-to-title transition's predicate.
func FullFrameScene() Checkpoint {
	var prev bool
	return func(_ context.Context, cur, _ image.Image) (bool, string) {
		lb := letterBand01(cur)
		ent := entropy01(cur)
		full := lb > letterboxTitleMin && ent > sceneEntropyMin
		ok := full && prev
		prev = full
		return ok, fmt.Sprintf("lb=%.3f ent=%.2f (two consecutive full-frame)", lb, ent)
	}
}

// All combines checkpoints with AND (the runner's Details show every
// clause's measurement, which is what a failing trace needs).
func All(cps ...Checkpoint) Checkpoint {
	return func(ctx context.Context, cur, ref image.Image) (bool, string) {
		ok := true
		parts := make([]string, 0, len(cps))
		for i, c := range cps {
			oci, detail := c(ctx, cur, ref)
			parts = append(parts, fmt.Sprintf("%d:%s", i, detail))
			if !oci {
				ok = false
			}
		}
		return ok, joinAll(parts)
	}
}

// Any combines checkpoints with OR: it holds when ANY clause holds (the
// screen-specific alternatives the drive steps can land on — the
// create-save step may resolve to the save notice, the file-select, the
// continue-anyway dialog or the name entry, and every one of those is a
// MEASURED class, never "changed"). The detail line carries every clause's
// measurement, so a failing trace shows what each alternative saw.
func Any(cps ...Checkpoint) Checkpoint {
	return func(ctx context.Context, cur, ref image.Image) (bool, string) {
		parts := make([]string, 0, len(cps))
		ok := false
		for i, c := range cps {
			oci, detail := c(ctx, cur, ref)
			parts = append(parts, fmt.Sprintf("%d:%s", i, detail))
			if oci {
				ok = true
			}
		}
		return ok, joinAll(parts)
	}
}

func joinAll(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += " " + p
		} else {
			out = p
		}
	}
	return out
}
