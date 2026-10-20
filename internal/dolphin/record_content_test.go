package dolphin

// record_content_test.go pins the content-kind mark (ADR-0089, the G8
// unblock recipe): the mark's resolution rules (a frame outside a
// text-bearing box run is an error, never a guess), the profile's
// per-message text_kind (the content line carries the kind; every other
// line keeps dialogue — the legacy fixtures' readers see no change), the
// box region's on-screen role (the kind when marked, the textbox when not),
// and the manifest's Content block (absent on the unmarked recording).

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// twoLineBoxRuns builds a box-family lineRuns with one settled line over the
// frame runs: frames [2,4) run that line; frames 0-1 sit outside a
// text-bearing run (lineOf -1: a box run whose reading never settled).
func twoLineBoxRuns() lineRuns {
	return lineRuns{
		Canonical: []string{"line one"},
		lineOf:    []int{-1, -1, 0, 0},
		runKey:    []string{"", "", "one", "one"},
	}
}

func TestContentLineOf(t *testing.T) {
	box := twoLineBoxRuns()
	// A frame inside the box run resolves to that run's line.
	if line, err := contentLineOf(3, box); err != nil || line != 0 {
		t.Fatalf("contentLineOf(3) = %d, %v; want line 0 (the run's canonical line)", line, err)
	}
	// A frame whose run never settled on a reading (lineOf -1) is OUTSIDE a
	// text-bearing run: an error, never a guess.
	if line, err := contentLineOf(0, box); err == nil {
		t.Fatalf("contentLineOf(0) = %d, %v; want the out-of-run error (lineOf[0] = -1)", line, err)
	}
	// Outside the sequence.
	if _, err := contentLineOf(9, box); err == nil {
		t.Fatal("contentLineOf(9): want an out-of-range error")
	}
	// A box frame of a text-bearing run resolves.
	if line, err := contentLineOf(2, box); err != nil || line != 0 {
		t.Fatalf("contentLineOf(2) = %d, %v; want line 0", line, err)
	}
}

func TestContentLineOfTextBearingRun(t *testing.T) {
	box := lineRuns{
		Canonical: []string{"line zero", "line one"},
		lineOf:    []int{0, 0, 1, 1},
		runKey:    []string{"zero", "zero", "one", "one"},
	}
	line, err := contentLineOf(1, box)
	if err != nil || line != 0 {
		t.Fatalf("contentLineOf(1) = %d, %v; want line 0 (the first box run's line)", line, err)
	}
}

func TestContentLineOfSnapped(t *testing.T) {
	// A mark on the run's own frame resolves directly.
	box := lineRuns{
		Canonical: []string{"the line"},
		lineOf:    []int{-1, -1, -1, 0, 0, 0},
		runKey:    []string{"", "", "", "line", "line", "line"},
	}
	if line, err := contentLineOfSnapped(4, box); err != nil || line != 0 {
		t.Fatalf("contentLineOfSnapped(4) = %d, %v; want line 0 (the frame's own run)", line, err)
	}
	// A mark in the capture gap (the frame before the run's first recorded
	// frame) snaps forward to the run.
	if line, err := contentLineOfSnapped(2, box); err != nil || line != 0 {
		t.Fatalf("contentLineOfSnapped(2) = %d, %v; want line 0 (the snap to the run one frame ahead)", line, err)
	}
	// A mark far from any text-bearing box run fails (the snap is bounded:
	// a later line is not the content box the run flagged).
	far := lineRuns{
		Canonical: []string{"the line"},
		lineOf:    make([]int, 80),
		runKey:    make([]string, 80),
	}
	for i := range far.lineOf {
		far.lineOf[i] = -1
	}
	far.lineOf[50] = 0
	if _, err := contentLineOfSnapped(0, far); err == nil {
		t.Fatal("contentLineOfSnapped(0): want the bounded-snap error (the run 50 frames ahead is not the flagged box)")
	}
	if line, err := contentLineOfSnapped(49, far); err != nil || line != 0 {
		t.Fatalf("contentLineOfSnapped(49) = %d, %v; want line 0 (one frame before the run, within the bound)", line, err)
	}
	// Out of the sequence: no snap.
	if _, err := contentLineOfSnapped(99, far); err == nil {
		t.Fatal("contentLineOfSnapped(99): want the out-of-range error")
	}
}

