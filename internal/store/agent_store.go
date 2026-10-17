package store

// The durable agent run log over migration 0007 (docs/spec/03 "durable
// tool/job log and resumable plan"). Three properties are load-bearing and
// live here rather than in any caller:
//
//  1. Conversation replay is exact. agent_messages are appended with a
//     per-run seq computed in the write transaction, so the resume path
//     (internal/agent.RunResume) replays the same message order a live run
//     saw, and a concurrent appender cannot interleave a duplicate seq.
//  2. Tool-call outcomes are the resume predicate. A row with status
//     'pending' is the only row a resume may execute; every other status
//     means a durable outcome already exists and the call is never
//     re-executed (docs/spec/03 scenario 7, "resumes without duplicates").
//     The table CHECK ties status to result_json/finished_at, so the
//     predicate is enforced by the schema, not by caller discipline.
//  3. JSON is guarded at the write edge. plan_json, budget_json,
//     content_json, args_json and result_json are json_valid-checked in the
//     schema; the writers below reject malformed JSON with the same error
//     style the rest of this package uses, so a bad row can only come from
//     a hand edit.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Agent run states: the closed vocabulary of docs/spec/03, mirrored by the
// CHECK on agent_runs.state.
const (
	AgentStateQueued    = "queued"
	AgentStateRunning   = "running"
	AgentStateBlocked   = "blocked"
	AgentStateReview    = "review"
	AgentStateFailed    = "failed"
	AgentStateSucceeded = "succeeded"
	AgentStateCancelled = "cancelled"
)

// Agent tool-call statuses: 'pending' is the not-yet-outcome state; the rest
// carry a persisted result envelope (docs/spec/17).
const (
	AgentToolPending   = "pending"
	AgentToolSucceeded = "succeeded"
	AgentToolFailed    = "failed"
	AgentToolRefused   = "refused"
	AgentToolTimeout   = "timeout"
	AgentToolCancelled = "cancelled"
)

// Agent message roles: the chat roles of the provider-neutral wire form.
const (
	AgentRoleSystem    = "system"
	AgentRoleUser      = "user"
	AgentRoleAssistant = "assistant"
	AgentRoleTool      = "tool"
)

// AgentRun is one durable agent run row.
type AgentRun struct {
	ID            string
	ProfileID     string // "" while the run has no profile yet
	State         string
	PlanJSON      []byte
	BudgetJSON    []byte
	PromptVersion string
	Model         string
	StopReason    string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// AgentMessage is one persisted conversational step.
type AgentMessage struct {
	RunID       string
	Seq         int64
	Role        string
	ContentJSON []byte
	CreatedAt   time.Time
}

// AgentToolCall is one persisted tool invocation. ResultJSON is nil exactly
// while Status is AgentToolPending.
type AgentToolCall struct {
	ID         string
	RunID      string
	Seq        int64
	Name       string
	ArgsJSON   []byte
	ResultJSON []byte
	Status     string
	StartedAt  time.Time
	FinishedAt *time.Time
	Error      string
}

// AgentStore provides the agent run log operations.
type AgentStore struct {
	s *Store
}

// NewAgentStore builds the agent run log over an open store.
func NewAgentStore(s *Store) *AgentStore { return &AgentStore{s: s} }

// validateJSONOrEmpty accepts an empty value (stored as {}) or valid JSON,
// and rejects anything else: a malformed plan/budget must fail the write,
// not be silently replaced.
func validateJSONOrEmpty(op string, raw []byte, field string) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("store: %s: %s is not valid JSON", op, field)
	}
	return string(raw), nil
}

func validAgentState(s string) bool {
	switch s {
	case AgentStateQueued, AgentStateRunning, AgentStateBlocked, AgentStateReview,
		AgentStateFailed, AgentStateSucceeded, AgentStateCancelled:
		return true
	}
	return false
}

func validAgentToolStatus(s string) bool {
	switch s {
	case AgentToolPending, AgentToolSucceeded, AgentToolFailed, AgentToolRefused,
		AgentToolTimeout, AgentToolCancelled:
		return true
	}
	return false
}

func validAgentRole(r string) bool {
	switch r {
	case AgentRoleSystem, AgentRoleUser, AgentRoleAssistant, AgentRoleTool:
		return true
	}
	return false
}

