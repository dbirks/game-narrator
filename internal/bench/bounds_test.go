package bench

// Unit tests for the bench's per-stage bottleneck summary: the envelope merge
// of the per-sample bucketed histograms and the recorded-bounds flag
// (docs/spec/16 M7, ADR-0064: the flag is a wall-clock diagnostic, never a
// verdict).

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

func sumNS(n int, p50, p95, mean, max time.Duration) pipeline.LatencySummary {
	return pipeline.LatencySummary{
		Count: uint64(n),
		P50:   p50,
		P95:   p95,
		Mean:  mean,
		Max:   max,
	}
}

// TestMergeLatencyEnvelopePinsTheMergeRule: count sums, max is the largest
// exact max, mean is the count-weighted mean, and p50/p95 take the LARGEST
// per-sample reading — the per-sample envelope, an upper bound on the merged
// percentile, never an optimistic (low-side) one.
func TestMergeLatencyEnvelope(t *testing.T) {
	perSample := []pipeline.Latency{
		{
			FrameToOCR:       sumNS(10, 2*time.Millisecond, 5*time.Millisecond, 3*time.Millisecond, 9*time.Millisecond),
			OCRToRanked:      sumNS(10, 1*time.Millisecond, 2*time.Millisecond, 1*time.Millisecond, 4*time.Millisecond),
			FrameToCommitted: sumNS(5, 5*time.Millisecond, 10*time.Millisecond, 6*time.Millisecond, 15*time.Millisecond),
		},
		{
			// A sample with no OCR-stage samples contributes nothing to that
			// stage (count 0), which the merge must not dilute.
			FrameToOCR:       pipeline.LatencySummary{},
			OCRToRanked:      sumNS(20, 5*time.Millisecond, 10*time.Millisecond, 7*time.Millisecond, 20*time.Millisecond),
			FrameToCommitted: sumNS(5, 10*time.Millisecond, 20*time.Millisecond, 12*time.Millisecond, 25*time.Millisecond),
		},
	}
	got := mergeLatency(perSample)

	f2o := got.FrameToOCR
	if f2o.Count != 10 || f2o.P50 != 2*time.Millisecond || f2o.P95 != 5*time.Millisecond || f2o.Max != 9*time.Millisecond {
		t.Fatalf("frame_to_ocr merge = %+v, want count 10, p50 2 ms, p95 5 ms, max 9 ms (the one sample that had it)", f2o)
	}
	o2r := got.OCRToRanked
	if o2r.Count != 30 || o2r.P50 != 5*time.Millisecond || o2r.P95 != 10*time.Millisecond || o2r.Max != 20*time.Millisecond {
		t.Fatalf("ocr_to_ranked merge = %+v, want the envelope of the two samples", o2r)
	}
	// Weighted mean: (10×1 ms + 20×7 ms) / 30 = 5 ms.
	if o2r.Mean != 5*time.Millisecond {
		t.Fatalf("ocr_to_ranked mean = %v, want 5 ms (count-weighted)", o2r.Mean)
	}
	e2e := got.FrameToCommitted
	if e2e.Count != 10 || e2e.P95 != 20*time.Millisecond || e2e.Max != 25*time.Millisecond {
		t.Fatalf("frame_to_committed merge = %+v, want count 10, p95 20 ms, max 25 ms", e2e)
	}
}

// TestMergeLatencyEmptyIsZero: a run with no samples anywhere merges to the
// zero summary (every stage count 0), which the shared function then reports
// as "nothing measured" instead of dropping the stage rows.
func TestMergeLatencyEmptyIsZero(t *testing.T) {
	got := mergeLatency(nil)
	if got.FrameToOCR.Count != 0 || got.OCRToRanked.Count != 0 || got.FrameToCommitted.Count != 0 {
		t.Fatalf("empty merge = %+v, want all zero", got)
	}
}

