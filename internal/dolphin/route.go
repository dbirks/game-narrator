// The scripted play route (docs/adr/0162): the JSON input of the A/V
// harness's route mode — the savestate to boot from (relative to the
// fixture root, like the harness's own) plus the ordered steps the
// orchestrator iterates: stick holds, button presses, quiet waits and
// segment marks.
//
// A route is a Script and nothing else: the same Step/Script runner turns
// it into one input path (the runner's own Step fields — Stick/Hold for a
// stick hold, Press/Hold for a press, Wait for a quiet hold, Name for a
// segment mark), so the route adds no second driver, no second input
// protocol and no second input surface. The bridge still owns every
// controller byte (the pipe FIFO is the only path in/out of the emulated
// pad) and the runner still bounds every step (timeout + repeat cap).

package dolphin

import (
	"encoding/json"
	"fmt"
	"time"
)

// RouteStep is one step of a scripted play route (ADR-0162): the stick
// hold, the button press or the quiet wait, plus the segment mark the step
// belongs to. A step may carry both an input and a mark: the mark names the
// route segment the step falls in (the segment an utterance's audio start
// is attributed to), and the boundary is the step's start.
type RouteStep struct {
	// Stick is the main-stick position for the hold, [-1..1] per axis; nil =
	// no stick (a press or a wait).
	Stick []float64 `json:"stick"`
	// MS is the stick hold in milliseconds (the runner's step hold); nil =
	// the bridge's default hold.
	MS *int64 `json:"ms"`
	// Press is the button token ("A", "B", …) held for the default hold; nil
	// = no press (a stick hold or a wait).
	Press *string `json:"press"`
	// WaitMS is the step's quiet hold in milliseconds (the route's
	// wait_ms); nil = immediate.
	WaitMS *int64 `json:"wait_ms"`
	// Mark names the route segment this step begins; "" = no mark.
	Mark string `json:"mark"`
	// Approach runs the pixel-only "approach" step (ADR-0166): screenshot →
	// the largest olive-arrow cluster → hold toward it (300 ms lateral / 400 ms
	// forward) → stop after max_steps or when the arrow's area stops growing.
	// The vision-free half of the telescope's approach, promoted into the
	// play loop's route vocabulary so a route can walk to a character without a
	// hand-tuned stick vector or a vision-model ask.
	Approach bool `json:"approach"`
	// MaxSteps bounds the approach loop (the approach's own step count; the
	// runner's step deadline bounds it wall-clock on top).
	MaxSteps int `json:"max_steps"`
}

// Route is one scripted play route: the savestate to boot from (relative
// to the fixture root) and the ordered steps.
type Route struct {
	// SaveState is the savestate path relative to the fixture root (the
	// same relative shape the harness's own fixture states use: "states/…").
	SaveState string      `json:"save_state"`
	Steps     []RouteStep `json:"steps"`
}

// ParseRoute decodes one route JSON document (the play-loop's
// `GN_AV_ROUTE` input). Unknown fields are ignored; a step without input
// and without a mark is an error (a route must say what it does).
func ParseRoute(b []byte) (Route, error) {
	var r Route
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("dolphin: route: %w", err)
	}
	if r.SaveState == "" && len(r.Steps) == 0 {
		return r, fmt.Errorf("dolphin: route: empty document (want save_state + steps)")
	}
	for i := range r.Steps {
		s := &r.Steps[i]
		if s.Approach && s.MaxSteps < 1 {
			return r, fmt.Errorf("dolphin: route step %d: approach needs max_steps >= 1", i)
		}
		if s.Approach && (s.Stick != nil || s.Press != nil) {
			return r, fmt.Errorf("dolphin: route step %d: approach is a hold-only step (no stick, no press)", i)
		}
		switch {
		case s.Stick != nil && s.Press != nil:
			return r, fmt.Errorf("dolphin: route step %d: stick and press are mutually exclusive", i)
		case s.Stick == nil && s.Press == nil && s.WaitMS == nil && s.Mark == "" && !s.Approach:
			return r, fmt.Errorf("dolphin: route step %d: no input, no wait, no mark", i)
		}
	}
	return r, nil
}

// ToScript renders the route into one Script: the steps in order, each
// carrying its mark as the step's trace label (a marked step keeps its
// input; the label names the segment it begins). The returned script is
// the runner's — RunScript drives it through the bridge.
func (r Route) ToScript() (Script, error) {
	s := Script{}
	for i := range r.Steps {
		st, err := r.Steps[i].Step()
		if err != nil {
			return s, fmt.Errorf("dolphin: route step %d: %w", i, err)
		}
		if r.Steps[i].Mark != "" {
			st.Name = r.Steps[i].Mark
		}
		if st.Name == "" {
			st.Name = stepInputName(st)
		}
		s.Steps = append(s.Steps, st)
	}
	return s, nil
}

// Step renders one route step onto the runner's Step. The error is the
// route's own validation (unknown button token, a stick that is not two
// numbers).
func (s RouteStep) Step() (Step, error) {
	st := Step{}
	if s.Approach {
		st.Approach = true
		st.MaxSteps = s.MaxSteps
		return st, nil
	}
	switch {
	case s.Stick != nil:
		if len(s.Stick) != 2 {
			return st, fmt.Errorf("stick needs [x, y]")
		}
		st.Stick = &Stick{X: clampStickAxis(s.Stick[0]), Y: clampStickAxis(s.Stick[1])}
		if s.MS != nil && *s.MS > 0 {
			st.Hold = time.Duration(*s.MS) * time.Millisecond
		}
	case s.Press != nil:
		btn, err := buttonToken(*s.Press)
		if err != nil {
			return st, err
		}
		st.Press = []Button{btn}
	default:
		if s.WaitMS != nil && *s.WaitMS > 0 {
			st.Wait = time.Duration(*s.WaitMS) * time.Millisecond
		}
	}
	return st, nil
}

// stepInputName names a step for the trace when the route did not mark
// it: the wire description ("A", "STICK(x=0.5, y=-1)", "A+B").
func stepInputName(s Step) string {
	return wireDescription(s)
}

// clampStickAxis maps one stick axis into the pipe's -1..1 range.
func clampStickAxis(v float64) float64 {
	if v > 1 {
		return 1
	}
	if v < -1 {
		return -1
	}
	return v
}

// buttonToken maps one route press token onto the runner's Button token.
func buttonToken(tok string) (Button, error) {
	switch Button(tok) {
	case ButtonA:
		return ButtonA, nil
	case ButtonB:
		return ButtonB, nil
	case ButtonX:
		return ButtonX, nil
	case ButtonY:
		return ButtonY, nil
	case ButtonZ:
		return ButtonZ, nil
	case ButtonStart:
		return ButtonStart, nil
	case ButtonL:
		return ButtonL, nil
	case ButtonR:
		return ButtonR, nil
	case ButtonUp:
		return ButtonUp, nil
	case ButtonDown:
		return ButtonDown, nil
	case ButtonLeft:
		return ButtonLeft, nil
	case ButtonRight:
		return ButtonRight, nil
	default:
		return "", fmt.Errorf("unknown button token %q", tok)
	}
}
