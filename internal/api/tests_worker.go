package api

// The in-process worker for scenario.run jobs (tests_runs.go's companion).
//
// Why the worker lives here rather than in cmd/game-narrator's runner: the
// runner's Register refuses kinds after Start, and the runner is started
// before the HTTP process exists — so a kind this API enqueues cannot be
// registered on that runner without editing the process wiring. The durable
// queue, however, does not care *who* claims its rows: this worker speaks
// the store's own claim/lease/heartbeat/complete protocol for exactly one
// kind, so a run is still a real queued job with attempts, a lease, cancel
// and retry semantics — the same row the /api/v1/jobs routes and the UI's job
// list read. A crash mid-run leaves the row running, the lease expires, the
// next claim takes it over (attempts < max_attempts), and a restart parks it
// recoverable for the operator's retry: the queue's crash story, unchanged.
//
// The worker starts lazily on the first run POST and stops of its own accord
// when the store stops answering: a closed store (the process shutting down)
// stops it on the very next claim, and ten consecutive claim errors —
// transient faults the store may still recover from — stop it as a fallback.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/scenario"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/store"
)

// Worker pacing. The lease comfortably exceeds one heartbeat so a slow box
// cannot lose its own lease; the run timeout bounds one attempt well above
// the harness's own wall time (the synthetic clip runs in seconds; the
// bound is for a pathological one, not a reason to hang).
const (
	scenarioLease         = 30 * time.Second
	scenarioHeartbeat     = 10 * time.Second
	scenarioRunTimeout    = 10 * time.Minute
	scenarioClaimPause    = 250 * time.Millisecond
	scenarioClaimStopErrs = 10
)

type scenarioWorker struct {
	s        *Server
	workerID string
}

// startScenarioWorker boots the worker exactly once per process. It is
// called from the run route (after the enqueue succeeded) so a process that
// never runs a scenario pays for nothing, and a test that never POSTs a run
// never starts a goroutine.
func (s *Server) startScenarioWorker() {
	s.workerOnce.Do(func() {
		if s.jobStore == nil {
			return
		}
		go newScenarioWorker(s).loop()
	})
}

func newScenarioWorker(s *Server) *scenarioWorker {
	return &scenarioWorker{s: s, workerID: fmt.Sprintf("scenario-%d", time.Now().UnixNano())}
}

// loop claims scenario.run jobs until the store stops answering.
func (w *scenarioWorker) loop() {
	errs := 0
	for {
		job, err := w.s.jobStore.Claim(context.Background(), w.workerID,
			[]string{JobKindScenarioRun}, scenarioLease)
		if err != nil {
			// A closed store is not a transient fault: it is the process's
			// shutdown, and the ten-error budget would park the worker ~2.5 s
			// past the moment it is done (issue #20). Stop on the first
			// closed-store answer; the budget stays only for faults a live
			// store may recover from.
			if isClosedStoreErr(err) {
				w.s.log.Info("scenario worker stopping: the job store is closed",
					"error", err.Error())
				return
			}
			errs++
			if errs >= scenarioClaimStopErrs {
				w.s.log.Info("scenario worker stopping: the job store stopped answering",
					"error", err.Error())
				return
			}
			time.Sleep(scenarioClaimPause)
			continue
		}
		if job == nil { // empty queue: an answer, not a failure
			errs = 0
			time.Sleep(scenarioClaimPause)
			continue
		}
		w.execute(*job)
	}
}