func TestContentMarkValidation(t *testing.T) {
	root := t.TempDir()
	rec, err := NewFixtureRecorder(&recFakeBridge{}, root, "cm-fixture")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	// A frame mark before the kind is named is a dangling claim.
	if err := rec.MarkContentFrame(0); err == nil {
		t.Fatal("MarkContentFrame before SetContentKind: want an error")
	}
	// The kind is the ADR-0089 vocabulary (a typo is a load failure, not a
	// silent mislabel).
	if err := rec.SetContentKind("widget"); err == nil {
		t.Fatal("SetContentKind(widget): want an unknown-kind error")
	}
	if err := rec.SetContentKind("item"); err != nil {
		t.Fatalf("SetContentKind(item): %v", err)
	}
	if err := rec.MarkContentFrame(-1); err == nil {
		t.Fatal("MarkContentFrame(-1): want a not-an-index error")
	}
	if err := rec.MarkContentFrame(1); err != nil {
		t.Fatalf("MarkContentFrame(1): %v", err)
	}
}

func TestContentMarkFinalizeResolution(t *testing.T) {
	// A mark on a frame outside a text-bearing box run fails the finalize
	// (the mark would name a line the recording never showed) — with the
	// frames kept for inspection, not shipped.
	ctx := context.Background()
	root := t.TempDir()
	base := color.RGBA{R: 40, G: 40, B: 40, A: 255}
	fake := &recFakeBridge{imgs: []image.Image{flatFrame(base), flatFrame(base)}}
	rec, err := NewFixtureRecorder(fake, root, "cm-final")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := rec.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot %d: %v", i, err)
		}
	}
	if err := rec.SetContentKind("item"); err != nil {
		t.Fatalf("SetContentKind: %v", err)
	}
	if err := rec.MarkContentFrame(1); err != nil {
		t.Fatalf("MarkContentFrame: %v", err)
	}
	if _, err := rec.Finalize(ctx, DriveTrace{}); err == nil || !strings.Contains(err.Error(), "content mark") {
		t.Fatalf("Finalize with an unresolvable mark = %v; want the content-mark error", err)
	}
}

