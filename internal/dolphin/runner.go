package dolphin

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// The named controller inputs a Script can press (the pipe.go wire tokens).
const (
	ButtonA     Button = "A"
	ButtonB     Button = "B"
	ButtonX     Button = "X"
	ButtonY     Button = "Y"
	ButtonZ     Button = "Z"
	ButtonStart Button = "START"
	ButtonL     Button = "L"
	ButtonR     Button = "R"
	ButtonUp    Button = "D_UP"
	ButtonDown  Button = "D_DOWN"
	ButtonLeft  Button = "D_LEFT"
	ButtonRight Button = "D_RIGHT"
)

// Runner defaults. A step with Timeout zero gets defaultStepTimeout; a
// Script with Poll zero polls checkpoints every defaultPoll. The poll
// cadence is wall clock: the emulated game moves at ~0.8-1.0x under
// llvmpipe (spike), so a 2 s cadence never out-runs the screens the
// checkpoints look for (a fade takes ~1-2 s of emulation, not of polling).
const (
	defaultStepTimeout = 90 * time.Second
	defaultPoll        = 2 * time.Second
)

// arrowStallSamples is how many consecutive approach samples the arrow
// cluster's area may stop growing before the approach step ends (the "arrived"
// stop of the ADR-0166 approach step: the arrow is a fixed world-space object
// whose screen area grows as the camera closes in, so a stall of this many
// samples means the hold has converged — the step stops instead of burning its
// max_steps budget on a walk that is no longer moving).
const arrowStallSamples = 3

// buttonsFromTokens maps Script step buttons onto the bridge's Buttons.
func buttonsFromTokens(ps []Button) Buttons {
	var b Buttons
	for _, p := range ps {
		switch p {
		case ButtonA:
			b.A = true
		case ButtonB:
			b.B = true
		case ButtonX:
			b.X = true
		case ButtonY:
			b.Y = true
		case ButtonZ:
			b.Z = true
		case ButtonStart:
			b.Start = true
		case ButtonL:
			b.L = true
		case ButtonR:
			b.R = true
		case ButtonUp:
			b.Up = true
		case ButtonDown:
			b.Down = true
		case ButtonLeft:
			b.Left = true
		case ButtonRight:
			b.Right = true
		}
	}
	return b
}

// wireDescription renders the input a step attempt injects (the trace's
// Wire field) — the button tokens for press steps, the stick position for
// stick steps.
func wireDescription(s Step) string {
	if s.Stick != nil {
		return fmt.Sprintf("STICK(x=%.1f y=%.1f)", s.Stick.X, s.Stick.Y)
	}
	var parts []string
	for _, b := range s.Press {
		parts = append(parts, string(b))
	}
	return strings.Join(parts, "+")
}

// stepHold resolves the per-attempt hold (zero = the bridge default).
func stepHold(s Step) time.Duration {
	if s.Hold > 0 {
		return s.Hold
	}
	return defaultHold
}

// sleepCtx waits d or until ctx is cancelled (whichever first).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// evidenceName is a safe file name for a step's labeled PNG.
func evidenceName(stepIdx int, stepName, kind string) string {
	sanitized := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		}
		return '-'
	}, stepName)
	return fmt.Sprintf("%02d-%s-%s.png", stepIdx, sanitized, kind)
}

// storeEvidence writes one labeled PNG into the script's evidence dir
// (best effort: a write failure is noted in the trace detail, never fatal —
// the hash is the record, the PNG is the human-readable aid).
func storeEvidence(dir, name string, img image.Image) (string, string) {
	if dir == "" || img == nil {
		return "", ""
	}
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", "evidence write failed: " + err.Error()
	}
	err = png.Encode(f, img)
	_ = f.Close()
	if err != nil {
		return "", "evidence encode failed: " + err.Error()
	}
	return path, ""
}

// RunScript drives the bridge through s's steps in order and returns the
// full trace. Every step is bounded (timeout + repeat cap): a run either
// finishes (OK) or returns the trace through the failed step, whose
// FinalPath names the last screenshot taken (the failure handoff the spec's
// evidence requirement asks for). ctx cancellation stops the run and is
// reported the same way.
func RunScript(ctx context.Context, b Bridge, s Script) (Trace, error) {
	if s.Poll <= 0 {
		s.Poll = defaultPoll
	}
	tr := Trace{OK: true, Started: time.Now(), Steps: make([]StepTrace, 0, len(s.Steps))}
	for idx, step := range s.Steps {
		st := runStep(ctx, b, s, idx, step)
		tr.Steps = append(tr.Steps, st)
		if !st.OK {
			tr.OK = false
			tr.LastError = st.errorText()
			break
		}
	}
	tr.Finished = time.Now()
	if tr.OK {
		return tr, nil
	}
	last := tr.Steps[len(tr.Steps)-1]
	return tr, fmt.Errorf("dolphin: script failed at step %q: %s (last screenshot: %s)",
		last.Name, tr.LastError, last.FinalPath)
}

