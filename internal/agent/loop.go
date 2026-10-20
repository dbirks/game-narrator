package agent

// The agent loop (docs/spec/03 "single orchestrator agent with typed tool
// calls, durable tool/job log and resumable plan").
//
// Run drives the model through tool calls until a stop condition:
//
//   - final answer            -> state succeeded
//   - max_steps               -> state blocked, reason max_steps
//   - global tool budget      -> state blocked, reason budget_exhausted
//   - identical call 3 times  -> bounded recovery (recovery.go): a
//                                corrective observation and a varied
//                                sampling on the next turn; state blocked,
//                                reason repetition only after maxRecoveries
//   - source HTTP 401/403     -> skipped for the run: further calls to it
//                                are refused without execution (recovery.go)
//   - tool review_required    -> state review,  reason review_required
//   - context cancellation    -> state cancelled
//   - model error             -> state failed,  reason model_error
//
// Every step is persisted before it is acted on (docs/spec/03 scenario 7):
// the assistant message is stored before any of its tool calls executes, and
// each tool call gets a 'pending' row before execution. Resume therefore has
// a total worklist: pending rows are executed exactly once, finished rows are
// replayed from their stored result, and a missing tool message is re-rendered
// from the stored result — never re-executed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/store"
)

// Options configures one run.
type Options struct {
	// RunID is the durable run id. Required when Store is set.
	RunID string
	// ProfileID is the profile being onboarded, once known.
	ProfileID string
	// Model is the model boundary (openai.Client, RouterCall, or a fake).
	Model Model
	// Registry is the tool allowlist. Required.
	Registry *Registry
	// CallerRole is the privilege level the executor enforces.
	CallerRole auth.Role
	// Store persists the run. Nil: in-memory run (no durability).
	Store *store.AgentStore
	// Hub receives agent.step / tool.started / tool.completed /
	// review.required events. Nil: no events.
	Hub *events.Hub
	// TraceID correlates the run's events.
	TraceID string
	// UserRequest starts the run. Required for a fresh run; for a resume of a
	// run that already has messages it may be empty.
	UserRequest string
	// Plan is the resumable plan (any JSON-able value) stored on the run row.
	Plan any
	// Budget freezes the run ceilings; zero fields take the defaults.
	Budget BudgetConfig
	// Prompt carries the dynamic system-prompt fields.
	Prompt PromptContext
	// SystemPrompt, when non-empty, is the run's system message verbatim:
	// a caller that renders its own versioned template (the assistant agent,
	// internal/assistant, ADR-0099) passes it here instead of the
	// onboarding render RenderWithTools performs below. Fresh runs only —
	// a resume replays the system message from the durable log, never
	// re-rendering it.
	SystemPrompt string
	// Deadline bounds the whole run; zero uses Budget.DeadlineSeconds.
	Deadline time.Duration
	// ModelLabel is the model id stored on the run row for the evidence
	// bundle. Log-safe: an id, never a URL or token.
	ModelLabel string
}

// Run executes the loop to a stop condition. The returned Outcome always
// carries the final run state; the error is non-nil only for option or
// bookkeeping failures (model/registry missing, store errors), never for the
// normal stop conditions, which are outcomes, not errors.
//
// A model that starts repeating itself is recovered, not stopped, up to
// maxRecoveries corrective observations (recovery.go): the repeated call is
// refused with a feedback envelope, the next model turn varies its sampling
// when the provider accepts it, and sources that refused access are skipped
// for the run. Only a model that repeats after the budget is spent stops the
// run, and then with the original stop reason.
func Run(ctx context.Context, opts Options) (Outcome, error) {
	if opts.Model == nil {
		return Outcome{}, fmt.Errorf("agent: Run: model is required")
	}
	if opts.Registry == nil {
		return Outcome{}, fmt.Errorf("agent: Run: registry is required")
	}
	if opts.Store != nil && opts.RunID == "" {
		return Outcome{}, fmt.Errorf("agent: Run: run_id is required when a store is set")
	}
	budget := opts.Budget
	if budget.MaxSteps <= 0 {
		budget.MaxSteps = DefaultMaxSteps
	}
	if budget.MaxToolCalls <= 0 {
		budget.MaxToolCalls = DefaultMaxToolCalls
	}
	deadline := opts.Deadline
	if deadline <= 0 && budget.DeadlineSeconds > 0 {
		deadline = time.Duration(budget.DeadlineSeconds) * time.Second
	}
	if deadline > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, deadline)
		defer cancel()
	}

	l := &loop{
		opts:              opts,
		budget:            budget,
		model:             opts.Model,
		reg:               opts.Registry,
		store:             opts.Store,
		exec:              NewExecutor(opts.Registry, opts.CallerRole, budget.MaxToolCalls),
		seen:              map[string]int{},
		skipped:           map[string]string{},
		outcomes:          map[string][]callOutcome{},
		samplingSupported: true,
	}
	out, err := l.run(ctx)
	if opts.Store != nil && err != nil {
		// Bookkeeping failed: leave the run in a state that tells the truth
		// (failed) rather than running-forever-then-crashed.
		if setErr := opts.Store.SetAgentRunState(ctx, opts.RunID, string(StateFailed), "store_error"); setErr == nil {
			out.State = StateFailed
		}
		return out, err
	}
	return out, err
}

