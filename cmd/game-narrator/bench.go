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
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/bench"
	"github.com/dbirks/game-narrator/internal/matcher"
)

const benchUsageText = `usage: game-narrator bench --set DIR --split tuning|holdout|all [--out DIR] [--generate]
                           [--src-profile PATH] [--builtin NAME] [--seed-base N]
       game-narrator bench captured --fixture DIR --image PATH --out DIR
       game-narrator bench calibrate --set DIR [--fixture DIR]... [--image PATH] --out DIR

bench: run a labelled benchmark set through the real pipeline (internal/bench,
ADR-0038) and write report.json, report.md and manifest.json under --out.
--generate writes the set's profile.json and truth.json and exits. The
builtin generators: "harbour" (default) renders the first synthetic set from
--src-profile; "black-water" (ADR-0075) renders the structurally different
second set (its lines are fixed in the generator, --src-profile is ignored).
captured: run the captured-Dolphin holdout (class (b) captured-frame replay):
the fixture's frames (verified against its manifest's per-frame SHA-256)
through the same real pipeline, truth from the fixture's oracle, the script
imported at run time from --image (default $GAME_NARRATOR_GAME_IMAGE_PATH) via
the windwaker adapter — the image is never stored (AGENTS.md rule 5). The
image's SHA-256 must match the fixture manifest's. NOT RUN (exit 3) when the
fixture dir or the image is absent.
calibrate: fit the matcher's temperature on the set's tuning split plus the
captured fixtures' real-frame observations (ADR-0035 "Calibration") through
the real pipeline and write calibration.json under --out. Each captured
fixture contributes its clean observations (class b), its deterministic
degraded-render variants (class b′: JPEG re-encode, downscale+upscale,
seeded Gaussian noise, fixed crop offset) and its hard-set sources (class
b″: the frames at the heavier degradation levels, ranked against the
adversarial candidate pool — the script plus the near-duplicate distractors
generated at bench time from the fixture's truth lines, in memory only;
ADR-0058 amendment 4). All variant frames are generated at bench time,
never stored, never reported as clean captures. The fit is the mixture
(clean + b′ + b″), never the hard set alone. The holdout split is never
read. The fit refuses a degenerate tuning set (fewer than 10 incorrect
top-1 examples or fewer than 50 samples, or an NLL minimum on the fit
range's boundary): it prints NOT FITTED: <reason> with the per-source
counts, exits non-zero, and writes no calibration.json. On a fit it prints
T plus the Brier / 10-bin ECE and reliability bins before (T = 1) and
after (the fitted T).
`

