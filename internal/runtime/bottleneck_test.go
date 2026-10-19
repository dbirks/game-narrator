package runtime

import (
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
)

// sum makes one stage's bucketed summary the way the pipeline reports it:
// the p50/p95 values are histogram bucket edges (1, 2, 5, …, 5000 ms), so the
// tests name edges, not invented exact values; max is exact (not bucketed).
func sum(count int, p50, p95, max time.Duration) pipeline.LatencySummary {
	return pipeline.LatencySummary{Count: uint64(count), P50: p50, P95: p95, Max: max}
}

// TestBottlenecksNamesTheSlowOCRIstage is the load-bearing unit test: a
// synthetic run whose OCR stage is slow must read "the bottleneck is OCR",
// not "the bottleneck is the run" (the end-to-end stage contains OCR by
// construction and must never be the answer when a sub-stage was measured).
func TestBottlenecksNamesTheSlowOCRStage(t *testing.T) {
	lat := pipeline.Latency{
		FrameToOCR:       sum(100, 50*time.Millisecond, 1000*time.Millisecond, 4000*time.Millisecond),
		OCRToRanked:      sum(100, 5*time.Millisecond, 20*time.Millisecond, 50*time.Millisecond),
		FrameToCommitted: sum(100, 100*time.Millisecond, 2000*time.Millisecond, 5000*time.Millisecond),
	}
	got := Bottlenecks(lat)

	if got.SlowestStage != StageFrameToOCR {
		t.Fatalf("slowest stage = %q, want %q: a slow OCR stage must name OCR", got.SlowestStage, StageFrameToOCR)
	}
	// The slow stage owns the share of the end-to-end its p95 implies:
	// 1000 ms of a 2000 ms end-to-end p95 is half.
	ocr := got.Stage(StageFrameToOCR)
	if ocr.Count != 100 || ocr.P95NS != int64(1000*time.Millisecond) {
		t.Fatalf("ocr stage row = %+v, want count 100 and p95 1000 ms", ocr)
	}
	if ocr.ShareOfEndToEnd <= 0 || ocr.ShareOfEndToEnd > 1 {
		t.Fatalf("ocr share = %v, want within (0, 1]", ocr.ShareOfEndToEnd)
	}
	if want := 1000.0 / 2000.0; ocr.ShareOfEndToEnd < want-1e-9 || ocr.ShareOfEndToEnd > want+1e-9 {
		t.Fatalf("ocr share = %v, want %v (p95 over end-to-end p95)", ocr.ShareOfEndToEnd, want)
	}
	ranked := got.Stage(StageOCRToRanked)
	if ranked.ShareOfEndToEnd > ocr.ShareOfEndToEnd {
		t.Fatalf("ranked share %v exceeds the slow ocr share %v", ranked.ShareOfEndToEnd, ocr.ShareOfEndToEnd)
	}
	e2e := got.EndToEnd()
	if e2e.Count != 100 || e2e.ShareOfEndToEnd != 1 {
		t.Fatalf("end-to-end row = %+v, want count 100 and share 1", e2e)
	}
}

// TestBottlenecksNamesTheSlowRankingStage is the mirror case: OCR fast, the
// ranking slow — the named stage is the ranking, and the OCR stage's share
// cannot exceed it.
func TestBottlenecksNamesTheSlowRankingStage(t *testing.T) {
	lat := pipeline.Latency{
		FrameToOCR:       sum(40, 2*time.Millisecond, 10*time.Millisecond, 30*time.Millisecond),
		OCRToRanked:      sum(40, 40*time.Millisecond, 400*time.Millisecond, 900*time.Millisecond),
		FrameToCommitted: sum(40, 60*time.Millisecond, 600*time.Millisecond, 1000*time.Millisecond),
	}
	got := Bottlenecks(lat)
	if got.SlowestStage != StageOCRToRanked {
		t.Fatalf("slowest stage = %q, want %q", got.SlowestStage, StageOCRToRanked)
	}
	if got.Stage(StageOCRToRanked).ShareOfEndToEnd < got.Stage(StageFrameToOCR).ShareOfEndToEnd {
		t.Fatal("the slow stage's share must be at least the fast stage's")
	}
}

// TestBottlenecksTieResolvesToPipelineOrder pins the tie rule: equal p95s name
// the earlier stage in pipeline order, so a rerun of the same numbers reads
// the same answer.
func TestBottlenecksTieResolvesToPipelineOrder(t *testing.T) {
	both := sum(10, 10*time.Millisecond, 100*time.Millisecond, 200*time.Millisecond)
	got := Bottlenecks(pipeline.Latency{
		FrameToOCR:       both,
		OCRToRanked:      both,
		FrameToCommitted: sum(10, 20*time.Millisecond, 150*time.Millisecond, 250*time.Millisecond),
	})
	if got.SlowestStage != StageFrameToOCR {
		t.Fatalf("slowest stage = %q, want %q (pipeline-order tie)", got.SlowestStage, StageFrameToOCR)
	}
}

