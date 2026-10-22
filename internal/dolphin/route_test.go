package dolphin

import (
	"context"
	"image"
	"testing"
	"time"
)

// TestParseRouteAndToScript pins the ADR-0162 route → Script mapping: the
// route's steps become the runner's own steps (stick hold / press / wait /
// mark) and nothing else, and the savestate rides along as data the caller
// resolves against the fixture root.
func TestParseRouteAndToScript(t *testing.T) {
	routeJSON := []byte(`{
		"save_state": "states/windwaker-deck-aryll.state",
		"steps": [
			{"wait_ms": 5000, "mark": "idle"},
			{"stick": [0, -1], "ms": 20000, "mark": "walk-deck"},
			{"press": "A", "mark": "talk-aryll"},
			{"wait_ms": 5000}
		]
	}`)
	r, err := ParseRoute(routeJSON)
	if err != nil {
		t.Fatalf("ParseRoute: %v", err)
	}
	if r.SaveState != "states/windwaker-deck-aryll.state" {
		t.Fatalf("route save_state = %q", r.SaveState)
	}
	if len(r.Steps) != 4 {
		t.Fatalf("route steps = %d; want 4", len(r.Steps))
	}
	s, err := r.ToScript()
	if err != nil {
		t.Fatalf("ToScript: %v", err)
	}
	if len(s.Steps) != 4 {
		t.Fatalf("script steps = %d; want 4", len(s.Steps))
	}
	// The idle mark step: a quiet wait, the mark as the trace label.
	if got := s.Steps[0]; got.Name != "idle" || got.Wait != 5000*time.Millisecond || got.Stick != nil || got.Press != nil {
		t.Fatalf("script step 0 = %+v; want mark idle / wait 5 s", got)
	}
	// The walk-deck stick hold: the mark is the trace label, the stick + hold ride the step.
	if got := s.Steps[1]; got.Name != "walk-deck" || got.Stick == nil || got.Stick.X != 0 || got.Stick.Y != -1 || got.Hold != 20*time.Second {
		t.Fatalf("script step 1 = %+v; want mark walk-deck / stick (0,-1) / hold 20 s", got)
	}
	// The talk-aryll press: the mark is the trace label, the press rides.
	if got := s.Steps[2]; got.Name != "talk-aryll" || got.Press == nil || len(got.Press) != 1 || got.Press[0] != ButtonA || got.Stick != nil {
		t.Fatalf("script step 2 = %+v; want mark talk-aryll / press A", got)
	}
	// The trailing wait: no mark, the wait rides.
	if got := s.Steps[3]; got.Name != "" || got.Wait != 5000*time.Millisecond || got.Press != nil || got.Stick != nil {
		t.Fatalf("script step 3 = %+v; want the unnamed wait of 5 s", got)
	}

	// Invalid routes are rejected, never guessed.
	if _, err := ParseRoute([]byte(`{"steps":[{"stick":[0,1],"press":"A"}]}`)); err == nil {
		t.Fatalf("ParseRoute: stick+press must be rejected")
	}
	if _, err := ParseRoute([]byte(`{"steps":[{"nope":1}]}`)); err == nil {
		t.Fatalf("ParseRoute: an input-less, mark-less step must be rejected")
	}
	if _, err := ParseRoute([]byte(`not json`)); err == nil {
		t.Fatalf("ParseRoute: malformed JSON must be rejected")
	}
}

// TestRunScriptCheckpointFreeStepsSucceed: a route's plain stick hold,
// press and wait (no checkpoint) each succeed after their one attempt. The
// runner used to leave OK false on this path, so the first route step failed
// with an empty reason.
func TestRunScriptCheckpointFreeStepsSucceed(t *testing.T) {
	r, err := ParseRoute([]byte(`{"save_state":"states/x.state","steps":[
		{"mark":"idle","wait_ms":5},
		{"mark":"walk","stick":[0,-1],"ms":5},
		{"press":"A"}]}`))
	if err != nil {
		t.Fatalf("ParseRoute: %v", err)
	}
	s, err := r.ToScript()
	if err != nil {
		t.Fatalf("ToScript: %v", err)
	}
	b := &fakeBridge{shots: []image.Image{image.NewRGBA(image.Rect(0, 0, 8, 8))}}
	tr, err := RunScript(context.Background(), b, s)
	if err != nil {
		t.Fatalf("RunScript: %v", err)
	}
	if len(tr.Steps) != 3 || !tr.OK {
		t.Fatalf("trace = %d steps, ok %v; want 3 ok steps", len(tr.Steps), tr.OK)
	}
	if len(b.injects) != 2 {
		t.Fatalf("injects = %d, want 2 (the stick hold and the press; the wait sends nothing)", len(b.injects))
	}
}
