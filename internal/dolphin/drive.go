package dolphin

import (
	"context"
	"fmt"
	"image"
	"strings"
	"time"
)

// ============================================================================
// The screen-driven drive (ADR-0043 amendment 5)
// ============================================================================
//
// The M6 drive to the first in-game dialogue box is a SCREEN-DRIVEN LOOP, not
// a linear step list. The linear form produced the false-positive structure
// the spec forbids (the 2026-10-17 run: steps that "passed" pre-satisfied or
// skipped because the previous step's overshoot landed on a different
// measured screen than the step assumed — the drive sat on the name-entry
// screen for 166 s while a step believed it was on the opening). Each
// iteration of the loop:
//
//  1. classifies the CURRENT screen (a measured class, never "changed");
//  2. picks the ONE action the screen's class prescribes;
//  3. sends it (or waits, for wait-only screens) and lets the screen settle;
//  4. re-classifies — logging (screen, action) -> next screen for every
//     iteration.
//
// Termination:
//
//   - the screen classifies as the in-game dialogue box -> success;
//   - the SAME screen key observed on >= 4 consecutive iterations with NO
//     visible change (max RMSE < driveLoopChangeMin) -> "loop detected"
//     (the input the screen prescribes has no effect there — the press loop
//     the spec's "avoiding false positives" forbids);
//   - the iteration cap or the time budget expires -> failure with the FULL
//     transition log and the last retained PNG (the evidence handoff).
//
// There are no pre-satisfied steps and no skipped steps: the action always
// comes from the screen that is actually on screen.

// DriveScreen is one screen family the drive loop knows.
type DriveScreen string

const (
	DriveScreenTitle         DriveScreen = "title"          // the title screen (either measured phase)
	DriveScreenPrompt        DriveScreen = "prompt"         // a Yes/No prompt (cursor side reported)
	DriveScreenSaveNotice    DriveScreen = "save_notice"    // the blobless system dialog (save-creation notices, info)
	DriveScreenFileSelect    DriveScreen = "file_select"    // the quest-log file list
	DriveScreenNameEntry     DriveScreen = "name_entry"     // the name-entry keyboard grid (measured, amendment 5)
	DriveScreenNameConfirm   DriveScreen = "name_confirm"   // the "is this name OK?" prompt after the name entry
	DriveScreenCutscene      DriveScreen = "cutscene"       // a letterboxed scene / the opening letter / a transition
	DriveScreenStorySubtitle DriveScreen = "story_subtitle" // the opening legend's letterbook panel carrying the narration subtitle (amendment 6; the drive's success screen — the first in-game narrated text)
	DriveScreenDialogueBox   DriveScreen = "dialogue_box"   // a rendered scene carrying the dialogue box (the drive's other success screen)
	DriveScreenUnknown       DriveScreen = "unknown"        // unmeasured frame (a fade, a blank)
)

// DriveOptions bounds one drive run.
type DriveOptions struct {
	// Deadline: the wall-clock budget for the whole run (zero =
	// defaultDriveDeadline).
	Deadline time.Duration
	// MaxIterations: the iteration cap (zero = defaultDriveIterations).
	MaxIterations int
	// Settle: the quiet time after each action before re-classifying (zero =
	// defaultDriveSettle; a measured fade takes 1-2 s of emulation).
	Settle time.Duration
	// EvidenceDir: "" = no PNGs (hashes/keys only), otherwise one PNG per
	// iteration (drive-iter-NNN-<screen>.png) — the frame-by-frame record.
	EvidenceDir string
}

// DriveTransition is one logged iteration: (screen, action) -> next screen
// (the next screen is the NEXT transition's classified screen; the run's
// final transition logs "<end>").
type DriveTransition struct {
	Index  int       // 0-based iteration number
	Time   time.Time // when the iteration's screenshot was taken
	Screen DriveScreen
	Side   CursorSide // the measured cursor side (prompts only)
	Action string     // the input sent ("A", "START", "START+A", "STICK(-1,0)x900ms", "A(text)", "wait")
	Detail string     // the measured signature line of this iteration's frame
	Text   bool       // the frame measured the legend's narration text (the subtitle timing record's sample)
}

