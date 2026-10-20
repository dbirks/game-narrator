package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	stdruntime "runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/pipeline"
	gnrt "github.com/dbirks/game-narrator/internal/runtime"
)

// RunBench runs one set's split end-to-end: render the frames into a temp dir,
// replay every sample through the real pipeline, compute the metrics, and
// write report.json / report.md / manifest.json into outDir. It returns the
// report so tests can assert its shape without re-reading the files.
// boundsPath is the recorded per-stage p95 bounds file to flag against
// ("" takes DefaultBoundsPath; a missing file degrades to no bound checks,
// never to an invented pass — see bounds.go).
func RunBench(ctx context.Context, setDir, split, outDir, boundsPath string) (*Report, error) {
	pinReplayEngineThreads()
	if split != SplitTuning && split != SplitHoldout && split != SplitAll {
		return nil, fmt.Errorf("bench: split must be tuning, holdout or all (got %q)", split)
	}
	ts, err := LoadTruth(setDir)
	if err != nil {
		return nil, err
	}
	prof, err := LoadScriptProfile(setDir)
	if err != nil {
		return nil, err
	}
	benchProfile = prof
	idx, err := prof.BuildIndex()
	if err != nil {
		return nil, fmt.Errorf("bench: matcher index: %v", err)
	}
	regions := prof.BuildRegions()

	var kept []Sample
	for _, s := range ts.Samples {
		if split == SplitAll || s.Split == split {
			kept = append(kept, s)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("bench: no samples in split %q", split)
	}
	runSet := *ts
	runSet.Samples = kept

	start := time.Now()
	cpuBefore := procCPUTime()
	framesDir, err := os.MkdirTemp("", "bench-frames-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(framesDir)
	dirs, err := RenderSamples(&runSet, prof.Layout, framesDir)
	if err != nil {
		return nil, err
	}
	// The samples are independent (fresh session, fresh frame dir), so they run
	// on a bounded worker pool. Each run is deterministic, so concurrency only
	// changes wall time, never the numbers; the CPU figure below is the whole
	// process's cumulative CPU, which is exactly what the benchmark reports.
	workers := replayWorkers(len(kept))
	jobs := make(chan int)
	var (
		mu       sync.Mutex
		notRun   []NotRunSample
		outcomes = make([]sampleOutcomes, len(kept))
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := kept[i]
				out, err := runSample(ctx, dirs[s.ID], "", s, idx, regions)
				if err != nil {
					fmt.Fprintf(os.Stderr, "bench: sample %s NOT RUN: %v\n", s.ID, err)
					mu.Lock()
					notRun = append(notRun, NotRunSample{SampleID: s.ID, Reason: err.Error()})
					mu.Unlock()
					continue
				}
				mu.Lock()
				outcomes[i] = out
				mu.Unlock()
			}
		}()
	}
	for i := range kept {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	// Compact: drop the NOT RUN slots (erroring samples) while keeping order.
	var ok []sampleOutcomes
	for _, o := range outcomes {
		if o.sample.id != "" {
			ok = append(ok, o)
		}
	}
	outcomes = ok
	duration := time.Since(start)
	if len(outcomes) == 0 {
		return nil, fmt.Errorf("bench: no samples completed (tesseract and magick both required; see the NOT RUN lines above)")
	}

	metrics := RunMetrics(outcomes)
	// The per-stage bottleneck summary (docs/spec/16 M7): the run's own
	// per-sample stage histograms merged across the run (the documented
	// envelope rule, bounds.go) and reduced by the one shared function the
	// session API, the support bundle and the developer page use — plus the
	// recorded-bounds flag. Wall-clock diagnostic, never a verdict (ADR-0064).
	perSampleLat := make([]pipeline.Latency, len(outcomes))
	for i := range outcomes {
		perSampleLat[i] = outcomes[i].stats.Latency
	}
	summary := gnrt.Bottlenecks(mergeLatency(perSampleLat))
	if boundsPath == "" {
		boundsPath = DefaultBoundsPath
	}
	bounds, err := CheckLatencyBounds(summary, boundsPath)
	if err != nil {
		return nil, err
	}
	rep := &Report{
		SchemaVersion: SchemaVersion,
		RunID:         time.Now().UTC().Format("20060102T150405Z"),
		Set:           ts.Set,
		Split:         split,
		Origin:        ts.Origin,
		Class:         ClassSynthetic,
		Samples:       len(outcomes),
		Metrics:       metrics,
		Bottlenecks:   summary,
		LatencyBounds: bounds,
		Diagnostics: Diagnostics{
			GoVersion:          stdruntime.Version(),
			Tesseract:          tesseractVersion(),
			OS:                 stdruntime.GOOS,
			SeedBase:           ts.SeedBase,
			SampleIDs:          sampleIDs(outcomes),
			CPUTimeSecs:        procCPUTime() - cpuBefore,
			PeakRSSMB:          procPeakRSSMB(),
			DurationSecs:       duration.Seconds(),
			MatcherCalibration: matcherCalibrationDigest(),
			NotRun:             notRun,
			Top1Misses:         top1Misses(outcomes),
		},
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if h, err := SetHash(filepath.Join(setDir, "truth.json")); err == nil {
		rep.Diagnostics.SetHash = h
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "report.json"), rep); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "report.md"), []byte(RenderMarkdown(rep, outcomes)), 0o644); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(outDir, "manifest.json"), manifestOf(rep)); err != nil {
		return nil, err
	}
	return rep, nil
}

