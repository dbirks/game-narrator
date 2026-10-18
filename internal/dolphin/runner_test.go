package dolphin

import (
	"context"
	"image"
	"image/color"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// fakeBridge is the runner's test double: it implements the Bridge
// interface, records every Inject in order, and serves screenshots from a
// queue (the last queued image repeats). No process, no FIFO, no X.
type fakeBridge struct {
	injects   []injectCall
	shots     []image.Image
	shotCalls int
}

type injectCall struct {
	buttons Buttons
	axes    Axes
	hold    time.Duration
}

func (f *fakeBridge) Launch(_ context.Context, _ LaunchSpec) error { return nil }

func (f *fakeBridge) Inject(_ context.Context, buttons Buttons, axes Axes, hold time.Duration) error {
	f.injects = append(f.injects, injectCall{buttons, axes, hold})
	return nil
}

func (f *fakeBridge) Screenshot(_ context.Context) (image.Image, error) {
	f.shotCalls++
	if len(f.shots) == 0 {
		return nil, context.Canceled
	}
	out := f.shots[0] // the front of the queue (or the last entry, which repeats)
	if len(f.shots) > 1 {
		f.shots = f.shots[1:]
	}
	return out, nil
}

func (f *fakeBridge) FrameSource(_ int) (capture.Source, error) { return nil, nil }
func (f *fakeBridge) SaveSnapshot(_ string) (string, error)     { return "", nil }
func (f *fakeBridge) WaitForGameFrame(_ context.Context, _ time.Duration) (Evidence, error) {
	return Evidence{}, nil
}
func (f *fakeBridge) ProveInput(_ context.Context) (Evidence, error) { return Evidence{}, nil }
func (f *fakeBridge) State() Status                                  { return Status{State: StateRunning} }
func (f *fakeBridge) SaveState(_ context.Context, _ int) (string, error) {
	return "", nil
}
func (f *fakeBridge) LoadState(_ context.Context, _ int) error { return nil }
func (f *fakeBridge) Stop()                                    {}

// solid is a flat-luma image (a blank screen at a given brightness).
func solid(w, h int, luma uint8) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	c := color.RGBA{R: luma, G: luma, B: luma}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// noisy is a deterministic bright "scene" (an xorshift over luma 80..240):
// high entropy, full-frame, and far (in RMSE) from any other seed — the
// synthetic title/UI/box scenes.
func noisy(w, h int, seed uint64) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	v := seed
	if v == 0 {
		v = 0x9e3779b97f4a7c15
	}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v ^= v << 13
			v ^= v >> 7
			v ^= v << 17
			l := uint8(80 + (v % 161)) // 80..240, ~uniform
			img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l})
		}
	}
	return img
}

// brightBox paints a mid-dark scene with a bright parchment band in the
// lower third carrying dense dark glyph strokes: the synthetic dialogue-box
// screen, built to clear every TextboxPresent clause (the real box
// calibration, ADR-0038, uses the same clauses on measured frames).
func brightBox(w, h int) *image.RGBA {
	img := solid(w, h, 90) // the deck behind the box
	for y := h - h/3; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 240, G: 235, B: 220})
		}
	}
	// Glyph strokes: one dark row every 6 rows, a dark block every 6 px
	// (50% of each stroke row dark, so the dark fraction and the contrast
	// both clear their clauses).
	for y := h - h/3; y < h; y += 6 {
		for x := 10; x < w-10; x += 6 {
			for dx := 0; dx < 3; dx++ {
				img.SetRGBA(x+dx, y, color.RGBA{R: 20, G: 15, B: 10})
			}
		}
	}
	return img
}

func ckp(c Checkpoint) *Checkpoint { return &c }

// shrinkAttemptWindow drops the attempt observation window to milliseconds
// for this test (the real 6 s floor would make the repeat-cap test take 20 s
// of wall clock).
func shrinkAttemptWindow(t *testing.T) {
	t.Helper()
	old := attemptWindowMin
	attemptWindowMin = 5 * time.Millisecond
	t.Cleanup(func() { attemptWindowMin = old })
}

