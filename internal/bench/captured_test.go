package bench

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// fixtureDir resolves the external fixture root (env, or the gitignored
// default under the repository root — tried from both the package dir and
// the working dir, since go test runs in the former) and the
// windwaker-newgame fixture under it. It reports ok=false when none exists.
func fixtureDir() (string, bool) {
	candidates := []string{}
	if root := os.Getenv("GAME_NARRATOR_FIXTURE_DIR"); root != "" {
		candidates = append(candidates, root)
	} else {
		candidates = append(candidates, filepath.Join("..", "..", "artifacts", "fixtures"), "artifacts/fixtures")
	}
	for _, c := range candidates {
		if st, err := os.Stat(filepath.Join(c, "windwaker-newgame")); err == nil && st.IsDir() {
			return filepath.Join(c, "windwaker-newgame"), true
		}
	}
	return "", false
}

// TestLoadCapturedFixture verifies the fixture's integrity gate (manifest
// schema + every frame's SHA-256, truth/manifest cross-check) and the
// segmentation rule: one leading empty sample (the precision fence), one
// dialogue sample per narration line with its trailing frames. Skipped when
// the external fixture is absent (NOT RUN, never a pass).
func TestLoadCapturedFixture(t *testing.T) {
	dir, ok := fixtureDir()
	if !ok {
		t.Skip("NOT RUN: fixture windwaker-newgame is absent (external, gitignored; record it with the dolphin fixture factory)")
	}
	fx, err := LoadCapturedFixture(dir)
	if err != nil {
		t.Fatalf("LoadCapturedFixture: %v", err)
	}
	if fx.ID != "windwaker-newgame" || fx.ManifestSHA == "" {
		t.Fatalf("fixture header: id %q sha %q", fx.ID, fx.ManifestSHA)
	}
	if len(fx.Rows) != len(fx.Manifest.Frames) {
		t.Fatalf("truth rows %d != manifest frames %d", len(fx.Rows), len(fx.Manifest.Frames))
	}

	segments := fx.Segments(func(text string) string { return "" }, nil)
	if len(segments) != 3 {
		t.Fatalf("segments = %d, want 3 (one leading empty + the fixture's two narration lines)", len(segments))
	}
	if segments[0].TextKind != "empty" {
		t.Errorf("segment 1 kind = %q, want empty (the leading frames precede the first line)", segments[0].TextKind)
	}
	if segments[0].MessageID != nil || segments[0].SourceText != "" {
		t.Errorf("empty segment carries truth: %+v", segments[0])
	}
	if segments[0].Split != SplitHoldout {
		t.Errorf("captured samples are the holdout split, got %q", segments[0].Split)
	}
	allAbsent := true
	for _, f := range segments[0].Frames {
		if !f.Absent {
			allAbsent = false
		}
	}
	if !allAbsent {
		t.Errorf("leading empty segment: every frame is oracle-absent")
	}
	for _, s := range segments[1:] {
		if s.TextKind != "dialogue" || s.Speaker != SpeakerUnknown || !s.IntendedSpeech {
			t.Errorf("dialogue segment %s: kind %q speaker %q intended %v", s.ID, s.TextKind, s.Speaker, s.IntendedSpeech)
		}
		if s.MessageID != nil {
			t.Errorf("segment %s: the no-op resolver maps nothing to a message id", s.ID)
		}
	}
	// The line's first frame is present; the advance frame after it is not.
	a := segments[1]
	if a.Frames[0].Absent {
		t.Errorf("line segment %s: its first frame is the first text frame (oracle present)", a.ID)
	}
	if len(a.Frames) > 1 && !a.Frames[len(a.Frames)-1].Absent {
		t.Errorf("line segment %s: the trailing frame is oracle-absent (the advance)", a.ID)
	}
	// Timestamps are the recording's own, strictly increasing.
	for _, s := range segments {
		for i := 1; i < len(s.Frames); i++ {
			if s.Frames[i].TsNS <= s.Frames[i-1].TsNS {
				t.Errorf("segment %s: timestamps not strictly increasing at frame %d", s.ID, i)
			}
		}
	}
}

