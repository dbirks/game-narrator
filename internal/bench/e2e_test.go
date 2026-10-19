package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBenchEndToEnd is docs/spec/12's frame-replay evidence for the bench
// harness itself: about ten generated samples rendered by the generator,
// replayed through the real pipeline (real Tesseract, real matcher, real
// session), graded against the generated truth, and reduced to a report that
// this test checks for well-formedness. recall > 0 is the load-bearing
// assertion: it proves the pipeline really read the frames (a stubbed OCR
// that answers nothing scores zero), not merely that the report writes.
//
// Synthetic by construction (ADR-0038): no game content, no real frames.
func TestBenchEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("tesseract"); err != nil {
		t.Skip("NOT RUN: tesseract not installed")
	}
	if _, err := exec.LookPath("magick"); err != nil {
		t.Skip("NOT RUN: ImageMagick (magick) not installed")
	}
	srcProfile := filepath.Join("..", "..", "testdata", "synthetic", "profiles", "dialogue-advance-001.json")
	ts, prof, err := GenerateSamples(srcProfile, 42)
	if err != nil {
		t.Fatalf("GenerateSamples: %v", err)
	}
	// Ten samples: the first eight (across fonts and resolutions), one
	// analog-noisy and one empty box, so the precision and OCR rows are
	// exercised too.
	pick := func(pred func(Sample) bool, n int) []Sample {
		var out []Sample
		for _, s := range ts.Samples {
			if len(out) == n {
				break
			}
			if pred(s) {
				out = append(out, s)
			}
		}
		return out
	}
	var kept []Sample
	seen := map[string]bool{}
	for _, s := range pick(func(s Sample) bool { return s.TextKind == "dialogue" && s.Variant.Noise == 0 }, 8) {
		kept = append(kept, s)
		seen[s.ID] = true
	}
	kept = append(kept, pick(func(s Sample) bool { return s.Variant.Noise > 0 && !seen[s.ID] }, 1)[0])
	kept = append(kept, pick(func(s Sample) bool { return s.TextKind == "empty" && !seen[s.ID] }, 1)[0])
	if len(kept) != 10 {
		t.Fatalf("picked %d samples, want 10", len(kept))
	}

	dir := t.TempDir()
	ts.Samples = kept
	writeSet(t, dir, ts, prof)

	outDir := filepath.Join(dir, "out")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	// The committed bounds file is the recorded-p95 guard: the run's merged
	// per-stage p95s are flagged (not gated) when they exceed it; the
	// assertion below says any exceedance is self-consistent and named in
	// report.md (a real regression is caught the same way on a quiet host).
	rep, err := RunBench(ctx, dir, SplitAll, outDir, filepath.Join("..", "..", "testdata", "bench", "latency-bounds.json"))
	if err != nil {
		t.Fatalf("RunBench: %v", err)
	}

	// Well-formed: every metric row is present and consistent; the files the
	// report promises are on disk.
	if rep.SchemaVersion != SchemaVersion || rep.Samples != 10 || rep.Split != SplitAll {
		t.Fatalf("report header: %+v", rep)
	}
	if len(rep.Metrics) < 14 {
		t.Fatalf("metrics = %d rows, want at least 14", len(rep.Metrics))
	}
	for _, m := range rep.Metrics {
		if m.N == 0 && m.Met != "n/a" {
			t.Errorf("%s: n=0 but met=%q (want n/a)", m.Name, m.Met)
		}
		if m.N > 0 && (m.Met == "n/a") {
			t.Errorf("%s: n=%d but met=n/a", m.Name, m.N)
		}
	}
	if rep.Diagnostics.SetHash == "" {
		t.Error("set hash missing from diagnostics")
	}
	for _, name := range []string{"report.json", "report.md", "manifest.json"} {
		if _, err := os.Stat(filepath.Join(outDir, name)); err != nil {
			t.Errorf("%s missing: %v", name, err)
		}
	}
	var onDisk Report
	raw, err := os.ReadFile(filepath.Join(outDir, "report.json"))
	if err != nil {
		t.Fatalf("read report.json: %v", err)
	}
	if err := json.Unmarshal(raw, &onDisk); err != nil {
		t.Fatalf("report.json does not parse: %v", err)
	}
	if len(onDisk.Metrics) != len(rep.Metrics) {
		t.Errorf("report.json carries %d metrics, in-memory %d", len(onDisk.Metrics), len(rep.Metrics))
	}
	var manifest map[string]any
	mraw, err := os.ReadFile(filepath.Join(outDir, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	if err := json.Unmarshal(mraw, &manifest); err != nil {
		t.Fatalf("manifest.json does not parse: %v", err)
	}
	for _, k := range []string{"git_sha", "go_version", "tesseract", "os", "seed", "set_hash"} {
		if manifest[k] == nil {
			t.Errorf("manifest.json missing %q", k)
		}
	}

	// The load-bearing assertion: the real pipeline read the real frames.
	byName := map[string]Metric{}
	for _, m := range rep.Metrics {
		byName[m.Name] = m
	}
	if got := byName["recognition_recall"]; got.N < 9 || got.Value <= 0 {
		t.Fatalf("recognition_recall = %v (n=%d): the pipeline produced no correct reading (want > 0)", got.Value, got.N)
	}
	// The commit latency row must be marked resolution-limited (capture-clock
	// grid quantization, ADR-0038): never a fake-exact number.
	if lat := byName["commit_latency_p50_ms"]; lat.N > 0 {
		if !lat.ResolutionLimited || lat.Met != "resolution-limited" {
			t.Errorf("commit_latency_p50_ms not marked resolution-limited: %+v", lat)
		}
		if lat.Bound < lat.Value {
			t.Errorf("latency bound %v below its value %v", lat.Bound, lat.Value)
		}
	}
	// The per-stage bottleneck summary is present and self-consistent (the
	// shared function's shape: three rows in pipeline order, the end-to-end
	// stage a baseline, a named slowest stage when anything was measured).
	if len(rep.Bottlenecks.Stages) != 3 {
		t.Fatalf("bottlenecks = %d stage rows, want 3", len(rep.Bottlenecks.Stages))
	}
	if rep.Bottlenecks.EndToEnd().Count == 0 {
		t.Fatalf("end-to-end stage count = 0 for a run with commits: %+v", rep.Bottlenecks)
	}
	if rep.Bottlenecks.SlowestStage == "" {
		t.Fatal("slowest stage is empty for a run with measured stages")
	}
	// The recorded-bounds guard flags (it does not gate): the stage
	// intervals are wall-clock (ADR-0064) and this package races the other
	// -race packages under the unit gate, so an exceeded p95 here is the
	// host-load situation the bounds' provenance note describes — never a
	// verdict on recognition. The assertion is that any exceedance is
	// self-consistent, carries its "what to do" note and is named in
	// report.md; a quiet-host run (make bench) is where a real regression
	// is caught.
	reportMD, err := os.ReadFile(filepath.Join(outDir, "report.md"))
	if err != nil {
		t.Fatalf("read report.md: %v", err)
	}
	for _, c := range rep.LatencyBounds {
		if c.Exceeded != (c.P95MS > c.BoundMS) {
			t.Errorf("stage %s exceeded = %v, want %v (p95 %v ms vs bound %v ms)",
				c.Stage, c.Exceeded, c.P95MS > c.BoundMS, c.P95MS, c.BoundMS)
		}
		if !c.Exceeded {
			continue
		}
		t.Logf("flag: stage %s p95 %v ms exceeds the recorded bound %v ms: %s",
			c.Stage, c.P95MS, c.BoundMS, c.Note)
		if c.Note == "" {
			t.Errorf("exceeded stage %s carries no note: the flag must say what to do", c.Stage)
		}
		if !strings.Contains(string(reportMD), fmt.Sprintf("`%s` exceeds its recorded bound", c.Stage)) {
			t.Errorf("report.md does not name the exceeded stage %s in its latency-bounds section", c.Stage)
		}
	}
	t.Logf("report: %s %s — recall %.1f%% (%d/%d), top1 %.1f%%, cer %.1f%%, p50 commit latency %v ms",
		rep.Set, rep.Split, byName["recognition_recall"].Value*100,
		countTrue(rep, "recognition_recall"), byName["recognition_recall"].N,
		byName["top1_exact_accuracy"].Value*100, byName["ocr_cer"].Value*100, byName["commit_latency_p50_ms"].Value)
}

// countTrue recovers the hit count from a ratio row for the log line.
func countTrue(rep *Report, name string) int {
	for _, m := range rep.Metrics {
		if m.Name == name {
			x := m.Value * float64(m.N)
			return int(x + 0.5)
		}
	}
	return 0
}

// writeSet writes one set's truth.json and profile.json.
func writeSet(t *testing.T, dir string, ts *TruthSet, prof *ScriptProfile) {
	t.Helper()
	raw, err := json.MarshalIndent(ts, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	raw, err = json.MarshalIndent(prof, "", " ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "profile.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
}
