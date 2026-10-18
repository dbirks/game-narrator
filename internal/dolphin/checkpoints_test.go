package dolphin

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// letterboxed paints black bands over the top and bottom of a bright
// scene: the synthetic letterboxed movie frame.
func letterboxed(w, h int) image.Image {
	img := noisy(w, h, 7)
	for y := 0; y < h/2 && y < lbRows; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{})
		}
	}
	for y := h - 1; y >= h-1-h/2 && y >= h-lbRows; y-- {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{})
		}
	}
	return img
}

// TestTextboxPresentClauses: the synthetic box clears every clause; a
// mid-dark sea (the title's lower third, measured bright 0.012 / dark
// 0.770 / contrast 30) clears none of them.
func TestTextboxPresentClauses(t *testing.T) {
	ctx := context.Background()
	box := brightBox(320, 240)
	sea := solid(320, 240, 90)
	ok, detail := TextboxPresent()(ctx, box, box)
	if !ok {
		t.Errorf("synthetic box must pass TextboxPresent: %s", detail)
	}
	ok, detail = TextboxPresent()(ctx, sea, sea)
	if ok {
		t.Errorf("a mid-dark screen must not pass TextboxPresent: %s", detail)
	}
	// Measured numbers: the title's lower third.
	b, d, c := textboxStats(sea)
	if b > 0.05 || d < 0.9 || c > 1 {
		t.Errorf("solid sea stats drifted (bright %.3f dark %.3f contrast %.1f)", b, d, c)
	}
}

// TestSceneChanged: the RMSE floor distinguishes "same screen" from "a
// different one".
func TestSceneChanged(t *testing.T) {
	ctx := context.Background()
	a, b := noisy(320, 240, 1), noisy(320, 240, 2)
	cp := SceneChanged(0.10)
	if ok, _ := cp(ctx, a, a); ok {
		t.Error("identical images must not count as a scene change")
	}
	if ok, detail := cp(ctx, b, a); !ok {
		t.Errorf("distinct scenes must count as a change: %s", detail)
	}
	// SceneChangedFrom pins the reference (the task's scene_changed_from).
	fixed := SceneChangedFrom(a, 0.10)
	if ok, _ := fixed(ctx, b, sea240()); !ok {
		t.Error("SceneChangedFrom must compare against the fixed image, not the runner's ref")
	}
}

func sea240() image.Image { return solid(320, 240, 90) }

// TestLetterbox: the band test separates the letterboxed movie from the
// full-frame title (the readiness helper's camera-motion-immune clause).
func TestLetterbox(t *testing.T) {
	ctx := context.Background()
	movie := letterboxed(320, 240)
	title := noisy(320, 240, 1)
	cp := Letterbox(letterboxMovieMax)
	if ok, detail := cp(ctx, movie, movie); !ok {
		t.Errorf("letterboxed screen must read letterboxed: %s", detail)
	}
	if ok, detail := cp(ctx, title, title); ok {
		t.Errorf("full-frame screen must not read letterboxed: %s", detail)
	}
}

// promptLike paints the synthetic memory-card prompt: a bright sky scene
// with a mid-dark (cloud) lower third — the measured prompt's structure
// (lb 0.317, lower3 bright 0.031), built to clear both PromptCard clauses.
func promptLike(w, h int) *image.RGBA {
	img := noisy(w, h, 5)
	for y := h - h/3; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 75, G: 75, B: 75})
		}
	}
	return img
}

// TestPromptCard: the synthetic prompt clears both clauses; the title's
// bright lower-third logo band fails the bright clause, and the
// letterboxed movie fails the full-frame clause.
func TestPromptCard(t *testing.T) {
	ctx := context.Background()
	cp := PromptCard()
	if ok, detail := cp(ctx, promptLike(320, 240), promptLike(320, 240)); !ok {
		t.Errorf("synthetic prompt must pass PromptCard: %s", detail)
	}
	title := noisy(320, 240, 1)
	for y := 240 - 80; y < 240; y++ {
		for x := 0; x < 320; x++ {
			title.SetRGBA(x, y, color.RGBA{R: 230, G: 230, B: 230})
		}
	}
	if ok, detail := cp(ctx, title, title); ok {
		t.Errorf("a bright lower-third (the title's logo band) must not pass PromptCard: %s", detail)
	}
	if ok, detail := cp(ctx, letterboxed(320, 240), letterboxed(320, 240)); ok {
		t.Errorf("the letterboxed movie must not pass PromptCard: %s", detail)
	}
}

