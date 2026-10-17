package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RunBench runs one set's split end-to-end: render the frames into a temp dir,
// replay every sample through the real pipeline, compute the metrics, and
// write report.json / report.md / manifest.json into outDir. It returns the
// report so tests can assert its shape without re-reading the files.
func RunBench(ctx context.Context, setDir, split, outDir string) (*Report, error) {
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
	dirs, err := RenderSamples(&runSet, framesDir)
	if err != nil {
		return nil, err
	}
	// The samples are independent (fresh session, fresh frame dir), so they run
	// on a bounded worker pool. Each run is deterministic, so concurrency only
	// changes wall time, never the numbers; the CPU figure below is the whole
	// process's cumulative CPU, which is exactly what the benchmark reports.
	workers := 4
	if len(kept) < workers {
		workers = len(kept)
	}
	jobs := make(chan int)
	var (
		mu       sync.Mutex
		outcomes = make([]sampleOutcomes, len(kept))
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				s := kept[i]
				out, err := runSample(ctx, dirs[s.ID], s, idx, regions)
				if err != nil {
					fmt.Fprintf(os.Stderr, "bench: sample %s NOT RUN: %v\n", s.ID, err)
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
	rep := &Report{
		SchemaVersion: SchemaVersion,
		RunID:         time.Now().UTC().Format("20060102T150405Z"),
		Set:           ts.Set,
		Split:         split,
		Origin:        ts.Origin,
		Samples:       len(outcomes),
		Metrics:       metrics,
		Diagnostics: Diagnostics{
			GoVersion:          runtime.Version(),
			Tesseract:          tesseractVersion(),
			OS:                 runtime.GOOS,
			SeedBase:           ts.SeedBase,
			SampleIDs:          sampleIDs(outcomes),
			CPUTimeSecs:        procCPUTime() - cpuBefore,
			PeakRSSMB:          procPeakRSSMB(),
			DurationSecs:       duration.Seconds(),
			MatcherCalibration: matcherCalibrationDigest(),
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
	fmt.Fprintf(&b, "- origin: **%s** (synthetic-only; real game frames arrive with the Dolphin fixture factory — ADR-0038)\n", rep.Origin)
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
	b.WriteString("\n## Manifest\n\n")
	fmt.Fprintf(&b, "go %s · os %s · seed %d · set hash `%s`\n", d.GoVersion, d.OS, d.SeedBase, d.SetHash)
	return b.String()
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
