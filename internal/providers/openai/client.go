// Package openai implements the client adapter for the openai_chat and
// openai_chat_vision protocols (docs/spec/06 "Provider registry").
//
// It is deliberately narrow: one chat-completions call, one model listing
// call, provider-side quirks normalised into ChatResponse, and every failure
// classified as a *providers.Error. No retries happen here — retry and circuit
// policy belong to the model router, which knows whether the originating
// observation is still current.
package openai

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Limits and defaults observed against real gateways.
const (
	// MaxImageBytes caps one image part. Larger crops must be re-encoded or
	// downsampled by the caller; sending them produces opaque 413s from some
	// gateways and multi-second stalls from others.
	MaxImageBytes = 4 << 20
	// DefaultUserAgent identifies us so gateway logs are attributable.
	DefaultUserAgent = "game-narrator/0.0 m0-providers"
	// roleDeveloper is rejected client-side: the ChatML role is not accepted by
	// the gateways we tested, and a mid-request 400 wastes the whole timeout.
	roleDeveloper = "developer"
)

// Typed capability errors (docs/spec/06 "Model-serving version and health
// negotiation"). A gateway that cannot guarantee a feature rejects the request
// with an HTTP 400; these sentinels let callers and diagnostics match the exact
// capability gap rather than parsing prose. Each is also surfaced as a
// *providers.Error of KindUnsupportedCapability (the existing classification),
// so the two views agree.
var (
	// ErrUnsupportedResponseFormat marks a 400 in which the gateway refused a
	// response_format (structured/JSON mode) request. Observed live on the
	// NInfer build (2026-10-17): 'this response_format requires constrained
	// output, which NInfer cannot guarantee; only {"type":"text"} is available'.
	ErrUnsupportedResponseFormat = errors.New("openai: response_format is not supported by this deployment")
	// ErrUnsupportedToolChoice marks a 400 in which the gateway refused
	// tool_choice=required. Observed live on the NInfer build (2026-10-17):
	// "tool_choice='required' requires at least one tool call, which NInfer
	// cannot guarantee". tool_choice auto is accepted by the same deployment.
	ErrUnsupportedToolChoice = errors.New("openai: tool_choice required is not supported by this deployment")
	// ErrToolCallMissing marks the degraded tool_choice=required -> auto retry
	// in which the model still produced no tool call: the deployment cannot be
	// coerced into a forced call, which is a capability gap, not a transport
	// accident. Not retryable by default (a retry cannot add the capability).
	ErrToolCallMissing = errors.New("openai: a tool call was requested but none arrived")
	// ErrOutputBudgetExhausted marks a completion that ended with
	// finish_reason "length" while producing neither text nor a tool call: the
	// model spent the whole output budget (usually on reasoning) and there is
	// no answer to use. Callers tell "the model rambled through its budget"
	// from "the gateway returned nothing" with errors.Is(err,
	// ErrOutputBudgetExhausted) (kind budget_exhausted, non-retryable); the
	// gateway's usage numbers ride along in the *providers.Error message and
	// the returned ChatResponse.
	ErrOutputBudgetExhausted = errors.New("openai: the output budget was exhausted with no text and no tool call")
)

// CapabilityState is one entry in a Capabilities snapshot.
type CapabilityState string

const (
	// CapabilityUnknown means this capability has not been probed (or recorded)
	// yet for this client's (base URL, model).
	CapabilityUnknown CapabilityState = "unknown"
	// CapabilitySupported means a request exercising the capability succeeded.
	CapabilitySupported CapabilityState = "supported"
	// CapabilityRejected means the gateway rejected the capability (HTTP 400).
	CapabilityRejected CapabilityState = "rejected"
)

// Capabilities is a read-only snapshot of the per-(base URL, model) findings
// the client has recorded for its lifetime, for diagnostics. It is a cache of
// observed behaviour, not a live probe: entries stay unknown until the client
// actually sends a request that exercises the capability.
type Capabilities struct {
	// JSONMode records the response_format json_object finding.
	JSONMode CapabilityState
	// ToolChoiceRequired records the tool_choice=required finding.
	ToolChoiceRequired CapabilityState
	// ReasoningEffort records the reasoning_effort field finding.
	ReasoningEffort CapabilityState
}

// Message is one chat message. Text carries the content; Images are PNG or
// JPEG bytes sent as base64 data URLs, in order, after the text part.
//
// Tool-call fields (docs/spec/03 "typed tool calls"):
//   - an assistant message that called tools sets ToolCalls; Text may be empty;
//   - a tool-result message (Role "tool") sets ToolCallID linking it to the
//     call being answered; Text carries the (wrapped) result.
type Message struct {
	Role   string
	Text   string
	Images [][]byte
	// ToolCalls is only set on assistant messages.
	ToolCalls []ToolCall
	// ToolCallID is only set on role "tool" messages.
	ToolCallID string
}

// ChatRequest is the provider-neutral request for one chat completion.
type ChatRequest struct {
	Messages           []Message
	MaxTokens          int
	Temperature        *float64
	ResponseFormatJSON bool
	// Tools declares the functions the model may call. Empty sends no
	// "tools" field at all, keeping pre-tool requests byte-identical for
	// gateways that reject the field (docs/spec/06 compatibility tests).
	Tools []ToolDef
	// ToolChoice constrains tool use; empty sends nothing.
	ToolChoice ToolChoice
	// JSONModeFallback opts into the JSON-mode degradation (docs/spec/06
	// "Model-serving version and health negotiation"): when the gateway rejects
	// response_format json_object with a 400, the client retries once without
	// response_format and with an appended "answer with a single JSON object
	// only" instruction, then validates the response contains one parseable
	// JSON object. The "JSON mode unsupported" finding is cached per
	// (base URL, model) for the client's lifetime so later calls skip the
	// doomed first attempt. Opt-in only: a plain ResponseFormatJSON request
	// still fails loudly when rejected.
	JSONModeFallback bool
	// ToolChoiceFallback opts into the tool_choice degradation: when the
	// gateway rejects tool_choice=required with a 400, the client retries once
	// with tool_choice auto and an instruction to call the tool. If the model
	// still does not call a tool, the call returns ErrToolCallMissing (wrapped
	// in a *providers.Error). The "tool_choice=required unsupported" finding is
	// cached per (base URL, model) the same way. Opt-in only.
	ToolChoiceFallback bool
	// ReasoningEffort overrides the reasoning_effort field for this one
	// request; empty sends nothing and the model's default applies. When the
	// client was built from a role, the role's configured effort is the
	// sensible default to pass here (request override > role default).
	ReasoningEffort string
}

