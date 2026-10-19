package agent

// Loop recovery (amendment to ADR-0025 §7). The repetition guard and the
// repeated failure of a source that refuses access (HTTP 401/403) are
// bounded recovery opportunities, not immediate stops: the loop feeds back
// a corrective observation (what was called, what happened, what remains
// available), varies sampling on the next model turn when the provider
// accepts the temperature field, and skips the refusing source for the
// rest of the run. Only after maxRecoveries observations has the loop
// stopped the run, and then with the original stop_reason, so the
// developer UI keeps seeing the closed ADR-0025 vocabulary
// ("blocked: repetition — resume when the fault clears").
//
// Durability: every synthesized refusal goes through the same pending-row +
// durable-envelope path as an executed call, so a restart re-renders it
// from the stored result (worklist case 3) and never re-executes a
// repeated or skipped call. The recovery state is re-derived from the log
// on resume — recoveries from refused rows carrying repetitionMarker,
// skipped sources from failed rows with a URL argument and a refusal
// error — so a restart cannot refund the recovery budget.
//
// Non-idempotency: a repeated or skipped call is refused, never
// re-executed; only the model round-trip itself is ever retried (once,
// with the rejected sampling field dropped), and a model call is the one
// action the loop is allowed to repeat (ADR-0025: the router carries it
// with Idempotent: true).
//
// Trust model: the observations are built only from trusted material —
// tool names, executor error lines and the model's own arguments. No
// fetched page content enters them, and nothing in this file is threaded
// into the system prompt (docs/spec/14).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/openai"
)

// maxRecoveries is the number of corrective observations one run may be
// given before the repetition guard stops it. Deliberately small: the step
// and tool-call budgets still cap the run, and a model that repeats a call
// after two explicit warnings has looped for good.
const maxRecoveries = 2

// repetitionMarker prefixes the error line of a synthesized repetition
// refusal. Resume counts the refused rows carrying it to re-derive the
// recovery budget spent.
const repetitionMarker = "repetition detected:"

// recoveryTemperature is the sampling requested for the single model turn
// that follows a corrective observation: clearly above the serving
// default so the model is pushed out of the loop it fell into. Set on that
// turn only; the next turn reverts to the provider default.
const recoveryTemperature = 0.8

// callOutcome is one remembered outcome of a (name, args) request, kept
// for the corrective observation's "previous attempts" line.
type callOutcome struct {
	status string
	err    string
}

// sourceURL returns the top-level "url" argument of a tool call, or ""
// when there is none. It is the loop's generic read of "which source did
// this call target" (only fetch_page declares one today) and what lets the
// loop skip a source that already refused access even when the model
// re-phrases the call around it.
func sourceURL(args json.RawMessage) string {
	if len(args) == 0 {
		return ""
	}
	var m struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(args, &m); err != nil {
		return ""
	}
	return strings.TrimSpace(m.URL)
}

// isSourceRefusal reports whether a tool error line is the source itself
// refusing access (HTTP 401/403) rather than a network or appliance fault:
// only such a refusal marks the source as skipped for the run.
func isSourceRefusal(err string) bool {
	return strings.Contains(err, "HTTP 401") || strings.Contains(err, "HTTP 403")
}

// recordOutcome remembers the last two outcomes of one (name, args)
// request. Bounded by design: at most two entries per distinct call, and
// the number of distinct calls is capped by the run's tool budget.
func (l *loop) recordOutcome(key string, status ToolResultStatus, err string) {
	out := callOutcome{status: string(status), err: clipErr(err)}
	prev := l.outcomes[key]
	if len(prev) >= 2 {
		prev = prev[len(prev)-1:]
	}
	l.outcomes[key] = append(prev, out)
}

// outcomesOf returns the remembered outcomes of one (name, args) request.
func (l *loop) outcomesOf(key string) []callOutcome {
	return l.outcomes[key]
}

// skipFeedback synthesizes the refused envelope for a call that targets a
// source which already refused access earlier in the run: no re-fetch and
// no tool budget spent, with an explicit instruction to move on.
func skipFeedback(tool, url, reason string) Result {
	return Result{
		SchemaVersion: ResultSchemaVersion,
		Name:          tool,
		Status:        StatusRefused,
		SourceIDs:     []string{},
		Error: fmt.Sprintf("source already skipped this run: %s (%s). It will not be fetched again. "+
			"Use a different source or continue with the evidence you already have.",
			url, shortReason(reason)),
	}
}

// skipNote is appended to the model-facing tool message of the call that
// just hit a refusing source: the correction at the moment of failure,
// before the model gets to decide to retry.
func skipNote(url, reason string) string {
	return fmt.Sprintf("[recovery note] This source refused access (%s) and is skipped for this run: %s. "+
		"Do not fetch it again; choose a different source or continue with the evidence you already have.",
		shortReason(reason), url)
}

// repetitionFeedback synthesizes the corrective observation for a call the
// model has already made twice in this run: the count, the previous
// attempts' outcomes, the sources skipped so far, the tools that remain,
// and what to do instead. It is a refused envelope because the call will
// not be executed.
func (l *loop) repetitionFeedback(name, key string) Result {
	var b strings.Builder
	b.WriteString(repetitionMarker)
	fmt.Fprintf(&b, " this exact call (%s with the same arguments) has now been requested %d times in this run and it will not be re-executed.",
		name, l.seen[key])
	if prev := l.outcomesOf(key); len(prev) > 0 {
		b.WriteString("\nPrevious attempts:")
		for _, o := range prev {
			if o.err == "" {
				fmt.Fprintf(&b, " %s;", o.status)
			} else {
				fmt.Fprintf(&b, " %s (%s);", o.status, o.err)
			}
		}
	}
	if s := skippedSourceSummary(l.skipped); s != "" {
		fmt.Fprintf(&b, "\nSkipped sources this run: %s.", s)
	}
	fmt.Fprintf(&b, "\nTools still available: %s.", strings.Join(l.reg.Names(), ", "))
	b.WriteString("\nCorrective guidance: change what you ask for — a different query or a different source — or continue with the evidence you already have. Do not repeat a call that has already been made or refused, and only call tools from the enabled-tools list.")
	return Result{
		SchemaVersion: ResultSchemaVersion,
		Name:          name,
		Status:        StatusRefused,
		SourceIDs:     []string{},
		Error:         clipFeedback(b.String()),
	}
}

