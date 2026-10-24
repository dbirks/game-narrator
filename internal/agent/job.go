package agent

// Job adapter (ADR-0028): the onboarding run executed as a durable job kind
// ("agent.run") on the docs/spec/08 workflow queue.
//
// The job queue is the crash boundary the run log promises: a process that
// dies mid-run leaves a `running` job, the next start recovers it (the kind
// is registered Resumable), and this handler re-invokes Run on the same
// run_id. Run is total over the durable log (ADR-0025's worklist), so a
// re-invoked attempt never re-executes a finished tool call — it picks the
// run up exactly where the crash stopped it.
//
// Outcomes are outcomes, not job failures: a run that lands in blocked,
// review or cancelled has stopped *for a human*, which is the run's own
// durable state, and re-queuing the job on that alone would spin. Only a
// bookkeeping error (store failure, bad payload) returns an error, so the
// queue's normal retry applies there.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKind is the durable queue kind an onboarding run executes as.
const JobKind = "agent.run"

// JobConfig freezes everything one attempt of an agent.run job needs. The
// production wiring (cmd/game-narrator) supplies it once at start; a test
// supplies a scripted model and an in-memory everything-else.
type JobConfig struct {
	// Model drives the loop (openai.Client, router call, or scripted fake).
	Model Model
	// Registry is the tool allowlist for every attempt.
	Registry *Registry
	// Store is the durable run log. Nil: in-memory attempts (unit tests).
	Store *store.AgentStore
	// Hub receives the run's agent.step / tool.* / review.required events.
	Hub *events.Hub
	// CallerRole is the privilege the executor enforces for the run.
	CallerRole auth.Role
	// Prompt carries the run's system-prompt fields.
	Prompt PromptContext
	// ModelLabel is the model id stored on the run row (log-safe).
	ModelLabel string
	// Budget freezes the run ceilings; zero fields take the defaults.
	Budget BudgetConfig
	// SetupStages is handed through to every attempt's Options (ADR-0108):
	// the setup board's stage observer. Nil: attempts run without it.
	SetupStages func(runID, profileID, toolName string, res Result)
}

// JobPayload is the agent.run job payload. RunID names the durable run;
// UserRequest is the opening request for a fresh run and must be empty on a
// resume (the conversation, including any user message the API appended,
// comes from the durable log). Plan is frozen on the run row at creation
// (the why of the run: title, web policy, staged bytes) so a resumed attempt
// on another process acts on the same intent.
type JobPayload struct {
	RunID       string          `json:"run_id"`
	ProfileID   string          `json:"profile_id,omitempty"`
	UserRequest string          `json:"user_request,omitempty"`
	Plan        json.RawMessage `json:"plan,omitempty"`
	// MaxSteps overrides the handler's frozen step ceiling for this attempt
	// (ADR-0187): the naming run scales its budget with the number of
	// placeholders (the handler's cfg.Budget stays the generic ceiling).
	MaxSteps int `json:"max_steps,omitempty"`
}

// NewJobHandler returns the jobs.Handler for the agent.run kind. cfg's Model
// and Registry are required; a handler that cannot drive a run is a
// configuration fault the process must not start with.
func NewJobHandler(cfg JobConfig) (jobs.Handler, error) {
	if cfg.Model == nil || cfg.Registry == nil {
		return nil, fmt.Errorf("agent: JobHandler: Model and Registry are required")
	}
	return func(ctx context.Context, job jobs.Job, _ jobs.ProgressFn) (json.RawMessage, error) {
		var p JobPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return nil, jobs.Permanent(fmt.Errorf("agent.run: payload is not valid JSON: %v", err))
		}
		if p.RunID == "" {
			return nil, jobs.Permanent(fmt.Errorf("agent.run: payload has no run_id"))
		}
		// Per-attempt step ceiling (a copy: cfg.Budget is the handler's,
		// shared by every attempt it executes).
		budget := cfg.Budget
		if p.MaxSteps > 0 {
			budget.MaxSteps = p.MaxSteps
			budget.MaxToolCalls = p.MaxSteps * 4
		}
		out, err := Run(ctx, Options{
			RunID:       p.RunID,
			ProfileID:   p.ProfileID,
			Model:       cfg.Model,
			Registry:    cfg.Registry,
			CallerRole:  cfg.CallerRole,
			Store:       cfg.Store,
			Hub:         cfg.Hub,
			TraceID:     job.TraceID,
			UserRequest: p.UserRequest,
			Plan:        anyPlan(p.Plan),
			Budget:      budget,
			Prompt:      cfg.Prompt,
			ModelLabel:  cfg.ModelLabel,
			SetupStages: cfg.SetupStages,
		})
		// The result is the machine-readable half of "why did this attempt
		// stop" for the developer UI; it is written even when the run
		// stopped for a human, so a blocked or reviewed run leaves evidence
		// on the job row (docs/spec/13).
		result, _ := json.Marshal(map[string]any{
			"run_id":      p.RunID,
			"state":       string(out.State),
			"stop_reason": out.StopReason,
			"steps":       out.Steps,
			"tool_calls":  out.ToolCalls,
		})
		if err != nil {
			// Bookkeeping only (option/store failure): the run row already
			// holds the honest state, and the queue's retry is the fix.
			return result, fmt.Errorf("agent.run %s: %w", p.RunID, err)
		}
		return result, nil
	}, nil
}

// JobResult is the durable outcome an agent.run attempt writes on its job
// row (docs/spec/13: the job row must answer "why did this attempt stop?").
type JobResult struct {
	RunID      string `json:"run_id"`
	State      State  `json:"state"`
	StopReason string `json:"stop_reason"`
	Steps      int    `json:"steps"`
	ToolCalls  int    `json:"tool_calls"`
}

// anyPlan hands the payload's plan (frozen JSON) to Options.Plan (any
// JSON-able value). Empty or invalid JSON degrades to a null plan rather
// than failing the attempt: the plan is evidence, not a correctness input.
func anyPlan(raw json.RawMessage) any {
	if len(raw) == 0 || !json.Valid(raw) {
		return nil
	}
	return json.RawMessage(raw)
}

// ResultOf decodes a completed agent.run job's result; the zero value and
// false come back when the row carries none (a job that never executed an
// attempt, e.g. cancelled in the queue).
func ResultOf(result json.RawMessage) (JobResult, bool) {
	var r JobResult
	if len(result) == 0 || json.Unmarshal(result, &r) != nil {
		return JobResult{}, false
	}
	return r, true
}