// Usage reports token counts. Reasoning is populated when the gateway returns
// reasoning token accounting, which some serving stacks split out of
// completion tokens.
type Usage struct {
	Prompt     int
	Completion int
	Reasoning  int
}

// ChatResponse is a normalised completion.
type ChatResponse struct {
	// Text is the answer with leading/trailing whitespace removed. Gateways
	// backed by reasoning models routinely prefix the answer with "\n\n".
	Text string
	// Reasoning carries a separate reasoning_content field when the backend
	// emits one. It is never merged into Text.
	Reasoning string
	// FinishReason is the provider's stop classification, e.g. "stop".
	FinishReason string
	// Model is the model id reported by the response, which may differ from
	// the requested alias.
	Model string
	// ToolCalls are the function invocations the model asked to execute.
	// Empty on a plain answer; when set, Text may legitimately be empty.
	ToolCalls []ToolCall
	Usage     Usage
	// Latency measures request write through full response body read.
	Latency time.Duration
	// RequestID comes from the x-request-id (or request-id) response header.
	RequestID string
}

// Client talks to one OpenAI-compatible deployment for one model.
type Client struct {
	BaseURL string
	Token   string
	Model   string
	// HTTP is the transport used for requests. When nil, a client with no
	// overall timeout is used: per-request deadlines come from the context, so
	// the caller (and later the router) owns cancellation (docs/spec/06
	// "Timeout and cancellation defaults").
	HTTP *http.Client
	// MaxImageBytes overrides MaxImageBytes for this client; zero uses the
	// package default.
	MaxImageBytes int
	// reasoningEffortDefault is the role default (providers.RoleConfig.
	// ReasoningEffort) set by NewFromRole. A non-empty ChatRequest.ReasoningEffort
	// overrides it per request; an empty request value falls back to it. A
	// client built with New() has no default and never sends the field unless
	// the request sets it.
	reasoningEffortDefault string

	// Capability findings are cached for the client's lifetime. A Client is
	// one (base URL, model) by construction, so a client-lifetime cache is
	// exactly the required per-(base URL, model) cache. Guarded by mu because a
	// shared client may serve concurrent requests.
	mu                   sync.Mutex
	jsonModeState        CapabilityState
	toolChoiceReqState   CapabilityState
	reasoningEffortState CapabilityState
}

// New returns a client for a base URL. baseURL is validated, not silently
// repaired, so a misconfigured env var fails at startup.
func New(baseURL, token, model string) (*Client, error) {
	if strings.TrimSpace(baseURL) == "" {
		return nil, providers.NewError(providers.KindUnsupportedCapability, 0, "openai: BaseURL is empty for model "+model)
	}
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("openai: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("openai: base URL scheme %q is not supported, use http or https", u.Scheme)
	}
	if u.Host == "" {
		return nil, errors.New("openai: base URL has no host")
	}
	if u.User != nil {
		return nil, errors.New("openai: credentials in the base URL are not allowed, use the token field")
	}
	c := &Client{BaseURL: strings.TrimRight(u.String(), "/"), Token: token, Model: model}
	// The redirect guard is the spec-14 control: the gateway is operator
	// configuration, so a response that moves the request to another host is
	// refused rather than followed (ADR-0068).
	c.HTTP = &http.Client{CheckRedirect: providers.SafeRedirect(u.String())}
	return c, nil
}

// NewFromRole builds a client for a configured role, rejecting roles that are
// not configured or whose protocol is not an OpenAI-compatible chat protocol.
func NewFromRole(cfg providers.RoleConfig) (*Client, error) {
	if !cfg.Configured() {
		err := providers.NewError(providers.KindUnsupportedCapability, 0, "role "+string(cfg.Role)+" is NOT CONFIGURED")
		err.Role = cfg.Role
		return nil, err
	}
	if !providers.ProtocolImplemented(cfg.Protocol) {
		err := providers.NewError(providers.KindUnsupportedCapability, 0,
			fmt.Sprintf("role %s uses protocol %q, which has no adapter in this build", cfg.Role, cfg.Protocol))
		err.Role = cfg.Role
		err.Model = cfg.Model
		return nil, err
	}
	c, err := New(cfg.BaseURL, cfg.Token(), cfg.Model)
	if err != nil {
		return nil, err
	}
	if cfg.Timeout > 0 {
		base := c.HTTP
		if base == nil {
			base = &http.Client{}
		}
		cloned := *base
		cloned.Timeout = cfg.Timeout
		c.HTTP = &cloned
	}
	// The role's configured reasoning effort becomes the client default so the
	// app's runtime paths (fast_reasoning / vision_ocr_fallback = none,
	// agent_reasoning = low) are bounded by default; a per-request
	// ChatRequest.ReasoningEffort still overrides it.
	c.reasoningEffortDefault = string(cfg.ReasoningEffort)
	return c, nil
}

// String renders the client identity for logs with the credential masked, so a
// stray %v of a client can never print a token.
func (c *Client) String() string {
	return fmt.Sprintf("openai.Client{BaseURL: %q, Model: %q, Token: %q, HTTP: %s}",
		c.BaseURL, c.Model, mask(c.Token), transportLabel(c.HTTP))
}

