//go:build unix

package dolphin

import (
	"context"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"
)

// The synthetic doubles for the amendment-5 measured clauses, built to the
// MEASURED stats of this host's retained frames (the ADR-0043 amendment 5
// record): the dim name-entry frame (dark base, the keyboard grid in the
// namegrid window with dense letter strokes, dark top/bottom bands) and the
// blink-OFF Yes/No prompt (the cursor blinked away: the dense text panel
// with no blob).

// synthNameEntDim is the measured-style DIM name-entry frame: a dark base,
// the keyboard grid in the namegrid window (x 0.20-0.80, y 0.40-0.84) with
// the letter strokes' horizontal ink (the measured namegrid edges
// 0.066-0.79 regime), the grid's lower rows dark (the measured screen's
// box band stays below the box clause, measured 93), and dark top/bottom
// bands (the measured lb 0.166-0.173, below the pill's 0.428).
func synthNameEntDim(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 40, G: 40, B: 40})
		}
	}
	for y := int(0.40 * float64(h)); y < int(0.84*float64(h)); y++ {
		for x := int(0.20 * float64(w)); x < int(0.80*float64(w)); x++ {
			l := uint8(150)
			if y >= int(0.65*float64(h)) {
				l = 100 // the grid's lower rows (the measured dim screen darkens there)
			}
			if x%8 < 2 {
				l = 50 // the letter strokes (horizontal ink)
			}
			img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l})
		}
	}
	return img
}

// synthBlinkOffPrompt is the measured blink-OFF create-save/continue prompt:
// the dark base, the bright text panel in the centre carrying dense text
// (the measured mid mean 155-164, text edges 0.095-0.134) and NO cursor
// blob (the measured blink-off: blob 0.000-0.002 both sides).
func synthBlinkOffPrompt(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 30, G: 30, B: 30})
		}
	}
	for y := int(0.30 * float64(h)); y < int(0.65*float64(h)); y++ {
		for x := int(0.25 * float64(w)); x < int(0.75*float64(w)); x++ {
			l := uint8(190)
			if x%4 == 0 {
				l = 60 // the dense text's horizontal ink
			}
			img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l})
		}
	}
	return img
}

// TestNameEntryMeasuredClauses: the name entry is its OWN class on its
// measured DIM frames (amendment 5 — the old bright-hint-bar clause matched
// only the bright pulse, which is why the 2026-10-17 run read the dim phase
// as filesel/dark and the bright phase as title). The confusables each fail
// the grid clause: the blobless save notice (sparse ink), the quest-log list
// (grid mean 143, above the 140 ceiling), the bright name entry (still its
// own class via the hint-bar clause), and the file selection.
func TestNameEntryMeasuredClauses(t *testing.T) {
	if c, side := ScreenSigOf(synthNameEntDim(320, 240)).ClassOf(); c != ClassNameEnt || side != CursorNone {
		t.Errorf("measured-style dim name entry class=%s side=%s, want nameent/none", c, side)
	}
	if c, _ := ScreenSigOf(synthNameEnt(320, 240)).ClassOf(); c != ClassNameEnt {
		t.Errorf("bright name entry regressed: class=%s, want nameent", c)
	}
	for name, f := range map[string]image.Image{
		"save notice":    synthDialog(320, 240),
		"file selection": synthFileSel(320, 240),
		"blink-off":      synthBlinkOffPrompt(320, 240),
		"box":            synthBox(320, 240),
	} {
		if c, _ := ScreenSigOf(f).ClassOf(); c == ClassNameEnt {
			t.Errorf("%s must NOT classify as nameent, got nameent", name)
		}
	}
}

