package agent

// Resume mechanics: the worklist that makes docs/spec/03 scenario 7 hold —
// "Interrupted 3,000-line import → restart resumes without duplicates".
//
// The total worklist after a restart, in order:
//
//  1. persisted assistant tool-call message with NO tool_calls row for a
//     call id     -> the crash happened before the row insert: insert a
//     pending row, then execute (case 1);
//  2. a pending row for a call id  -> execute exactly once and record the
//     outcome (case 2);
//  3. a finished row for a call id -> never re-execute; if the tool message
//     is missing (crash between the outcome write and the message append)
//     re-render it from the stored result (case 3).
//
// After the worklist is drained the conversation ends with a tool result for
// every assistant tool call, so the next model round-trip is well-formed.
//
// Contexts: the run context (runCtx) gates the model and the tool handlers;
// the durable writes use fresh short contexts (persistCtx), because a run
// that was cancelled mid-call must still record its outcome — the
// cancellation is exactly what the log is for.

import (
	"context"
	"encoding/json"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/store"
)

// persistFresh creates the run row and stores the opening messages. An
// existing non-terminal run row (a retry after a cancelled fresh run) is
// reused: the opening messages are appended only when the run has none yet.
func (l *loop) persistFresh(conv []openai.Message) error {
	if l.store == nil {
		return nil
	}
	ctx, cancel := persistCtx()
	defer cancel()
	existing, err := l.store.GetAgentRun(ctx, l.opts.RunID)
	switch {
	case err == nil && existing.ID != "":
		if existing.State == string(StateSucceeded) || existing.State == string(StateFailed) {
			return nil // a closed run is not restarted by persistFresh
		}
		// Reused row: append opening messages only when none were stored.
		msgs, merr := l.store.ListAgentMessages(ctx, l.opts.RunID)
		if merr != nil {
			return merr
		}
		if len(msgs) > 0 {
			return nil
		}
	default:
		plan, perr := json.Marshal(l.opts.Plan)
		if perr != nil || len(plan) == 0 {
			plan = []byte("{}")
		}
		budgetJSON, _ := json.Marshal(l.budget)
		if err := l.store.CreateAgentRun(ctx, store.AgentRun{
			ID:            l.opts.RunID,
			ProfileID:     l.opts.ProfileID,
			State:         store.AgentStateRunning,
			PlanJSON:      plan,
			BudgetJSON:    budgetJSON,
			PromptVersion: PromptVersion,
			Model:         l.opts.ModelLabel,
		}); err != nil {
			return err
		}
	}
	for _, m := range conv {
		if err := l.persistMessage(m); err != nil {
			return err
		}
	}
	return nil
}

// insertToolCall records a model-requested call as pending and returns its
// row id (the model's call id). A pre-existing row for the id is reused —
// this makes the insert idempotent across a crash between the assistant
// message persist and the row insert.
func (l *loop) insertToolCall(id, name string, args json.RawMessage) (string, error) {
	if l.store == nil {
		return id, nil
	}
	ctx, cancel := persistCtx()
	defer cancel()
	rows, err := l.store.ListAgentToolCalls(ctx, l.opts.RunID)
	if err != nil {
		return "", err
	}
	for _, r := range rows {
		if r.ID == id {
			return id, nil // pending or finished row already exists
		}
	}
	_, err = l.store.InsertAgentToolCall(ctx, store.AgentToolCall{
		ID:        id,
		RunID:     l.opts.RunID,
		Name:      name,
		ArgsJSON:  normaliseArgs(args),
		Status:    store.AgentToolPending,
		StartedAt: time.Now().UTC(),
	})
	if err != nil {
		return "", err
	}
	return id, nil
}

// finishToolCall persists the durable outcome of an executed call. The row
// is guarded on status='pending' inside the store, so a duplicate finish
// (crash-restart racing a slow tool) cannot overwrite the first outcome.
func (l *loop) finishToolCall(rowID string, res Result) error {
	if l.store == nil {
		return nil
	}
	raw, err := json.Marshal(res)
	if err != nil {
		return err
	}
	ctx, cancel := persistCtx()
	defer cancel()
	return l.store.FinishAgentToolCall(ctx, rowID, string(res.Status), raw, time.Now().UTC(), res.Error)
}