// TestRunScriptStepOrderAndWire: three steps (wait, press, stick) run in
// order, the trace records the wire bytes, and the run reports OK.
//
// The shot queue is the synthetic run, one entry per Screenshot call: the
// wait step finds the title (two consecutive full-frame samples), the press
// step sees the title→UI change, the stick step sees the UI→box change, and
// each step's finalise grab consumes its own entry.
func TestRunScriptStepOrderAndWire(t *testing.T) {
	shrinkAttemptWindow(t)
	title, ui, box := noisy(320, 240, 1), noisy(320, 240, 2), brightBox(320, 240)
	fb := &fakeBridge{shots: []image.Image{
		solid(320, 240, 50), // step 1 ref: boot, dark, not a scene
		title,               // step 1 poll 1: full-frame (first)
		title,               // step 1 poll 2: full-frame (second → holds)
		title,               // step 1 finalise
		title,               // step 2 ref: the title before the press
		ui,                  // step 2 poll: the settled UI after the press
		ui,                  // step 2 finalise
		ui,                  // step 3 ref: the UI before the stick
		box,                 // step 3 poll: the dialogue box
		box,                 // step 3 finalise
	}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "wait-step", Until: ckp(FullFrameScene()), Timeout: 3 * time.Second},
			{Name: "press-step", Press: []Button{ButtonA, ButtonStart}, Hold: 5 * time.Millisecond,
				Until: ckp(SceneChanged(0.10)), Timeout: 3 * time.Second},
			{Name: "stick-step", Stick: &Stick{X: 0, Y: -1}, Hold: 5 * time.Millisecond,
				Until: ckp(All(SceneChanged(0.10), SceneChanged(0.05))), Timeout: 3 * time.Second},
		},
	}
	tr, err := RunScript(context.Background(), fb, s)
	if err != nil {
		t.Fatalf("RunScript: %v (trace: %+v)", err, tr)
	}
	if !tr.OK {
		t.Fatalf("run not OK: %s", tr.LastError)
	}
	if len(tr.Steps) != 3 {
		t.Fatalf("want 3 step traces, got %d", len(tr.Steps))
	}
	if tr.Steps[0].Wire != "" {
		t.Errorf("wait step wire = %q, want empty", tr.Steps[0].Wire)
	}
	if tr.Steps[1].Wire != "A+START" {
		t.Errorf("press step wire = %q, want A+START", tr.Steps[1].Wire)
	}
	if want := "STICK(x=0.0 y=-1.0)"; tr.Steps[2].Wire != want {
		t.Errorf("stick step wire = %q, want %q", tr.Steps[2].Wire, want)
	}
	// The fake saw the two input steps, in order, with the right payloads.
	if len(fb.injects) != 2 {
		t.Fatalf("fake saw %d injects, want 2", len(fb.injects))
	}
	if !fb.injects[0].buttons.A || !fb.injects[0].buttons.Start || fb.injects[0].buttons.B {
		t.Errorf("first inject buttons = %+v", fb.injects[0].buttons)
	}
	if fb.injects[1].axes.MainY != -1 || fb.injects[1].axes.MainX != 0 {
		t.Errorf("second inject axes = %+v", fb.injects[1].axes)
	}
	// Every step recorded both screenshots (ref and final) and sane timings.
	for i, st := range tr.Steps {
		if st.RefHash == "" || st.FinalHash == "" {
			t.Errorf("step %d: missing screenshot hashes (ref=%q final=%q)", i, st.RefHash, st.FinalHash)
		}
		if st.Start.IsZero() || st.End.Before(st.Start) {
			t.Errorf("step %d: bad timing start=%v end=%v", i, st.Start, st.End)
		}
		if st.RefPath != "" || st.FinalPath != "" {
			t.Errorf("step %d: no evidence dir named, paths must be empty", i)
		}
	}
}

