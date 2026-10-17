package openai

// Capability-fallback unit tests (docs/spec/06 "Model-serving version and
// health negotiation"). These drive a *synthetic* httptest gateway that plays
// the exact 400 the NInfer build returned on 2026-10-17, then the degraded
// response. They prove the adapter's request shape, retry-once, caching and
// typed-error plumbing (AGENTS.md rule 3: synthetic stub, NOT evidence of a
// real end-to-end run — the live tests in live_test.go provide that).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers"
)

// The literal 400 bodies observed against qwen3-8-27b on the rolled-back
// NInfer build (2026-10-17), redacted to the error envelope only.
const (
	jsonReject400 = `{"error":{"type":"invalid_request_error","message":"this response_format requires constrained output, which NInfer cannot guarantee; only {\"type\":\"text\"} is available"}}`
	toolReject400 = `{"error":{"type":"invalid_request_error","message":"tool_choice='required' requires at least one tool call, which NInfer cannot guarantee"}}`
)

const jsonAccept200 = `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`

const toolAccept200 = `{"model":"fake-model","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\"text\":\"ping\"}"}}]}}]}`

// scriptedGateway serves one response per request from scripted slices and
// records the decoded body of every request, so a test can assert the exact
// wire shape of each attempt (first attempt, capability-fallback retry, ...).
type scriptedGateway struct {
	mu       sync.Mutex
	srv      *httptest.Server
	statuses []int
	bodies   []string
	requests []map[string]any
}

func newScriptedGateway(t *testing.T, statuses []int, bodies []string) *scriptedGateway {
	t.Helper()
	g := &scriptedGateway{statuses: statuses, bodies: bodies}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		g.mu.Lock()
		g.requests = append(g.requests, body)
		idx := len(g.requests) - 1
		st, b := http.StatusOK, "{}"
		if idx < len(g.statuses) {
			st = g.statuses[idx]
		}
		if idx < len(g.bodies) {
			b = g.bodies[idx]
		}
		g.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "scripted-request-id")
		w.WriteHeader(st)
		_, _ = w.Write([]byte(b))
	}))
	t.Cleanup(g.srv.Close)
	return g
}

// client builds an adapter pointed at the scripted server.
func (g *scriptedGateway) client() *Client {
	c, err := New(g.srv.URL, "test-token-do-not-log", "fake-model")
	if err != nil {
		panic(err) // unreachable in tests
	}
	return c
}

// snapshot returns a copy of the recorded request bodies (race-safe).
func (g *scriptedGateway) snapshot() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]map[string]any, len(g.requests))
	copy(out, g.requests)
	return out
}

// systemInstruction returns the text of the first system message in a captured
// request body, or "" when there is none.
func systemInstruction(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for _, m := range msgs {
		mm, ok := m.(map[string]any)
		if !ok || mm["role"] != "system" {
			continue
		}
		if txt, ok := mm["content"].(string); ok {
			return txt
		}
	}
	return ""
}

// 400 JSON mode -> the fallback request has no response_format and carries the
// "single JSON object only" instruction, and the response is the clean object.
func TestJSONModeFallback400RetriesWithoutResponseFormat(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusBadRequest, http.StatusOK}, []string{jsonReject400, jsonAccept200})
	c := g.client()
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: `Return {"ok":true}`}},
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != `{"ok":true}` {
		t.Errorf("Text = %q, want the extracted JSON object", resp.Text)
	}
	reqs := g.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (first attempt + fallback)", len(reqs))
	}
	first, ok := reqs[0]["response_format"].(map[string]any)
	if !ok || first["type"] != "json_object" {
		t.Errorf("first request response_format = %v, want json_object", reqs[0]["response_format"])
	}
	if _, present := reqs[1]["response_format"]; present {
		t.Errorf("fallback request must not send response_format, got %v", reqs[1]["response_format"])
	}
	if !strings.Contains(systemInstruction(reqs[1]), "single JSON object only") {
		t.Errorf("fallback request must carry the JSON-only instruction; system = %q", systemInstruction(reqs[1]))
	}
	// The finding is cached as rejected.
	if caps := c.Capabilities(); caps.JSONMode != CapabilityRejected {
		t.Errorf("Capabilities().JSONMode = %q, want rejected", caps.JSONMode)
	}
}

// The cached "rejected" finding makes the next call skip the doomed first
// attempt and send the degraded form directly.
func TestJSONModeFallbackCacheSkipsDoomedFirstAttempt(t *testing.T) {
	g := newScriptedGateway(t,
		[]int{http.StatusBadRequest, http.StatusOK, http.StatusOK},
		[]string{jsonReject400, jsonAccept200, jsonAccept200})
	c := g.client()
	req := ChatRequest{
		Messages:           []Message{{Role: "user", Text: `Return {"ok":true}`}},
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	reqs := g.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("made %d requests, want 3 (2 on the first call, 1 on the second)", len(reqs))
	}
	// The second call's single request must skip the doomed first attempt:
	// no response_format, and the instruction is present.
	if _, present := reqs[2]["response_format"]; present {
		t.Errorf("second call must skip the doomed first attempt; response_format present: %v", reqs[2]["response_format"])
	}
	if !strings.Contains(systemInstruction(reqs[2]), "single JSON object only") {
		t.Errorf("second call must send the JSON-only instruction; system = %q", systemInstruction(reqs[2]))
	}
}