// TestBlinkOffPromptClauses: the cursor blinks off on the measured WOTW
// prompts, and on a blink-off frame the drive must NOT see a save notice
// (whose action is A — landing on a prompt with the cursor on No selects
// the no-save path). The dense text panel keeps the frame on the prompt
// family with the side UNKNOWN (the drive waits for the measured side); the
// sparse blobless notice keeps the dialog class.
func TestBlinkOffPromptClauses(t *testing.T) {
	if c, side := ScreenSigOf(synthBlinkOffPrompt(320, 240)).ClassOf(); c != ClassContinue || side != CursorNone {
		t.Errorf("blink-off prompt class=%s side=%s, want continue/none (the side is unmeasured while the cursor blinks)", c, side)
	}
	if c, side := ScreenSigOf(synthDialog(320, 240)).ClassOf(); c != ClassDialog || side != CursorNone {
		t.Errorf("blobless notice class=%s side=%s, want dialog/none", c, side)
	}
	if c, side := ScreenSigOf(synthContinue(320, 240, "no")).ClassOf(); c != ClassContinue || side != CursorRight {
		t.Errorf("cursor-on-No prompt class=%s side=%s, want continue/right", c, side)
	}
	if c, side := ScreenSigOf(synthContinue(320, 240, "yes")).ClassOf(); c != ClassContinue || side != CursorLeft {
		t.Errorf("cursor-on-Yes prompt class=%s side=%s, want continue/left", c, side)
	}
}

// fastOpts is the synthetic drive's options: millisecond settle so the loop
// tests run in wall-clock milliseconds (the real values are 2 s settle, the
// defaults).
func fastOpts(maxIter int) DriveOptions {
	return DriveOptions{Deadline: 30 * time.Second, MaxIterations: maxIter, Settle: time.Millisecond}
}

// TestDriveLoopReachesBox: the screen-driven drive takes the synthetic WOTW
// flow (title -> create-save prompt -> Yes -> save notice -> name entry ->
// name confirm -> the dialogue box) and terminates on the box class. The
// action sequence is the MEASURED one: START on the title, the main stick
// left on the cursor-on-No prompt, A on Yes, A on the notice, START-then-A
// (never A alone) on the name entry, A on the confirm.
func TestDriveLoopReachesBox(t *testing.T) {
	b := &fakeBridge{shots: []image.Image{
		synthTitleDim(320, 240),
		synthContinue(320, 240, "no"),
		synthContinue(320, 240, "yes"),
		synthDialog(320, 240),
		synthNameEntDim(320, 240),
		synthContinue(320, 240, "yes"), // the name confirm (after the name entry)
		synthBox(320, 240),             // the ClassBox double (the in-game dialogue box)
	}}
	drv, err := RunDrive(context.Background(), b, fastOpts(90), WindWakerNewGameDrive())
	if err != nil || !drv.OK {
		t.Fatalf("drive did not reach the box: ok=%v err=%v reason=%q\n%s", drv.OK, err, drv.Reason, drv.TransitionLog())
	}
	if drv.Reason != "dialogue_box" {
		t.Errorf("reason = %q, want dialogue_box", drv.Reason)
	}
	want := []DriveScreen{DriveScreenTitle, DriveScreenPrompt, DriveScreenPrompt,
		DriveScreenSaveNotice, DriveScreenNameEntry, DriveScreenNameConfirm, DriveScreenDialogueBox}
	if len(drv.Transitions) != len(want) {
		t.Fatalf("transitions = %d, want %d:\n%s", len(drv.Transitions), len(want), drv.TransitionLog())
	}
	for i, w := range want {
		if drv.Transitions[i].Screen != w {
			t.Errorf("transition %d screen = %s, want %s", i, drv.Transitions[i].Screen, w)
		}
	}
	// The measured action sequence: START, stick-left, A, A, A+START+A
	// (three injects: type the letter, cursor to END, confirm the name), A.
	if len(b.injects) != 8 {
		t.Fatalf("injects = %d, want 8", len(b.injects))
	}
	if !b.injects[0].buttons.Start || b.injects[0].buttons.A {
		t.Errorf("title action = %v, want START alone", b.injects[0])
	}
	if b.injects[1].axes.MainX != -1 || b.injects[1].axes.MainY != 0 {
		t.Errorf("prompt:no action axes = %+v, want the measured stick left (MainX=-1)", b.injects[1].axes)
	}
	if b.injects[1].buttons.Start || b.injects[1].buttons.A {
		t.Errorf("prompt:no action must be the stick with NO button (A would select No), got %+v", b.injects[1])
	}
	// injects 2..7: A (yes), A (notice), then the name entry's A, START, A,
	// then the confirm's A.
	for i, wantA := range []bool{true, true, true, false, true, true} {
		btn := b.injects[2+i].buttons
		if btn.A != wantA || btn.Start != (i == 3) {
			t.Errorf("inject %d buttons = %+v, want A=%v START=%v", 2+i, btn, wantA, i == 3)
		}
	}
	// The name entry's FIRST inject must be A without START: it types the
	// letter under the cursor into the name (an empty name's START+A is
	// measured rejected — the bounce to the file selection). The SECOND
	// inject is START alone (the cursor to END); the third, A, confirms.
	if b.injects[4].buttons.Start || !b.injects[4].buttons.A {
		t.Errorf("name entry first inject = %+v, want A alone (type the first letter)", b.injects[4])
	}
	if !b.injects[5].buttons.Start || b.injects[5].buttons.A {
		t.Errorf("name entry second inject = %+v, want START alone (the cursor to END)", b.injects[5])
	}
	// The transition log carries the (screen, action) -> next screen lines.
	log := drv.TransitionLog()
	for _, want := range []string{"title:- --(START)--> prompt:no", "prompt:no --(STICK(-1,0)x900ms)--> prompt:yes", "name_entry:- --(A+START+A(name))--> name_confirm:yes", "name_confirm:yes --(A)--> dialogue_box", "dialogue_box:- --(done)--> <end>"} {
		if !strings.Contains(log, want) {
			t.Errorf("transition log missing %q:\n%s", want, log)
		}
	}
}

