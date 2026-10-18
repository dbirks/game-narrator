// The script runner types (docs/spec/11 step 3: "apply DTM/controller
// sequence and checkpoint screenshots"). A Script is a sequence of bounded
// steps: each step injects its input (buttons or stick), then polls its
// Checkpoint against fresh screenshots until the checkpoint holds, the step
// timeout passes, or the repeat cap is hit. The runner records a per-step
// trace (timings, wire bytes, checkpoint details, screenshot hashes and
// paths) — the event evidence the failure handoff needs.
package dolphin

import (
	"context"
	"image"
	"time"
)

// Button is a controller button token (the pipe wire tokens; the runner
// maps them onto Buttons in the order the wire bytes need).
type Button string

// Stick is an absolute main-stick position (-1..1 each axis; the runner
// maps it onto the pipe protocol's 0..1 SET MAIN, 0.5 = centred).
type Stick struct{ X, Y float64 }

// Checkpoint is a predicate over one fresh screenshot (cur) and the
// step-start reference (ref). It returns whether the step's target screen
// is reached and a one-line measured detail (the trace shows the numbers,
// not just the verdict). A checkpoint is bound to one RunScript run:
// stateful variants (Settled, StableFor, FullFrameScene) keep per-run
// sample state in closures, and the script builders allocate one instance
// per step.
type Checkpoint func(ctx context.Context, cur, ref image.Image) (bool, string)

// Step is one bounded action. Press and Stick are mutually exclusive.
type Step struct {
	Name string // trace label

	// Press: the buttons injected per attempt (held for Hold).
	Press []Button
	// Hold: how long a press is held (zero = defaultHold, the bridge's
	// spike-measured 500 ms).
	Hold time.Duration
	// Stick: the stick position set for the hold (the stick is released to
	// neutral by the bridge after the hold).
	Stick *Stick
	// After: quiet time before the first attempt (lets transitions settle).
	After time.Duration
	// Until: the checkpoint polled after each attempt (nil = the step
	// sends its input once and succeeds).
	Until *Checkpoint
	// SkipUnless: the screen-specific precondition — the step runs ONLY
	// when this checkpoint holds on the step-start screenshot; otherwise
	// the step is skipped (ok, zero input). Screen-specific handlers (the
	// "cannot be saved — continue anyway?" step) use this so they never
	// fire on the wrong screen (ADR-0043 amendment 2).
	SkipUnless *Checkpoint
	// Timeout: the step's total wall-clock budget (zero = defaultStepTimeout).
	Timeout time.Duration
	// MaxRepeats: extra input attempts after the first (the repeat cap).
	MaxRepeats int
}

// Script is a run: steps in order, one poll cadence, an optional evidence
// dir for the per-step PNGs (empty = hashes only).
type Script struct {
	Steps       []Step
	Poll        time.Duration // checkpoint poll cadence (zero = defaultPoll)
	EvidenceDir string        // "" = no PNGs stored
}

// StepTrace is the measured record of one step.
type StepTrace struct {
	Name       string
	Start, End time.Time
	Attempts   int
	Wire       string   // the button bytes sent, in order ("" = none)
	Details    []string // one per Until evaluation (the checkpoint details)
	RefHash    string   // the step-start screenshot hash
	RefPath    string   // its PNG ("" when no evidence dir)
	RefSig     string   // the step-start screen signature key (the loop-guard ref)
	FinalHash  string   // the last screenshot's hash
	FinalPath  string   // the last screenshot's path (the failure handoff)
	FinalSig   string   // the last screenshot's screen signature key
	OK         bool
	Skipped    bool // the SkipUnless precondition did not hold (zero input)

	lastErr string // the step's failure detail ("" when OK); unexported
	// because the run-level trace carries it in LastError.
}

// Trace is the whole run's record: OK overall, the steps in order, and the
// failed step's detail when not OK.
type Trace struct {
	OK        bool
	Started   time.Time
	Finished  time.Time
	Steps     []StepTrace
	LastError string // the failed step's detail ("" when OK)
}
