package store

// Auth persistence for migration 0006: users, server-side sessions, API tokens
// and the auth audit trail (docs/spec/08 "API version and user
// authentication", docs/spec/14 "Authorization"). ADR-0016 records the design.
//
// Three invariants are load-bearing here rather than in the callers:
//
//  1. No secret is ever readable back out of this package. Sessions and API
//     tokens are keyed by the SHA-256 of the value the client presents
//     (id_hash, token_hash, csrf_hash); the raw values exist only in the
//     caller's memory for the length of one request. There is deliberately no
//     method on AuthStore that returns a raw token, and no column that could
//     hold one — a CHECK on each digest column enforces the length, so a
//     hand-written row cannot smuggle a plaintext credential in as a "hash".
//  2. Revocation is a write, not a cache invalidation. RotatePassword and
//     SetUserDisabled update the user and revoke every session and token in one
//     BEGIN IMMEDIATE transaction (repo_tx.go), so docs/spec/14's "stolen old
//     session cookie invalidated" cannot land in a half state where the
//     password changed but the sessions survived.
//  3. Fail closed. "No such user", "revoked", "expired" are distinct answers so
//     the API can tell a client to re-authenticate (401) rather than that it
//     lacks a role (403); a read that errors is an error, never an empty user.
//
// Timestamps use stampMS (fixed-width UTC milliseconds), the form jobs_store.go
// uses: session and token expiry is compared with `<=` on TEXT, and
// mixed-offset RFC 3339 strings are not chronologically sortable
// (0003_jobs_events.sql explains why).

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// Roles are TEXT here because that is what the 0006 CHECK enum enforces; the
// ordered domain type and the grant table live in internal/auth, which owns
// what each role may do. The store refuses anything outside the enum so a
// caller cannot persist a role no policy will ever match — which would be a
// silently powerless (or, worse, silently unconstrained) principal.
const (
	RoleViewer    = "viewer"
	RoleCurator   = "curator"
	RoleOperator  = "operator"
	RoleDeveloper = "developer"
	RoleAdmin     = "admin"
)

// ValidAuthRole reports whether role is one of the 0006 enum values.
func ValidAuthRole(role string) bool {
	switch role {
	case RoleViewer, RoleCurator, RoleOperator, RoleDeveloper, RoleAdmin:
		return true
	}
	return false
}

// digestLength is the hex length of a SHA-256 digest, i.e. what every *_hash
// column in 0006 stores.
const digestLength = 64

// HashSecret returns the lowercase hex SHA-256 of a high-entropy credential (a
// session token, a CSRF token or an API token) — the only form this package
// ever persists for one. It lives here so the write path and the read path
// cannot disagree about the encoding: the likeliest way a token store becomes
// unusable is two places hex-encoding differently.
func HashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// User is a row of `users`. PasswordHash is the PBKDF2 envelope, never a
// password.
type User struct {
	ID                string
	Username          string
	PasswordHash      string
	Role              string
	Disabled          bool
	CreatedAt         time.Time
	PasswordChangedAt time.Time
}

// Session is a row of `sessions`. IDHash is the digest of the cookie token; the
// raw token cannot be recovered from this row.
type Session struct {
	IDHash     string
	UserID     string
	CSRFHash   string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	UserAgent  string
	RevokedAt  time.Time // zero = live
}

// Revoked reports whether the session was explicitly revoked.
func (s *Session) Revoked() bool { return !s.RevokedAt.IsZero() }

// APIToken is a row of `api_tokens`. TokenHash is the digest of the `gn_…`
// value; the raw token is only ever returned by the call that created it.
type APIToken struct {
	ID         string
	UserID     string
	Name       string
	TokenHash  string
	Role       string
	CreatedAt  time.Time
	LastUsedAt time.Time // zero = never used
	ExpiresAt  time.Time // zero = no expiry
	RevokedAt  time.Time // zero = live
}

// Live reports whether the token may still authenticate at now.
//
// A zero ExpiresAt means the column is NULL, i.e. "no expiry" — the meaning 0006
// documents and the CLI's default — and it must never be read as the epoch. This
// is the bug an unqualified `ExpiresAt.Before(now)` produces: a Go zero time is
// 1-01-01, which is before everything, so every non-expiring token on the
// appliance would be dead on arrival.
func (t *APIToken) Live(now time.Time) bool {
	if !t.RevokedAt.IsZero() {
		return false
	}
	return t.ExpiresAt.IsZero() || t.ExpiresAt.After(now)
}

