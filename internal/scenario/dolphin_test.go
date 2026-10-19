package scenario

// dolphin_test.go is the M6 replay gate: it walks the committed scenarios
// under testdata/scenarios/dolphin, runs each one through the real
// recognition/runtime path on the recorded fixture (captured-frame replay,
// ADR-0037), and writes the run's HTML report under
// artifacts/scenarios/<run-id>/index.html (the gitignored evidence bundle).
//
// NOT RUN semantics (the gate convention): when the fixture is absent on this
// host — never recorded here, or recorded on another host with the game text
// the scenarios reference — the test skips with "NOT RUN" and setup
// instructions (run the recorder: TestRealRecordFixture / make test-dolphin).
// A scenario whose fixture hash no longer matches reports NOT RUN per
// scenario and is never counted as a pass.

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/dolphin"
)

func TestDolphinScenarios(t *testing.T) {
	root := repoRootFor(t)
	paths, err := LoadDolphinScenarios(root)
	if err != nil {
		// A missing testdata/scenarios/dolphin is a harness defect on THIS
		// checkout (the directory is committed); a host with the tree but no
		// YAMLs is the same — fail, with the instruction to restore it.
		t.Fatalf("dolphin scenarios: %v (restore testdata/scenarios/dolphin/)", err)
	}
	if len(paths) == 0 {
		t.Fatal("no scenario YAMLs under testdata/scenarios/dolphin (the M6 authored set is missing)")
	}

	var scs []*Scenario
	for _, path := range paths {
		sc, err := Load(path)
		if err != nil {
			t.Fatalf("%s: load: %v", filepath.Base(path), err)
		}
		if sc.Origin != OriginDolphin {
			t.Fatalf("%s: origin %q, want %q (the dolphin dir is the dolphin class)", filepath.Base(path), sc.Origin, OriginDolphin)
		}
		scs = append(scs, sc)
	}
	// The authored set is grouped by fixture. ADR-0054's "one fixture, many
	// scenarios" holds PER GROUP: the newgame recording is one group (boot →
	// title → legend), and the savestate recordings (first-dialogue,
	// legend-press, ADR-0054 amendment 1 + 8) are their own groups. Each
	// group replays from its own artifact and gets its own report bundle, so
	// the per-group one-fixture invariant is preserved without forcing every
	// scenario in the directory onto a single recording. The assertion
	// semantics (RunDolphinScenario, runScenarioCore, evaluate) are unchanged:
	// only the grouping and per-group report are restructured.
	type fixtureGroup struct {
		fixtureID   string
		fxDir       string
		man         dolphin.FixtureManifest
		manifestSHA string
		usable      bool
		unusableWhy string
		scs         []*Scenario
	}
	var groups []*fixtureGroup
	index := map[string]*fixtureGroup{}
	for _, sc := range scs {
		g := index[sc.Input.Fixture]
		if g == nil {
			g = &fixtureGroup{fixtureID: sc.Input.Fixture, fxDir: filepath.Join(fixtureRoot(root), sc.Input.Fixture)}
			index[sc.Input.Fixture] = g
			groups = append(groups, g)
		}
		g.scs = append(g.scs, sc)
	}
	for _, g := range groups {
		man, err := dolphin.VerifyFixture(g.fxDir)
		if err != nil {
			g.unusableWhy = err.Error()
			continue
		}
		sha, err := dolphin.ManifestSHA256(g.fxDir)
		if err != nil {
			g.unusableWhy = err.Error()
			continue
		}
		g.man, g.manifestSHA, g.usable = man, sha, true
	}

	ctx, cancel := context.WithTimeout(context.Background(), DolphinRunBudget)
	defer cancel()

	type runRec struct {
		sc  *Scenario
		res RunResult
	}
	runID := time.Now().UTC().Format("20060102-150405")
	reportRoot := filepath.Join(root, "artifacts", "scenarios", runID)
	var passed, failed, notRun, blocked []string
	for _, g := range groups {
		var recs []runRec
		for _, sc := range g.scs {
			sc := sc
			t.Run(sc.ID, func(t *testing.T) {
				res, err := RunDolphinScenario(ctx, root, sc)
				if err != nil {
					t.Fatalf("run: %v", err)
				}
				fmt.Println(res.Summary())
				switch res.Status {
				case "passed":
					passed = append(passed, sc.ID)
				case "failed":
					failed = append(failed, sc.ID)
					t.Fatalf("scenario failed: %s", failedDetails(res))
				case "blocked":
					blocked = append(blocked, sc.ID)
				case "not_run":
					notRun = append(notRun, sc.ID)
				}
				recs = append(recs, runRec{sc: sc, res: res})
			})
		}

		// The group's report bundle: artifacts/scenarios/<run-id>/
		// <fixture-slug>/, frames sliced from THIS group's manifest. An
		// unusable fixture group has no run evidence (every scenario in it is
		// NOT RUN), so it is registered but not bundled.
		if !g.usable {
			t.Logf("NOT RUN (group %s): fixture not usable at %s (%s). Setup: set %s and run `go test -run TestRealRecordFixture ./internal/dolphin/` (or `make test-dolphin`) with dolphin-emu, Xvfb, ffmpeg, x11-utils and tesseract on PATH; the fixture lands in $%s/%s",
				g.fixtureID, g.fxDir, g.unusableWhy, dolphin.EnvGameImage, dolphin.EnvFixtureDir, g.fixtureID)
			continue
		}
		var runs []ScenarioReportData
		for _, r := range recs {
			if r.res.Status == "not_run" {
				continue // no run, no evidence
			}
			frames := []dolphin.FixtureFrame{}
			if r.res.Status != "blocked" {
				// Blocked rows are the register (ADR-0037): status + reason
				// only, no run evidence attached.
				frames = sliceManifestFrames(g.man, r.sc.Input)
			}
			runs = append(runs, ScenarioReportData{
				Scenario: r.sc,
				Result:   r.res,
				Frames:   frames,
			})
		}
		doc := ReportDoc{
			RunID:        runID,
			GeneratedAt:  time.Now(),
			FixtureDir:   g.fxDir,
			FixtureID:    g.fixtureID,
			ManifestSHA:  g.manifestSHA,
			FrameCount:   len(g.man.Frames),
			GameImageSHA: g.man.GameImageSHA256,
		}
		reportPath, err := WriteHTMLReport(filepath.Join(reportRoot, reportSlug(g.fixtureID)), doc, runs)
		if err != nil {
			t.Fatalf("write HTML report (%s): %v", g.fixtureID, err)
		}
		t.Logf("HTML report: %s", reportPath)
	}

	if len(failed) > 0 {
		t.Errorf("%d dolphin scenario(s) failed: %v", len(failed), failed)
	}
	if len(passed)+len(notRun)+len(blocked) != len(scs) {
		t.Errorf("ran %d scenario(s), want %d", len(passed)+len(notRun)+len(blocked), len(scs))
	}
	// A fixture that verifies means the scenarios could run: at least one
	// must pass, or the replay harness is not exercised.
	if len(passed) == 0 && len(notRun) == 0 {
		t.Errorf("no dolphin scenario passed (failed: %v)", failed)
	}
	// The NOT RUN scenarios are the honest register: every one must name the
	// fixture it could not run.
	for _, id := range notRun {
		t.Logf("NOT RUN (registered): %s", id)
	}
}

// sliceManifestFrames reduces the fixture's manifest to one scenario's slice
// (the same 0-based inclusive semantics RunDolphinScenario applies; last == 0
// means "to the end", mirroring the loader's absence encoding).
func sliceManifestFrames(man dolphin.FixtureManifest, in Input) []dolphin.FixtureFrame {
	first := in.First
	last := len(man.Frames) - 1
	if in.Last != 0 {
		last = in.Last
	}
	if first < 0 || first > last || last >= len(man.Frames) {
		return nil
	}
	return man.Frames[first : last+1]
}
