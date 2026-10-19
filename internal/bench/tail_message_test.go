package bench

// TestTop1GradingWindowTrailingMessage reproduces, on class (a) synthetic
// frames with invented text (no game material), the top-1 grading-window
// failure measured on the captured holdout's sample cx0014 (ADR-0058
// amendment 2) and pins the amendment-3 grading that fixes it: a sample span
// whose trailing frames show a DIFFERENT in-script message (there: the game's
// continuation prompt rendered in the subtitle bar). The pipeline and the
// matcher behave correctly on both lines — the shown line is committed, and
// the trailing line is ranked top-1 when it appears. The top-1 objective is
// graded on the truth line's DISPLAY WINDOW (the oracle's frames where the
// line is on screen), where the truth is the top-1; the legacy
// last-ranking-row-of-the-span reading still records the trailing message as
// a miss, kept visible as the informational top1_last_row figure rather than
// hidden. The assertions pin that shape so a future change to the matcher,
// the tracker or the metric cannot silently move the class of failure
// without updating this test.

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTop1GradingWindowTrailingMessage(t *testing.T) {
	if testing.Short() {
		t.Skip("needs magick and tesseract (real pipeline replay)")
	}
	for _, tool := range []string{"magick", "tesseract"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("NOT RUN: %s is required for the replay", tool)
		}
	}

	// Invented lines: A is the dialogue the sample is graded on, B is a
	// short prompt-like line that the trailing frames render.
	const (
		textA = "the tide carried the old song back to the sleeping shore"
		textB = "press on to continue the tale"
		idA   = "msg-tail-window-a"
		idB   = "msg-tail-window-b"
	)
	vars := func(s string) *string { return &s }

	frames := frameSchedule() // 10 frames: box in frames 3-9, advance frame 10
	mkSample := func(id, text, msgID string) Sample {
		return Sample{
			ID: id, Frames: frames, SourceText: text, MessageID: vars(msgID),
			Speaker: SpeakerUnknown, TextKind: "dialogue", Scene: SceneID,
			IntendedSpeech: true, Split: SplitForID(id),
			Variant: Variant{Font: Fonts[0], Resolution: "640x480"},
		}
	}
	ts := &TruthSet{
		SchemaVersion: SchemaVersion,
		Set:           "bench-synthetic-tail-window",
		Origin:        "generated_synthetic",
		SeedBase:      1,
		ProfileFile:   "profile.json",
		Samples:       []Sample{mkSample("bn0001", textA, idA), mkSample("bn0002", textB, idB)},
	}
	if err := ts.Validate(); err != nil {
		t.Fatalf("set validation: %v", err)
	}
	prof := &ScriptProfile{
		SchemaVersion: SchemaVersion,
		ID:            "bench-synthetic-tail-window",
		Origin:        "generated_synthetic",
		Description:   "Tail-window test script (invented text, ADR-0058 amendment 2).",
		Scene:         SceneID,
		Regions:       []Region{{ID: "bottom", Type: "textbox", X: 0.0975, Y: 0.7565, W: 0.8025, H: 0.1515}},
		Messages: []ScriptLine{
			{ID: idA, Text: textA, Speaker: SpeakerUnknown},
			{ID: idB, Text: textB, Speaker: SpeakerUnknown},
		},
	}
	idx, err := prof.BuildIndex()
	if err != nil {
		t.Fatalf("BuildIndex: %v", err)
	}
	benchProfile = prof
	regions := prof.BuildRegions()

	work := t.TempDir()
	dirs, err := RenderSamples(ts, work)
	if err != nil {
		t.Fatalf("render: %v", err)
	}

	// Extended sample: s1's own ten frames, then s2's ten frames (its text
	// frames render message B) shifted past s1's capture clock. This is the
	// captured cx0014 shape: the graded line's window, then trailing frames
	// that show another in-script message.
	extDir := filepath.Join(work, "s1-extended")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatal(err)
	}
	shift := int64(10) * frameStepNS
	extFrames := make([]Frame, 0, 20)
	for i, f := range ts.Samples[0].Frames {
		name := fmt.Sprintf("t%s.png", strconv.Itoa(i+1))
		if err := copyFile(filepath.Join(dirs["bn0001"], f.File), filepath.Join(extDir, name)); err != nil {
			t.Fatal(err)
		}
		extFrames = append(extFrames, Frame{File: name, TsNS: f.TsNS})
	}
	for i, f := range ts.Samples[1].Frames {
		name := fmt.Sprintf("t%s.png", strconv.Itoa(i+11))
		if err := copyFile(filepath.Join(dirs["bn0002"], f.File), filepath.Join(extDir, name)); err != nil {
			t.Fatal(err)
		}
		extFrames = append(extFrames, Frame{File: name, TsNS: f.TsNS + shift})
	}
	ext := ts.Samples[0]
	ext.Frames = extFrames
	// The oracle for the extended span: line A is on screen on s1's box
	// frames only; s2's frames render line B (the trailing content) and are
	// outside A's display window — the captured cx0014 shape, where the
	// fixture's per-frame oracle labels play this role.
	first := len(ts.Samples[0].Frames)
	// s1's frames are the first block: line A's window is its box-present
	// frames; s2's frames (the trailing content, line B) are outside it.
	ext.DisplayWindow = make([]bool, len(extFrames))
	for i := 0; i < first; i++ {
		ext.DisplayWindow[i] = ts.Samples[0].Frames[i].File == "text.png"
	}
	tsPath := filepath.Join(work, "ts-extended.json")
	if err := writeManifest(tsPath, ext.Frames); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pinReplayEngineThreads()
	out, err := runSample(ctx, extDir, tsPath, ext, idx, regions)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}

	// 1. The shown line is committed first: the recall path is unaffected by
	// the trailing content.
	if len(out.commitMsgIDs) == 0 || out.commitMsgIDs[0] != idA {
		t.Fatalf("commits = %v, want the shown line %s first", out.commitMsgIDs, idA)
	}
	// 2. The trailing message is ranked top-1 when it is on screen: the
	// matcher is right about both lines (the last-row reading).
	if out.topID != idB {
		t.Fatalf("last ranking row's top = %s, want the trailing line %s", out.topID, idB)
	}
	// 3. Display-window grading (ADR-0058 amendment 3): graded on the frames
	// the oracle says line A is on screen, the truth line is the top-1 — the
	// cx0014-shaped miss is a grading-window artifact, not a matcher error.
	if !out.windowRanked {
		t.Fatalf("window ranking = none, want a ranking row inside the display window")
	}
	if out.windowTopID != idA {
		t.Fatalf("display-window top-1 = %s, want the truth line %s (the committed/stable reading in the window)", out.windowTopID, idA)
	}
	// 4. The misses list records the readings separately: the window reading
	// is a hit, the legacy last-row reading still records the trailing
	// message (visible, not hidden — the amendment-2 evidence stays on the
	// record as the top1_last_row figure).
	misses := top1Misses([]sampleOutcomes{out})
	if len(misses) != 1 || misses[0].SampleID != ext.ID || misses[0].MessageID != idA || misses[0].TopID != idB || misses[0].Reading != Top1ReadingLastRow {
		t.Fatalf("top1Misses = %+v, want exactly one miss: the last-row reading (truth %s, top-1 %s)", misses, idA, idB)
	}
	// 5. The metric rows grade one way and keep the other visible:
	// top1_exact_accuracy (display window) counts the truth as correct,
	// top1_last_row (legacy span-wide figure) records the miss.
	m := RunMetrics([]sampleOutcomes{out})
	if g := findMetric(t, m, "top1_exact_accuracy"); g.N != 1 || g.Value != 1.0 || g.Met != "met" {
		t.Errorf("top1_exact_accuracy = %+v, want 1/1 met on the display window", g)
	}
	if g := findMetric(t, m, "top1_last_row"); g.N != 1 || g.Value != 0.0 {
		t.Errorf("top1_last_row = %+v, want 0/1 (the trailing message still reads the legacy figure down)", g)
	}
	// 6. The harness lost no trace rows: the ring is sized to the sample and
	// the overflow fence (Stats.TraceDropped) reads zero (ADR-0058 amendment
	// 3).
	if out.stats.TraceDropped != 0 {
		t.Fatalf("trace ring dropped %d rows on a %d-frame sample sized to %d rows: the harness must lose no rows", out.stats.TraceDropped, len(ext.Frames), benchTraceMax(len(ext.Frames)))
	}
	// 7. The scripted speech side speaks only the truth line, once.
	if len(out.speechStarted) != 1 || out.speechStarted[0].messageID != idA {
		t.Fatalf("speech.started = %+v, want exactly the truth line %s once", out.speechStarted, idA)
	}
}

// copyFile copies one rendered frame into the extended sample's frame dir.
func copyFile(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, raw, 0o644)
}