type loop struct {
	opts   Options
	budget BudgetConfig
	model  Model
	reg    *Registry
	store  *store.AgentStore
	exec   *Executor
	// seen counts identical (name+args) requests for the repetition guard.
	seen map[string]int
	// recoveries counts the corrective observations given this run; the
	// repetition guard stops the run once it reaches maxRecoveries
	// (recovery.go). Re-derived from the log on resume.
	recoveries int
	// skipped maps a source URL that refused access (HTTP 401/403) in this
	// run to the refusal line; calls targeting it are refused without
	// execution (recovery.go). Re-derived from the log on resume.
	skipped map[string]string
	// outcomes holds the last few results per repeat key so a corrective
	// observation can report what happened to the repeated call (recovery.go).
	outcomes map[string][]callOutcome
	// varyNext marks the next model request to vary its sampling after a
	// corrective observation (recovery.go); cleared after use.
	varyNext bool
	// samplingSupported is false once the provider rejected the temperature
	// field; the loop then stops varying sampling for the rest of the run.
	samplingSupported bool
	// resumedConv is the conversation rebuilt from the durable log on a
	// resume; empty for a fresh run.
	resumedConv []openai.Message
	// resumedSteps/resumedCalls seed the outcome counters on a resume so a
	// restart cannot "refund" step or tool-call budget.
	resumedSteps int
	resumedCalls int
}

// persistCtx derives a short-lived context for durable writes and reads.
// The run context may already be cancelled — cancellation is exactly what
// the log records — and a cancelled context must not fail the write that
// stores the cancellation (the jobs runner uses the same convention for
// event publishing).
func persistCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (l *loop) persistMessage(m openai.Message) error {
	if l.store == nil {
		return nil
	}
	raw, err := json.Marshal(toPersisted(m))
	if err != nil {
		return fmt.Errorf("agent: persist message: %w", err)
	}
	ctx, cancel := persistCtx()
	defer cancel()
	_, err = l.store.AppendAgentMessage(ctx, l.opts.RunID, m.Role, raw)
	return err
}