// An unparseable fallback response is a typed bad_schema error and is NOT
// retried further (exactly one fallback attempt was made).
func TestJSONModeFallbackUnparseableIsTypedError(t *testing.T) {
	prose := `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"Sure, here is the answer."},"finish_reason":"stop"}]}`
	g := newScriptedGateway(t, []int{http.StatusBadRequest, http.StatusOK}, []string{jsonReject400, prose})
	c := g.client()
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: `Return {"ok":true}`}},
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
	})
	if err == nil {
		t.Fatal("expected a typed error for an unparseable fallback response, got nil")
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindBadSchema {
		t.Errorf("Kind = %q, want bad_schema (err=%s)", pe.Kind, pe)
	}
	if pe.Retryable {
		t.Error("a capability-driven bad_schema must not be retried")
	}
	if n := len(g.snapshot()); n != 2 {
		t.Errorf("made %d requests, want 2 (no further retries after an unparseable fallback)", n)
	}
}

// tool_choice=required -> the fallback request sends auto (not required) and
// carries the call-a-tool instruction; the tool call still arrives.
func TestToolChoiceRequiredFallback400RetriesAsAuto(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusBadRequest, http.StatusOK}, []string{toolReject400, toolAccept200})
	c := g.client()
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: "use the tool"}},
		Tools:              []ToolDef{{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice:         ToolChoiceRequired,
		ToolChoiceFallback: true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "echo_tool" {
		t.Fatalf("ToolCalls = %+v, want one echo_tool call", resp.ToolCalls)
	}
	reqs := g.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2", len(reqs))
	}
	if tc, _ := reqs[0]["tool_choice"].(string); tc != "required" {
		t.Errorf("first tool_choice = %v, want required", reqs[0]["tool_choice"])
	}
	if tc, _ := reqs[1]["tool_choice"].(string); tc != "auto" {
		t.Errorf("fallback tool_choice = %v, want auto", reqs[1]["tool_choice"])
	}
	if !strings.Contains(systemInstruction(reqs[1]), "calling a tool") {
		t.Errorf("fallback request must carry the call-a-tool instruction; system = %q", systemInstruction(reqs[1]))
	}
	if caps := c.Capabilities(); caps.ToolChoiceRequired != CapabilityRejected {
		t.Errorf("Capabilities().ToolChoiceRequired = %q, want rejected", caps.ToolChoiceRequired)
	}
}

// The degraded tool_choice path: the gateway rejects required, the auto retry
// succeeds but the model still does not call a tool -> ErrToolCallMissing.
func TestToolChoiceRequiredFallbackNoCallIsToolCallMissing(t *testing.T) {
	noCall := `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"I will not use tools."},"finish_reason":"stop"}]}`
	g := newScriptedGateway(t, []int{http.StatusBadRequest, http.StatusOK}, []string{toolReject400, noCall})
	c := g.client()
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: "use the tool"}},
		Tools:              []ToolDef{{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice:         ToolChoiceRequired,
		ToolChoiceFallback: true,
	})
	if err == nil {
		t.Fatal("expected ErrToolCallMissing, got nil")
	}
	if !errors.Is(err, ErrToolCallMissing) {
		t.Fatalf("error = %v (%T), want ErrToolCallMissing", err, err)
	}
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindUnsupportedCapability {
		t.Fatalf("error = %v, want *providers.Error of unsupported_capability", err)
	}
	if pe.Retryable {
		t.Error("a capability gap must not be marked retryable")
	}
}

// A required request WITHOUT the opt-in flag still fails loudly with the typed
// sentinel (no silent degradation).
func TestToolChoiceRequiredWithoutFallbackIsTypedError(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusBadRequest}, []string{toolReject400})
	c := g.client()
	_, err := c.Chat(context.Background(), ChatRequest{
		Messages:   []Message{{Role: "user", Text: "use the tool"}},
		Tools:      []ToolDef{{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice: ToolChoiceRequired,
	})
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !errors.Is(err, ErrUnsupportedToolChoice) {
		t.Fatalf("error = %v, want ErrUnsupportedToolChoice", err)
	}
	var pe *providers.Error
	if !errors.As(err, &pe) || pe.Kind != providers.KindUnsupportedCapability {
		t.Fatalf("error = %v, want *providers.Error of unsupported_capability", err)
	}
	if n := len(g.snapshot()); n != 1 {
		t.Errorf("made %d requests, want 1 (no fallback without the opt-in flag)", n)
	}
}