// execute runs one claimed job end to end: heartbeat while it runs, the
// harness call, the report blobs, the durable completion. Every exit path
// either finalizes the row (Complete/Fail) or reports why it cannot
// (cancelled, lease lost) without writing a terminal state a lease is no
// longer entitled to. The job's ctx is cancelled before the heartbeat join
// on every path (issue #20): the heartbeat answers ctx.Done() at once, so a
// finished job releases its worker goroutine immediately instead of parking
// it in the join until the next 10 s tick.
func (w *scenarioWorker) execute(job store.Job) {
	ctx, cancel := context.WithCancel(context.Background())

	// Heartbeat until the run ends or the job stops being ours: state !=
	// running (an out-of-band cancel) or ErrConflict (lease stolen) both
	// mean "stop and do not finalize".
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		t := time.NewTicker(scenarioHeartbeat)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				state, err := w.s.jobStore.Heartbeat(ctx, job.ID, w.workerID, scenarioLease)
				if err != nil || state != store.JobRunning {
					cancel()
					return
				}
			}
		}
	}()
	// Registered after the join it orders: it runs first (LIFO), cancels the
	// ctx so the heartbeat exits at once, and only then waits for it. The
	// earlier shape (defer cancel() at the top, defer <-hbDone below) ran
	// the join before the cancel and left the worker parked up to one full
	// heartbeat tick after every finished job.
	defer func() {
		cancel()
		<-hbDone
	}()

	var payload scenarioRunPayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil || payload.ScenarioID == "" {
		w.fail(job, "the job payload is not a scenario run request")
		return
	}
	root, ok := w.s.harnessRoot()
	if !ok {
		w.fail(job, "no repository root carries testdata/scenarios: set GAME_NARRATOR_REPO_ROOT or wire the harness")
		return
	}
	sc, err := loadScenarioByID(root, payload.ScenarioID)
	if err != nil {
		w.fail(job, err.Error())
		return
	}

	runCtx, cancelRun := context.WithTimeout(ctx, scenarioRunTimeout)
	defer cancelRun()
	res, err := scenario.RunScenario(runCtx, root, sc)
	if err != nil {
		if ctx.Err() != nil {
			w.s.log.Info("scenario run abandoned without finalizing: the job is no longer running",
				"job", job.ID, "error", err.Error())
			return
		}
		w.fail(job, "running the scenario failed: "+err.Error())
		return
	}

	report, err := w.storeReport(res, sc)
	if err != nil {
		w.fail(job, "storing the scenario report failed: "+err.Error())
		return
	}

	resultJSON, _ := json.Marshal(scenarioRunResult{
		Status:  res.Status,
		Summary: res.Summary(),
		Report:  &report,
	})
	if err := w.s.jobStore.Complete(ctx, job.ID, resultJSON); err != nil {
		// A cancelled or lease-lost job refuses the finalization; the row's
		// own state is then the truth, and the log line is the audit.
		w.s.log.Warn("scenario run finished but could not finalize its job",
			"job", job.ID, "state", job.State, "error", err.Error())
	}
}

// isClosedStoreErr reports whether err is a query the store refused because
// its database is closed. database/sql's sentinel for that answer
// (errDBClosed, "sql: database is closed") is unexported and the store's
// error mapping only wraps it, so the close is recognised by the sentinel's
// message — std's stable error text, not driver output.
func isClosedStoreErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "sql: database is closed")
}

// fail records the job as failed (not retryable: the reason is a property of
// the request or the checkout, and a blind retry would burn the attempts
// budget repeating the same error).
func (w *scenarioWorker) fail(job store.Job, reason string) {
	if err := w.s.jobStore.Fail(context.Background(), job.ID, reason, false); err != nil {
		w.s.log.Warn("recording the scenario job failure failed", "job", job.ID, "error", err.Error())
	}
}

// scenarioReportDoc is the JSON report the job stores: the harness verdict,
// every assertion's line, the pipeline and session self-reports and the
// faults that fired, so a report reader can audit the run without the
// original checkout open (the events it summarizes are gone after the run).
type scenarioReportDoc struct {
	Schema     string           `json:"schema"`
	ScenarioID string           `json:"scenario_id"`
	Catalog    string           `json:"catalog,omitempty"`
	Mode       string           `json:"mode"`
	Origin     string           `json:"origin"`
	Status     string           `json:"status"`
	Reason     string           `json:"reason,omitempty"`
	Summary    string           `json:"summary"`
	FinishedAt time.Time        `json:"finished_at"`
	Assertions []resultView     `json:"assertions"`
	Pipeline   pipeline.Stats   `json:"pipeline_stats"`
	Session    session.Stats    `json:"session_stats"`
	Faults     []scenario.Fault `json:"faults,omitempty"`
	// PipelineJSON / SessionJSON feed the HTML page; the JSON document keeps
	// the structured forms above.
	PipelineJSON string `json:"-"`
	SessionJSON  string `json:"-"`
}

