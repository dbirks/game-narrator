// Command game-narrator bench runs the docs/spec/12 benchmark harness: a
// labelled synthetic set through the real pipeline (ModeReplay + Tesseract),
// with the provisional-objective metrics written as report.json / report.md /
// manifest.json under --out (artifacts/ is gitignored). See internal/bench and
// ADR-0038. --generate (re)writes the set's profile.json and truth.json from
// the synthetic profile instead of running.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bench"
)

const benchUsageText = `usage: game-narrator bench --set DIR --split tuning|holdout|all [--out DIR] [--generate]
                           [--src-profile PATH] [--seed-base N]

bench: run a labelled benchmark set through the real pipeline (internal/bench,
ADR-0038) and write report.json, report.md and manifest.json under --out.
--generate writes the set's profile.json and truth.json from the synthetic
profile (no pipeline run; no magick needed) and exits.`

func newBenchFlagSet() (*flag.FlagSet, *string, *string, *string, *bool, *string, *uint64) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchUsageText) }
	set := fs.String("set", "", "the benchmark set directory (testdata/bench/<set>)")
	split := fs.String("split", bench.SplitHoldout, "which split to run")
	out := fs.String("out", "", "output directory (artifacts/<run-id>/bench)")
	generate := fs.Bool("generate", false, "write profile.json + truth.json and exit")
	src := fs.String("src-profile", "testdata/synthetic/profiles/dialogue-advance-001.json", "the synthetic profile whose lines are rendered")
	seed := fs.Uint64("seed-base", 20260709, "the deterministic seed base recorded in truth.json")
	return fs, set, split, out, generate, src, seed
}

func runBench(args []string, log *slog.Logger) (int, error) {
	fs, set, split, out, generate, src, seed := newBenchFlagSet()
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *set == "" {
		fs.Usage()
		return 2, fmt.Errorf("%w: bench requires --set", errUsage)
	}

	if *generate {
		ts, prof, err := bench.GenerateSamples(*src, *seed)
		if err != nil {
			return 1, err
		}
		profRaw, _ := json.MarshalIndent(prof, "", " ")
		tsRaw, _ := json.MarshalIndent(ts, "", " ")
		if err := os.WriteFile(*set+"/profile.json", append(profRaw, '\n'), 0o644); err != nil {
			return 1, err
		}
		if err := os.WriteFile(*set+"/truth.json", append(tsRaw, '\n'), 0o644); err != nil {
			return 1, err
		}
		fmt.Printf("bench: wrote %s/profile.json and %s/truth.json (%d samples)\n", *set, *set, len(ts.Samples))
		return 0, nil
	}
	if *out == "" {
		fs.Usage()
		return 2, fmt.Errorf("%w: bench requires --out (use artifacts/<run-id>/bench)", errUsage)
	}
	// Honest preflight: without the engine and the renderer every sample
	// would be NOT RUN — say so once, up front, with an actionable message.
	if _, err := exec.LookPath("tesseract"); err != nil {
		return 3, fmt.Errorf("NOT RUN: bench needs the tesseract binary (install tesseract-ocr; see docs/spec/16)")
	}
	if _, err := exec.LookPath("magick"); err != nil {
		return 3, fmt.Errorf("NOT RUN: bench needs ImageMagick (magick) for frame generation (install imagemagick; see docs/spec/16)")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	rep, err := bench.RunBench(ctx, *set, *split, *out)
	if err != nil {
		if err.Error() != "" && (containsNOTRun(err.Error())) {
			log.Warn("bench not run", "error", err)
			return 3, nil
		}
		return 1, err
	}
	// The markdown table is the operator-facing summary; the files under --out
	// are the evidence (AGENTS.md "report with evidence").
	fmt.Print(benchRenderSummary(rep))
	return 0, nil
}

// containsNOTRun reports whether the error is an environment NOT RUN (missing
// tesseract/magick/font), which is an honest skip, not a failure.
func containsNOTRun(s string) bool {
	return strings.Contains(s, "NOT RUN: ")
}

// benchRenderSummary is the one-line table the Makefile tail prints: the
// objective-bearing metrics and their verdicts, straight from the report.
func benchRenderSummary(rep *bench.Report) string {
	var b []string
	b = append(b, fmt.Sprintf("bench: %s %s — %d samples, %s", rep.Set, rep.Split, rep.Samples, rep.Diagnostics.Tesseract))
	for _, m := range rep.Metrics {
		if m.Objective == "" {
			continue
		}
		val := "n/a"
		if m.N > 0 {
			switch m.Kind {
			case "count":
				val = fmt.Sprintf("%d", int(m.Value))
			default:
				val = fmt.Sprintf("%.1f%%", m.Value*100)
			}
		}
		b = append(b, fmt.Sprintf("  %-28s %6s  (n=%-3d) %s  %s", m.Name, val, m.N, m.Met, m.Objective))
	}
	return strings.Join(b, "\n") + "\n"
}