// SubTiming is one narration subtitle's measured visibility record (the
// fade-in/hold/fade-out the recognition pipeline needs, sampled at the
// drive's iteration cadence — each span is bracketed by samples, so a
// 0 value on an open span means "still going when the run ended" or
// "the fade is shorter than the cadence and was not sampled").
type SubTiming struct {
	FirstIndex int           // the first text-bearing iteration
	LastIndex  int           // the last text-bearing iteration
	FadeIn     time.Duration // the text-less sample before the first text sample -> that first text sample
	Hold       time.Duration // the first text sample -> the last text sample (the measured fully-visible span)
	FadeOut    time.Duration // the last text sample -> the next text-less sample (0 when open at the run's end)
	Open       bool          // the run ended while this subtitle's text was still up
}

// DriveTrace is the whole run's record: the full transition log (the
// orchestrator's report), the outcome, and the last frame's evidence.
type DriveTrace struct {
	OK          bool
	Reason      string // "story_subtitle", "dialogue_box", "loop detected: ...", "budget exhausted: ...", "cancelled: ..."
	Transitions []DriveTransition
	LastKey     string // the last screen's key (class:side)
	LastPath    string // the last retained PNG (the failure/success handoff)
	// SuccessScreen/SuccessPath/SuccessText name the SUCCESS frame (the first
	// story-subtitle frame WITH text, or the first dialogue box — whichever
	// the loop reached first): SuccessPath is its evidence PNG (the crop-box
	// handoff frame), SuccessText whether its bottom bar measured the text.
	SuccessScreen DriveScreen
	SuccessPath   string
	SuccessText   bool
	// SubCrop is the measured bottom-bar subtitle region in normalised frame
	// fractions (x0, y0, x1, y1) — the live-session E2E's textbox-region
	// configuration (ADR-0043 amendment 6). Set on a story-subtitle success.
	SubCrop [4]float64
	// SubTimings is the narration visibility record (requirement 3): one
	// SubTiming per measured text run, in order.
	SubTimings []SubTiming
	Started    time.Time
	Finished   time.Time
}

// TransitionLog renders the full (screen, action) -> next screen log — one
// line per iteration, the report the task asks for.
func (t DriveTrace) TransitionLog() string {
	var b strings.Builder
	for i, tr := range t.Transitions {
		next := "<end>"
		if i+1 < len(t.Transitions) {
			n := t.Transitions[i+1]
			next = string(n.Screen)
			if n.Screen == DriveScreenPrompt || n.Screen == DriveScreenNameConfirm {
				next += ":" + string(n.Side)
			}
		}
		fmt.Fprintf(&b, "[%3d] %s:%s --(%s)--> %s (%s)\n",
			tr.Index, tr.Screen, tr.Side, tr.Action, next, tr.Detail)
	}
	return b.String()
}

// The drive's defaults (measured on this host, llvmpipe: a fade settles in
// 1-2 s of emulation; the drive must never out-run the screens).
//
// defaultDriveIterations is the iteration BACKSTOP, not the expected length:
// the 2026-10-18 slow-START boot (the title's dim "press START" phase did
// not register the press for ~54 iterations — a measured ~2.5 min of title
// before the prompt appeared) still needs to reach the legend, and the loop
// detector (the same screen with no visible change >= 4 iterations) already
// fails fast on a screen that is truly dead. The 15-minute time budget is
// the true limit; the cap only bounds a pathological no-op loop the loop
// detector would miss (a screen that keeps "changing" while ignoring the
// input, like the orbiting title).
const (
	defaultDriveDeadline     = 15 * time.Minute
	defaultDriveIterations   = 200
	defaultDriveSettle       = 2 * time.Second
	driveScreenshotRetryWait = 1 * time.Second
	driveScreenshotRetries   = 3
)

