package systemone

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// doRaw issues one POST and returns the response body plus provenance, with
// the effective request id and its source. Every request carries a
// client-minted ULID as X-Request-Id: the live gateway returns no request-id
// header on the /systemone route (ADR-0021 amendment), so the id we sent is
// what makes the provider-side logs searchable when it answers none of its
// own. Every failure is a *providers.Error classified the way the openai
// adapter does and carries the same effective id.
func (c *Client) doRaw(ctx context.Context, raw []byte) (time.Duration, http.Header, []byte, string, string, error) {
	clientID := newRequestID()
	endpoint, err := c.endpoint()
	if err != nil {
		return 0, nil, nil, "", "", c.transportError(err, 0, clientID)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, "", "", c.transportError(err, 0, clientID)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "game-narrator/0.0 m3-systemone")
	req.Header.Set("X-Request-Id", clientID)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	start := time.Now()
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return time.Since(start), nil, nil, "", "", c.transportError(err, 0, clientID)
	}
	defer func() { io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16)) }()
	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	latency := time.Since(start)
	header := resp.Header
	reqID, reqIDSource := RecordRequestID(requestID(header), clientID)
	if readErr != nil {
		return latency, header, nil, reqID, reqIDSource, c.transportError(readErr, resp.StatusCode, reqID)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return latency, header, nil, reqID, reqIDSource, c.httpError(resp.StatusCode, respBody, reqID)
	}
	return latency, header, respBody, reqID, reqIDSource, nil
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
// reqID is the correlation id to attach (the client-minted X-Request-Id, or
// the effective id once the response headers are known); it is recorded on
// the error so even a dead request stays searchable in provider logs.
func (c *Client) transportError(err error, status int, reqID string) *providers.Error {
	// A transport that has already classified its own failure into the shared
	// taxonomy (for example a recorded-cassette miss) is respected rather than
	// flattened into a generic transport failure.
	var classified *providers.Error
	if errors.As(err, &classified) {
		out := *classified
		if out.Model == "" {
			out.Model = c.Model
		}
		if out.RequestID == "" {
			out.RequestID = reqID
		}
		if _, isTaxonomy := err.(*providers.Error); !isTaxonomy {
			out.Err = err
		}
		return &out
	}
	var e *providers.Error
	switch {
	case errors.Is(err, context.Canceled):
		e = providers.NewError(providers.KindCancelled, status, "systemone: request cancelled by caller")
	case errors.Is(err, context.DeadlineExceeded):
		e = providers.NewError(providers.KindTimeout, status, "systemone: request deadline exceeded")
	default:
		// Transport strings are not echoed verbatim: they can embed the full
		// request URL, and a URL with credentials would then reach a log.
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
			e = providers.NewError(providers.KindTimeout, status, "network timeout")
		case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"),
			strings.Contains(msg, "connection reset"), strings.Contains(msg, "eof"),
			strings.Contains(msg, "unexpected ssl"), strings.Contains(msg, "tls handshake"):
			e = providers.NewError(providers.KindUnavailable, status, "endpoint unreachable")
		default:
			e = providers.NewError(providers.KindUnavailable, status, "systemone: transport failure")
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, providers.ErrRedirectRefused) {
		e.Err = err
	}
	e.Model = c.Model
	if e.RequestID == "" {
		e.RequestID = reqID
	}
	// A refused redirect is a configuration fact, not a transient one.
	e.Retryable = e.Kind.RetryableByDefault() && !errors.Is(err, providers.ErrRedirectRefused)
	return e
}

// httpError maps a non-2xx response onto the taxonomy (docs/spec/06 "Failure
// taxonomy"): 401/403 auth, 404 unknown model/route is a capability gap,
// 413 payload, 422 schema (the gateway's detail message is preserved), 429
// rate limit, 5xx and 408 unavailable/timeout.
func (c *Client) httpError(status int, body []byte, reqID string) *providers.Error {
	providerMsg := extractProviderMessage(body)
	kind := providers.KindUnavailable
	switch {
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		kind = providers.KindAuth
	case status == http.StatusRequestTimeout:
		kind = providers.KindTimeout
	case status == http.StatusRequestEntityTooLarge:
		kind = providers.KindPayloadTooLarge
	case status == http.StatusTooManyRequests:
		kind = providers.KindRateLimited
	case status == http.StatusNotFound:
		// A 404 from the gateway almost always means the model id (or the
		// route) is not served here: a permanent capability failure.
		kind = providers.KindUnsupportedCapability
	case status >= 500:
		kind = providers.KindUnavailable
	case status == http.StatusUnprocessableEntity, status == http.StatusBadRequest:
		kind = providers.KindBadSchema
	case status >= 400:
		kind = providers.KindBadSchema
	default:
		kind = providers.KindUnavailable
	}
	msg := fmt.Sprintf("systemone: http %d %s", status, http.StatusText(status))
	if providerMsg != "" {
		msg += ": " + providerMsg
	}
	e := providers.NewError(kind, status, msg)
	e.Retryable = kind.RetryableByDefault()
	e.RequestID = reqID
	e.Model = c.Model
	return e
}

// extractProviderMessage pulls the gateway's detail out of an error body
// without dumping the whole payload. FastAPI-style {"detail": "..."} bodies
// (the observed shape of the System One route) are handled first, then
// OpenAI-style {"error":{"message":"..."}}.
func extractProviderMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var fastapi struct {
		Detail any `json:"detail"`
	}
	if err := json.Unmarshal(body, &fastapi); err == nil && fastapi.Detail != nil {
		switch d := fastapi.Detail.(type) {
		case string:
			if d != "" {
				return d
			}
		case map[string]any:
			if msg, ok := d["msg"].(string); ok && msg != "" {
				return msg
			}
		case []any:
			if len(d) > 0 {
				if s, ok := d[0].(string); ok && s != "" {
					return s
				}
			}
		}
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &payload); err == nil {
		if payload.Error.Message != "" {
			if payload.Error.Type != "" {
				return payload.Error.Type + ": " + payload.Error.Message
			}
			return payload.Error.Message
		}
		if payload.Msg != "" {
			return payload.Msg
		}
	}
	text := strings.TrimSpace(string(body))
	if i := strings.IndexAny(text, "\n\r"); i >= 0 {
		text = text[:i]
	}
	return text
}