// AuthAudit is a row of `auth_audit`.
type AuthAudit struct {
	OccurredAt time.Time
	UserID     string
	Event      string
	RemoteAddr string
	Detail     json.RawMessage
}

// AuthStore is the repository over the 0006 tables. Like the other
// repositories in this package it is a stateless view of a *Store and is safe
// for concurrent use.
type AuthStore struct {
	store *Store
}

// NewAuthStore attaches the auth repository to an open store.
func NewAuthStore(s *Store) *AuthStore { return &AuthStore{store: s} }

// ── typed errors ────────────────────────────────────────────────────────────

// ErrInvalidRole is a role outside the 0006 enum: the caller's fault (400),
// distinct from a constraint violation (409).
var ErrInvalidRole = errors.New("store: invalid auth role")

// ErrInvalidSecret means a digest-shaped column was handed something that is
// not a SHA-256 hex digest — including an empty string, which is what "we
// stored the raw token instead" looks like.
var ErrInvalidSecret = errors.New("store: invalid secret digest")

// ErrInvalidUsername is a username 0006's CHECK cannot accept.
var ErrInvalidUsername = errors.New("store: invalid username")

// ErrInvalidPasswordHash is a hash that is empty, absurdly long, or not the
// self-describing envelope internal/auth writes.
var ErrInvalidPasswordHash = errors.New("store: invalid password hash")

// ErrInvalidAuditEvent is an audit event name the schema rejects.
var ErrInvalidAuditEvent = errors.New("store: invalid audit event")

// ── users ───────────────────────────────────────────────────────────────────

// CreateUserParams is the input to CreateUser.
type CreateUserParams struct {
	Username     string
	PasswordHash string
	Role         string
	Now          time.Time
}

// CreateUser inserts one user. A duplicate username (case-insensitively) is
// ErrConflict, never a silent update: two principals with interchangeable names
// is exactly the confusion docs/spec/14's role ACL exists to prevent.
func (a *AuthStore) CreateUser(ctx context.Context, p CreateUserParams) (*User, error) {
	if !ValidAuthRole(p.Role) {
		return nil, fmt.Errorf("store: create user: role %q: %w", p.Role, ErrInvalidRole)
	}
	if err := checkUsername(p.Username); err != nil {
		return nil, err
	}
	if err := checkPasswordHash(p.PasswordHash); err != nil {
		return nil, err
	}
	now := p.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	u := &User{
		ID:                games.NewID(),
		Username:          p.Username,
		PasswordHash:      p.PasswordHash,
		Role:              p.Role,
		CreatedAt:         now,
		PasswordChangedAt: now,
	}
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, "insert user",
			`INSERT INTO users (id, username, password_hash, role, disabled, created_at, password_changed_at)
			 VALUES (?, ?, ?, ?, 0, ?, ?)`,
			u.ID, u.Username, u.PasswordHash, u.Role, stampMS(u.CreatedAt), stampMS(u.PasswordChangedAt))
		return err
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

const userColumns = `id, username, password_hash, role, disabled, created_at, password_changed_at`

