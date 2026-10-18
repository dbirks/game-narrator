package systemone

import (
	"crypto/rand"
	"strings"
	"time"
)

// Client-generated correlation ids for the X-Request-Id request header.
//
// The collective gateway answers the /systemone route with no request-id
// response header (verified live 2026-10-18: the first live Dolphin E2E left
// decision.requested/decision.resolved with an empty request_id), so without
// a client-side id a decision request cannot be correlated with the
// provider-side logs at all — the evidence-bundle contract (AGENTS.md rule 3,
// docs/spec/07) needs a request id on every Clef call. The client therefore
// mints one ULID per request and sends it as X-Request-Id; when the provider
// returns no id of its own the client id is surfaced as the Decision's
// RequestID (RecordRequestID), so the same id the provider-side logs can be
// searched by is what the rows and UI record. This mirrors the TTS fix
// (internal/providers/tts, ADR-0050).
//
// Layout: the standard ULID one — 48 bits of big-endian millisecond
// timestamp plus 80 bits of randomness, Crockford base32, 26 characters.
// Self-contained rather than shared with games.NewID: the providers layer
// sits below the catalogue layers and must not import them, and a
// third-party ULID module is a dependency decision (ADR-0008), not an
// import. The 16-byte buffer is the full 128-bit value, most significant
// first; the leading zero byte is the padding the 48-bit timestamp leaves.

const requestIDAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// RequestIDSourceProvider and RequestIDSourceClient name the two origins a
// Decision's RequestID can have (RecordRequestID): the gateway's own
// request-id response header, or the X-Request-Id the client minted and
// sent.
const (
	RequestIDSourceProvider = "provider"
	RequestIDSourceClient   = "client"
)

// newRequestID mints a fresh 26-character Crockford base32 ULID.
// crypto/rand.Read is documented to never return an error on the platforms
// this project supports (internal/api's request ids make the same
// assumption); a predictable id would make the correlation key collide
// silently, so the failure is raised rather than papered over.
func newRequestID() string {
	var random [10]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("systemone: newRequestID: no system randomness: " + err.Error())
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
// 26 Crockford base32 characters (130 bits; the top 2 bits of the value are
// always zero, so the first character is in 0-7). Crockford base32 is
// order-preserving for digits before letters, so lexical order of two ids
// is temporal across millisecond boundaries, like games.NewID's ids.
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
// otherwise the client id that was sent as X-Request-Id. The source
// records which, so the evidence can distinguish provider-issued ids from
// client minted ones.
func RecordRequestID(providerID, clientID string) (id, source string) {
	if p := strings.TrimSpace(providerID); p != "" {
		return p, RequestIDSourceProvider
	}
	return clientID, RequestIDSourceClient
}