// TestResolveTruthMapping is the truth-to-script mapping the captured holdout
// grades with: an exact folded-key match, a small edit-distance match (the
// record-time reading and the BMG text differ by a glyph or two), and a
// non-match (a different line is never attached to the closest script row).
func TestResolveTruthMapping(t *testing.T) {
	msgs := []games.Message{
		{ID: "m1", OriginalText: "Long ago, there existed a kingdom where the power lay hidden."},
		{ID: "m2", OriginalText: "It was a prosperous land blessed with green forests."},
		{ID: "m3", OriginalText: "An entirely different line that is much longer than the others."},
	}
	resolve := resolveTruth(msgs)

	// Exact after the fold (punctuation and case are folded away).
	if got := resolve("Long ago, there existed a kingdom where the power lay hidden!"); got != "m1" {
		t.Errorf("exact match: got %q, want m1", got)
	}
	// One glyph off (a Tesseract-class error): within the tolerance.
	if got := resolve("It was a prosperous lund blessed with green forests."); got != "m2" {
		t.Errorf("fuzzy match: got %q, want m2", got)
	}
	// A different line: no attachment, ever.
	if got := resolve("The wind carried the song of the far islands today."); got != "" {
		t.Errorf("non-match: got %q, want empty", got)
	}
}

// TestSegmentsRegression is the guard against the segmentation loop: a line
// at the very end of the capture (no trailing frame) and trailing empty
// frames after the last line must both segment in O(rows) — the original
// loop spun forever on a non-line row the inner loop could not consume.
func TestSegmentsRegression(t *testing.T) {
	row := func(file string, present bool, text, line string) capturedRow {
		return capturedRow{File: file, TsNS: 0, Present: present, Text: text, Line: line, Full: text != ""}
	}
	mk := func(rows []capturedRow) *CapturedFixture {
		return &CapturedFixture{ID: "regression", Rows: rows, Region: Region{ID: "bottom", Type: "textbox", X: 0.1, Y: 0.7, W: 0.8, H: 0.2}, Resolution: "640x480"}
	}
	resolve := func(text string) string { return "" }

	// Line at the very end: the segment run must not outlive the rows.
	fx := mk([]capturedRow{row("f1", false, "", ""), row("f2", true, "a line", "a"), row("f3", true, "a line", "a")})
	segs := fx.Segments(resolve, nil)
	if len(segs) != 2 { // leading empty + the line
		t.Fatalf("segments = %d, want 2 (line at end of capture)", len(segs))
	}
	// Trailing empty frame after the last line: it extends the line's span
	// (the advance frame), it is not a new sample.
	fx = mk([]capturedRow{row("f1", true, "a line", "a"), row("f2", true, "a line", "a"), row("f3", false, "", "")})
	segs = fx.Segments(resolve, nil)
	if len(segs) != 1 || len(segs[0].Frames) != 3 {
		t.Fatalf("segments = %d (frames %d), want 1 sample of 3 frames (trailing empty belongs to the line)", len(segs), len(segs[0].Frames))
	}
	// No line at all: the whole capture is one empty sample.
	fx = mk([]capturedRow{row("f1", false, "", ""), row("f2", false, "", "")})
	segs = fx.Segments(resolve, nil)
	if len(segs) != 1 || segs[0].TextKind != "empty" {
		t.Fatalf("segments = %d, want 1 empty sample (no narration lines)", len(segs))
	}
	// Two line occurrences with an empty frame between: the middle empty frame
	// belongs to the first line's span, the second line is its own sample.
	fx = mk([]capturedRow{
		row("f1", true, "first line", "a"), row("f2", true, "first line", "a"),
		row("f3", false, "", ""),
		row("f4", true, "second line", "b"),
	})
	segs = fx.Segments(resolve, nil)
	if len(segs) != 2 { // no leading empty (the capture starts on a line)
		t.Fatalf("segments = %d, want 2 (two line occurrences, no leading empty)", len(segs))
	}
	if len(segs[0].Frames) != 3 || len(segs[1].Frames) != 1 {
		t.Fatalf("frame counts %d/%d, want 3/1 (the middle empty frame extends the first span)", len(segs[0].Frames), len(segs[1].Frames))
	}
}