// LogValue keeps the same guarantee for structured logging.
func (c *Client) LogValue() any {
	token := ""
	if c.Token != "" {
		token = providers.MaskValue
	}
	return map[string]any{
		"base_url": c.BaseURL,
		"model":    c.Model,
		"token":    token,
	}
}

func mask(secret string) string {
	if secret == "" {
		return ""
	}
	return providers.MaskValue
}

func transportLabel(h *http.Client) string {
	if h == nil {
		return "context-deadline-only"
	}
	return fmt.Sprintf("timeout=%s", h.Timeout)
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	// No transport-level Timeout: the context deadline is the single source of
	// truth so a superseded observation cancels promptly (docs/spec/13).
	// Clients built by hand (tests, tooling) still get the redirect guard:
	// it derives from BaseURL, so no configuration is lost.
	return &http.Client{CheckRedirect: providers.SafeRedirect(c.BaseURL)}
}

func (c *Client) maxImageBytes() int {
	if c.MaxImageBytes > 0 {
		return c.MaxImageBytes
	}
	return MaxImageBytes
}

// Capabilities returns a read-only snapshot of the capability findings this
// client has recorded for its (base URL, model) (docs/spec/06 "Model-serving
// version and health negotiation"). It is a cache of observed behaviour for
// diagnostics, not a live probe.
func (c *Client) Capabilities() Capabilities {
	c.mu.Lock()
	defer c.mu.Unlock()
	return Capabilities{JSONMode: c.jsonModeState, ToolChoiceRequired: c.toolChoiceReqState, ReasoningEffort: c.reasoningEffortState}
}

func (c *Client) jsonModeRejected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.jsonModeState == CapabilityRejected
}

func (c *Client) setJSONMode(state CapabilityState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.jsonModeState = state
}

func (c *Client) toolChoiceReqRejected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.toolChoiceReqState == CapabilityRejected
}

func (c *Client) setToolChoiceRequired(state CapabilityState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.toolChoiceReqState = state
}

func (c *Client) reasoningEffortRejected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reasoningEffortState == CapabilityRejected
}

func (c *Client) setReasoningEffort(state CapabilityState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reasoningEffortState = state
}

func (c *Client) endpoint(path string) (string, error) {
	base := strings.TrimSpace(c.BaseURL)
	if base == "" {
		return "", providers.NewError(providers.KindUnsupportedCapability, 0, "openai: BaseURL is empty")
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("openai: invalid base URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("openai: base URL scheme %q is not supported", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	return u.String(), nil
}

// do issues a request and returns the response latency and headers,
// classifying every failure mode as a *providers.Error.
func (c *Client) do(ctx context.Context, method, endpoint string, body any, out any) (time.Duration, http.Header, error) {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, &providers.Error{Kind: providers.KindBadSchema, Msg: "openai: cannot encode request", Err: err}
		}
		reader = bytes.NewReader(buf)
	}
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return time.Since(start), nil, c.transportError(err, 0)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", DefaultUserAgent)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return time.Since(start), nil, c.transportError(err, 0)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) }()
	// Drain-and-close keeps the connection reusable; the body is already read.
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	latency := time.Since(start)
	reqID := requestID(resp.Header)
	if readErr != nil {
		return latency, resp.Header, c.transportError(readErr, resp.StatusCode)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return latency, resp.Header, c.httpError(resp.StatusCode, respBody, reqID, latency)
	}
	if out != nil {
		if err := json.Unmarshal(respBody, out); err != nil {
			e := providers.NewError(providers.KindBadSchema, resp.StatusCode, "openai: response is not valid JSON")
			e.Err, e.RequestID, e.Model, e.Retryable = err, reqID, c.Model, false
			return latency, resp.Header, e
		}
	}
	return latency, resp.Header, nil
}

const maxResponseBytes = 8 << 20

func requestID(h http.Header) string {
	for _, k := range []string{"X-Request-Id", "Request-Id", "X-Amzn-Requestid", "X-Oneapi-Request-Id"} {
		if v := strings.TrimSpace(h.Get(k)); v != "" {
			return v
		}
	}
	return ""
}

// transportError classifies a transport-level failure, distinguishing caller
// cancellation from an expired deadline from an unreachable endpoint.
func (c *Client) transportError(err error, status int) *providers.Error {
	// A transport that has already classified its own failure into the shared
	// taxonomy (for example a recorded-cassette miss from
	// internal/providers/recorded) is respected rather than flattened into a
	// generic transport failure: the classification and the correlation handle
	// in its message are the useful part. The original error is kept as the
	// cause so transport-specific types stay reachable with errors.As.
	var classified *providers.Error
	if errors.As(err, &classified) {
		out := *classified
		if out.Model == "" {
			out.Model = c.Model
		}
		if _, isTaxonomy := err.(*providers.Error); !isTaxonomy {
			out.Err = err
		}
		return &out
	}
	var e *providers.Error
	switch {
	case errors.Is(err, context.Canceled):
		e = providers.NewError(providers.KindCancelled, status, "openai: request cancelled by caller")
	case errors.Is(err, context.DeadlineExceeded):
		e = providers.NewError(providers.KindTimeout, status, "openai: request deadline exceeded")
	default:
		// Transport strings are not echoed verbatim: they can embed the full
		// request URL, and a URL with credentials would then reach a log.
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline"):
			e = providers.NewError(providers.KindTimeout, status, "network timeout")
		case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"),
			strings.Contains(msg, "connection reset"), strings.Contains(msg, "eof"),
			strings.Contains(msg, "unexpected ssl"), strings.Contains(msg, "tls handshake"):
			e = providers.NewError(providers.KindUnavailable, status, "endpoint unreachable")
		default:
			e = providers.NewError(providers.KindUnavailable, status, "openai: transport failure")
		}
	}
	// The context error is preserved for errors.Is; the raw transport string is
	// not, because it may contain the endpoint URL. The redirect-refusal
	// sentinel is preserved too: it names the control that refused the hop.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, providers.ErrRedirectRefused) {
		e.Err = err
	}
	e.Model = c.Model
	// A refused redirect is a configuration fact, not a transient one: the
	// next attempt would follow the same redirect (docs/spec/06 retry rule).
	e.Retryable = e.Kind.RetryableByDefault() && !errors.Is(err, providers.ErrRedirectRefused)
	return e
}

