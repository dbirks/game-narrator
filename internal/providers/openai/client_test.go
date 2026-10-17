package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// The responses in this file are RECORDED bodies captured from a real
// OpenAI-compatible gateway during M0 bring-up and then redacted: model ids are
// replaced with "recorded-model" and no endpoint or credential appears. They
// exercise the client's schema handling deterministically; the live contract
// tests in live_test.go are what prove the gateway itself behaves this way
// (docs/spec/12 "Contract" mode: recorded every PR, live conditional).

const (
	recordedModel = "recorded-model"
	recordedToken = "recorded-token-do-not-log"
)

// recordedChatSuccess is the literal gateway shape for a reasoning-enabled
// chat model: content prefixed with blank lines, a separate reasoning_content
// field, and reasoning token accounting under usage.
const recordedChatSuccess = `{
  "id": "chatcmpl-recorded-0001",
  "object": "chat.completion",
  "created": 1760000000,
  "model": "` + recordedModel + `",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "\n\npong",
        "reasoning_content": "The user wants the literal token pong."
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 14, "completion_tokens": 9, "total_tokens": 23, "reasoning_tokens": 7}
}`

// recordedChatVision is a recorded vision answer (two colours).
const recordedChatVision = `{"id":"chatcmpl-recorded-0002","model":"` + recordedModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"red blue"},"finish_reason":"stop"}],"usage":{"prompt_tokens":120,"completion_tokens":4,"total_tokens":124}}`

// recordedChatJSON is a recorded structured-output answer.
const recordedChatJSON = `{"model":"` + recordedModel + `","choices":[{"index":0,"message":{"role":"assistant","content":"{\"ok\":true}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":6,"total_tokens":26}}`

const recordedModels = `{"object":"list","data":[{"id":"` + recordedModel + `","object":"model","owned_by":"recorded"},{"id":"another-model","object":"model"}]}`

// recordedServer returns a server that always answers with body and status,
// plus a pointer capturing the last request for assertions.
func recordedServer(t *testing.T, status int, body string) (*httptest.Server, *capture) {
	t.Helper()
	captured := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.store(capturedRequest{
			Method: r.Method,
			Path:   r.URL.Path,
			Auth:   r.Header.Get("Authorization"),
			Header: r.Header.Clone(),
			Raw:    raw,
		})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "recorded-request-id")
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, captured
}

// capturedRequest records a request in full so assertions can inspect the exact
// wire body. It is stored in an atomic.Value, so handler writes stay race-free
// under -race even if a client ever issues concurrent requests.
type capturedRequest struct {
	Method string
	Path   string
	Auth   string
	Header http.Header
	Raw    []byte
}

type capture struct{ value atomic.Value }

func (c *capture) store(v capturedRequest) { c.value.Store(v) }
func (c *capture) load() capturedRequest {
	v, _ := c.value.Load().(capturedRequest)
	return v
}

func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	return &Client{BaseURL: srv.URL + "/v1", Token: recordedToken, Model: recordedModel}
}

func TestChatRecordedNormalizesLeadingNewlinesAndReasoning(t *testing.T) {
	srv, cap := recordedServer(t, http.StatusOK, recordedChatSuccess)
	c := newTestClient(t, srv)
	req := ChatRequest{Messages: []Message{{Role: "user", Text: "Reply with exactly: pong"}}, MaxTokens: 32}
	resp, err := c.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text != "pong" {
		t.Errorf("Text = %q, want \"pong\" after trimming the recorded leading blank lines", resp.Text)
	}
	if !strings.Contains(resp.Reasoning, "pong") {
		t.Errorf("Reasoning = %q, want the recorded reasoning_content", resp.Reasoning)
	}
	if resp.Reasoning == resp.Text {
		t.Error("reasoning_content must not be merged into Text")
	}
	if resp.FinishReason != "stop" {
		t.Errorf("FinishReason = %q", resp.FinishReason)
	}
	if resp.Model != recordedModel {
		t.Errorf("Model = %q, want %q", resp.Model, recordedModel)
	}
	if resp.Usage.Prompt != 14 || resp.Usage.Completion != 9 || resp.Usage.Reasoning != 7 {
		t.Errorf("Usage = %+v, want {14 9 7}", resp.Usage)
	}
	if resp.Latency <= 0 {
		t.Errorf("Latency = %s, want a measured duration", resp.Latency)
	}
	if resp.RequestID != "recorded-request-id" {
		t.Errorf("RequestID = %q, want the x-request-id header value", resp.RequestID)
	}
	if cap.load().Path != "/v1/chat/completions" {
		t.Errorf("path = %q, want /v1/chat/completions", cap.load().Path)
	}
}

func TestChatRecordedSendsBearerAuthorizationAndRequestBodyShape(t *testing.T) {
	srv, cap := recordedServer(t, http.StatusOK, recordedChatSuccess)
	c := newTestClient(t, srv)
	temp := 0.2
	if _, err := c.Chat(context.Background(), ChatRequest{
		Messages:    []Message{{Role: "system", Text: "You are terse."}, {Role: "user", Text: "pong?"}},
		MaxTokens:   16,
		Temperature: &temp,
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	req := cap.load()
	if req.Auth != "Bearer "+recordedToken {
		t.Errorf("Authorization = %q, want \"Bearer <recorded token>\"", "<redacted>")
	}
	var wire chatRequestBody
	if err := json.Unmarshal(req.Raw, &wire); err != nil {
		t.Fatalf("decode recorded request body: %v\nbody=%s", err, req.Raw)
	}
	if wire.Model != recordedModel {
		t.Errorf("wire model = %q", wire.Model)
	}
	if wire.MaxTokens != 16 || wire.Temperature == nil || *wire.Temperature != 0.2 {
		t.Errorf("wire sampling = %+v %+v", wire.MaxTokens, wire.Temperature)
	}
	if wire.Stream {
		t.Error("stream must be false; buffered responses are the M0 contract")
	}
	if wire.ResponseFormat != nil {
		t.Error("response_format must be omitted when JSON mode is not requested")
	}
	if wire.ReasoningEffort != "" {
		t.Errorf("reasoning_effort = %q, want the field omitted when not requested", wire.ReasoningEffort)
	}
	if len(wire.Messages) != 2 {
		t.Fatalf("got %d messages", len(wire.Messages))
	}
	if text, ok := wire.Messages[0].Content.(string); !ok || text != "You are terse." {
		t.Errorf("text-only message content must be a plain string, got %#v", wire.Messages[0].Content)
	}
	if wire.Messages[1].Role != "user" {
		t.Errorf("message role = %q", wire.Messages[1].Role)
	}
	if ct := req.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestChatRecordedVisionRequestCarriesDataURLImageParts(t *testing.T) {
	srv, cap := recordedServer(t, http.StatusOK, recordedChatVision)
	c := newTestClient(t, srv)
	img := testPNG(t)
	if _, err := c.Chat(context.Background(), ChatRequest{
		Messages: []Message{{Role: "user", Text: "Name the two colours.", Images: [][]byte{img}}},
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var wire chatRequestBody
	if err := json.Unmarshal(cap.load().Raw, &wire); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	// json.Unmarshal decoded `any` into a generic slice, so round-trip the part
	// array through its wire form to inspect it structurally.
	rawParts, err := json.Marshal(wire.Messages[0].Content)
	if err != nil {
		t.Fatalf("re-encode message content: %v", err)
	}
	if _, isString := wire.Messages[0].Content.(string); isString {
		t.Fatalf("image message content must be an array of parts, got a bare string")
	}
	var parts []contentPart
	if err := json.Unmarshal(rawParts, &parts); err != nil {
		t.Fatalf("message content is not an array of content parts: %v\n%s", err, rawParts)
	}
	if len(parts) != 2 {
		t.Fatalf("got %d parts, want a text part followed by an image part", len(parts))
	}
	if parts[0].Type != "text" || parts[0].Text != "Name the two colours." {
		t.Errorf("part 0 = %+v", parts[0])
	}
	if parts[1].Type != "image_url" || parts[1].ImageURL == nil {
		t.Fatalf("part 1 = %+v, want an image_url part", parts[1])
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(parts[1].ImageURL.URL, prefix) {
		t.Fatalf("image data URL = %.40q..., want the %q prefix", parts[1].ImageURL.URL, prefix)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(parts[1].ImageURL.URL, prefix))
	if err != nil {
		t.Fatalf("base64 payload is not decodable: %v", err)
	}
	if !bytes.Equal(decoded, img) {
		t.Errorf("decoded payload is %d bytes, want the original %d bytes", len(decoded), len(img))
	}
}

func TestChatRecordedJSONModeSetsResponseFormat(t *testing.T) {
	srv, cap := recordedServer(t, http.StatusOK, recordedChatJSON)
	c := newTestClient(t, srv)
	resp, err := c.Chat(context.Background(), ChatRequest{
		Messages:           []Message{{Role: "user", Text: `Answer JSON {"ok":true}`}},
		ResponseFormatJSON: true,
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	var wire chatRequestBody
	if err := json.Unmarshal(cap.load().Raw, &wire); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if wire.ResponseFormat == nil || wire.ResponseFormat.Type != "json_object" {
		t.Errorf("response_format = %+v, want {Type:json_object}", wire.ResponseFormat)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(resp.Text), &parsed); err != nil {
		t.Errorf("Text = %q, want parsable JSON: %v", resp.Text, err)
	}
}

// TestChatRecordedErrorClassification is the failure-taxonomy contract from
// docs/spec/06, driven by recorded error bodies.
func TestChatRecordedErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		body      string
		wantKind  providers.Kind
		retryable bool
	}{
		{
			name:      "401 unauthorized",
			status:    http.StatusUnauthorized,
			body:      `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`,
			wantKind:  providers.KindAuth,
			retryable: false,
		},
		{
			name:      "403 forbidden",
			status:    http.StatusForbidden,
			body:      `{"error":{"message":"model not allowed for this key","type":"access_denied"}}`,
			wantKind:  providers.KindAuth,
			retryable: false,
		},
		{
			name:      "404 unknown model",
			status:    http.StatusNotFound,
			body:      `{"error":{"message":"The model 'recorded-model' does not exist","type":"invalid_request_error"}}`,
			wantKind:  providers.KindUnsupportedCapability,
			retryable: false,
		},
		{
			name:      "400 unsupported developer role",
			status:    http.StatusBadRequest,
			body:      `{"error":{"message":"Invalid message role: developer is not supported","type":"invalid_request_error"}}`,
			wantKind:  providers.KindUnsupportedCapability,
			retryable: false,
		},
		{
			name:      "400 rejected response_format",
			status:    http.StatusBadRequest,
			body:      `{"error":{"message":"response_format is not supported by this deployment"}}`,
			wantKind:  providers.KindUnsupportedCapability,
			retryable: false,
		},
		{
			name:      "400 opaque validation failure",
			status:    http.StatusBadRequest,
			body:      `{"error":{"message":"field required"}}`,
			wantKind:  providers.KindBadSchema,
			retryable: false,
		},
		{
			name:      "408 gateway timeout",
			status:    http.StatusRequestTimeout,
			body:      `upstream request timeout`,
			wantKind:  providers.KindTimeout,
			retryable: true,
		},
		{
			name:      "413 payload too large",
			status:    http.StatusRequestEntityTooLarge,
			body:      `{"error":{"message":"maximum request size exceeded"}}`,
			wantKind:  providers.KindPayloadTooLarge,
			retryable: false,
		},
		{
			name:      "429 rate limited",
			status:    http.StatusTooManyRequests,
			body:      `{"error":{"message":"You have exceeded current quota","type":"rate_limit_error"}}`,
			wantKind:  providers.KindRateLimited,
			retryable: true,
		},
		{
			name:      "429 insufficient quota",
			status:    http.StatusTooManyRequests,
			body:      `{"error":{"message":"insufficient_quota","code":"insufficient_quota"}}`,
			wantKind:  providers.KindRateLimited,
			retryable: true,
		},
		{
			name:      "500 server error",
			status:    http.StatusInternalServerError,
			body:      `{"error":{"message":"internal error"}}`,
			wantKind:  providers.KindUnavailable,
			retryable: true,
		},
		{
			name:   "503 upstream unavailable",
			status: http.StatusServiceUnavailable,
			body: `<html><body>Service Unavailable
extra line ignored</body></html>`,
			wantKind:  providers.KindUnavailable,
			retryable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := recordedServer(t, tc.status, tc.body)
			c := newTestClient(t, srv)
			_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "pong?"}}})
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("Chat error = %v (%T), want *providers.Error", err, err)
			}
			if pe.Kind != tc.wantKind {
				t.Errorf("Kind = %q, want %q (err=%s)", pe.Kind, tc.wantKind, pe)
			}
			if pe.Status != tc.status {
				t.Errorf("Status = %d, want %d", pe.Status, tc.status)
			}
			if pe.Retryable != tc.retryable {
				t.Errorf("Retryable = %v, want %v for kind %q", pe.Retryable, tc.retryable, pe.Kind)
			}
			if pe.RequestID != "recorded-request-id" {
				t.Errorf("RequestID = %q, want the recorded x-request-id header", pe.RequestID)
			}
			if strings.Contains(pe.Error(), recordedToken) {
				t.Errorf("error text leaked the token: %s", pe)
			}
			if len(pe.Error()) > 1200 {
				t.Errorf("error text is %d bytes; provider echoes must be capped", len(pe.Error()))
			}
		})
	}
}

func TestChatRecordedMalformedJSONIsBadSchema(t *testing.T) {
	cases := map[string]string{
		"truncated object":     `{"model":"recorded-model","choices":[{`,
		"html error page":      `<!doctype html><html>boom`,
		"empty choices":        `{"model":"recorded-model","choices":[],"usage":{}}`,
		"missing content":      `{"model":"recorded-model","choices":[{"index":0,"message":{"role":"assistant"},"finish_reason":"stop"}]}`,
		"content null":         `{"model":"recorded-model","choices":[{"index":0,"message":{"role":"assistant","content":null},"finish_reason":"stop"}]}`,
		"two choices returned": `{"model":"recorded-model","choices":[{"index":0,"message":{"content":"a"}},{"index":1,"message":{"content":"b"}}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, _ := recordedServer(t, http.StatusOK, body)
			c := newTestClient(t, srv)
			_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "hi"}}})
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v (%T), want *providers.Error", err, err)
			}
			if pe.Kind != providers.KindBadSchema {
				t.Errorf("Kind = %q, want bad_schema (err=%s)", pe.Kind, pe)
			}
			if pe.Retryable {
				t.Error("a schema failure must never be marked retryable")
			}
		})
	}
}

