package tts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers"
)

// transportError classifies a transport-level failure, mirroring the OpenAI
// adapter (internal/providers/openai): caller cancellation vs deadline vs
// unreachable endpoint, with the context error preserved for errors.Is.
// reqID is the correlation id sent on the request (the client-minted
// X-Request-Id, or the provider's when the response headers carried one);
// empty when the request could not even be built.
func (c *Client) transportError(err error, status int, reqID string) *providers.Error {
	var classified *providers.Error
	if errors.As(err, &classified) {
		out := *classified
		if out.Model == "" {
			out.Model = c.Model
		}
		if out.RequestID == "" {
			out.RequestID = reqID
		}
		return &out
	}
	var e *providers.Error
	switch {
	case errors.Is(err, context.Canceled):
		e = providers.NewError(providers.KindCancelled, status, "tts: request cancelled by caller")
	case errors.Is(err, context.DeadlineExceeded):
		e = providers.NewError(providers.KindTimeout, status, "tts: request deadline exceeded")
	default:
		msg := strings.ToLower(err.Error())
		switch {
		case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline"):
			e = providers.NewError(providers.KindTimeout, status, "network timeout")
		case strings.Contains(msg, "connection refused"), strings.Contains(msg, "no such host"),
			strings.Contains(msg, "connection reset"), strings.Contains(msg, "eof"),
			strings.Contains(msg, "unexpected ssl"), strings.Contains(msg, "tls handshake"):
			e = providers.NewError(providers.KindUnavailable, status, "endpoint unreachable")
		default:
			e = providers.NewError(providers.KindUnavailable, status, "tts: transport failure")
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		e.Err = err
	}
	e.Model = c.Model
	e.RequestID = reqID
	e.Retryable = e.Kind.RetryableByDefault()
	return e
}

// httpError maps a non-2xx response onto the taxonomy. The provider message
// is redacted (token, endpoints) and truncated by providers.NewError.
func (c *Client) httpError(status int, body []byte, reqID string) *providers.Error {
	providerMsg := extractProviderMessage(body)
	kind := kindForStatus(status, providerMsg)
	msg := fmt.Sprintf("tts: %s", http.StatusText(status))
	if providerMsg != "" {
		msg += ": " + providerMsg
	}
	e := providers.NewError(kind, status, msg)
	e.RequestID = reqID
	e.Model = c.Model
	e.Retryable = kind.RetryableByDefault()
	switch kind {
	case providers.KindRateLimited, providers.KindUnavailable, providers.KindTimeout:
		e.Retryable = true
	default:
		e.Retryable = false
	}
	return e
}

// kindForStatus is the shared mapping with the voice-specific additions from
// docs/spec/06: a 404 is a route the deployment does not serve (the gateway
// answers exactly this for /audio/voices), a 400 naming an unsupported
// capability is unsupported_capability, and the voice patterns are handled
// by classifyGenerateError so a 400 defaulting to bad_schema stays honest.
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
		"unknown model", "invalid model", "not found", "task_type", "response_format"} {
		if strings.Contains(msg, hint) {
			return true
		}
	}
	return false
}

// extractProviderMessage pulls the message out of the OpenAI-style error body
// without dumping the whole payload, which can echo the request.
func extractProviderMessage(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	var envelope struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	if envelope.Error.Message != "" {
		return envelope.Error.Message
	}
	return envelope.Message
}

// isSeedRejection reports whether the 400 text blames the seed field, which
// is the only case where a seed-less retry is defined.
func isSeedRejection(err error) bool {
	pe := asProvidersError(err)
	if pe == nil || pe.Status != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(pe.Msg), "seed")
}

// isBaseTaskTypeDemand reports whether the 400 text is the checkpoint
// rejecting the server-default task for this deployment — the exact case
// where a task_type:"Base" retry is defined.
func isBaseTaskTypeDemand(err error) bool {
	pe := asProvidersError(err)
	if pe == nil || pe.Status != http.StatusBadRequest {
		return false
	}
	msg := strings.ToLower(pe.Msg)
	return strings.Contains(msg, "does not support task_type") ||
		(strings.Contains(msg, "task_type") && strings.Contains(msg, "use task_type"))
}
