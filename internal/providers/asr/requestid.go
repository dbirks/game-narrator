// Package asr is the typed client for the speech_asr role: an
// OpenAI-compatible POST /v1/audio/transcriptions endpoint (docs/spec/06
// "Provider registry" — the qwen3-asr checkpoint the collective gateway
// serves). The contract is the wire the deployment actually answers, verified
// 2026-10-18: multipart fields model, file (WAV), response_format (json or
// verbose_json), language. The endpoint returns no request-id header, so the
// client mints one ULID per request and records its source, exactly like the
// TTS client (internal/providers/tts/requestid.go): the same id the
// provider-side logs can be searched by is what the QA rows and UI record
// (AGENTS.md rule 3, evidence-bundle contract).
package asr

import (
	"crypto/rand"
	"strings"
	"time"
)

// Request id sources (the same two states as the TTS client).
const (
	// RequestIDSourceClient: the id the client minted and sent as X-Request-Id;
	// the provider answered without its own id.
	RequestIDSourceClient = "client"
	// RequestIDSourceProvider: the id the provider returned in the
	// X-Request-Id response header; it wins verbatim.
	RequestIDSourceProvider = "provider"
)

const requestIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newRequestID mints a fresh 26-character Crockford base32 ULID: 48 bits of
// big-endian millisecond timestamp plus 80 bits of randomness. Self-contained
// rather than shared with games.NewID: the providers layer sits below the
// catalogue layers and must not import them, and a third-party ULID module is
// a dependency decision (ADR-0008), not an import.
func newRequestID() string {
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("asr: newRequestID: no system randomness: " + err.Error())
	}
	ms := uint64(time.Now().UTC().UnixMilli())
	if ms == 0 {
		// A zero timestamp would sort before every real id and read as "the
		// epoch"; refuse it at the one place that knows.
		ms = 1
	}
	var v [16]byte
	v[0] = byte(ms >> 40)
	v[1] = byte(ms >> 32)
	v[2] = byte(ms >> 24)
	v[3] = byte(ms >> 16)
	v[4] = byte(ms >> 8)
	v[5] = byte(ms)
	copy(v[6:], random[:])
	return encodeCrockford(v)
}

// encodeCrockford renders the 128 bits of src (most significant first) as
// 26 Crockford base32 characters. Crockford base32 is order-preserving for
// digits before letters, so lexical order of two ids is temporal across
// millisecond boundaries, like games.NewID's ids.
func encodeCrockford(src [16]byte) string {
	var out [26]byte
	for i := range out {
		var field byte
		for bit := 0; bit < 5; bit++ {
			pos := i*5 + bit
			if pos >= 128 {
				break
			}
			field <<= 1
			if src[pos/8]&(1<<uint(7-pos%8)) != 0 {
				field |= 1
			}
		}
		out[i] = requestIDAlphabet[field]
	}
	return string(out[:])
}

// validRequestID reports whether s is a well-formed correlation id in
// canonical spelling (26 Crockford characters, first character 0-7).
func validRequestID(s string) bool {
	if len(s) != 26 {
		return false
	}
	var val [256]int8
	for i := range val {
		val[i] = -1
	}
	for v, c := range requestIDAlphabet {
		val[c] = int8(v)
	}
	if val[s[0]] > 7 {
		return false
	}
	for i := 0; i < 26; i++ {
		if val[s[i]] < 0 {
			return false
		}
	}
	return true
}

// RecordRequestID resolves the effective request id for one completed
// request: the provider's id when it returned one (it wins, verbatim),
// otherwise the client id that was sent as X-Request-Id. The source records
// which, so the evidence can distinguish provider-issued ids from client
// minted ones.
func RecordRequestID(providerID, clientID string) (id, source string) {
	if p := strings.TrimSpace(providerID); p != "" {
		return p, RequestIDSourceProvider
	}
	return clientID, RequestIDSourceClient
}