func TestChatRecordedNullContentWithReasoningIsNotAnError(t *testing.T) {
	// finish_reason "stop" with reasoning only is a complete (if odd)
	// answer: it must be returned, not rejected.
	srv, _ := recordedServer(t, http.StatusOK,
		`{"model":"`+recordedModel+`","choices":[{"index":0,"message":{"role":"assistant","content":null,"reasoning_content":"still thinking"},"finish_reason":"stop"}]}`)
	c := newTestClient(t, srv)
	resp, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatalf("a reasoning-only answer must be returned, not rejected: %v", err)
	}
	if resp.Text != "" || resp.Reasoning != "still thinking" || resp.FinishReason != "stop" {
		t.Errorf("resp = %+v", resp)
	}
}

// finish_reason "length" with no text and no tool call is the observed
// "the model spent the whole output budget reasoning" failure (live on
// qwen3-8-27b: the budget is consumed by reasoning_content and content comes
// back empty). It is a typed, non-retryable budget_exhausted error carrying
// the gateway's usage numbers, so callers can tell it from a schema-less
// empty message and act (raise the budget or lower the reasoning effort).
func TestChatRecordedLengthFinishWithoutAnswerIsBudgetExhausted(t *testing.T) {
	srv, _ := recordedServer(t, http.StatusOK,
		`{"model":"`+recordedModel+`","choices":[{"index":0,"message":{"role":"assistant","content":"","reasoning_content":"thinking about it"},"finish_reason":"length"}],"usage":{"prompt_tokens":65,"completion_tokens":48,"completion_tokens_details":{"reasoning_tokens":48}}}`)
	c := newTestClient(t, srv)
	resp, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "hi"}}, MaxTokens: 48})
	if err == nil {
		t.Fatal("want ErrOutputBudgetExhausted, got nil error")
	}
	if !errors.Is(err, ErrOutputBudgetExhausted) {
		t.Fatalf("error = %v (%T), want ErrOutputBudgetExhausted", err, err)
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != providers.KindBudgetExhausted {
		t.Errorf("Kind = %q, want budget_exhausted (err=%s)", pe.Kind, pe)
	}
	if pe.Retryable {
		t.Error("a budget exhaustion must not be retried blindly")
	}
	if !strings.Contains(pe.Msg, "completion=48") || !strings.Contains(pe.Msg, "reasoning=48") {
		t.Errorf("error message must carry the usage numbers: %q", pe.Msg)
	}
	// The response is still returned with its usage, so diagnostics keep the
	// correlation and the token accounting.
	if resp.FinishReason != "length" || resp.Usage.Completion != 48 || resp.Usage.Reasoning != 48 || resp.Usage.Prompt != 65 {
		t.Errorf("resp = %+v, want the length finish and the gateway usage numbers", resp)
	}
}