// UserByID loads a user, or ErrNotFound.
func (a *AuthStore) UserByID(ctx context.Context, id string) (*User, error) {
	var out *User
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		u, err := scanUser(q.queryRow(ctx, "get user",
			"SELECT "+userColumns+" FROM users WHERE id = ?", id))
		out = u
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UserByUsername loads a user by case-insensitive username, or ErrNotFound.
// The login path treats "no such user" and "wrong password" identically at the
// wire (401, one problem type), so this returning ErrNotFound is not an oracle:
// the caller must not branch on it into a different response.
func (a *AuthStore) UserByUsername(ctx context.Context, username string) (*User, error) {
	var out *User
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		u, err := scanUser(q.queryRow(ctx, "get user by username",
			"SELECT "+userColumns+" FROM users WHERE username = ?", username))
		out = u
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListUsers returns every user in creation order. The projection deliberately
// still carries password_hash (callers hand it to the verifier); the API's DTO
// is what keeps it off the wire, and userDTO has no field for it — see
// internal/api/auth.go.
func (a *AuthStore) ListUsers(ctx context.Context) ([]*User, error) {
	var out []*User
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, "list users",
			"SELECT "+userColumns+" FROM users ORDER BY created_at, id")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			u, err := scanUserRows(rows)
			if err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HasRole reports whether at least one user holds role, disabled or not.
// First-run setup asks this about admin: a disabled admin still *exists*, so
// the setup endpoint stays closed and the operator must use `admin create` —
// the alternative would let a network caller resurrect setup by disabling the
// account it just created.
func (a *AuthStore) HasRole(ctx context.Context, role string) (bool, error) {
	if !ValidAuthRole(role) {
		return false, fmt.Errorf("store: has role: %q: %w", role, ErrInvalidRole)
	}
	var n int
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		return q.queryRow(ctx, "count role",
			`SELECT count(*) FROM users WHERE role = ?`, role).Scan(&n)
	})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// RotatePasswordParams is the input to RotatePassword.
type RotatePasswordParams struct {
	UserID          string
	PasswordHash    string
	Now             time.Time
	RevokeSessions  bool
	RevokeAPITokens bool
}

// RevocationReport counts what an action voided, so "we logged out the stolen
// cookie" is evidence rather than a claim. It is returned by the API and stored
// in the audit detail.
type RevocationReport struct {
	Sessions int
	Tokens   int
}

// RotatePassword writes the new hash, moves password_changed_at, and revokes
// sessions/tokens in the same transaction.
func (a *AuthStore) RotatePassword(ctx context.Context, p RotatePasswordParams) (RevocationReport, error) {
	var out RevocationReport
	if err := checkPasswordHash(p.PasswordHash); err != nil {
		return out, err
	}
	if strings.TrimSpace(p.UserID) == "" {
		return out, errors.New("store: rotate password: user id is required")
	}
	now := p.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, "rotate password",
			`UPDATE users SET password_hash = ?, password_changed_at = ? WHERE id = ?`,
			p.PasswordHash, stampMS(now), p.UserID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return notFound("rotate password", "user")
		}
		if p.RevokeSessions {
			if out.Sessions, err = revokeSessionsHere(ctx, tx, p.UserID, now); err != nil {
				return err
			}
		}
		if p.RevokeAPITokens {
			if out.Tokens, err = revokeTokensHere(ctx, tx, p.UserID, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return RevocationReport{}, err
	}
	return out, nil
}

// SetUserDisabled withdraws (or restores) a principal. Disabling revokes every
// session and token in the same transaction: a disabled user holding a live
// cookie is the fail-open state docs/spec/14 exists to prevent. Restoring does
// not resurrect anything — old credentials stay revoked and the user logs in
// again.
func (a *AuthStore) SetUserDisabled(ctx context.Context, userID string, disabled bool, at time.Time) (RevocationReport, error) {
	var out RevocationReport
	if strings.TrimSpace(userID) == "" {
		return out, errors.New("store: set user disabled: user id is required")
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	at = at.UTC()
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, "set user disabled",
			`UPDATE users SET disabled = ? WHERE id = ?`, boolInt(disabled), userID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return notFound("set user disabled", "user")
		}
		if !disabled {
			return nil
		}
		if out.Sessions, err = revokeSessionsHere(ctx, tx, userID, at); err != nil {
			return err
		}
		out.Tokens, err = revokeTokensHere(ctx, tx, userID, at)
		return err
	})
	if err != nil {
		return RevocationReport{}, err
	}
	return out, nil
}