func newBenchFlagSet() (*flag.FlagSet, *string, *string, *string, *bool, *string, *string, *uint64) {
	fs := flag.NewFlagSet("bench", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.Usage = func() { fmt.Fprint(os.Stderr, benchUsageText) }
	set := fs.String("set", "", "the benchmark set directory (testdata/bench/<set>)")
	split := fs.String("split", bench.SplitHoldout, "which split to run")
	out := fs.String("out", "", "output directory (artifacts/<run-id>/bench)")
	generate := fs.Bool("generate", false, "write profile.json + truth.json and exit")
	src := fs.String("src-profile", "testdata/synthetic/profiles/dialogue-advance-001.json", "the synthetic profile whose lines are rendered")
	builtin := fs.String("builtin", "harbour", "the --generate builtin: harbour (the first set from --src-profile) or black-water (the second set, ADR-0075)")
	seed := fs.Uint64("seed-base", 20260709, "the deterministic seed base recorded in truth.json")
	return fs, set, split, out, generate, src, builtin, seed
}

func runBench(args []string, log *slog.Logger) (int, error) {
	if len(args) > 0 && args[0] == "calibrate" {
		return runBenchCalibrate(args[1:], log)
	}
	if len(args) > 0 && args[0] == "captured" {
		return runBenchCaptured(args[1:], log)
	}
	fs, set, split, out, generate, src, builtin, seed := newBenchFlagSet()
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *set == "" {
		fs.Usage()
		return 2, fmt.Errorf("%w: bench requires --set", errUsage)
	}

	if *generate {
		var (
			ts   *bench.TruthSet
			prof *bench.ScriptProfile
			err  error
		)
		switch *builtin {
		case "harbour":
			ts, prof, err = bench.GenerateSamples(*src, *seed)
		case "black-water":
			ts, prof, err = bench.GenerateSecondGameSamples(*seed)
		default:
			return 2, fmt.Errorf("%w: unknown --builtin %q (harbour, black-water)", errUsage, *builtin)
		}
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
	rep, err := bench.RunBench(ctx, *set, *split, *out, "")
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

// runBenchCaptured is `bench captured`: the captured-Dolphin holdout
// (class (b) captured-frame replay). The frames come from the external
// fixture (verified against its manifest), the truth from the fixture's
// oracle, and the script is imported at run time from the user's image
// through the windwaker adapter — never stored, and its SHA-256 must match
// the fixture manifest's. NOT RUN (exit 3) with an actionable message when
// the fixture dir, the image or tesseract is missing.
func runBenchCaptured(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("bench captured", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fixture := fs.String("fixture", "", "the dolphin fixture dir (e.g. $GAME_NARRATOR_FIXTURE_DIR/windwaker-newgame)")
	image := fs.String("image", os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH"), "the game image (default: $GAME_NARRATOR_GAME_IMAGE_PATH)")
	out := fs.String("out", "", "output directory (artifacts/<run-id>/bench-captured)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *fixture == "" || *out == "" {
		fs.Usage()
		return 2, fmt.Errorf("%w: bench captured requires --fixture and --out", errUsage)
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		return 3, fmt.Errorf("NOT RUN: bench captured needs the tesseract binary (install tesseract-ocr; see docs/spec/16)")
	}
	if st, err := os.Stat(*fixture); err != nil || !st.IsDir() {
		return 3, fmt.Errorf("NOT RUN: bench captured: fixture dir %q is missing (record it with the dolphin fixture factory, ADR-0054, under $GAME_NARRATOR_FIXTURE_DIR; see docs/spec/16)", *fixture)
	}
	if *image == "" {
		return 3, fmt.Errorf("NOT RUN: bench captured: no game image (set GAME_NARRATOR_GAME_IMAGE_PATH to the user's Wind Waker image; see docs/spec/16)")
	}
	// Run-level wall bound (ADR-0064): 30-minute floor + 10s per fixture
	// frame. The per-sample deadlines are gone, so this is the replay's only
	// patience bound, scaled to the fixture: no realistic load on a working
	// host reaches it (measured per-frame cost is ~0.14s on the dense
	// fixture), but a wedged run still cannot hang the gate forever. Loading
	// the fixture here runs the SHA-256 verification gate up front, so a
	// corrupt fixture is refused before the run starts.
	frames, err := bench.CapturedFrameCount(*fixture)
	if err != nil {
		return 1, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), bench.ReplayRunBound(frames))
	defer cancel()
	rep, err := bench.RunCapturedBench(ctx, *fixture, *image, *out)
	if err != nil {
		if containsNOTRun(err.Error()) {
			log.Warn("bench captured not run", "error", err)
			return 3, nil
		}
		return 1, err
	}
	fmt.Print(benchRenderSummary(rep))
	return 0, nil
}

// runBenchCalibrate is `bench calibrate`: fit the matcher's temperature on
// the set's tuning split plus the captured fixtures' real-frame observations
// (ADR-0035 "Calibration") and write calibration.json. The fit never touches
// the synthetic holdout split. A degenerate fit (no interior NLL optimum:
// too few incorrect top-1 examples, too few samples, or the minimum on the
// fit range's boundary) is a refusal, not a result: it prints
// `NOT FITTED: <reason>` with the per-source counts and exits non-zero
// without writing calibration.json.
// Adoption of a fitted T as the default is a separate, reviewed step (a new
// calibration revision + ADR numbers).
func runBenchCalibrate(args []string, log *slog.Logger) (int, error) {
	fs := flag.NewFlagSet("bench calibrate", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	set := fs.String("set", "", "the benchmark set directory (testdata/bench/<set>)")
	out := fs.String("out", "", "output directory (artifacts/<run-id>/calibration)")
	// --fixture is registered (so flag.Parse accepts it) but collected by
	// fsArgsFixture: the std flag package has no multi-value string flag and
	// the fit may need several fixtures' real-frame observations.
	fs.String("fixture", "", "a captured fixture dir (repeatable; needs --image)")
	image := fs.String("image", os.Getenv("GAME_NARRATOR_GAME_IMAGE_PATH"), "the game image for the captured fixtures (default: $GAME_NARRATOR_GAME_IMAGE_PATH)")
	if err := fs.Parse(args); err != nil {
		return 2, err
	}
	if *set == "" || *out == "" {
		fs.Usage()
		return 2, fmt.Errorf("%w: bench calibrate requires --set and --out", errUsage)
	}
	if _, err := exec.LookPath("tesseract"); err != nil {
		return 3, fmt.Errorf("NOT RUN: bench calibrate needs the tesseract binary (install tesseract-ocr; see docs/spec/16)")
	}
	if _, err := exec.LookPath("magick"); err != nil {
		return 3, fmt.Errorf("NOT RUN: bench calibrate needs ImageMagick (magick) for frame generation (install imagemagick; see docs/spec/16)")
	}
	// Run-level wall bound (ADR-0064): the synthetic source alone runs under
	// the 30-minute floor; each captured fixture adds its whole frame list
	// once per replay source — the clean capture, every degraded variant
	// (class b′) and every hard-set variant (class b″) — so the per-frame
	// term scales with that. A corrupt fixture is refused here, before the
	// fit starts.
	fixtureDirs := fsArgsFixture(args)
	capturedFrames := 0
	for _, dir := range fixtureDirs {
		n, err := bench.CapturedFrameCount(dir)
		if err != nil {
			return 1, err
		}
		capturedFrames += n * (1 + len(bench.DegradedVariants()) + len(bench.HardsetVariants()))
	}
	ctx, cancel := context.WithTimeout(context.Background(), bench.ReplayRunBound(capturedFrames))
	defer cancel()
	var insufficient *matcher.ErrInsufficientCalibrationData
	cal, err := bench.CalibrateCombined(ctx, *set, fixtureDirs, *image, *out)
	if err != nil {
		// A degenerate fit is a refusal, not a harness failure: print the
		// reason (with the counts, for the data error) and exit non-zero
		// without writing a T.
		if errors.As(err, &insufficient) || errors.Is(err, matcher.ErrFitHitRangeBound) {
			msg := err.Error()
			if !strings.HasPrefix(msg, "bench: ") {
				msg = strings.TrimPrefix(msg, "matcher: ")
			}
			fmt.Fprintf(os.Stdout, "NOT FITTED: %s\n", strings.TrimPrefix(msg, "bench: "))
			return 4, nil
		}
		if containsNOTRun(err.Error()) {
			log.Warn("bench calibrate not run", "error", err)
			return 3, nil
		}
		return 1, err
	}
	fmt.Printf("bench calibrate: %s (tuning) — T = %.6g, would be calibration revision %d\n",
		cal.Set, cal.Temperature, cal.CalibrationRevision)
	fmt.Printf("  n=%d used, %d skipped (empty box / retrieval miss); NLL at T=1 %.6g → at fit %.6g\n",
		cal.SamplesUsed, cal.SamplesSkipped, cal.NLLAtT1, cal.NLLAtFit)
	fmt.Printf("  brier at T=1 %.6g → at fit %.6g; ece(10bin) at T=1 %.6g → at fit %.6g\n",
		cal.BrierAtT1, cal.BrierAtFit, cal.ECEAtT1, cal.ECEAtFit)
	for _, bin := range cal.ReliabilityFit {
		beforeMean, beforeAcc := reliabilityBinAt(cal.ReliabilityT1, bin.Lo, bin.Hi)
		fmt.Printf("  reliability [%4.2f,%4.2f): n=%-4d after mean prob %.4f / accuracy %.4f — before (T=1) mean %.4f / acc %.4f\n",
			bin.Lo, bin.Hi, bin.N, bin.MeanProb, bin.Acc, beforeMean, beforeAcc)
	}
	for _, s := range cal.Sources {
		fmt.Printf("  [%s: class %s — %d used, %d incorrect top-1, %d skipped, %d not-run]\n", s.Name, s.Class, s.Samples, s.IncorrectTop1, s.Skipped, s.NotRun)
	}
	fmt.Printf("bench calibrate: wrote %s/calibration.json\n", *out)
	return 0, nil
}

// reliabilityBinAt looks up one bin's before/after row for the side-by-side
// print; an empty bin at the fitted T (no samples binned there) prints 0/0
// for the T=1 side as well, which is the honest reading (the T=1 row is
// simply not in the occupied-bins list).
func reliabilityBinAt(bins []bench.CalibrationBin, lo, hi float64) (mean, acc float64) {
	for _, b := range bins {
		if b.Lo == lo && b.Hi == hi {
			return b.MeanProb, b.Acc
		}
	}
	return 0, 0
}

// fsArgsFixture collects the repeated --fixture values the flag package
// does not repeat on its own (std flag has no multi-value string flag).
func fsArgsFixture(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--fixture" || args[i] == "-fixture" {
			if i+1 < len(args) {
				out = append(out, args[i+1])
				i++
			}
			continue
		}
		if strings.HasPrefix(args[i], "--fixture=") {
			out = append(out, strings.TrimPrefix(args[i], "--fixture="))
		}
	}
	return out
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