// runStep executes one step and fills its trace record.
func runStep(ctx context.Context, b Bridge, s Script, idx int, step Step) StepTrace {
	st := StepTrace{Name: step.Name, Start: time.Now()}

	// The step-start screenshot: the checkpoint's ref (before any input this
	// step sends) and the evidence "ref" PNG.
	ref, err := b.Screenshot(ctx)
	if err != nil {
		st.End = time.Now()
		return st.withError("step-start screenshot: " + err.Error())
	}
	st.RefHash = capture.HashFrame(ref)
	st.RefSig = ScreenSigOf(ref).Key()
	path, note := storeEvidence(s.EvidenceDir, evidenceName(idx, step.Name, "ref"), ref)
	st.RefPath = path
	if note != "" {
		st.Details = append(st.Details, note)
	}

	// The loop guard: armed when this step starts on a Yes/No prompt (the
	// create-save parchment or the continue-anyway dialog). After the step's
	// input, if the screen stays on the same prompt (same key: class +
	// cursor side) for loopConfirmSamples consecutive polls, the input had
	// no effect — the press selected the option that returns to this prompt
	// — and the step fails fast (it must never pass on a looped prompt).
	guard := newLoopGuard(st.RefSig)

	timeout := step.Timeout
	if timeout <= 0 {
		timeout = defaultStepTimeout
	}
	deadline := time.Now().Add(timeout)

	// An approach step (ADR-0166): the pixel-only walk toward the interaction
	// arrow — screenshot → the LARGEST qualifying olive-arrow cluster → hold the
	// stick toward it → stop after MaxSteps or when the arrow's area stops
	// growing. It owns its loop (the stop conditions are the step count and the
	// area stall, not a screen predicate), so it never consults the Until
	// checkpoint; a hold/press/wait step cannot re-aim its stick from a fresh
	// frame per iteration anyway.
	if step.Approach {
		ok, detail := runApproachStep(ctx, b, step, &st, deadline)
		st.OK = ok
		st.End = time.Now()
		st.Details = append(st.Details, detail)
		if !ok {
			st.lastErr = detail
		}
		finaliseStep(b, ctx, s, idx, step, &st)
		return st
	}

	// A step with no checkpoint succeeds after (at most) one attempt.
	if step.Until == nil {
		st.Attempts, st.Wire = 0, ""
		wire, herr := attemptInput(ctx, b, step)
		if wire != "" {
			st.Wire = wire
		}
		st.Attempts = 1
		if step.Wait > 0 {
			// The step's quiet hold (the scripted-route wait, ADR-0162): the
			// time the step holds before its final state is recorded — for a
			// wait-only step the hold is the whole step, for a hold-bearing
			// step it is the quiet time after the input's own hold.
			if err := sleepCtx(ctx, step.Wait); err != nil {
				st.End = time.Now()
				return st.withError(err.Error())
			}
		}
		st.End = time.Now()
		if herr != nil {
			return st.withError("inject: " + herr.Error())
		}
		// No checkpoint: the attempt (and its quiet hold) is the whole step,
		// so it succeeded. Without this the trace kept OK false with no error
		// text and RunScript failed every checkpoint-free step ("script
		// failed at step …: " with an empty reason) — invisible until the
		// play routes (ADR-0162) made plain stick/press/wait steps common.
		st.OK = true
		finaliseStep(b, ctx, s, idx, step, &st)
		return st
	}

	// Screen-specific skip: a step with SkipUnless runs ONLY when the
	// step-start screen is the screen the step handles (the "cannot be
	// saved — continue anyway?" handler must never fire on the
	// file-select). Skipped = ok with zero input — the run takes the other
	// path (a working card), so skipping is the success branch, not a
	// failure.
	if step.SkipUnless != nil {
		if ok, detail := (*step.SkipUnless)(ctx, ref, ref); !ok {
			st.Details = append(st.Details, "skipped: not on the step's screen — "+detail)
			st.OK = true
			st.Skipped = true
			st.End = time.Now()
			st.FinalHash, st.FinalPath, st.FinalSig = st.RefHash, st.RefPath, st.RefSig
			return st
		}
	}

	// Already satisfied at step start: no input is sent (the run may have
	// reached this screen via the previous step's overshoot).
	if ok, detail := (*step.Until)(ctx, ref, ref); ok {
		st.Details = append(st.Details, "pre-satisfied: "+detail)
		st.OK = true
		st.End = time.Now()
		finaliseStep(b, ctx, s, idx, step, &st)
		return st
	}

	// Quiet time before the first attempt (transitions need settling).
	if step.After > 0 {
		if err := sleepCtx(ctx, step.After); err != nil {
			st.End = time.Now()
			st.FinalHash, st.FinalPath = st.RefHash, st.RefPath
			st.FinalSig = st.RefSig
			return st.withError(err.Error())
		}
	}

	// Per-attempt observation window: after each input attempt the checkpoint
	// is polled for this long before the runner re-injects (the repeat cap's
	// cadence). Four polls at the script's cadence, floored at attemptWindowMin
	// — long enough for a fade + screen settle (measured: the post-START
	// screen settles ~2-4 s after the press) so a slow transition is not
	// misread as "the input did nothing".
	attemptWindow := 4 * s.Poll
	if attemptWindow < attemptWindowMin {
		attemptWindow = attemptWindowMin
	}

	maxAttempts := 1 + step.MaxRepeats
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if time.Now().After(deadline) {
			break
		}
		st.Attempts = attempt
		wire, herr := attemptInput(ctx, b, step)
		if wire != "" {
			if st.Wire == "" {
				st.Wire = wire
			} else {
				st.Wire += ";" + wire
			}
		}
		if herr != nil {
			st.End = time.Now()
			finaliseStep(b, ctx, s, idx, step, &st)
			return st.withError("inject: " + herr.Error())
		}
		held, perr := pollWindow(ctx, b, s.Poll, *step.Until, ref, deadline,
			time.Now().Add(attemptWindow), &st, guard)
		if perr != nil {
			st.End = time.Now()
			finaliseStep(b, ctx, s, idx, step, &st)
			return st.withError(perr.Error())
		}
		if held {
			st.OK = true
			st.End = time.Now()
			finaliseStep(b, ctx, s, idx, step, &st)
			return st
		}
		if errors.Is(perr, errLoopDetected) {
			st.End = time.Now()
			finaliseStep(b, ctx, s, idx, step, &st)
			return st.withError(fmt.Sprintf("loop detected: %s", perr.Error()))
		}
	}
	// The attempt budget is spent: wait out the rest of the step's deadline
	// (the input may simply be slow) before failing the run here.
	if held, perr := pollWindow(ctx, b, s.Poll, *step.Until, ref, deadline,
		time.Now().Add(maxInt64Duration), &st, guard); held {
		st.OK = true
		st.End = time.Now()
		finaliseStep(b, ctx, s, idx, step, &st)
		return st
	} else if perr != nil {
		st.End = time.Now()
		finaliseStep(b, ctx, s, idx, step, &st)
		if errors.Is(perr, errLoopDetected) {
			return st.withError(fmt.Sprintf("loop detected: %s", perr.Error()))
		}
		return st.withError(perr.Error())
	}
	st.End = time.Now()
	finaliseStep(b, ctx, s, idx, step, &st)
	if st.OK {
		return st
	}
	last := ""
	if len(st.Details) > 0 {
		last = st.Details[len(st.Details)-1]
	}
	return st.withError(fmt.Sprintf("checkpoint not met within %s (%d attempt(s)); last: %s",
		timeout, st.Attempts, last))
}