// A length finish that produced a tool call is a complete answer, not a
// budget failure: tool callers must not be blocked by the empty-text rule.
func TestChatRecordedLengthFinishWithToolCallIsNotAnError(t *testing.T) {
	srv, _ := recordedServer(t, http.StatusOK,
		`{"model":"`+recordedModel+`","choices":[{"index":0,"finish_reason":"length","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"echo_tool","arguments":"{\"text\":\"ping\"}"}}]}}]}`)
	c := newTestClient(t, srv)
	resp, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "hi"}}})
	if err != nil {
		t.Fatalf("a tool call on a length finish must be returned: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Errorf("ToolCalls = %+v, want one call", resp.ToolCalls)
	}
}

func TestChatTimeoutContextDeadlineIsClassifiedTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			fmt.Fprint(w, recordedChatSuccess)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Text: "slow down"}}})
	requireKind(t, err, providers.KindTimeout)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("timeout error must unwrap to context.DeadlineExceeded so callers can distinguish it: %v", err)
	}
}

func TestChatCancellationIsClassifiedCancelled(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		fmt.Fprint(w, recordedChatSuccess)
	}))
	t.Cleanup(func() { close(release); srv.Close() })
	c := newTestClient(t, srv)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	_, err := c.Chat(ctx, ChatRequest{Messages: []Message{{Role: "user", Text: "abort"}}})
	requireKind(t, err, providers.KindCancelled)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Error("a cancellation must not be reported as a timeout")
	}
	if err != nil && strings.Contains(err.Error(), recordedToken) {
		t.Errorf("error leaked the token: %v", err)
	}
}

func TestChatConnectionRefusedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now nothing is listening
	c := &Client{BaseURL: url + "/v1", Token: recordedToken, Model: recordedModel}
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{{Role: "user", Text: "anyone there?"}}})
	pe := requireKind(t, err, providers.KindUnavailable)
	if !pe.Retryable {
		t.Error("an unreachable endpoint must be retryable")
	}
	if pe.Status != 0 {
		t.Errorf("Status = %d, want 0 because no response was received", pe.Status)
	}
}

func TestChatRejectsDeveloperRoleClientSide(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, recordedChatSuccess)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{
		{Role: "developer", Text: "You are helpful."},
		{Role: "user", Text: "hi"},
	}})
	pe := requireKind(t, err, providers.KindUnsupportedCapability)
	if !strings.Contains(pe.Msg, "developer") {
		t.Errorf("Msg = %q, want it to name the rejected role", pe.Msg)
	}
	if pe.Status != 0 {
		t.Errorf("Status = %d, want a local rejection with no HTTP status", pe.Status)
	}
	if got := atomic.LoadInt32(&hits); got != 0 {
		t.Errorf("server was hit %d times; the rejection must happen before any request leaves", got)
	}
}

func TestChatRejectsOversizeImageBeforeSending(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		fmt.Fprint(w, recordedChatSuccess)
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(t, srv)
	oversize := bytes.Repeat([]byte{0x00}, MaxImageBytes+1)
	_, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{
		{Role: "user", Text: "read this", Images: [][]byte{oversize}},
	}})
	pe := requireKind(t, err, providers.KindPayloadTooLarge)
	if !strings.Contains(pe.Msg, "byte") {
		t.Errorf("Msg = %q, want it to state the size limit", pe.Msg)
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Error("an oversize payload must be rejected locally")
	}
	// Exactly at the cap is allowed (the limit is inclusive of 4 MiB).
	c.MaxImageBytes = 1024
	if _, err := c.Chat(context.Background(), ChatRequest{Messages: []Message{
		{Role: "user", Images: [][]byte{bytes.Repeat([]byte("a"), 1025)}},
	}}); err == nil {
		t.Error("a per-client limit override must be enforced")
	} else {
		requireKind(t, err, providers.KindPayloadTooLarge)
	}
}

