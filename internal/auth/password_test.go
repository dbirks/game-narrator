package auth

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

// testPassword is 25 characters, above the 12-character floor, and chosen so a
// test log line containing it is obvious (docs/spec/13 log hygiene: a secret in
// a fixture must be one a reader can spot).
const testPassword = "correct horse battery staple"

func TestPasswordCostDefaultIsOWASP2023Floor(t *testing.T) {
	// docs/spec/14 "Authorization: secure secrets store"; ADR-0016 records why
	// PBKDF2 (no x/crypto for argon2) and why this number.
	if PBKDF2Iterations != 600_000 {
		t.Fatalf("PBKDF2Iterations = %d, want 600000", PBKDF2Iterations)
	}
	if MinPasswordLen != 12 || MaxPasswordLen != 256 {
		t.Fatalf("length policy = [%d,%d], want [12,256]", MinPasswordLen, MaxPasswordLen)
	}
}

func TestHashPasswordEnvelopeShape(t *testing.T) {
	encoded, err := HashPassword(testPassword)
	if err != nil {
		t.Fatalf("HashPassword: %v", err)
	}
	// pbkdf2-sha256$<iter>$<b64salt>$<b64key>
	fields := strings.Split(encoded, "$")
	if len(fields) != 4 {
		t.Fatalf("envelope %q has %d fields, want 4", encoded, len(fields))
	}
	if fields[0] != "pbkdf2-sha256" {
		t.Fatalf("algorithm = %q", fields[0])
	}
	if fields[1] != "600000" {
		t.Fatalf("iterations = %q, want the current policy 600000", fields[1])
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[2])
	if err != nil {
		t.Fatalf("salt is not unpadded base64: %v", err)
	}
	if len(salt) != 16 {
		t.Fatalf("salt is %d bytes, want 16", len(salt))
	}
	key, err := base64.RawStdEncoding.DecodeString(fields[3])
	if err != nil {
		t.Fatalf("key is not unpadded base64: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("derived key is %d bytes, want 32", len(key))
	}
	if strings.Contains(encoded, testPassword) {
		t.Fatal("the envelope contains the password")
	}
}

func TestVerifyPasswordRoundTripAndFailure(t *testing.T) {
	// Cheap cost: the shape and comparison are what is under test here; the
	// production cost is asserted above and exercised once in
	// TestHashPasswordEnvelopeShape.
	encoded, err := hashPasswordWith(testPassword, 1000)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	ok, err := VerifyPassword(testPassword, encoded)
	if err != nil || !ok {
		t.Fatalf("verify of the same password = %v, %v", ok, err)
	}
	for _, wrong := range []string{
		"",
		"correct horse battery stapl",           // one character short
		"Correct horse battery staple",          // one character different
		"correct horse battery staple!",         // one character long
		testPassword + strings.Repeat("x", 300), // over the max length
	} {
		ok, err := VerifyPassword(wrong, encoded)
		if err != nil {
			t.Fatalf("verify(%q) returned an error, want a false answer: %v", trunc(wrong, 20), err)
		}
		if ok {
			t.Fatalf("verify accepted a different password %q", trunc(wrong, 30))
		}
	}
}

func TestVerifyPasswordRejectsTamperedEnvelopes(t *testing.T) {
	encoded, err := hashPasswordWith(testPassword, 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, tampered := range []string{
		"pbkdf2-sha256$1000$" + strings.Split(encoded, "$")[2] + "$AAAAAAAAAAAAAAAAAAAAAAAAAAA",
		strings.Replace(encoded, "pbkdf2-sha256", "md5", 1),
		"not-an-envelope",
		"pbkdf2-sha256$1$aaa$bbb",                          // below the iteration floor
		"pbkdf2-sha256$999999999999$aaa$bbb",               // above the ceiling
		strings.Join(strings.Split(encoded, "$")[:3], "$"), // truncated
	} {
		ok, err := VerifyPassword(testPassword, tampered)
		if ok {
			t.Fatalf("verify accepted tampered envelope %q", trunc(tampered, 40))
		}
		if err == nil {
			t.Fatalf("tampered envelope %q was reported as a wrong password, not as malformed", trunc(tampered, 40))
		}
	}
}

func TestNeedsUpgradeFollowsTheConfiguredCost(t *testing.T) {
	weak, err := hashPasswordWith(testPassword, 1000)
	if err != nil {
		t.Fatal(err)
	}
	strong, err := hashPasswordWith(testPassword, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if !NeedsUpgradeTo(weak, 5000) {
		t.Fatal("a 1000-iteration hash was not flagged for upgrade to 5000")
	}
	if NeedsUpgradeTo(strong, 5000) {
		t.Fatal("a hash at the current cost was flagged for upgrade")
	}
	if NeedsUpgradeTo(strong, 1000) {
		t.Fatal("an upgrade must never go backwards: lowering the cost is not an upgrade")
	}
	// A malformed envelope is reported as needing an upgrade, and ParamsFor is
	// what distinguishes it from a merely weak one.
	if !NeedsUpgrade("garbage") {
		t.Fatal("a malformed envelope must be reported as unusable")
	}
	if _, err := ParamsFor("garbage"); err == nil {
		t.Fatal("ParamsFor accepted a malformed envelope")
	}
	// And the default-cost view agrees that anything below 600k is upgradable —
	// which is the property that makes a future increase apply itself on login
	// instead of needing a migration over rows nobody can verify.
	if !NeedsUpgrade(weak) {
		t.Fatal("NeedsUpgrade did not flag a 1000-iteration hash against the 600000 default")
	}
}

func TestValidatePasswordLength(t *testing.T) {
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLen-1)); err == nil {
		t.Fatal("accepted a password below the floor")
	}
	if err := ValidatePassword(strings.Repeat("a", MinPasswordLen)); err != nil {
		t.Fatalf("rejected a password at the floor: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLen)); err != nil {
		t.Fatalf("rejected a password at the ceiling: %v", err)
	}
	if err := ValidatePassword(strings.Repeat("a", MaxPasswordLen+1)); err == nil {
		t.Fatal("accepted a password above the ceiling — the KDF cost would be attacker-controlled")
	}
	if err := ValidatePassword(""); err == nil {
		t.Fatal("accepted an empty password")
	}
}

func TestHashPasswordDoesNotTrimTheCredential(t *testing.T) {
	// Leading/trailing spaces are part of the password. Stripping them would
	// silently change what "the same password" means between two clients.
	withSpace := testPassword + " "
	a, err := hashPasswordWith(withSpace, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hashPasswordWith(testPassword, 1000); err != nil {
		t.Fatal(err)
	}
	if ok, _ := VerifyPassword(testPassword, a); ok {
		t.Fatal("a password with a trailing space verified against the same password without one")
	}
}

func TestDummyVerifyActuallyRunsTheKDF(t *testing.T) {
	// The point of this function is that an unknown account costs the same as a
	// known one. If it ever short-circuits, login for a nonexistent username
	// becomes measurably faster than for a real one — a user-enumeration oracle
	// on the one endpoint docs/spec/14 assumes a LAN attacker can reach. A
	// coarse lower bound is the assertion that will not turn into a flaky
	// benchmark: 20k iterations is ~8 ms on this host and >1 ms anywhere.
	start := time.Now()
	dummyVerifyPassword(testPassword, 20_000)
	if elapsed := time.Since(start); elapsed < time.Millisecond {
		t.Fatalf("dummy verify of 20000 iterations took %s: it is not deriving a key, so unknown-account logins are faster than known-account ones", elapsed)
	}
}

// trunc keeps a secret-shaped value short enough to print in a failure message
// without turning the test log into a copy of the credential under test.
func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
