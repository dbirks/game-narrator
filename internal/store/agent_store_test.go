package store

import (
	"fmt"
	"testing"
	"time"
)

// Agent run log tests over migration 0007 (docs/spec/03 "durable tool/job
// log and resumable plan"). The invariants under test:
//
//   - state and status vocabularies are the closed sets of the schema CHECKs;
//   - JSON columns refuse invalid JSON at the write edge;
//   - a tool call starts pending and ends with exactly one durable outcome
//     (a second finish is a conflict, not an overwrite);
//   - message and call seqs are per-run and strictly ascending;
//   - the resume predicate (pending rows) is exactly the worklist a restart
//     may execute.

func agentTestRun(t *testing.T, id string) AgentRun {
	t.Helper()
	return AgentRun{
		ID:            id,
		State:         AgentStateRunning,
		PlanJSON:      []byte(`{"steps":["identify","import"]}`),
		BudgetJSON:    []byte(`{"max_steps":24,"max_tool_calls":60}`),
		PromptVersion: "onboarding-1",
		Model:         "test-model",
	}
}

func TestAgentRunCreateGetState(t *testing.T) {
	ctx := testCtx(t)
	s := openIn(t)
	_, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	a := NewAgentStore(s)

	if err := a.CreateAgentRun(ctx, agentTestRun(t, "run-1")); err != nil {
		t.Fatalf("CreateAgentRun: %v", err)
	}
	run, err := a.GetAgentRun(ctx, "run-1")
	if err != nil {
		t.Fatalf("GetAgentRun: %v", err)
	}
	if run.State != AgentStateRunning || run.PromptVersion != "onboarding-1" || run.Model != "test-model" {
		t.Fatalf("run = %+v", run)
	}
	if string(run.PlanJSON) != `{"steps":["identify","import"]}` {
		t.Fatalf("plan = %s", run.PlanJSON)
	}

	// Invalid states and malformed JSON are refused at the write edge.
	if err := a.CreateAgentRun(ctx, AgentRun{ID: "run-bad", State: "exploded", PlanJSON: []byte(`{}`), BudgetJSON: []byte(`{}`), PromptVersion: "v"}); err == nil {
		t.Fatal("invalid state accepted")
	}
	if err := a.CreateAgentRun(ctx, AgentRun{ID: "run-bad2", State: AgentStateRunning, PlanJSON: []byte(`{nope`), BudgetJSON: []byte(`{}`), PromptVersion: "v"}); err == nil {
		t.Fatal("invalid plan_json accepted")
	}
	if err := a.CreateAgentRun(ctx, AgentRun{ID: "run-bad3", State: AgentStateRunning, PlanJSON: []byte(`{}`), BudgetJSON: []byte(`{}`), PromptVersion: ""}); err == nil {
		t.Fatal("empty prompt_version accepted")
	}

	for _, st := range []string{AgentStateBlocked, AgentStateReview, AgentStateCancelled, AgentStateSucceeded} {
		if err := a.SetAgentRunState(ctx, "run-1", st, "reason-x"); err != nil {
			t.Fatalf("SetAgentRunState(%s): %v", st, err)
		}
	}
	run, err = a.GetAgentRun(ctx, "run-1")
	if err != nil || run.State != AgentStateSucceeded || run.StopReason != "reason-x" {
		t.Fatalf("final run = %+v err %v", run, err)
	}
}

