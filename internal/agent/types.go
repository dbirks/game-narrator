// Package agent implements the onboarding agent core (docs/spec/03,
// ADR-0025): a typed tool registry with registration-time schema validation,
// a guarded executor (allowlist, args schema, role, budget, timeout), a
// versioned system-prompt template, a durable run log, and the agent loop
// that drives the model through tool calls until a stop condition.
//
// The concrete research/import/voice tools and the API/UI come in later
// tasks; this package defines the contract they plug into.
//
// Trust model (docs/spec/14 "Trust zones"): the model is zone E (authenticated
// but mutable), tool inputs are model-directed, and everything a tool reads
// from the outside world is zone C/D untrusted data. Untrusted text reaches
// the model only through the delimited, labelled channel implemented in
// loop.go — never concatenated into the system prompt, never re-parsed as
// instructions.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// SideEffect classifies what a tool may do to system state
// (docs/spec/03 "Tool registry and minimum schemas").
type SideEffect string

const (
	// SideEffectNone: read-only, no system state changes.
	SideEffectNone SideEffect = "none"
	// SideEffectDraft: creates or edits draft revisions only; publishing
	// always happens through the approval boundary.
	SideEffectDraft SideEffect = "draft"
	// SideEffectExternal: touches the network or an external service
	// (search, fetch); still bound by the executor's budgets and timeouts.
	SideEffectExternal SideEffect = "external"
)

// ToolResultStatus is the outcome classification in the wrapped tool result
// (docs/spec/17 "Agent tool result").
type ToolResultStatus string

const (
	StatusSucceeded ToolResultStatus = "succeeded"
	StatusFailed    ToolResultStatus = "failed"
	StatusRefused   ToolResultStatus = "refused"
	StatusTimeout   ToolResultStatus = "timeout"
	StatusCancelled ToolResultStatus = "cancelled"
)

// ResultSchemaVersion is the version of the wrapped tool result envelope.
const ResultSchemaVersion = 1

// Call is one tool invocation the model asked to execute.
type Call struct {
	ID        string
	Name      string
	Arguments json.RawMessage
}

// Result is the wrapped tool result. The executor owns SchemaVersion,
// ToolCallID, Name and Status; the handler owns Results, SourceIDs,
// ReviewRequired and (via its error return) the failure.
type Result struct {
	// SchemaVersion is ResultSchemaVersion (1).
	SchemaVersion int `json:"schema_version"`
	// ToolCallID links this envelope to the model's call id.
	ToolCallID string `json:"tool_call_id"`
	// Name is the tool that produced (or refused) the result.
	Name string `json:"name"`
	// Status is one of succeeded|failed|refused|timeout|cancelled.
	Status ToolResultStatus `json:"status"`
	// Results is the tool's structured output (valid JSON object or array).
	// Empty for failed/refused results.
	Results json.RawMessage `json:"results,omitempty"`
	// Error is a short human-readable failure or refusal line. It never
	// carries a stack, a full payload or a credential (AGENTS.md).
	Error string `json:"error,omitempty"`
	// SourceIDs lists the content sources (internal/store sources ids) the
	// result is grounded in. Empty when the result is not source-grounded.
	SourceIDs []string `json:"source_ids"`
	// ReviewRequired asks the loop to stop at the review boundary
	// (docs/spec/03 "Human-in-the-loop boundaries"); the run lands in state
	// "review", not "succeeded".
	ReviewRequired bool `json:"review_required,omitempty"`
}

// Handler runs one tool call. On success it returns the output portion of the
// wrapped Result; on failure it returns an error, which the executor wraps
// with the call identity and a failed/timeout/cancelled status.
//
// Handlers must be deterministic in their side effects for one (run, seq):
// the durable log guarantees a call is executed at most once, so a handler
// that re-executes on resume would defeat docs/spec/03 scenario 7.
type Handler func(ctx context.Context, call Call) (Result, error)

// Budget is the resource ceiling for one tool within a run
// (docs/spec/14 "Abuse and resource controls": bound tool budget).
type Budget struct {
	// MaxCallsPerRun caps executions of this tool per run. Zero means the
	// per-run cap is governed by the run's global tool budget only.
	MaxCallsPerRun int
	// Timeout bounds one execution. Zero uses DefaultToolTimeout.
	Timeout time.Duration
}

// DefaultToolTimeout bounds a tool execution when a tool declares no
// timeout of its own. Search and fetch stay comfortably under the model's
// patience; parser-heavy tools may declare a longer one.
const DefaultToolTimeout = 60 * time.Second