// TestRunScriptRepeatCap: a press step whose checkpoint never holds
// re-injects exactly MaxRepeats extra times, then fails the run (the later
// step never runs) — the repeat cap, not the timeout, ends the step.
func TestRunScriptRepeatCap(t *testing.T) {
	shrinkAttemptWindow(t)
	fb := &fakeBridge{shots: []image.Image{solid(320, 240, 50)}} // never changes
	s := Script{
		// 2 ms cadence: three windows of 8 ms keep the nominal wall clock
		// (~25 ms of sleeps) so loaded hosts (another worker running
		// emulators) cannot inflate the poll sleeps past the elapsed bound
		// below — the repeat cap is asserted by the ATTEMPT COUNT, the
		// timing only guards that the deadline is not the thing that ran out.
		Poll: 2 * time.Millisecond,
		Steps: []Step{
			{Name: "never-advances", Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(SceneChanged(0.99)), Timeout: 1 * time.Second, MaxRepeats: 2},
			{Name: "must-not-run", Press: []Button{ButtonB}, Hold: 2 * time.Millisecond, Until: nil},
		},
	}
	start := time.Now()
	tr, err := RunScript(context.Background(), fb, s)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("run should fail (checkpoint never met)")
	}
	if tr.OK || len(tr.Steps) != 1 {
		t.Fatalf("run must fail at step 0 with the later step untraced: tr=%+v", tr)
	}
	st := tr.Steps[0]
	if st.Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (1 + MaxRepeats 2)", st.Attempts)
	}
	if st.Wire != "A;A;A" {
		t.Errorf("wire = %q, want A;A;A", st.Wire)
	}
	if len(fb.injects) != 3 {
		t.Errorf("fake saw %d injects, want 3", len(fb.injects))
	}
	// The cap (3 attempts of an 8ms window) is what bound the INPUT, not the
	// 1 s deadline: with 1 s and an 8 ms window a hundred attempts would
	// have fit, yet exactly three happened. The deadline is kept
	// load-tolerant: a tight deadline on a loaded host can end the step
	// before the cap, which would make the attempts assertion vacuous.
	// (The runner waits out the rest of the deadline after the attempt
	// budget is spent, so elapsed lands just above the deadline.)
	if elapsed > 1500*time.Millisecond {
		t.Errorf("step took %s, want just over the 1s deadline, not more (the cap, not the timeout, ends it)", elapsed)
	}
	if st.FinalHash == "" || st.RefHash == "" {
		t.Errorf("failed step must hand back screenshot hashes (ref=%q final=%q)", st.RefHash, st.FinalHash)
	}
	if st.RefPath != st.FinalPath {
		// No evidence dir: the handback falls back to the step-start path.
		t.Logf("note: ref=%q final=%q", st.RefPath, st.FinalPath)
	}
}

// TestRunScriptTimeout: a wait-only step whose checkpoint never holds fails
// at its deadline, with the per-evaluation details retained.
func TestRunScriptTimeout(t *testing.T) {
	shrinkAttemptWindow(t)
	fb := &fakeBridge{shots: []image.Image{solid(320, 240, 50)}} // never a scene
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "never-settles", Until: ckp(FullFrameScene()), Timeout: 200 * time.Millisecond},
		},
	}
	start := time.Now()
	tr, err := RunScript(context.Background(), fb, s)
	elapsed := time.Since(start)
	if err == nil || tr.OK {
		t.Fatalf("run should fail at the step deadline; tr=%+v err=%v", tr, err)
	}
	if elapsed < 150*time.Millisecond || elapsed > 5*time.Second {
		t.Errorf("step ran %s, want ~200ms (deadline-bounded)", elapsed)
	}
	if len(tr.Steps[0].Details) < 2 {
		t.Errorf("details = %v, want at least two evaluations", tr.Steps[0].Details)
	}
}