// writeBoundsFile writes one recorded-bounds file and returns its path.
func writeBoundsFile(t *testing.T, ms map[string]float64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bounds.json")
	b := LatencyBoundsFile{
		Note:       "test bounds",
		MeasuredAt: "test-run",
		BoundsMS:   ms,
	}
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// slowOCRSummary is the synthetic run behind the bounds tests: a measured
// OCR stage (p95 1000 ms), a fast ranking stage and a 2000 ms end-to-end —
// the shape the slow-OCR unit test in internal/runtime asserts.
func slowOCRSummary() runtime.BottleneckSummary {
	return runtime.Bottlenecks(pipeline.Latency{
		FrameToOCR:       sumNS(100, 50*time.Millisecond, 1000*time.Millisecond, 200*time.Millisecond, 4000*time.Millisecond),
		OCRToRanked:      sumNS(100, 5*time.Millisecond, 20*time.Millisecond, 10*time.Millisecond, 50*time.Millisecond),
		FrameToCommitted: sumNS(100, 100*time.Millisecond, 2000*time.Millisecond, 300*time.Millisecond, 5000*time.Millisecond),
	})
}

// TestCheckLatencyBoundsFlagsTheExceededStage: the OCR stage exceeds its
// recorded bound and the ranking stage stays inside — exactly one flag, on
// the right stage, with the note that says what to do about it.
func TestCheckLatencyBoundsFlagsTheExceededStage(t *testing.T) {
	path := writeBoundsFile(t, map[string]float64{
		"frame_to_ocr":       500,
		"ocr_to_ranked":      100,
		"frame_to_committed": 5000,
	})
	checks, err := CheckLatencyBounds(slowOCRSummary(), path)
	if err != nil {
		t.Fatalf("CheckLatencyBounds: %v", err)
	}
	if len(checks) != 3 {
		t.Fatalf("checks = %d rows, want 3 (every sampled stage with a recorded bound)", len(checks))
	}
	var flagged []string
	for _, c := range checks {
		wantExceeded := c.Stage == "frame_to_ocr" // 1000 ms p95 over a 500 ms bound
		if c.Exceeded != wantExceeded {
			t.Fatalf("stage %s exceeded = %v, want %v (p95 %v ms vs bound %v ms)",
				c.Stage, c.Exceeded, wantExceeded, c.P95MS, c.BoundMS)
		}
		if c.Exceeded {
			flagged = append(flagged, c.Stage)
			if c.Note == "" {
				t.Errorf("exceeded stage %s has no note: the flag must say what to do about it", c.Stage)
			}
		}
	}
	if len(flagged) != 1 || flagged[0] != "frame_to_ocr" {
		t.Fatalf("flagged stages = %v, want exactly [frame_to_ocr]", flagged)
	}
}

// TestRenderMarkdownNamesTheExceededStage: the report.md a human reads must
// carry the flag — the section, the table row and the named line with the
// note — so the guard's visible artifact is asserted without a loaded host
// (the e2e run races the other -race packages under the unit gate).
func TestRenderMarkdownNamesTheExceededStage(t *testing.T) {
	path := writeBoundsFile(t, map[string]float64{
		"frame_to_ocr":       500,
		"ocr_to_ranked":      100,
		"frame_to_committed": 5000,
	})
	checks, err := CheckLatencyBounds(slowOCRSummary(), path)
	if err != nil {
		t.Fatalf("CheckLatencyBounds: %v", err)
	}
	rep := &Report{
		SchemaVersion: SchemaVersion,
		RunID:         "test-run",
		Set:           "test-set",
		Split:         "all",
		Origin:        "synthetic",
		Class:         ClassSynthetic,
		Samples:       100,
		Bottlenecks:   slowOCRSummary(),
		LatencyBounds: checks,
		GeneratedAt:   "test",
	}
	md := RenderMarkdown(rep, nil)
	if !strings.Contains(md, "## Latency bounds") {
		t.Fatalf("report.md has no latency-bounds section")
	}
	if !strings.Contains(md, "`frame_to_ocr` exceeds its recorded bound") {
		t.Errorf("report.md does not name the exceeded stage frame_to_ocr:\n%s", md)
	}
	if strings.Contains(md, "`ocr_to_ranked` exceeds its recorded bound") {
		t.Errorf("report.md flags ocr_to_ranked, which stayed inside its bound")
	}
}

// TestCheckLatencyBoundsNoFileIsNoCheck: a missing bounds file is an honest
// absence (no checks, no error, no invented pass), and a corrupt one is a
// harness defect (the error, not a silent skip).
func TestCheckLatencyBoundsNoFileIsNoCheck(t *testing.T) {
	checks, err := CheckLatencyBounds(slowOCRSummary(), filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing bounds file answered an error, want a clean no-check: %v", err)
	}
	if checks != nil {
		t.Fatalf("missing bounds file answered %d checks, want none", len(checks))
	}
	corrupt := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{ not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := CheckLatencyBounds(slowOCRSummary(), corrupt); err == nil {
		t.Fatal("a corrupt bounds file passed silently; it must fail the run loudly")
	}
}

// TestCheckLatencyBoundsSkipsUnsampledAndUnrecordedStages: a stage with no
// samples is not checked (nothing to judge), and a stage with samples but no
// recorded bound is not checked either (not checked is not a pass).
func TestCheckLatencyBoundsSkipsUnsampledAndUnrecordedStages(t *testing.T) {
	sum := runtime.Bottlenecks(pipeline.Latency{
		// Only the OCR stage and the end-to-end stage have samples.
		FrameToOCR:       sumNS(10, 2*time.Millisecond, 5*time.Millisecond, 3*time.Millisecond, 9*time.Millisecond),
		FrameToCommitted: sumNS(10, 5*time.Millisecond, 10*time.Millisecond, 6*time.Millisecond, 15*time.Millisecond),
	})
	// A bound for the ranking stage (no samples) and none for the OCR stage
	// (samples, no bound): the only checkable stage is the end-to-end one.
	path := writeBoundsFile(t, map[string]float64{"ocr_to_ranked": 100, "frame_to_committed": 1000})
	checks, err := CheckLatencyBounds(sum, path)
	if err != nil {
		t.Fatalf("CheckLatencyBounds: %v", err)
	}
	if len(checks) != 1 || checks[0].Stage != "frame_to_committed" {
		t.Fatalf("checks = %+v, want exactly the sampled-and-recorded stage", checks)
	}
}