// tesseractVersion is the engine's own first banner line; "unavailable" when
// the binary cannot answer (the run itself would have reported NOT RUN).
func tesseractVersion() string {
	out, err := exec.Command("tesseract", "--version").CombinedOutput()
	if err != nil {
		return "unavailable"
	}
	lines := strings.Split(string(out), "\n")
	v := strings.TrimSpace(lines[0])
	if strings.HasPrefix(v, "tesseract ") {
		v = strings.TrimPrefix(v, "tesseract ")
	}
	return v
}

// manifestOf is the run's identity manifest: the git SHA, Go version,
// tesseract version, OS, seed and set hash the metrics were produced with.
func manifestOf(rep *Report) map[string]any {
	return map[string]any{
		"git_sha":             gitSHA(),
		"go_version":          rep.Diagnostics.GoVersion,
		"tesseract":           rep.Diagnostics.Tesseract,
		"os":                  rep.Diagnostics.OS,
		"seed":                rep.Diagnostics.SeedBase,
		"set_hash":            rep.Diagnostics.SetHash,
		"set":                 rep.Set,
		"split":               rep.Split,
		"origin":              rep.Origin,
		"class":               rep.Class,
		"matcher_calibration": rep.Diagnostics.MatcherCalibration,
	}
}

func gitSHA() string {
	if out, err := exec.Command("git", "rev-parse", "HEAD").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			return s
		}
	}
	return "unknown"
}

// RenderMarkdown is report.md: the metric table with the provisional-objective
// verdict, the per-class breakdown, and the environment. It is the document a
// human reads; report.json is the one a machine re-scores.
func RenderMarkdown(rep *Report, outcomes []sampleOutcomes) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Benchmark report — %s (%s)\n\n", rep.Set, rep.Split)
	fmt.Fprintf(&b, "- origin: **%s** (%s)\n", rep.Origin, classNote(rep.Class))
	fmt.Fprintf(&b, "- run: %s · %d samples · %s · tesseract %s\n", rep.RunID, rep.Samples, rep.GeneratedAt, rep.Diagnostics.Tesseract)
	fmt.Fprintf(&b, "- duration %.1fs (frame generation + replay) · process CPU %.1fs · peak RSS %.0f MB (bench process only; tesseract/magick are subprocesses)\n\n",
		rep.Diagnostics.DurationSecs, rep.Diagnostics.CPUTimeSecs, rep.Diagnostics.PeakRSSMB)
	b.WriteString("| metric | value | n | provisional objective | met |\n")
	b.WriteString("|---|---|---|---|---|\n")
	for _, m := range rep.Metrics {
		obj := m.Objective
		if obj == "" {
			obj = "—"
		}
		fmt.Fprintf(&b, "| %s | %s | %d | %s | %s |\n", m.Name, formatValue(m), m.N, obj, m.Met)
	}
	renderBottlenecks(&b, rep)
	b.WriteString("\n## Breakdown — exact outcome per render class (dialogue: exact commit; empty box: no commit)\n\n")
	b.WriteString("| class | correct | total |\n|---|---|---|\n")
	type cls struct {
		key      string
		hit, tot int
	}
	counts := map[string]*cls{}
	order := []string{}
	for _, o := range outcomes {
		kind := o.sample.textKind
		if kind == "empty" {
			kind = "empty box"
		}
		font := o.sample.variant.Font
		if font == "" {
			font = "none"
		}
		key := fmt.Sprintf("%s %s %s%s", kind, font, o.sample.variant.Resolution, noiseTag(o.sample.variant.Noise))
		c, ok := counts[key]
		if !ok {
			c = &cls{key: key}
			counts[key] = c
			order = append(order, key)
		}
		c.tot++
		switch kind {
		case "empty box": // correctness is silence: no commit at all
			if len(o.commitMsgIDs) == 0 {
				c.hit++
			}
		default:
			if len(o.commitMsgIDs) > 0 && o.commitMsgIDs[0] == o.sample.msgID {
				c.hit++
			}
		}
	}
	for _, k := range order {
		c := counts[k]
		fmt.Fprintf(&b, "| %s | %d | %d |\n", c.key, c.hit, c.tot)
	}
	d := rep.Diagnostics
	if len(d.NotRun) > 0 {
		b.WriteString("\n## Not run — samples the harness failed (excluded from every metric's denominator; the set was NOT run in full)\n\n")
		b.WriteString("| sample | reason |\n|---|---|\n")
		for _, nr := range d.NotRun {
			fmt.Fprintf(&b, "| %s | %s |\n", nr.SampleID, nr.Reason)
		}
	}
	if len(d.Top1Misses) > 0 {
		b.WriteString("\n## Top-1 misses — ranked in-script samples whose first candidate was not the truth (message ids; no game text in this report)\n\n")
		b.WriteString("reading: `window` = the graded display-window reading (top1_exact_accuracy); `last_row` = the legacy last-ranking-row-of-the-span figure (top1_last_row)\n\n")
		b.WriteString("| sample | reading | truth message | top-1 message | top-1 prob |\n|---|---|---|---|---|\n")
		for _, m := range d.Top1Misses {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %.4f |\n", m.SampleID, m.Reading, m.MessageID, m.TopID, m.TopProb)
		}
	}
	b.WriteString("\n## Manifest\n\n")
	fmt.Fprintf(&b, "go %s · os %s · seed %d · set hash `%s`\n", d.GoVersion, d.OS, d.SeedBase, d.SetHash)
	return b.String()
}

