package scenario

// dolphin_cleanup_test.go is the regression gate for the /tmp leak: a worker
// crashed with EDQUOT behind six gn-scenario-dolphin-* dirs of 233 MB each,
// left behind because the materialized replay dir was only removed on
// materialize's own error paths and never by the caller. Every materialized
// replay dir must be removed when its scenario finishes — success, failure,
// or panic — and in tests the dir must land under a t.TempDir()-rooted base
// dir (the injectable base of materializeDolphinReplay), never silently under
// the system temp dir.
//
// The fixture this runs against is generated in-test (solid synthetic frames,
// a manifest that hashes them, a truth.json, a profile.json): no game
// capture, no real OCR, no committed fixture (AGENTS.md rule 5).

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
)

// assertNoReplayDirs fails the test if baseDir carries any
// gn-scenario-dolphin-* entry: the leak this regression exists to catch.
func assertNoReplayDirs(t *testing.T, baseDir string) {
	t.Helper()
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		t.Fatalf("read replay base %s: %v", baseDir, err)
	}
	var offenders []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "gn-scenario-dolphin-") {
			offenders = append(offenders, e.Name())
		}
	}
	if len(offenders) > 0 {
		t.Errorf("replay base %s still holds %d gn-scenario-dolphin-* dir(s) after the scenario finished: %v", baseDir, len(offenders), offenders)
	}
}

// fakeDolphinFrameRow is one frame of the in-test fixture: the file name, its
// capture time and the truth the harness is owed.
type fakeDolphinFrameRow struct {
	file    string
	tsNS    int64
	present bool
	text    string
	full    bool
}

// fakeDolphinFixture writes one tiny synthetic fixture under fxRoot/fxID —
// six 640x480 near-black frames (a bright bar in the textbox rect grows per
// frame, so every crop hash is unique), a manifest.json that hashes them, a
// truth.json and the profile.json the dolphin runner reads from the fixture
// dir — and returns the SHA-256 of its manifest.json.
func fakeDolphinFixture(t *testing.T, fxRoot, fxID string) string {
	t.Helper()
	must := func(err error) {
		if err != nil {
			t.Fatalf("fake fixture %s: %v", fxID, err)
		}
	}
	fxDir := filepath.Join(fxRoot, fxID)
	must(os.MkdirAll(filepath.Join(fxDir, "frames"), 0o755))

	rows := []fakeDolphinFrameRow{
		{"frame-001.png", 100 * int64(time.Millisecond), false, "", false},
		{"frame-002.png", 200 * int64(time.Millisecond), true, "", false},
		{"frame-003.png", 300 * int64(time.Millisecond), true, "Line a appears.", false},
		{"frame-004.png", 400 * int64(time.Millisecond), true, "Line a appears.", true},
		{"frame-005.png", 500 * int64(time.Millisecond), true, "Line a appears.", true},
		{"frame-006.png", 600 * int64(time.Millisecond), true, "Line a appears.", true},
	}

	// Write the frames: near-black (the presence panel detector wants ≥30%
	// dark share) with a bright bar inside the textbox rect whose width grows
	// per frame. The bar (not a single pixel) survives the OCR preprocess
	// recipe (Otsu + median filter), so every frame keeps a distinct crop
	// hash and the truth engine answers each read from its own row — the same
	// shape as a typewriter reveal in the real corpus.
	type manifestFrame struct {
		File   string `json:"file"`
		TsNS   int64  `json:"ts_ns"`
		SHA256 string `json:"sha256"`
		Screen string `json:"screen"`
		Sig    string `json:"sig"`
	}
	frames := make([]manifestFrame, 0, len(rows))
	for i, row := range rows {
		img := image.NewRGBA(image.Rect(0, 0, 640, 480))
		for y := 0; y < 480; y++ {
			for x := 0; x < 640; x++ {
				img.SetRGBA(x, y, color.RGBA{8, 8, 8, 255})
			}
		}
		for y := 405; y < 430; y++ {
			for x := 150; x < 150+20*(i+1); x++ {
				img.SetRGBA(x, y, color.RGBA{220, 220, 220, 255})
			}
		}
		var buf bytes.Buffer
		must(png.Encode(&buf, img))
		path := filepath.Join(fxDir, "frames", row.file)
		must(os.WriteFile(path, buf.Bytes(), 0o644))
		sum := sha256.Sum256(buf.Bytes())
		frames = append(frames, manifestFrame{File: row.file, TsNS: row.tsNS, SHA256: hex.EncodeToString(sum[:]), Screen: "dialogue", Sig: ""})
	}

	type manifest struct {
		Schema int             `json:"schema"`
		ID     string          `json:"id"`
		Origin string          `json:"origin"`
		Frames []manifestFrame `json:"frames"`
	}
	manRaw, err := json.MarshalIndent(manifest{Schema: dolphin.FixtureSchema, ID: fxID, Origin: "generated_synthetic", Frames: frames}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(fxDir, "manifest.json"), append(manRaw, '\n'), 0o644))

	type truthRow struct {
		File    string `json:"file"`
		TsNS    int64  `json:"ts_ns"`
		Present bool   `json:"box_present"`
		Text    string `json:"text"`
		Line    string `json:"line"`
		Full    bool   `json:"text_is_full_line"`
	}
	truthRows := make([]truthRow, 0, len(rows))
	for _, row := range rows {
		line := ""
		if row.text != "" {
			line = "a"
		}
		truthRows = append(truthRows, truthRow{File: row.file, TsNS: row.tsNS, Present: row.present, Text: row.text, Line: line, Full: row.full})
	}
	truthRaw, err := json.MarshalIndent(truthRows, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(fxDir, "truth.json"), append(truthRaw, '\n'), 0o644))

	profile := map[string]any{
		"id":          "cleanup",
		"origin":      "generated_synthetic",
		"description": "In-test profile for the replay-dir cleanup regression: one invented line and one distractor.",
		"text_kind":   "dialogue",
		"messages": []any{
			map[string]any{"id": "line-a", "original_text": "Line a appears."},
			map[string]any{"id": "line-b", "original_text": "Line b never appears."},
		},
		"edges": []any{
			map[string]any{"from": "line-a", "to": "line-b", "type": "next", "confidence": 0.9},
		},
		"regions": []any{
			map[string]any{"id": "bottom", "type": "textbox", "x": 0.096875, "y": 0.75625, "w": 0.80625, "h": 0.158333},
		},
	}
	profRaw, err := json.MarshalIndent(profile, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(fxDir, "profile.json"), append(profRaw, '\n'), 0o644))

	// The scenario's manifest_sha256 reference: the SHA-256 of manifest.json.
	sum := sha256.Sum256(append(manRaw, '\n'))
	return hex.EncodeToString(sum[:])
}

