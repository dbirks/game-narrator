// scenarios_test.go is the M7 runner: go test walks testdata/scenarios/
// synthetic, loads and runs every scenario through the harness, and reports
// each one. A scenario the harness cannot run honestly (status: blocked) is
// reported BLOCKED with its reason — never as a pass (ADR-0037 "blocked ≠
// pass").
package scenario

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// repoRootFor returns the repository root relative to the test's working
// directory (internal/scenario), so scenario input paths resolve the same
// way they do under make.
func repoRootFor(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(cwd, "..", "..")
}

// TestScenarios runs every scenario under testdata/scenarios/synthetic.
func TestScenarios(t *testing.T) {
	root := repoRootFor(t)
	dir := filepath.Join(root, "testdata", "scenarios", "synthetic")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read scenarios dir: %v", err)
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	if len(paths) == 0 {
		t.Fatalf("no scenario YAMLs under %s", dir)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var blocked, failed, passed []string
	for _, path := range paths {
		sc, err := Load(path)
		if err != nil {
			t.Errorf("%s: load: %v", filepath.Base(path), err)
			failed = append(failed, filepath.Base(path))
			continue
		}
		t.Run(sc.ID, func(t *testing.T) {
			res, err := RunScenario(ctx, root, sc)
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			// The one-line report the runner owes (the milestone's final
			// command greps these lines for GN-T / BLOCKED).
			fmt.Println(res.Summary())
			switch res.Status {
			case "blocked":
				blocked = append(blocked, sc.ID)
				// A blocked scenario is reported, not passed: the test
				// records the registration and moves on.
				t.Logf("BLOCKED (registered in docs/status/blocked.md): %s", res.Reason)
			case "passed":
				passed = append(passed, sc.ID)
			default:
				failed = append(failed, sc.ID)
				t.Fatalf("scenario failed: %s", failedDetails(res))
			}
		})
	}

	if len(failed) > 0 {
		t.Errorf("%d scenario(s) failed: %v", len(failed), failed)
	}
	if len(passed)+len(blocked) != len(paths) {
		t.Errorf("ran %d scenario(s), want %d", len(passed)+len(blocked), len(paths))
	}
	if len(passed) == 0 {
		t.Errorf("no scenario passed: the harness is not exercised")
	}
}

// TestBlockedScenariosAreReportedNotPassed pins the ADR-0037 rule at the
// runner level: a status: blocked scenario loads, reports BLOCKED with its
// reason, and is never counted as a pass.
func TestBlockedScenariosAreReportedNotPassed(t *testing.T) {
	root := repoRootFor(t)
	dir := filepath.Join(root, "testdata", "scenarios", "synthetic")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read scenarios dir: %v", err)
	}
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		sc, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("%s: load: %v", e.Name(), err)
		}
		if sc.Status != StatusBlocked {
			continue
		}
		found++
		res, err := RunScenario(context.Background(), root, sc)
		if err != nil {
			t.Fatalf("%s: run: %v", sc.ID, err)
		}
		if res.Status != "blocked" {
			t.Fatalf("%s: a blocked scenario reported status %q, want blocked", sc.ID, res.Status)
		}
		if res.Reason == "" {
			t.Fatalf("%s: a blocked scenario has no reason", sc.ID)
		}
		if res.Report.Passed {
			t.Fatalf("%s: a blocked scenario reported a passing report — blocked ≠ pass", sc.ID)
		}
		if got := res.Summary(); !strings.HasPrefix(got, "BLOCKED: ") {
			t.Fatalf("%s: summary %q does not start with BLOCKED:", sc.ID, got)
		}
	}
	if found == 0 {
		t.Fatal("no blocked scenario found under testdata/scenarios/synthetic: the register rule is untested")
	}
}

// TestScenarioLoads pins that the catalog-labelled M7 scenarios load with
// their faults parsed, so a typo in a fault entry is a load failure, not a
// silent no-op scenario.
func TestScenarioLoads(t *testing.T) {
	root := repoRootFor(t)
	cases := []struct {
		file    string
		catalog string
		faults  int
	}{
		{"gn-t032-rapid-advance.yaml", "GN-T032", 1},
		{"gn-t034-decision-delayed.yaml", "GN-T034", 1},
		{"gn-t034-decision-duplicate.yaml", "GN-T034", 1},
		{"gn-t044-endpoint-loss.yaml", "GN-T044", 1},
		{"gn-t046-voice-missing.yaml", "GN-T046", 1},
		{"gn-t047-tts-partial-cancel.yaml", "GN-T047", 1},
	}
	for _, c := range cases {
		sc, err := Load(filepath.Join(root, "testdata", "scenarios", "synthetic", c.file))
		if err != nil {
			t.Errorf("%s: load: %v", c.file, err)
			continue
		}
		if sc.Catalog != c.catalog {
			t.Errorf("%s: catalog = %q, want %q", c.file, sc.Catalog, c.catalog)
		}
		if len(sc.Faults) != c.faults {
			t.Errorf("%s: %d fault(s), want %d", c.file, len(sc.Faults), c.faults)
		}
		if sc.Origin != OriginGeneratedSynthetic {
			t.Errorf("%s: origin = %q, want generated_synthetic", c.file, sc.Origin)
		}
	}
}
