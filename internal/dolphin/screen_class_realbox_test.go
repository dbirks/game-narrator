//go:build unix

package dolphin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The drive's dialogue_box SUCCESS gate (sig.RealBoxForm, ADR-0065): the
// classifier's measured DARK box form — never the bright structural clause,
// which also matches the pre-box animated scene and the name-entry screen
// (the 2026-10-19 test-full false success: the drive closed on a name-entry
// frame as the dialogue box at 25 s, the post-drive recheck then found the
// nameent screen — class=nameent lb=0.297 boxband[mean=118 edges=0.096] —
// for the whole 2-minute window). These tests unit the gate on the synthetic
// shapes (below) and, when the recorded fixtures are present, on the REAL
// retained frames (class (b) captured-frame replay, AGENTS.md rule 3).
// Test names deliberately avoid the "Real" pattern: they are unit tests
// (test-unit), not part of the test-dolphin gate's -run Real run.

// TestDarkBoxFormClauses: the synthetic shapes. The measured dark box
// (the drive's goal) is the real-box form; the bright structural double and
// the measured failure shape (the name-entry frame) are not.
func TestDarkBoxFormClauses(t *testing.T) {
	// The measured real box (the dark form): ClassBox and RealBoxForm.
	sig := ScreenSigOf(synthBoxDark(320, 240))
	if c, _ := sig.ClassOf(); c != ClassBox {
		t.Fatalf("synthetic dark box class=%s, want box: %s", c, sig.SigLine())
	}
	if !sig.RealBoxForm() {
		t.Fatalf("synthetic dark box RealBoxForm=false: the drive's measured goal must pass its own gate: %s", sig.SigLine())
	}

	// The bright structural form (the synthBox double, the BoxPresent
	// structural clauses): ClassBox but NOT the measured real-box form — the
	// drive's success gate must not terminate on it.
	sig = ScreenSigOf(synthBox(320, 240))
	if c, _ := sig.ClassOf(); c != ClassBox {
		t.Fatalf("bright box double regressed: class=%s, want box (the BoxPresent structural form)", c)
	}
	if sig.RealBoxForm() {
		t.Errorf("bright box double RealBoxForm=true (boxband mean %.0f >= the bright floor %.0f): the measured real box is the classifier's DARK form",
			sig.Regions["boxband"].mean, boxBandMeanMin)
	}

	// The measured failure shape (the 2026-10-19 false success): the
	// name-entry screen's bright-pulse frame — the nameent class on its
	// measured clauses (the grid ink, the 0.297 letterbox band) and a box
	// band that matches the BRIGHT structural clauses (measured mean 118 /
	// edges 0.096) — is never the drive's success, whatever class the pulse
	// frame lands on.
	sig = ScreenSigOf(synthNameEntBoxLike(320, 240))
	c, _ := sig.ClassOf()
	if c != ClassNameEnt && c != ClassBox {
		t.Fatalf("failure-shape frame class=%s, want nameent (its measured clauses) or the bright box family", c)
	}
	if sig.RealBoxForm() {
		t.Fatalf("failure-shape frame RealBoxForm=true (lb=%.3f bb[mean=%.1f edges=%.3f]): the name-entry screen was the false success — it must never measure the real box",
			sig.Letterbox, sig.Regions["boxband"].mean, sig.Regions["boxband"].edges)
	}
	if c == ClassNameEnt {
		// The classifier's own nameent signature is what the frame measures:
		// the grid ink clause still holds on this double.
		if ng := sig.Regions["namegrid"]; ng.edges < nameEntryGridInkMin || ng.mean < nameEntryGridMeanMin || ng.mean > nameEntryGridMeanMax {
			t.Errorf("failure-shape frame lost its nameent grid signature: ng[mean=%.1f edges=%.3f]", ng.mean, ng.edges)
		}
	}
}

// TestScreenClassRecordedFrames: the recorded-frame half — the classifier's
// real-box form on the REAL retained frames of the fixture factory (ADR-0054):
// no recorded name-entry frame may measure the real-box form (the false
// success's class), and every dialogue-box frame of the dedicated
// first-dialogue recording IS the measured real box. The frames are external
// (the fixture dir at test time, never committed — AGENTS.md rule 5) and
// NOT RUN (skipped, with the setup line) without them.
func TestScreenClassRecordedFrames(t *testing.T) {
	root := fixtureRootForTest(t)
	ids := []string{FixtureIDWindWaker, "windwaker-legend-dense", "windwaker-legend-press", "windwaker-first-dialogue"}
	present := map[string]bool{}
	for _, id := range ids {
		if _, err := os.Stat(filepath.Join(root, id, "manifest.json")); err == nil {
			present[id] = true
		}
	}
	if len(present) == 0 {
		t.Skipf("NOT RUN: no recorded fixtures under %s (recorded by make test-dolphin's fixture tests, ADR-0054) — the recorded-frame classifier check needs the retained name_entry/dialogue_box frames", root)
	}
	nameEntChecked := 0
	for _, id := range ids {
		if !present[id] {
			continue
		}
		dir := filepath.Join(root, id)
		manB, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			t.Fatalf("%s: read manifest: %v", id, err)
		}
		var man FixtureManifest
		if err := json.Unmarshal(manB, &man); err != nil {
			t.Fatalf("%s: parse manifest: %v", id, err)
		}
		for _, fr := range man.Frames {
			if fr.Screen != "name_entry" {
				continue
			}
			img, err := readPNGFile(filepath.Join(dir, "frames", fr.File))
			if err != nil {
				t.Fatalf("%s %s: %v", id, fr.File, err)
			}
			nameEntChecked++
			sig := ScreenSigOf(img)
			if sig.RealBoxForm() {
				t.Errorf("%s %s: the recorded NAME-ENTRY frame measures the real-box form (lb=%.3f bb[mean=%.1f edges=%.3f]) — the 2026-10-19 false-success class",
					id, fr.File, sig.Letterbox, sig.Regions["boxband"].mean, sig.Regions["boxband"].edges)
			}
		}
	}
	boxChecked := 0
	if present["windwaker-first-dialogue"] {
		dir := filepath.Join(root, "windwaker-first-dialogue")
		manB, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if err != nil {
			t.Fatalf("windwaker-first-dialogue: read manifest: %v", err)
		}
		var man FixtureManifest
		if err := json.Unmarshal(manB, &man); err != nil {
			t.Fatalf("windwaker-first-dialogue: parse manifest: %v", err)
		}
		for _, fr := range man.Frames {
			if fr.Screen != "dialogue_box" {
				continue
			}
			img, err := readPNGFile(filepath.Join(dir, "frames", fr.File))
			if err != nil {
				t.Fatalf("windwaker-first-dialogue %s: %v", fr.File, err)
			}
			boxChecked++
			sig := ScreenSigOf(img)
			if !sig.RealBoxForm() {
				t.Errorf("windwaker-first-dialogue %s: the recorded first-dialogue frame is NOT the measured real box (class=%s lb=%.3f bb[mean=%.1f edges=%.3f]) — the drive's success gate claims exactly this shape",
					fr.File, func() string { c, _ := sig.ClassOf(); return string(c) }(), sig.Letterbox, sig.Regions["boxband"].mean, sig.Regions["boxband"].edges)
			}
		}
	} else {
		t.Logf("windwaker-first-dialogue fixture absent: the positive recorded-frame check (every dialogue-box frame is the real box) is NOT RUN")
	}
	t.Logf("recorded frames: name_entry=%d (none may be the real-box form) first-dialogue dialogue_box=%d (all must be the real-box form)", nameEntChecked, boxChecked)
}