func revokeSessionsHere(ctx context.Context, tx *Tx, userID string, at time.Time) (int, error) {
	res, err := tx.exec(ctx, "revoke user sessions",
		`UPDATE sessions SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		stampMS(at), userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

func revokeTokensHere(ctx context.Context, tx *Tx, userID string, at time.Time) (int, error) {
	res, err := tx.exec(ctx, "revoke user tokens",
		`UPDATE api_tokens SET revoked_at = ? WHERE user_id = ? AND revoked_at IS NULL`,
		stampMS(at), userID)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// ── sessions ────────────────────────────────────────────────────────────────

// InsertSessionParams is the input to InsertSession. IDHash and CSRFHash are
// digests; ExpiresAt is the absolute deadline (the idle deadline is derived
// from LastSeenAt at read time).
type InsertSessionParams struct {
	IDHash    string
	UserID    string
	CSRFHash  string
	CreatedAt time.Time
	ExpiresAt time.Time
	UserAgent string
}

// InsertSession records a login.
func (a *AuthStore) InsertSession(ctx context.Context, p InsertSessionParams) (*Session, error) {
	if err := checkDigest("session id hash", p.IDHash); err != nil {
		return nil, err
	}
	if err := checkDigest("csrf hash", p.CSRFHash); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.UserID) == "" {
		return nil, errors.New("store: insert session: user id is required")
	}
	created := p.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expires := p.ExpiresAt.UTC()
	if expires.IsZero() {
		return nil, errors.New("store: insert session: an absolute expiry is required")
	}
	if !expires.After(created) {
		return nil, errors.New("store: insert session: expires_at must be after created_at")
	}
	ua := truncStr(p.UserAgent, 256)
	s := &Session{
		IDHash:     p.IDHash,
		UserID:     p.UserID,
		CSRFHash:   p.CSRFHash,
		CreatedAt:  created,
		LastSeenAt: created,
		ExpiresAt:  expires,
		UserAgent:  ua,
	}
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, "insert session",
			`INSERT INTO sessions (id_hash, user_id, csrf_hash, created_at, last_seen_at, expires_at, user_agent, revoked_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, NULL)`,
			s.IDHash, s.UserID, s.CSRFHash, stampMS(s.CreatedAt), stampMS(s.LastSeenAt),
			stampMS(s.ExpiresAt), s.UserAgent)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

const sessionColumns = `id_hash, user_id, csrf_hash, created_at, last_seen_at, expires_at, user_agent, revoked_at`

// SessionByHash loads the session whose token hashes to hash. ErrNotFound is
// the answer for "never issued" and "pruned after a restore" alike — both get
// the same client behaviour (re-authenticate) and neither is an oracle.
func (a *AuthStore) SessionByHash(ctx context.Context, hash string) (*Session, error) {
	if err := checkDigest("session id hash", hash); err != nil {
		return nil, err
	}
	var out *Session
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		s, err := scanSession(q.queryRow(ctx, "get session",
			"SELECT "+sessionColumns+" FROM sessions WHERE id_hash = ?", hash))
		out = s
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TouchSession advances last_seen_at, which is the idle-timeout clock.
func (a *AuthStore) TouchSession(ctx context.Context, hash string, seen time.Time) error {
	if err := checkDigest("session id hash", hash); err != nil {
		return err
	}
	if seen.IsZero() {
		seen = time.Now().UTC()
	}
	return a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, "touch session",
			`UPDATE sessions SET last_seen_at = ? WHERE id_hash = ? AND revoked_at IS NULL`,
			stampMS(seen.UTC()), hash)
		return err
	})
}

// RevokeSession voids one session. Idempotent: a row that is already revoked
// (or never existed) is not an error — logout must succeed from the client's
// point of view either way.
func (a *AuthStore) RevokeSession(ctx context.Context, hash string, at time.Time) error {
	if err := checkDigest("session id hash", hash); err != nil {
		return err
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, "revoke session",
			`UPDATE sessions SET revoked_at = ? WHERE id_hash = ? AND revoked_at IS NULL`,
			stampMS(at.UTC()), hash)
		return err
	})
}

// RevokeUserSessions voids every live session of one user.
func (a *AuthStore) RevokeUserSessions(ctx context.Context, userID string, at time.Time) (int, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var n int
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var err error
		n, err = revokeSessionsHere(ctx, tx, userID, at.UTC())
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// RevokeUserTokens voids every live API token of one user.
func (a *AuthStore) RevokeUserTokens(ctx context.Context, userID string, at time.Time) (int, error) {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var n int
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var err error
		n, err = revokeTokensHere(ctx, tx, userID, at.UTC())
		return err
	})
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ListUserSessions returns one user's sessions, newest first, including the
// revoked ones an operator is investigating.
func (a *AuthStore) ListUserSessions(ctx context.Context, userID string) ([]*Session, error) {
	var out []*Session
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, "list sessions",
			"SELECT "+sessionColumns+" FROM sessions WHERE user_id = ? ORDER BY created_at DESC", userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			s, err := scanSessionRows(rows)
			if err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── api tokens ──────────────────────────────────────────────────────────────

// InsertAPITokenParams is the input to InsertAPIToken.
type InsertAPITokenParams struct {
	ID        string
	UserID    string
	Name      string
	TokenHash string
	Role      string
	CreatedAt time.Time
	ExpiresAt time.Time // zero = no expiry
}

// InsertAPIToken records an integration credential. The caller keeps the raw
// token; this stores the digest.
func (a *AuthStore) InsertAPIToken(ctx context.Context, p InsertAPITokenParams) (*APIToken, error) {
	if !ValidAuthRole(p.Role) {
		return nil, fmt.Errorf("store: insert api token: role %q: %w", p.Role, ErrInvalidRole)
	}
	if err := checkDigest("token hash", p.TokenHash); err != nil {
		return nil, err
	}
	name := strings.TrimSpace(p.Name)
	if name == "" || len(name) > 100 {
		return nil, errors.New("store: insert api token: name must be 1..100 characters")
	}
	if strings.TrimSpace(p.UserID) == "" {
		return nil, errors.New("store: insert api token: user id is required")
	}
	created := p.CreatedAt.UTC()
	if created.IsZero() {
		created = time.Now().UTC()
	}
	expires := p.ExpiresAt.UTC()
	if !expires.IsZero() && !expires.After(created) {
		return nil, errors.New("store: insert api token: expires_at must be after created_at")
	}
	tok := &APIToken{
		ID:        p.ID,
		UserID:    p.UserID,
		Name:      name,
		TokenHash: p.TokenHash,
		Role:      p.Role,
		CreatedAt: created,
		ExpiresAt: expires,
	}
	if tok.ID == "" {
		tok.ID = games.NewID()
	}
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var expiresAt any
		if !tok.ExpiresAt.IsZero() {
			expiresAt = stampMS(tok.ExpiresAt)
		}
		_, err := tx.exec(ctx, "insert api token",
			`INSERT INTO api_tokens (id, user_id, name, token_hash, role, created_at, last_used_at, expires_at, revoked_at)
			 VALUES (?, ?, ?, ?, ?, ?, NULL, ?, NULL)`,
			tok.ID, tok.UserID, tok.Name, tok.TokenHash, tok.Role, stampMS(tok.CreatedAt), expiresAt)
		return err
	})
	if err != nil {
		return nil, err
	}
	return tok, nil
}

const tokenColumns = `id, user_id, name, token_hash, role, created_at, last_used_at, expires_at, revoked_at`

// APITokenByHash resolves a presented bearer token. ErrNotFound must also be
// what a caller produces for a revoked or expired token when it does not want
// to oracle which tokens existed, so the distinction is deliberately not made
// here either.
func (a *AuthStore) APITokenByHash(ctx context.Context, hash string) (*APIToken, error) {
	if err := checkDigest("token hash", hash); err != nil {
		return nil, err
	}
	var out *APIToken
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		t, err := scanToken(q.queryRow(ctx, "get api token",
			"SELECT "+tokenColumns+" FROM api_tokens WHERE token_hash = ?", hash))
		out = t
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TouchAPIToken records a use. Callers throttle it (once a minute per token) so
// a harness polling the API cannot write this row on every request.
func (a *AuthStore) TouchAPIToken(ctx context.Context, id string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, "touch api token",
			`UPDATE api_tokens SET last_used_at = ? WHERE id = ?`, stampMS(at.UTC()), id)
		return err
	})
}

// RevokeAPIToken voids one token of one user. Idempotent, like RevokeSession.
func (a *AuthStore) RevokeAPIToken(ctx context.Context, userID, tokenID string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		_, err := tx.exec(ctx, "revoke api token",
			`UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
			stampMS(at.UTC()), tokenID, userID)
		return err
	})
}

