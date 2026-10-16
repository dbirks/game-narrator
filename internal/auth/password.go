package auth

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Password hashing parameters (docs/spec/14 "Authorization": secure secrets
// store; ADR-0016 records why PBKDF2 and not Argon2id).
//
// PBKDF2-HMAC-SHA256 at 600,000 iterations is OWASP's 2023+ floor for PBKDF2
// when the alternative on the same hardware would be Argon2id at a memory-hard
// setting. It is the weaker of the two families by design: this project's only
// SQLite driver is pure-Go, golang.org/x/crypto is not a dependency, and AGENTS.md
// "No new modules" plus the supply-chain section of docs/spec/14 make pulling one
// in for a KDF a decision to take deliberately (ADR-0016 lists it as the first
// follow-up), not an accident of this task. The envelope is self-describing, so
// moving to argon2id later is a new prefix and a rehash-on-login, not a rewrite
// of every row: see ParamsFor and NeedsUpgrade.
const (
	// PBKDF2Iterations is the current work factor written into new hashes.
	PBKDF2Iterations = 600_000
	// passwordSaltBytes is 16 bytes of CSPRNG: long enough that two users with
	// the same password cannot share a hash, and rainbow tables cannot be
	// pre-computed for the whole table at once.
	passwordSaltBytes = 16
	// passwordKeyBytes is SHA-256's block output. Deriving more than the hash
	// function's natural output buys no strength; deriving less loses it.
	passwordKeyBytes = 32

	// MinPasswordLen is the length floor. docs/spec/14's threat model assumes a
	// LAN-resident attacker who can reach the login route and will try
	// dictionary passwords, so the floor is what makes 600k iterations
	// meaningful: at 12 characters an offline guess against a stolen hash is
	// already expensive, at 6 it is not.
	MinPasswordLen = 12
	// MaxPasswordLen bounds the KDF input. Without a cap, a caller can make
	// login cost (input size × 600k iterations) and turn the appliance's single
	// CPU budget into a self-inflicted denial of service (docs/spec/14 "Abuse
	// and resource controls").
	MaxPasswordLen = 256

	// minStoredIterations/maxStoredIterations bound what a *stored* hash may ask
	// verification to cost. The count is read from the row, so a database that
	// was edited (or restored from a hostile backup) could otherwise demand any
	// amount of work per login. These are not the policy — PBKDF2Iterations is —
	// they are the ceiling on what one row can make the server do.
	minStoredIterations = 1_000
	maxStoredIterations = 5_000_000
)

// passwordAlgo is the envelope prefix this package writes.
const passwordAlgo = "pbkdf2-sha256"

// ErrWeakPassword is a password policy failure — the caller's fault, reported
// as 400 with the rule quoted.
var ErrWeakPassword = errors.New("auth: password does not meet the length policy")

// ErrMalformedPasswordHash means a stored hash is not an envelope this package
// can interpret. It is a server-side/data defect, never a wrong-password
// answer: conflating the two would tell a user their password is wrong when the
// row is corrupt, and then they would change it, destroying the evidence.
var ErrMalformedPasswordHash = errors.New("auth: stored password hash is malformed")

// ValidatePassword enforces the length policy. It intentionally checks length
// and nothing else: composition rules ("must contain a symbol") measurably
// reduce entropy in practice and push users to write the password on a sticky
// note, while length is what PBKDF2 cost actually multiplies.
func ValidatePassword(pw string) error {
	switch {
	case pw == "":
		return fmt.Errorf("%w: a password is required", ErrWeakPassword)
	case len(pw) < MinPasswordLen:
		return fmt.Errorf("%w: at least %d characters are required, got %d",
			ErrWeakPassword, MinPasswordLen, len(pw))
	case len(pw) > MaxPasswordLen:
		return fmt.Errorf("%w: at most %d characters are accepted, got %d",
			ErrWeakPassword, MaxPasswordLen, len(pw))
	}
	return nil
}

// HashPassword derives and encodes a self-describing PBKDF2 envelope:
//
//	pbkdf2-sha256$<iterations>$<base64(salt)>$<base64(derived key)>
//
// The iteration count rides along so a later increase can be applied on the
// next successful login instead of a migration that cannot verify anything
// (the whole point of a password hash is that the server cannot read it).
func HashPassword(password string) (string, error) {
	return hashPasswordWith(password, PBKDF2Iterations)
}

func hashPasswordWith(password string, iterations int) (string, error) {
	if err := ValidatePassword(password); err != nil {
		return "", err
	}
	if iterations < minStoredIterations || iterations > maxStoredIterations {
		return "", fmt.Errorf("auth: refusing to write an envelope with %d iterations", iterations)
	}
	salt := make([]byte, passwordSaltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("auth: password salt: %w", err)
	}
	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, passwordKeyBytes)
	if err != nil {
		return "", fmt.Errorf("auth: pbkdf2: %w", err)
	}
	return strings.Join([]string{
		passwordAlgo,
		strconv.Itoa(iterations),
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	}, "$"), nil
}

// PasswordParams is what an envelope declares about its own cost.
type PasswordParams struct {
	Algorithm  string
	Iterations int
	KeyBytes   int
}