// CreateAgentRun inserts a new run. The caller supplies the id (a ULID);
// state must be a run state, plan/budget must be valid JSON, and the
// prompt version identifies the template generation (migration 0007).
func (a *AgentStore) CreateAgentRun(ctx context.Context, r AgentRun) error {
	const op = "create agent run"
	if strings.TrimSpace(r.ID) == "" {
		return fmt.Errorf("store: %s: id is required", op)
	}
	if !validAgentState(r.State) {
		return fmt.Errorf("store: %s: state %q is not a run state", op, r.State)
	}
	if len(r.PromptVersion) < 1 || len(r.PromptVersion) > 32 {
		return fmt.Errorf("store: %s: prompt_version must be 1..32 chars", op)
	}
	plan, err := validateJSONOrEmpty(op, r.PlanJSON, "plan_json")
	if err != nil {
		return err
	}
	budget, err := validateJSONOrEmpty(op, r.BudgetJSON, "budget_json")
	if err != nil {
		return err
	}
	if err := a.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, op,
			`INSERT INTO agent_runs (id, profile_id, state, plan_json, budget_json,
				prompt_version, model, stop_reason, created_at, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.ID, nullStr(r.ProfileID), r.State, string(plan), string(budget),
			r.PromptVersion, r.Model, r.StopReason, nowStamp(), nowStamp())
		return err
	}); err != nil {
		return err
	}
	return nil
}

// GetAgentRun loads one run.
func (a *AgentStore) GetAgentRun(ctx context.Context, id string) (AgentRun, error) {
	const op = "get agent run"
	var r AgentRun
	var profile sql.NullString
	var created, updated string
	err := a.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, op,
			`SELECT id, profile_id, state, plan_json, budget_json, prompt_version,
				model, stop_reason, created_at, updated_at FROM agent_runs WHERE id = ?`, id,
		).Scan(&r.ID, &profile, &r.State, &r.PlanJSON, &r.BudgetJSON, &r.PromptVersion,
			&r.Model, &r.StopReason, &created, &updated)
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return AgentRun{}, notFound(op, "run "+id)
		}
		return AgentRun{}, mapReadError(op, err)
	}
	if profile.Valid {
		r.ProfileID = profile.String
	}
	r.CreatedAt, err = parseStamp(created)
	if err != nil {
		return AgentRun{}, mapReadError(op, err)
	}
	r.UpdatedAt, err = parseStamp(updated)
	if err != nil {
		return AgentRun{}, mapReadError(op, err)
	}
	return r, nil
}

// SetAgentRunState moves the run to a new state, recording the stop reason
// for non-success stops. It is the single write path for state transitions,
// so updated_at and the state stay consistent under the CHECKs.
func (a *AgentStore) SetAgentRunState(ctx context.Context, id, state, stopReason string) error {
	const op = "set agent run state"
	if !validAgentState(state) {
		return fmt.Errorf("store: %s: state %q is not a run state", op, state)
	}
	return a.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`UPDATE agent_runs SET state = ?, stop_reason = ?, updated_at = ? WHERE id = ?`,
			state, stopReason, nowStamp(), id)
		if err != nil {
			return err
		}
		return touchedOrConflict(ctx, tx, op, res, id)
	})
}

// AppendAgentMessage persists one conversational step and returns its seq.
// The seq is computed in the write transaction (max+1), so the order is
// exactly append order even under a crash-and-restart.
func (a *AgentStore) AppendAgentMessage(ctx context.Context, runID, role string, contentJSON []byte) (int64, error) {
	const op = "append agent message"
	if !validAgentRole(role) {
		return 0, fmt.Errorf("store: %s: role %q is not a message role", op, role)
	}
	if len(contentJSON) == 0 || !json.Valid(contentJSON) {
		return 0, fmt.Errorf("store: %s: content_json must be valid JSON", op)
	}
	var seq int64
	err := a.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		if err := tx.queryRow(ctx, op,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM agent_messages WHERE run_id = ?`, runID,
		).Scan(&seq); err != nil {
			return mapReadError(op, err)
		}
		_, err := tx.exec(ctx, op,
			`INSERT INTO agent_messages (run_id, seq, role, content_json, created_at)
			 VALUES (?, ?, ?, ?, ?)`,
			runID, seq, role, string(contentJSON), nowStamp())
		return err
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// ListAgentMessages returns the run's conversation in seq order — the exact
// replay order the resume path uses.
func (a *AgentStore) ListAgentMessages(ctx context.Context, runID string) ([]AgentMessage, error) {
	const op = "list agent messages"
	var out []AgentMessage
	err := a.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT run_id, seq, role, content_json, created_at FROM agent_messages
			 WHERE run_id = ? ORDER BY seq ASC`, runID)
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				m       AgentMessage
				created string
			)
			if err := rows.Scan(&m.RunID, &m.Seq, &m.Role, &m.ContentJSON, &created); err != nil {
				return mapReadError(op, err)
			}
			m.CreatedAt, err = parseStamp(created)
			if err != nil {
				return mapReadError(op, err)
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// InsertAgentToolCall records one tool invocation. With Status
// AgentToolPending it is a call in flight (result NULL); with any other
// status it must carry the persisted result envelope. The per-run seq is
// computed in the write transaction.
func (a *AgentStore) InsertAgentToolCall(ctx context.Context, tc AgentToolCall) (int64, error) {
	const op = "insert agent tool call"
	if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.RunID) == "" {
		return 0, fmt.Errorf("store: %s: id and run_id are required", op)
	}
	if !validAgentToolStatus(tc.Status) {
		return 0, fmt.Errorf("store: %s: status %q is not a tool status", op, tc.Status)
	}
	if len(tc.ArgsJSON) == 0 || !json.Valid(tc.ArgsJSON) {
		return 0, fmt.Errorf("store: %s: args_json must be valid JSON", op)
	}
	if tc.Status != AgentToolPending {
		if len(tc.ResultJSON) == 0 || !json.Valid(tc.ResultJSON) {
			return 0, fmt.Errorf("store: %s: status %q requires a valid result_json", op, tc.Status)
		}
		if tc.FinishedAt == nil {
			return 0, fmt.Errorf("store: %s: status %q requires finished_at", op, tc.Status)
		}
	} else if tc.ResultJSON != nil || tc.FinishedAt != nil {
		return 0, fmt.Errorf("store: %s: pending calls carry no result", op)
	}
	var seq int64
	err := a.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, op,
			`SELECT COALESCE(MAX(seq), -1) + 1 FROM tool_calls WHERE run_id = ?`, tc.RunID,
		).Scan(&seq)
	})
	if err != nil {
		return 0, err
	}
	err = a.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var result any
		var finished any
		if tc.Status != AgentToolPending {
			result = string(tc.ResultJSON)
			finished = stampMS(*tc.FinishedAt)
		}
		_, err := tx.exec(ctx, op,
			`INSERT INTO tool_calls (id, run_id, seq, name, args_json, result_json,
				status, started_at, finished_at, error)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			tc.ID, tc.RunID, seq, tc.Name, string(tc.ArgsJSON), result, tc.Status,
			stampMS(tc.StartedAt), finished, tc.Error)
		return err
	})
	if err != nil {
		return 0, err
	}
	return seq, nil
}

