package agent

// The resume test is the acceptance check for docs/spec/03 scenario 7:
// "Interrupted 3,000-line import → restart resumes without duplicates". The
// fake model asks for THREE parallel tool calls; the second call's handler
// cancels the run context (simulating a killed process). The run must end
// cancelled with a durable worklist (call C has no row yet). A restart
// (store closed and reopened) then resumes: the handler counters must prove
// A and B were NOT re-executed, and C was executed exactly once.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers/openai"
	"github.com/dbirks/game-narrator/internal/store"
)

func TestRunResumeExecutesOnlyPendingCall(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, store.FileName)

	s1, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s1.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	es1 := store.NewAgentStore(s1)

	ech := newCountingEcho()
	reg := testRegistry(t, echoTool(t, ech.handler))
	argA := json.RawMessage(`{"text":"a"}`)
	argB := json.RawMessage(`{"text":"b"}`)
	argC := json.RawMessage(`{"text":"c"}`)

	// Phase 1: run until the second call's handler kills the process.
	ctx1, cancel1 := context.WithCancel(context.Background())
	ech.cancelOn[string(argB)] = cancel1
	defer cancel1()
	modelA := &scriptedModel{responses: []scriptedResponse{
		{toolCalls: openaiToolCalls3(argA, argB, argC)},
		{text: "all done"},
	}}
	out1, err := Run(ctx1, runOpts("run-resume", modelA, reg, es1, nil))
	if err != nil {
		t.Fatalf("phase 1: Run: %v", err)
	}
	if out1.State != StateCancelled {
		t.Fatalf("phase 1 state = %s reason %s, want cancelled", out1.State, out1.StopReason)
	}
	if got := ech.count(string(argC)); got != 0 {
		t.Fatalf("call C executed %d times in phase 1, want 0 (the kill happened before it)", got)
	}
	rows, err := es1.ListAgentToolCalls(ctx, "run-resume")
	if err != nil {
		t.Fatalf("phase 1 rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("phase 1 rows = %d, want 2 (A and B only; C was never inserted)", len(rows))
	}
	for _, r := range rows {
		if r.Status != store.AgentToolSucceeded {
			t.Fatalf("phase 1 row %s status = %s, want succeeded", r.ID, r.Status)
		}
	}

	// Simulate the process restart: close the store and reopen the file.
	if err := s1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	s2, err := store.Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if _, err := s2.Migrate(ctx); err != nil {
		t.Fatalf("Migrate (reopen): %v", err)
	}
	defer s2.Close()
	es2 := store.NewAgentStore(s2)

	// Phase 2: resume. The model only needs to answer after the pending call
	// completes.
	modelB := &scriptedModel{responses: []scriptedResponse{
		{text: "all done"},
	}}
	out2, err := Run(ctx, runOpts("run-resume", modelB, reg, es2, nil))
	if err != nil {
		t.Fatalf("phase 2: Run: %v", err)
	}
	if out2.State != StateSucceeded || out2.Answer != "all done" {
		t.Fatalf("phase 2 = %+v, want succeeded / 'all done'", out2)
	}

	// The duplicate-free contract: every call executed exactly once across
	// both phases (the handler counters are the proof, per the test design).
	for key, want := range map[string]int{
		string(argA): 1, string(argB): 1, string(argC): 1,
	} {
		if got := ech.count(key); got != want {
			t.Fatalf("call %s executed %d times, want %d (resume must not duplicate)", key, got, want)
		}
	}

	// The resume made exactly one model call, and its conversation carried a
	// tool result for every one of the three calls.
	if len(modelB.requests) != 1 {
		t.Fatalf("phase 2 model requests = %d, want 1", len(modelB.requests))
	}
	saw := map[string]bool{}
	for _, m := range modelB.requests[0].Messages {
		if m.Role == RoleTool {
			saw[m.ToolCallID] = true
		}
	}
	for _, id := range []string{"cA", "cB", "cC"} {
		if !saw[id] {
			t.Fatalf("resumed conversation lacks a tool result for %s", id)
		}
	}

	rows, err = es2.ListAgentToolCalls(ctx, "run-resume")
	if err != nil {
		t.Fatalf("phase 2 rows: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("phase 2 rows = %d, want 3", len(rows))
	}
	for _, r := range rows {
		if r.Status != store.AgentToolSucceeded {
			t.Fatalf("row %s status = %s, want succeeded", r.ID, r.Status)
		}
	}
}

// openaiToolCalls3 builds the three parallel calls used by the resume test.
func openaiToolCalls3(a, b, c json.RawMessage) []openai.ToolCall {
	return []openai.ToolCall{
		{ID: "cA", Name: "echo_tool", Arguments: a},
		{ID: "cB", Name: "echo_tool", Arguments: b},
		{ID: "cC", Name: "echo_tool", Arguments: c},
	}
}