// ListAPITokens returns one user's tokens. token_hash is selected as ” — the
// struct field exists so one scan function serves the list and the get, and a
// list response has no business carrying the credential digests that the get
// path needs (docs/spec/14 "no raw key" covers our own credentials too).
func (a *AuthStore) ListAPITokens(ctx context.Context, userID string) ([]*APIToken, error) {
	var out []*APIToken
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, "list api tokens",
			`SELECT id, user_id, name, '', role, created_at, last_used_at, expires_at, revoked_at
			 FROM api_tokens WHERE user_id = ? ORDER BY created_at DESC`, userID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				t         APIToken
				created   string
				lastUsed  sql.NullString
				expiresAt sql.NullString
				revokedAt sql.NullString
			)
			if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.Role,
				&created, &lastUsed, &expiresAt, &revokedAt); err != nil {
				return mapReadError("api token scan", err)
			}
			if t.CreatedAt, err = mustStamp(created); err != nil {
				return err
			}
			if t.LastUsedAt, err = optStamp(lastUsed); err != nil {
				return err
			}
			if t.ExpiresAt, err = optStamp(expiresAt); err != nil {
				return err
			}
			if t.RevokedAt, err = optStamp(revokedAt); err != nil {
				return err
			}
			out = append(out, &t)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── retention ───────────────────────────────────────────────────────────────

