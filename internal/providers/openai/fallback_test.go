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

const (
	reasoningReject400 = `{"error":{"type":"invalid_request_error","message":"reasoning_effort is not supported by this deployment"}}`
	pingAccept200      = `{"model":"fake-model","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`
	// combinedAccept200 answers both degraded features at once (a JSON object
	// in the content AND a tool call), which is the only shape that can satisfy
	// a request that wants both.
	combinedAccept200 = `{"model":"fake-model","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"{\"ok\":true}","tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\"text\":\"ping\"}"}}]}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`
)

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

// 400 naming reasoning_effort -> the client retries exactly once without the
// field (the model default applies) and caches the finding, mirroring the
// JSON-mode and tool_choice degradations.
func TestReasoningEffortRejectionRetriesWithoutTheField(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusBadRequest, http.StatusOK}, []string{reasoningReject400, pingAccept200})
	c := g.client()
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages:        []Message{{Role: "user", Text: "pong?"}},
		MaxTokens:       32,
		ReasoningEffort: "low",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "pong" {
		t.Errorf("Text = %q, want pong", resp.Text)
	}
	reqs := g.snapshot()
	if len(reqs) != 2 {
		t.Fatalf("made %d requests, want 2 (first attempt + degraded retry)", len(reqs))
	}
	if eff, _ := reqs[0]["reasoning_effort"].(string); eff != "low" {
		t.Errorf("first request reasoning_effort = %v, want low", reqs[0]["reasoning_effort"])
	}
	if _, present := reqs[1]["reasoning_effort"]; present {
		t.Errorf("retry must omit reasoning_effort, got %v", reqs[1]["reasoning_effort"])
	}
	if caps := c.Capabilities(); caps.ReasoningEffort != CapabilityRejected {
		t.Errorf("Capabilities().ReasoningEffort = %q, want rejected", caps.ReasoningEffort)
	}
}

// The cached "rejected" finding makes the next call skip the doomed first
// attempt: reasoning_effort is never sent at all.
func TestReasoningEffortRejectedCacheSkipsDoomedFirstAttempt(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusOK}, []string{pingAccept200})
	c := g.client()
	c.reasoningEffortState = CapabilityRejected
	if _, err := c.Chat(context.Background(), ChatRequest{
		Messages:        []Message{{Role: "user", Text: "pong?"}},
		ReasoningEffort: "high",
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	reqs := g.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1 (skip the doomed first attempt)", len(reqs))
	}
	if _, present := reqs[0]["reasoning_effort"]; present {
		t.Errorf("request must omit reasoning_effort after a cached rejection, got %v", reqs[0]["reasoning_effort"])
	}
}

// A request without ReasoningEffort sends no reasoning_effort field at all
// (empty = omit, so gateways that reject the field never see it from roles
// that have no default).
func TestReasoningEffortEmptySendsNoField(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusOK}, []string{pingAccept200})
	c := g.client()
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "pong?"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	reqs := g.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1", len(reqs))
	}
	if _, present := reqs[0]["reasoning_effort"]; present {
		t.Errorf("request must omit reasoning_effort, got %v", reqs[0]["reasoning_effort"])
	}
}