// runApproachStep runs one approach step (ADR-0166): the bounded pixel-only
// loop — screenshot → the LARGEST qualifying olive-arrow cluster → hold the
// stick toward it (300 ms lateral / 400 ms forward) → stop after MaxSteps, or
// when the arrow's cluster area stops growing for three consecutive samples.
// The arrow's own pixels are the whole signal (the detector moved to
// internal/dolphin/arrow.go for exactly this — no vision model), so the step
// is deterministic and synthetic-frame-testable. Returns (ok, detail): ok is
// the stop verdict, detail the trace line (the stall reason, the max_steps
// reason, or the failure text).
func runApproachStep(ctx context.Context, b Bridge, step Step, st *StepTrace, deadline time.Time) (bool, string) {
	maxSteps := step.MaxSteps
	if maxSteps < 1 {
		return false, "approach: max_steps < 1"
	}
	const (
		holdForward = 400 * time.Millisecond // the centred hold: walk straight at the arrow
		holdLateral = 300 * time.Millisecond // the off-centre hold: steer toward the arrow
	)
	prevArea := -1 // the previous sample's largest-cluster area; -1 = the first sample grows
	stalled := 0
	for i := 0; i < maxSteps; i++ {
		if time.Now().After(deadline) {
			return false, fmt.Sprintf("approach: deadline exceeded after %d step(s)", i)
		}
		img, err := b.Screenshot(ctx)
		if err != nil {
			return false, "approach screenshot: " + err.Error()
		}
		clusters := oliveArrowClusters(img)
		if len(clusters) == 0 {
			// The arrow never qualified (off-frame / no olive fill): there is
			// nothing to aim at, and the step ends here rather than burning the
			// budget on a walk that cannot find a target.
			detail := "approach: no qualifying olive cluster (the arrow is not on screen) — ended"
			st.Details = append(st.Details, detail)
			return true, detail
		}
		ar := clusters[0] // the list is densest-first: the largest cluster is the arrow
		if ar.count > prevArea {
			stalled = 0
		} else {
			stalled++
			if stalled >= arrowStallSamples {
				detail := fmt.Sprintf("approach: the arrow's area stopped growing at %d px — arrived", ar.count)
				st.Details = append(st.Details, detail)
				return true, detail
			}
		}
		prevArea = ar.count
		// Aim: off-centre by more than the 0.08 alignment floor → hold the stick
		// that way (lateral); centred → hold forward.
		// Stick Y +1 is forward (away from the camera), measured on the
		// play-loop routes (ADR-0162): a lateral correction is a forward
		// diagonal, so Link closes in while turning toward the arrow.
		sx, sy := 0.0, 1.0
		hold := holdForward
		if ar.ok && math.Abs(ar.cx-0.5) > 0.08 {
			sx = -1.0
			if ar.cx > 0.5 {
				sx = 1.0
			}
			sy = 0.5
			hold = holdLateral
		}
		step.Stick = &Stick{X: sx, Y: sy}
		step.Hold = hold
		wire, ierr := attemptInput(ctx, b, step)
		if ierr != nil {
			return false, "approach inject: " + ierr.Error()
		}
		if st.Wire == "" {
			st.Wire = wire // the trace records the hold the approach sent
		}
	}
	return true, fmt.Sprintf("approach: max_steps %d reached", maxSteps)
}