// PruneAuthReport counts what retention removed. docs/spec/02 keeps the
// operational store bounded, and sessions/audit rows are operational data.
type PruneAuthReport struct {
	Sessions int
	Tokens   int
	Audit    int
}

// PruneExpired deletes sessions past their absolute deadline or whose idle
// window has lapsed, expired or long-revoked tokens, and audit rows older than
// retainAudit (at most maxAudit rows per call, so an appliance that has not
// pruned in a year cannot spend one login doing it).
//
// Idle-expired sessions are *deleted*, not marked: nothing can resurrect them,
// and the row's only remaining reader would be a forensic query that the audit
// trail already answers.
func (a *AuthStore) PruneExpired(ctx context.Context, now time.Time, idleTimeout, retainAudit time.Duration, maxAudit int) (PruneAuthReport, error) {
	var out PruneAuthReport
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if maxAudit <= 0 {
		maxAudit = 1000
	}
	deadline := stampMS(now)
	idleCutoff := stampMS(now.Add(-idleTimeout))
	auditCutoff := stampMS(now.Add(-retainAudit))
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		res, err := tx.exec(ctx, "prune sessions",
			`DELETE FROM sessions
			 WHERE expires_at <= ?
			    OR (revoked_at IS NOT NULL AND revoked_at <= ?)
			    OR (revoked_at IS NULL AND last_seen_at <= ?)`,
			deadline, deadline, idleCutoff)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil {
			out.Sessions = int(n)
		}
		res, err = tx.exec(ctx, "prune api tokens",
			`DELETE FROM api_tokens
			 WHERE (expires_at IS NOT NULL AND expires_at <= ?)
			    OR (revoked_at IS NOT NULL AND revoked_at <= ?)`,
			deadline, deadline)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil {
			out.Tokens = int(n)
		}
		res, err = tx.exec(ctx, "prune auth audit",
			`DELETE FROM auth_audit WHERE id IN
			 (SELECT id FROM auth_audit WHERE occurred_at <= ? ORDER BY occurred_at LIMIT ?)`,
			auditCutoff, maxAudit)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err == nil {
			out.Audit = int(n)
		}
		return nil
	})
	if err != nil {
		return PruneAuthReport{}, err
	}
	return out, nil
}

// ── audit ───────────────────────────────────────────────────────────────────

// AuthAuditParams is the input to RecordAuthAudit.
type AuthAuditParams struct {
	OccurredAt time.Time
	UserID     string
	Event      string
	RemoteAddr string
	Detail     json.RawMessage
}