// renderBottlenecks is the report's per-stage latency section: the run's own
// stage intervals merged across the samples and reduced by the shared summary
// function, with the named slowest stage and the recorded-bounds flag
// (docs/spec/16 M7 "diagnostics expose bottlenecks").
func renderBottlenecks(b *strings.Builder, rep *Report) {
	b.WriteString("\n## Per-stage latency — bottleneck summary\n\n")
	b.WriteString("Wall-clock stage intervals measured by the pipeline itself (the same numbers the session's stats carry), merged across the run's samples (p50/p95 are the per-sample envelope — the largest per-sample reading, never an optimistic merge) and reduced by the shared summary function (runtime.Bottlenecks, the one the session API, the support bundle and the developer page use). A wall-clock diagnostic, not a verdict: the capture-clock metrics above are the verdicts (ADR-0064).\n\n")
	unmeasured := true
	for _, s := range rep.Bottlenecks.Stages {
		if s.Count > 0 {
			unmeasured = false
			break
		}
	}
	if unmeasured {
		b.WriteString("No stage was measured in this run (no OCR, no ranking, no commit).\n")
		return
	}
	if rep.Bottlenecks.SlowestStage != "" {
		label := gnrt.StageLabels[rep.Bottlenecks.SlowestStage]
		fmt.Fprintf(b, "**Slowest stage: %s (`%s`)**\n\n", label, rep.Bottlenecks.SlowestStage)
	}
	b.WriteString("| stage | samples | p50 | p95 | max | share of end-to-end (p95) |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	shareAboveOne := false
	for _, s := range rep.Bottlenecks.Stages {
		label := gnrt.StageLabels[s.Stage]
		fmt.Fprintf(b, "| %s (`%s`) | %d | %s | %s | %s | %.2f |\n",
			label, s.Stage, s.Count, msCell(s.P50NS), msCell(s.P95NS), msCell(s.MaxNS), s.ShareOfEndToEnd)
		if s.Stage != gnrt.StageFrameToCommitted && s.ShareOfEndToEnd > 1 {
			shareAboveOne = true
		}
	}
	if shareAboveOne {
		b.WriteString("\nA share above 1 is the honest ratio of two histogram bucket edges over two different sample populations (the stage's own samples and the commit's): for sub-millisecond intervals the stage's p95 edge can land above the end-to-end's, and the report says that instead of clamping a measurement.\n")
	}

	b.WriteString("\n## Latency bounds (recorded p95 — flag, not verdict)\n\n")
	if len(rep.LatencyBounds) == 0 {
		b.WriteString("No recorded bounds were checked: the bounds file is absent for this run, or no stage had both samples and a recorded bound. The recorded bounds live in `testdata/bench/latency-bounds.json` with their provenance note; the guard flags (it does not gate) an exceeded p95, because the stage intervals are wall-clock and host-load dependent (ADR-0064).\n")
		return
	}
	b.WriteString("| stage | p95 (this run, wall clock) | recorded bound | exceeded |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, c := range rep.LatencyBounds {
		exceeded := "no"
		if c.Exceeded {
			exceeded = "**YES**"
		}
		fmt.Fprintf(b, "| `%s` | %s ms | %s ms | %s |\n", c.Stage, msNum(c.P95MS), msNum(c.BoundMS), exceeded)
	}
	for _, c := range rep.LatencyBounds {
		if c.Exceeded {
			fmt.Fprintf(b, "\n`%s` exceeds its recorded bound: %s\n", c.Stage, c.Note)
		}
	}
}

// msCell renders one nanosecond value as a millisecond cell: whole-millisecond
// edges (the histogram edges) as integers, an exact max that is not a whole
// millisecond with one decimal — the report never pretends to a finer
// precision than the measurement has.
func msCell(ns int64) string {
	if ns%1_000_000 == 0 {
		return fmt.Sprintf("%d", ns/1_000_000)
	}
	return fmt.Sprintf("%.1f", float64(ns)/1e6)
}

// msNum renders an already-millisecond figure with at most one decimal.
func msNum(ms float64) string {
	if ms == float64(int64(ms)) {
		return fmt.Sprintf("%d", int64(ms))
	}
	return fmt.Sprintf("%.1f", ms)
}

func classNote(class string) string {
	switch class {
	case ClassCaptured:
		return "class (b) captured-frame replay: frames from the external Dolphin fixture (verified against the manifest's per-frame SHA-256), truth from the fixture's oracle, script imported at run time from the user's game image — never stored, never committed (AGENTS.md rule 5)"
	default:
		return "class (a) synthetic stub; real game frames arrive with the Dolphin fixture factory — ADR-0038"
	}
}

func noiseTag(noise float64) string {
	if noise > 0 {
		return " (noisy)"
	}
	return ""
}

func formatValue(m Metric) string {
	switch {
	case m.N == 0:
		return "n/a"
	case m.Kind == "info":
		// Info rows carry no number: the statement is the value.
		return m.Detail
	case m.Kind == "ms":
		if m.ResolutionLimited {
			// The value is a frame-grid offset, not a measured duration: show the
			// honest bound with it (ADR-0038 latency resolution note).
			return fmt.Sprintf("%.0f ms (≤ %.0f ms)", m.Value, m.Bound)
		}
		return fmt.Sprintf("%.0f ms", m.Value)
	case m.Kind == "seconds":
		return fmt.Sprintf("%.1f s", m.Value)
	case m.Kind == "count":
		return strconv.Itoa(int(m.Value))
	case m.Kind == "ratio":
		return fmt.Sprintf("%.1f%%", m.Value*100)
	default:
		return fmt.Sprintf("%v", m.Value)
	}
}

func sampleIDs(outcomes []sampleOutcomes) []string {
	ids := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		ids = append(ids, o.sample.id)
	}
	return ids
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0o644)
}