func TestChatClientSideRequestValidation(t *testing.T) {
	srv, _ := recordedServer(t, http.StatusOK, recordedChatSuccess)
	c := newTestClient(t, srv)
	cases := map[string]struct {
		req  ChatRequest
		kind providers.Kind
	}{
		"no messages": {ChatRequest{}, providers.KindBadSchema},
		"empty role":  {ChatRequest{Messages: []Message{{Text: "hi"}}}, providers.KindBadSchema},
		"empty image": {ChatRequest{Messages: []Message{{Role: "user", Images: [][]byte{{}}}}}, providers.KindBadSchema},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.Chat(context.Background(), tc.req)
			requireKind(t, err, tc.kind)
		})
	}
}

func TestListModelsRecorded(t *testing.T) {
	srv, cap := recordedServer(t, http.StatusOK, recordedModels)
	c := newTestClient(t, srv)
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 2 || ids[0] != recordedModel || ids[1] != "another-model" {
		t.Errorf("ids = %v", ids)
	}
	if cap.load().Path != "/v1/models" {
		t.Errorf("path = %q, want /v1/models", cap.load().Path)
	}
	if cap.load().Auth != "Bearer "+recordedToken {
		t.Error("ListModels must send the bearer token")
	}
}

func TestListModelsRecordedBareArrayAndErrorPaths(t *testing.T) {
	t.Run("bare array", func(t *testing.T) {
		srv, _ := recordedServer(t, http.StatusOK, `["`+recordedModel+`","m2"]`)
		c := newTestClient(t, srv)
		ids, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		if len(ids) != 2 || ids[0] != recordedModel {
			t.Errorf("ids = %v", ids)
		}
	})
	t.Run("data envelope of objects without ids", func(t *testing.T) {
		srv, _ := recordedServer(t, http.StatusOK, `{"data":[]}`)
		c := newTestClient(t, srv)
		_, err := c.ListModels(context.Background())
		requireKind(t, err, providers.KindBadSchema)
	})
	t.Run("401 is auth", func(t *testing.T) {
		srv, _ := recordedServer(t, http.StatusUnauthorized, `{"error":{"message":"no key"}}`)
		c := newTestClient(t, srv)
		requireKind(t, secondError(c.ListModels(context.Background())), providers.KindAuth)
	})
}