type resultView struct {
	Type   string `json:"type"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// storeReport renders the verdict as JSON and as a script-free HTML page and
// commits both to the asset store. The content hashes are the report's
// identity: identical verdicts dedupe, and the run route links them.
func (w *scenarioWorker) storeReport(res scenario.RunResult, sc *scenario.Scenario) (scenarioReportRef, error) {
	if w.s.assets == nil {
		return scenarioReportRef{}, fmt.Errorf("no asset store is wired into this process")
	}
	jsonSelf, err := json.Marshal(res.Stats)
	if err != nil {
		return scenarioReportRef{}, err
	}
	sessionSelf, err := json.Marshal(res.SessionStats)
	if err != nil {
		return scenarioReportRef{}, err
	}
	doc := scenarioReportDoc{
		Schema:       "game-narrator.scenario-report/1",
		ScenarioID:   sc.ID,
		Catalog:      sc.Catalog,
		Mode:         sc.Mode,
		Origin:       sc.Origin,
		Status:       res.Status,
		Reason:       res.Reason,
		Summary:      res.Summary(),
		FinishedAt:   time.Now().UTC(),
		Pipeline:     res.Stats,
		Session:      res.SessionStats,
		Faults:       res.Faults,
		PipelineJSON: string(jsonSelf),
		SessionJSON:  string(sessionSelf),
	}
	for _, a := range res.Report.Results {
		doc.Assertions = append(doc.Assertions, resultView{Type: a.Type, Passed: a.Passed, Detail: a.Detail})
	}
	jsonBytes, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return scenarioReportRef{}, err
	}
	htmlBytes, err := renderReportHTML(doc)
	if err != nil {
		return scenarioReportRef{}, err
	}

	put := func(b []byte) (assets.Info, error) {
		id, _, err := w.s.assets.PutStaged(context.Background(), bytes.NewReader(b), "")
		if err != nil {
			return assets.Info{}, err
		}
		info, err := w.s.assets.Commit(context.Background(), id)
		if err != nil {
			_ = w.s.assets.Abort(context.Background(), id)
			return assets.Info{}, err
		}
		return info, nil
	}
	jsonInfo, err := put(jsonBytes)
	if err != nil {
		return scenarioReportRef{}, err
	}
	htmlInfo, err := put(htmlBytes)
	if err != nil {
		return scenarioReportRef{}, err
	}
	return scenarioReportRef{JSON: jsonInfo, HTML: htmlInfo}, nil
}

// reportHTML is the HTML report: static, style-only, no script — the same
// bytes serve the viewer's sandbox="" iframe and a standalone download.
var reportHTML = template.Must(template.New("report").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Scenario report: {{.ScenarioID}}</title>
<style>
body{font-family:system-ui,sans-serif;margin:2rem;color:#1a1a1a}
h1{font-size:1.4rem}
.badge{display:inline-block;padding:.15rem .6rem;border-radius:.5rem;font-weight:600}
.passed{background:#d8f3dc;color:#14532d}.failed{background:#fde2e2;color:#7f1d1d}.blocked{background:#fef3c7;color:#92400e}
table{border-collapse:collapse;margin-top:1rem;width:100%}
th,td{border:1px solid #ccc;padding:.35rem .6rem;text-align:left}
.ok{color:#14532d}.bad{color:#7f1d1d}
pre{background:#f5f5f5;padding:.6rem;overflow-x:auto}
.muted{color:#555}
</style>
</head>
<body>
<h1>Scenario {{.ScenarioID}}{{if .Catalog}} <span class="muted">[{{.Catalog}}]</span>{{end}}</h1>
<p>
<span class="badge {{.Status}}">{{.Status}}</span>
<span class="muted">mode {{.Mode}} &middot; origin {{.Origin}} &middot; finished {{.FinishedAt.Format "2006-01-02 15:04:05 UTC"}}</span>
</p>
<p><strong>{{.Summary}}</strong></p>
{{if .Reason}}<p><strong>Blocked because:</strong> {{.Reason}}</p>{{end}}
{{if .Faults}}
<h2>Faults injected</h2>
<ul>{{range .Faults}}<li>{{.Type}}</li>{{end}}</ul>
{{end}}
{{if .Assertions}}
<h2>Assertions</h2>
<table>
<tr><th>Assertion</th><th>Result</th><th>Detail</th></tr>
{{range .Assertions}}<tr><td>{{.Type}}</td><td class="{{if .Passed}}ok{{else}}bad{{end}}">{{if .Passed}}passed{{else}}FAILED{{end}}</td><td>{{.Detail}}</td></tr>{{end}}
</table>
{{end}}
<h2>Self-reports</h2>
<pre>{{.PipelineJSON}}</pre>
<pre>{{.SessionJSON}}</pre>
<p class="muted">Generated by game-narrator. This document is static (no scripts): it is rendered inside the /tests viewer's sandboxed iframe and served as-is when downloaded.</p>
</body>
</html>`))

func renderReportHTML(doc scenarioReportDoc) ([]byte, error) {
	var buf bytes.Buffer
	if err := reportHTML.Execute(&buf, doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