// driveLoopChangeMin is the "visible change" floor for the loop detector
// (RMSE, 0..1): a screen that renders STATIC stays within ~0.005 of its own
// previous sample (the llvmpipe render noise); a cursor blink moves ~0.03+;
// a text advance / page turn moves 0.05+. The loop detector fires only when
// the screen is the SAME class for >= driveLoopSameMin consecutive
// iterations AND the max change between consecutive samples stayed below
// this floor — an animated cutscene (frames always changing) or a blinking
// prompt therefore never reads as a loop, while a dead screen (the input
// has no effect) fails fast. The detector ARMS only on NON-WAIT actions
// (amendment 6): the measured WOTW flow carries a timed pure-black
// transition (file selection -> the legend) whose retained frames show
// 4-cadence samples of 0.0000 change inside the ~28 s black span — a
// static wait-only screen is a transition in progress, not a dead screen,
// so a wait iteration never arms the detector; the success screen likewise
// disarms it (the legend's hold phases are static wait-only frames by
// design — the narration advances on its own, nothing is pressed), so a
// static text hold is not a dead screen either.
const (
	// storySubObserveIters bounds the legend observation after the success
	// frame is recorded (amendment 6): the drive keeps sampling (WAIT-ONLY —
	// nothing is pressed, the legend advances on its own) until one full
	// subtitle run is measured (a text run closed by a text-less sample: the
	// fade-in/hold/fade-out record the recognition pipeline needs) or this
	// many post-success story-subtitle iterations have passed (~75 s at the
	// 2 s settle cadence; the measured legend runs 40+ s, so the budget
	// covers multiple subtitles and ends on a closed run in practice).
	storySubObserveIters = 30
	driveLoopChangeMin   = 0.02
	driveLoopSameMin     = 4 // consecutive same-screen iterations with no change
)

// StorySubCrop is the measured bottom-bar narration-subtitle region
// (normalised frame fractions x0, y0, x1, y1) — the live-session E2E's
// textbox-region configuration (ADR-0043 amendment 6): the union bbox of
// the retained legend frames' bright text pixels (measured x 0.155-0.841,
// y 0.792-0.875 on the 19 text-bearing frames of the 2026-10-17 run) padded
// ~0.035 per side for the longer lines and the fade states the drive samples
// between them.
var StorySubCrop = [4]float64{0.12, 0.75, 0.88, 0.92}

// DriveAction is the input one drive iteration sends (or wait, for
// wait-only screens). Compound actions (the name entry's START-then-A) are
// one action, logged as one wire description.
type DriveAction struct {
	Wire string // the trace's wire description ("A", "START+A", ...)
	Run  func(ctx context.Context, b Bridge) error
}

// ActionTable picks the iteration's action from the measured screen. It is
// the game-specific half of the drive (the Wind Waker table in
// scripts_windwaker.go); prev/seenNameEntry carry the one piece of history
// the measured screens need (a Yes/No prompt seen right after the name entry
// is the name-CONFIRM prompt, not the create-save prompt).
type ActionTable func(screen DriveScreen, side CursorSide, prevScreen DriveScreen, seenNameEntry bool, frame image.Image) DriveAction