// httpError maps a non-2xx response onto the taxonomy. The provider message is
// redacted and truncated, and a 404 whose body names a model is treated as a
// capability gap rather than a routing accident.
func (c *Client) httpError(status int, body []byte, reqID string, latency time.Duration) *providers.Error {
	providerMsg := extractProviderMessage(body)
	kind := kindForStatus(status, providerMsg)
	msg := fmt.Sprintf("openai: %s", http.StatusText(status))
	if providerMsg != "" {
		msg += ": " + providerMsg
	}
	e := providers.NewError(kind, status, msg)
	e.Retryable = retryableStatus(kind, status)
	e.RequestID = reqID
	e.Model = c.Model
	e.Err = &HTTPStatusError{Status: status, RequestID: reqID, Latency: latency}
	return e
}

// HTTPStatusError carries the status code and timing for the router's metrics
// without holding the (possibly large, possibly sensitive) response body.
type HTTPStatusError struct {
	Status    int
	RequestID string
	Latency   time.Duration
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("provider returned http %d in %s (request_id=%s)", e.Status, e.Latency.Round(time.Millisecond), e.RequestID)
}

// kindForStatus maps an HTTP status plus provider text to a taxonomy kind.
func kindForStatus(status int, providerMsg string) providers.Kind {
	lower := strings.ToLower(providerMsg)
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		return providers.KindAuth
	case status == http.StatusRequestTimeout:
		return providers.KindTimeout
	case status == http.StatusRequestEntityTooLarge:
		return providers.KindPayloadTooLarge
	case status == http.StatusTooManyRequests:
		return providers.KindRateLimited
	case status == http.StatusNotFound:
		// A 404 from a gateway almost always means the model id (or the route)
		// is not served here: a permanent capability failure, not a transient
		// one. Retrying it just wastes a deadline.
		return providers.KindUnsupportedCapability
	case status >= 500:
		return providers.KindUnavailable
	case status == http.StatusBadRequest, status == http.StatusUnprocessableEntity, status == http.StatusUnsupportedMediaType:
		if mentionsCapability(lower) {
			return providers.KindUnsupportedCapability
		}
		return providers.KindBadSchema
	default:
		if status >= 400 {
			return providers.KindBadSchema
		}
		return providers.KindUnavailable
	}
}

func mentionsCapability(msg string) bool {
	for _, hint := range []string{"not supported", "unsupported", "does not exist", "no such model",
		"unknown model", "invalid model", "not found", "unknown role", "response_format", "image",
		"developer", "max_tokens", "temperature", "tool_choice", "reasoning_effort"} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// retryableStatus decides retryability by taxonomy kind. docs/spec/06 requires
// retries to distinguish transient network failure from permanent schema or
// policy failure: rate limits, unreachable endpoints and timeouts are treated
// as transient; auth, payload, capability and schema failures are permanent and
// must fail over or degrade rather than be re-attempted.
func retryableStatus(kind providers.Kind, status int) bool {
	switch kind {
	case providers.KindRateLimited, providers.KindUnavailable, providers.KindTimeout:
		return true
	default:
		return false
	}
}

// extractProviderMessage pulls the message out of an OpenAI-style error body
// without dumping the whole payload.
func extractProviderMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    json.RawMessage
			CodeStr string `json:"-"`
		} `json:"error"`
		Detail string `json:"detail"`
		Msg    string `json:"msg"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.Error.Message != "" {
			if payload.Error.Type != "" {
				return payload.Error.Type + ": " + payload.Error.Message
			}
			return payload.Error.Message
		}
		for _, v := range []string{payload.Detail, payload.Msg} {
			if v != "" {
				return v
			}
		}
	}
	// Plain-text or HTML error pages: keep the first line only.
	text := strings.TrimSpace(string(body))
	if i := strings.IndexAny(text, "\n\r"); i >= 0 {
		text = text[:i]
	}
	return text
}

// chatMessage is the wire form of one message.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	ToolCalls  []wireToolCall `json:"tool_calls,omitempty"`
}

type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

type chatRequestBody struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	MaxTokens      int             `json:"max_tokens,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	Stream         bool            `json:"stream"`
	ResponseFormat *responseFormat `json:"response_format,omitempty"`
	Tools          []wireTool      `json:"tools,omitempty"`
	ToolChoice     any             `json:"tool_choice,omitempty"`
	// ReasoningEffort is sent only when non-empty (the gateway honours
	// "none"/"low"/"medium"/"high" on chat completions; per-request thinking
	// budget fields are ignored by the serving stacks we tested). The value is
	// dropped for the retry attempt when the gateway rejects the field.
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
}

type responseFormat struct {
	Type string `json:"type"`
}