// TestStableFor: the anchored-duration predicate — two equal samples pass
// after the duration elapses; a different sample re-anchors (and fails).
func TestStableFor(t *testing.T) {
	ctx := context.Background()
	a := solid(320, 240, 150)
	b := solid(320, 240, 240)
	cp := StableFor(30*time.Millisecond, 0.01)
	if ok, _ := cp(ctx, a, a); ok {
		t.Error("the first sample sets the anchor; it cannot already be stable")
	}
	time.Sleep(40 * time.Millisecond)
	if ok, _ := cp(ctx, a, a); !ok {
		t.Error("the same screen past the duration must be stable")
	}
	// A new scene re-anchors: immediately after the change it is not stable.
	if ok, _ := cp(ctx, b, a); ok {
		t.Error("a changed screen must re-anchor and fail immediately")
	}
}

// TestSettled: two consecutive full-frame samples that do not move more than
// crossStepMax settle; the title's orbit (samples 0.12+ apart) never does.
func TestSettled(t *testing.T) {
	ctx := context.Background()
	// A settled UI: two near-identical full-frame samples.
	ui1 := noisy(320, 240, 20)
	ui2 := nudge(ui1) // one pixel moved
	cp := Settled()
	if ok, _ := cp(ctx, ui1, ui1); ok {
		t.Error("the first sample cannot be 'two consecutive'")
	}
	if ok, detail := cp(ctx, ui2, ui2); !ok {
		t.Errorf("two stable full-frame samples must settle: %s", detail)
	}
	// The title's orbit: consecutive samples move >= crossStepMax.
	orbit1, orbit2 := noisy(320, 240, 31), noisy(320, 240, 32)
	cpo := Settled()
	_, _ = cpo(ctx, orbit1, orbit1)
	if ok, _ := cpo(ctx, orbit2, orbit2); ok {
		t.Error("a moving full-frame orbit must not settle")
	}
}

// nudge changes one pixel of a copy of img (a fresh image, same screen).
func nudge(img image.Image) image.Image {
	b := img.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.At(x, y)
			out.Set(x, y, c)
		}
	}
	out.SetRGBA(b.Max.X-1, b.Max.Y-1, color.RGBA{R: 255, A: 255})
	return out
}

// TestAllCombinesAnds: All() is a measured AND — every clause's detail is
// shown and one failing clause fails the whole.
func TestAllCombinesAnds(t *testing.T) {
	ctx := context.Background()
	box := brightBox(320, 240)
	all := All(SceneChanged(0.10), TextboxPresent())
	// Identical images: the change clause fails, the box clause passes.
	ok, detail := all(ctx, box, box)
	if ok {
		t.Error("All must fail when one clause fails")
	}
	if detail == "" {
		t.Error("All must carry the combined details")
	}
	diff := noisy(320, 240, 99)
	ok, _ = all(ctx, box, diff)
	if !ok {
		t.Error("All must pass when every clause passes")
	}
}

// TestScriptEvidenceNames: the evidence file names are sanitized and indexed.
func TestScriptEvidenceNames(t *testing.T) {
	got := evidenceName(3, "confirm-character name!", "final")
	want := "03-confirm-character-name--final.png"
	if got != want {
		t.Errorf("evidenceName = %q, want %q", got, want)
	}
	dir := t.TempDir()
	img := solid(64, 48, 100)
	path, note := storeEvidence(dir, "x.png", img)
	if note != "" {
		t.Fatalf("storeEvidence: %s", note)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("evidence PNG not on disk: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "x.png"))
	if err != nil {
		t.Fatalf("read evidence PNG: %v", err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Errorf("evidence PNG unreadable: %v", err)
	}
}

// boxLike paints a smooth mid-tone frame (the boat scene) with a bright
// glyph-inked band inside the box window: the synthetic first-dialogue
// screen.
func boxLike(w, h int) *image.RGBA {
	img := solid(w, h, 130)
	x0, x1 := int(boxBandX0*float64(w)), int(boxBandX1*float64(w))
	y0, y1 := int(boxBandY0*float64(h)), int(boxBandY1*float64(h))
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			v := uint8(150)
			if (x/4)%2 == 0 {
				v = 220
			}
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v})
		}
	}
	return img
}