// RecordAuthAudit appends one auth decision. It has no field where a password,
// a raw token or a hash would belong, and Detail is validated as JSON so a
// caller that concatenated a secret into a string is rejected rather than
// stored.
//
// Audit failures are returned, never swallowed: an auth event that was not
// recorded is a hole in the trail docs/spec/14 depends on. The caller decides
// whether a dropped row fails the request (for a privileged write it does) or
// only gets logged.
func (a *AuthStore) RecordAuthAudit(ctx context.Context, p AuthAuditParams) error {
	event := strings.TrimSpace(p.Event)
	if event == "" || len(event) > 100 {
		return fmt.Errorf("store: record auth audit: event %q: %w", truncStr(event, 32), ErrInvalidAuditEvent)
	}
	at := p.OccurredAt.UTC()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	detail := "{}"
	if len(p.Detail) > 0 {
		// A caller that handed us something which is not JSON is not asking for an
		// empty detail: orDefault would silently replace the evidence with {}, and
		// an audit trail that quietly drops its own payload is worse than one that
		// refuses the row. 0006's json_valid CHECK is the same rule from the other
		// side.
		if !json.Valid(p.Detail) {
			return fmt.Errorf("store: record auth audit: detail is not JSON: %w", ErrInvalidAuditEvent)
		}
		detail = string(p.Detail)
	}
	err := a.store.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		var userID any
		if strings.TrimSpace(p.UserID) != "" {
			userID = p.UserID
		}
		_, err := tx.exec(ctx, "insert auth audit",
			`INSERT INTO auth_audit (occurred_at, user_id, event, remote_addr, detail_json)
			 VALUES (?, ?, ?, ?, ?)`,
			stampMS(at), userID, event, truncStr(p.RemoteAddr, 128), detail)
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

// ListAuthAudit returns the newest auth events for the developer inspector and
// `doctor`, bounded by MaxListLimit.
func (a *AuthStore) ListAuthAudit(ctx context.Context, limit int) ([]AuthAudit, error) {
	limit = clampLimit(limit, MaxListLimit)
	var out []AuthAudit
	err := a.store.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, "list auth audit",
			`SELECT occurred_at, user_id, event, remote_addr, detail_json
			 FROM auth_audit ORDER BY id DESC LIMIT ?`, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				e      AuthAudit
				at     string
				userID sql.NullString
				event  string
				remote string
				detail string
			)
			if err := rows.Scan(&at, &userID, &event, &remote, &detail); err != nil {
				return mapReadError("auth audit scan", err)
			}
			if e.OccurredAt, err = mustStamp(at); err != nil {
				return err
			}
			e.UserID, e.Event, e.RemoteAddr = optText(userID), event, remote
			e.Detail = json.RawMessage(detail)
			out = append(out, e)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ── scanning and validation helpers ─────────────────────────────────────────

func scanUser(row *sql.Row) (*User, error) {
	var (
		u        User
		disabled int
		created  string
		changed  string
	)
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &created, &changed); err != nil {
		return nil, mapReadError("user", err)
	}
	u.Disabled = disabled != 0
	var err error
	if u.CreatedAt, err = mustStamp(created); err != nil {
		return nil, fmt.Errorf("store: user %s created_at: %w", u.ID, err)
	}
	if u.PasswordChangedAt, err = mustStamp(changed); err != nil {
		return nil, fmt.Errorf("store: user %s password_changed_at: %w", u.ID, err)
	}
	return &u, nil
}

func scanUserRows(rows *sql.Rows) (*User, error) {
	var (
		u        User
		disabled int
		created  string
		changed  string
	)
	if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.Role, &disabled, &created, &changed); err != nil {
		return nil, mapReadError("user scan", err)
	}
	u.Disabled = disabled != 0
	var err error
	if u.CreatedAt, err = mustStamp(created); err != nil {
		return nil, fmt.Errorf("store: user %s created_at: %w", u.ID, err)
	}
	if u.PasswordChangedAt, err = mustStamp(changed); err != nil {
		return nil, fmt.Errorf("store: user %s password_changed_at: %w", u.ID, err)
	}
	return &u, nil
}

func scanSession(row *sql.Row) (*Session, error) {
	var (
		s         Session
		created   string
		lastSeen  string
		expires   string
		revokedAt sql.NullString
	)
	if err := row.Scan(&s.IDHash, &s.UserID, &s.CSRFHash, &created, &lastSeen, &expires, &s.UserAgent, &revokedAt); err != nil {
		return nil, mapReadError("session", err)
	}
	return finishSession(&s, created, lastSeen, expires, revokedAt)
}

func scanSessionRows(rows *sql.Rows) (*Session, error) {
	var (
		s         Session
		created   string
		lastSeen  string
		expires   string
		revokedAt sql.NullString
	)
	if err := rows.Scan(&s.IDHash, &s.UserID, &s.CSRFHash, &created, &lastSeen, &expires, &s.UserAgent, &revokedAt); err != nil {
		return nil, mapReadError("session scan", err)
	}
	return finishSession(&s, created, lastSeen, expires, revokedAt)
}

func finishSession(s *Session, created, lastSeen, expires string, revokedAt sql.NullString) (*Session, error) {
	var err error
	if s.CreatedAt, err = mustStamp(created); err != nil {
		return nil, fmt.Errorf("store: session %s created_at: %w", truncDigest(s.IDHash), err)
	}
	if s.LastSeenAt, err = mustStamp(lastSeen); err != nil {
		return nil, fmt.Errorf("store: session %s last_seen_at: %w", truncDigest(s.IDHash), err)
	}
	if s.ExpiresAt, err = mustStamp(expires); err != nil {
		return nil, fmt.Errorf("store: session %s expires_at: %w", truncDigest(s.IDHash), err)
	}
	if s.RevokedAt, err = optStamp(revokedAt); err != nil {
		return nil, fmt.Errorf("store: session %s revoked_at: %w", truncDigest(s.IDHash), err)
	}
	return s, nil
}

