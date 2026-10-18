package api

// The /tests page's run controls (docs/spec/09 "/tests" run controls and the
// HTML report viewer/download).
//
// A run is a real job on the durable queue (kind scenario.run): the POST
// enqueues it and answers 202 with the job id; an in-process worker — the
// same claim/lease/heartbeat/complete protocol the jobs runner speaks, aimed
// at this one kind — executes it through the scenario harness (internal/
// scenario.RunScenario) and stores the report as content-addressed assets.
// The API layer holds the worker itself because the job runner is started
// before the HTTP process can register a kind on it, and the durable row is
// still the source of truth: a crash mid-run leaves the row running, the
// lease expires, the next claim takes it over, and a restart parks it
// recoverable for the operator's retry — the queue's existing crash story,
// unchanged.

import (
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKindScenarioRun is the durable queue's kind for one /tests scenario run.
const JobKindScenarioRun = "scenario.run"

// EnvRepoRoot names the variable a packaged process uses to point the dev
// harness at a checkout: the e2e binary resolves the root from its working
// directory instead, and a production appliance without either answers 503.
const EnvRepoRoot = "GAME_NARRATOR_REPO_ROOT"

// lookPath is the PATH probe the dolphin-readiness section uses; a variable so
// a handler test can assert the presence booleans without installing an
// emulator.
var lookPath = exec.LookPath

// dirExists is the harness-root probe: a root that does not carry the
// scenario tree is not a root, whatever it is named.
func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

// harnessRoot resolves the repository root the harness reads, in order: the
// WithTestHarness value, the GAME_NARRATOR_REPO_ROOT variable, the working
// directory. A candidate that lacks testdata/scenarios is rejected, so the
// answer is either a usable root or an honest "not wired".
func (s *Server) harnessRoot() (string, bool) {
	if s.harnessRepoRoot != "" {
		return s.harnessRepoRoot, true
	}
	if v, ok := osLookupEnv(EnvRepoRoot); ok {
		if v = strings.TrimSpace(v); v != "" && dirExists(filepath.Join(v, "testdata", "scenarios")) {
			return v, true
		}
	}
	if wd, err := os.Getwd(); err == nil && dirExists(filepath.Join(wd, "testdata", "scenarios")) {
		return wd, true
	}
	return "", false
}

// scenarioRunPayload is the job's payload: which scenario the run executes.
type scenarioRunPayload struct {
	ScenarioID string `json:"scenario_id"`
}

// scenarioReportRef names the report blobs the job stored, so the run-status
// route and the report route can hand the viewer its two links.
type scenarioReportRef struct {
	JSON assets.Info `json:"json"`
	HTML assets.Info `json:"html"`
}

type scenarioRunResult struct {
	Status  string             `json:"status"`
	Summary string             `json:"summary"`
	Report  *scenarioReportRef `json:"report,omitempty"`
}

// handleDevTestRun answers POST /api/v1/dev/tests/scenarios/{id}/run
// (developer role). The route's pattern is a trailing wildcard because a
// scenario id carries its directory (synthetic/dialogue-advance-001), which a
// single-segment {id} cannot hold: the handler requires the final path segment
// to be "run" and treats everything before it as the id.
func (s *Server) handleDevTestRun(w http.ResponseWriter, r *http.Request) {
	if s.jobStore == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "jobs-unavailable",
			"Jobs Unavailable", "no durable job queue is wired into this process")
		return
	}
	rest := strings.TrimSpace(r.PathValue("rest"))
	id, action := splitScenariosRunPath(rest)
	if action != "run" {
		s.writeProblem(w, r, http.StatusNotFound, "unknown-test-action",
			"Not Found", "the only run control on a scenario is /run")
		return
	}
	root, ok := s.harnessRoot()
	if !ok {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "dev-harness-not-wired",
			"Dev Harness Not Wired",
			"no repository root carries testdata/scenarios in this process: set "+EnvRepoRoot+
				" to a checkout (or run the process from one) and restart")
		return
	}
	sc, err := loadScenarioByID(root, id)
	if err != nil {
		s.writeProblem(w, r, http.StatusNotFound, "scenario-not-found",
			"Scenario Not Found", err.Error())
		return
	}
	// A blocked scenario is run: the harness reports it BLOCKED with its
	// reason and the run produces that report (ADR-0037: blocked ≠ pass, but
	// a run that says so is the page's whole point).
	payload, _ := json.Marshal(scenarioRunPayload{ScenarioID: sc.ID})
	job, _, err := s.jobStore.Enqueue(r.Context(), store.EnqueueParams{
		Kind:        JobKindScenarioRun,
		Payload:     payload,
		MaxAttempts: 3,
	})
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "enqueue-failed",
			"Internal Error", "queueing the scenario run failed; see the server log")
		return
	}
	s.startScenarioWorker()
	writeJSON(w, http.StatusAccepted, map[string]string{
		"job_id":      job.ID,
		"scenario_id": sc.ID,
		"status":      job.State,
	})
}