// TestDriveLoopDetectedDeadPrompt: a prompt that ignores the prescribed
// input (the screen is the same key every iteration with no visible change)
// fails fast with "loop detected" after 4 consecutive same-screen
// iterations — the press loop the spec's "avoiding false positives" forbids.
func TestDriveLoopDetectedDeadPrompt(t *testing.T) {
	b := &fakeBridge{shots: []image.Image{synthContinue(320, 240, "no")}} // the last queued image repeats
	drv, err := RunDrive(context.Background(), b, fastOpts(90), WindWakerNewGameDrive())
	if err == nil {
		t.Fatalf("a dead prompt must fail the drive")
	}
	if !drv.OK && !strings.Contains(drv.Reason, "loop detected") {
		t.Errorf("reason = %q, want loop detected", drv.Reason)
	}
	if !strings.Contains(err.Error(), "loop detected") {
		t.Errorf("error = %q, want loop detected", err)
	}
	if len(drv.Transitions) != driveLoopSameMin {
		t.Errorf("transitions = %d, want %d (fail fast at the 4th same-screen iteration)", len(drv.Transitions), driveLoopSameMin)
	}
}

// TestDriveUnmeasuredSideNeverGuessesA: on a blink-off prompt (the cursor
// side unmeasured) the drive must never press A: it waits for the measured
// side and only sends the measured stick move after 3 consecutive no-side
// iterations. A static blink-off frame then fails the loop detector (the
// input has no effect on a dead screen) — no A is injected at all.
func TestDriveUnmeasuredSideNeverGuessesA(t *testing.T) {
	b := &fakeBridge{shots: []image.Image{synthBlinkOffPrompt(320, 240)}}
	drv, err := RunDrive(context.Background(), b, fastOpts(90), WindWakerNewGameDrive())
	if err == nil || strings.Contains(drv.Reason, "dialogue_box") {
		t.Fatalf("a static blink-off prompt must fail the drive, got ok=%v err=%v", drv.OK, err)
	}
	if !strings.Contains(drv.Reason, "loop detected") {
		t.Errorf("reason = %q, want loop detected (the static screen ignores the stick)", drv.Reason)
	}
	for i, inj := range b.injects {
		if inj.buttons.A {
			t.Errorf("inject %d pressed A on an unmeasured side (buttons %+v) — the spec forbids guessing A", i, inj.buttons)
		}
	}
	// The action sequence: wait, wait, then the measured stick on the 3rd
	// no-side iteration.
	if len(drv.Transitions) < 3 {
		t.Fatalf("transitions = %d, want >= 3", len(drv.Transitions))
	}
	if drv.Transitions[0].Action != "wait" || drv.Transitions[1].Action != "wait" {
		t.Errorf("first two actions = %q %q, want wait wait (the side is unmeasured)", drv.Transitions[0].Action, drv.Transitions[1].Action)
	}
	if !strings.HasPrefix(drv.Transitions[2].Action, "STICK") {
		t.Errorf("3rd no-side action = %q, want the measured stick move", drv.Transitions[2].Action)
	}
}