// TestBoxPresentClauses: the synthetic box clears all three clauses; the
// letter-scroll (same glyph band, textured parchment background) fails the
// entropy clause; the smooth bright screen (no glyph edges) fails the edge
// clause. These are the three measured screen classes on this host.
func TestBoxPresentClauses(t *testing.T) {
	ctx := context.Background()
	cp := BoxPresent()
	if ok, detail := cp(ctx, boxLike(320, 240), boxLike(320, 240)); !ok {
		t.Errorf("synthetic box must pass BoxPresent: %s", detail)
	}
	scroll := noisy(320, 240, 7)
	fw, fh := 320.0, 240.0
	x0, x1 := int(boxBandX0*fw), int(boxBandX1*fw)
	y0, y1 := int(boxBandY0*fh), int(boxBandY1*fh)
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			v := uint8(150)
			if (x/4)%2 == 0 {
				v = 220
			}
			scroll.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v})
		}
	}
	if ok, detail := cp(ctx, scroll, scroll); ok {
		t.Errorf("scroll-like frame must fail BoxPresent (entropy clause): %s", detail)
	}
	if ok, detail := cp(ctx, solid(320, 240, 150), solid(320, 240, 150)); ok {
		t.Errorf("smooth bright screen must fail BoxPresent (edge clause): %s", detail)
	}
}

// synthDialog builds a system-dialog frame: a dark background with a bright
// centre panel (the save notices / reading state) — measured to classify as
// ClassDialog.
func synthDialog(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 50) // dark background
		}
	}
	for y := int(0.30 * float64(h)); y < int(0.65*float64(h)); y++ { // bright centre panel
		for x := int(0.30 * float64(w)); x < int(0.70*float64(w)); x++ {
			set(x, y, 160)
		}
	}
	return img
}

// TestScreenSpecificGates: the screen-specific checkpoints gate on the
// MEASURED class — the box, the prompt, the save-creation dialog, and the
// overall-dark screen each pass only their own gate (the negative checks the
// drive requires against the confusable screens).
func TestScreenSpecificGates(t *testing.T) {
	ctx := context.Background()
	box := synthBox(320, 240)
	prompt := synthPrompt(320, 240)
	dialog := synthDialog(320, 240)
	dark := solid(320, 240, 50)

	// BoxPresent / ScreenIsClass(box) pass only on the box.
	if ok, d := BoxPresent()(ctx, box, box); !ok {
		t.Errorf("synthetic box must pass BoxPresent: %s", d)
	}
	for name, f := range map[string]image.Image{"prompt": prompt, "dialog": dialog, "dark": dark} {
		if ok, d := BoxPresent()(ctx, f, f); ok {
			t.Errorf("%s must not pass BoxPresent: %s", name, d)
		}
	}
	// The screen-class gates each hold only on their own screen.
	contNo := synthContinue(320, 240, "no")
	cases := []struct {
		want ScreenClass
		gate Checkpoint
		img  image.Image
	}{
		{ClassBox, ScreenIsClass(ClassBox), box},
		{ClassContinue, MemoryCardPrompt(), contNo}, // the create-save prompt gate (amendment 4: the dark prompt family, cursor on No)
		{ClassDialog, SaveNotice(), dialog},
		{ClassDark, ScreenIsClass(ClassDark), dark},
	}
	for _, c := range cases {
		if ok, d := c.gate(ctx, c.img, c.img); !ok {
			t.Errorf("%s must hold on its own screen (want %s): %s", c.img, c.want, d)
		}
	}
	// The confusable negatives: the save-notice dialog and the overall-dark
	// screen are both "dark-ish", but SaveNotice (the dialog) must NOT hold on
	// the overall-dark screen, and vice versa.
	if ok, d := SaveNotice()(ctx, dark, dark); ok {
		t.Errorf("the overall-dark screen must not pass SaveNotice (the dialog): %s", d)
	}
	if ok, d := ScreenIsClass(ClassDark)(ctx, dialog, dialog); ok {
		t.Errorf("the dialog (bright centre) must not pass the overall-dark class: %s", d)
	}
}