func scanToken(row *sql.Row) (*APIToken, error) {
	var (
		t         APIToken
		created   string
		lastUsed  sql.NullString
		expiresAt sql.NullString
		revokedAt sql.NullString
	)
	if err := row.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.Role,
		&created, &lastUsed, &expiresAt, &revokedAt); err != nil {
		return nil, mapReadError("api token", err)
	}
	var err error
	if t.CreatedAt, err = mustStamp(created); err != nil {
		return nil, fmt.Errorf("store: api token %s created_at: %w", t.ID, err)
	}
	if t.LastUsedAt, err = optStamp(lastUsed); err != nil {
		return nil, fmt.Errorf("store: api token %s last_used_at: %w", t.ID, err)
	}
	if t.ExpiresAt, err = optStamp(expiresAt); err != nil {
		return nil, fmt.Errorf("store: api token %s expires_at: %w", t.ID, err)
	}
	if t.RevokedAt, err = optStamp(revokedAt); err != nil {
		return nil, fmt.Errorf("store: api token %s revoked_at: %w", t.ID, err)
	}
	return &t, nil
}

// mustStamp reads a required TEXT timestamp. SQLite has no date type
// (0001_init.sql), so an unparsable string is corruption to be reported, not a
// zero time to be mistaken for "expired in 1970" or "never used".
func mustStamp(s string) (time.Time, error) { return parseStamp(s) }

// optStamp reads a nullable TEXT timestamp; NULL is the zero time, which every
// caller here interprets as "no deadline"/"never", the documented meaning of
// the column being NULL.
func optStamp(v sql.NullString) (time.Time, error) {
	if !v.Valid || v.String == "" {
		return time.Time{}, nil
	}
	return parseStamp(v.String)
}

// truncDigest keeps a digest prefix for error text: enough to correlate a log
// line with a row, and useless as a credential.
func truncDigest(digest string) string {
	if len(digest) > 8 {
		return digest[:8] + "…"
	}
	return digest
}

// checkDigest enforces the 0006 CHECK on a digest column: exactly 64 lowercase
// hex characters. It rejects an empty value too, which is the shape a caller
// would produce if it stored a raw token by mistake — the failure mode this
// file exists to prevent.
func checkDigest(what, digest string) error {
	if len(digest) != digestLength {
		return fmt.Errorf("store: %s must be a %d-char sha256 hex digest, got %d chars: %w",
			what, digestLength, len(digest), ErrInvalidSecret)
	}
	for i := 0; i < len(digest); i++ {
		c := digest[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return fmt.Errorf("store: %s is not lowercase hex: %w", what, ErrInvalidSecret)
		}
	}
	return nil
}

// checkUsername mirrors the schema's bounds and the charset internal/auth
// enforces: 3-64 of [A-Za-z0-9._@+-], first and last alphanumeric, ASCII only.
// ASCII matters because SQLite's NOCASE collation folds ASCII only, so a
// non-ASCII name would be unique in a way login can never reproduce.
func checkUsername(name string) error {
	if len(name) < 3 || len(name) > 64 {
		return fmt.Errorf("store: username %q must be 3..64 bytes: %w", truncStr(name, 24), ErrInvalidUsername)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		switch {
		case alnum:
		case c == '.' || c == '_' || c == '-' || c == '@' || c == '+':
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("store: username %q must start and end alphanumeric: %w",
					truncStr(name, 24), ErrInvalidUsername)
			}
		default:
			return fmt.Errorf("store: username %q has an unsupported character %q: %w",
				truncStr(name, 24), c, ErrInvalidUsername)
		}
	}
	return nil
}

// checkPasswordHash keeps a malformed envelope out of the column so a later
// login never has to distinguish "unusable row" from "wrong password".
// internal/auth.HashPassword is the only producer; the check is deliberately
// loose about the iteration count (that is the point of a self-describing
// hash) and strict about the shape.
func checkPasswordHash(hash string) error {
	if len(hash) < 32 || len(hash) > 512 {
		return fmt.Errorf("store: password hash length %d: %w", len(hash), ErrInvalidPasswordHash)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") && !strings.HasPrefix(hash, "argon2id$") {
		return fmt.Errorf("store: password hash has no known algorithm prefix: %w", ErrInvalidPasswordHash)
	}
	return nil
}

func truncStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