// TestDriveBudgetExhausted: a screen that keeps subtly changing (the loop
// detector's no-change gate never closes) but never reaches the box burns
// the iteration cap and fails with the full transition log.
func TestDriveBudgetExhausted(t *testing.T) {
	// A wobble between two title-luma frames: same class every iteration,
	// RMSE ~0.04 (> driveLoopChangeMin, so the loop detector never fires),
	// no box. The iteration cap is the only bound.
	var shots []image.Image
	for i := 0; i < 8; i++ {
		l := uint8(100)
		if i%2 == 1 {
			l = 110
		}
		shots = append(shots, solid(320, 240, l))
	}
	b := &fakeBridge{shots: shots}
	drv, err := RunDrive(context.Background(), b, fastOpts(6), WindWakerNewGameDrive())
	if err == nil || !strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("a changing-but-never-advancing screen must burn the iteration cap: err=%v", err)
	}
	if !strings.Contains(drv.Reason, "budget exhausted") {
		t.Errorf("reason = %q, want budget exhausted", drv.Reason)
	}
	if len(drv.Transitions) != 6 {
		t.Errorf("transitions = %d, want 6 (the cap)", len(drv.Transitions))
	}
}

// TestDriveStaticWaitScreenIsNotALoop: a screen that prescribes WAIT (the
// measured timed pure-black transition: file selection -> the legend, whose
// retained frames show 4-cadence 0.0000-change samples inside the ~28 s
// black span) must NOT read as a loop — the detector guards against
// ineffective INPUT, and waiting through a timed transition is correct
// behaviour. The budget is the bound for a screen that never changes.
func TestDriveStaticWaitScreenIsNotALoop(t *testing.T) {
	// A static near-black frame: the cutscene family (wait-only — the box
	// band carries no glyph ink), the shape of the measured black span.
	b := &fakeBridge{shots: []image.Image{solid(320, 240, 20)}}
	drv, err := RunDrive(context.Background(), b, fastOpts(6), WindWakerNewGameDrive())
	if err == nil {
		t.Fatalf("a never-changing screen must burn the iteration cap: ok=%v reason=%q", drv.OK, drv.Reason)
	}
	if strings.Contains(err.Error(), "loop detected") {
		t.Fatalf("a static WAIT-ONLY screen must not read as a loop (the detector arms only on non-wait actions): %v", err)
	}
	if !strings.Contains(drv.Reason, "budget exhausted") {
		t.Errorf("reason = %q, want budget exhausted", drv.Reason)
	}
	for i, tr := range drv.Transitions {
		if tr.Action != "wait" {
			t.Errorf("iteration %d action = %q, want wait (nothing is pressed on a timed transition)", i, tr.Action)
		}
	}
}