// splitScenariosRunPath splits the wildcard remainder into the scenario id
// (every segment but the last) and the action (the last segment).
func splitScenariosRunPath(rest string) (id, action string) {
	segs := strings.Split(strings.Trim(rest, "/"), "/")
	if len(segs) < 2 || segs[len(segs)-1] == "" {
		return "", ""
	}
	return strings.Join(segs[:len(segs)-1], "/"), segs[len(segs)-1]
}

// loadScenarioByID finds the scenario whose parsed id field equals id: first
// the file named after the id (the catalog's layout), then the tree itself, so
// a renamed file is still reachable by its id.
func loadScenarioByID(root, id string) (*scenario.Scenario, error) {
	dir := filepath.Join(root, "testdata", "scenarios")
	direct := filepath.Join(dir, filepath.FromSlash(id)+".yaml")
	if _, err := os.Stat(direct); err == nil {
		sc, err := scenario.Load(direct)
		if err != nil {
			return nil, fmt.Errorf("scenario %q does not load: %v", id, err)
		}
		return sc, nil
	}
	var found *scenario.Scenario
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if found != nil || err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".yaml") {
			return nil
		}
		sc, loadErr := scenario.Load(path)
		if loadErr == nil && sc.ID == id {
			found = sc
		}
		return nil
	})
	if found == nil {
		return nil, fmt.Errorf("no scenario with id %q is present under testdata/scenarios", id)
	}
	return found, nil
}

// handleDevTestRunStatus answers GET /api/v1/dev/tests/runs/{job_id}
// (developer role): the job's state and, once succeeded, the report's two
// links. The job row is the single source of truth the UI polls; the report
// blobs are only reachable through this page's report route (developer-gated
// like everything else under /api/v1/dev).
func (s *Server) handleDevTestRunStatus(w http.ResponseWriter, r *http.Request) {
	if s.jobStore == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "jobs-unavailable",
			"Jobs Unavailable", "no durable job queue is wired into this process")
		return
	}
	job, err := s.jobStore.GetJob(r.Context(), r.PathValue("job_id"))
	if err != nil {
		s.writeProblem(w, r, http.StatusNotFound, "run-not-found",
			"Run Not Found", "no scenario run job has that id")
		return
	}
	var result scenarioRunResult
	haveResult := false
	if job.State == store.JobSucceeded && len(job.Result) > 0 {
		haveResult = json.Unmarshal(job.Result, &result) == nil
	}
	var payload scenarioRunPayload
	_ = json.Unmarshal(job.Payload, &payload)
	view := map[string]any{
		"job_id": job.ID, "kind": job.Kind, "state": job.State,
		"scenario_id": payload.ScenarioID, "attempts": job.Attempts,
		"created_at": job.CreatedAt.UTC().Format(time.RFC3339),
		"updated_at": job.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if haveResult {
		view["status"] = result.Status
		view["summary"] = result.Summary
		if result.Report != nil {
			// Bare content hashes, not paths: the client owns the report
			// route's template and builds the URL, so a client that changes
			// the route does not have to chase this response.
			view["report"] = map[string]string{
				"json": result.Report.JSON.SHA256,
				"html": result.Report.HTML.SHA256,
			}
		}
	} else if job.State == store.JobFailed {
		view["status"] = "failed"
		view["summary"] = jobFailureSummary(job)
	}
	writeJSON(w, http.StatusOK, view)
}

// jobFailureSummary renders a failed run's last attempt error (if the store
// recorded one) so the run page can say why the run stopped without the UI
// having to read the job attempts route.
func jobFailureSummary(job *store.Job) string {
	if r := strings.TrimSpace(string(job.Result)); r != "" {
		var e struct {
			Error string `json:"error"`
		}
		if json.Unmarshal([]byte(r), &e) == nil && e.Error != "" {
			return e.Error
		}
	}
	return "the run job ended failed; the job's attempts carry the detail"
}

// handleDevTestReport answers GET /api/v1/dev/tests/reports/{sha256}
// (developer role): one stored report blob, by content hash. The media type
// comes from the store's own sniffing (text/html for the HTML report); the
// JSON report is served as application/json explicitly because sniffing does
// not name it.
func (s *Server) handleDevTestReport(w http.ResponseWriter, r *http.Request) {
	if s.assets == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "assets-unavailable",
			"Assets Unavailable", "no asset store is wired into this process")
		return
	}
	hash := strings.ToLower(strings.TrimSpace(r.PathValue("sha256")))
	rc, info, err := s.assets.Get(r.Context(), hash)
	if err != nil {
		s.writeProblem(w, r, http.StatusNotFound, "report-not-found",
			"Report Not Found", "no stored report blob has that hash (it may have been garbage-collected)")
		return
	}
	defer rc.Close()
	if strings.HasPrefix(info.MediaType, "text/html") {
		// The viewer embeds this in a sandbox="" iframe: no scripts may run
		// (docs/spec/14), and the same bytes must be safe standalone.
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	} else {
		info.MediaType = "application/json"
	}
	w.Header().Set("Content-Type", info.MediaType)
	w.Header().Set("Content-Length", fmt.Sprint(info.Size))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}