// FinishAgentToolCall records the durable outcome of a pending call. The
// UPDATE is guarded on status='pending' so a racing duplicate finish (two
// processes, a crash-restart mid-tool) cannot overwrite the first outcome —
// the loser of the race sees touchedOrConflict and the resume predicate
// stays single-valued.
func (a *AgentStore) FinishAgentToolCall(ctx context.Context, id, status string, resultJSON []byte, finishedAt time.Time, errMsg string) error {
	const op = "finish agent tool call"
	if !validAgentToolStatus(status) || status == AgentToolPending {
		return fmt.Errorf("store: %s: status %q is not a finish status", op, status)
	}
	if len(resultJSON) == 0 || !json.Valid(resultJSON) {
		return fmt.Errorf("store: %s: result_json must be valid JSON", op)
	}
	return a.s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, op,
			`UPDATE tool_calls SET status = ?, result_json = ?, finished_at = ?, error = ?
			 WHERE id = ? AND status = 'pending'`,
			status, string(resultJSON), stampMS(finishedAt), errMsg, id)
		if err != nil {
			return err
		}
		return touchedOrConflict(ctx, tx, op, res, id)
	})
}

// ListAgentToolCalls returns the run's tool calls in seq order.
func (a *AgentStore) ListAgentToolCalls(ctx context.Context, runID string) ([]AgentToolCall, error) {
	const op = "list agent tool calls"
	var out []AgentToolCall
	err := a.s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, op,
			`SELECT id, run_id, seq, name, args_json, result_json, status,
				started_at, finished_at, error FROM tool_calls WHERE run_id = ? ORDER BY seq ASC`, runID)
		if err != nil {
			return mapReadError(op, err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				tc        AgentToolCall
				result    sql.NullString
				finished  sql.NullString
				startedAt string
			)
			if err := rows.Scan(&tc.ID, &tc.RunID, &tc.Seq, &tc.Name, &tc.ArgsJSON,
				&result, &tc.Status, &startedAt, &finished, &tc.Error); err != nil {
				return mapReadError(op, err)
			}
			if result.Valid {
				tc.ResultJSON = []byte(result.String)
			}
			if finished.Valid {
				t, err := parseStamp(finished.String)
				if err != nil {
					return mapReadError(op, err)
				}
				tc.FinishedAt = &t
			}
			t, err := parseStamp(startedAt)
			if err != nil {
				return mapReadError(op, err)
			}
			tc.StartedAt = t
			out = append(out, tc)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PendingAgentToolCalls returns the calls that have no durable outcome yet,
// in seq order. This is the resume worklist: a run with non-empty pending
// calls was interrupted mid-step and exactly these calls may be executed.
func (a *AgentStore) PendingAgentToolCalls(ctx context.Context, runID string) ([]AgentToolCall, error) {
	all, err := a.ListAgentToolCalls(ctx, runID)
	if err != nil {
		return nil, err
	}
	var out []AgentToolCall
	for _, tc := range all {
		if tc.Status == AgentToolPending {
			out = append(out, tc)
		}
	}
	return out, nil
}