// maxInt64Duration is the time.Duration ceiling; the "no window end" sentinel.
const maxInt64Duration = time.Duration(1<<63 - 1)

// errLoopDetected is the typed fail-fast the loop guard returns: the step's
// input left the screen on the SAME prompt (the Yes/No loop the spec's
// "avoiding false positives" forbids). The step never passes on it.
var errLoopDetected = errors.New("loop detected")

// loopConfirmSamples is how many CONSECUTIVE same-screen samples (at the
// script's poll cadence) the guard needs before it calls a loop. Three at
// the 2 s cadence is 6 s of an unchanged prompt — long past any transition
// (measured 1-2 s), short enough to fail fast rather than burn the deadline.
const loopConfirmSamples = 3

// loopGuard watches the stream of screen keys after a step's input and fires
// when the screen stays on the step-start screen (by signature key) for
// loopConfirmSamples consecutive samples. It is armed only for steps that
// START on a prompt (the Yes/No loop case): a stable-class screen that
// legitimately changes content while keeping its class (the opening letter's
// page turns) must not read as a loop.
type loopGuard struct {
	refKey string // the step-start screen key (a prompt)
	same   int    // consecutive samples matching refKey
	fired  bool
	key    string // the key that looped
}

// newLoopGuard arms the guard for a step-start key (nil when the step does
// not start on a screen that loops in place). It arms for BOTH prompt
// classes (the bright create-save parchment and the dark continue-anyway
// dialog, the two screens the measured loop bounces between) and for the
// quest-log file-selection list (a stable menu whose key carries no side, so
// a press that leaves it unchanged is a loop too — measured: A alone does
// nothing on it and the key cannot drift). A stable-class screen that
// legitimately changes content while keeping its class (the opening letter's
// page turns) must not read as a loop, which is why the other classes stay
// un-armed.
func newLoopGuard(refKey string) *loopGuard {
	if !strings.HasPrefix(refKey, string(ClassPrompt)) && !strings.HasPrefix(refKey, string(ClassContinue)) &&
		!strings.HasPrefix(refKey, string(ClassFileSel)) {
		return nil
	}
	return &loopGuard{refKey: refKey}
}

