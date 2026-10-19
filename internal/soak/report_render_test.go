package soak

// Rendering tests for the soak report's per-stage latency section
// (docs/spec/16 M7 "diagnostics expose bottlenecks"): the section names the
// slowest stage from the shared summary function's output, and the JSON
// report carries the same rows — a report that omits the artifact is a
// report that hides the bottleneck.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// slowOCRLatency is the synthetic run behind the tests: a measured OCR stage
// (p95 1000 ms), a fast ranking stage and a 2000 ms end-to-end — the shape
// the unit test in internal/runtime asserts.
func slowOCRLatency() pipeline.Latency {
	return pipeline.Latency{
		FrameToOCR: pipeline.LatencySummary{
			Count: 100, P50: 50 * time.Millisecond, P95: 1000 * time.Millisecond,
			Mean: 200 * time.Millisecond, Max: 4000 * time.Millisecond,
		},
		OCRToRanked: pipeline.LatencySummary{
			Count: 100, P50: 5 * time.Millisecond, P95: 20 * time.Millisecond,
			Mean: 10 * time.Millisecond, Max: 50 * time.Millisecond,
		},
		FrameToCommitted: pipeline.LatencySummary{
			Count: 100, P50: 100 * time.Millisecond, P95: 2000 * time.Millisecond,
			Mean: 300 * time.Millisecond, Max: 5000 * time.Millisecond,
		},
	}
}

func slowOCRResult() Result {
	return Result{
		RunID:        "test-run",
		Mode:         "chaos",
		Seed:         42,
		DurationWall: 100 * time.Second,
		Pipeline:     pipeline.Stats{Commits: 100, Latency: slowOCRLatency()},
		Bottlenecks:  runtime.Bottlenecks(slowOCRLatency()),
	}
}

// TestSoakReportNamesTheSlowestStage: the markdown report's per-stage section
// renders the shared summary's rows and names the slow stage — here OCR, the
// stage the synthetic run made slow.
func TestSoakReportNamesTheSlowestStage(t *testing.T) {
	r := slowOCRResult()
	dir := t.TempDir()
	r.Config.OutDir = dir
	if err := writeMarkdown(dir, &r); err != nil {
		t.Fatalf("writeMarkdown: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.md"))
	if err != nil {
		t.Fatal(err)
	}
	md := string(raw)
	for _, want := range []string{
		"## Per-stage latency (bottleneck summary)",
		"**Slowest stage: capture → OCR (`frame_to_ocr`)**",
		"| capture → OCR (`frame_to_ocr`) | 100 | 50 | 1000 | 4000 | 0.50 |",
		"| capture → committed (end-to-end) (`frame_to_committed`) | 100 | 100 | 2000 | 5000 | 1.00 |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("report.md does not contain %q:\n%s", want, md)
		}
	}
}

// TestSoakReportJSONCarriesTheBottlenecks: the machine-readable report holds
// the same summary the markdown renders, so a reviewer and a script read the
// same artifact.
func TestSoakReportJSONCarriesTheBottlenecks(t *testing.T) {
	r := slowOCRResult()
	dir := t.TempDir()
	r.Config.OutDir = dir
	if err := writeJSON(dir, &r); err != nil {
		t.Fatalf("writeJSON: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct {
		Bottlenecks runtime.BottleneckSummary `json:"bottlenecks"`
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("report.json does not parse: %v", err)
	}
	if onDisk.Bottlenecks.SlowestStage != runtime.StageFrameToOCR {
		t.Fatalf("json slowest_stage = %q, want %q", onDisk.Bottlenecks.SlowestStage, runtime.StageFrameToOCR)
	}
	if len(onDisk.Bottlenecks.Stages) != len(r.Bottlenecks.Stages) {
		t.Fatalf("json carries %d stage rows, in-memory %d", len(onDisk.Bottlenecks.Stages), len(r.Bottlenecks.Stages))
	}
}