// buildMessages converts the neutral request into the wire form, rejecting
// unsupported roles and oversized images before anything leaves the process.
func (c *Client) buildMessages(req ChatRequest) ([]chatMessage, error) {
	if len(req.Messages) == 0 {
		return nil, providers.NewError(providers.KindBadSchema, 0, "openai: chat request has no messages")
	}
	out := make([]chatMessage, 0, len(req.Messages))
	for i, m := range req.Messages {
		role := strings.TrimSpace(m.Role)
		if role == "" {
			return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d has an empty role", i))
		}
		if strings.EqualFold(role, roleDeveloper) {
			// Observed on the live gateway: the ChatML "developer" role is
			// rejected with a 400. Fail locally with the same taxonomy kind.
			return nil, providers.NewError(providers.KindUnsupportedCapability, 0,
				`openai: message role "developer" is not supported by this gateway; use "system" or "user"`)
		}
		if strings.EqualFold(role, "tool") {
			// A tool-result message: the content is the (wrapped) result text
			// and tool_call_id is what links it to the call being answered.
			if m.ToolCallID == "" {
				return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d has role tool but no tool_call_id", i))
			}
			if m.ToolCalls != nil || len(m.Images) != 0 {
				return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d (tool) must carry text only", i))
			}
			out = append(out, chatMessage{Role: role, Content: m.Text, ToolCallID: m.ToolCallID})
			continue
		}
		if len(m.ToolCalls) > 0 {
			// An assistant tool-call message. An empty Text becomes JSON null,
			// which the verified gateway returns and accepts (qwen3-8-27b sends
			// content null on a tool_calls turn).
			if !strings.EqualFold(role, "assistant") {
				return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d carries tool_calls but is not an assistant message", i))
			}
			cm := chatMessage{Role: role, ToolCalls: toolCallsToWire(m.ToolCalls)}
			if strings.TrimSpace(m.Text) != "" {
				cm.Content = m.Text
			}
			for j, c := range m.ToolCalls {
				if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.Name) == "" {
					return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d tool_call %d has no id or name", i, j))
				}
				if len(c.Arguments) > 0 && !json.Valid(c.Arguments) {
					return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d tool_call %s has invalid JSON arguments", i, c.ID))
				}
			}
			out = append(out, cm)
			continue
		}
		if len(m.Images) == 0 {
			out = append(out, chatMessage{Role: role, Content: m.Text})
			continue
		}
		parts := make([]contentPart, 0, len(m.Images)+1)
		if m.Text != "" {
			parts = append(parts, contentPart{Type: "text", Text: m.Text})
		}
		for j, img := range m.Images {
			if len(img) == 0 {
				return nil, providers.NewError(providers.KindBadSchema, 0, fmt.Sprintf("openai: message %d image %d is empty", i, j))
			}
			if len(img) > c.maxImageBytes() {
				return nil, providers.NewError(providers.KindPayloadTooLarge, 0, fmt.Sprintf(
					"openai: message %d image %d is %d bytes, over the %d byte limit; downsample or re-encode the crop",
					i, j, len(img), c.maxImageBytes()))
			}
			parts = append(parts, contentPart{
				Type:     "image_url",
				ImageURL: &imageURL{URL: "data:" + detectImageMIME(img) + ";base64," + base64.StdEncoding.EncodeToString(img)},
			})
		}
		out = append(out, chatMessage{Role: role, Content: parts})
	}
	return out, nil
}

// detectImageMIME sniffs the container so the data URL media type is honest;
// gateways that validate it will reject image/png for JPEG bytes.
func detectImageMIME(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 6 && (bytes.Equal(b[:4], []byte("GIF8"))):
		return "image/gif"
	case len(b) >= 12 && bytes.Equal(b[4:8], []byte("ftyp")) && (bytes.Equal(b[8:12], []byte("avif"))):
		return "image/avif"
	case len(b) >= 12 && string(b[0:4]) == "RIFF" && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	default:
		return "application/octet-stream"
	}
}