// observe feeds one polled sample's key. attempts is the step's attempt
// count: with no input sent yet there is nothing to loop, so the count only
// advances once an attempt has happened.
func (g *loopGuard) observe(key string, attempts int) {
	if g == nil || g.fired || attempts < 1 {
		return
	}
	if key == g.refKey {
		g.same++
	} else {
		g.same = 0
	}
	if g.same >= loopConfirmSamples {
		g.fired = true
		g.key = key
	}
}

// attemptWindowMin floors each input attempt's observation window. It is a
// variable only so the unit tests can shrink the window to milliseconds; the
// real run keeps the 6 s floor so a slow fade is not misread as a missed
// input (the post-START screen settles 2-4 s after the press).
var attemptWindowMin = 6 * time.Second

// pollWindow evaluates the checkpoint on fresh screenshots at the Poll
// cadence until it holds, the step deadline or the window end passes, a
// probe fails, or the loop guard fires. Every evaluation's detail is appended
// to st (the per-sample trace a failed step needs) and (held, probeErr) is
// returned; probeErr wraps errLoopDetected when the guard fired.
func pollWindow(ctx context.Context, b Bridge, poll time.Duration, cp Checkpoint, ref image.Image, deadline, windowEnd time.Time, st *StepTrace, guard *loopGuard) (bool, error) {
	for {
		if time.Now().After(deadline) || time.Now().After(windowEnd) {
			return false, nil
		}
		shot, serr := b.Screenshot(ctx)
		if serr != nil {
			return false, fmt.Errorf("checkpoint screenshot: %w", serr)
		}
		guard.observe(ScreenSigOf(shot).Key(), st.Attempts)
		if guard != nil && guard.fired {
			return false, fmt.Errorf("%w: the screen stayed on %q for %d consecutive samples after %q (the press left the same prompt — it did not advance)",
				errLoopDetected, guard.key, loopConfirmSamples, st.Wire)
		}
		ok, detail := cp(ctx, shot, ref)
		st.Details = append(st.Details, fmt.Sprintf("a%d: %s", st.Attempts, detail))
		if ok {
			return true, nil
		}
		if err := sleepCtx(ctx, poll); err != nil {
			return false, err
		}
	}
}

// attemptInput injects one attempt of the step's input and returns its wire
// description (""). A wait-only step (no Press, no Stick) injects nothing.
func attemptInput(ctx context.Context, b Bridge, step Step) (string, error) {
	if step.Stick != nil {
		err := b.Inject(ctx, Buttons{}, Axes{MainX: step.Stick.X, MainY: step.Stick.Y}, stepHold(step))
		return wireDescription(step), err
	}
	if len(step.Press) == 0 {
		// A wait-only step with a checkpoint: its "attempt" is just the poll
		// window (no input to send), so there is no hold to sleep out.
		return "", nil
	}
	err := b.Inject(ctx, buttonsFromTokens(step.Press), Axes{}, stepHold(step))
	return wireDescription(step), err
}

// finaliseStep records the step's final screenshot (hash, signature key and
// evidence PNG). ctx is advisory: the grab is a fast bounded lookup. An error
// path that already handed back the step-start shot (FinalHash set) is left
// alone.
func finaliseStep(b Bridge, ctx context.Context, s Script, idx int, step Step, st *StepTrace) {
	if st.FinalHash != "" {
		return
	}
	shot, err := b.Screenshot(ctx)
	if err != nil {
		// No fresh shot: hand back the step-start one (it is the last
		// picture the run actually has).
		st.FinalHash, st.FinalPath = st.RefHash, st.RefPath
		st.FinalSig = st.RefSig
		return
	}
	st.FinalHash = capture.HashFrame(shot)
	st.FinalSig = ScreenSigOf(shot).Key()
	path, note := storeEvidence(s.EvidenceDir, evidenceName(idx, step.Name, "final"), shot)
	st.FinalPath = path
	if note != "" {
		st.Details = append(st.Details, note)
	}
}

// withError stamps the failure detail on the record (the trace keeps it for
// the report).
func (st StepTrace) withError(msg string) StepTrace {
	st.OK = false
	st.lastErr = msg
	return st
}

func (st StepTrace) errorText() string { return st.lastErr }