// --- process resource readings (/proc/self) ---------------------------------

// clkTCK: Linux USER_HZ. This project builds and runs on Linux only (Arch
// development, Debian release per AGENTS.md), where the tick rate is 100.
const clkTCK = 100

// procCPUTime is the process's cumulative CPU seconds (user + system), from
// /proc/self/stat. It covers the whole bench process, generation included —
// which is the honest number for "the run's CPU time".
func procCPUTime() float64 {
	raw, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	s := string(raw)
	// The comm field (in parens) may contain spaces; the numeric fields start
	// after the last ')'.
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 >= len(s) {
		return 0
	}
	f := strings.Fields(s[i+1:])
	// utime is field 14 overall = index 11 after state(3) and the comm field.
	if len(f) < 12 {
		return 0
	}
	utime, _ := strconv.ParseInt(f[11], 10, 64)
	stime, _ := strconv.ParseInt(f[12], 10, 64)
	return float64(utime+stime) / clkTCK
}

// procPeakRSSMB is the process's peak resident set (VmHWM, kB → MB): the
// high-water mark for the whole process lifetime, the stable RSS number to
// report for one run.
func procPeakRSSMB() float64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) != 3 || f[2] != "kB" {
			return 0
		}
		kb, _ := strconv.ParseFloat(f[1], 64)
		return kb / 1024
	}
	return 0
}
