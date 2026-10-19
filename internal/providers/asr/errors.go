package asr

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers"
)

// ProbeWAV is the capability probe's audio: a 0.25 s, 440 Hz, 16 kHz mono
// PCM16 tone. It is a content probe, not a health ping — the endpoint must
// actually transcribe bytes (docs/spec/13 "no generic success on an unprobed
// service"), and a tone is guaranteed non-silent, non-clipped and small
// enough that the probe stays well inside the role's per-request budget.
func ProbeWAV() []byte {
	const rate = 16000
	const secs = 0.25
	n := int(rate * secs)
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(math.MaxInt16 / 8 * math.Sin(2*math.Pi*440*float64(i)/rate))
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	out := make([]byte, 44+len(data))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(data)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:], 1) // mono
	binary.LittleEndian.PutUint32(out[24:], rate)
	binary.LittleEndian.PutUint32(out[28:], rate*2)
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:], uint32(len(data)))
	copy(out[44:], data)
	return out
}

// Probe is the capability probe: one transcription of ProbeWAV through the
// client's real request path. The recognised text is asserted non-empty by
// Transcribe itself (an empty 200 is a decode error); the probe therefore
// proves the route end to end — multipart accepted, model served, JSON
// answered. The doctor uses it for the speech_asr row (docs/spec/13:
// "ASR transcript" is a named preflight check).
func (c *Client) Probe(ctx context.Context) (TranscribeResult, error) {
	return c.Transcribe(ctx, TranscribeRequest{WAV: ProbeWAV()})
}

// transportError classifies a transport-level failure, mirroring the OpenAI
// and TTS adapters: caller cancellation vs deadline vs unreachable endpoint,
// with the context error preserved for errors.Is.
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
		e = providers.NewError(providers.KindCancelled, status, "asr: request cancelled by caller")
	case errors.Is(err, context.DeadlineExceeded):
		e = providers.NewError(providers.KindTimeout, status, "asr: request deadline exceeded")
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
			e = providers.NewError(providers.KindUnavailable, status, "asr: transport failure")
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, providers.ErrRedirectRefused) {
		e.Err = err
	}
	e.Model = c.Model
	e.RequestID = reqID
	// A refused redirect is a configuration fact, not a transient one.
	e.Retryable = e.Kind.RetryableByDefault() && !errors.Is(err, providers.ErrRedirectRefused)
	return e
}

// httpError maps a non-2xx response onto the taxonomy. The provider message
// is redacted (token, endpoints) and truncated by providers.NewError.
func (c *Client) httpError(status int, body []byte, reqID string) *providers.Error {
	providerMsg := extractProviderMessage(body)
	kind := kindForStatus(status, providerMsg)
	msg := fmt.Sprintf("asr: %s", http.StatusText(status))
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

// kindForStatus is the shared mapping with the docs/spec/06 additions: a 404
// is a route the deployment does not serve, and a 400 naming an unsupported
// field (response_format, model) is unsupported_capability.
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
		"unknown model", "invalid model", "not found", "response_format", "transcription"} {
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