// TestDriveTransitionLogNamesTheFailure: the failure handoff (the task's
// honest-failure contract) names the farthest correctly identified screen
// and the reason, from the same trace the success path uses.
func TestDriveTransitionLogNamesTheFailure(t *testing.T) {
	b := &fakeBridge{shots: []image.Image{synthContinue(320, 240, "no")}}
	drv, _ := RunDrive(context.Background(), b, fastOpts(90), WindWakerNewGameDrive())
	if drv.LastKey == "" {
		t.Fatal("the failure trace must carry the last screen key (the farthest identified screen)")
	}
	log := drv.TransitionLog()
	if !strings.Contains(log, "prompt:no") || !strings.Contains(log, "STICK") {
		t.Errorf("the failure log must name the screen and the action sent:\n%s", log)
	}
}

// ============================================================================
// Amendment 6: the opening legend's narration subtitle (the M6 drive's
// measured success screen) — the classifier clauses, the subtitle-timing
// record, and the wait-only observation.
// ============================================================================

// synthStorySub is the measured-style opening-legend frame (ADR-0043
// amendment 6): the letterboxed storybook panel (PURE-BLACK top/bottom bands
// — the measured lb 0.000) carrying the white narration subtitle in the
// bottom black bar — two bright lines with glyph strokes in the measured
// subbar window (y 0.76-0.90; the measured line positions 0.78-0.83 and
// 0.84-0.88). withText=false is the same frame WITHOUT the bottom-bar text
// (a fade, or between subtitles: the measured bright 0.000 / edges <= 0.039).
func synthStorySub(w, h int, withText bool) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 35) // the dark base (the measured frames' mean 66-83)
		}
	}
	for y := int(0.10 * float64(h)); y < int(0.70*float64(h)); y++ { // the panel's art
		for x := int(0.15 * float64(w)); x < int(0.85*float64(w)); x++ {
			set(x, y, 120) // the measured mid band 110-151
		}
	}
	for y := 0; y < lbRows; y++ { // the pure-black letterbox bands (lb ~0.008)
		for x := 0; x < w; x++ {
			set(x, y, 2)
			set(x, h-1-y, 2)
		}
	}
	if !withText {
		return img
	}
	// The narration subtitle's two lines: bright strokes over the black bar
	// (the horizontal |dluma| steps carry the edge clause; the bright
	// fraction carries the bright clause — the class is ONLY claimed with
	// the measured text, never on the fade).
	for _, y0 := range []float64{0.78, 0.845} {
		for y := int(y0 * float64(h)); y < int((y0+0.045)*float64(h)); y++ {
			for x := int(0.20 * float64(w)); x < int(0.80*float64(w)); x++ {
				l := uint8(235)
				if x%4 >= 2 {
					l = 10 // the glyph gaps
				}
				set(x, y, l)
			}
		}
	}
	return img
}

// TestStorySubMeasuredClauses: the letterboxed legend frame CARRYING the
// bottom-bar text is its own class (storysub), the text-less fade is NOT
// (the cutscene family — the drive waits on it), and the confusable screens
// (name entry, file selection, the box, the title, the dialogs) are not
// storysub (their dark-gray bands measure lb 0.15+ where the legend's bars
// are pure black, and none of them carries the subbar text).
func TestStorySubMeasuredClauses(t *testing.T) {
	if c, side := ScreenSigOf(synthStorySub(320, 240, true)).ClassOf(); c != ClassStorySub || side != CursorNone {
		t.Errorf("legend frame with text class=%s side=%s, want storysub/none", c, side)
	}
	if c, _ := ScreenSigOf(synthStorySub(320, 240, false)).ClassOf(); c == ClassStorySub {
		t.Errorf("text-less legend frame classified storysub — the class must be claimed only on a frame that MEASURES the text (the measured fade reads dark/unknown)")
	}
	for name, f := range map[string]image.Image{
		"name entry":      synthNameEnt(320, 240),
		"name entry dim":  synthNameEntDim(320, 240),
		"file selection":  synthFileSel(320, 240),
		"dialogue box":    synthBox(320, 240),
		"title":           synthTitleDim(320, 240),
		"save notice":     synthDialog(320, 240),
		"create-save no":  synthContinue(320, 240, "no"),
		"create-save yes": synthContinue(320, 240, "yes"),
	} {
		if c, _ := ScreenSigOf(f).ClassOf(); c == ClassStorySub {
			t.Errorf("%s must NOT classify as storysub, got storysub", name)
		}
	}
	// The crop handoff is a valid normalised region (the live-session E2E's
	// textbox-region configuration): inside the frame, x0 < x1, y0 < y1.
	if StorySubCrop[0] < 0 || StorySubCrop[1] < 0 || StorySubCrop[2] > 1 || StorySubCrop[3] > 1 ||
		StorySubCrop[0] >= StorySubCrop[2] || StorySubCrop[1] >= StorySubCrop[3] {
		t.Errorf("StorySubCrop = %v, want a valid normalised region", StorySubCrop)
	}
}