func TestHealthProbeRecorded(t *testing.T) {
	srv, _ := recordedServer(t, http.StatusNotFound, `{"error":{"message":"no such route"}}`)
	c := newTestClient(t, srv)
	requireKind(t, c.Health(context.Background()), providers.KindUnsupportedCapability)

	okSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(okSrv.Close)
	c2 := newTestClient(t, okSrv)
	if err := c2.Health(context.Background()); err != nil {
		t.Errorf("Health on a 200 = %v, want nil", err)
	}
}

func TestNewValidatesBaseURL(t *testing.T) {
	for _, bad := range []string{"", "   ", "ftp://gateway.example.invalid/v1", "https://user:pw@gateway.example.invalid", "://nope", "https://"} {
		if _, err := New(bad, "t", "m"); err == nil {
			t.Errorf("New(%q) returned no error", bad)
		}
	}
	c, err := New("https://gateway.example.invalid/v1/", "t", "m")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.BaseURL != "https://gateway.example.invalid/v1" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", c.BaseURL)
	}
	if c.HTTP != nil {
		t.Error("HTTP must stay nil by default so context deadlines are the only timeout")
	}
	// A nil HTTP client still works.
	if _, err := (&Client{BaseURL: "https://gateway.example.invalid/v1", Model: "m"}).endpoint("/chat/completions"); err != nil {
		t.Errorf("endpoint: %v", err)
	}
}

