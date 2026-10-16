package auth

import (
	"strings"
	"testing"
	"time"
)

func TestNewSessionTokenIsUnpredictableAndCookieSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		raw, err := NewSessionToken()
		if err != nil {
			t.Fatalf("NewSessionToken: %v", err)
		}
		if seen[raw] {
			t.Fatalf("session token repeated: %s", raw)
		}
		seen[raw] = true
		// 32 bytes -> 43 base64url chars, and nothing a cookie header would eat.
		if len(raw) != 43 {
			t.Fatalf("token length = %d, want 43 (32 bytes of base64url)", len(raw))
		}
		if strings.ContainsAny(raw, "=+/ ") {
			t.Fatalf("token %q is not cookie-safe (would need quoting in a Set-Cookie or a dotenv)", raw)
		}
	}
}

func TestNewCSRFTokenIsSessionShapedAndIndependent(t *testing.T) {
	a, err := NewCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two CSRF tokens were identical")
	}
	if s, err := NewSessionToken(); err != nil || len(s) != len(a) {
		t.Fatalf("csrf token is %d chars, session %d chars; both should be 256-bit credentials", len(a), len(s))
	}
}

func TestNewAPITokenFormat(t *testing.T) {
	raw, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, APITokenPrefix) {
		t.Fatalf("token %q does not start with %q", raw, APITokenPrefix)
	}
	if !LooksLikeAPIToken(raw) {
		t.Fatalf("LooksLikeAPIToken rejected our own token %q", raw)
	}
	if len(raw) > 64 {
		t.Fatalf("token is %d chars; it must stay pasteable into a config file", len(raw))
	}
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		other, err := NewAPIToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[other] {
			t.Fatalf("api token repeated: %s", other)
		}
		seen[other] = true
	}
}

func TestLooksLikeAPITokenRejectsForeignShapes(t *testing.T) {
	for _, raw := range []string{
		"",
		"gn_",
		"sk_live_51Hxxxxxxxx",            // somebody else's secret format
		"gn_not-base32-chars-!!",         // punctuation that base32 forbids
		"gn_" + strings.Repeat("A", 200), // absurd length: no lookup budget for it
		"GN_UPPERCASE_PREFIX",
		" gn_ABCD",
	} {
		if LooksLikeAPIToken(raw) {
			t.Errorf("LooksLikeAPIToken accepted %q", trunc(raw, 32))
		}
	}
	// Lowercase body is accepted: base32 is uppercase, but an operator pasting a
	// token that a shell or a config file lowercased should be *looked up* (and
	// then not found, which is a different and honest answer than refusing to
	// look at all).
	raw, err := NewAPIToken()
	if err != nil {
		t.Fatal(err)
	}
	if !LooksLikeAPIToken(strings.ToLower(raw)) {
		t.Errorf("LooksLikeAPIToken rejected the lowercase form of %q", raw)
	}
}

func TestDigestSecretIsStableAndNotTheInput(t *testing.T) {
	raw := "gn_TESTTOKENDIGEST"
	a, b := DigestSecret(raw), DigestSecret(raw)
	if a != b {
		t.Fatal("the same credential digested differently; lookups would miss")
	}
	if a == raw || strings.Contains(a, raw) {
		t.Fatalf("digest %q exposes the credential", a)
	}
	if len(a) != 64 {
		t.Fatalf("digest is %d chars, want 64 hex (store.checkDigest enforces it)", len(a))
	}
	if other := DigestSecret(raw + "x"); other == a {
		t.Fatal("two different credentials collided")
	}
}

func TestLoginLimiterRefillsAndCaps(t *testing.T) {
	now := time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)
	l := NewLimiter(3, 30*time.Second, func() time.Time { return now })

	for i := 0; i < 3; i++ {
		if ok, wait := l.Allow("ip:192.0.2.1"); !ok || wait != 0 {
			t.Fatalf("attempt %d was refused with %s remaining", i+1, wait)
		}
	}
	ok, wait := l.Allow("ip:192.0.2.1")
	if ok {
		t.Fatal("the fourth attempt was allowed against a capacity of three")
	}
	if wait <= 0 {
		t.Fatalf("a refusal must name a wait, got %s", wait)
	}
	// One token per 10 s (3 per 30 s).
	now = now.Add(11 * time.Second)
	if ok, _ := l.Allow("ip:192.0.2.1"); !ok {
		t.Fatal("the bucket had not refilled a token after 11 s of a 10 s refill interval")
	}
	if ok, _ := l.Allow("ip:192.0.2.1"); ok {
		t.Fatal("the bucket gave two tokens for one refill interval")
	}
	// A long idle must not bank more than the capacity.
	now = now.Add(1 * time.Hour)
	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow("ip:192.0.2.1"); !ok {
			t.Fatalf("refill after idle failed at attempt %d", i+1)
		}
	}
	if ok, _ := l.Allow("ip:192.0.2.1"); ok {
		t.Fatal("an hour of idle banked more than the burst capacity: buckets must not accumulate indefinitely")
	}
}

func TestLoginLimiterKeysAreIndependent(t *testing.T) {
	l := NewLimiter(2, time.Minute, nil)
	for i := 0; i < 2; i++ {
		l.Allow("ip:192.0.2.1")
	}
	if ok, _ := l.Allow("ip:192.0.2.1"); ok {
		t.Fatal("exhausted key was allowed")
	}
	if ok, _ := l.Allow("ip:192.0.2.2"); !ok {
		t.Fatal("a different address shares somebody else's lockout — this is what keeps one LAN host from locking out the appliance")
	}
	l.Reset("ip:192.0.2.1")
	if ok, _ := l.Allow("ip:192.0.2.1"); !ok {
		t.Fatal("Reset did not clear the key: a user who finally types the right password must not stay throttled")
	}
	if got := l.Keys(); got != 2 {
		t.Fatalf("Keys() = %d, want 2", got)
	}
}

func TestLimiterRefusesNewKeysWhenFull(t *testing.T) {
	// maxLimiterKeys is the memory bound (docs/spec/14 "Bound tool budget").
	// The important half is which way it fails: a flood of fresh keys must get a
	// refusal, not an unmetered pass.
	now := time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)
	l := NewLimiter(1, time.Hour, func() time.Time { return now })
	for i := 0; i < maxLimiterKeys; i++ {
		l.Allow("ip:198.51.100." + string(rune('A'+i%26)) + itoa(i))
	}
	if got := l.Keys(); got < maxLimiterKeys-26 {
		t.Fatalf("Keys() = %d, expected the table to be near its bound (%d)", got, maxLimiterKeys)
	}
	if ok, wait := l.Allow("ip:203.0.113.99-new"); ok {
		t.Fatal("a brand-new key was allowed past the bound: an attacker who exhausts the table would be rate-limited by nothing")
	} else if wait <= 0 {
		t.Fatalf("refusal carried no Retry-After: %s", wait)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