// repetitionBoundFeedback is the final observation when the recovery budget
// is spent: the same call was requested again after maxRecoveries
// corrections. The run stops on it with the original stop_reason.
func (l *loop) repetitionBoundFeedback(name string) Result {
	var b strings.Builder
	b.WriteString(repetitionMarker)
	fmt.Fprintf(&b, " this exact call (%s) was requested again after %d corrective observations; "+
		"the recovery budget for this run is exhausted and the run stops here.",
		name, maxRecoveries)
	return Result{
		SchemaVersion: ResultSchemaVersion,
		Name:          name,
		Status:        StatusRefused,
		SourceIDs:     []string{},
		Error:         clipFeedback(b.String()),
	}
}

// temperatureRejected reports whether a model error is the provider
// rejecting the temperature field itself (a 400-class failure classified
// unsupported_capability that names temperature) rather than a transient
// fault or a different capability gap.
func temperatureRejected(err error) bool {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return false
	}
	return pe.Kind == providers.KindUnsupportedCapability &&
		strings.Contains(strings.ToLower(pe.Error()), "temperature")
}

// chatWithRecovery issues one model round-trip, applying the recovery
// sampling variation when it is pending. If the provider rejects the
// temperature field, the loop remembers it, drops the field and gives the
// same round-trip one honest retry (docs/spec/06 capability-degradation
// pattern): a model call is the one action the loop may repeat — it is
// idempotent by construction (the router carries it with Idempotent: true)
// and no assistant message has been persisted for it yet, so the retry
// duplicates nothing.
func (l *loop) chatWithRecovery(ctx context.Context, conv []openai.Message) (openai.ChatResponse, error) {
	req := openai.ChatRequest{
		Messages:   conv,
		Tools:      l.reg.Definitions(),
		ToolChoice: openai.ToolChoiceAuto,
	}
	if l.varyNext {
		l.varyNext = false
		if l.samplingSupported {
			t := recoveryTemperature
			req.Temperature = &t
		}
	}
	resp, err := l.model.Chat(ctx, req)
	if err != nil && ctx.Err() == nil && req.Temperature != nil && temperatureRejected(err) {
		// The provider does not take a sampling override: degrade exactly
		// once and stop varying sampling for the rest of the run. The
		// corrective observation (not the sampling) is what recovers the
		// model; the variation is a second-order nudge.
		l.samplingSupported = false
		req.Temperature = nil
		resp, err = l.model.Chat(ctx, req)
	}
	return resp, err
}

// recordSynthesizedCall durably records a call the loop refused without
// executing it (a skipped source or the repetition guard) through the same
// pending-row + outcome-envelope path as an executed call: row, events,
// tool message. This is what keeps "a repeated or skipped call is never
// re-executed" true across a restart: on resume the worklist case 3
// re-renders the stored envelope instead of executing.
func (l *loop) recordSynthesizedCall(tc openai.ToolCall, res Result, conv *[]openai.Message) error {
	res.ToolCallID = tc.ID
	rowID, err := l.insertToolCall(tc.ID, tc.Name, tc.Arguments)
	if err != nil {
		return err
	}
	l.emitEvent(events.ToolStarted, map[string]any{
		"run_id": l.opts.RunID, "tool_call_id": tc.ID, "name": tc.Name,
	})
	if err := l.finishToolCall(rowID, res); err != nil {
		return err
	}
	l.emitEvent(events.ToolCompleted, map[string]any{
		"run_id": l.opts.RunID, "tool_call_id": tc.ID, "name": tc.Name, "status": string(res.Status),
	})
	toolMsg := openai.Message{
		Role:       RoleTool,
		Text:       renderToolMessage(l.toolOrUnknown(tc.Name), res),
		ToolCallID: tc.ID,
	}
	if err := l.persistMessage(toolMsg); err != nil {
		return err
	}
	*conv = append(*conv, toolMsg)
	return nil
}

// skippedSourceSummary renders the skipped-source map for the corrective
// observation: at most five entries, reasons clipped, so one hostile page
// cannot grow the observation without bound.
func skippedSourceSummary(skipped map[string]string) string {
	if len(skipped) == 0 {
		return ""
	}
	var parts []string
	n := 0
	for url, reason := range skipped {
		n++
		if n > 5 {
			parts = append(parts, fmt.Sprintf("…and %d more", len(skipped)-5))
			break
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", url, shortReason(reason)))
	}
	return strings.Join(parts, "; ")
}

// shortReason clips a tool error line for use inside a synthesized
// observation (the full line stays on the tool_calls row and the
// model-facing message of the failed call itself).
func shortReason(s string) string {
	if len(s) <= 120 {
		return s
	}
	return s[:117] + "…"
}

// clipFeedback bounds a synthesized observation: it is model-facing text
// and log-facing text at once, so it stays far under a message-size fault
// line while keeping the actionable parts (count, outcomes, guidance).
func clipFeedback(s string) string {
	if len(s) <= 1500 {
		return s
	}
	return s[:1497] + "…"
}