func TestNewFromRoleRejectsUnconfiguredAndUndeployedRoles(t *testing.T) {
	lookup := func(key string) (string, bool) {
		values := map[string]string{
			providers.EnvGatewayURL:                   "https://gateway.example.invalid/v1",
			providers.EnvGatewayToken:                 recordedToken,
			"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL": "fast-model",
			"GAME_NARRATOR_ROLE_DECISION_MODEL":       "Cloudflare/clef-omni",
		}
		v, ok := values[key]
		return v, ok
	}
	roles, err := providers.LoadRoles(lookup)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	fast, err := NewFromRole(roles[providers.RoleFastReasoning])
	if err != nil {
		t.Fatalf("NewFromRole(fast_reasoning): %v", err)
	}
	if fast.Model != "fast-model" || fast.BaseURL != "https://gateway.example.invalid/v1" {
		t.Errorf("client = %+v", fast)
	}
	if fast.HTTP == nil || fast.HTTP.Timeout != roles[providers.RoleFastReasoning].Timeout {
		t.Errorf("per-role timeout not applied to the transport: %+v", fast.HTTP)
	}
	if strings.Contains(fmt.Sprintf("%v", fast), recordedToken) {
		t.Error("the resolved client must not print the token in a default formatting of its fields")
	}
	_, err = NewFromRole(roles[providers.RoleDecision])
	pe := requireKind(t, err, providers.KindUnsupportedCapability)
	if !strings.Contains(pe.Msg, "adapter") {
		t.Errorf("Msg = %q, want an explicit missing-adapter statement", pe.Msg)
	}
	_, err = NewFromRole(roles[providers.RoleAgentReasoning])
	requireKind(t, err, providers.KindUnsupportedCapability)
	if !strings.Contains(err.Error(), "NOT CONFIGURED") {
		t.Errorf("error = %v, want the NOT CONFIGURED preflight phrase", err)
	}
}

func TestDetectImageMIME(t *testing.T) {
	if got := detectImageMIME(testPNG(t)); got != "image/png" {
		t.Errorf("detectImageMIME(png) = %q", got)
	}
	if got := detectImageMIME([]byte{0xff, 0xd8, 0xff, 0xe0}); got != "image/jpeg" {
		t.Errorf("detectImageMIME(jpeg) = %q", got)
	}
	if got := detectImageMIME([]byte("whatever")); got != "application/octet-stream" {
		t.Errorf("detectImageMIME(unknown) = %q", got)
	}
}

// requireKind asserts the error is a *providers.Error of the expected kind.
func requireKind(t *testing.T, err error, want providers.Kind) *providers.Error {
	t.Helper()
	if err == nil {
		t.Fatalf("want a %q error, got nil", want)
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *providers.Error", err, err)
	}
	if pe.Kind != want {
		t.Fatalf("Kind = %q, want %q (err=%s)", pe.Kind, want, pe)
	}
	return pe
}

// secondError keeps the error of a two-value call for requireKind.
func secondError(_ any, err error) error { return err }

// testPNG returns a deterministic 8x8 PNG (diagonal checkerboard) so vision
// request bodies can be compared byte for byte.
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			if (x+y)%2 == 0 {
				img.Set(x, y, color.RGBA{R: 255, A: 255})
			} else {
				img.Set(x, y, color.RGBA{B: 255, A: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode PNG: %v", err)
	}
	return buf.Bytes()
}