// TestDolphinReplayDirCleanup runs a dolphin scenario replay against the
// in-test fake fixture with the materialized replay dir under a
// t.TempDir()-rooted base, and asserts no gn-scenario-dolphin-* dir remains
// under that base afterwards — on a passing scenario, on a failing scenario,
// and on a materialize error (out-of-range slice).
func TestDolphinReplayDirCleanup(t *testing.T) {
	root := t.TempDir()
	fxRoot := filepath.Join(root, "fixtures")
	baseDir := filepath.Join(root, "replay-base")
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		t.Fatalf("replay base: %v", err)
	}

	const fxID = "cleanup-fixture"
	manifestSHA := fakeDolphinFixture(t, fxRoot, fxID)
	t.Setenv(dolphin.EnvFixtureDir, fxRoot)

	sc := &Scenario{
		SchemaVersion: SchemaVersion,
		ID:            "dolphin-cleanup/fake",
		Origin:        OriginDolphin,
		Profile:       "cleanup",
		Mode:          ModeFrameReplay,
		Input:         Input{Fixture: fxID, ManifestSHA256: manifestSHA},
		Inference:     Inference{Decision: InferenceNone, TTS: InferenceNone},
		Assertions: []Assertion{
			{Type: "message_detected", MessageID: "line-a", MaxDelayMS: 1500},
			{Type: "message_committed_count", MessageID: "line-a", Count: 1},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// The materialized dir must actually land under the injected base:
	// materialize it once and prove the dir appears there before cleanup.
	fxDir := filepath.Join(fxRoot, fxID)
	dir, _, _, man, cleanup, err := materializeDolphinReplay(fxDir, Input{Fixture: fxID}, baseDir)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !strings.HasPrefix(dir, filepath.Join(baseDir, "gn-scenario-dolphin-")) {
		t.Fatalf("materialized dir %s is not under the injected base %s", dir, baseDir)
	}
	if len(man.Frames) != 6 {
		t.Fatalf("fake fixture has %d frames, want 6", len(man.Frames))
	}
	cleanup()
	assertNoReplayDirs(t, baseDir)

	t.Run("success leaves no replay dir", func(t *testing.T) {
		res, err := runDolphinScenario(ctx, root, sc, baseDir)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if res.Status != "passed" {
			t.Fatalf("status %q, want passed (%s)", res.Status, failedDetails(res))
		}
		assertNoReplayDirs(t, baseDir)
	})

	t.Run("failure leaves no replay dir", func(t *testing.T) {
		fail := *sc
		fail.ID = "dolphin-cleanup/fail"
		// An assertion the run cannot satisfy: zero commits of a line that
		// never appears, demanded three times.
		fail.Assertions = []Assertion{{Type: "message_committed_count", MessageID: "line-ghost", Count: 3}}
		res, err := runDolphinScenario(ctx, root, &fail, baseDir)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if res.Status != "failed" {
			t.Fatalf("status %q, want failed", res.Status)
		}
		assertNoReplayDirs(t, baseDir)
	})

	t.Run("materialize error leaves no partial dir", func(t *testing.T) {
		bad := *sc
		bad.Input.Last = 99 // the fixture has 6 frames
		_, err := runDolphinScenario(ctx, root, &bad, baseDir)
		if err == nil {
			t.Fatal("want an out-of-range slice error, got nil")
		}
		assertNoReplayDirs(t, baseDir)
	})
}