// RunDrive runs the screen-driven loop against the bridge and returns the
// full transition log. It is the only thing that classifies screens and
// sends inputs: the linear Script/RunScript machinery stays available for
// other bounded drives, but the M6 drive-to-first-dialogue is this loop.
func RunDrive(ctx context.Context, b Bridge, opts DriveOptions, table ActionTable) (DriveTrace, error) {
	if opts.Deadline <= 0 {
		opts.Deadline = defaultDriveDeadline
	}
	if opts.MaxIterations <= 0 {
		opts.MaxIterations = defaultDriveIterations
	}
	if opts.Settle <= 0 {
		opts.Settle = defaultDriveSettle
	}
	deadline := time.Now().Add(opts.Deadline)

	tr := DriveTrace{Started: time.Now()}
	var (
		prevImg    image.Image
		prevKey    string
		prevScreen DriveScreen
		streak     int = 1 // the first sample starts a same-key run of one (the bookkeeping below compares against the PREVIOUS sample, so the baseline counts itself)
		maxChange  float64
		seenName   bool
		obsIters   int // story-subtitle observations after the success frame (amendment 6)
	)

	// finishSuccess closes the run as a SUCCESS on its recorded success
	// screen (amendment 6): the reason is the success screen's name, the
	// transition log is complete, and the subtitle-timing record is built
	// from the measured text presence of every iteration.
	finishSuccess := func(reason string) (DriveTrace, error) {
		tr.OK = true
		tr.Reason = reason
		tr.SubTimings = subTimingsOf(tr.Transitions)
		tr.Finished = time.Now()
		return tr, nil
	}

	screenshot := func() (image.Image, error) {
		var last error
		for i := 0; i < driveScreenshotRetries; i++ {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			img, err := b.Screenshot(ctx)
			if err == nil {
				return img, nil
			}
			last = err
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(driveScreenshotRetryWait):
			}
		}
		return nil, last
	}

	for iter := 0; iter < opts.MaxIterations; iter++ {
		if ctx.Err() != nil {
			if tr.SuccessScreen != "" {
				return finishSuccess(string(tr.SuccessScreen))
			}
			tr.Finished = time.Now()
			tr.Reason = "cancelled: " + ctx.Err().Error()
			return tr, fmt.Errorf("dolphin: drive cancelled: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			if tr.SuccessScreen != "" {
				// The success frame was recorded before the budget closed: the
				// success condition is met (the timing record is partial, its
				// open runs marked Open).
				return finishSuccess(string(tr.SuccessScreen))
			}
			tr.Finished = time.Now()
			tr.Reason = fmt.Sprintf("budget exhausted: %d iterations in %s (last screen %s)",
				iter, time.Since(tr.Started).Round(time.Second), tr.LastKey)
			return tr, fmt.Errorf("dolphin: drive budget exhausted after %d iterations (last screen %s; full log above)", iter, tr.LastKey)
		}

		img, err := screenshot()
		if err != nil {
			tr.Finished = time.Now()
			tr.Reason = "screenshot failed: " + err.Error()
			return tr, fmt.Errorf("dolphin: drive screenshot: %w", err)
		}
		sig := ScreenSigOf(img)
		screen, side := DriveScreenOf(sig)
		// The name-confirm prompt (amendment 5): a Yes/No prompt seen AFTER
		// the name entry is the "is this name OK?" confirm (same measured
		// shape as the create-save prompt — the cursor blob is what any
		// WOTW Yes/No carries). It gets its own label so the transition log
		// names the screen, and its own loop-detector key (a re-raise of the
		// save prompt after naming must read as a NEW screen, not as the
		// create-save prompt looping).
		effective := screen
		if screen == DriveScreenPrompt && seenName {
			effective = DriveScreenNameConfirm
		}
		key := screenKey(effective, side)

		// The iteration's record: the screen now on screen and the action
		// its class prescribes.
		act := table(effective, side, prevScreen, seenName, img)
		nt := DriveTransition{Index: iter, Time: time.Now(), Screen: effective, Side: side,
			Action: act.Wire, Detail: sig.SigLine()}
		if screen == DriveScreenNameEntry {
			seenName = true
		}
		if screen == DriveScreenStorySubtitle {
			nt.Text = true // the class is only claimed on a frame that MEASURES the bottom-bar text
		}

		// The goal (amendment 6): the first in-game narrated text, whichever
		// measured success screen the loop reaches FIRST — the opening
		// legend's narration subtitle (a story_subtitle frame, which is only
		// claimed with the measured bottom-bar text) or the full-frame
		// dialogue box. The first one reached keeps the credit.
		switch {
		case screen == DriveScreenDialogueBox:
			if tr.SuccessScreen == "" {
				tr.SuccessScreen = DriveScreenDialogueBox
			}
			tr.Transitions = append(tr.Transitions, nt)
			tr.LastKey = key
			if opts.EvidenceDir != "" {
				p, _ := storeEvidence(opts.EvidenceDir, fmt.Sprintf("drive-iter-%03d-%s.png", iter, screen), img)
				tr.LastPath = p
				if tr.SuccessPath == "" {
					tr.SuccessPath = p
				}
			}
			return finishSuccess(string(tr.SuccessScreen))

		case screen == DriveScreenStorySubtitle:
			if tr.SuccessScreen == "" {
				tr.SuccessScreen = DriveScreenStorySubtitle
				tr.SuccessText = true
				tr.SubCrop = StorySubCrop // the measured bottom-bar crop, normalised
			}
			// The legend advances on its own: NOTHING is pressed on it (a
			// press may skip a subtitle). The loop keeps observing wait-only
			// (the observation-close check below) to record each subtitle's
			// fade-in/hold/fade-out.
		}

		// Send the action (wait-only actions just sleep) and settle.
		if err := act.Run(ctx, b); err != nil {
			if tr.SuccessScreen != "" {
				return finishSuccess(string(tr.SuccessScreen))
			}
			tr.Transitions = append(tr.Transitions, nt)
			tr.LastKey = key
			tr.Finished = time.Now()
			tr.Reason = "inject failed: " + err.Error()
			return tr, fmt.Errorf("dolphin: drive inject on %s: %w", effective, err)
		}
		if act.Wire != "wait" {
			if err := sleepCtx(ctx, opts.Settle); err != nil {
				if tr.SuccessScreen != "" {
					return finishSuccess(string(tr.SuccessScreen))
				}
				tr.Transitions = append(tr.Transitions, nt)
				tr.LastKey = key
				tr.Finished = time.Now()
				tr.Reason = "cancelled: " + err.Error()
				return tr, fmt.Errorf("dolphin: drive cancelled during settle: %w", err)
			}
		}

		// The loop detector: the same screen key on >= driveLoopSameMin
		// consecutive iterations with no visible change (the input has no
		// effect on this screen — the press loop the spec forbids).
		// The streak bookkeeping runs EVERY iteration (the
		// screen key and the max change between consecutive samples are
		// measured regardless of the action), so a static screen sampled
		// through wait iterations still counts its run length.
		if prevImg != nil {
			if key == prevKey {
				streak++
				if d := rmse01(img, prevImg, normW, normH); d > maxChange {
					maxChange = d
				}
			} else {
				streak = 1
				maxChange = 0
			}
		}
		// The loop detector FIRES only on a NON-WAIT action (amendment 6):
		// it guards against ineffective INPUT — a dead press loop (A/START/
		// STICK with no effect) fails fast on its own non-wait iteration —
		// never against a timed transition the drive correctly waits through:
		// the measured WOTW flow carries a timed pure-black transition (file
		// selection -> the legend) whose retained frames (the 2026-10-17
		// re-run's 057-063) show 4-cadence samples of 0.0000 change inside
		// the ~28 s black span, which a wait-only-armed detector would read
		// as a dead screen although the game advances on its own. The success
		// screen likewise disarms it (the legend's hold phases are static
		// wait-only frames by design).
		if tr.SuccessScreen == "" && act.Wire != "wait" && streak >= driveLoopSameMin && maxChange < driveLoopChangeMin {
			tr.Transitions = append(tr.Transitions, nt)
			tr.LastKey = key
			if opts.EvidenceDir != "" {
				p, _ := storeEvidence(opts.EvidenceDir, fmt.Sprintf("drive-iter-%03d-%s.png", iter, effective), img)
				tr.LastPath = p
			}
			tr.Finished = time.Now()
			tr.Reason = fmt.Sprintf("loop detected: screen %s unchanged for %d consecutive iterations (max frame change %.4f < %.2f; last action %q)",
				key, streak, maxChange, driveLoopChangeMin, act.Wire)
			return tr, fmt.Errorf("dolphin: %s", tr.Reason)
		}

		prevImg, prevKey, prevScreen = img, key, effective
		tr.Transitions = append(tr.Transitions, nt)
		tr.LastKey = key
		if opts.EvidenceDir != "" {
			p, _ := storeEvidence(opts.EvidenceDir, fmt.Sprintf("drive-iter-%03d-%s.png", iter, effective), img)
			tr.LastPath = p
			if tr.SuccessScreen != "" && tr.SuccessPath == "" {
				tr.SuccessPath = p // the first success frame's evidence PNG
			}
		}

		// The legend observation close (amendment 6): the story-subtitle
		// screen is wait-only (nothing is pressed — the legend advances on
		// its own, a press may skip a subtitle); after the success frame is
		// recorded the run keeps sampling to build the subtitle-timing
		// record and closes once one FULL subtitle is measured (a text run
		// closed by a text-less sample: the fade-in/hold/fade-out the
		// recognition pipeline needs) or the observation budget is spent.
		if screen == DriveScreenStorySubtitle {
			obsIters++
			done := obsIters >= storySubObserveIters
			if !done {
				for _, st := range subTimingsOf(tr.Transitions) {
					if !st.Open {
						done = true
						break
					}
				}
			}
			if done {
				return finishSuccess("story_subtitle")
			}
		}
	}
	if tr.SuccessScreen != "" {
		// The budget closed after the success frame was recorded (the
		// observation window ran out before a full subtitle closed): the
		// success condition is met, the timing record is partial with its
		// open runs marked Open.
		return finishSuccess(string(tr.SuccessScreen))
	}
	tr.Finished = time.Now()
	tr.Reason = fmt.Sprintf("budget exhausted: %d iterations in %s (last screen %s)",
		len(tr.Transitions), time.Since(tr.Started).Round(time.Second), tr.LastKey)
	tr.SubTimings = subTimingsOf(tr.Transitions)
	return tr, fmt.Errorf("dolphin: %s (full log above; last screenshot: %s)", tr.Reason, tr.LastPath)
}