// buildBody assembles the wire body for one attempt. jsonMode controls whether
// response_format is sent; choice is the already-projected tool_choice value.
// Factored out of Chat so the capability-fallback retry can rebuild the body
// with a degraded feature without duplicating the sampling/field mapping.
func (c *Client) buildBody(req ChatRequest, messages []chatMessage, tools []wireTool, choice any, jsonMode bool) chatRequestBody {
	body := chatRequestBody{
		Model:      c.Model,
		Messages:   messages,
		Tools:      tools,
		ToolChoice: choice,
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		t := *req.Temperature
		body.Temperature = &t
	}
	if jsonMode {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	if strings.TrimSpace(req.ReasoningEffort) != "" {
		body.ReasoningEffort = strings.TrimSpace(req.ReasoningEffort)
	}
	return body
}

// Chat performs one chat completion.
//
// When JSONModeFallback / ToolChoiceFallback are set, a gateway 400 that
// rejects the corresponding capability is degraded and retried exactly once
// (docs/spec/06 "Model-serving version and health negotiation"): JSON mode
// retries without response_format plus a "single JSON object only" instruction
// and then validates the response is one parseable object; tool_choice=required
// retries as auto plus a "call a tool" instruction and reports ErrToolCallMissing
// if no call arrives. A 400 that names reasoning_effort degrades the request
// to "omit the field" and retries once without it. Each finding is cached per
// (base URL, model) so later calls skip the doomed first attempt, and when
// several degradations apply at once the retry carries the composed instruction
// (all degraded features), never just the last one. Without the opt-in flags,
// a rejection still surfaces as a typed *providers.Error (KindUnsupportedCapability)
// tagged with the capability sentinel.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	tools, err := toolsToWire(req.Tools)
	if err != nil {
		return ChatResponse{}, err
	}
	endpoint, err := c.endpoint("/chat/completions")
	if err != nil {
		return ChatResponse{}, err
	}

	wantJSON := req.ResponseFormatJSON
	wantRequired := req.ToolChoice == ToolChoiceRequired

	// reasoning_effort is a per-request field: a non-empty
	// ChatRequest.ReasoningEffort wins over the role default (NewFromRole);
	// otherwise the role default applies, and the empty value sends nothing
	// (the model's own default). A cached "rejected" finding means the first
	// attempt already omits it (skip the doomed first attempt).
	bodyReq := req
	if strings.TrimSpace(bodyReq.ReasoningEffort) != "" {
		bodyReq.ReasoningEffort = strings.TrimSpace(bodyReq.ReasoningEffort)
	} else {
		bodyReq.ReasoningEffort = strings.TrimSpace(c.reasoningEffortDefault)
	}

	// Apply the cached capability findings up front so a known-rejected
	// capability is skipped on the first attempt and the degraded form is sent
	// instead (the "skip the doomed first attempt" behaviour).
	jsonMode := wantJSON
	jsonDegraded := false
	toolChoice := req.ToolChoice
	toolDegraded := false
	if wantJSON && req.JSONModeFallback && c.jsonModeRejected() {
		jsonMode = false
		jsonDegraded = true
	}
	if wantRequired && req.ToolChoiceFallback && c.toolChoiceReqRejected() {
		toolChoice = ToolChoiceAuto
		toolDegraded = true
	}
	if bodyReq.ReasoningEffort != "" && c.reasoningEffortRejected() {
		bodyReq.ReasoningEffort = ""
	}

	// rebuildBody assembles the wire body for the current attempt from the
	// live feature flags, so every retry re-derives the messages (with the
	// composed degraded instruction), tool_choice and reasoning_effort in one
	// place. When both the JSON and the tool_choice degradation apply, the
	// instruction carries both parts instead of the second rebuild discarding
	// the first.
	rebuildBody := func() (chatRequestBody, error) {
		msgs, err := c.buildMessages(ChatRequest{Messages: withFallbackInstruction(req.Messages, jsonDegraded, toolDegraded)})
		if err != nil {
			return chatRequestBody{}, err
		}
		ch, err := choiceToWire(toolChoice)
		if err != nil {
			return chatRequestBody{}, err
		}
		return c.buildBody(bodyReq, msgs, tools, ch, jsonMode), nil
	}

	body, err := rebuildBody()
	if err != nil {
		return ChatResponse{}, err
	}
	// Track what the *current* attempt sends so a post-attempt error is only
	// tagged with a capability sentinel when the failing request actually
	// exercised the rejected feature.
	sentJSON := jsonMode
	sentRequired := toolChoice == ToolChoiceRequired
	sentReasoning := bodyReq.ReasoningEffort != ""

	var payload chatResponsePayload
	latency, header, callErr := c.do(ctx, http.MethodPost, endpoint, body, &payload)
	// Bounded degradation loop (docs/spec/06 "Model-serving version and
	// health negotiation"): a 400 that names a feature the current attempt
	// sent is a capability finding, not a schema accident — degrade that one
	// feature, cache the finding and retry. Each capability degrades at most
	// once (its sent* flag flips and stays off), so a call makes at most
	// three attempts: the initial attempt plus one retry per rejected
	// feature. When both the JSON and the tool_choice degradation apply in
	// one call, the loop degrades them in sequence and every rebuild carries
	// the composed instruction of all features degraded so far.
	for callErr != nil {
		degraded := false
		switch {
		// First attempt sent response_format and the gateway rejected it:
		// degrade once, without response_format, with the JSON instruction.
		case wantJSON && req.JSONModeFallback && sentJSON && IsJSONModeRejection(callErr):
			c.setJSONMode(CapabilityRejected)
			jsonMode, jsonDegraded, sentJSON = false, true, false
			degraded = true
		// First attempt sent tool_choice=required and the gateway rejected it:
		// degrade once, to auto, with the call-a-tool instruction.
		case wantRequired && req.ToolChoiceFallback && sentRequired && IsToolChoiceRejection(callErr):
			c.setToolChoiceRequired(CapabilityRejected)
			toolChoice = ToolChoiceAuto
			toolDegraded, sentRequired = true, false
			degraded = true
		// First attempt sent reasoning_effort and the gateway rejected the
		// field: degrade once by omitting it (the model default applies).
		case sentReasoning && IsReasoningEffortRejection(callErr):
			c.setReasoningEffort(CapabilityRejected)
			bodyReq.ReasoningEffort = ""
			sentReasoning = false
			degraded = true
		}
		if !degraded {
			break
		}
		body, err = rebuildBody()
		if err != nil {
			return ChatResponse{}, err
		}
		latency, header, callErr = c.do(ctx, http.MethodPost, endpoint, body, &payload)
	}
	if callErr != nil {
		return ChatResponse{}, tagCapabilityError(callErr, wantJSON, sentJSON, wantRequired, sentRequired)
	}

	out, err := payload.toChatResponse(c.Model)
	if err != nil {
		// out is returned even on error: the budget-exhausted path fills it
		// with the finish reason, usage and reasoning so diagnostics keep the
		// correlation and token accounting of the failed attempt.
		return out, err
	}
	out.Latency = latency
	// The response body may also carry an id; the header is the correlation
	// handle the gateway logs by, so it wins.
	out.RequestID = requestID(header)
	if out.RequestID == "" {
		out.RequestID = payload.ID
	}

	// Record a successful probe of the feature the final attempt sent.
	if wantJSON && sentJSON {
		c.setJSONMode(CapabilitySupported)
	}
	if wantRequired && sentRequired {
		c.setToolChoiceRequired(CapabilitySupported)
	}
	if sentReasoning {
		c.setReasoningEffort(CapabilitySupported)
	}

	// The degraded JSON path: the gateway does not guarantee structured output,
	// so validate the model still produced one parseable JSON object. A
	// non-object answer is a capability failure of the degraded path, typed and
	// not retried. The text is normalised to the extracted object so callers
	// get exactly one clean object.
	if jsonDegraded {
		obj, ok := extractJSONObject(out.Text)
		if !ok {
			e := providers.NewError(providers.KindBadSchema, http.StatusOK,
				"openai: JSON-mode fallback response is not a single parseable JSON object")
			e.Model = out.Model
			e.RequestID = out.RequestID
			return out, e
		}
		out.Text = string(obj)
	}

	// The degraded tool_choice=required -> auto path (whether skipped up front
	// from the cache or retried after a rejection): if the model still did not
	// call a tool, report the capability gap rather than a silent success.
	if toolDegraded && len(out.ToolCalls) == 0 {
		e := providers.NewError(providers.KindUnsupportedCapability, 0,
			"openai: tool_choice required degraded to auto but the model still did not call a tool")
		e.Err = ErrToolCallMissing
		e.Model = out.Model
		e.RequestID = out.RequestID
		return out, e
	}
	return out, nil
}