// TestRunScriptPreSatisfied: a checkpoint that holds at step start passes the
// step with NO input sent (the previous step's overshoot case).
func TestRunScriptPreSatisfied(t *testing.T) {
	fb := &fakeBridge{shots: []image.Image{brightBox(320, 240)}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "already-there", Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(TextboxPresent()), Timeout: 3 * time.Second},
		},
	}
	tr, err := RunScript(context.Background(), fb, s)
	if err != nil || !tr.OK {
		t.Fatalf("pre-satisfied step must pass: tr=%+v err=%v", tr, err)
	}
	if len(fb.injects) != 0 {
		t.Errorf("pre-satisfied step sent %d injects, want 0", len(fb.injects))
	}
	if tr.Steps[0].Attempts != 0 {
		t.Errorf("attempts = %d, want 0 (no attempt was made)", tr.Steps[0].Attempts)
	}
}

// TestRunScriptSkipUnless: a screen-specific step (SkipUnless) runs ONLY on
// its own screen — when the step-start screen is not the one the step
// handles (the continue-anyway dialog is absent on this path), the step is
// SKIPPED: ok, zero input, no attempts (the run takes the other path, so
// skipping is the success branch — ADR-0043 amendment 2). When the screen
// IS the step's screen, the step runs normally.
func TestRunScriptSkipUnless(t *testing.T) {
	shrinkAttemptWindow(t)
	box := synthBox(320, 240) // the box-class synthetic (class=box, key=box)
	contNo := synthContinue(320, 240, "no")
	contYes := synthContinue(320, 240, "yes")

	// Path 1: the box is on screen (a working card took the drive past the
	// save prompt) — both continue-handler steps must skip (zero input).
	fb := &fakeBridge{shots: []image.Image{
		box, // step 1 (move-cursor-to-yes-continue) ref: NOT the continue dialog
		box, // step 2 (confirm-continue-anyway) ref: still NOT it
	}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "move-cursor-to-yes-continue", SkipUnless: ckp(ContinuePrompt()),
				Press: []Button{ButtonLeft}, Hold: 2 * time.Millisecond,
				Until: ckp(ContinuePromptYes()), Timeout: 3 * time.Second},
			{Name: "confirm-continue-anyway", SkipUnless: ckp(ContinuePrompt()),
				Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassBox)), Timeout: 3 * time.Second},
		},
	}
	tr, err := RunScript(context.Background(), fb, s)
	if err != nil || !tr.OK {
		t.Fatalf("skipped steps must leave the run OK: tr=%+v err=%v", tr, err)
	}
	if len(fb.injects) != 0 {
		t.Errorf("skipped steps sent %d injects, want 0 (zero input on the wrong screen)", len(fb.injects))
	}
	for i, st := range tr.Steps {
		if !st.OK || !st.Skipped || st.Attempts != 0 {
			t.Errorf("step %d = %+v, want ok+skipped+0 attempts", i, st)
		}
	}

	// Path 2: the continue dialog IS on screen (the save failed) — the steps
	// run: the cursor moves No → Yes (the strict gate holds), then the
	// confirming A lands on the box.
	fb2 := &fakeBridge{shots: []image.Image{
		contNo,  // step 1 ref: the continue dialog (cursor on No)
		contYes, // step 1 poll: the cursor moved to Yes (the gate holds)
		contYes, // step 1 finalise
		contYes, // step 2 ref: the dialog is still on screen (cursor on Yes) → the step runs
		box,     // step 2 poll: the A press left the dialog → the box
		box,     // step 2 finalise
	}}
	s2 := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "move-cursor-to-yes-continue", SkipUnless: ckp(ContinuePrompt()),
				Press: []Button{ButtonLeft}, Hold: 2 * time.Millisecond,
				Until: ckp(ContinuePromptYes()), Timeout: 3 * time.Second},
			{Name: "confirm-continue-anyway", SkipUnless: ckp(ContinuePrompt()),
				Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassBox)), Timeout: 3 * time.Second},
		},
	}
	tr2, err := RunScript(context.Background(), fb2, s2)
	if err != nil || !tr2.OK {
		t.Fatalf("the continue-handler path must run (not skip): tr=%+v err=%v", tr2, err)
	}
	if len(fb2.injects) != 2 {
		t.Errorf("the continue-handler path sent %d injects, want 2 (the cursor move, then A)", len(fb2.injects))
	}
	if tr2.Steps[0].Skipped || tr2.Steps[1].Skipped {
		t.Errorf("neither step may skip on the continue dialog: %+v", tr2.Steps)
	}
}