// ParamsFor reads an envelope's parameters without doing any key derivation. It
// exists so the upgrade decision (and the developer UI's "this account's hash
// predates the current work factor" line) costs nothing.
func ParamsFor(encoded string) (PasswordParams, error) {
	fields := strings.Split(encoded, "$")
	if len(fields) != 4 {
		return PasswordParams{}, fmt.Errorf("%w: expected 4 envelope fields, got %d",
			ErrMalformedPasswordHash, len(fields))
	}
	if fields[0] != passwordAlgo {
		return PasswordParams{}, fmt.Errorf("%w: algorithm %q", ErrMalformedPasswordHash, fields[0])
	}
	iter, err := strconv.Atoi(fields[1])
	if err != nil {
		return PasswordParams{}, fmt.Errorf("%w: iterations %q: %w", ErrMalformedPasswordHash, fields[1], err)
	}
	salt, err := base64.RawStdEncoding.DecodeString(fields[2])
	if err != nil {
		return PasswordParams{}, fmt.Errorf("%w: salt is not base64: %w", ErrMalformedPasswordHash, err)
	}
	key, err := base64.RawStdEncoding.DecodeString(fields[3])
	if err != nil {
		return PasswordParams{}, fmt.Errorf("%w: key is not base64: %w", ErrMalformedPasswordHash, err)
	}
	if len(salt) != passwordSaltBytes {
		return PasswordParams{}, fmt.Errorf("%w: salt is %d bytes, want %d",
			ErrMalformedPasswordHash, len(salt), passwordSaltBytes)
	}
	if len(key) != passwordKeyBytes {
		return PasswordParams{}, fmt.Errorf("%w: key is %d bytes, want %d",
			ErrMalformedPasswordHash, len(key), passwordKeyBytes)
	}
	if iter < minStoredIterations || iter > maxStoredIterations {
		return PasswordParams{}, fmt.Errorf("%w: %d iterations is outside [%d,%d]",
			ErrMalformedPasswordHash, iter, minStoredIterations, maxStoredIterations)
	}
	return PasswordParams{Algorithm: fields[0], Iterations: iter, KeyBytes: len(key)}, nil
}

// NeedsUpgrade reports whether a stored envelope is weaker than the default
// policy and should be re-derived on the next successful login.
//
// NeedsUpgradeTo is the same question against an explicitly chosen cost. The
// service answers it with its own configured cost (WithPasswordCost), which
// exists for two reasons: the OWASP number is sized for a modern x86 core and
// docs/spec/10's target is a 4 GB ARM appliance, where 600k iterations is
// hundreds of milliseconds on the login path; and a parameter upgrade — the
// property that makes self-describing envelopes worth having — cannot be
// tested at all without a second cost to upgrade toward.
//
// A malformed envelope also answers "upgrade": the row cannot be verified
// against anything, and the only recovery is a deliberate password reset, which
// the caller reaches by treating this as true and re-hashing after a password
// check that will necessarily fail. It is reported separately from a wrong
// password so an operator sees "this account's credential store is corrupt"
// rather than "somebody is guessing passwords".
func NeedsUpgrade(encoded string) bool { return NeedsUpgradeTo(encoded, PBKDF2Iterations) }

// NeedsUpgradeTo reports whether encoded is weaker than the given cost. A
// malformed envelope answers true: see NeedsUpgrade.
func NeedsUpgradeTo(encoded string, iterations int) bool {
	p, err := ParamsFor(encoded)
	if err != nil {
		return true
	}
	return p.Iterations < iterations
}

// VerifyPassword derives the candidate's key and compares it to the stored
// envelope in constant time.
//
// The comparison is over digests, so it does not leak how many leading bytes of
// the derived key matched; the *cost* is deliberately constant given the stored
// iteration count (verification always runs at the strength the row declares),
// which is what makes a timing difference between "no such user" and "wrong
// password" unobservable when the caller verifies against a dummy hash — see
// Service.Login.
func VerifyPassword(password, encoded string) (bool, error) {
	p, err := ParamsFor(encoded)
	if err != nil {
		return false, err
	}
	fields := strings.Split(encoded, "$")
	salt, err := base64.RawStdEncoding.DecodeString(fields[2])
	if err != nil {
		return false, fmt.Errorf("%w: salt: %w", ErrMalformedPasswordHash, err)
	}
	want, err := base64.RawStdEncoding.DecodeString(fields[3])
	if err != nil {
		return false, fmt.Errorf("%w: key: %w", ErrMalformedPasswordHash, err)
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, p.Iterations, p.KeyBytes)
	if err != nil {
		return false, fmt.Errorf("auth: pbkdf2 verify: %w", err)
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

// dummyVerifyPassword runs the KDF at the current cost against a fixed salt, so
// a login for an unknown username costs the same as one for a known account.
// Without it the endpoint answers "no such user" in microseconds and the rest
// in hundreds of milliseconds, which is a user-enumeration oracle on a box that
// docs/spec/14 assumes can be reached from a LAN (the enumeration is the
// precondition for a targeted guess).
func dummyVerifyPassword(password string, iterations int) {
	if iterations < minStoredIterations {
		iterations = minStoredIterations
	}
	salt := make([]byte, passwordSaltBytes)
	copy(salt, "gn-dummy-salt!") // fixed 16 bytes: this salt guards nothing
	_, _ = pbkdf2.Key(sha256.New, password, salt, iterations, passwordKeyBytes)
}