// executePending drains the resume worklist. On return, *conv holds the
// rebuilt conversation plus one tool message per assistant tool call; out is
// stopped (State set to a non-running value) when a pending call hit the
// review boundary or the run context was cancelled.
func (l *loop) executePending(runCtx context.Context, conv *[]openai.Message, out *Outcome) error {
	if l.store == nil {
		return nil
	}
	ctx, cancel := persistCtx()
	defer cancel()
	msgs := *conv
	lastIdx := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			lastIdx = i
			break
		}
	}
	if lastIdx < 0 {
		return nil
	}
	rows, err := l.store.ListAgentToolCalls(ctx, l.opts.RunID)
	if err != nil {
		return err
	}
	byID := make(map[string]store.AgentToolCall, len(rows))
	for _, r := range rows {
		byID[r.ID] = r
	}
	haveToolMsg := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == RoleTool {
			haveToolMsg[m.ToolCallID] = true
		}
	}

	for _, tc := range msgs[lastIdx].ToolCalls {
		row, exists := byID[tc.ID]
		switch {
		case exists && row.Status == store.AgentToolPending:
			// Case 2: the call started before the crash and never recorded
			// an outcome. Execute it — exactly once, ever.
			if err := l.runCall(runCtx, tc.ID, tc.Name, tc.Arguments, tc.ID, conv, out); err != nil {
				return err
			}
		case exists:
			// Case 3: durable outcome. Never re-execute.
			if !haveToolMsg[tc.ID] {
				var res Result
				if err := json.Unmarshal(row.ResultJSON, &res); err != nil {
					return err
				}
				*conv = append(*conv, openai.Message{
					Role:       RoleTool,
					Text:       renderToolMessage(l.toolOrUnknown(tc.Name), res),
					ToolCallID: tc.ID,
				})
			}
		default:
			// Case 1: crash before the row insert. Insert, then execute.
			if err := l.runCall(runCtx, tc.ID, tc.Name, tc.Arguments, tc.ID, conv, out); err != nil {
				return err
			}
		}
		if out.State != StateRunning {
			return nil
		}
	}
	return nil
}

// runCall executes one call end-to-end: pending row (if needed), executor,
// durable outcome, tool message, events. It sets *out when the run stops.
func (l *loop) runCall(runCtx context.Context, id, name string, args json.RawMessage, rowID string, conv *[]openai.Message, out *Outcome) error {
	if l.store == nil {
		rowID = id
	} else {
		rid, err := l.insertToolCall(id, name, args)
		if err != nil {
			return err
		}
		rowID = rid
	}
	l.emitEvent(events.ToolStarted, map[string]any{"run_id": l.opts.RunID, "tool_call_id": id, "name": name})

	res := l.exec.Execute(runCtx, Call{ID: id, Name: name, Arguments: args})
	if err := l.finishToolCall(rowID, res); err != nil {
		return err
	}
	l.emitEvent(events.ToolCompleted, map[string]any{
		"run_id": l.opts.RunID, "tool_call_id": id, "name": name, "status": string(res.Status),
	})

	*conv = append(*conv, openai.Message{
		Role:       RoleTool,
		Text:       renderToolMessage(l.toolOrUnknown(name), res),
		ToolCallID: id,
	})
	if res.ReviewRequired {
		l.emitEvent(events.ReviewRequired, map[string]any{"run_id": l.opts.RunID, "tool": name, "tool_call_id": id})
		out.State = StateReview
		out.StopReason = StopReasonReview
	} else if runCtx.Err() != nil {
		out.State = StateCancelled
		out.StopReason = StopReasonCancelled
	}
	return nil
}

// normaliseArgs renders empty/null args as {} so the persisted args_json is
// always an object the executor and the resume path can treat alike.
func normaliseArgs(args json.RawMessage) json.RawMessage {
	if len(args) == 0 || string(args) == "null" {
		return json.RawMessage("{}")
	}
	return args
}

// finish records the final state and stop reason on the run row and returns
// the filled outcome.
func (l *loop) finish(out *Outcome, state State, reason string) (Outcome, error) {
	out.State = state
	out.StopReason = reason
	if l.store != nil {
		ctx, cancel := persistCtx()
		defer cancel()
		if err := l.store.SetAgentRunState(ctx, l.opts.RunID, string(state), reason); err != nil {
			return *out, err
		}
	}
	return *out, nil
}

// finishCancelled records a caller cancellation. The run is NOT failed: it
// is resumable exactly where it stopped (its worklist is the pending rows
// and the not-yet-inserted calls of the last assistant message).
func (l *loop) finishCancelled(out *Outcome) (Outcome, error) {
	return l.finish(out, StateCancelled, StopReasonCancelled)
}
