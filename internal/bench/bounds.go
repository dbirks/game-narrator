// Package bench — the per-stage bottleneck summary for a bench run and its
// recorded p95 bounds (docs/spec/16 M7 "diagnostics expose bottlenecks").
//
// A bench run is many isolated sample runs, each with its own pipeline and
// its own bucketed stage histograms (internal/pipeline/stats.go). The
// per-stage numbers the report shows are those histograms merged across the
// run's samples with the envelope rule in mergeLatency — the honest merge of
// bucketed summaries — reduced by the one shared summary function
// (runtime.Bottlenecks, the same call the session API, the support bundle and
// the developer page use) so every surface reads the same arithmetic.
//
// The recorded bounds (DefaultBoundsPath) are a *flag*, not a verdict: the
// stage intervals are wall-clock (host-load dependent, ADR-0064), exactly the
// class of number the bench already reports without grading on (duration,
// CPU, peak RSS). A stage whose p95 exceeds its recorded bound is named in
// the report; the bench's capture-clock metrics and verdicts are untouched.
package bench

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// DefaultBoundsPath is the committed per-stage p95 bounds file, relative to
// the working directory: `make bench` and the bench command run from the
// repository root, where the file lives. A missing file degrades to no bound
// checks (the report then carries no latency_bounds section) — the guard is
// an evidence artifact, and a bench run with no recorded bounds still says so
// by absence, never by inventing a pass.
const DefaultBoundsPath = "testdata/bench/latency-bounds.json"

// LatencyBoundsFile is testdata/bench/latency-bounds.json: the wall-clock p95
// bound per stage, recorded from a measured run with headroom (see the file's
// own provenance note). The bounds are a flag with headroom, not a gate:
// ADR-0064 keeps the bench's verdicts load-invariant, and the flag is the
// place a load-dependent diagnostic may live.
type LatencyBoundsFile struct {
	// Note records the provenance: which measured run the bounds come from
	// and the headroom rule applied to them.
	Note string `json:"note"`
	// MeasuredAt names the measured run the bounds were recorded from.
	MeasuredAt string `json:"measured_at"`
	// BoundsMS is the wall-clock p95 bound per stage, in milliseconds.
	BoundsMS map[string]float64 `json:"bounds_ms"`
}

// LatencyBoundCheck is one stage's row of the report's latency_bounds
// section: the run's merged p95 against the recorded bound, and whether the
// run exceeded it. Exceeded is a flag (the report names the stage); it never
// changes a capture-clock metric or a verdict.
type LatencyBoundCheck struct {
	// Stage is one of the three measured stage names.
	Stage string `json:"stage"`
	// P95MS is the run's merged p95 for the stage, in milliseconds (wall
	// clock; the per-sample envelope, see mergeLatency).
	P95MS float64 `json:"p95_ms"`
	// BoundMS is the recorded bound for the stage, in milliseconds.
	BoundMS float64 `json:"bound_ms"`
	// Exceeded is true when P95MS is strictly above BoundMS.
	Exceeded bool `json:"exceeded"`
	// Note is empty on a pass; on an exceedance it names what to do about
	// it (re-record the bound with evidence, or fix the regression).
	Note string `json:"note,omitempty"`
}

// mergeLatency merges one run's per-sample bucketed stage summaries into one
// run-level summary. Bucketed histograms do not merge without loss (the
// pipeline buckets by design — a 4-core appliance does not retain every
// sample of a 30 fps run), so the merge is the documented envelope:
//
//   - count sums,
//   - max is the largest per-sample max (exact: max is not bucketed),
//   - mean is the count-weighted mean of the per-sample means,
//   - p50 and p95 are the LARGEST per-sample p50/p95: the per-sample
//     envelope, a conservative upper bound on the merged population's
//     percentile (never an optimistic one — the direction that would lie is
//     the low side, and this merge cannot take it).
//
// A sample with no samples for a stage contributes nothing to that stage.
func mergeLatency(perSample []pipeline.Latency) pipeline.Latency {
	mergeOne := func(summaries []pipeline.LatencySummary) pipeline.LatencySummary {
		var out pipeline.LatencySummary
		var meanWeighted int64
		for _, s := range summaries {
			if s.Count == 0 {
				continue
			}
			out.Count += s.Count
			meanWeighted += s.Mean.Nanoseconds() * int64(s.Count)
			if s.Max > out.Max {
				out.Max = s.Max
			}
			if s.P50 > out.P50 {
				out.P50 = s.P50
			}
			if s.P95 > out.P95 {
				out.P95 = s.P95
			}
		}
		if out.Count > 0 {
			out.Mean = time.Duration(meanWeighted/int64(out.Count)) * time.Nanosecond
		}
		return out
	}

	var frames, ocrs, commits []pipeline.LatencySummary
	for _, lat := range perSample {
		frames = append(frames, lat.FrameToOCR)
		ocrs = append(ocrs, lat.OCRToRanked)
		commits = append(commits, lat.FrameToCommitted)
	}
	return pipeline.Latency{
		FrameToOCR:       mergeOne(frames),
		OCRToRanked:      mergeOne(ocrs),
		FrameToCommitted: mergeOne(commits),
	}
}

// CheckLatencyBounds reduces the run's shared summary against the recorded
// bounds file. One row per stage that has both samples and a recorded bound:
// a stage missing from the file is not checked (and not reported as passed).
// A missing or unreadable file answers (nil, nil): the run had no recorded
// bounds to be judged against, which the report then shows by absence.
// Read errors other than "file absent" are returned: a bounds file that
// exists but cannot be parsed is a harness defect, not an absence.
func CheckLatencyBounds(sum runtime.BottleneckSummary, boundsPath string) ([]LatencyBoundCheck, error) {
	raw, err := os.ReadFile(boundsPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("bench: read latency bounds %s: %w", boundsPath, err)
	}
	var bounds LatencyBoundsFile
	if err := json.Unmarshal(raw, &bounds); err != nil {
		return nil, fmt.Errorf("bench: parse latency bounds %s: %w", boundsPath, err)
	}
	var checks []LatencyBoundCheck
	for _, stage := range sum.Stages {
		if stage.Count == 0 {
			continue
		}
		bound, ok := bounds.BoundsMS[stage.Stage]
		if !ok {
			continue // no recorded bound for this stage: not checked, not a pass
		}
		p95ms := float64(stage.P95NS) / 1e6
		check := LatencyBoundCheck{
			Stage:    stage.Stage,
			P95MS:    p95ms,
			BoundMS:  bound,
			Exceeded: p95ms > bound,
		}
		if check.Exceeded {
			check.Note = "exceeds the recorded bound (wall-clock p95, per-sample envelope): either a real regression to fix, or host load of the kind ADR-0064 keeps out of the verdicts — re-measure on a quiet host and re-record the bound with evidence before dismissing it"
		}
		checks = append(checks, check)
	}
	return checks, nil
}