func TestWriteProfileContentKind(t *testing.T) {
	root := t.TempDir()
	rec := &FixtureRecorder{id: "cm-profile", root: root}
	sub := lineRuns{Canonical: []string{"sub line"}}
	box := lineRuns{
		Canonical: []string{"dialogue line", "obtained line"},
		lineOf:    []int{0, 1},
		runKey:    []string{"dlg", "itm"},
	}
	// The marked profile: the box line 1 (absolute index 2 — the sub family
	// has one line, so the box lines run past it) carries the content kind;
	// the dialogue line keeps dialogue; the box region is the item region.
	if _, err := rec.writeProfile(sub, box, 0, 2, FixtureSize{W: 640, H: 480}, "cm", 2, "item"); err != nil {
		t.Fatalf("writeProfile (marked): %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "profile.json"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	var p struct {
		TextKind string `json:"text_kind"`
		Messages []struct {
			ID       string `json:"id"`
			Original string `json:"original_text"`
			TextKind string `json:"text_kind"`
		} `json:"messages"`
		Regions []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("profile parse: %v", err)
	}
	if p.TextKind != "dialogue" {
		t.Fatalf("profile text_kind = %q, want dialogue (the profile-level default)", p.TextKind)
	}
	if len(p.Messages) != 3 {
		t.Fatalf("messages = %d, want 3", len(p.Messages))
	}
	// The sub line and the dialogue line keep dialogue; the content line
	// carries item.
	if p.Messages[0].TextKind != "dialogue" {
		t.Fatalf("message 0 (sub) text_kind = %q, want dialogue", p.Messages[0].TextKind)
	}
	if p.Messages[1].TextKind != "dialogue" {
		t.Fatalf("message 1 (box line 0) text_kind = %q, want dialogue", p.Messages[1].TextKind)
	}
	if p.Messages[2].TextKind != "item" {
		t.Fatalf("message 2 (box line 1, the content line) text_kind = %q, want item", p.Messages[2].TextKind)
	}
	// The box region is the item region (the content kind's on-screen role)
	// and it is the PRIMARY region (the harness reads Regions[0]; the bottom
	// bar rides along because the sub family has lines).
	if len(p.Regions) == 0 || p.Regions[0].ID != "box" || p.Regions[0].Type != "item" {
		t.Fatalf("regions[0] = %+v, want the box region typed item (primary)", p.Regions)
	}

	// The unmarked profile: every line dialogue, the box region the textbox.
	root2 := t.TempDir()
	rec2 := &FixtureRecorder{id: "cm-profile-plain", root: root2}
	if _, err := rec2.writeProfile(sub, box, 0, 2, FixtureSize{W: 640, H: 480}, "cm", -1, ""); err != nil {
		t.Fatalf("writeProfile (unmarked): %v", err)
	}
	raw2, err := os.ReadFile(filepath.Join(root2, "profile.json"))
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	var p2 struct {
		Messages []struct {
			TextKind string `json:"text_kind"`
		} `json:"messages"`
		Regions []struct {
			Type string `json:"type"`
		} `json:"regions"`
	}
	if err := json.Unmarshal(raw2, &p2); err != nil {
		t.Fatalf("profile parse: %v", err)
	}
	for i, m := range p2.Messages {
		if m.TextKind != "dialogue" {
			t.Fatalf("unmarked message %d text_kind = %q, want dialogue", i, m.TextKind)
		}
	}
	if p2.Regions[0].Type != "textbox" {
		t.Fatalf("unmarked box region type = %q, want textbox", p2.Regions[0].Type)
	}
}

func TestContentBlock(t *testing.T) {
	if b := contentBlock("", 3); b != nil {
		t.Fatalf("contentBlock(unmarked) = %+v, want nil (the legacy shape)", b)
	}
	if b := contentBlock("item", -1); b != nil {
		t.Fatalf("contentBlock(no frame) = %+v, want nil", b)
	}
	b := contentBlock("item", 7)
	if b == nil || b.Kind != "item" || b.Frame != 7 {
		t.Fatalf("contentBlock(item, 7) = %+v", b)
	}
	var m FixtureManifest
	m.Content = b
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("manifest marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"content":{"kind":"item","frame":7}`) {
		t.Fatalf("manifest JSON lacks the content block: %s", raw)
	}
	var back FixtureManifest
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("manifest unmarshal: %v", err)
	}
	if back.Content == nil || back.Content.Kind != "item" || back.Content.Frame != 7 {
		t.Fatalf("manifest round trip = %+v", back.Content)
	}
}

// The FramesPublished index contract the mark relies on (the drive's own
// Screenshot publishes inline): after one Screenshot, the frame is at
// FramesPublished()-1.
func TestFramesPublishedIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	base := color.RGBA{R: 40, G: 40, B: 40, A: 255}
	fake := &recFakeBridge{imgs: []image.Image{flatFrame(base), flatFrame(base), flatFrame(base)}}
	rec, err := NewFixtureRecorder(fake, root, "cm-idx")
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if got := rec.FramesPublished(); got != 0 {
		t.Fatalf("FramesPublished before any frame = %d, want 0", got)
	}
	for i := 0; i < 3; i++ {
		if _, err := rec.Screenshot(ctx); err != nil {
			t.Fatalf("Screenshot %d: %v", i, err)
		}
		if got := rec.FramesPublished(); got != i+1 {
			t.Fatalf("FramesPublished after frame %d = %d, want %d (the drive's frame is at index FramesPublished()-1)", i, got, i+1)
		}
	}
	// A full finalize still self-verifies with the mark absent (the unit
	// frames classify off-box, so no mark is set here).
	if _, err := rec.Finalize(ctx, DriveTrace{OK: true, Reason: fmt.Sprintf("unit: %d frames", 3)}); err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	_ = time.Now() // keep the time import honest across edits
}
