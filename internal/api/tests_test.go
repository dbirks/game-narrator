package api

// The /tests page's handler tests: the developer gate, the scenario catalog
// (including the BLOCKED row's reason), the run-control contract, and the
// worker's end-to-end path on a blocked scenario — which runs the real
// harness call (internal/scenario.RunScenario answers BLOCKED immediately,
// without a pipeline or OCR, so the test needs no fixtures beyond the two
// YAML documents).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/store"
)

// testHarnessRoot builds a repository root carrying the two scenario
// documents the tests need: one runnable, one blocked with a reason.
func testHarnessRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	scenarios := filepath.Join(root, "testdata", "scenarios", "synthetic")
	if err := os.MkdirAll(scenarios, 0o755); err != nil {
		t.Fatal(err)
	}
	runnable := `schema_version: 1
id: synthetic/test-runnable
origin: generated_synthetic
profile: test-profile
mode: frame_replay
input:
  frames_dir: testdata/synthetic/replay/none
  timestamps: timestamps.json
inference:
  decision: none
  tts: none
assertions:
  - type: no_duplicate_commit
`
	blocked := `schema_version: 1
catalog: GN-T999
id: synthetic/test-blocked
origin: generated_synthetic
profile: test-profile
mode: frame_replay
status: blocked
reason: the fixture for this scenario is not captured yet
input:
  frames_dir: testdata/synthetic/replay/none
  timestamps: timestamps.json
inference:
  decision: none
  tts: none
`
	if err := os.WriteFile(filepath.Join(scenarios, "test-runnable.yaml"), []byte(runnable), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scenarios, "test-blocked.yaml"), []byte(blocked), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func testHarnessServer(t *testing.T, a *testAuth, root string) http.Handler {
	t.Helper()
	as, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("asset store: %v", err)
	}
	return a.handler(
		WithWebAssets(uiBuild()),
		WithStore(a.st),
		WithJobs(store.NewJobsStore(a.st)),
		WithImport(as, nil, store.NewJobsStore(a.st)),
		WithTestHarness(root),
	)
}

// TestDevTestsRequiresDeveloper pins the scope table's dev-prefix grant for
// the /tests routes.
func TestDevTestsRequiresDeveloper(t *testing.T) {
	a := newTestAuth(t)
	root := testHarnessRoot(t)
	h := testHarnessServer(t, a, root)
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleCurator, auth.RoleOperator} {
		rec := a.get(t, h, role, "/api/v1/dev/tests")
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET /api/v1/dev/tests as %s = %d, want 403", role, rec.Code)
		}
	}
}

