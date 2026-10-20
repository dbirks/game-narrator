// Package runtime — the per-stage bottleneck summary (docs/spec/16 M7:
// "diagnostics expose bottlenecks"; docs/spec/05 "Timings and benchmarks").
//
// The pipeline measures three stage intervals on wall clocks and reports them
// bucketed (internal/pipeline/stats.go): capture→OCR, OCR→ranked and
// capture→committed (the end-to-end figure). Bottlenecks reduces one run's
// own stage stats to the artifact every surface reads: p50/p95/max per stage,
// each stage's share of the end-to-end latency at the p95 point, and the name
// of the slowest stage. It is the single call site for that arithmetic, so
// the developer API, the support bundle, the developer page, the bench report
// and the soak report can never disagree about which stage is the bottleneck.
//
// What it deliberately is not: a verdict. The stage intervals are wall-clock
// (host-load dependent, the same class as the bench's duration and CPU
// figures, ADR-0064), so a surface that grades on them must say so, and the
// bench's recorded bounds are a flag with headroom, never a gate.
package runtime

import (
	"github.com/dbirks/game-narrator/internal/pipeline"
)

// The three stage names the pipeline measures. They are the json names of
// pipeline.Latency's fields: a report row, the API view and the openapi enum
// all spell the stage the same way.
const (
	StageFrameToOCR       = "frame_to_ocr"       // capture → OCR ready
	StageOCRToRanked      = "ocr_to_ranked"      // OCR ready → ranking applied
	StageFrameToCommitted = "frame_to_committed" // capture → commit (end-to-end)
)

// StageLabels is the human rendering of each stage, in pipeline order.
var StageLabels = map[string]string{
	StageFrameToOCR:       "capture → OCR",
	StageOCRToRanked:      "OCR → rank",
	StageFrameToCommitted: "capture → committed (end-to-end)",
}

// stageOrder is the fixed report order: the two proper sub-stages first, the
// end-to-end stage last (it is the baseline the shares are measured against).
var stageOrder = []string{StageFrameToOCR, StageOCRToRanked, StageFrameToCommitted}

// StageLatency is one measured stage of one run's own latency stats.
type StageLatency struct {
	// Stage is one of the three stage names (frame_to_ocr, ocr_to_ranked,
	// frame_to_committed).
	Stage string `json:"stage"`
	// Count is how many samples the stage's histogram held.
	Count uint64 `json:"count"`
	// P50NS, P95NS and MaxNS are the stage's p50, p95 and maximum, in
	// nanoseconds. P50NS and P95NS are histogram bucket edges (the pipeline
	// buckets by design, internal/pipeline/stats.go), so they are the
	// reported edge, not an invented exact value.
	P50NS int64 `json:"p50_ns"`
	P95NS int64 `json:"p95_ns"`
	MaxNS int64 `json:"max_ns"`
	// ShareOfEndToEnd is p95(stage) / p95(frame_to_committed): how much of
	// the end-to-end latency the stage occupies at the p95 point. 1 for the
	// end-to-end stage itself (when it has samples), 0 when the stage or the
	// end-to-end stage has no samples. It is the ratio of two bucket edges
	// over two different sample populations (the stage's own samples and the
	// commit's), so for sub-millisecond intervals the quantized edges can put
	// it above 1 — the honest reading is "this stage's p95 edge is above the
	// end-to-end p95 edge", and the report says that instead of clamping a
	// measurement to a prettier number.
	ShareOfEndToEnd float64 `json:"share_of_end_to_end"`
}