// IsJSONModeRejection reports whether an error looks like a gateway refusing
// structured output rather than a transport or schema accident, so callers can
// record it as a capability finding (docs/spec/06 "Model-serving version and
// health negotiation") instead of a contract violation.
func IsJSONModeRejection(err error) bool {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return false
	}
	if pe.Kind != providers.KindUnsupportedCapability && pe.Kind != providers.KindBadSchema {
		return false
	}
	lower := strings.ToLower(pe.Msg)
	return strings.Contains(lower, "response_format") || strings.Contains(lower, "response format") ||
		strings.Contains(lower, "json")
}

// IsToolChoiceRejection reports whether an error looks like a gateway refusing
// tool_choice=required rather than a transport or schema accident, so callers
// can record it as a capability finding. Mirrors IsJSONModeRejection.
func IsToolChoiceRejection(err error) bool {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return false
	}
	if pe.Kind != providers.KindUnsupportedCapability && pe.Kind != providers.KindBadSchema {
		return false
	}
	return strings.Contains(strings.ToLower(pe.Msg), "tool_choice")
}

// IsReasoningEffortRejection reports whether an error looks like a gateway
// refusing the reasoning_effort field rather than a transport or schema
// accident, so the adapter can record it as a capability finding and retry
// once without the field (mirrors IsJSONModeRejection). Matches on the field
// name in both spellings the serving stacks use.
func IsReasoningEffortRejection(err error) bool {
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return false
	}
	if pe.Kind != providers.KindUnsupportedCapability && pe.Kind != providers.KindBadSchema {
		return false
	}
	lower := strings.ToLower(pe.Msg)
	return strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning effort")
}

// tagCapabilityError wraps a failed capability request in its typed sentinel
// (ErrUnsupportedResponseFormat / ErrUnsupportedToolChoice) so callers and
// diagnostics can match the exact gap. It only tags when the *failing* request
// actually exercised the rejected feature (sentJSON / sentRequired), so an
// unrelated error on a degraded retry is not mislabelled. The existing
// *providers.Error classification (KindUnsupportedCapability) is preserved; the
// sentinel is attached as the cause so errors.Is(err, sentinel) also matches.
func tagCapabilityError(err error, wantJSON, sentJSON, wantRequired, sentRequired bool) error {
	var sentinel error
	switch {
	case wantJSON && sentJSON && IsJSONModeRejection(err):
		sentinel = ErrUnsupportedResponseFormat
	case wantRequired && sentRequired && IsToolChoiceRejection(err):
		sentinel = ErrUnsupportedToolChoice
	}
	if sentinel == nil {
		return err
	}
	var pe *providers.Error
	if !errors.As(err, &pe) {
		return err
	}
	pe.Err = sentinel
	return err
}

// injectInstruction returns a copy of msgs with instruction merged into the
// leading system message (or prepended as a new one when none exists). The
// original slice and its messages are not mutated. Used by the capability
// fallback retries, which steer the model with a system instruction.
func injectInstruction(msgs []Message, instruction string) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		if strings.EqualFold(strings.TrimSpace(out[i].Role), "system") {
			if strings.TrimSpace(out[i].Text) != "" {
				out[i].Text = strings.TrimSpace(out[i].Text) + "\n" + instruction
			} else {
				out[i].Text = instruction
			}
			return out
		}
	}
	prepended := make([]Message, 0, len(out)+1)
	prepended = append(prepended, Message{Role: "system", Text: instruction})
	return append(prepended, out...)
}

// withJSONOnlyInstruction is the JSON-mode fallback instruction.
const (
	jsonOnlyInstruction = "Answer with a single JSON object only."
	toolCallInstruction = "You must answer by calling a tool; do not answer with plain text."
)

// composeInstruction merges the degraded-capability instructions that apply to
// the current attempt into one system instruction. Composing (instead of
// rebuilding from the original messages on every fallback) is what keeps the
// JSON instruction alive when the tool_choice fallback fires second — and
// vice versa.
func composeInstruction(jsonDegraded, toolDegraded bool) string {
	var parts []string
	if jsonDegraded {
		parts = append(parts, jsonOnlyInstruction)
	}
	if toolDegraded {
		parts = append(parts, toolCallInstruction)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// withFallbackInstruction merges the composed degraded instructions into the
// leading system message; with no degraded feature it returns msgs unchanged
// (no empty instruction is ever injected).
func withFallbackInstruction(msgs []Message, jsonDegraded, toolDegraded bool) []Message {
	instruction := composeInstruction(jsonDegraded, toolDegraded)
	if instruction == "" {
		return msgs
	}
	return injectInstruction(msgs, instruction)
}

// extractJSONObject returns the outermost balanced {...} substring of text as a
// validated JSON object, reporting whether text contains one such parseable
// object. Used by the JSON-mode fallback, where the gateway does not guarantee
// structured output: the model is steered by instruction and the response is
// then checked to actually be a single object.
func extractJSONObject(text string) (json.RawMessage, bool) {
	start := strings.Index(text, "{")
	if start < 0 {
		return nil, false
	}
	depth := 0
	for i := start; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				candidate := text[start : i+1]
				if !json.Valid([]byte(candidate)) {
					return nil, false
				}
				return json.RawMessage(candidate), true
			}
		}
	}
	return nil, false
}

