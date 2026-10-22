package openai

// Tool-calling tests. The first group plays a *recorded-style fake gateway*
// (httptest server echoing the exact OpenAI wire shapes observed against
// qwen3-8-27b on 2026-10-17) — this proves the request and response plumbing
// without a live endpoint, and is NOT evidence of a real end-to-end run
// (AGENTS.md rule 3). The final test is live against the configured
// agent_reasoning gateway and skips with NOT RUN when it is absent.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

const echoToolSchema = `{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`

// fakeGateway serves one scripted chat/completions exchange and returns the
// raw request body so tests can assert what the adapter actually sent.
func fakeGateway(t *testing.T, respond func(gotBody map[string]any) []byte) (*Client, *map[string]any) {
	t.Helper()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat/completions" && r.Method == http.MethodPost {
			_ = json.NewDecoder(r.Body).Decode(&got)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(respond(got))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "test-token-do-not-log", "fake-tool-model")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, &got
}

// testCtx matches the package convention (context.Background): the fake
// gateway always answers, so no deadline is needed.
func testCtx() context.Context { return context.Background() }

// The request must carry tools and tool_choice exactly as the OpenAI shape
// defines them, and the assistant tool_calls / tool result messages must
// round-trip into the wire form the gateway expects.
func TestToolCallingRequestWireShape(t *testing.T) {
	c, got := fakeGateway(t, func(b map[string]any) []byte {
		return []byte(`{"id":"cmpl-1","object":"chat.completion","model":"fake-tool-model",
			"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`)
	})
	_, err := c.Chat(testCtx(), ChatRequest{
		Messages: []Message{
			{Role: "system", Text: "sys"},
			{Role: "user", Text: "hi"},
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "echo_tool", Arguments: json.RawMessage(`{"text":"ping"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Text: `{"ok":true}`},
		},
		Tools:      []ToolDef{{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice: ToolChoiceAuto,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	tools, ok := (*got)["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %v, want one declaration", (*got)["tools"])
	}
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "echo_tool" {
		t.Errorf("tool name = %v", fn["name"])
	}
	if tc, _ := (*got)["tool_choice"].(string); tc != "auto" {
		t.Errorf("tool_choice = %v, want auto", (*got)["tool_choice"])
	}
	msgs := (*got)["messages"].([]any)
	asst := msgs[2].(map[string]any)
	calls := asst["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("assistant tool_calls = %v", calls)
	}
	c0 := calls[0].(map[string]any)
	if c0["id"] != "call_1" || c0["type"] != "function" {
		t.Errorf("tool_call[0] = %v", c0)
	}
	if c0["function"].(map[string]any)["arguments"] != `{"text":"ping"}` {
		t.Errorf("arguments = %v (must be a JSON string on the wire)", c0["function"])
	}
	toolMsg := msgs[3].(map[string]any)
	if toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != `{"ok":true}` {
		t.Errorf("tool message = %v", toolMsg)
	}
}

// A parallel tool_calls response (finish_reason "tool_calls", content null,
// two calls) must parse into two ToolCalls with valid JSON arguments.
func TestToolCallingParallelResponse(t *testing.T) {
	c, _ := fakeGateway(t, func(b map[string]any) []byte {
		return []byte(`{"id":"cmpl-2","object":"chat.completion","model":"fake-tool-model",
			"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"echo_tool","arguments":"{\"text\":\"one\"}"}},
				{"id":"call_b","type":"function","function":{"name":"get_time","arguments":"{\"tz\":\"UTC\"}"}}
			]}}],
			"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	})
	resp, err := c.Chat(testCtx(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "use the tools"}},
		Tools: []ToolDef{
			{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)},
			{Name: "get_time", Description: "Time.", Parameters: json.RawMessage(`{"type":"object","properties":{"tz":{"type":"string"}}}`)},
		},
		ToolChoice: ToolChoiceRequired,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.FinishReason != "tool_calls" {
		t.Errorf("FinishReason = %q, want tool_calls", resp.FinishReason)
	}
	if resp.Text != "" {
		t.Errorf("Text = %q, want empty on a pure tool_calls turn", resp.Text)
	}
	if len(resp.ToolCalls) != 2 {
		t.Fatalf("ToolCalls = %d, want 2", len(resp.ToolCalls))
	}
	if resp.ToolCalls[0].Name != "echo_tool" || resp.ToolCalls[1].Name != "get_time" {
		t.Errorf("names = %v, %v", resp.ToolCalls[0].Name, resp.ToolCalls[1].Name)
	}
	args, err := resp.ToolCalls[0].ArgumentsObject()
	if err != nil {
		t.Fatalf("ArgumentsObject: %v", err)
	}
	if args["text"] != "one" {
		t.Errorf("args = %v", args)
	}
}

// A tool call whose arguments string is not JSON is a protocol violation:
// the whole response must fail as bad_schema, never execute garbage.
func TestToolCallingMalformedArgumentsRejected(t *testing.T) {
	c, _ := fakeGateway(t, func(b map[string]any) []byte {
		return []byte(`{"id":"cmpl-3","object":"chat.completion","model":"fake-tool-model",
			"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_x","type":"function","function":{"name":"echo_tool","arguments":"{not json"}}]}}]}`)
	})
	_, err := c.Chat(testCtx(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "go"}},
		Tools:    []ToolDef{{Name: "echo_tool", Parameters: json.RawMessage(echoToolSchema)}},
	})
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindBadSchema {
		t.Fatalf("err = %v, want bad_schema", err)
	}
}

