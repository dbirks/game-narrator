// report.go writes the soak's evidence: report.json (the full machine-readable
// result) and report.md (the summary a reviewer reads first), under
// artifacts/soak/<run-id>/. The report is written even when the run failed —
// a failed soak's numbers are the evidence (AGENTS.md "first reproduce"), and
// the provenance is stated on the page: the frames are generated_synthetic.

package soak

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/runtime"
)

// newRunID is the run's label and directory name: the UTC stamp plus the mode.
func newRunID(mode string) string {
	return time.Now().UTC().Format("20060102-150405") + "-" + mode
}

// writeReport writes the JSON and markdown reports and returns the directory
// it wrote to. On write failure the directory is returned and the error
// carried on the Result (a report that cannot be written is itself a finding).
func writeReport(r *Result) string {
	dir := r.Config.OutDir
	if dir == "" {
		dir = filepath.Join(r.Config.RepoRoot, "artifacts", "soak")
	}
	dir = filepath.Join(dir, r.RunID)

	err := os.MkdirAll(dir, 0o755)
	if err != nil {
		r.ReportErr = fmt.Sprintf("report: mkdir %s: %v", dir, err)
		return dir
	}
	if err := writeJSON(dir, r); err != nil {
		r.ReportErr = err.Error()
	}
	if err := writeMarkdown(dir, r); err != nil {
		r.ReportErr = appendErr(r.ReportErr, err.Error())
	}
	return dir
}

// writeJSON is the machine-readable report.
func writeJSON(dir string, r *Result) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("report: json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("report: write json: %v", err)
	}
	return nil
}

// writeMarkdown is the human report: the verdict table, the resource table
// (every sample), the fault table and the per-kind counters.
func writeMarkdown(dir string, r *Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# Soak run %s\n\n", r.RunID)
	fmt.Fprintf(&b, "- **Mode:** %s (seed %d)\n", r.Mode, r.Seed)
	fmt.Fprintf(&b, "- **Duration:** requested %s, wall %s\n", r.Config.Duration, r.DurationWall.Round(time.Second))
	fmt.Fprintf(&b, "- **Clip(s):** %s @ speed %g\n", strings.Join(r.Config.Clips, ", "), r.Config.Speed)
	fmt.Fprintf(&b, "- **Profile:** %s (origin: generated_synthetic — fixture, not real game coverage)\n", r.Config.Profile)
	fmt.Fprintf(&b, "- **Verdict:** %s\n\n", verdictWord(r))

	b.WriteString("## Pass criteria\n\n")
	b.WriteString("| Criterion | Result | Detail |\n| --- | --- | --- |\n")
	for _, v := range r.Verdicts {
		res := "PASS"
		if !v.Passed {
			res = "FAIL"
		}
		fmt.Fprintf(&b, "| %s | %s | %s |\n", v.Name, res, v.Detail)
	}

	b.WriteString("\n## Resources\n\n")
	fmt.Fprintf(&b, "Baseline (pre-run): RSS %d KiB, goroutines %d, FDs %d\n", r.Baseline.RSSKB, r.Baseline.Goroutines, r.Baseline.FDs)
	fmt.Fprintf(&b, "Final (post-teardown): RSS %d KiB, goroutines %d, FDs %d\n", r.Final.RSSKB, r.Final.Goroutines, r.Final.FDs)
	fmt.Fprintf(&b, "RSS %s over the post-warm-up window; slope %.2f MiB/min.\n\n", r.rssGrowth(), r.rssSlopeMiBPerMin())
	b.WriteString("| t (s) | RSS (MiB) | goroutines | FDs | commits | frames in | frames dropped | speech started/ended | provider errors | faults | panics |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |\n")
	t0 := r.Baseline.T
	for _, s := range r.Samples {
		fmt.Fprintf(&b, "| %.0f | %.1f | %d | %d | %d | %d | %d | %d/%d | %d | %d | %d |\n",
			s.T.Sub(t0).Seconds(), float64(s.RSSKB)/1024, s.Goroutines, s.FDs, s.Commits,
			s.FramesDelivered, s.FramesDropped, s.SpeechStarted, s.SpeechEnded,
			s.ProviderErrors, s.FaultsFired, s.RecoveredPanics)
	}

	b.WriteString("\n## Faults\n\n")
	if len(r.FaultEvents) == 0 {
		b.WriteString("No faults injected (clean replay run).\n")
	} else {
		fmt.Fprintf(&b, "%d fault activation(s): %s\n\n", len(r.FaultEvents), faultKindsLine(r.Faults))
		b.WriteString("| t (s) | kind | role | detail | commits at fault | recovered | recovery (ms) |\n")
		b.WriteString("| --- | --- | --- | --- | --- | --- | --- |\n")
		for _, ev := range r.FaultEvents {
			recovered := yesno(ev.Recovered)
			if !ev.Judged {
				// Open window at run end: the run's stop froze the commit
				// counter, so the fault is reported, not graded.
				recovered = "open window"
			}
			fmt.Fprintf(&b, "| %.1f | %s | %s | %s | %d | %s | %d |\n",
				ev.Wall.Sub(t0).Seconds(), ev.Kind, ev.Role, ev.Detail, ev.CommitsAtFire,
				recovered, ev.WaitMS)
		}
	}

	b.WriteString("\n## Provider errors by kind\n\n")
	if len(r.ProviderErrors) == 0 {
		b.WriteString("None.\n")
	} else {
		b.WriteString(faultKindsLine(r.ProviderErrors) + "\n")
	}

	b.WriteString("\n## Counters\n\n")
	fmt.Fprintf(&b, "- pipeline commits: %d (sidecar commits: %d)\n", r.Pipeline.Commits, sidecarCommitsOf(r))
	fmt.Fprintf(&b, "- pipeline frames delivered/dropped: %d / %d (source chaos drops: %d)\n",
		r.Pipeline.FramesDelivered, r.Pipeline.FramesDropped, r.SourceDrops)
	fmt.Fprintf(&b, "- session fences: stale %d, duplicated %d, rejected %d, cancelled %d, commits %d\n",
		r.Session.Stale, r.Session.Duplicated, r.Session.RejectedState, r.Session.Cancellations, r.Session.Commits)
	fmt.Fprintf(&b, "- stale-generation applied (must stay 0): %d\n", r.Pipeline.StaleApplied)
	fmt.Fprintf(&b, "- replay degraded frames (must stay 0): %d\n", r.Pipeline.ReplayDegradedFrames)

	b.WriteString("\n## Per-stage latency (bottleneck summary)\n\n")
	writeBottleneckSummary(&b, r.Bottlenecks)
	if len(r.PassFailures) > 0 {
		b.WriteString("\n## Pass failures\n\n")
		for _, f := range r.PassFailures {
			fmt.Fprintf(&b, "- %s\n", f)
		}
	}
	if r.RunErr != "" {
		fmt.Fprintf(&b, "\n**Run error:** %s\n", r.RunErr)
	}

	if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte(b.String()), 0o644); err != nil {
		return fmt.Errorf("report: write markdown: %v", err)
	}
	return nil
}

