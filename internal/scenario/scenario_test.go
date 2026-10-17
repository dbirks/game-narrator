package scenario

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	lineA = "The harbour bell rang twice before anyone looked up."
	lineB = "Nobody in the village had seen that boat before."
)

var clipTruth = []Truth{
	{File: "frame-013.png", TsNS: 1_300_000_000, Text: lineA, Line: "a", Full: true},
	{File: "frame-014.png", TsNS: 1_400_000_000, Text: lineA, Line: "a", Full: true},
	{File: "frame-019.png", TsNS: 1_900_000_000, Text: lineB, Line: "b", Full: true},
}

// --- loader -----------------------------------------------------------------

func TestLoadScenarioValid(t *testing.T) {
	sc, err := Load(filepath.Join("..", "..", "testdata", "scenarios", "synthetic", "dialogue-advance-001.yaml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if sc.SchemaVersion != 1 || sc.ID != "synthetic/dialogue-advance-001" ||
		sc.Origin != OriginGeneratedSynthetic || sc.Mode != ModeFrameReplay ||
		sc.Profile != "dialogue-advance-001" {
		t.Fatalf("header = %+v", sc)
	}
	if sc.Input.FramesDir != "testdata/synthetic/replay/dialogue-advance-001" || sc.Input.Timestamps != "timestamps.json" {
		t.Fatalf("input = %+v", sc.Input)
	}
	if sc.Inference.Decision != InferenceNone || sc.Inference.TTS != InferenceNone {
		t.Fatalf("inference = %+v", sc.Inference)
	}
	if len(sc.Assertions) != 7 {
		t.Fatalf("len(assertions) = %d, want 7", len(sc.Assertions))
	}
	types := make([]string, 0, len(sc.Assertions))
	for _, a := range sc.Assertions {
		types = append(types, a.Type)
	}
	want := []string{
		"message_detected", "message_detected",
		"message_committed_count", "message_committed_count",
		"speech_cancelled", "no_duplicate_commit", "no_stale_generation_applied",
	}
	for i := range want {
		if types[i] != want[i] {
			t.Fatalf("assertion %d type = %q, want %q", i, types[i], want[i])
		}
	}
	if sc.Assertions[0].MessageID != "demo-page-1" || sc.Assertions[0].MaxDelayMS != 1500 {
		t.Fatalf("assertion 0 = %+v", sc.Assertions[0])
	}
	if sc.Assertions[4].Reason != "dialogue_advanced" || sc.Assertions[4].Count != 1 {
		t.Fatalf("assertion 4 = %+v", sc.Assertions[4])
	}
}

func TestLoadScenarioErrors(t *testing.T) {
	base := `schema_version: 1
id: synthetic/x
origin: generated_synthetic
profile: p
mode: frame_replay
input:
  frames_dir: d
  timestamps: timestamps.json
inference:
  decision: none
  tts: none
assertions:
  - type: no_duplicate_commit
`
	cases := []struct {
		name    string
		mutate  string // appended or substituted text
		wantSub string
	}{
		{"missing origin", strings.Replace(base, "origin: generated_synthetic\n", "", 1), "origin is required"},
		{"bad origin", strings.Replace(base, "origin: generated_synthetic", "origin: vibes", 1), "origin is required and must be one of"},
		{"bad mode", strings.Replace(base, "mode: frame_replay", "mode: vibes", 1), "mode must be"},
		{"unknown top-level key", base + "bogus: 1\n", "unknown scenario field"},
		{"unknown assertion", strings.Replace(base, "no_duplicate_commit", "message_detectd", 1), "unknown assertion type"},
		{"missing assertion field", strings.Replace(base, "  - type: no_duplicate_commit\n", "  - type: message_detected\n    message_id: m\n", 1), "max_delay_ms is required"},
		{"unexpected assertion field", strings.Replace(base, "  - type: no_duplicate_commit\n", "  - type: no_duplicate_commit\n    grace_ms: 5\n", 1), "unexpected field"},
		{"quoted int", strings.Replace(base, "  - type: no_duplicate_commit\n", "  - type: message_detected\n    message_id: m\n    max_delay_ms: \"1500\"\n", 1), "must be a non-negative integer"},
		{"missing input", strings.Replace(base, "input:\n  frames_dir: d\n  timestamps: timestamps.json\n", "", 1), "input is required"},
		{"bad inference", strings.Replace(base, "tts: none", "tts: sometimes", 1), "inference.tts must be one of"},
		{"no assertions", strings.Replace(base, "assertions:\n  - type: no_duplicate_commit\n", "assertions: []\n", 1), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "s.yaml")
			if err := os.WriteFile(path, []byte(c.mutate), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := Load(path)
			if err == nil {
				t.Fatalf("want error for %s", c.name)
			}
			if !strings.Contains(err.Error(), "line ") {
				t.Fatalf("error has no line number: %v", err)
			}
			if c.wantSub != "" && !strings.Contains(err.Error(), c.wantSub) {
				t.Fatalf("error %q does not contain %q", err, c.wantSub)
			}
		})
	}
	// "assertions: []" is flow style: the parser must reject it by line, and the
	// loader never gets to run.
	path := filepath.Join(t.TempDir(), "s.yaml")
	_ = os.WriteFile(path, []byte(strings.Replace(base, "assertions:\n  - type: no_duplicate_commit\n", "assertions: []\n", 1)), 0o644)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "line") {
		t.Fatalf("flow-style list must be a line-numbered error, got %v", err)
	}
}