// TestSubTimingsOf: the narration visibility record is one SubTiming per
// maximal run of text-bearing iterations, bracketed by the surrounding
// text-less samples (the fade-in/hold/fade-out the recognition pipeline
// needs); the last run is open when the observation window closed on text.
func TestSubTimingsOf(t *testing.T) {
	mk := func(i int, text bool, at time.Duration) DriveTransition {
		return DriveTransition{Index: i, Time: time.Unix(0, 0).Add(at), Text: text}
	}
	// text run 1-2 (closed by sample 3), text run 4 (open at the end).
	sts := subTimingsOf([]DriveTransition{
		mk(0, false, 0), mk(1, true, 2*time.Second), mk(2, true, 4*time.Second),
		mk(3, false, 6*time.Second), mk(4, true, 8*time.Second),
	})
	if len(sts) != 2 {
		t.Fatalf("runs = %d, want 2", len(sts))
	}
	r1 := sts[0]
	if r1.FirstIndex != 1 || r1.LastIndex != 2 || r1.Open {
		t.Errorf("run 1 = %+v, want first=1 last=2 closed", r1)
	}
	if r1.FadeIn != 2*time.Second || r1.Hold != 2*time.Second || r1.FadeOut != 2*time.Second {
		t.Errorf("run 1 spans = in %v hold %v out %v, want 2s/2s/2s (the sample cadence brackets the measured spans)", r1.FadeIn, r1.Hold, r1.FadeOut)
	}
	r2 := sts[1]
	if r2.FirstIndex != 4 || r2.LastIndex != 4 || !r2.Open || r2.FadeOut != 0 {
		t.Errorf("run 2 = %+v, want first=4 last=4 open (the window closed on text)", r2)
	}
	// No text -> no runs.
	if sts := subTimingsOf([]DriveTransition{mk(0, false, 0), mk(1, false, 1)}); len(sts) != 0 {
		t.Errorf("all text-less: runs = %d, want 0", len(sts))
	}
	// A single open run from the first sample.
	sts = subTimingsOf([]DriveTransition{mk(0, true, 0)})
	if len(sts) != 1 || !sts[0].Open || sts[0].FadeIn != 0 {
		t.Errorf("single text sample: %+v, want one open run with zero fade-in", sts)
	}
}