// Tool is one registered, model-visible function. Only registered tools are
// offered to the model and only they execute (docs/spec/03 "Use only
// declared tools and outputs").
type Tool struct {
	// Name is the wire name the model calls. Restricted to [a-z0-9_] (see
	// registry.go) so it is safe in logs, SQL and the prompt.
	Name string
	// Description is what the model sees when deciding whether to call it.
	Description string
	// InputSchema is the JSON Schema (object) the arguments must satisfy.
	// Validated at registration and re-checked on every call.
	InputSchema json.RawMessage
	// OutputSchema is the JSON Schema of the results; advisory for prompt
	// wording, not enforced on handler output (handlers are trusted code).
	OutputSchema json.RawMessage
	// MinRole is the lowest caller role allowed to have this tool executed.
	MinRole auth.Role
	// Budget caps executions and per-call time.
	Budget Budget
	// SideEffect classifies state impact (none|draft|external).
	SideEffect SideEffect
	// Handler performs the call.
	Handler Handler
	// UntrustedOutput marks tools whose output carries text captured from
	// untrusted sources (web, user imports). When such a result is rendered
	// back to the model, its payload is wrapped in the untrusted-data
	// delimiters (loop.go) — the same contract as web.QuoteUntrusted.
	UntrustedOutput bool
}

// Run states: the closed vocabulary of docs/spec/03 "checkpoint", mirrored
// by the CHECK on agent_runs.state (migration 0007).
type State string

const (
	StateQueued    State = "queued"
	StateRunning   State = "running"
	StateBlocked   State = "blocked"
	StateReview    State = "review"
	StateFailed    State = "failed"
	StateSucceeded State = "succeeded"
	StateCancelled State = "cancelled"
	// StateAbandoned is the operator's way out of a run that is waiting
	// (blocked on a provider fault or a human, stopped by a pause) without
	// deleting anything: the run stops being status data and leaves nothing
	// resumable behind (issue #45, ADR-0028). It is terminal.
	StateAbandoned State = "abandoned"
)

// Terminal reports whether the run can no longer make progress.
func (s State) Terminal() bool {
	switch s {
	case StateFailed, StateSucceeded, StateCancelled, StateAbandoned:
		return true
	}
	return false
}

// Stop reasons recorded on the run row for non-successful stops. They are the
// machine-readable half of "why did this run stop" for the developer UI.
const (
	StopReasonBudget     = "budget_exhausted"
	StopReasonRepetition = "repetition"
	StopReasonMaxSteps   = "max_steps"
	StopReasonReview     = "review_required"
	StopReasonCancelled  = "cancelled"
	StopReasonModelError = "model_error"
	StopReasonToolError  = "tool_error"
	// StopReasonAbandoned records an operator-initiated stop (issue #45,
	// ADR-0028): the run was waiting and the operator ended it rather than
	// leaving the status bar naming a blocked run forever.
	StopReasonAbandoned = "abandoned"
	// StopReasonProviderUnavailable is the degraded-mode stop (ADR-0028):
	// the agent_reasoning endpoint was unavailable (breaker open, 404/503)
	// and the run is *waiting for the provider*, not broken. blocked is a
	// resumable state, so POST .../resume retries the same model round-trip
	// once the endpoint is back; a permanent error still lands in failed.
	StopReasonProviderUnavailable = "provider_unavailable"
)

// BudgetConfig freezes the run's ceilings at start (migration 0007
// budget_json; docs/spec/14).
type BudgetConfig struct {
	// MaxSteps caps model round-trips (default DefaultMaxSteps).
	MaxSteps int `json:"max_steps"`
	// MaxToolCalls caps total executed tool calls across all tools.
	MaxToolCalls int `json:"max_tool_calls"`
	// DeadlineSeconds caps the wall-clock life of the run. Zero: none.
	DeadlineSeconds int `json:"deadline_seconds,omitempty"`
}

// DefaultMaxSteps is the default model round-trip ceiling (task contract).
const DefaultMaxSteps = 24

// DefaultMaxToolCalls is the default total tool-call ceiling.
const DefaultMaxToolCalls = 60

// Conversation message roles: the chat roles plus the persistence mapping in
// migration 0007.
const (
	RoleSystem    = "system"
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleTool      = "tool"
)

// Model is the model boundary the loop drives. openai.Client satisfies it
// directly; production wiring supplies a RouterCall (router.Do over
// agent_reasoning), tests supply a scripted fake.
type Model interface {
	Chat(ctx context.Context, req openai.ChatRequest) (openai.ChatResponse, error)
}

// Outcome is what the loop reports to its caller.
type Outcome struct {
	// State is the final run state.
	State State
	// StopReason is empty on success; otherwise one of the StopReason*
	// constants.
	StopReason string
	// Answer is the model's final text when the run ended with one.
	Answer string
	// Steps is the number of model round-trips made.
	Steps int
	// ToolCalls is the number of tool calls attempted (including refusals).
	ToolCalls int
}
