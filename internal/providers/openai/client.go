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

// Message is one chat message. Text carries the content; Images are PNG or
// JPEG bytes sent as base64 data URLs, in order, after the text part.
type Message struct {
	Role   string
	Text   string
	Images [][]byte
}

// ChatRequest is the provider-neutral request for one chat completion.
type ChatRequest struct {
	Messages           []Message
	MaxTokens          int
	Temperature        *float64
	ResponseFormatJSON bool
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
	Usage Usage
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
	return &Client{BaseURL: strings.TrimRight(u.String(), "/"), Token: token, Model: model}, nil
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
	return c, nil
}

// String renders the client identity for logs with the credential masked, so a
// stray %v of a client can never print a token.
func (c Client) String() string {
	return fmt.Sprintf("openai.Client{BaseURL: %q, Model: %q, Token: %q, HTTP: %s}",
		c.BaseURL, c.Model, mask(c.Token), transportLabel(c.HTTP))
}

// LogValue keeps the same guarantee for structured logging.
func (c Client) LogValue() any {
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
	return &http.Client{}
}

func (c *Client) maxImageBytes() int {
	if c.MaxImageBytes > 0 {
		return c.MaxImageBytes
	}
	return MaxImageBytes
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
	// not, because it may contain the endpoint URL.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		e.Err = err
	}
	e.Model = c.Model
	e.Retryable = e.Kind.RetryableByDefault()
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
		"developer", "max_tokens", "temperature"} {
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
	Role    string `json:"role"`
	Content any    `json:"content"`
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

// Chat performs one chat completion.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	messages, err := c.buildMessages(req)
	if err != nil {
		return ChatResponse{}, err
	}
	body := chatRequestBody{
		Model:    c.Model,
		Messages: messages,
	}
	if req.MaxTokens > 0 {
		body.MaxTokens = req.MaxTokens
	}
	if req.Temperature != nil {
		t := *req.Temperature
		body.Temperature = &t
	}
	if req.ResponseFormatJSON {
		body.ResponseFormat = &responseFormat{Type: "json_object"}
	}
	endpoint, err := c.endpoint("/chat/completions")
	if err != nil {
		return ChatResponse{}, err
	}
	var payload chatResponsePayload
	latency, header, callErr := c.do(ctx, http.MethodPost, endpoint, body, &payload)
	if callErr != nil {
		return ChatResponse{}, callErr
	}
	out, err := payload.toChatResponse(c.Model)
	if err != nil {
		return ChatResponse{}, err
	}
	out.Latency = latency
	// The response body may also carry an id; the header is the correlation
	// handle the gateway logs by, so it wins.
	out.RequestID = requestID(header)
	if out.RequestID == "" {
		out.RequestID = payload.ID
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

type chatResponsePayload struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Model   string `json:"model"`
	Created int64  `json:"created"`
	Choices []struct {
		Index        int    `json:"index"`
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string  `json:"role"`
			Content          *string `json:"content"`
			ReasoningContent *string `json:"reasoning_content"`
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
	if strings.TrimSpace(text) == "" && out.Reasoning == "" {
		e := providers.NewError(providers.KindBadSchema, http.StatusOK,
			"openai: response message has no content and no reasoning_content")
		e.Model = out.Model
		e.RequestID = p.ID
		return ChatResponse{}, e
	}
	out.Text = strings.TrimSpace(text)
	out.Usage = Usage{
		Prompt:     p.Usage.PromptTokens,
		Completion: p.Usage.CompletionTokens,
	}
	if p.Usage.ReasoningTokens != nil {
		out.Usage.Reasoning = *p.Usage.ReasoningTokens
	} else if p.Usage.CompletionDetails != nil && p.Usage.CompletionDetails.ReasoningTokens != nil {
		out.Usage.Reasoning = *p.Usage.CompletionDetails.ReasoningTokens
	}
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