// TestDriveLoopReachesStorySubtitle (amendment 6): the drive's success
// screen is the opening legend's narration subtitle — the first in-game
// narrated text, whichever success screen the loop reaches FIRST (the
// subtitle or the dialogue box). On the synthetic legend the drive:
// records the first text-measured frame as the success, presses NOTHING on
// the legend (the narration advances on its own; a press may skip it), and
// keeps sampling wait-only until one full subtitle run is measured (a text
// run closed by a text-less sample: the timing record the pipeline needs).
func TestDriveLoopReachesStorySubtitle(t *testing.T) {
	b := &fakeBridge{shots: []image.Image{
		synthTitleDim(320, 240),
		synthContinue(320, 240, "no"),
		synthContinue(320, 240, "yes"),
		synthDialog(320, 240),
		synthNameEntDim(320, 240),
		synthContinue(320, 240, "yes"), // the name confirm (after the name entry)
		solid(320, 240, 5),             // the timed pure-black transition
		synthStorySub(320, 240, true),  // the legend, subtitle visible (success frame)
		synthStorySub(320, 240, true),  // hold
		synthStorySub(320, 240, false), // fade out (closes run 1)
		synthStorySub(320, 240, true),  // the next subtitle (closes the run)
	}}
	drv, err := RunDrive(context.Background(), b, fastOpts(90), WindWakerNewGameDrive())
	if err != nil || !drv.OK {
		t.Fatalf("drive did not reach the legend: ok=%v err=%v reason=%q\n%s", drv.OK, err, drv.Reason, drv.TransitionLog())
	}
	if drv.Reason != "story_subtitle" || drv.SuccessScreen != DriveScreenStorySubtitle || !drv.SuccessText {
		t.Errorf("success = %q screen=%s text=%v, want story_subtitle/storysub/true", drv.Reason, drv.SuccessScreen, drv.SuccessText)
	}
	if drv.SubCrop != StorySubCrop {
		t.Errorf("SubCrop = %v, want the measured bottom-bar crop %v", drv.SubCrop, StorySubCrop)
	}
	// NOTHING is pressed on the legend: the injects stop at the name
	// confirm's A (8 total: START, stick, A, A, A, START, A, A) — the last
	// 5 iterations (the black transition and the legend) carry wait only.
	if len(b.injects) != 8 {
		t.Fatalf("injects = %d, want 8 (nothing is pressed while the legend plays)", len(b.injects))
	}
	for i, tr := range drv.Transitions[6:11] {
		if tr.Action != "wait" {
			t.Errorf("iteration %d action = %q, want wait (the legend advances on its own)", 6+i, tr.Action)
		}
	}
	// The timing record: run 1 (iters 7-8, closed by the text-less iter 9)
	// and the open run 2 (iter 10, the observation window closed on text).
	if len(drv.SubTimings) != 2 {
		t.Fatalf("SubTimings = %+v, want 2 runs", drv.SubTimings)
	}
	if drv.SubTimings[0].FirstIndex != 7 || drv.SubTimings[0].LastIndex != 8 || drv.SubTimings[0].Open {
		t.Errorf("subtitle run 1 = %+v, want first=7 last=8 closed", drv.SubTimings[0])
	}
	if drv.SubTimings[1].FirstIndex != 10 || !drv.SubTimings[1].Open {
		t.Errorf("subtitle run 2 = %+v, want first=10 open", drv.SubTimings[1])
	}
	// The transition log names the success screen with its measured text flag
	// carried by the class (the storysub class is only claimed with text).
	log := drv.TransitionLog()
	for _, want := range []string{"story_subtitle:- --(wait)-->", "name_entry:- --(A+START+A(name))--> name_confirm:yes"} {
		if !strings.Contains(log, want) {
			t.Errorf("transition log missing %q:\n%s", want, log)
		}
	}
}

// TestDriveStorySubWinsWhenReachedFirst: when BOTH success screens are
// reached (the legend's subtitle, then the in-game dialogue box), the FIRST
// reached keeps the credit (the task's "whichever comes first").
func TestDriveStorySubWinsWhenReachedFirst(t *testing.T) {
	b := &fakeBridge{shots: []image.Image{
		solid(320, 240, 5),            // a letterboxed dark frame (the loop's start)
		synthStorySub(320, 240, true), // the legend's narration subtitle (first success)
		synthBox(320, 240),            // the in-game dialogue box (reached second)
	}}
	drv, err := RunDrive(context.Background(), b, fastOpts(90), WindWakerNewGameDrive())
	if err != nil || !drv.OK {
		t.Fatalf("drive = ok %v err %v reason %q", drv.OK, err, drv.Reason)
	}
	if drv.Reason != "story_subtitle" || drv.SuccessScreen != DriveScreenStorySubtitle {
		t.Errorf("credit = %q/%s, want story_subtitle (reached first)", drv.Reason, drv.SuccessScreen)
	}
}