// writeBottleneckSummary is the report's per-stage latency section: the run's
// own stage intervals reduced by the shared summary function (the one the
// session API, the support bundle and the developer page use), with the named
// slowest stage (docs/spec/16 M7). Wall-clock diagnostic, never a pass
// criterion: the pass table above is the verdict (ADR-0064's class of number).
func writeBottleneckSummary(b *strings.Builder, s runtime.BottleneckSummary) {
	unmeasured := true
	for _, st := range s.Stages {
		if st.Count > 0 {
			unmeasured = false
			break
		}
	}
	if unmeasured {
		b.WriteString("No stage was measured in this run (no OCR, no ranking, no commit).\n")
		return
	}
	if s.SlowestStage != "" {
		fmt.Fprintf(b, "**Slowest stage: %s (`%s`)**\n\n", runtime.StageLabels[s.SlowestStage], s.SlowestStage)
	}
	b.WriteString("| stage | samples | p50 | p95 | max | share of end-to-end (p95) |\n")
	b.WriteString("| --- | --- | --- | --- | --- | --- |\n")
	for _, st := range s.Stages {
		fmt.Fprintf(b, "| %s (`%s`) | %d | %s | %s | %s | %.2f |\n",
			runtime.StageLabels[st.Stage], st.Stage, st.Count,
			msCell(st.P50NS), msCell(st.P95NS), msCell(st.MaxNS), st.ShareOfEndToEnd)
	}
	b.WriteString("\nWall-clock stage intervals measured by the pipeline itself (the same numbers the session's stats carry). A share above 1 is the honest ratio of two histogram bucket edges over different sample populations (sub-millisecond intervals), not a clamped measurement.\n")
}

// msCell renders one nanosecond value as a millisecond cell: whole-millisecond
// edges (the histogram edges) as integers, an exact value that is not a whole
// millisecond with one decimal — the report never pretends to a finer
// precision than the measurement has.
func msCell(ns int64) string {
	if ns%1_000_000 == 0 {
		return fmt.Sprintf("%d", ns/1_000_000)
	}
	return fmt.Sprintf("%.1f", float64(ns)/1e6)
}

// verdictWord is the header's one word.
func verdictWord(r *Result) string {
	if r.Passed() {
		return "PASS"
	}
	return "FAIL"
}

// rssGrowth is the summary-line growth figure over the post-warm-up window.
func (r *Result) rssGrowth() string {
	pts := r.postWarmupSamples()
	if len(pts) < 2 || pts[0].RSSKB == 0 {
		return "n/a"
	}
	last := pts[len(pts)-1].RSSKB
	pct := float64(last-pts[0].RSSKB) / float64(pts[0].RSSKB) * 100
	return fmt.Sprintf("%+.1f%%", pct)
}

// rssSlopeMiBPerMin is the summary-line slope figure (0 when the fit is not
// runnable: fewer than three post-warm-up samples).
func (r *Result) rssSlopeMiBPerMin() float64 {
	pts := r.postWarmupSamples()
	snaps := make([]snap, 0, len(pts))
	for _, p := range pts {
		snaps = append(snaps, snap{t: p.T, rss: p.RSSKB})
	}
	return leakSlopeMiBPerMin(snaps)
}

// faultKindsLine renders a kind->count map as "kind×count, …" (or "none").
func faultKindsLine(m map[string]int64) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sortStrings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s×%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "NO"
}

// sidecarCommitsOf reads the sidecar's commit total from the last sample (the
// report's counter table carries it; the Result keeps the pipeline's as its
// commits of record).
func sidecarCommitsOf(r *Result) int64 {
	if len(r.Samples) == 0 {
		return 0
	}
	return r.Samples[len(r.Samples)-1].SidecarCommits
}
