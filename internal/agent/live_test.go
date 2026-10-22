package agent

// Live smoke (the only live test in this package, per the task contract): a
// two-tool registry (echo_tool, get_time) drives the real configured
// agent_reasoning model (qwen3-8-27b as verified 2026-10-17) through
// router.Do. A question that needs one tool must produce a valid tool call
// that executes, and the run must end succeeded within 6 steps.
//
// NOT RUN (skip) when the gateway env is absent. Log-safe: model id, steps
// and latency only — never the gateway URL or token (AGENTS.md rule 5).

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/router"
)

func TestLiveAgentSmokeTwoTools(t *testing.T) {
	// Gate marker (ADR-0065): live tests run only under make test-models;
	// the unit gate (and any manual go test) must never call the gateway.
	if os.Getenv(providers.EnvGate) != "test-models" {
		t.Skipf("live provider test outside the test-models gate: run via make test-models (it sets %s=test-models)", providers.EnvGate)
	}
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	cfg := roles[providers.RoleAgentReasoning]
	if strings.TrimSpace(cfg.BaseURL) == "" || strings.TrimSpace(cfg.Token()) == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by GAME_NARRATOR_GATEWAY_TOKEN_ENV")
	}
	// GAME_NARRATOR_LIVE_AGENT_MODEL → the role's configured model →
	// qwen3-8-27b (the collective model verified tool-capable on 2026-10-17).
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_AGENT_MODEL"))
	if model == "" {
		model = strings.TrimSpace(cfg.Model)
	}
	if model == "" {
		model = "flint" // the collective replaced qwen3-8-27b with flint on 2026-10-21
	}
	cfg.Model = model
	roleReg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{
		providers.RoleAgentReasoning: cfg,
	})
	r := router.New(roleReg, router.Options{})

	getTime := Tool{
		Name:        "get_time",
		Description: "Get the current UTC time. No arguments.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{}}`),
		MinRole:     auth.RoleViewer,
		SideEffect:  SideEffectNone,
		Handler: func(ctx context.Context, call Call) (Result, error) {
			out, _ := json.Marshal(map[string]any{"utc": time.Now().UTC().Format(time.RFC3339)})
			return Result{Results: out}, nil
		},
	}
	echo := Tool{
		Name:        "echo_tool",
		Description: "Echo the given text verbatim.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`),
		MinRole:     auth.RoleViewer,
		SideEffect:  SideEffectNone,
		Handler: func(ctx context.Context, call Call) (Result, error) {
			var args map[string]any
			if err := json.Unmarshal(call.Arguments, &args); err != nil {
				return Result{}, err
			}
			out, _ := json.Marshal(map[string]any{"echo": args["text"]})
			return Result{Results: out}, nil
		},
	}
	reg := NewRegistry()
	for _, tool := range []Tool{echo, getTime} {
		if err := reg.Register(tool); err != nil {
			t.Fatalf("register %s: %v", tool.Name, err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	start := time.Now()
	out, err := Run(ctx, Options{
		RunID:      "live-smoke",
		Model:      RouterCall(roleReg, r),
		Registry:   reg,
		CallerRole: auth.RoleOperator,
		UserRequest: "What time is it right now? Use the get_time tool to find out, " +
			"then answer with the time in one short sentence.",
		Budget:     BudgetConfig{MaxSteps: 6, MaxToolCalls: 4},
		ModelLabel: model,
		Prompt:     PromptContext{UserRole: auth.RoleOperator},
	})
	latency := time.Since(start)
	if err != nil {
		t.Fatalf("live Run: %v", err)
	}
	if out.State != StateSucceeded {
		t.Fatalf("live run state = %s reason %s steps %d", out.State, out.StopReason, out.Steps)
	}
	if out.Steps > 6 {
		t.Fatalf("live run steps = %d, want <= 6", out.Steps)
	}
	if out.ToolCalls == 0 {
		t.Fatal("the live model never called a tool; the question required get_time")
	}
	t.Logf("live agent smoke: model=%s steps=%d tool_calls=%d latency=%s",
		model, out.Steps, out.ToolCalls, latency.Round(time.Millisecond))
}