// cursorPrompt builds a synthetic prompt with a near-white cursor blob on the
// wanted side (left = Yes, right = No) — to exercise the cursor-side gates.
func cursorPrompt(w, h int, left bool) *image.RGBA {
	img := synthPrompt(w, h)
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	x0, x1 := int(0.25*float64(w)), int(0.35*float64(w))
	if !left {
		x0, x1 = int(0.55*float64(w)), int(0.65*float64(w))
	}
	for y := int(0.60 * float64(h)); y < int(0.70*float64(h)); y++ {
		for x := x0; x < x1; x++ {
			set(x, y, 240) // the near-white cursor blob
		}
	}
	return img
}

// TestCursorNotNoGate: the move-cursor gate FAILS while the cursor is on No
// (right) — the negative check the task forbids — and HOLDS when the cursor
// is on Yes (left) or the side is ambiguous.
func TestCursorNotNoGate(t *testing.T) {
	ctx := context.Background()
	gate := CursorNotNo()
	if ok, d := gate(ctx, cursorPrompt(320, 240, false), cursorPrompt(320, 240, false)); ok {
		t.Errorf("cursor on No (right) must FAIL CursorNotNo: %s", d)
	}
	if ok, d := gate(ctx, cursorPrompt(320, 240, true), cursorPrompt(320, 240, true)); !ok {
		t.Errorf("cursor on Yes (left) must pass CursorNotNo: %s", d)
	}
	// Ambiguous (no blob): the side is not No, so the gate holds (the loop
	// guard is the backstop, not this gate).
	if ok, d := gate(ctx, synthPrompt(320, 240), synthPrompt(320, 240)); !ok {
		t.Errorf("ambiguous cursor side must pass CursorNotNo: %s", d)
	}
}

// synthContinue builds the synthetic "cannot be saved — continue anyway?"
// dialog (ADR-0043 amendment 2, measured from the retained real frames): the
// dark DIALOG base (near-black lower third, dark background, bright centre
// panel) CARRYING the near-white cursor blob on the wanted side (left =
// Yes, right = No). With no blob it is the plain info dialog (save notice /
// no-save screen) — the measured negative (0.000 blob both sides).
func synthContinue(w, h int, cursorSide string) *image.RGBA {
	img := synthDialog(w, h) // the dark base + bright centre panel
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	// The option windows: left (0.15-0.50, 0.55-0.85), right (0.50-0.85,
	// 0.55-0.85) of the frame — the blob sits inside the chosen one.
	var x0, x1 int
	switch cursorSide {
	case "yes":
		x0, x1 = int(0.25*float64(w)), int(0.35*float64(w))
	case "no":
		x0, x1 = int(0.55*float64(w)), int(0.65*float64(w))
	default:
		return img // the blobless info dialog
	}
	for y := int(0.60 * float64(h)); y < int(0.70*float64(h)); y++ {
		for x := x0; x < x1; x++ {
			set(x, y, 240) // the near-white cursor blob (luma >= 220)
		}
	}
	return img
}

// synthTitleDim builds the synthetic title DIM ("press START") phase
// (ADR-0043 amendment 4, measured from the retained real frames p01 /
// 01-final): mid-luma frame (mean ~105), bright letterbox bands (luma 120,
// band ~0.47), a mid centre region (mid ~125 < 130), a small bright hint
// block in the bottom band (the "press START" text, lower-third bright
// ~0.038). The OLD classifier read exactly this shape as the memory-card
// prompt (the false pass: lb > 0.25, lower-third bright < 0.10, text-window
// mean >= 110); the amended classifier must read it as the title.
func synthTitleDim(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 95) // the dim title's sky
		}
	}
	for y := int(0.30 * float64(h)); y < int(0.65*float64(h)); y++ { // the boat/centre region
		for x := int(0.30 * float64(w)); x < int(0.70*float64(w)); x++ {
			set(x, y, 125)
		}
	}
	band := 30
	if band > h/2 {
		band = h / 2
	}
	for y := 0; y < band; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 120) // top letterbox band
		}
	}
	for y := h - band; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 120) // bottom letterbox band
		}
	}
	// The "press START" hint: a small bright block in the bottom band.
	for y := h - band + 4; y < h-band+16; y++ {
		for x := w/2 - 40; x < w/2+40; x++ {
			set(x, y, 200)
		}
	}
	return img
}