// run is the state machine. conv holds the live conversation.
func (l *loop) run(ctx context.Context) (Outcome, error) {
	var (
		out   Outcome
		conv  []openai.Message
		steps int
		calls int
	)
	out.State = StateRunning

	// 1. Conversation: resume from the durable log or start fresh.
	resumed, err := l.loadConversation()
	if err != nil {
		return out, err
	}
	if !resumed {
		if l.opts.UserRequest == "" {
			return out, fmt.Errorf("agent: Run: user_request is required for a fresh run")
		}
		sys := RenderWithTools(l.opts.Prompt, l.reg)
		if l.opts.SystemPrompt != "" {
			sys = l.opts.SystemPrompt
		}
		conv = append(conv,
			openai.Message{Role: RoleSystem, Text: sys},
			openai.Message{Role: RoleUser, Text: l.opts.UserRequest},
		)
		if err := l.persistFresh(conv); err != nil {
			return out, err
		}
	} else {
		conv = l.resumedConv
		steps, calls = l.resumedSteps, l.resumedCalls
		// Seed the executor budget so a restart cannot "refund" tool calls.
		l.exec.SeedCalls(l.resumedCalls)
		// 2. On resume, execute the worklist of calls that lack a durable
		//    outcome (docs/spec/03 scenario 7). This runs before any further
		//    model round-trip so the model never sees an unanswered assistant
		//    tool-call message, and it is where "resumes without duplicates"
		//    is enforced: only pending rows execute.
		if err := l.executePending(ctx, &conv, &out); err != nil {
			return out, err
		}
		if out.State != StateRunning { // a pending call stopped the run (review)
			out.Steps, out.ToolCalls = steps, calls
			return out, nil
		}
	}

	// 3. The main loop.
	for steps < l.budget.MaxSteps {
		if ctx.Err() != nil {
			return l.finishCancelled(&out)
		}
		if l.exec.Remaining() == 0 {
			return l.finish(&out, StateBlocked, StopReasonBudget)
		}

		resp, err := l.chatWithRecovery(ctx, conv)
		if err != nil {
			if ctx.Err() != nil {
				// The caller stopped the run mid-call: cancellation, not a
				// model fault (the loop top checks ctx between round-trips,
				// but a slow call can outlive the cancellation).
				return l.finishCancelled(&out)
			}
			if IsProviderUnavailable(err) {
				// Degraded mode (docs/spec/03 checkpoint, ADR-0028): the
				// endpoint is down, not the run. blocked is resumable, so
				// the human (or the operator's resume call) retries the
				// exact same round-trip once the provider is back; the
				// assistant message for this step was never persisted,
				// so nothing is lost or duplicated.
				l.emitEvent(events.AgentStep, map[string]any{"run_id": l.opts.RunID, "step": steps + 1, "error": clipErr(err.Error())})
				return l.finish(&out, StateBlocked, StopReasonProviderUnavailable)
			}
			l.emitEvent(events.AgentStep, map[string]any{"run_id": l.opts.RunID, "step": steps + 1, "error": clipErr(err.Error())})
			return l.finish(&out, StateFailed, StopReasonModelError)
		}
		steps++

		assistant := openai.Message{Role: RoleAssistant, Text: resp.Text}
		for _, tc := range resp.ToolCalls {
			assistant.ToolCalls = append(assistant.ToolCalls, tc)
		}
		if err := l.persistMessage(assistant); err != nil {
			return out, err
		}
		conv = append(conv, assistant)
		l.emitEvent(events.AgentStep, map[string]any{
			"run_id": l.opts.RunID, "step": steps,
			"finish_reason": resp.FinishReason, "tool_calls": len(resp.ToolCalls),
		})

		if len(resp.ToolCalls) == 0 {
			out.Answer = resp.Text
			out.Steps, out.ToolCalls = steps, calls
			return l.finish(&out, StateSucceeded, "")
		}

		// Parallel calls execute sequentially in the order the model gave
		// them (the loop is single-threaded; determinism over concurrency).
		for _, tc := range resp.ToolCalls {
			if ctx.Err() != nil {
				// A hard stop between calls: the not-yet-inserted calls have no
				// row, which is exactly the worklist shape a resume expects.
				out.Steps, out.ToolCalls = steps, calls
				return l.finishCancelled(&out)
			}
			calls++
			key := repeatKey(tc.Name, tc.Arguments)

			url := sourceURL(tc.Arguments)
			if reason, ok := l.skipped[url]; ok {
				// A source that already refused access is skipped for the run:
				// refuse without executing (no re-fetch, no tool budget spent)
				// so a model re-phrasing the call around the same URL cannot
				// burn the fetch budget on it (recovery.go).
				res := skipFeedback(tc.Name, url, reason)
				if err := l.recordSynthesizedCall(tc, res, &conv); err != nil {
					return out, err
				}
				continue
			}

			l.seen[key]++
			if l.seen[key] >= 3 {
				if l.recoveries >= maxRecoveries {
					// Recovery budget spent: record the call as a durable
					// refusal so the resume worklist stays total (it can only
					// ever be re-rendered, never re-executed), then stop with
					// the original reason.
					res := l.repetitionBoundFeedback(tc.Name)
					if err := l.recordSynthesizedCall(tc, res, &conv); err != nil {
						return out, err
					}
					out.Steps, out.ToolCalls = steps, calls
					return l.finish(&out, StateBlocked, StopReasonRepetition)
				}
				// Bounded recovery (recovery.go): the call is NOT re-executed.
				// The model gets a corrective observation and its next turn
				// varies sampling (when the provider accepts it) to break the
				// loop. Never a retry of a non-idempotent action.
				l.recoveries++
				l.varyNext = true
				res := l.repetitionFeedback(tc.Name, key)
				l.emitEvent(events.AgentStep, map[string]any{
					"run_id": l.opts.RunID, "step": steps,
					"recovery": l.recoveries, "max_recoveries": maxRecoveries,
					"repeated_call": tc.Name,
				})
				if err := l.recordSynthesizedCall(tc, res, &conv); err != nil {
					return out, err
				}
				continue
			}

			rowID, err := l.insertToolCall(tc.ID, tc.Name, tc.Arguments)
			if err != nil {
				return out, err
			}
			l.emitEvent(events.ToolStarted, map[string]any{
				"run_id": l.opts.RunID, "tool_call_id": tc.ID, "name": tc.Name,
			})
			res := l.exec.Execute(ctx, Call{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
			if ferr := l.finishToolCall(rowID, res); ferr != nil {
				return out, ferr
			}
			l.emitEvent(events.ToolCompleted, map[string]any{
				"run_id": l.opts.RunID, "tool_call_id": tc.ID, "name": tc.Name, "status": string(res.Status),
			})

			l.recordOutcome(key, res.Status, res.Error)
			if res.Status == StatusFailed && url != "" && isSourceRefusal(res.Error) {
				// The source itself refused access: skip it for the run so the
				// model's next attempt at it is a refusal, not a re-fetch.
				l.skipped[url] = res.Error
			}

			if res.ReviewRequired {
				l.emitEvent(events.ReviewRequired, map[string]any{
					"run_id": l.opts.RunID, "tool": tc.Name, "tool_call_id": tc.ID,
				})
				return l.finish(&out, StateReview, StopReasonReview)
			}
			if ctx.Err() != nil {
				return l.finishCancelled(&out)
			}

			toolMsg := openai.Message{
				Role:       RoleTool,
				Text:       renderToolMessage(l.toolOrUnknown(tc.Name), res),
				ToolCallID: tc.ID,
			}
			if reason, skip := l.skipped[url]; skip && url != "" {
				toolMsg.Text += "\n" + skipNote(url, reason)
			}
			if err := l.persistMessage(toolMsg); err != nil {
				return out, err
			}
			conv = append(conv, toolMsg)
		}
	}

	// Loop ceiling reached: the run made progress but the model did not
	// answer within the step budget.
	out.Steps = steps
	out.ToolCalls = calls
	return l.finish(&out, StateBlocked, StopReasonMaxSteps)
}

// IsProviderUnavailable reports whether a model boundary error is the
// endpoint being unreachable or rate-limited (docs/spec/06 failure
// taxonomy: timeout, unavailable, rate_limited, replica_mismatch,
// voice_missing) rather than a permanent refusal (auth, bad_schema, ...).
// That split is what turns a gateway outage into a resumable blocked run
// instead of a dead one (ADR-0028, docs/spec/13 "degraded modes").
//
// The router's "role not configured" refusal is included: an unconfigured
// agent_reasoning role is the operator telling the appliance "no model here
// yet", and the honest outcome for the run is blocked (with that as its
// reason), resumable once the role exists — not a dead run. A 404 from a
// configured gateway stays a permanent capability gap (the openai adapter's
// kindForStatus), because a gateway that is serving but does not serve the
// model is not an outage a retry outlives.
func IsProviderUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var pe *providers.Error
	if errors.As(err, &pe) {
		if pe.Kind.RetryableByDefault() {
			return true
		}
		if pe.Kind == providers.KindUnsupportedCapability {
			if pe.Status == 0 {
				return true
			}
			// A 404 whose body names a model that is "not currently available"
			// is a transient serving condition, not a capability gap: the
			// collective gateway (observed 2026-10-18) answers 404
			// not_found_error while the configured model's replica is absent,
			// and the same model id succeeds moments later. The run is not at
			// fault, so the degraded mode applies: blocked + resumable, and the
			// resume retries the exact same round-trip. The taxonomy-level fix
			// (classifying that 404 at the wire) is a providers follow-up.
			return pe.Status == 404 &&
				strings.Contains(strings.ToLower(pe.Error()), "not currently available")
		}
		return false
	}
	// An unclassified error from the wire is treated as permanent: an
	// unknown fault that the taxonomy cannot name is not the outage this
	// degraded path exists for, and retrying the unknown is not the
	// bounded behaviour docs/spec/06 requires.
	return false
}

// loadConversation rebuilds the conversation from the durable log. It
// returns (true, nil) when the run already has messages (a resume), and
// (false, nil) for a fresh run.
func (l *loop) loadConversation() (bool, error) {
	if l.store == nil {
		return false, nil
	}
	ctx, cancel := persistCtx()
	defer cancel()
	run, err := l.store.GetAgentRun(ctx, l.opts.RunID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil // no run row yet: fresh
		}
		return false, err
	}
	switch run.State {
	case string(StateFailed):
		// failed is the only terminal state a resume must refuse: the run
		// stopped on a permanent fault (or a store error), and re-driving
		// it would repeat the fault. succeeded is deliberately *not*
		// terminal here (ADR-0028): a finished onboarding conversation
		// continues when the human approves a publish or sends a follow-up
		// message — the durable log is the total record either way.
		return false, fmt.Errorf("agent: run %s is already %s", run.ID, run.State)
	}
	// queued / running / blocked / review / cancelled are all resumable:
	// blocked and review are waiting on a human, cancelled is a killed
	// process — every one of them has a well-defined worklist in the log.
	msgs, err := l.store.ListAgentMessages(ctx, l.opts.RunID)
	if err != nil {
		return false, err
	}
	if len(msgs) == 0 {
		// The run row exists but no steps were persisted: treat as fresh.
		return false, nil
	}
	// Rebuild the conversation in persisted order.
	var conv []openai.Message
	for _, m := range msgs {
		var pm persistedMessage
		if err := json.Unmarshal(m.ContentJSON, &pm); err != nil {
			return false, fmt.Errorf("agent: resume %s: message %d is not valid content_json: %v", l.opts.RunID, m.Seq, err)
		}
		conv = append(conv, pm.toMessage())
	}
	// Seed the repetition counter from the persisted tool calls so the guard
	// survives a resume (three identical calls split across a restart still
	// count as three). The recovery state (recovery.go) is re-derived from
	// the same rows: refused rows carrying the repetition marker count the
	// recovery budget already spent, failed rows with a URL argument and a
	// source-refusal error re-mark the skipped sources, and every finished
	// row feeds the "previous attempts" line of a later observation. A
	// restart therefore cannot refund the recovery budget.
	rows, err := l.store.ListAgentToolCalls(ctx, l.opts.RunID)
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		key := repeatKey(r.Name, r.ArgsJSON)
		l.seen[key]++
		if r.Status != store.AgentToolPending {
			l.recordOutcome(key, ToolResultStatus(r.Status), r.Error)
		}
		if r.Status == store.AgentToolFailed {
			if u := sourceURL(r.ArgsJSON); u != "" && isSourceRefusal(r.Error) {
				l.skipped[u] = r.Error
			}
		}
		if r.Status == store.AgentToolRefused && strings.HasPrefix(r.Error, repetitionMarker) {
			l.recoveries++
		}
	}
	if l.recoveries > maxRecoveries {
		l.recoveries = maxRecoveries
	}
	// The rebuilt conversation cannot be returned through the signature
	// without restructuring; stash it for executePending/run.
	l.resumedConv = conv
	return true, nil
}

var _ = fmt.Sprintf // import kept honest during split

// resume fields stashed by loadConversation.
type loopWithConv struct{}

// toolOrUnknown returns the tool for message rendering, or a minimal one if
// the call named an unregistered tool (its result is a refusal either way).
func (l *loop) toolOrUnknown(name string) Tool {
	if t, ok := l.reg.Tool(name); ok {
		return t
	}
	return Tool{Name: name, UntrustedOutput: false}
}

// repeatKey canonicalises (name, args) for the repetition guard. The args are
// canonicalised (compact JSON) so the same call with different key order still
// counts as the repetition it is.
func repeatKey(name string, args json.RawMessage) string {
	if len(args) == 0 || string(args) == "null" {
		return name + "|{}"
	}
	compact, err := json.Marshal(json.RawMessage(args))
	if err != nil {
		return name + "|" + string(args)
	}
	return name + "|" + string(compact)
}

func (l *loop) emitEvent(name string, data map[string]any) {
	if l.opts.Hub == nil {
		return
	}
	emit(l.opts.Hub, l.opts.TraceID, name, data)
}