// subTimingsOf builds the narration visibility record (amendment 6) from
// the iteration sequence: one SubTiming per maximal run of text-bearing
// iterations, bracketed by the surrounding text-less samples. The spans are
// sampled at the drive's iteration cadence (each boundary is a sample), so
// a 0 FadeIn means the run started at the first observed sample and an
// Open run is still fading when the observation window closed.
func subTimingsOf(transitions []DriveTransition) []SubTiming {
	var out []SubTiming
	i := 0
	for i < len(transitions) {
		if !transitions[i].Text {
			i++
			continue
		}
		start := i
		for i < len(transitions) && transitions[i].Text {
			i++
		}
		end := i - 1
		st := SubTiming{
			FirstIndex: transitions[start].Index,
			LastIndex:  transitions[end].Index,
			Hold:       transitions[end].Time.Sub(transitions[start].Time),
		}
		if start > 0 {
			st.FadeIn = transitions[start].Time.Sub(transitions[start-1].Time)
		}
		if end+1 < len(transitions) {
			st.FadeOut = transitions[end+1].Time.Sub(transitions[end].Time)
		} else {
			st.Open = true
		}
		out = append(out, st)
	}
	return out
}

// screenKey is the loop-detector and transition-log identity: the class plus
// the measured cursor side (the two prompt sides differ, so a cursor move is
// a screen change for the detector).
func screenKey(screen DriveScreen, side CursorSide) string {
	if screen == DriveScreenPrompt || screen == DriveScreenNameConfirm {
		return string(screen) + ":" + string(side)
	}
	return string(screen)
}