// TestTitleDimPhaseClass: the title's dim "press START" phase is the TITLE
// class, not the prompt class (the false pass the 2026-10-17 real run
// exposed: the START step "passed" on this screen, the START press landed
// on the bright logo phase and the screen stayed on the title for the whole
// drive). The synthetic double is built to satisfy the OLD prompt clause
// (the regression the test pins), and the amended classifier must reject it
// to the title (the mid-mean and letterbox clauses, amendment 4).
func TestTitleDimPhaseClass(t *testing.T) {
	img := synthTitleDim(320, 240)
	c, _ := ScreenSigOf(img).ClassOf()
	if c != ClassTitle {
		t.Fatalf("the dim title phase must classify as title, got %s (the old prompt false pass)", c)
	}
	if k := ScreenSigOf(img).Key(); k != "title" {
		t.Errorf("dim title key = %q, want title (the loop guard must not arm on it)", k)
	}
	// The confusable negatives keep their classes: the dark create-save
	// prompt (mid 160, the cursor blob) is continue; the synthetic bright
	// prompt double is still the prompt class (the doubles the loop tests
	// rely on); the quest-log list is filesel.
	if c, _ := ScreenSigOf(synthContinue(320, 240, "no")).ClassOf(); c != ClassContinue {
		t.Errorf("the dark prompt must stay continue, got %s", c)
	}
	if c, _ := ScreenSigOf(synthPrompt(320, 240)).ClassOf(); c != ClassPrompt {
		t.Errorf("the synthetic prompt double must stay prompt, got %s", c)
	}
	if c, _ := ScreenSigOf(synthFileSel(320, 240)).ClassOf(); c != ClassFileSel {
		t.Errorf("the quest-log list must stay filesel, got %s", c)
	}
	// And the memory-card prompt gate (the create-save prompt, cursor on No)
	// holds on the dark prompt and NOT on the dim title (the negative check
	// that would have caught the 2026-10-17 false pass).
	ctx := context.Background()
	gate := MemoryCardPrompt()
	if ok, d := gate(ctx, synthTitleDim(320, 240), synthTitleDim(320, 240)); ok {
		t.Errorf("the dim title must NOT pass the create-save prompt gate: %s", d)
	}
	if ok, d := gate(ctx, synthContinue(320, 240, "no"), synthContinue(320, 240, "no")); !ok {
		t.Errorf("the dark prompt (cursor No) must pass the create-save prompt gate: %s", d)
	}
	// The strict Yes gate: holds on cursor Yes, fails on the dim title and
	// on cursor No.
	yesGate := MemoryCardPromptYes()
	if ok, d := yesGate(ctx, synthContinue(320, 240, "yes"), synthContinue(320, 240, "yes")); !ok {
		t.Errorf("cursor on Yes must pass the strict gate: %s", d)
	}
	if ok, d := yesGate(ctx, synthTitleDim(320, 240), synthTitleDim(320, 240)); ok {
		t.Errorf("the dim title must NOT pass the strict Yes gate: %s", d)
	}
	if ok, d := yesGate(ctx, synthContinue(320, 240, "no"), synthContinue(320, 240, "no")); ok {
		t.Errorf("cursor on No must NOT pass the strict Yes gate: %s", d)
	}
}

