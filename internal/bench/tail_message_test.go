package bench

// TestTop1GradingWindowTrailingMessage reproduces, on class (a) synthetic
// frames with invented text (no game material), the top-1 grading-window
// failure measured on the captured holdout's sample cx0014 (ADR-0058
// amendment 2): a sample span whose trailing frames show a DIFFERENT
// in-script message (there: the game's continuation prompt rendered in the
// subtitle bar). The pipeline and the matcher behave correctly on both lines
// — the shown line is committed, and the trailing line is ranked top-1 when
// it appears — but the top-1 metric grades the LAST ranking row of the whole
// span, so the sample is recorded as a top-1 miss. The assertions pin that
// shape so a future change to the matcher, the tracker or the metric cannot
// silently move the class of failure without updating this test.

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
	// matcher is right about both lines.
	if out.topID != idB {
		t.Fatalf("last ranking row's top = %s, want the trailing line %s", out.topID, idB)
	}
	// 3. The top-1 grading window (last ranking row of the whole span)
	// records the sample as a miss — the exact shape of the cx0014 entry in
	// the captured report.
	misses := top1Misses([]sampleOutcomes{out})
	if len(misses) != 1 || misses[0].SampleID != ext.ID || misses[0].MessageID != idA || misses[0].TopID != idB {
		t.Fatalf("top1Misses = %+v, want one miss (truth %s, top-1 %s)", misses, idA, idB)
	}
	// 4. The scripted speech side speaks only the truth line, once.
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