// TestDevTestsCatalogRendersBlockedWithReason pins the catalog the page
// renders: both scenarios present, the blocked one with status blocked and
// its reason, the runnable one without a spurious one.
func TestDevTestsCatalogRendersBlockedWithReason(t *testing.T) {
	a := newTestAuth(t)
	root := testHarnessRoot(t)
	h := testHarnessServer(t, a, root)
	rec := a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/dev/tests = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		RepoRootKnown bool             `json:"repo_root_known"`
		Scenarios     []scenarioView   `json:"scenarios"`
		Gates         []gateView       `json:"gates"`
		Dolphin       dolphinReadyView `json:"dolphin"`
		Providers     providerReady    `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not JSON: %v", err)
	}
	if !body.RepoRootKnown {
		t.Error("repo_root_known = false, want true (the harness is wired)")
	}
	var runnable, blocked *scenarioView
	for i := range body.Scenarios {
		switch body.Scenarios[i].ID {
		case "synthetic/test-runnable":
			runnable = &body.Scenarios[i]
		case "synthetic/test-blocked":
			blocked = &body.Scenarios[i]
		}
	}
	if runnable == nil || blocked == nil {
		t.Fatalf("catalog missing a scenario: runnable=%v blocked=%v", runnable, blocked)
	}
	if runnable.Status != "runnable" || runnable.Reason != "" {
		t.Errorf("runnable scenario = %+v, want status runnable and no reason", runnable)
	}
	if blocked.Status != "blocked" || blocked.Reason != "the fixture for this scenario is not captured yet" {
		t.Errorf("blocked scenario = %+v, want status blocked with its YAML reason", blocked)
	}
	if blocked.Catalog != "GN-T999" {
		t.Errorf("blocked scenario catalog = %q, want GN-T999", blocked.Catalog)
	}
	if len(body.Gates) == 0 {
		t.Error("no gate rows rendered")
	}
}

// TestDevTestRunUnknownScenarioAndStatus pin the 404 contracts: an unknown
// scenario id is not a queued run, and an unknown job id is not a run status.
func TestDevTestRunUnknownScenarioAndStatus(t *testing.T) {
	a := newTestAuth(t)
	root := testHarnessRoot(t)
	h := testHarnessServer(t, a, root)

	rec := doReq(t, h, a.as(auth.RoleDeveloper,
		httptest.NewRequest(http.MethodPost, "/api/v1/dev/tests/scenarios/synthetic/nope/run", nil)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("run of an unknown scenario = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	rec = a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests/runs/01UNKNOWN")
	if rec.Code != http.StatusNotFound {
		t.Errorf("run status of an unknown job = %d, want 404", rec.Code)
	}
	// A wrong final segment is not a run control either.
	rec = doReq(t, h, a.as(auth.RoleDeveloper,
		httptest.NewRequest(http.MethodPost, "/api/v1/dev/tests/scenarios/synthetic/test-runnable/other", nil)))
	if rec.Code != http.StatusNotFound {
		t.Errorf("action other than /run = %d, want 404", rec.Code)
	}
}

// TestDevTestRunBlockedScenarioEndToEnd is the worker's real path: the POST
// enqueues a durable scenario.run job, the in-process worker claims it, the
// harness reports BLOCKED with the reason, the report blobs are stored, the
// job succeeds, and both report routes serve them.
func TestDevTestRunBlockedScenarioEndToEnd(t *testing.T) {
	a := newTestAuth(t)
	root := testHarnessRoot(t)
	h := testHarnessServer(t, a, root)

	rec := doReq(t, h, a.as(auth.RoleDeveloper,
		httptest.NewRequest(http.MethodPost, "/api/v1/dev/tests/scenarios/synthetic/test-blocked/run", nil)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("202 body carries no job id: %s", rec.Body.String())
	}

	// The worker claims on its own pace; poll the run-status route, the same
	// path the UI uses, until the job is terminal.
	deadline := time.Now().Add(20 * time.Second)
	var status map[string]any
	for time.Now().Before(deadline) {
		srec := a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests/runs/"+accepted.JobID)
		if srec.Code != http.StatusOK {
			t.Fatalf("run status = %d, want 200", srec.Code)
		}
		if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
			t.Fatalf("status body is not JSON: %v", err)
		}
		state, _ := status["state"].(string)
		if state == store.JobSucceeded || state == store.JobFailed || state == store.JobCancelled {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status["state"] != store.JobSucceeded {
		t.Fatalf("the run job ended %v, want succeeded (status %s)", status["state"], mustJSON(status))
	}
	if status["status"] != "blocked" {
		t.Errorf("run status = %v, want blocked (a blocked scenario reports BLOCKED, never pass)", status["status"])
	}
	summary, _ := status["summary"].(string)
	if !strings.HasPrefix(summary, "BLOCKED:") || !strings.Contains(summary, "not captured yet") {
		t.Errorf("summary = %q, want the BLOCKED line with the reason", summary)
	}
	report, _ := status["report"].(map[string]any)
	htmlHash, _ := report["html"].(string)
	if htmlHash == "" {
		t.Fatalf("no HTML report link in the run status: %s", mustJSON(status))
	}
	// The report route serves by content hash; the link is the bare hash.
	htmlURL := "/api/v1/dev/tests/reports/" + htmlHash

	// The report itself: script-free HTML with the verdict and the reason.
	hrec := a.get(t, h, auth.RoleDeveloper, htmlURL)
	if hrec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", htmlURL, hrec.Code)
	}
	if ct := hrec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("report Content-Type = %q, want text/html", ct)
	}
	htmlBody, _ := io.ReadAll(hrec.Body)
	if strings.Contains(strings.ToLower(string(htmlBody)), "<script") {
		t.Error("the HTML report contains a <script> tag: the viewer's sandbox relies on it being script-free")
	}
	if !strings.Contains(string(htmlBody), "not captured yet") {
		t.Error("the HTML report does not carry the blocked reason")
	}

	// A developer role is what the gate allows; a viewer may not read the
	// report (the /api/v1/dev prefix grant).
	vrec := a.get(t, h, auth.RoleViewer, htmlURL)
	if vrec.Code != http.StatusForbidden {
		t.Errorf("report as viewer = %d, want 403", vrec.Code)
	}
}

// repoRootFromCwd walks up from the test's working directory (the package
// directory) to the checkout root: the repro test below runs the harness's
// real synthetic fixture, which only exists in the repository.
func repoRootFromCwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir := wd
	for i := 0; i < 5; i++ {
		if fi, err := os.Stat(filepath.Join(dir, "testdata", "scenarios", "synthetic", "dialogue-advance-001.yaml")); err == nil && !fi.IsDir() {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Skip("the repository checkout (testdata/scenarios/synthetic/dialogue-advance-001.yaml) is not reachable from this working directory")
	return ""
}

// TestDevTestRunDialogueAdvanceReachesTerminal is the repro for the e2e
// timeout (continuation 2026-10-18): the UI's run-control POSTed the real
// synthetic/dialogue-advance-001 scenario (202) and polled the run status
// until the page's 300 s wait expired. This test walks the same path a Go
// client — and the page's polling — sees: enqueue, then the run-status route
// until the job is terminal, and it pins the two things the UI keys on: the
// job's terminal state and the harness verdict's exact vocabulary
// ("passed" | "failed" | "blocked", per internal/scenario.RunResult), which
// the page's PASS/FAIL/BLOCKED pills and the report's CSS classes render.
// A worker that enqueued but never finalised would leave the job queued or
// running past the bound, and a verdict spelled differently from the page's
// contract would fail the status assertion, not a 300 s browser wait.
func TestDevTestRunDialogueAdvanceReachesTerminal(t *testing.T) {
	a := newTestAuth(t)
	root := repoRootFromCwd(t)
	h := testHarnessServer(t, a, root)

	rec := doReq(t, h, a.as(auth.RoleDeveloper,
		httptest.NewRequest(http.MethodPost, "/api/v1/dev/tests/scenarios/synthetic/dialogue-advance-001/run", nil)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("202 body carries no job id: %s", rec.Body.String())
	}

	// Poll the run-status route on the UI's own interval shape (1 s) with a
	// bound well below the e2e wait: the clip paces ~2.3 s of capture clock
	// plus frame decode, so a healthy worker is terminal in tens of seconds.
	deadline := time.Now().Add(120 * time.Second)
	var status map[string]any
	for time.Now().Before(deadline) {
		srec := a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests/runs/"+accepted.JobID)
		if srec.Code != http.StatusOK {
			t.Fatalf("run status = %d, want 200", srec.Code)
		}
		if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
			t.Fatalf("status body is not JSON: %v", err)
		}
		state, _ := status["state"].(string)
		if state == store.JobSucceeded || state == store.JobFailed || state == store.JobCancelled {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status["state"] != store.JobSucceeded {
		t.Fatalf("the run job did not reach succeeded within the bound; final state %v (status %s)", status["state"], mustJSON(status))
	}
	// The vocabulary the page and the report render: "passed" — not "pass".
	if status["status"] != "passed" {
		t.Errorf("run verdict = %v, want \"passed\" (the page maps exactly this value to its PASS pill)", status["status"])
	}
	summary, _ := status["summary"].(string)
	if !strings.HasPrefix(summary, "PASS: ") {
		t.Errorf("summary = %q, want the PASS line", summary)
	}
	report, _ := status["report"].(map[string]any)
	htmlHash, _ := report["html"].(string)
	if htmlHash == "" {
		t.Fatalf("no HTML report link: %s", mustJSON(status))
	}
	// The stored report carries the same verdict, so the viewer shows it too.
	hrec := a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests/reports/"+htmlHash)
	if hrec.Code != http.StatusOK {
		t.Fatalf("GET report = %d, want 200", hrec.Code)
	}
	htmlBody, _ := io.ReadAll(hrec.Body)
	if !strings.Contains(string(htmlBody), "PASS: ") {
		t.Error("the HTML report does not carry the PASS summary line")
	}
}

// mustJSON renders a map for an error message.

// goroutinesWithFrame counts the goroutines whose stack carries frame — the
// stack sample is the same seam the leak gate (fixture_test.go) uses, only
// synchronous and per-frame. It overcounts a stack that names the frame
// twice (a frame line plus its "created by" line), which is fine: the
// callers test for zero.
func goroutinesWithFrame(frame string) int {
	buf := make([]byte, 1<<20)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), frame)
}

// TestScenarioWorkerReleasesAFinishedJob is issue #20's timing assertion: a
// finished job must not keep the worker's goroutine parked in the heartbeat
// join for up to one 10 s tick. Both observations start when the job reads
// terminal (its durable completion has just happened) and both are bounded
// to 1 s:
//
//  1. while the store is still open, the job's execute — and the heartbeat
//     it spawned, whose stack carries the same frames — are gone: the worker
//     has returned to its claim loop instead of parking in <-hbDone until
//     the next tick; and
//
//  2. once the store closes, the worker's claim loop exits within the bound:
//     a closed store is not the transient fault the ten-error budget was
//     for, so the worker stops on the first answer instead of ~2.5 s later.
func TestScenarioWorkerReleasesAFinishedJob(t *testing.T) {
	a := newTestAuth(t)
	root := testHarnessRoot(t)
	h := testHarnessServer(t, a, root)

	rec := doReq(t, h, a.as(auth.RoleDeveloper,
		httptest.NewRequest(http.MethodPost, "/api/v1/dev/tests/scenarios/synthetic/test-blocked/run", nil)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("run = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var accepted struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.JobID == "" {
		t.Fatalf("202 body carries no job id: %s", rec.Body.String())
	}

	// The worker claims on its own pace; poll the run-status route until the
	// job is terminal. A blocked scenario still succeeds as a job (the
	// verdict is in the report), so terminal here is succeeded.
	deadline := time.Now().Add(20 * time.Second)
	var state string
	for {
		srec := a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests/runs/"+accepted.JobID)
		if srec.Code != http.StatusOK {
			t.Fatalf("run status = %d, want 200", srec.Code)
		}
		var status map[string]any
		if err := json.Unmarshal(srec.Body.Bytes(), &status); err != nil {
			t.Fatalf("status body is not JSON: %v", err)
		}
		state, _ = status["state"].(string)
		if state == store.JobSucceeded {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the run job did not reach succeeded within the bound (state %v)", state)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 1. The job's worker is released: no goroutine still carries the
	// execute frames. With the pre-fix defer order (join before cancel) the
	// worker parks here for up to one full 10 s heartbeat tick per job.
	release := time.Now().Add(time.Second)
	for {
		if goroutinesWithFrame("api.(*scenarioWorker).execute") == 0 {
			break
		}
		if !time.Now().Before(release) {
			t.Fatalf("the job's worker is still inside execute 1 s after the job finished: it parks in the heartbeat join instead of returning (issue #20)")
		}
		time.Sleep(25 * time.Millisecond)
	}

	// 2. Close the store and the claim loop must stop on the first
	// closed-store answer. The later t.Cleanup closes it again; the second
	// Close is a harmless no-op answer.
	if err := a.st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	stop := time.Now().Add(time.Second)
	for {
		if goroutinesWithFrame("api.(*scenarioWorker).loop") == 0 {
			break
		}
		if !time.Now().Before(stop) {
			t.Fatalf("the worker's claim loop is still alive 1 s after the store closed: a closed store must stop it on the first answer, not the ten-error budget (issue #20)")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// TestSplitScenariosRunPath pins the wildcard remainder parsing: the id may
// carry its directory, the action is the final segment, and empty or
// single-segment remainders parse to "no run".
func TestSplitScenariosRunPath(t *testing.T) {
	cases := []struct {
		rest, id, action string
	}{
		{"synthetic/dialogue-advance-001/run", "synthetic/dialogue-advance-001", "run"},
		{"a/run", "a", "run"},
		{"a/b/c/run", "a/b/c", "run"},
		{"a/run/extra", "a/run", "extra"},
		{"", "", ""},
		{"run", "", ""},
	}
	for _, tc := range cases {
		id, action := splitScenariosRunPath(tc.rest)
		if id != tc.id || action != tc.action {
			t.Errorf("split(%q) = (%q, %q), want (%q, %q)", tc.rest, id, action, tc.id, tc.action)
		}
	}
}

// TestDevTestHarnessNotWired pins the honest-degradation answer: without a
// resolvable repository root the catalog route still answers (repo_root_known
// false) and the run route names the variable that would fix it.
func TestDevTestHarnessNotWired(t *testing.T) {
	a := newTestAuth(t)
	as, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// No WithTestHarness, the variable unset, and the package directory carries
	// no testdata/scenarios: the only resolution left is "not wired".
	t.Setenv(EnvRepoRoot, "")

	h := a.handler(
		WithWebAssets(uiBuild()),
		WithStore(a.st),
		WithJobs(store.NewJobsStore(a.st)),
		WithImport(as, nil, store.NewJobsStore(a.st)),
	)
	rec := a.get(t, h, auth.RoleDeveloper, "/api/v1/dev/tests")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/dev/tests = %d, want 200", rec.Code)
	}
	var body struct {
		RepoRootKnown bool `json:"repo_root_known"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.RepoRootKnown {
		t.Error("repo_root_known = true for a root without testdata/scenarios")
	}

	run := doReq(t, h, a.as(auth.RoleDeveloper,
		httptest.NewRequest(http.MethodPost, "/api/v1/dev/tests/scenarios/synthetic/any/run", nil)))
	if run.Code != http.StatusServiceUnavailable {
		t.Fatalf("run without a harness = %d, want 503", run.Code)
	}
	if !strings.Contains(run.Body.String(), EnvRepoRoot) {
		t.Errorf("the 503 does not name the variable: %s", run.Body.String())
	}
}