// TestBottlenecksEmptyStats is the no-session case: every stage reads count 0,
// no stage is named and the end-to-end share is 0 — an honest "nothing was
// measured", never a fabricated winner.
func TestBottlenecksEmptyStats(t *testing.T) {
	got := Bottlenecks(pipeline.Latency{})
	if len(got.Stages) != 3 {
		t.Fatalf("stages = %d rows, want 3 (every measured stage is reported)", len(got.Stages))
	}
	for _, s := range got.Stages {
		if s.Count != 0 || s.P50NS != 0 || s.P95NS != 0 || s.MaxNS != 0 || s.ShareOfEndToEnd != 0 {
			t.Fatalf("stage %s = %+v, want all zeros for unmeasured stats", s.Stage, s)
		}
	}
	if got.SlowestStage != "" {
		t.Fatalf("slowest stage = %q, want empty (no samples)", got.SlowestStage)
	}
}

// TestBottlenecksOnlyEndToEndNamesEndToEnd: a run that committed without
// measuring a sub-stage (every region skipped OCR by hash) still names the
// stage it did measure — the end-to-end stage, the whole run.
func TestBottlenecksOnlyEndToEndNamesEndToEnd(t *testing.T) {
	got := Bottlenecks(pipeline.Latency{
		FrameToCommitted: sum(5, 50*time.Millisecond, 120*time.Millisecond, 300*time.Millisecond),
	})
	if got.SlowestStage != StageFrameToCommitted {
		t.Fatalf("slowest stage = %q, want %q", got.SlowestStage, StageFrameToCommitted)
	}
	if got.EndToEnd().ShareOfEndToEnd != 1 {
		t.Fatalf("end-to-end share = %v, want 1", got.EndToEnd().ShareOfEndToEnd)
	}
}

// TestBottlenecksQuantizedShareMayExceedOne pins the honest-ratio rule: the
// stage's and the end-to-end's p95s are bucket edges over different sample
// populations, so a sub-ms run whose stage edge lands above the end-to-end
// edge reports a share above 1 — the measurement, not a clamped prettier one.
func TestBottlenecksQuantizedShareMayExceedOne(t *testing.T) {
	got := Bottlenecks(pipeline.Latency{
		FrameToOCR:       sum(200, 2*time.Millisecond, 10*time.Millisecond, 30*time.Millisecond),
		OCRToRanked:      sum(200, 2*time.Millisecond, 5*time.Millisecond, 10*time.Millisecond),
		FrameToCommitted: sum(200, 2*time.Millisecond, 5*time.Millisecond, 40*time.Millisecond),
	})
	if got.Stage(StageFrameToOCR).ShareOfEndToEnd != 2 {
		t.Fatalf("ocr share = %v, want 2 (10 ms edge over a 5 ms edge, unclamped)", got.Stage(StageFrameToOCR).ShareOfEndToEnd)
	}
	if got.SlowestStage != StageFrameToOCR {
		t.Fatalf("slowest stage = %q, want %q", got.SlowestStage, StageFrameToOCR)
	}
}

// TestBottlenecksZeroEndToEndP95KeepsSharesZero: a non-empty end-to-end stage
// whose p95 edge is 0 (every sample under the smallest bucket edge) cannot
// divide a share, so the sub-stages keep share 0 instead of reporting NaN or
// +Inf.
func TestBottlenecksZeroEndToEndP95KeepsSharesZero(t *testing.T) {
	got := Bottlenecks(pipeline.Latency{
		FrameToOCR:       sum(3, 0, 0, time.Microsecond),
		OCRToRanked:      sum(3, 0, 0, 2*time.Microsecond),
		FrameToCommitted: sum(3, 0, 0, 3*time.Microsecond),
	})
	if got.SlowestStage != StageFrameToOCR {
		t.Fatalf("slowest stage = %q, want %q (the only non-zero max stage still loses to pipeline order on a p95 tie)", got.SlowestStage, StageFrameToOCR)
	}
	for _, s := range got.Stages {
		if s.Stage == StageFrameToCommitted {
			continue
		}
		if s.ShareOfEndToEnd != 0 {
			t.Fatalf("stage %s share = %v, want 0 (end-to-end p95 is 0)", s.Stage, s.ShareOfEndToEnd)
		}
	}
}