type chatResponsePayload struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string          `json:"role"`
			Content          *string         `json:"content"`
			ReasoningContent *string         `json:"reasoning_content"`
			ToolCalls        json.RawMessage `json:"tool_calls"`
		} `json:"message"`
		Text *string `json:"text"`
	} `json:"choices"`
	Usage struct {
		PromptTokens      int  `json:"prompt_tokens"`
		CompletionTokens  int  `json:"completion_tokens"`
		TotalTokens       int  `json:"total_tokens"`
		ReasoningTokens   *int `json:"reasoning_tokens"`
		CompletionDetails *struct {
			ReasoningTokens *int `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
	} `json:"usage"`
	ErrorField json.RawMessage `json:"error"`
}

func (p chatResponsePayload) toChatResponse(requestedModel string) (ChatResponse, error) {
	if len(p.Choices) == 0 {
		e := providers.NewError(providers.KindBadSchema, http.StatusOK, "openai: response contains no choices")
		e.Model = p.Model
		return ChatResponse{}, e
	}
	if len(p.Choices) > 1 {
		// n>1 is never requested; a gateway returning several choices means the
		// caller cannot know which one to narrate.
		e := providers.NewError(providers.KindBadSchema, http.StatusOK, "openai: response contains multiple choices but only one is expected")
		e.Model = p.Model
		return ChatResponse{}, e
	}
	choice := p.Choices[0]
	out := ChatResponse{
		FinishReason: choice.FinishReason,
		Model:        p.Model,
	}
	if out.Model == "" {
		out.Model = requestedModel
	}
	text := ""
	if choice.Message.Content != nil {
		text = *choice.Message.Content
	} else if choice.Text != nil {
		// A few compatible servers answer completions-style responses.
		text = *choice.Text
	}
	if choice.Message.ReasoningContent != nil {
		out.Reasoning = strings.TrimSpace(*choice.Message.ReasoningContent)
	}
	calls, err := parseToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return ChatResponse{}, err
	}
	usage := Usage{
		Prompt:     p.Usage.PromptTokens,
		Completion: p.Usage.CompletionTokens,
	}
	if p.Usage.ReasoningTokens != nil {
		usage.Reasoning = *p.Usage.ReasoningTokens
	} else if p.Usage.CompletionDetails != nil && p.Usage.CompletionDetails.ReasoningTokens != nil {
		usage.Reasoning = *p.Usage.CompletionDetails.ReasoningTokens
	}
	// finish_reason "length" with neither text nor a tool call means the model
	// spent the whole output budget (observed live on qwen3-8-27b: it burns
	// max_tokens on reasoning and emits an empty answer) — a distinct,
	// actionable failure (raise the budget or lower the reasoning effort) that
	// callers must not confuse with a schema-less empty message. A
	// reasoning-only answer on "stop" stays a normal response.
	if strings.EqualFold(choice.FinishReason, "length") && strings.TrimSpace(text) == "" && len(calls) == 0 {
		reasoning := ""
		if choice.Message.ReasoningContent != nil {
			reasoning = strings.TrimSpace(*choice.Message.ReasoningContent)
		}
		e := providers.NewError(providers.KindBudgetExhausted, http.StatusOK,
			fmt.Sprintf("openai: output budget exhausted: finish_reason=length with no text and no tool calls (usage prompt=%d completion=%d reasoning=%d)",
				usage.Prompt, usage.Completion, usage.Reasoning))
		e.Err = ErrOutputBudgetExhausted
		e.Model = p.Model
		e.RequestID = p.ID
		resp := ChatResponse{FinishReason: choice.FinishReason, Model: out.Model, Reasoning: reasoning, Usage: usage, RequestID: p.ID}
		return resp, e
	}
	if strings.TrimSpace(text) == "" && out.Reasoning == "" && len(calls) == 0 {
		e := providers.NewError(providers.KindBadSchema, http.StatusOK,
			"openai: response message has no content, no reasoning_content and no tool_calls")
		e.Model = out.Model
		e.RequestID = p.ID
		return ChatResponse{}, e
	}
	out.Text = strings.TrimSpace(text)
	out.ToolCalls = calls
	out.Usage = usage
	return out, nil
}

// modelsResponse is the documented listing envelope: {"data":[{"id":...}]}.
type modelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
	} `json:"data"`
	// Some gateways answer with a bare array instead.
	bare []string
}

func (m *modelsResponse) UnmarshalJSON(b []byte) error {
	type alias modelsResponse
	var a alias
	if err := json.Unmarshal(b, &a); err == nil && len(a.Data) > 0 {
		*m = modelsResponse(a)
		return nil
	}
	var arr []any
	if err := json.Unmarshal(b, &arr); err == nil {
		for _, item := range arr {
			switch v := item.(type) {
			case string:
				m.bare = append(m.bare, v)
			case map[string]any:
				if id, ok := v["id"].(string); ok && id != "" {
					m.Data = append(m.Data, struct {
						ID      string `json:"id"`
						Object  string `json:"object"`
						OwnedBy string `json:"owned_by"`
					}{ID: id})
				}
			}
		}
		if len(m.Data) > 0 || len(m.bare) > 0 {
			return nil
		}
	}
	var lastErr error
	if err := json.Unmarshal(b, &a); err != nil {
		lastErr = err
	} else {
		lastErr = errors.New("no model ids in response")
	}
	return lastErr
}

// ListModels returns the deployed model ids. The gateway health/capability
// negotiation in docs/spec/06 starts here: a role whose model id is absent from
// this list is misconfigured, not transiently broken.
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	endpoint, err := c.endpoint("/models")
	if err != nil {
		return nil, err
	}
	var payload modelsResponse
	if _, _, callErr := c.do(ctx, http.MethodGet, endpoint, nil, &payload); callErr != nil {
		return nil, callErr
	}
	ids := make([]string, 0, len(payload.Data)+len(payload.bare))
	for _, d := range payload.Data {
		if d.ID != "" {
			ids = append(ids, d.ID)
		}
	}
	ids = append(ids, payload.bare...)
	if len(ids) == 0 {
		return nil, providers.NewError(providers.KindBadSchema, http.StatusOK, "openai: model listing contains no ids")
	}
	return ids, nil
}

// Health performs the cheapest available probe. /health is not guaranteed on a
// compatible gateway, so a non-200 is reported as a capability finding rather
// than a hard failure of the adapter.
func (c *Client) Health(ctx context.Context) error {
	endpoint, err := c.endpoint("/health")
	if err != nil {
		return err
	}
	if _, _, callErr := c.do(ctx, http.MethodGet, endpoint, nil, nil); callErr != nil {
		return callErr
	}
	return nil
}