// TestContinuePromptClass: the continue-anyway dialog (the save failure's
// Yes/No screen) is its OWN measured class — the dialog base + the cursor
// blob — and the confusable screens each fail it (the measured negative
// checks, ADR-0043 amendment 2): the plain info dialog (no blob), the
// save-notice dialog (no blob), the create-save parchment PROMPT (bright,
// never the dark dialog base), and the overall-dark screen (dark centre).
func TestContinuePromptClass(t *testing.T) {
	ctx := context.Background()
	contNo := synthContinue(320, 240, "no")
	contYes := synthContinue(320, 240, "yes")
	info := synthContinue(320, 240, "") // blobless = the info dialog
	dialog := synthDialog(320, 240)
	prompt := synthPrompt(320, 240)
	dark := solid(320, 240, 50)

	// Class + key: the continue dialog is class=continue with the measured
	// side, and the key carries the side (the loop-guard comparison).
	if c, side := ScreenSigOf(contNo).ClassOf(); c != ClassContinue || side != CursorRight {
		t.Errorf("continue dialog (cursor No) class=%s side=%s, want continue/no", c, side)
	}
	if c, side := ScreenSigOf(contYes).ClassOf(); c != ClassContinue || side != CursorLeft {
		t.Errorf("continue dialog (cursor Yes) class=%s side=%s, want continue/yes", c, side)
	}
	if k := ScreenSigOf(contNo).Key(); k != "continue:no" {
		t.Errorf("continue (cursor No) key = %q, want continue:no", k)
	}
	if k := ScreenSigOf(contYes).Key(); k != "continue:yes" {
		t.Errorf("continue (cursor Yes) key = %q, want continue:yes", k)
	}

	// The gates hold on their own screen...
	if ok, d := ContinuePrompt()(ctx, contNo, contNo); !ok {
		t.Errorf("continue dialog must pass ContinuePrompt: %s", d)
	}
	if ok, d := ContinuePromptYes()(ctx, contYes, contYes); !ok {
		t.Errorf("continue dialog (cursor Yes) must pass ContinuePromptYes: %s", d)
	}
	// ...and the confusables fail them (the negative checks). The synthetic
	// bright prompt double is a confusable too (it is NOT the dark dialog
	// base — amendment 4's false pass is the title's dim phase, which is
	// the title class now, not a prompt).
	if ok, d := ContinuePromptYes()(ctx, contNo, contNo); ok {
		t.Errorf("cursor on No must FAIL the strict ContinuePromptYes: %s", d)
	}
	for name, f := range map[string]image.Image{"info dialog": info, "save notice": dialog, "bright prompt double": prompt, "overall-dark": dark} {
		if ok, d := ContinuePrompt()(ctx, f, f); ok {
			t.Errorf("%s must NOT pass ContinuePrompt: %s", name, d)
		}
		if ok, d := SaveNotice()(ctx, f, f); ok && name != "save notice" && name != "info dialog" {
			t.Errorf("%s must NOT pass SaveNotice (the blobless dialog class): %s", name, d)
		}
		// The continue dialog is NOT the plain dialog class either (the blob
		// split): SaveNotice must fail on it.
		if ok, d := SaveNotice()(ctx, contNo, contNo); ok {
			t.Errorf("the continue dialog must NOT pass SaveNotice (it carries the cursor blob): %s", d)
		}
	}
}

// TestContinueLoopGuardArms: the loop guard arms on the continue class too
// (the save-failure prompt loops exactly like the create-save prompt — a
// step that starts on continue and stays on the same key must fail fast).
func TestContinueLoopGuardArms(t *testing.T) {
	if g := newLoopGuard("dialog"); g != nil {
		t.Fatalf("guard must not arm on the plain dialog ref (no cursor loop there)")
	}
	if g := newLoopGuard("continue:no"); g == nil {
		t.Fatalf("guard must arm on a continue ref (the save-failure Yes/No loop)")
	}
	g := newLoopGuard("continue:no")
	for i := 0; i < loopConfirmSamples; i++ {
		g.observe("continue:no", 1)
	}
	if !g.fired || g.key != "continue:no" {
		t.Fatalf("guard must fire on the looped continue key (key=%q fired=%v)", g.key, g.fired)
	}
}

// synthNameEnt builds the synthetic name-entry screen (the measured class:
// a bright keyboard grid filling the lower half + the dense letter-button
// ink in the text window) that ClassOf measures as ClassNameEnt.
func synthNameEnt(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 110) // the mid-bright panel base
		}
	}
	for y := int(0.45 * float64(h)); y < h; y++ { // the bright keyboard grid
		for x := int(0.15 * float64(w)); x < int(0.85*float64(w)); x++ {
			set(x, y, 190)
		}
	}
	// The letter-button ink: vertical strokes (horizontal edges) across the
	// text window (y 0.35-0.60) — the measured text.edges 0.104-0.134. Kept
	// ABOVE the box band (y 0.69-0.84), which the measured name entry keeps
	// smooth (edges 0.000) — the box clause is the one the grid must NOT
	// clear.
	for y := int(0.33 * float64(h)); y < int(0.55*float64(h)); y++ {
		for x := int(0.28 * float64(w)); x < int(0.72*float64(w)); x++ {
			if (x/4)%2 == 0 {
				set(x, y, 30)
			}
		}
	}
	// The bright bottom hint bar (measured botbar bright 0.941).
	for y := int(0.87 * float64(h)); y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 230)
		}
	}
	return img
}