// DriveScreenOf maps the package's measured ScreenClass to the drive's
// screen families (amendment 5). The mapping is MEASURED, not assumed:
//
//   - the name-entry screen is its OWN class (ClassNameEnt: the keyboard
//     grid's measured dense ink, stable across the screen's frame pulse —
//     the class the old drive never entered for this screen, which is why
//     the old run sat on it for 166 s reading it as filesel/title);
//   - a Yes/No prompt (ClassPrompt or the dark ClassContinue family) with
//     the cursor measured on the LEFT option is the "yes" side (Yes is the
//     left option in the measured text order);
//   - the letterbox scenes (ClassScene, the opening letter, the near-black
//     transitions) are the cutscene family: A only when a text box is on
//     screen, otherwise wait.
func DriveScreenOf(sig ScreenSig) (DriveScreen, CursorSide) {
	c, side := sig.ClassOf()
	switch c {
	case ClassTitle:
		return DriveScreenTitle, CursorNone
	case ClassPrompt, ClassContinue:
		return DriveScreenPrompt, side
	case ClassDialog:
		return DriveScreenSaveNotice, CursorNone
	case ClassFileSel:
		return DriveScreenFileSelect, CursorNone
	case ClassNameEnt:
		return DriveScreenNameEntry, CursorNone
	case ClassStorySub:
		return DriveScreenStorySubtitle, CursorNone
	case ClassLetter, ClassScene, ClassDark:
		return DriveScreenCutscene, CursorNone
	case ClassBox:
		return DriveScreenDialogueBox, CursorNone
	default:
		return DriveScreenUnknown, CursorNone
	}
}

// cutsceneTextPresent reports whether a text box (a glyph-inked bright band
// in the lower third) is on screen — the clause that gates the cutscene's A
// (the opening letter's pages and any in-scene text advance on A; a bare
// boat scene waits). It is the box-band clause WITHOUT the entropy clause
// (which BoxPresent keeps to reject the letter scroll from the in-game box —
// the letter IS a screen whose text A must advance, so the rejection does
// not apply here).
func cutsceneTextPresent(img image.Image) bool {
	mean, edges := boxBandStats(img)
	return mean >= boxBandMeanMin && edges >= boxBandEdgesMin
}
