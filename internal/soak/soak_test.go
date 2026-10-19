// soak_test.go runs the soak at its two scales. TestSoakShort is the CI
// default: a ~100 s chaos soak (seeded, so the fault mix is reproducible)
// with the full pass criteria, inside the two-minute budget. TestSoakLong is
// the documented gate: it reads GAME_NARRATOR_SOAK_DURATION (e.g. 1h) and
// GAME_NARRATOR_SOAK_CHAOS=1 and reports NOT RUN when the duration is unset —
// the 1h replay / 2h chaos pair is the make soak target, not test-full.

package soak_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/soak"
)

const (
	clipDir = "testdata/synthetic/replay/dialogue-advance-001"
	profile = "dialogue-advance-001"
)

// repoRoot is the repository root: the test runs from internal/soak.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("repo root %s does not look like the repository: %v", root, err)
	}
	return root
}

// runSoak runs one soak and prints its summary line (the "soak:" line the
// gate greps). It fails the test on a failed verdict or a run error.
func runSoak(t *testing.T, cfg soak.Config) soak.Result {
	t.Helper()
	root := repoRoot(t)
	cfg.RepoRoot = root
	if cfg.Clips == nil {
		cfg.Clips = []string{clipDir}
	}
	if cfg.Profile == "" {
		cfg.Profile = profile
	}
	// The test timeout must outlive the run plus the stop phase.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration+time.Minute)
	defer cancel()
	res, err := soak.Run(ctx, cfg)
	if err != nil {
		t.Fatalf("soak.Run: %v", err)
	}
	t.Log(res.SummaryLine())
	for _, v := range res.Verdicts {
		if !v.Passed {
			t.Logf("soak: FAIL %s: %s", v.Name, v.Detail)
		}
	}
	if !res.Passed() {
		t.Fatalf("soak run %s failed: %s", res.RunID, res.FirstFailure())
	}
	// The per-stage bottleneck summary is the run's own artifact
	// (docs/spec/16 M7): a run with commits measured the end-to-end stage,
	// and the shared function names the slowest stage — visible in the
	// report, never an assumption.
	if res.Pipeline.Commits > 0 && res.Bottlenecks.EndToEnd().Count == 0 {
		t.Fatalf("soak: the end-to-end stage has no samples for a run with %d commits", res.Pipeline.Commits)
	}
	if res.Bottlenecks.EndToEnd().Count > 0 && res.Bottlenecks.SlowestStage == "" {
		t.Fatal("soak: the bottleneck summary names no slowest stage for a run with measured stages")
	}
	return res
}

// TestSoakShort is the CI soak: ~100 s of chaos (seeded) through the real
// pipeline + session, the full pass criteria, inside the two-minute budget.
func TestSoakShort(t *testing.T) {
	res := runSoak(t, soak.Config{
		Duration:    100 * time.Second,
		SampleEvery: 5 * time.Second,
		Chaos: soak.ChaosConfig{
			Enabled: true,
			Seed:    42,
		},
	})
	// The short run must have produced real evidence: commits on both layers,
	// at least one fault (seed 42's mix is deterministic), and post-teardown
	// counters back at the baseline's neighbourhood.
	if res.Pipeline.Commits == 0 {
		t.Fatal("soak: the pipeline committed nothing — the replay load never ran")
	}
	if res.FaultsTotal() == 0 {
		t.Fatal("soak: no chaos fault fired in 100 s (seed 42, rate 0.15) — the chaos path is not being exercised")
	}
	if len(res.Samples) < 10 {
		t.Fatalf("soak: only %d samples in 100 s (want >= 10 at 5 s sampling)", len(res.Samples))
	}
}

// TestSoakLong is the documented long soak (docs/spec/12: 1h replay, 2h
// chaos; the make soak target runs the pair). GAME_NARRATOR_SOAK_DURATION is
// the run's budget, GAME_NARRATOR_SOAK_CHAOS=1 switches the chaos on. Without
// the duration the test reports NOT RUN — a long soak is never run by
// accident.
func TestSoakLong(t *testing.T) {
	dur := os.Getenv("GAME_NARRATOR_SOAK_DURATION")
	if dur == "" {
		t.Skip("NOT RUN: long soak — set GAME_NARRATOR_SOAK_DURATION (e.g. 1h); GAME_NARRATOR_SOAK_CHAOS=1 adds the chaos (see docs/dev-environment.md)")
	}
	parsed, err := time.ParseDuration(dur)
	if err != nil {
		t.Fatalf("GAME_NARRATOR_SOAK_DURATION %q: %v", dur, err)
	}
	if parsed < time.Minute {
		t.Fatalf("GAME_NARRATOR_SOAK_DURATION %q: the long soak needs at least a minute", dur)
	}
	chaos := os.Getenv("GAME_NARRATOR_SOAK_CHAOS") == "1"
	cfg := soak.Config{
		Duration:    parsed,
		SampleEvery: 30 * time.Second,
		Chaos:       soak.ChaosConfig{Enabled: chaos, Seed: 7},
	}
	res := runSoak(t, cfg)
	if chaos && res.FaultsTotal() == 0 {
		t.Fatal("soak: chaos was enabled but no fault fired — the chaos path is not being exercised")
	}
}
