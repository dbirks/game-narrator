package providers

import (
	"fmt"
	"os"
	"strings"
)

// Kind classifies a provider failure so routing policy can decide between a
// bounded retry, a fallback role, a degraded mode, or a hard stop
// (docs/spec/06 "Failure taxonomy", docs/spec/13).
type Kind string

// The taxonomy from docs/spec/06. Only the kinds an OpenAI-compatible chat
// adapter can produce are mapped in this milestone; the others are declared so
// adapters implemented later (System One, TTS, ASR) and the router switch on
// one vocabulary.
const (
	KindTimeout               Kind = "timeout"
	KindUnavailable           Kind = "unavailable"
	KindRateLimited           Kind = "rate_limited"
	KindBadSchema             Kind = "bad_schema"
	KindAuth                  Kind = "auth"
	KindPayloadTooLarge       Kind = "payload_too_large"
	KindUnsupportedCapability Kind = "unsupported_capability"
	KindWrongCheckpoint       Kind = "wrong_checkpoint"
	KindVoiceMissing          Kind = "voice_missing"
	KindReplicaMismatch       Kind = "replica_mismatch"
	KindDecodeError           Kind = "decode_error"
	KindAudioInvalid          Kind = "audio_invalid"
	KindContentRefused        Kind = "content_refused"
	KindCancelled             Kind = "cancelled"
	KindStaleResult           Kind = "stale_result"
)

// ValidKind reports whether k is a taxonomy member.
func (k Kind) Valid() bool {
	switch k {
	case KindTimeout, KindUnavailable, KindRateLimited, KindBadSchema, KindAuth,
		KindPayloadTooLarge, KindUnsupportedCapability, KindWrongCheckpoint,
		KindVoiceMissing, KindReplicaMismatch, KindDecodeError, KindAudioInvalid,
		KindContentRefused, KindCancelled, KindStaleResult:
		return true
	default:
		return false
	}
}

// RetryableByDefault records whether the kind is transient in general. An
// Error carries its own Retryable flag because a gateway can make an
// exception (for example a 429 that carries no retry hint and must not be
// hammered).
func (k Kind) RetryableByDefault() bool {
	switch k {
	case KindTimeout, KindUnavailable, KindRateLimited, KindReplicaMismatch, KindVoiceMissing:
		return true
	default:
		return false
	}
}

// Error is the provider error every adapter returns. Msg is a short,
// human-readable description that has been checked for credentials: it never
// contains a token and never contains a full request body (AGENTS.md rule 5).
type Error struct {
	// Kind is the taxonomy classification.
	Kind Kind
	// Status is the HTTP status, or 0 when the failure happened before or
	// instead of an HTTP response (client-side rejection, transport error).
	Status int
	// Retryable tells the router this exact failure may be re-attempted while
	// the event is still current.
	Retryable bool
	// Msg describes the failure without credentials or payload dumps.
	Msg string
	// Err is the underlying cause, if any, for errors.Is/As inspection.
	Err error
	// RequestID correlates the failure with provider-side logs when the
	// gateway exposed one.
	RequestID string
	// Role and Model carry provenance when the adapter knows them.
	Role  Role
	Model string
}

// Error renders the classification, status, provider message and cause.
func (e *Error) Error() string {
	var b strings.Builder
	kind := e.Kind
	if kind == "" {
		kind = KindUnavailable
	}
	fmt.Fprintf(&b, "%s", kind)
	if e.Status != 0 {
		fmt.Fprintf(&b, " (http %d)", e.Status)
	}
	if e.Model != "" {
		fmt.Fprintf(&b, " model=%s", e.Model)
	}
	if e.RequestID != "" {
		fmt.Fprintf(&b, " request_id=%s", e.RequestID)
	}
	if e.Msg != "" {
		fmt.Fprintf(&b, ": %s", e.Msg)
	}
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	return b.String()
}

// Unwrap exposes the transport/decode cause.
func (e *Error) Unwrap() error { return e.Err }

// Is treats errors with the same classification as equivalent, so callers can
// write errors.Is(err, &providers.Error{Kind: providers.KindRateLimited}).
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Kind == e.Kind
}

// NewError builds a classified error. Unknown kinds degrade to
// KindUnavailable rather than silently disappearing, and the message is
// truncated so a provider echo can never smuggle a whole prompt or credential
// into a log line.
func NewError(kind Kind, status int, msg string) *Error {
	if !kind.Valid() {
		msg = fmt.Sprintf("unknown failure kind %q in message %q", string(kind), Redact(msg, ""))
		kind = KindUnavailable
	}
	return &Error{Kind: kind, Status: status, Retryable: kind.RetryableByDefault(), Msg: Redact(msg, "")}
}

// Redact removes every occurrence of a secret and caps length. Adapters call
// it on anything a provider echoes back before putting it in an Error.
func Redact(msg, secret string) string {
	msg = strings.TrimSpace(msg)
	if secret != "" {
		msg = strings.ReplaceAll(msg, secret, MaskValue)
	}
	// Bearer tokens sometimes appear in provider error text verbatim.
	if i := strings.Index(strings.ToLower(msg), "bearer "); i >= 0 {
		end := strings.IndexAny(msg[i:], " ,\n\t\"'")
		if end < 0 {
			msg = msg[:i] + "Bearer " + MaskValue
		} else {
			msg = msg[:i] + "Bearer " + MaskValue + msg[i+end:]
		}
	}
	const max = 400
	if len(msg) > max {
		msg = msg[:max] + "…(truncated)"
	}
	return msg
}

// envLookup is the default environment reader; tests inject their own.
var envLookup = os.LookupEnv