// BottleneckSummary is one run's per-stage latency summary: every measured
// stage with its p50/p95/max and its share of the end-to-end latency, plus
// the name of the slowest stage. The audio output fields are present only
// when the caller attached them (WithAudio): a bench or soak report that has
// no audio counters omits them rather than printing zero-rows that read as
// "measured and empty".
type BottleneckSummary struct {
	// Stages is one row per measured stage, in pipeline order (the
	// end-to-end stage last).
	Stages []StageLatency `json:"stages"`
	// SlowestStage is the proper sub-stage (capture→OCR or OCR→rank) with
	// the largest p95: the stage that owns the most of the end-to-end
	// latency. The end-to-end stage is the baseline, never a candidate —
	// it contains both sub-stages and would otherwise always win. When no
	// sub-stage has samples but the end-to-end stage does, it is named
	// instead (the whole run is one stage then). "" when no stage has
	// samples. Ties resolve to the earlier stage in pipeline order.
	SlowestStage string `json:"slowest_stage"`
	// AudioUnderruns and AudioOutputLatency are the run's audio output
	// counters (ADR-0074, docs/spec/13 "playback underruns"): the started
	// playbacks that ended in a sink failure, and the sink-accepted →
	// device-first-sample latency table. Attached with WithAudio; omitted
	// (zero values) when the caller has no audio counters to report.
	AudioUnderruns     uint64                  `json:"audio_underruns,omitempty"`
	AudioOutputLatency pipeline.LatencySummary `json:"audio_output_latency,omitempty"`
}

// WithAudio attaches the run's audio output counters to the summary
// (ADR-0074): the underrun count and the output-latency table. The stage
// rows are untouched — audio is the device's half of the pipeline, not a
// stage the capture→commit chain measures.
func (s BottleneckSummary) WithAudio(underruns uint64, outLatency pipeline.LatencySummary) BottleneckSummary {
	s.AudioUnderruns = underruns
	s.AudioOutputLatency = outLatency
	return s
}

// Bottlenecks computes the summary from one run's own stage stats — the
// pipeline's three measured intervals. All three stage rows are always
// present (a stage with no samples reads count 0 and zero latencies), so a
// report can show "this stage was never measured" instead of omitting it.
func Bottlenecks(lat pipeline.Latency) BottleneckSummary {
	byName := map[string]pipeline.LatencySummary{
		StageFrameToOCR:       lat.FrameToOCR,
		StageOCRToRanked:      lat.OCRToRanked,
		StageFrameToCommitted: lat.FrameToCommitted,
	}
	e2e := byName[StageFrameToCommitted]

	stages := make([]StageLatency, 0, len(stageOrder))
	for _, name := range stageOrder {
		s := byName[name]
		st := StageLatency{
			Stage: name,
			Count: s.Count,
			P50NS: s.P50.Nanoseconds(),
			P95NS: s.P95.Nanoseconds(),
			MaxNS: s.Max.Nanoseconds(),
		}
		switch {
		case name == StageFrameToCommitted && s.Count > 0:
			// The baseline names itself.
			st.ShareOfEndToEnd = 1
		case s.Count > 0 && e2e.Count > 0 && e2e.P95.Nanoseconds() > 0:
			st.ShareOfEndToEnd = float64(s.P95.Nanoseconds()) / float64(e2e.P95.Nanoseconds())
		}
		stages = append(stages, st)
	}

	// The slowest proper sub-stage by p95; the earlier stage wins a tie.
	slowest := ""
	for _, name := range []string{StageFrameToOCR, StageOCRToRanked} {
		s := byName[name]
		if s.Count == 0 {
			continue
		}
		if slowest == "" || s.P95.Nanoseconds() > byName[slowest].P95.Nanoseconds() {
			slowest = name
		}
	}
	if slowest == "" && e2e.Count > 0 {
		// No sub-stage was measured (for example a run whose regions never
		// needed OCR): the end-to-end stage is the whole run, so it is the
		// answer, named honestly.
		slowest = StageFrameToCommitted
	}

	return BottleneckSummary{Stages: stages, SlowestStage: slowest}
}

// EndToEnd returns the end-to-end stage row of the summary (the
// frame_to_committed row). It always has a row; callers check Count.
func (s BottleneckSummary) EndToEnd() StageLatency {
	for i := range s.Stages {
		if s.Stages[i].Stage == StageFrameToCommitted {
			return s.Stages[i]
		}
	}
	return StageLatency{}
}

// Stage returns the named stage row, or a zero row when the name is not one
// of the three measured stages.
func (s BottleneckSummary) Stage(name string) StageLatency {
	for i := range s.Stages {
		if s.Stages[i].Stage == name {
			return s.Stages[i]
		}
	}
	return StageLatency{}
}
