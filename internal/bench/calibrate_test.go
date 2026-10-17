package bench

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/matcher"
)

// --- holdout isolation -------------------------------------------------------

// TestTuningSamplesNeverReadsHoldout is the structural holdout fence of the
// calibration fit: the fit may only ever see tuning-split samples, and the
// filter decides from the split recorded in truth.json (which LoadTruth
// re-derives from the sample id and refuses on mismatch). No fit input is
// ever assembled from a holdout id.
func TestTuningSamplesNeverReadsHoldout(t *testing.T) {
	// A set spanning both sides, with the split values the id hash actually
	// decides (a hand-set split field would fail LoadTruth, so this mirrors
	// what the generator writes).
	msg := "m1"
	var samples []Sample
	bySplit := map[string]int{}
	for i := 1; i <= 60; i++ {
		id := fmt.Sprintf("bn%04d", i)
		s := SplitForID(id)
		bySplit[s]++
		samples = append(samples, Sample{
			ID: id, Frames: []Frame{{File: "a.png", TsNS: 100_000_000 * int64(i)}},
			SourceText: "hello world", MessageID: &msg, Speaker: "keeper",
			TextKind: "dialogue", Scene: SceneID, IntendedSpeech: true, Split: s,
		})
	}
	if bySplit[SplitTuning] == 0 || bySplit[SplitHoldout] == 0 {
		t.Fatalf("split degenerate: %+v", bySplit)
	}
	kept := tuningSamples(&TruthSet{Samples: samples})
	if len(kept) != bySplit[SplitTuning] {
		t.Fatalf("tuningSamples kept %d, want %d (only the tuning split)", len(kept), bySplit[SplitTuning])
	}
	holdoutIDs := map[string]bool{}
	for _, s := range samples {
		if s.Split == SplitHoldout {
			holdoutIDs[s.ID] = true
		}
	}
	for _, s := range kept {
		if holdoutIDs[s.ID] {
			t.Fatalf("holdout sample %s leaked into the tuning selection — the fit would see it", s.ID)
		}
	}
}

// --- latency resolution ------------------------------------------------------

// TestMetricsLatencyResolutionLimited: the commit latency is a frame-grid
// offset on the capture clock. Known case: first text frame at 300 ms, commit
// transition stamped at 600 ms (the helper's fixed value) on a 100 ms grid —
// the reported value is the 300 ms grid offset, and the true latency is at
// most one frame gap (100 ms) above it, which the row must say.
func TestMetricsLatencyResolutionLimited(t *testing.T) {
	o := oneDialogueSample("m1", "abcd", "m1", 0.9) // firstText 300 ms, commit 600 ms
	o.sample.frameStepNS = 100_000_000
	m := RunMetrics([]sampleOutcomes{o})
	for _, name := range []string{"commit_latency_p50_ms", "commit_latency_p95_ms"} {
		got := findMetric(t, m, name)
		if got.Value != 300 {
			t.Errorf("%s: value = %v, want 300 (capture-clock grid offset)", name, got.Value)
		}
		if !got.ResolutionLimited || got.Met != "resolution-limited" {
			t.Errorf("%s: not marked resolution-limited (met = %q)", name, got.Met)
		}
		if got.Bound != 400 {
			t.Errorf("%s: bound = %v, want 400 (value + one 100 ms frame gap)", name, got.Bound)
		}
		if !strings.Contains(got.Detail, "100 ms") {
			t.Errorf("%s: detail %q does not state the frame interval", name, got.Detail)
		}
	}
	// With no samples the rows stay n/a and carry no bound or marker.
	empty := findMetric(t, RunMetrics(nil), "commit_latency_p50_ms")
	if empty.Met != "n/a" || empty.ResolutionLimited || empty.Bound != 0 {
		t.Errorf("empty latency row = %+v, want n/a without resolution marker", empty)
	}
}

// frameStepOf takes the largest gap, so a non-uniform schedule reports a
// conservative (wider) resolution bound, never a narrower one.
func TestFrameStepOf(t *testing.T) {
	frames := []Frame{{TsNS: 100}, {TsNS: 200}, {TsNS: 500}, {TsNS: 600}}
	if got := frameStepOf(frames); got != 300 {
		t.Fatalf("frameStepOf = %d, want 300 (the widest gap)", got)
	}
	if got := frameStepOf([]Frame{{TsNS: 100}}); got != 0 {
		t.Fatalf("single frame: frameStepOf = %d, want 0", got)
	}
}

// --- calibrate ----------------------------------------------------------------

// TestCalibrateTuningOnly runs the real calibration mode on a small tuning
// subset (same generator, real pipeline, real Tesseract) and asserts the fit
// REFUSES on the synthetic set: the pipeline reads every line correctly, so
// the tuning set has 0 incorrect top-1 examples — the degenerate shape the
// sufficiency guard exists for. Calibrate must return the typed data error
// with the counts, and calibration.json must not be written. A successful
// e2e fit needs real frames with labelled errors (the Dolphin fixture
// factory, M6; ADR-0035 "Calibration"). Skips NOT RUN when the engine or
// renderer is absent, like the rest of the bench tests.
func TestCalibrateTuningOnly(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract not installed")
	}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick not installed")
	}
	srcProfile := filepath.Join("..", "..", "testdata", "synthetic", "profiles", "dialogue-advance-001.json")
	ts, prof, err := GenerateSamples(srcProfile, 42)
	if err != nil {
		t.Fatalf("GenerateSamples: %v", err)
	}
	// Eight tuning-split dialogue samples: below the 50-sample floor and 100%
	// top-1 correct — the refusal is the expected, asserted outcome.
	var kept []Sample
	for _, s := range ts.Samples {
		if len(kept) == 8 {
			break
		}
		if s.Split == SplitTuning && s.TextKind == "dialogue" {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		t.Fatalf("no tuning dialogue samples in the generated set")
	}
	dir := t.TempDir()
	ts.Samples = kept
	writeSet(t, dir, ts, prof)

	outDir := filepath.Join(dir, "cal")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	cal, err := Calibrate(ctx, dir, outDir)
	if err == nil {
		t.Fatalf("Calibrate on the all-correct synthetic set succeeded with T = %v, want the insufficient-data refusal", cal.Temperature)
	}
	var insuf *matcher.ErrInsufficientCalibrationData
	if !errors.As(err, &insuf) {
		t.Fatalf("error = %v (%T), want *matcher.ErrInsufficientCalibrationData", err, err)
	}
	if insuf.Incorrect != 0 {
		t.Errorf("Incorrect = %d, want 0 (the synthetic pipeline reads every line correctly)", insuf.Incorrect)
	}
	if insuf.Samples < 1 || insuf.Samples > 8 {
		t.Errorf("Samples = %d, want the ≤8 usable tuning samples the fit saw", insuf.Samples)
	}
	if insuf.MinIncorrect != 10 || insuf.MinSamples != 50 {
		t.Errorf("thresholds = MinIncorrect %d / MinSamples %d, want the defaults 10 / 50", insuf.MinIncorrect, insuf.MinSamples)
	}
	// The refusal must not write a T.
	if _, err := os.Stat(filepath.Join(outDir, "calibration.json")); !os.IsNotExist(err) {
		t.Errorf("calibration.json exists after a refused fit (%v), want no file", err)
	}
}