// TestRunScriptContinueLoopFailsFast: the save-failure prompt loops exactly
// like the create-save prompt — a step that starts on the continue dialog
// and whose press leaves the SAME dialog (same key continue:no) must fail
// fast with "loop detected", never pass.
func TestRunScriptContinueLoopFailsFast(t *testing.T) {
	old := attemptWindowMin
	attemptWindowMin = 50 * time.Millisecond
	t.Cleanup(func() { attemptWindowMin = old })
	contNo := synthContinue(320, 240, "no")
	// ref = the continue dialog (cursor on No), and every poll returns the
	// SAME dialog: the A press selected No (or did nothing) — a loop.
	fb := &fakeBridge{shots: []image.Image{contNo, contNo, contNo, contNo, contNo}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "confirm-continue-anyway", Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassBox)), Timeout: 5 * time.Second, MaxRepeats: 3},
		},
	}
	start := time.Now()
	tr, err := RunScript(context.Background(), fb, s)
	elapsed := time.Since(start)
	if err == nil || tr.OK {
		t.Fatalf("a looped continue dialog must fail the run; tr=%+v err=%v", tr, err)
	}
	if !strings.Contains(strings.ToLower(tr.LastError), "loop detected") {
		t.Fatalf("the failure must say loop detected, got: %s", tr.LastError)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("step took %s, want fast (the loop guard, not the deadline, must end it)", elapsed)
	}
	if st := tr.Steps[0]; st.FinalSig != "continue:no" {
		t.Errorf("final signature = %q, want the looped continue key", st.FinalSig)
	}
}

// TestRunScriptEvidence: the per-step PNGs land in the evidence dir when one
// is named, and the ref/final hashes differ (the screen actually changed).
func TestRunScriptEvidence(t *testing.T) {
	dir := t.TempDir()
	fb := &fakeBridge{shots: []image.Image{solid(320, 240, 50), brightBox(320, 240)}}
	s := Script{
		Poll:        10 * time.Millisecond,
		EvidenceDir: dir,
		Steps: []Step{
			{Name: "to-the-box", Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(TextboxPresent()), Timeout: 3 * time.Second},
		},
	}
	tr, err := RunScript(context.Background(), fb, s)
	if err != nil || !tr.OK {
		t.Fatalf("run should reach the box: tr=%+v err=%v", tr, err)
	}
	st := tr.Steps[0]
	if st.RefPath == "" || st.FinalPath == "" {
		t.Fatalf("evidence paths missing: ref=%q final=%q", st.RefPath, st.FinalPath)
	}
	if st.RefHash == st.FinalHash {
		t.Errorf("ref and final hashes equal: the screen did not change?")
	}
	// The box detail was measured and recorded.
	if len(st.Details) == 0 {
		t.Errorf("no checkpoint details recorded")
	}
}

// synthPrompt builds a synthetic Yes/No memory-card prompt (a bright centre
// panel over a dark sky with bright top/bottom bands) that ClassOf measures
// as ClassPrompt — the test double for the prompt-loop scenarios.
func synthPrompt(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 60) // dark sky
		}
	}
	for y := int(0.30 * float64(h)); y < int(0.65*float64(h)); y++ { // centre panel
		for x := int(0.30 * float64(w)); x < int(0.70*float64(w)); x++ {
			set(x, y, 150)
		}
	}
	for y := 0; y < 30; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 150) // top band
		}
	}
	for y := h - 30; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 150) // bottom band
		}
	}
	return img
}