func TestToolDefValidation(t *testing.T) {
	c, _ := fakeGateway(t, func(b map[string]any) []byte {
		return []byte(`{"id":"cmpl-4","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	})
	_, err := c.Chat(testCtx(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "go"}},
		Tools:    []ToolDef{{Name: "", Parameters: json.RawMessage(echoToolSchema)}},
	})
	if err == nil {
		t.Fatal("empty tool name accepted, want bad_schema")
	}
	_, err = c.Chat(testCtx(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "go"}},
		Tools:    []ToolDef{{Name: "t", Parameters: json.RawMessage(`[1]`)}},
	})
	if err == nil {
		t.Fatal("non-object parameters accepted, want bad_schema")
	}
	// Named tool choices must be valid; a keyword that collides with a tool
	// name is one of the three keywords itself and stays a keyword.
	if !ToolChoice("echo_tool").Valid() {
		t.Error("named tool choice should be valid")
	}
	if (ToolChoice(ToolChoiceAuto)).Valid() == false {
		t.Error("keyword choices must be valid")
	}
}

// Live: one tool definition against the configured agent_reasoning gateway;
// the model must answer with a tool call whose arguments parse as JSON.
// NOT RUN (skip) when the gateway env is absent.
//
// This exercises the app's real usage: tool_choice auto. The gateway's 27B
// rejects tool_choice=required (2026-10-17 NInfer build), so the app degrades
// to auto (docs/spec/06). The separate required probe
// (TestLiveToolChoiceRequiredIsACapabilityFinding) records that capability
// finding rather than asserting it.
//
// The request also pins reasoning_effort=low with a 1024-token budget: with
// the model's default effort and MaxTokens 256, ~1/5 of runs spent the whole
// budget on reasoning and ended finish_reason=length with an empty answer
// (observed live 2026-10-18). That is exactly the failure
// ErrOutputBudgetExhausted classifies; the pinned effort keeps the tool call
// inside the budget.
func TestLiveToolCallingExecutesOneTool(t *testing.T) {
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
	cfg.Timeout = 90 * time.Second
	client, err := NewFromRole(cfg)
	if err != nil {
		t.Fatalf("NewFromRole: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages:        []Message{{Role: "user", Text: `Use the echo_tool function to echo the exact text "agent-live". Do not answer anything else.`}},
		MaxTokens:       1024,
		Tools:           []ToolDef{{Name: "echo_tool", Description: "Echo the given text back verbatim.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice:      ToolChoiceAuto,
		ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("live tool Chat: %v", err)
	}
	if len(resp.ToolCalls) == 0 {
		t.Fatalf("live model did not call a tool: finish=%s text=%q", resp.FinishReason, truncate(resp.Text, 120))
	}
	call := resp.ToolCalls[0]
	if call.Name != "echo_tool" {
		t.Fatalf("model called %q, want echo_tool", call.Name)
	}
	obj, err := call.ArgumentsObject()
	if err != nil {
		t.Fatalf("arguments did not parse: %v", err)
	}
	if _, ok := obj["text"]; !ok {
		t.Errorf("arguments %v missing 'text'", obj)
	}
	// Log-safe: model id, finish reason, latency. Never the URL or token.
	t.Logf("live tool call: model=%s finish=%s tool=%s args_keys=%d latency=%s usage=%d/%d reasoning_tokens=%d",
		resp.Model, resp.FinishReason, call.Name, len(obj), resp.Latency.Round(time.Millisecond),
		resp.Usage.Prompt, resp.Usage.Completion, resp.Usage.Reasoning)
}

// TestLiveToolChoiceRequiredIsACapabilityFinding probes the real behaviour of
// tool_choice=required against the configured gateway and records a CAPABILITY
// FINDING either way (pass with a logged finding), mirroring the JSON-mode
// test. It does NOT assert required is available: the app degrades to auto
// (docs/spec/06), so a rejection is expected and honest, not a failure.
func TestLiveToolChoiceRequiredIsACapabilityFinding(t *testing.T) {
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
	model := strings.TrimSpace(os.Getenv("GAME_NARRATOR_LIVE_AGENT_MODEL"))
	if model == "" {
		model = strings.TrimSpace(cfg.Model)
	}
	if model == "" {
		model = "flint" // the collective replaced qwen3-8-27b with flint on 2026-10-21
	}
	cfg.Model = model
	cfg.Timeout = 60 * time.Second
	client, err := NewFromRole(cfg)
	if err != nil {
		t.Fatalf("NewFromRole: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := client.Chat(ctx, ChatRequest{
		Messages:   []Message{{Role: "user", Text: `Use the echo_tool function to echo the exact text "agent-live".`}},
		MaxTokens:  256,
		Tools:      []ToolDef{{Name: "echo_tool", Description: "Echo the given text back verbatim.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice: ToolChoiceRequired,
	})
	if err != nil {
		// Expected on the NInfer build: a typed capability rejection.
		var pe *providers.Error
		if errors.As(err, &pe) && errors.Is(err, ErrUnsupportedToolChoice) {
			t.Logf("CAPABILITY FINDING: live gateway rejects tool_choice=required (model=%s); %s. "+
				"The app degrades to auto (docs/spec/06).", model, pe)
			return
		}
		t.Fatalf("live tool_choice=required probe: %v, want ErrUnsupportedToolChoice", err)
	}
	// The gateway accepted required: record that (unexpected) capability and
	// still require a tool call to have arrived.
	t.Logf("CAPABILITY FINDING: live gateway accepts tool_choice=required (model=%s); forced tool calls are available", model)
	if len(resp.ToolCalls) == 0 {
		t.Fatalf("gateway accepted required but no tool call arrived: finish=%s text=%q", resp.FinishReason, truncate(resp.Text, 120))
	}
	_ = client.Capabilities()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return fmt.Sprintf("%s…", s[:n])
}
