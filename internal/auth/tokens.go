package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/store"
)

// Credential shapes (ADR-0016). Every one of them is 256 bits of CSPRNG output
// rendered in an encoding that survives its transport: base64url for values that
// travel in a Cookie header, base32 (RFC 4648 alphabet, no padding, no symbols
// that a shell will eat) for a value an operator pastes into a config file.
const (
	// credentialBytes is 32 bytes = 256 bits, the guessing floor for any
	// credential this package mints. A session id at this size needs no
	// server-side secrecy beyond unguessability, which is why storing its
	// SHA-256 is sufficient (docs/spec/14 "stolen old session cookie").
	credentialBytes = 32

	// apiTokenEntropyBytes is 160 bits. It is smaller than a session credential
	// deliberately: an API token is typed by humans, printed in terminals and
	// stored in CI secrets, and 160 bits is still far beyond any offline guess
	// once only its digest is stored (26^32-shaped keyspace for base32 over 20
	// bytes). Longer values raise the copy-error rate without raising the
	// guessing cost above "impossible".
	apiTokenEntropyBytes = 20

	// APITokenPrefix identifies a Game Narrator bearer token. The literal is a
	// security feature, not decoration: a secret that is recognisable at a glance
	// can be spotted in a screenshot, a shell history line or a leaked CI log,
	// and secret scanners key on it (docs/spec/14 "no raw key" covers ours).
	APITokenPrefix = "gn_"
)

// ErrInvalidCredential is returned when a presented value cannot be a
// credential this service issued — wrong prefix, wrong length, unencodable
// bytes. It is answered as "not authorised" (401), never as a parse error, so
// the endpoint does not describe its token format to a caller who is guessing.
var ErrInvalidCredential = errors.New("auth: credential is not one this service issued")

// newCredential returns n random bytes as unpadded base64url.
func newCredential(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: no system randomness for a credential: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// NewSessionToken mints the value that goes into the gn_session cookie. The
// caller stores only DigestSecret(token) (0006's sessions.id_hash).
func NewSessionToken() (string, error) { return newCredential(credentialBytes) }

// NewCSRFToken mints the value the SPA echoes in X-CSRF-Token. It is bound to
// one session, hashed at rest like the session id, and has no authority of its
// own — which is why it may live in a readable cookie (docs/spec/08 "State-
// changing calls require CSRF protection under cookie auth").
func NewCSRFToken() (string, error) { return newCredential(credentialBytes) }

// NewAPIToken mints a `gn_…` bearer token. The raw value is returned once and
// is never recoverable afterwards: 0006 stores only its digest, so losing it
// means issuing a new one, which is the intended operator experience for a
// credential that may be sitting in a CI secret store.
func NewAPIToken() (string, error) {
	buf := make([]byte, apiTokenEntropyBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("auth: no system randomness for an api token: %w", err)
	}
	// StdEncoding (A-Z2-7) rather than HexEncoding: hex would be 40 characters
	// for 20 bytes. No padding, because '=' inside a URL or a dotenv value is
	// quoting noise an operator should not have to think about.
	return APITokenPrefix + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// LooksLikeAPIToken reports whether a bearer value has this service's shape. It
// is a cheap pre-filter so an obviously foreign Authorization header is not run
// through SHA-256 and a database lookup; it is NOT an authorization decision.
func LooksLikeAPIToken(raw string) bool {
	if !strings.HasPrefix(raw, APITokenPrefix) {
		return false
	}
	body := raw[len(APITokenPrefix):]
	if len(body) == 0 || len(body) > 64 {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if (c >= 'A' && c <= 'Z') || (c >= '2' && c <= '7') || (c >= 'a' && c <= 'z') {
			continue
		}
		return false
	}
	return true
}

// DigestSecret is the SHA-256 hex digest of a high-entropy credential, i.e. the
// only form of it that may be persisted. It delegates to store.HashSecret so
// the writer (this package) and the reader (the repository's lookup) cannot
// drift apart on encoding — the classic way a token store becomes unable to
// resolve its own tokens.
func DigestSecret(raw string) string { return store.HashSecret(raw) }

// ConstantTimeEqualHex compares two hex digests without branching on their
// contents. CSRF verification uses it: the presented token is hashed first and
// the digests are then compared in constant time, so neither the token nor a
// prefix of it is ever handled by a variable-time string comparison.
func ConstantTimeEqualHex(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
