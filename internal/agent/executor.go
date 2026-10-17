package agent

// The executor is the guardrail layer of docs/spec/14: "Tool executor
// verifies allowlist, args schema, role and cost before action". It runs in
// a fixed order and refuses before doing anything expensive:
//
//  1. allowlist     — the tool is registered;
//  2. args          — the arguments are valid JSON and satisfy the
//     tool's input schema;
//  3. role          — the caller role is at least the tool's MinRole;
//  4. budget        — the global per-run call budget and the tool's own
//     per-run budget are not exhausted;
//  5. timeout       — the call runs under its own deadline.
//
// Every outcome — success, handler failure, timeout, refusal — is wrapped
// into the Result envelope of docs/spec/17 "Agent tool result", so the model
// always gets a structured answer it can reason about, and the durable log
// stores exactly the same envelope.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
)

// Executor enforces the guardrails for one run. It is not safe for
// concurrent use: the agent loop drives it sequentially.
type Executor struct {
	registry       *Registry
	role           auth.Role
	maxToolCalls   int
	callsByTool    map[string]int
	totalCalls     int
	defaultTimeout time.Duration
}

// NewExecutor builds the guardrail layer for one run. maxToolCalls is the
// global per-run ceiling (BudgetConfig.MaxToolCalls).
func NewExecutor(reg *Registry, role auth.Role, maxToolCalls int) *Executor {
	return &Executor{
		registry:       reg,
		role:           role,
		maxToolCalls:   maxToolCalls,
		callsByTool:    map[string]int{},
		defaultTimeout: DefaultToolTimeout,
	}
}

// TotalCalls reports how many calls have been dispatched (executed or
// timed out; refusals do not consume budget).
func (e *Executor) TotalCalls() int { return e.totalCalls }

// SeedCalls pre-loads the budget counters with calls already executed before
// a resume, so a restart cannot "refund" the run's tool budget.
func (e *Executor) SeedCalls(n int) {
	if n > e.totalCalls {
		e.totalCalls = n
	}
}

// SeedCallsFor pre-loads one tool's per-run counter on a resume.
func (e *Executor) SeedCallsFor(name string, n int) {
	if n > e.callsByTool[name] {
		e.callsByTool[name] = n
	}
}

// Remaining reports the global budget left before the next dispatch.
func (e *Executor) Remaining() int {
	if e.maxToolCalls <= 0 {
		return 0 // no global cap configured
	}
	if left := e.maxToolCalls - e.totalCalls; left < 0 {
		return 0
	}
	return e.maxToolCalls - e.totalCalls
}

// Execute runs one model-requested call through the guardrails and returns
// the wrapped result. It never panics on bad input: every bad input becomes
// a refused result with a machine-usable error line.
func (e *Executor) Execute(ctx context.Context, call Call) Result {
	res := Result{
		SchemaVersion: ResultSchemaVersion,
		ToolCallID:    call.ID,
		Name:          call.Name,
		SourceIDs:     []string{},
	}
	tool, ok := e.registry.Tool(call.Name)
	if !ok {
		// Unknown tool: refuse. The model asked for something that does not
		// exist in the allowlist — the loop feeds this back so it can course-
		// correct (this is exactly the injection-attempt outcome: a "shell"
		// call gets a refusal, never an execution).
		res.Status = StatusRefused
		res.Error = fmt.Sprintf("tool %q is not registered; only declared tools may be called", call.Name)
		return res
	}

	// 1+2. Arguments: valid JSON, then schema.
	args := call.Arguments
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage("{}")
	}
	if !json.Valid(args) {
		res.Status = StatusRefused
		res.Error = "arguments are not valid JSON"
		return res
	}
	if err := ValidateArgs(tool.InputSchema, args); err != nil {
		res.Status = StatusRefused
		res.Error = fmt.Sprintf("arguments fail input schema: %v", err)
		return res
	}

	// 3. Role ceiling: the caller must hold at least the tool's MinRole
	// (docs/spec/14 "role ACL").
	if !e.role.AtLeast(tool.MinRole) {
		res.Status = StatusRefused
		res.Error = fmt.Sprintf("role %s may not call tool %q (requires %s)",
			auth.RoleNames[e.role], tool.Name, auth.RoleNames[tool.MinRole])
		return res
	}

	// 4. Budget: global first, then per-tool.
	if e.maxToolCalls > 0 && e.totalCalls >= e.maxToolCalls {
		res.Status = StatusRefused
		res.Error = fmt.Sprintf("run tool budget exhausted (%d calls)", e.maxToolCalls)
		return res
	}
	if tool.Budget.MaxCallsPerRun > 0 && e.callsByTool[tool.Name] >= tool.Budget.MaxCallsPerRun {
		res.Status = StatusRefused
		res.Error = fmt.Sprintf("budget for tool %q exhausted (%d calls)", tool.Name, tool.Budget.MaxCallsPerRun)
		return res
	}

	// 5. Timeout-bound execution.
	timeout := tool.Budget.Timeout
	if timeout <= 0 {
		timeout = e.defaultTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	e.totalCalls++
	e.callsByTool[tool.Name]++

	dispatch := Call{ID: call.ID, Name: call.Name, Arguments: args}
	out, err := tool.Handler(callCtx, dispatch)
	if err != nil {
		// Classification: a deadline on the *call* context is a timeout;
		// any other context error is the caller cancelling the run.
		switch {
		case errors.Is(callCtx.Err(), context.DeadlineExceeded):
			res.Status = StatusTimeout
			res.Error = fmt.Sprintf("tool timed out after %s", timeout)
		case callCtx.Err() != nil || ctx.Err() != nil:
			res.Status = StatusCancelled
			res.Error = "tool cancelled by caller"
		default:
			res.Status = StatusFailed
			res.Error = clipErr(err.Error())
		}
		return res
	}

	// Success: the handler supplied the output portion; the envelope fields
	// are the executor's.
	res.Status = StatusSucceeded
	if len(out.Results) > 0 {
		res.Results = out.Results
	}
	if out.SourceIDs != nil {
		res.SourceIDs = out.SourceIDs
	}
	res.ReviewRequired = out.ReviewRequired
	return res
}

// clipErr keeps failure lines short enough for the run log and the prompt
// (a handler error can echo a long URL or payload).
func clipErr(s string) string {
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}