// synthFileSel builds the synthetic quest-log "File Selection" screen (the
// task's file-select screen, ADR-0043 amendment 3; measured from this host's
// retained frames): the DARK base (near-black lower third, mean 95-106) with
// the dense slot text (the high-entropy clause) and the mid-luma slot panel
// in the box-band window (the measured mean 140-143, below the 190 bright
// threshold). Its measured confusable is the blobless info dialog
// (synthDialog): the same dark base, sparse 3-line text (low entropy) and a
// DARK box-band window (measured mean 65).
func synthFileSel(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	// The dark base: deterministic noise over luma 15..85 (mostly <= 90 for
	// the near-black lower-third clause) — many luma levels for the
	// high-entropy clause (the real screen's 0.74-0.84 comes from the dense
	// text + icons over the textured dark panel).
	v := uint64(0x5eed)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v ^= v << 13
			v ^= v >> 7
			v ^= v << 17
			set(x, y, uint8(15+(v%71))) // 15..85
		}
	}
	// The dense text band (the slot list's names): alternating bright/dark
	// strokes across the mid rows.
	for y := int(0.30 * float64(h)); y < int(0.55*float64(h)); y++ {
		for x := int(0.20 * float64(w)); x < int(0.80*float64(w)); x++ {
			if (x+y)%3 != 0 {
				set(x, y, 150)
			}
		}
	}
	// The slot panel: mid-luma (180 — below the 190 bright threshold, as
	// measured: lower3.bright 0.000 on the real screen) filling the box-band
	// window (x 0.31-0.75, y 0.69-0.84) — the measured box-band mean 140-143
	// — and, like the real screen, diluting the lower-third dark fraction
	// below 0.85 (so the screen does NOT enter the dark family).
	for y := int(0.69 * float64(h)); y < int(0.84*float64(h)); y++ {
		for x := int(0.31 * float64(w)); x < int(0.75*float64(w)); x++ {
			set(x, y, 180)
		}
	}
	return img
}

// TestFileSelectClass: the quest-log file selection is its OWN measured
// class (ADR-0043 amendment 3): the dark base + the dense slot text (entropy
// 0.74-0.84) + the bright slot panel in the box band (mean 140-143) — and
// the confusable screens each fail it: the blobless info dialog (0.000 blob,
// entropy 0.52-0.68, box-band mean 65), the name entry (bright, hint bar),
// the create-save prompt (bright parchment), the box (bright band, glyph
// ink), the overall-dark screen (no panel, no dense text), and the continue
// dialog (dark but a single cursor blob and no slot panel).
func TestFileSelectClass(t *testing.T) {
	ctx := context.Background()
	fs := synthFileSel(320, 240)
	if c, side := ScreenSigOf(fs).ClassOf(); c != ClassFileSel || side != CursorNone {
		t.Errorf("synthetic file selection class=%s side=%s, want filesel/none", c, side)
	}
	if k := ScreenSigOf(fs).Key(); k != "filesel" {
		t.Errorf("file selection key = %q, want filesel", k)
	}
	if ok, d := FileSelect()(ctx, fs, fs); !ok {
		t.Errorf("synthetic file selection must pass FileSelect: %s", d)
	}
	for name, f := range map[string]image.Image{
		"info dialog":        synthDialog(320, 240),
		"name entry":         synthNameEnt(320, 240),
		"create-save prompt": synthPrompt(320, 240),
		"box":                synthBox(320, 240),
		"overall dark":       solid(320, 240, 50),
		"continue dialog":    synthContinue(320, 240, "no"),
	} {
		if ok, d := FileSelect()(ctx, f, f); ok {
			t.Errorf("%s must NOT pass FileSelect: %s", name, d)
		}
	}
	// The negative the class exists for: the info dialog is the file
	// selection's confusable (same dark base), and the dialog's blob clause
	// is what separates them — the info dialog must classify as dialog.
	if c, _ := ScreenSigOf(synthDialog(320, 240)).ClassOf(); c != ClassDialog {
		t.Errorf("the blobless info dialog must classify as dialog, got %s", c)
	}
	// The loop guard arms on the stable quest-log list (a press that leaves
	// it on the same key is a loop — A alone is a measured no-op there).
	if g := newLoopGuard("filesel"); g == nil {
		t.Fatalf("the loop guard must arm on a filesel ref (a stable menu that loops in place)")
	}
}