func TestAgentMessagesSequencing(t *testing.T) {
	ctx := testCtx(t)
	s := openIn(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	a := NewAgentStore(s)
	if err := a.CreateAgentRun(ctx, agentTestRun(t, "run-m")); err != nil {
		t.Fatalf("create: %v", err)
	}
	for i, role := range []string{AgentRoleSystem, AgentRoleUser, AgentRoleAssistant, AgentRoleTool} {
		content := []byte(fmt.Sprintf(`{"role":"%s","content":"m%d"}`, role, i))
		seq, err := a.AppendAgentMessage(ctx, "run-m", role, content)
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seq != int64(i) {
			t.Fatalf("seq = %d, want %d", seq, i)
		}
	}
	if _, err := a.AppendAgentMessage(ctx, "run-m", "wizard", []byte(`{}`)); err == nil {
		t.Fatal("invalid role accepted")
	}
	if _, err := a.AppendAgentMessage(ctx, "run-m", AgentRoleUser, []byte(`{bad`)); err == nil {
		t.Fatal("invalid content_json accepted")
	}
	msgs, err := a.ListAgentMessages(ctx, "run-m")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	for i, m := range msgs {
		if m.Seq != int64(i) || m.RunID != "run-m" {
			t.Fatalf("msg %d = %+v", i, m)
		}
	}
}

func TestAgentToolCallLifecycle(t *testing.T) {
	ctx := testCtx(t)
	s := openIn(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	a := NewAgentStore(s)
	if err := a.CreateAgentRun(ctx, agentTestRun(t, "run-t")); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()

	// A finished insert must carry the result envelope.
	if _, err := a.InsertAgentToolCall(ctx, AgentToolCall{
		ID: "tc-1", RunID: "run-t", Name: "echo_tool", ArgsJSON: []byte(`{"text":"a"}`),
		Status: AgentToolSucceeded, ResultJSON: []byte(`{"schema_version":1}`), FinishedAt: &now,
	}); err != nil {
		t.Fatalf("insert finished: %v", err)
	}
	if _, err := a.InsertAgentToolCall(ctx, AgentToolCall{
		ID: "tc-1b", RunID: "run-t", Name: "echo_tool", ArgsJSON: []byte(`{}`),
		Status: AgentToolSucceeded,
	}); err == nil {
		t.Fatal("finished status without result_json accepted")
	}
	// A pending insert must not carry a result.
	if _, err := a.InsertAgentToolCall(ctx, AgentToolCall{
		ID: "tc-2", RunID: "run-t", Name: "echo_tool", ArgsJSON: []byte(`{}`),
		Status: AgentToolPending, ResultJSON: []byte(`{}`),
	}); err == nil {
		t.Fatal("pending with result accepted")
	}
	// Pending row insert.
	seq, err := a.InsertAgentToolCall(ctx, AgentToolCall{
		ID: "tc-2", RunID: "run-t", Name: "echo_tool", ArgsJSON: []byte(`{"text":"b"}`),
		Status: AgentToolPending, StartedAt: now,
	})
	if err != nil {
		t.Fatalf("insert pending: %v", err)
	}
	if seq != 1 {
		t.Fatalf("seq = %d, want 1", seq)
	}

	pending, err := a.PendingAgentToolCalls(ctx, "run-t")
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "tc-2" {
		t.Fatalf("pending = %+v, want only tc-2", pending)
	}

	// Finish the pending row; a duplicate finish must conflict.
	if err := a.FinishAgentToolCall(ctx, "tc-2", AgentToolSucceeded, []byte(`{"schema_version":1,"status":"succeeded"}`), now, ""); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := a.FinishAgentToolCall(ctx, "tc-2", AgentToolRefused, []byte(`{"schema_version":1}`), now, "late"); err == nil {
		t.Fatal("second finish overwrote the first outcome")
	}
	if err := a.FinishAgentToolCall(ctx, "tc-2", AgentToolPending, []byte(`{}`), now, ""); err == nil {
		t.Fatal("finish to pending accepted")
	}

	rows, err := a.ListAgentToolCalls(ctx, "run-t")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if rows[0].Status != AgentToolSucceeded || rows[1].Status != AgentToolSucceeded {
		t.Fatalf("statuses = %s, %s", rows[0].Status, rows[1].Status)
	}
	if rows[1].FinishedAt == nil {
		t.Fatal("finished_at missing on finished row")
	}
}

// The resume predicate is total: after a crash mid-run, the only rows a
// restart may execute are the pending ones, and finished rows keep their
// stored outcome across a reopen.
func TestAgentToolCallPendingSurvivesReopen(t *testing.T) {
	ctx := testCtx(t)
	dir := t.TempDir()
	s, err := Open(ctx, dir+"/"+FileName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	a := NewAgentStore(s)
	if err := a.CreateAgentRun(ctx, agentTestRun(t, "run-r")); err != nil {
		t.Fatalf("create: %v", err)
	}
	now := time.Now().UTC()
	_, err = a.InsertAgentToolCall(ctx, AgentToolCall{
		ID: "tc-a", RunID: "run-r", Name: "import_tool", ArgsJSON: []byte(`{"asset":"1"}`),
		Status: AgentToolSucceeded, ResultJSON: []byte(`{"ok":true}`), FinishedAt: &now,
	})
	if err != nil {
		t.Fatalf("insert a: %v", err)
	}
	_, err = a.InsertAgentToolCall(ctx, AgentToolCall{
		ID: "tc-b", RunID: "run-r", Name: "import_tool", ArgsJSON: []byte(`{"asset":"2"}`),
		Status: AgentToolPending, StartedAt: now,
	})
	if err != nil {
		t.Fatalf("insert b: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen: the pending worklist is intact, the finished row untouched.
	s2, err := Open(ctx, dir+"/"+FileName)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	a2 := NewAgentStore(s2)
	pending, err := a2.PendingAgentToolCalls(ctx, "run-r")
	if err != nil {
		t.Fatalf("pending after reopen: %v", err)
	}
	if len(pending) != 1 || pending[0].ID != "tc-b" {
		t.Fatalf("pending after reopen = %+v, want only tc-b", pending)
	}
	rows, err := a2.ListAgentToolCalls(ctx, "run-r")
	if err != nil {
		t.Fatalf("list after reopen: %v", err)
	}
	if len(rows) != 2 || rows[0].Status != AgentToolSucceeded {
		t.Fatalf("rows after reopen = %+v", rows)
	}
}