func TestLevenshtein(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"abc", "", 3},
		{"abc", "abc", 0},
		{"land", "lund", 1},
		{"kitten", "sitting", 3},
		{"flaw", "lawn", 2},
	}
	for _, c := range cases {
		if got := levenshtein(c.a, c.b); got != c.want {
			t.Errorf("levenshtein(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestCapturedBenchNotRun is the honest-skip contract: no image, a
// non-matching image, and a missing fixture dir are each NOT RUN with an
// actionable message — never a failure, never a pass.
// TestCapturedProfileCarriesTheSpeakerSidecar (ADR-0070) locks the bench
// wiring that makes named_speaker_accuracy gradeable on captured holdouts:
// the adapter's speaker sidecar flows into the bench script lines, and a
// parse without one leaves every line unknown — never a guessed value.
func TestCapturedProfileCarriesTheSpeakerSidecar(t *testing.T) {
	msgs := []games.Message{
		{ID: "m1", OriginalText: "invented line one"},
		{ID: "m2", OriginalText: "invented line two"},
		{ID: "m3", OriginalText: "invented line three"},
	}
	parsed := &adapter.ParseResult{
		Messages:    msgs,
		SpeakerKeys: []string{"ww:spk:7", "", "ww:spk:7"},
	}
	speakerOf := speakerKeyOfMessage(parsed)
	if speakerOf("m1") != "ww:spk:7" || speakerOf("m2") != "" || speakerOf("m3") != "ww:spk:7" {
		t.Fatalf("speakerKeyOfMessage: %q / %q / %q, want the sidecar's keys in order", speakerOf("m1"), speakerOf("m2"), speakerOf("m3"))
	}
	fx := &CapturedFixture{ID: "unit-fixture", Region: Region{ID: "textbox"}}
	prof := CapturedProfile(fx, msgs, speakerOf)
	if len(prof.Messages) != 3 {
		t.Fatalf("profile has %d lines, want 3", len(prof.Messages))
	}
	if prof.Messages[0].Speaker != "ww:spk:7" || prof.Messages[1].Speaker != SpeakerUnknown || prof.Messages[2].Speaker != "ww:spk:7" {
		t.Errorf("line speakers = %q / %q / %q, want the sidecar key, unknown for the unattributed line, the key again",
			prof.Messages[0].Speaker, prof.Messages[1].Speaker, prof.Messages[2].Speaker)
	}
	plain := CapturedProfile(fx, msgs, speakerKeyOfMessage(&adapter.ParseResult{Messages: msgs}))
	for i, l := range plain.Messages {
		if l.Speaker != SpeakerUnknown {
			t.Errorf("line %d speaker = %q, want unknown (no sidecar, no guess)", i, l.Speaker)
		}
	}
}

func TestCapturedBenchNotRun(t *testing.T) {
	dir, ok := fixtureDir()
	if !ok {
		t.Skip("NOT RUN: fixture absent (this test needs its manifest for the image-SHA gate)")
	}
	ctx := context.Background()
	// No image at all.
	if _, err := RunCapturedBench(ctx, dir, "", t.TempDir()); err == nil || !strings.Contains(err.Error(), "NOT RUN") {
		t.Errorf("empty image: got %v, want a NOT RUN error", err)
	}
	// An image that is not the fixture's image: the SHA-256 gate must refuse.
	dummy := filepath.Join(t.TempDir(), "image.iso")
	if err := os.WriteFile(dummy, []byte("not a game image"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := RunCapturedBench(ctx, dir, dummy, t.TempDir()); err == nil || !strings.Contains(err.Error(), "NOT RUN") {
		t.Errorf("mismatched image: got %v, want a NOT RUN error (SHA-256 gate)", err)
	}
	// A missing fixture dir.
	if _, err := RunCapturedBench(ctx, filepath.Join(dir, "does-not-exist"), dummy, t.TempDir()); err == nil || !strings.Contains(err.Error(), "fixture") {
		t.Errorf("missing fixture: got %v, want an error naming the fixture", err)
	}
}

// TestCapturedEndToEnd is the class-(b) captured-frame replay gate for the
// captured holdout: the fixture's frames (verified against the manifest)
// through the real pipeline (real Tesseract, real matcher over the imported
// script, real tracker), graded against the fixture's oracle. It is NOT RUN
// — never a pass — when tesseract, the external fixture or the user's game
// image is absent. It asserts the run is well-formed and honestly labelled;
// it does not assert the quality targets (M7 measures those and adjusts them
// via ADR with the evidence).
func TestCapturedEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract not installed")
	}
	dir, ok := fixtureDir()
	if !ok {
		t.Skip("NOT RUN: fixture windwaker-newgame is absent (set GAME_NARRATOR_FIXTURE_DIR)")
	}
	image := os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH")
	if image == "" {
		t.Skip("NOT RUN: GAME_NARRATOR_GAME_IMAGE_PATH not set (the user's Wind Waker image)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	outDir := t.TempDir()
	rep, err := RunCapturedBench(ctx, dir, image, outDir)
	if err != nil {
		if strings.Contains(err.Error(), "NOT RUN") {
			t.Skipf("NOT RUN: %v", err)
		}
		t.Fatalf("RunCapturedBench: %v", err)
	}

	if rep.Class != ClassCaptured || rep.Origin != "dolphin" || rep.Split != SplitHoldout {
		t.Fatalf("report header: class %q origin %q split %q (want b/dolphin/holdout)", rep.Class, rep.Origin, rep.Split)
	}
	if rep.Samples < 1 {
		t.Fatalf("no samples completed")
	}
	for _, m := range rep.Metrics {
		if m.N == 0 && m.Met != "n/a" {
			t.Errorf("%s: n=0 but met=%q (want n/a)", m.Name, m.Met)
		}
		if m.N > 0 && m.Met == "n/a" {
			t.Errorf("%s: n=%d but met=n/a", m.Name, m.N)
		}
	}
	// The latency rows, when present, are resolution-limited on the recording's
	// own frame grid (never a fake-exact number).
	for _, m := range rep.Metrics {
		if strings.HasPrefix(m.Name, "commit_latency_") && m.N > 0 {
			if !m.ResolutionLimited || m.Met != "resolution-limited" {
				t.Errorf("%s not marked resolution-limited: %+v", m.Name, m)
			}
		}
	}
	for _, name := range []string{"report.json", "report.md", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	var manifest map[string]any
	raw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("manifest.json does not parse: %v", err)
	}
	for _, k := range []string{"class", "fixture_manifest_sha", "game_image_sha256", "script_messages"} {
		if manifest[k] == nil {
			t.Errorf("manifest.json missing %q", k)
		}
	}
	// The game text never leaks into the report: the manifest names the image
	// by hash only (AGENTS.md rule 5).
	if s, _ := manifest["game_image_sha256"].(string); len(s) != 64 {
		t.Errorf("game_image_sha256 = %q, want a 64-hex-char hash", s)
	}
	md, err := os.ReadFile(filepath.Join(outDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	if !strings.Contains(string(md), "class (b) captured-frame replay") {
		t.Errorf("report.md does not carry the class (b) provenance note")
	}
	byName := map[string]Metric{}
	for _, m := range rep.Metrics {
		byName[m.Name] = m
	}
	t.Logf("captured holdout: %d samples — recall %.1f%%, top1 %.1f%%, cer %.1f%%, speech precision %.1f%%, p50 commit latency %v ms",
		rep.Samples,
		byName["recognition_recall"].Value*100,
		byName["top1_exact_accuracy"].Value*100,
		byName["ocr_cer"].Value*100,
		byName["positive_speech_precision"].Value*100,
		byName["commit_latency_p50_ms"].Value)
}