// synthBox builds a synthetic first-dialogue screen (a mid-dark scene with a
// bright, glyph-inked box band in the measured box-band window) that ClassOf
// measures as ClassBox and BoxPresent holds on — the test double for the
// box the drive drives toward.
func synthBox(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	set := func(x, y int, l uint8) { img.SetRGBA(x, y, color.RGBA{R: l, G: l, B: l}) }
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			set(x, y, 90) // mid-dark scene
		}
	}
	for y := int(0.69 * float64(h)); y < int(0.84*float64(h)); y++ { // bright box band
		for x := int(0.31 * float64(w)); x < int(0.75*float64(w)); x++ {
			set(x, y, 235)
		}
	}
	for y := int(0.69 * float64(h)); y < int(0.84*float64(h)); y += 4 { // glyph strokes
		for x := int(0.33 * float64(w)); x < int(0.73*float64(w)); x += 5 {
			for dx := 0; dx < 2; dx++ {
				set(x+dx, y, 15)
			}
		}
	}
	return img
}

// TestLoopGuardPure: the guard arms only on a prompt ref, fires after
// loopConfirmSamples consecutive same-key samples (once an attempt has
// happened), and a changed screen resets the count.
func TestLoopGuardPure(t *testing.T) {
	if g := newLoopGuard("title"); g != nil {
		t.Fatalf("guard must not arm on a non-prompt ref")
	}
	g := newLoopGuard("prompt:no")
	if g == nil {
		t.Fatalf("guard must arm on a prompt ref")
	}
	g.observe("prompt:no", 0)
	g.observe("prompt:no", 0)
	if g.fired {
		t.Fatalf("no input yet: the guard must not fire")
	}
	g.observe("prompt:no", 1)
	g.observe("prompt:no", 1)
	if g.fired {
		t.Fatalf("must not fire before %d consecutive samples", loopConfirmSamples)
	}
	g.observe("prompt:no", 1)
	if !g.fired || g.key != "prompt:no" {
		t.Fatalf("must fire after %d consecutive same-key samples (key=%q)", loopConfirmSamples, g.key)
	}
	// A changed screen resets the count.
	g2 := newLoopGuard("prompt:no")
	g2.observe("prompt:no", 1)
	g2.observe("prompt:yes", 1) // the cursor moved: reset
	g2.observe("prompt:no", 1)
	g2.observe("prompt:no", 1)
	if g2.fired {
		t.Fatalf("a changed screen in between must reset the loop count")
	}
}

// TestRunScriptPromptLoopFailsFast: a step that starts on the prompt and
// presses A but the screen stays on the same prompt (the Yes/No loop) must
// FAIL the run with "loop detected" — it must never pass.
func TestRunScriptPromptLoopFailsFast(t *testing.T) {
	old := attemptWindowMin
	attemptWindowMin = 50 * time.Millisecond
	t.Cleanup(func() { attemptWindowMin = old })
	p := synthPrompt(320, 240)
	// ref = prompt, then every poll returns the SAME prompt (the press did
	// not move the cursor / did not advance): a loop.
	fb := &fakeBridge{shots: []image.Image{p, p, p, p, p, p}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "confirm-yes", Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassBox)), Timeout: 5 * time.Second, MaxRepeats: 3},
		},
	}
	start := time.Now()
	tr, err := RunScript(context.Background(), fb, s)
	elapsed := time.Since(start)
	if err == nil || tr.OK {
		t.Fatalf("a looped prompt must fail the run; tr=%+v err=%v", tr, err)
	}
	if !strings.Contains(strings.ToLower(tr.LastError), "loop detected") {
		t.Fatalf("the failure must say loop detected, got: %s", tr.LastError)
	}
	// It must fail FAST (well under the 5 s deadline): the loop guard, not
	// the timeout, ends the step.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("step took %s, want fast (the loop guard, not the deadline, must end it)", elapsed)
	}
	if st := tr.Steps[0]; st.FinalSig != "prompt:-" {
		t.Errorf("final signature = %q, want the looped prompt key", st.FinalSig)
	}
}