func TestLoadProfile(t *testing.T) {
	p, err := LoadProfile(filepath.Join("..", "..", "testdata", "synthetic", "profiles", "dialogue-advance-001.json"))
	if err != nil {
		t.Fatalf("LoadProfile: %v", err)
	}
	if len(p.Messages) < 12 {
		t.Fatalf("len(messages) = %d, want >= 12 (two lines plus distractors)", len(p.Messages))
	}
	msgs := p.ToMessages()
	byID := make(map[string]string, len(msgs))
	for _, m := range msgs {
		if m.NormalizedText == "" {
			t.Fatalf("message %s has no normalized text", m.ID)
		}
		byID[m.ID] = m.NormalizedText
	}
	// Normalized keys are retrieval-folded: lower case, punctuation stripped.
	if byID["demo-page-1"] != fold(lineA) || byID["demo-page-2"] != fold(lineB) {
		t.Fatalf("normalized keys do not fold through textnorm: %q / %q", byID["demo-page-1"], byID["demo-page-2"])
	}
	if len(p.Edges) != 1 || p.Edges[0].From != "demo-page-1" || p.Edges[0].To != "demo-page-2" || p.Edges[0].Type != "next" {
		t.Fatalf("edges = %+v", p.Edges)
	}
	regs := p.ToRegions()
	if len(regs) != 1 || regs[0].ID != "bottom" {
		t.Fatalf("regions = %+v", regs)
	}
	// The rect must be the one truth.json labels, or OCR reads the wrong pixels.
	if regs[0].RectX != 0.096875 || regs[0].RectY != 0.75625 || regs[0].RectW != 0.80625 || regs[0].RectH != 0.158333 {
		t.Fatalf("region rect = (%f, %f, %f, %f), want the truth.json rect", regs[0].RectX, regs[0].RectY, regs[0].RectW, regs[0].RectH)
	}
}

func TestLoadProfileErrors(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "p.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no messages", `{"id": "p", "messages": [], "regions": [{"id": "b", "type": "textbox", "x": 0.1, "y": 0.7, "w": 0.5, "h": 0.2}]}`, "at least one message"},
		{"edge to unknown", `{"id": "p", "messages": [{"id": "m", "original_text": "x"}], "edges": [{"from": "m", "to": "ghost", "type": "next", "confidence": 0.5}], "regions": [{"id": "b", "type": "textbox", "x": 0.1, "y": 0.7, "w": 0.5, "h": 0.2}]}`, "unknown message"},
		{"bad rect", `{"id": "p", "messages": [{"id": "m", "original_text": "x"}], "regions": [{"id": "b", "type": "textbox", "x": 0.9, "y": 0.7, "w": 0.5, "h": 0.2}]}`, "does not fit"},
		{"no regions", `{"id": "p", "messages": [{"id": "m", "original_text": "x"}]}`, "at least one region"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := LoadProfile(write(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
}

func TestLoadTruth(t *testing.T) {
	rows, err := LoadTruth(filepath.Join("..", "..", "testdata", "synthetic", "replay", "dialogue-advance-001"))
	if err != nil {
		t.Fatalf("LoadTruth: %v", err)
	}
	if len(rows) != 24 {
		t.Fatalf("len(truth) = %d, want 24", len(rows))
	}
	fullA, fullB := 0, 0
	for _, r := range rows {
		if r.Full && r.Line == "a" {
			fullA++
		}
		if r.Full && r.Line == "b" {
			fullB++
		}
	}
	if fullA == 0 || fullB == 0 {
		t.Fatalf("truth marks no full-line frame for line a (=%d) or b (=%d)", fullA, fullB)
	}
}