// When BOTH the JSON and the tool_choice capability are rejected in the same
// call, the second degraded retry must start from the first degraded
// messages: the final request carries both instructions (JSON-only AND
// call-a-tool), not just the last one. (Before the fix, the tool_choice
// rebuild re-derived messages from the ORIGINAL request and discarded the
// JSON instruction.)
func TestCombinedJSONAndToolChoiceDegradationKeepsBothInstructions(t *testing.T) {
	g := newScriptedGateway(t,
		[]int{http.StatusBadRequest, http.StatusBadRequest, http.StatusOK},
		[]string{jsonReject400, toolReject400, combinedAccept200})
	c := g.client()
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: "use the tool and answer as JSON"}},
		Tools:              []ToolDef{{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice:         ToolChoiceRequired,
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
		ToolChoiceFallback: true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != `{"ok":true}` || len(resp.ToolCalls) != 1 {
		t.Fatalf("resp = %+v, want the extracted JSON object and one tool call", resp)
	}
	reqs := g.snapshot()
	if len(reqs) != 3 {
		t.Fatalf("made %d requests, want 3 (attempt + JSON fallback + tool fallback)", len(reqs))
	}
	// Attempt 1: both raw features, no instruction yet.
	if _, present := reqs[0]["response_format"]; !present {
		t.Error("first request must send response_format")
	}
	if tc, _ := reqs[0]["tool_choice"].(string); tc != "required" {
		t.Errorf("first tool_choice = %v, want required", reqs[0]["tool_choice"])
	}
	if got := systemInstruction(reqs[0]); got != "" {
		t.Errorf("first request must carry no fallback instruction, got %q", got)
	}
	// Attempt 2: JSON degraded (no response_format, JSON instruction),
	// tool_choice still raw (required).
	if _, present := reqs[1]["response_format"]; present {
		t.Errorf("second request must drop response_format, got %v", reqs[1]["response_format"])
	}
	if tc, _ := reqs[1]["tool_choice"].(string); tc != "required" {
		t.Errorf("second tool_choice = %v, want required (only JSON degraded so far)", reqs[1]["tool_choice"])
	}
	sys2 := systemInstruction(reqs[1])
	if !strings.Contains(sys2, "single JSON object only") || strings.Contains(sys2, "calling a tool") {
		t.Errorf("second request must carry only the JSON instruction, got %q", sys2)
	}
	// Attempt 3: BOTH degraded; the instruction must carry BOTH parts — this is
	// the regression the fix addresses.
	if _, present := reqs[2]["response_format"]; present {
		t.Errorf("third request must not send response_format")
	}
	if tc, _ := reqs[2]["tool_choice"].(string); tc != "auto" {
		t.Errorf("third tool_choice = %v, want auto", reqs[2]["tool_choice"])
	}
	sys3 := systemInstruction(reqs[2])
	if !strings.Contains(sys3, "single JSON object only") || !strings.Contains(sys3, "calling a tool") {
		t.Errorf("third request must carry BOTH fallback instructions, got %q", sys3)
	}
	caps := c.Capabilities()
	if caps.JSONMode != CapabilityRejected || caps.ToolChoiceRequired != CapabilityRejected {
		t.Errorf("Capabilities() = %+v, want both rejected", caps)
	}
}

// The same combined degradation from the CACHED findings: both known-rejected
// capabilities are skipped up front, so exactly one request goes out and it
// already carries the composed instruction.
func TestCombinedJSONAndToolChoiceDegradationFromCacheIsOneRequest(t *testing.T) {
	g := newScriptedGateway(t, []int{http.StatusOK}, []string{combinedAccept200})
	c := g.client()
	c.jsonModeState = CapabilityRejected
	c.toolChoiceReqState = CapabilityRejected
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: "use the tool and answer as JSON"}},
		Tools:              []ToolDef{{Name: "echo_tool", Description: "Echo.", Parameters: json.RawMessage(echoToolSchema)}},
		ToolChoice:         ToolChoiceRequired,
		ResponseFormatJSON: true,
		JSONModeFallback:   true,
		ToolChoiceFallback: true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %+v, want one call", resp.ToolCalls)
	}
	reqs := g.snapshot()
	if len(reqs) != 1 {
		t.Fatalf("made %d requests, want 1 (both findings cached: skip both doomed attempts)", len(reqs))
	}
	if _, present := reqs[0]["response_format"]; present {
		t.Errorf("request must not send response_format, got %v", reqs[0]["response_format"])
	}
	if tc, _ := reqs[0]["tool_choice"].(string); tc != "auto" {
		t.Errorf("tool_choice = %v, want auto", reqs[0]["tool_choice"])
	}
	sys := systemInstruction(reqs[0])
	if !strings.Contains(sys, "single JSON object only") || !strings.Contains(sys, "calling a tool") {
		t.Errorf("request must carry BOTH fallback instructions, got %q", sys)
	}
}