// TestRunScriptPromptNoLoopOnAdvance: the same step, but the press ADVANCES
// the screen (prompt → box): the loop guard must NOT fire and the step
// passes (the box checkpoint holds).
func TestRunScriptPromptNoLoopOnAdvance(t *testing.T) {
	old := attemptWindowMin
	attemptWindowMin = 50 * time.Millisecond
	t.Cleanup(func() { attemptWindowMin = old })
	p := synthPrompt(320, 240)
	box := synthBox(320, 240)
	// ref = prompt, then the press advances to the box (the checkpoint holds).
	fb := &fakeBridge{shots: []image.Image{p, box, box}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "confirm-yes", Press: []Button{ButtonA}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassBox)), Timeout: 2 * time.Second, MaxRepeats: 3},
		},
	}
	tr, err := RunScript(context.Background(), fb, s)
	if err != nil || !tr.OK {
		t.Fatalf("an advancing press must pass (no false loop); tr=%+v err=%v", tr, err)
	}
	if st := tr.Steps[0]; st.FinalSig != "box" {
		t.Errorf("final signature = %q, want box", st.FinalSig)
	}
}

// TestRunScriptFileSelLoopFailsFast: a step that starts on the quest-log
// file selection (a stable menu) and whose press leaves it unchanged must
// FAIL fast with "loop detected" — the measured A/START-alone no-op (ADR-
// 0043 amendment 3) must fail, not pass, and not burn the deadline.
func TestRunScriptFileSelLoopFailsFast(t *testing.T) {
	old := attemptWindowMin
	attemptWindowMin = 50 * time.Millisecond
	t.Cleanup(func() { attemptWindowMin = old })
	fs := synthFileSel(320, 240)
	fb := &fakeBridge{shots: []image.Image{fs, fs, fs, fs, fs, fs}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "choose-file", Press: []Button{ButtonA, ButtonStart}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassNameEnt)), Timeout: 5 * time.Second, MaxRepeats: 3},
		},
	}
	start := time.Now()
	tr, err := RunScript(context.Background(), fb, s)
	elapsed := time.Since(start)
	if err == nil || tr.OK {
		t.Fatalf("a looped file selection must fail the run; tr=%+v err=%v", tr, err)
	}
	if !strings.Contains(strings.ToLower(tr.LastError), "loop detected") {
		t.Fatalf("the failure must say loop detected, got: %s", tr.LastError)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("step took %s, want fast (the loop guard, not the deadline, must end it)", elapsed)
	}
	if st := tr.Steps[0]; st.FinalSig != "filesel" {
		t.Errorf("final signature = %q, want the looped filesel key", st.FinalSig)
	}
}

// TestRunScriptFileSelAdvancePasses: the same step, but the A+START press
// LEAVES the file selection (→ name entry): the guard must not fire and the
// step passes.
func TestRunScriptFileSelAdvancePasses(t *testing.T) {
	old := attemptWindowMin
	attemptWindowMin = 50 * time.Millisecond
	t.Cleanup(func() { attemptWindowMin = old })
	fs := synthFileSel(320, 240)
	ne := synthNameEnt(320, 240)
	fb := &fakeBridge{shots: []image.Image{fs, ne, ne}}
	s := Script{
		Poll: 10 * time.Millisecond,
		Steps: []Step{
			{Name: "choose-file", Press: []Button{ButtonA, ButtonStart}, Hold: 2 * time.Millisecond,
				Until: ckp(ScreenIsClass(ClassNameEnt)), Timeout: 2 * time.Second},
		},
	}
	tr, err := RunScript(context.Background(), fb, s)
	if err != nil || !tr.OK {
		t.Fatalf("a filesel→nameent advance must pass; tr=%+v err=%v", tr, err)
	}
	if st := tr.Steps[0]; st.FinalSig != "nameent" {
		t.Errorf("final signature = %q, want nameent", st.FinalSig)
	}
}
