package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// Repo is the persistence the auth service needs. *store.AuthStore satisfies it
// exactly; the interface is here so the credential rules below can be exercised
// against a fake, and so the service cannot reach around the auth tables into
// somebody else's repository.
type Repo interface {
	CreateUser(ctx context.Context, p UserCreation) (*StoredUser, error)
	UserByID(ctx context.Context, id string) (*StoredUser, error)
	UserByUsername(ctx context.Context, username string) (*StoredUser, error)
	ListUsers(ctx context.Context) ([]*StoredUser, error)
	HasRole(ctx context.Context, role string) (bool, error)
	RotatePassword(ctx context.Context, p PasswordRotation) (Revocations, error)
	SetUserDisabled(ctx context.Context, userID string, disabled bool, at time.Time) (Revocations, error)

	InsertSession(ctx context.Context, p SessionCreation) (*StoredSession, error)
	SessionByHash(ctx context.Context, hash string) (*StoredSession, error)
	TouchSession(ctx context.Context, hash string, seen time.Time) error
	RevokeSession(ctx context.Context, hash string, at time.Time) error
	RevokeUserSessions(ctx context.Context, userID string, at time.Time) (int, error)
	ListUserSessions(ctx context.Context, userID string) ([]*StoredSession, error)

	InsertAPIToken(ctx context.Context, p TokenCreation) (*StoredToken, error)
	APITokenByHash(ctx context.Context, hash string) (*StoredToken, error)
	TouchAPIToken(ctx context.Context, id string, at time.Time) error
	RevokeAPIToken(ctx context.Context, userID, tokenID string, at time.Time) error
	RevokeUserTokens(ctx context.Context, userID string, at time.Time) (int, error)
	ListAPITokens(ctx context.Context, userID string) ([]*StoredToken, error)

	PruneExpired(ctx context.Context, now time.Time, idleTimeout, retainAudit time.Duration, maxAudit int) (PruneReport, error)
	RecordAuthAudit(ctx context.Context, p AuditRecord) error
}

// Identity is the authenticated principal behind one request. It is what the
// middleware attaches to the context and what every handler reads, so it is the
// only type in this package that crosses the HTTP boundary.
//
// Role is the *effective* role: for a session it is the user's role, for an API
// token it is the token's own ceiling clamped to the user's current role — a
// token minted for an operator whose account was later demoted must not keep
// developer powers (docs/spec/14 "role ACL").
type Identity struct {
	User      *StoredUser
	Role      Role
	Method    string // MethodCookie or MethodToken
	Kind      string // "session" or "api-token"
	Session   *StoredSession
	Token     *StoredToken
	ExpiresAt time.Time
}

// Credential methods, as they appear in /auth/me and the audit trail.
const (
	MethodCookie = "cookie"
	MethodToken  = "token"
)

// IsCookie reports whether this identity came from a browser session, i.e.
// whether CSRF rules apply (docs/spec/08 "State-changing calls require CSRF
// protection under cookie auth" — a bearer token needs no CSRF because a
// cross-origin fetch cannot attach one implicitly).
func (i *Identity) IsCookie() bool { return i != nil && i.Method == MethodCookie }

// Authenticated reports whether a principal was established.
func (i *Identity) Authenticated() bool { return i != nil && i.User != nil && i.Role >= RoleViewer }

// Username returns the principal's name for logs and payloads.
func (i *Identity) Username() string {
	if i == nil || i.User == nil {
		return ""
	}
	return i.User.Username
}

// ── errors ──────────────────────────────────────────────────────────────────

// Sentinel errors. Each one maps to exactly one HTTP outcome in
// internal/api/auth.go, so the mapping stays a table rather than a guess.
var (
	// ErrInvalidCredentials is wrong password *or* unknown username *or* a
	// disabled account, deliberately undifferentiated: the response must not
	// confirm that a username exists (docs/spec/14).
	ErrInvalidCredentials = errors.New("auth: credentials are not valid for this account")
	// ErrUnknownSession is a cookie this service never issued (or pruned).
	ErrUnknownSession = errors.New("auth: no session matches this cookie")
	// ErrSessionRevoked is a cookie that was voided — logout, password change,
	// disable, or a stale one left behind by a restored backup.
	ErrSessionRevoked = errors.New("auth: this session has been revoked")
	// ErrSessionExpired is the absolute 7-day deadline passing.
	ErrSessionExpired = errors.New("auth: this session has expired")
	// ErrSessionIdle is the 12-hour idle timeout passing.
	ErrSessionIdle = errors.New("auth: this session has been idle for too long")
	// ErrAlreadyInitialized means first-run setup ran when an admin already
	// exists (409).
	ErrAlreadyInitialized = errors.New("auth: an admin account already exists")
	// ErrSetupNotLocal means setup was attempted off-loopback (403).
	ErrSetupNotLocal = errors.New("auth: setup is only available from the local machine")
	// ErrInvalidCSRF is a missing or wrong X-CSRF-Token (403).
	ErrInvalidCSRF = errors.New("auth: the CSRF token is missing or does not match this session")
	// ErrInvalidOrigin is an Origin/Referer that does not match the request
	// host (403).
	ErrInvalidOrigin = errors.New("auth: the request origin does not match this host")
	// ErrNotPermitted is a role shortfall (403).
	ErrNotPermitted = errors.New("auth: this principal's role is not sufficient")
	// ErrInvalidUsername is a username outside the allowed shape (400).
	ErrInvalidUsername = errors.New("auth: username must be 3..64 characters of letters, digits, . _ - @ +, starting and ending alphanumeric")
	// ErrRoleTooHigh is a token asking for more than its owner holds (400).
	ErrRoleTooHigh = errors.New("auth: an api token may not exceed its owner's role")
	// ErrUnknownToken is a bearer token that does not resolve (401).
	ErrUnknownToken = errors.New("auth: no api token matches this credential")
	// ErrInternal is a data/infrastructure fault (500). It is deliberately
	// opaque: the detail stays in the log, not on the wire.
	ErrInternal = errors.New("auth: internal authentication fault")
)

// RateLimited reports a throttled login/setup attempt, carrying the value the
// API puts in Retry-After (429).
type RateLimited struct {
	RetryAfter time.Duration
}

func (e *RateLimited) Error() string {
	return fmt.Sprintf("auth: too many authentication attempts, retry after %s", e.RetryAfter.Round(time.Second))
}

// RetryAfterSeconds is the integer Retry-After header value, rounded up so the
// header never promises less than the limiter actually withholds.
func (e *RateLimited) RetryAfterSeconds() int {
	secs := int(e.RetryAfter/time.Second) + 1
	return secs
}

// ── service ─────────────────────────────────────────────────────────────────

// SessionPolicy is the session lifetime contract of docs/spec/08: 12 h idle,
// 7 d absolute.
type SessionPolicy struct {
	IdleTimeout   time.Duration
	AbsoluteTTL   time.Duration
	TouchInterval time.Duration
	// AuditRetention is how long auth_audit rows are kept, and PruneBatch the
	// row cap per sweep (docs/spec/02 keeps the operational store bounded).
	AuditRetention time.Duration
	PruneBatch     int
}

// DefaultSessionPolicy returns the spec's lifetimes. TouchInterval is the write
// throttle on last_seen_at: refreshing it on every request would make a
// reconnecting SSE client the busiest writer on the appliance, and 12 h of idle
// timeout is not measurable at minute resolution.
func DefaultSessionPolicy() SessionPolicy {
	return SessionPolicy{
		IdleTimeout:    12 * time.Hour,
		AbsoluteTTL:    7 * 24 * time.Hour,
		TouchInterval:  time.Minute,
		AuditRetention: 90 * 24 * time.Hour,
		PruneBatch:     1000,
	}
}

// Service implements login, session and token verification, CSRF, password
// changes and the first-run rule on top of a Repo.
type Service struct {
	repo     Repo
	clock    func() time.Time
	sessions SessionPolicy
	limiter  *Limiter
	log      *slog.Logger

	// iterations is this service's PBKDF2 work factor, PBKDF2Iterations unless
	// WithPasswordCost says otherwise.
	iterations int
}

// Option customises a Service.
type Option func(*Service)

// WithClock injects time. Expiry is otherwise untestable without sleeping, and
// "idle and absolute expiry" is an acceptance test, not a comment.
func WithClock(now func() time.Time) Option {
	return func(s *Service) {
		if now != nil {
			s.clock = now
		}
	}
}

// WithSessionPolicy overrides the lifetimes (tests, and a future config route).
func WithSessionPolicy(p SessionPolicy) Option {
	return func(s *Service) {
		if p.IdleTimeout > 0 {
			s.sessions.IdleTimeout = p.IdleTimeout
		}
		if p.AbsoluteTTL > 0 {
			s.sessions.AbsoluteTTL = p.AbsoluteTTL
		}
		if p.TouchInterval > 0 {
			s.sessions.TouchInterval = p.TouchInterval
		}
		if p.AuditRetention > 0 {
			s.sessions.AuditRetention = p.AuditRetention
		}
		if p.PruneBatch > 0 {
			s.sessions.PruneBatch = p.PruneBatch
		}
	}
}

// WithLimiter replaces the login limiter (tests use a tiny bucket).
func WithLimiter(l *Limiter) Option {
	return func(s *Service) {
		if l != nil {
			s.limiter = l
		}
	}
}

// WithPasswordCost sets this service's PBKDF2 work factor. The default is
// PBKDF2Iterations (600k, OWASP 2023+); a deployment on the docs/spec/10 ARM
// target may tune it, and a test must tune it — a suite that hashes at the
// production cost spends its whole budget inside the KDF, and the parameter
// upgrade path (a stored hash below the current cost being re-derived on
// login) is only observable with two costs in play.
//
// It refuses anything below minStoredIterations: a service configured to hash
// cheaply is not a fast service, it is an unprotected password file.
func WithPasswordCost(iterations int) Option {
	return func(s *Service) {
		if iterations >= minStoredIterations && iterations <= maxStoredIterations {
			s.iterations = iterations
		}
	}
}

// PasswordCost reports the configured work factor.
func (s *Service) PasswordCost() int { return s.iterations }

// WithLogger attaches a logger; nothing in this package logs a credential.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.log = l
		}
	}
}

// NewService wires the auth service onto a repository.
func NewService(repo Repo, opts ...Option) *Service {
	s := &Service{
		repo:       repo,
		clock:      time.Now,
		sessions:   DefaultSessionPolicy(),
		limiter:    NewLoginLimiter(),
		log:        slog.Default(),
		iterations: PBKDF2Iterations,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Now exposes the service clock so the HTTP layer stamps responses with the
// same time the policy decided on.
func (s *Service) Now() time.Time { return s.clock().UTC() }

// Sessions reports the effective lifetimes.
func (s *Service) Sessions() SessionPolicy { return s.sessions }

// ValidateUsername enforces the credential-store's name rules, which are the
// schema's CHECK plus the ASCII-only requirement that makes SQLite's NOCASE
// uniqueness mean what it looks like.
func ValidateUsername(name string) error {
	name = strings.TrimSpace(name)
	if len(name) < 3 || len(name) > 64 {
		return fmt.Errorf("%w: %q is %d characters", ErrInvalidUsername, name, len(name))
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
		switch {
		case alnum:
		case c == '.' || c == '_' || c == '-' || c == '@' || c == '+':
			if i == 0 || i == len(name)-1 {
				return fmt.Errorf("%w: %q must start and end alphanumeric", ErrInvalidUsername, name)
			}
		default:
			return fmt.Errorf("%w: %q contains %q", ErrInvalidUsername, name, c)
		}
	}
	return nil
}

// Status answers GET /api/v1/auth/status: whether the appliance still needs its
// first admin.
func (s *Service) Status(ctx context.Context) (setupRequired bool, err error) {
	hasAdmin, err := s.repo.HasRole(ctx, "admin")
	if err != nil {
		return false, fmt.Errorf("auth: setup status: %w", err)
	}
	return !hasAdmin, nil
}

// Setup creates the very first admin, from the local machine only
// (docs/spec/14 "local-first, set admin on first run"; docs/spec/10 "Bind
// localhost on initial boot").
//
// loopback must be the *transport* answer, computed by the caller from
// r.RemoteAddr. It is a parameter rather than something read from a context so
// that no code path can accidentally feed it a header.
func (s *Service) Setup(ctx context.Context, username, password, remoteAddr string, loopback bool) (*StoredUser, error) {
	if !loopback {
		s.audit(ctx, "setup.refused", "", remoteAddr, map[string]any{"reason": "not loopback"})
		return nil, ErrSetupNotLocal
	}
	required, err := s.Status(ctx)
	if err != nil {
		return nil, err
	}
	if !required {
		s.audit(ctx, "setup.refused", "", remoteAddr, map[string]any{"reason": "admin exists"})
		return nil, ErrAlreadyInitialized
	}
	u, err := s.createUser(ctx, username, password, RoleAdmin, remoteAddr, "setup")
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "setup.complete", u.ID, remoteAddr, map[string]any{"username": u.Username, "role": "admin"})
	return u, nil
}

// CreateUser is the admin-facing create (POST /api/v1/users).
func (s *Service) CreateUser(ctx context.Context, username, password string, role Role, actor *Identity, remoteAddr string) (*StoredUser, error) {
	if !role.Valid() {
		return nil, fmt.Errorf("%w: %q is not a role", ErrInvalidUsername, role)
	}
	u, err := s.createUser(ctx, username, password, role, remoteAddr, "admin")
	if err != nil {
		return nil, err
	}
	s.audit(ctx, "user.create", u.ID, remoteAddr, map[string]any{
		"username": u.Username, "role": u.Role, "by": actor.key(),
	})
	return u, nil
}

func (s *Service) createUser(ctx context.Context, username, password string, role Role, remoteAddr, via string) (*StoredUser, error) {
	username = strings.TrimSpace(username)
	if err := ValidateUsername(username); err != nil {
		return nil, err
	}
	if err := ValidatePassword(password); err != nil {
		return nil, err
	}
	hash, err := hashPasswordWith(password, s.iterations)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	u, err := s.repo.CreateUser(ctx, UserCreation{
		Username:     username,
		PasswordHash: hash,
		Role:         role.String(),
		Now:          s.Now(),
	})
	if err != nil {
		return nil, err
	}
	s.log.Info("auth: user created", "user_id", u.ID, "username", u.Username,
		"role", u.Role, "via", via, "remote_addr", remoteAddr)
	return u, nil
}

// LoginParams is one authentication attempt.
type LoginParams struct {
	Username   string
	Password   string
	UserAgent  string
	RemoteAddr string
	// ForwardedProto is the *validated* scheme hint (already gated on the
	// request arriving from a configured trusted proxy by the caller). "" means
	// "no hint".
	ForwardedProto string
}

// SessionGrant is what a successful login hands the client. Token and CSRF are
// the only times their raw values exist anywhere.
type SessionGrant struct {
	User      *StoredUser
	Token     string
	CSRF      string
	CreatedAt time.Time
	ExpiresAt time.Time
	Secure    bool
}

// Login validates credentials and mints a session.
func (s *Service) Login(ctx context.Context, p LoginParams) (*SessionGrant, error) {
	username := strings.TrimSpace(p.Username)

	// Two independent buckets: per source address (one host cannot burn through
	// the whole allowance) and per username (a distributed attacker cannot grind
	// one account past a dozen hosts). Both must have a token.
	ipKey, userKey := loginKeys(username, p.RemoteAddr)
	if wait := s.spend(ipKey); wait > 0 {
		s.audit(ctx, "login.rate_limited", "", p.RemoteAddr, map[string]any{"username": username, "scope": "address"})
		return nil, &RateLimited{RetryAfter: wait}
	}
	if wait := s.spend(userKey); wait > 0 {
		s.audit(ctx, "login.rate_limited", "", p.RemoteAddr, map[string]any{"username": username, "scope": "account"})
		return nil, &RateLimited{RetryAfter: wait}
	}

	user, err := s.repo.UserByUsername(ctx, username)
	switch {
	case errors.Is(err, ErrNoSuchUser):
		// Same cost, same answer as a wrong password: see dummyVerifyPassword.
		dummyVerifyPassword(p.Password, s.iterations)
		s.audit(ctx, "login.failure", "", p.RemoteAddr, map[string]any{"username": username, "reason": "unknown account"})
		return nil, ErrInvalidCredentials
	case err != nil:
		return nil, fmt.Errorf("%w: lookup: %v", ErrInternal, err)
	}

	ok, err := VerifyPassword(p.Password, user.PasswordHash)
	if err != nil {
		// A malformed stored hash is not a wrong password. Say so in the log
		// and the audit trail; the client still gets the uniform answer,
		// because "your row is corrupt" is itself a user-enumeration hint.
		s.log.Error("auth: stored password hash is unusable",
			"user_id", user.ID, "username", user.Username, "error", err)
		s.audit(ctx, "login.failure", user.ID, p.RemoteAddr, map[string]any{"username": username, "reason": "unusable credential"})
		return nil, ErrInvalidCredentials
	}
	if !ok {
		s.audit(ctx, "login.failure", user.ID, p.RemoteAddr, map[string]any{"username": username, "reason": "wrong password"})
		return nil, ErrInvalidCredentials
	}
	if user.Disabled {
		// Audited distinctly (an operator needs to see that somebody with a real
		// password is being turned away) but answered identically to a wrong
		// password on the wire.
		s.audit(ctx, "login.failure", user.ID, p.RemoteAddr, map[string]any{"username": username, "reason": "account disabled"})
		return nil, ErrInvalidCredentials
	}

	if NeedsUpgradeTo(user.PasswordHash, s.iterations) {
		// Rehash at the current cost. Sessions and tokens are *not* revoked:
		// nothing about this login suggests a compromise, the user did not
		// change anything, and killing every device for a server-side
		// parameter bump would train operators to distrust the appliance.
		// A password the user actually changes goes through ChangePassword,
		// which does revoke everything.
		newHash, herr := hashPasswordWith(p.Password, s.iterations)
		if herr != nil {
			return nil, fmt.Errorf("%w: rehash: %v", ErrInternal, herr)
		}
		if _, err := s.repo.RotatePassword(ctx, PasswordRotation{
			UserID:       user.ID,
			PasswordHash: newHash,
			Now:          s.Now(),
		}); err != nil {
			// A failed upgrade must not block the login it was meant to
			// improve; it is logged and retried on the next successful login.
			s.log.Warn("auth: could not upgrade password hash parameters",
				"user_id", user.ID, "error", err)
		} else {
			user.PasswordHash = newHash
			s.audit(ctx, "password.upgraded", user.ID, p.RemoteAddr, map[string]any{"username": username})
		}
	}

	token, err := NewSessionToken()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	csrf, err := NewCSRFToken()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	now := s.Now()
	grant := &SessionGrant{
		User:      user,
		Token:     token,
		CSRF:      csrf,
		CreatedAt: now,
		ExpiresAt: now.Add(s.sessions.AbsoluteTTL),
		Secure:    schemeIsHTTPS(p.ForwardedProto),
	}
	if _, err := s.repo.InsertSession(ctx, SessionCreation{
		IDHash:    DigestSecret(token),
		UserID:    user.ID,
		CSRFHash:  DigestSecret(csrf),
		CreatedAt: now,
		ExpiresAt: grant.ExpiresAt,
		UserAgent: p.UserAgent,
	}); err != nil {
		return nil, fmt.Errorf("%w: insert session: %v", ErrInternal, err)
	}
	s.limiter.Reset(ipKey)
	s.limiter.Reset(userKey)
	s.audit(ctx, "login.success", user.ID, p.RemoteAddr, map[string]any{"username": username})
	return grant, nil
}

// VerifySession resolves a cookie value to a principal, applying revocation,
// the absolute deadline, the idle deadline, the disabled flag and the
// "password changed since this session was minted" rule.
func (s *Service) VerifySession(ctx context.Context, rawToken string) (*Identity, error) {
	if rawToken == "" {
		return nil, ErrUnknownSession
	}
	hash := DigestSecret(rawToken)
	sess, err := s.repo.SessionByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, ErrNoSuchUser) {
			return nil, ErrUnknownSession
		}
		return nil, fmt.Errorf("%w: session lookup: %v", ErrInternal, err)
	}
	now := s.Now()
	switch {
	case sess.Revoked():
		return nil, ErrSessionRevoked
	case !sess.ExpiresAt.After(now):
		return nil, ErrSessionExpired
	case now.Sub(sess.LastSeenAt) >= s.sessions.IdleTimeout:
		// Mark it, so the next request does not have to re-derive why the
		// previous one was refused, and so `admin` tooling can see it died.
		if err := s.repo.RevokeSession(ctx, sess.IDHash, now); err != nil {
			s.log.Warn("auth: could not revoke an idle-expired session", "error", err)
		}
		return nil, ErrSessionIdle
	}

	user, err := s.repo.UserByID(ctx, sess.UserID)
	if err != nil {
		if errors.Is(err, ErrNoSuchUser) {
			// The principal is gone (deleted user, or a restored backup whose
			// sessions outlived it). Void the cookie rather than answer 500:
			// the client's only correct move is to log in again.
			if rerr := s.repo.RevokeSession(ctx, sess.IDHash, now); rerr != nil {
				s.log.Warn("auth: could not revoke an orphaned session", "error", rerr)
			}
			return nil, ErrUnknownSession
		}
		return nil, fmt.Errorf("%w: user lookup: %v", ErrInternal, err)
	}
	if user.Disabled {
		return nil, ErrSessionRevoked
	}
	if user.PasswordChangedAt.After(sess.CreatedAt) {
		// Defence in depth for docs/spec/14's stolen-cookie test: revocation on
		// a password change is a bulk UPDATE, and this predicate makes the
		// rule true even for a session row that some path failed to revoke.
		// Millisecond resolution is enough because both stamps are written from
		// the same clock in the same transaction.
		return nil, ErrSessionRevoked
	}

	if s.sessions.TouchInterval > 0 && now.Sub(sess.LastSeenAt) >= s.sessions.TouchInterval {
		if err := s.repo.TouchSession(ctx, sess.IDHash, now); err != nil {
			// A dropped touch shortens nobody's deadline by more than the
			// throttle interval, and refusing a working request over it would
			// be worse than the bookkeeping gap.
			s.log.Warn("auth: could not refresh session last_seen_at", "error", err)
		} else {
			sess.LastSeenAt = now
		}
	}

	role, err := ParseRole(user.Role)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	return &Identity{
		User:      user,
		Role:      role,
		Method:    MethodCookie,
		Kind:      "session",
		Session:   sess,
		ExpiresAt: sess.ExpiresAt,
	}, nil
}

// VerifyToken resolves a `gn_…` bearer credential. It is exempt from CSRF by
// construction (docs/spec/08: CSRF applies "under cookie auth") because a
// cross-origin page cannot attach an Authorization header implicitly — that is
// the whole reason the two mechanisms differ.
func (s *Service) VerifyToken(ctx context.Context, rawToken string) (*Identity, error) {
	if !LooksLikeAPIToken(rawToken) {
		return nil, ErrUnknownToken
	}
	tok, err := s.repo.APITokenByHash(ctx, DigestSecret(rawToken))
	if err != nil {
		if errors.Is(err, ErrNoSuchUser) {
			return nil, ErrUnknownToken
		}
		return nil, fmt.Errorf("%w: token lookup: %v", ErrInternal, err)
	}
	now := s.Now()
	if !tok.Live(now) {
		return nil, ErrUnknownToken
	}
	user, err := s.repo.UserByID(ctx, tok.UserID)
	if err != nil {
		if errors.Is(err, ErrNoSuchUser) {
			return nil, ErrUnknownToken
		}
		return nil, fmt.Errorf("%w: token owner lookup: %v", ErrInternal, err)
	}
	if user.Disabled {
		return nil, ErrUnknownToken
	}
	ownerRole, err := ParseRole(user.Role)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	tokenRole, err := ParseRole(tok.Role)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	effective := ownerRole
	if tokenRole < effective {
		effective = tokenRole
	}
	if effective == RoleNone {
		return nil, ErrUnknownToken
	}

	// Throttled bookkeeping: "was this credential used at all" is what an
	// operator needs before revoking it, and once a minute per token is enough
	// to answer that without making a harness the busiest writer on the box.
	if tok.LastUsedAt.IsZero() || now.Sub(tok.LastUsedAt) >= time.Minute {
		if err := s.repo.TouchAPIToken(ctx, tok.ID, now); err != nil {
			s.log.Warn("auth: could not record api token use", "token_id", tok.ID, "error", err)
		}
	}

	return &Identity{
		User:      user,
		Role:      effective,
		Method:    MethodToken,
		Kind:      "api-token",
		Token:     tok,
		ExpiresAt: tok.ExpiresAt,
	}, nil
}

// CheckCSRF verifies the unsafe-method header for a cookie-authenticated
// request. Bearer requests are exempt; see VerifyToken.
func (s *Service) CheckCSRF(id *Identity, headerToken, origin, referer, host string) error {
	if id == nil || !id.IsCookie() {
		return nil
	}
	if headerToken == "" {
		return ErrInvalidCSRF
	}
	if id.Session == nil {
		return ErrInvalidCSRF
	}
	if !ConstantTimeEqualHex(DigestSecret(headerToken), id.Session.CSRFHash) {
		return ErrInvalidCSRF
	}
	if !OriginAllowed(origin, referer, host) {
		return ErrInvalidOrigin
	}
	return nil
}

// OriginAllowed implements the second CSRF layer: when a browser sent an
// Origin/Referer, it must name this host. SameSite=Strict is the primary
// defence, and this catches the clients and proxies that strip or ignore it —
// while not rejecting a non-browser client (curl, a CLI, a Playwright context
// without a referer) that sends neither, which is why absence is allowed and a
// *mismatched* value is not (docs/spec/08 "State-changing calls require CSRF
// protection under cookie auth").
func OriginAllowed(origin, referer, host string) bool {
	if origin != "" {
		scheme, ohost, ok := splitURL(origin)
		if !ok {
			return false
		}
		_ = scheme
		return sameHost(ohost, host)
	}
	if referer != "" {
		scheme, rhost, ok := splitURL(referer)
		if !ok {
			return false
		}
		_ = scheme
		return sameHost(rhost, host)
	}
	return true
}

func splitURL(raw string) (scheme, host string, ok bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	// Only the two schemes this service speaks.
	i := strings.Index(raw, "://")
	if i <= 0 {
		return "", "", false
	}
	scheme, rest := raw[:i], raw[i+3:]
	j := strings.IndexAny(rest, "/?#")
	if j >= 0 {
		rest = rest[:j]
	}
	return scheme, rest, true
}

func sameHost(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	// Strip a default port the browser may or may not have included, so
	// "localhost" and "localhost:8080" compare equal when the request itself
	// arrived on that port (r.Host carries it either way).
	return a == b || a == b+":80" || a == b+":443" || b == a+":80" || b == a+":443"
}

// Logout voids the calling session. A token-authenticated caller has no session
// to log out of; revoking the token is an admin action on /users/{id}/tokens.
func (s *Service) Logout(ctx context.Context, id *Identity, remoteAddr string) error {
	if id == nil || !id.IsCookie() || id.Session == nil {
		return ErrNotPermitted
	}
	if err := s.repo.RevokeSession(ctx, id.Session.IDHash, s.Now()); err != nil {
		return fmt.Errorf("%w: revoke session: %v", ErrInternal, err)
	}
	s.audit(ctx, "logout", id.User.ID, remoteAddr, map[string]any{"username": id.User.Username})
	return nil
}

// ChangePassword is the self-service route, and the one that proves
// docs/spec/14's "stolen old session cookie invalidated": everything this
// principal holds is void, including the call's own session, which is why the
// response says re-authentication is required and clears the cookies.
func (s *Service) ChangePassword(ctx context.Context, id *Identity, current, next, remoteAddr string) (Revocations, error) {
	var out Revocations
	if id == nil || id.User == nil {
		return out, ErrNotPermitted
	}
	ok, err := VerifyPassword(current, id.User.PasswordHash)
	if err != nil || !ok {
		if err != nil {
			s.log.Error("auth: current password hash unusable", "user_id", id.User.ID, "error", err)
		}
		s.audit(ctx, "password.failure", id.User.ID, remoteAddr, map[string]any{"username": id.User.Username})
		return out, ErrInvalidCredentials
	}
	return s.rotatePassword(ctx, id.User, next, remoteAddr, id.key(), "password.change")
}

// AdminSetPassword is the admin reset (POST /api/v1/users/{id}/password). It
// does not require the old password — that is the whole point of an admin — and
// revokes everything, because an administrator resetting a credential is
// asserting that the old one may be compromised.
func (s *Service) AdminSetPassword(ctx context.Context, actor *Identity, targetID, next, remoteAddr string) (Revocations, error) {
	user, err := s.repo.UserByID(ctx, targetID)
	if err != nil {
		return Revocations{}, err
	}
	return s.rotatePassword(ctx, user, next, remoteAddr, actor.key(), "password.admin_reset")
}

func (s *Service) rotatePassword(ctx context.Context, user *StoredUser, next, remoteAddr, by, event string) (Revocations, error) {
	if err := ValidatePassword(next); err != nil {
		return Revocations{}, err
	}
	hash, err := hashPasswordWith(next, s.iterations)
	if err != nil {
		return Revocations{}, fmt.Errorf("%w: hash: %v", ErrInternal, err)
	}
	report, err := s.repo.RotatePassword(ctx, PasswordRotation{
		UserID:          user.ID,
		PasswordHash:    hash,
		Now:             s.Now(),
		RevokeSessions:  true,
		RevokeAPITokens: true,
	})
	if err != nil {
		return Revocations{}, fmt.Errorf("%w: rotate: %v", ErrInternal, err)
	}
	s.audit(ctx, event, user.ID, remoteAddr, map[string]any{
		"username":         user.Username,
		"by":               by,
		"sessions_revoked": report.Sessions,
		"tokens_revoked":   report.Tokens,
	})
	s.log.Info("auth: password rotated and credentials revoked",
		"user_id", user.ID, "username", user.Username, "event", event,
		"sessions_revoked", report.Sessions, "tokens_revoked", report.Tokens)
	return report, nil
}

// SetUserDisabled withdraws a principal. Disabling revokes every credential of
// that user in the same transaction the flag is set (store.AuthStore).
func (s *Service) SetUserDisabled(ctx context.Context, actor *Identity, targetID string, disabled bool, remoteAddr string) (Revocations, error) {
	report, err := s.repo.SetUserDisabled(ctx, targetID, disabled, s.Now())
	if err != nil {
		return report, err
	}
	event := "user.enable"
	if disabled {
		event = "user.disable"
	}
	s.audit(ctx, event, targetID, remoteAddr, map[string]any{
		"by":               actor.key(),
		"sessions_revoked": report.Sessions,
		"tokens_revoked":   report.Tokens,
	})
	return report, nil
}

// CreateTokenParams is the input to CreateToken.
type CreateTokenParams struct {
	UserID string
	Name   string
	Role   Role
	// TTL of zero means "no expiry", which the CLI uses and the API exposes as
	// an explicit choice rather than a silent default.
	TTL time.Duration
}

// CreateToken mints an API token. The raw value is returned once — it is not
// recoverable from the row, so a lost token is a revoked token (docs/spec/08
// "API token for integration CLI", docs/spec/14 "no raw key" in payloads).
func (s *Service) CreateToken(ctx context.Context, actor *Identity, p CreateTokenParams, remoteAddr string) (string, *StoredToken, error) {
	owner, err := s.repo.UserByID(ctx, p.UserID)
	if err != nil {
		return "", nil, err
	}
	ownerRole, err := ParseRole(owner.Role)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	if !p.Role.Valid() {
		return "", nil, fmt.Errorf("%w: %q", ErrRoleTooHigh, p.Role)
	}
	if p.Role > ownerRole {
		return "", nil, fmt.Errorf("%w: owner is %s, asked for %s", ErrRoleTooHigh, ownerRole, p.Role)
	}
	raw, err := NewAPIToken()
	if err != nil {
		return "", nil, fmt.Errorf("%w: %v", ErrInternal, err)
	}
	now := s.Now()
	tok, err := s.repo.InsertAPIToken(ctx, TokenCreation{
		UserID:    owner.ID,
		Name:      p.Name,
		TokenHash: DigestSecret(raw),
		Role:      p.Role.String(),
		CreatedAt: now,
		ExpiresAt: func() time.Time {
			if p.TTL <= 0 {
				return time.Time{}
			}
			return now.Add(p.TTL)
		}(),
	})
	if err != nil {
		return "", nil, fmt.Errorf("%w: insert token: %v", ErrInternal, err)
	}
	s.audit(ctx, "token.create", owner.ID, remoteAddr, map[string]any{
		"token_id": tok.ID, "name": tok.Name, "role": tok.Role, "by": actor.key(),
		"expires": tok.ExpiresAt.IsZero(),
	})
	return raw, tok, nil
}

// RevokeToken voids one API token.
func (s *Service) RevokeToken(ctx context.Context, actor *Identity, userID, tokenID, remoteAddr string) error {
	if err := s.repo.RevokeAPIToken(ctx, userID, tokenID, s.Now()); err != nil {
		return err
	}
	s.audit(ctx, "token.revoke", userID, remoteAddr, map[string]any{"token_id": tokenID, "by": actor.key()})
	return nil
}

// Users lists accounts for the admin route.
func (s *Service) Users(ctx context.Context) ([]*StoredUser, error) { return s.repo.ListUsers(ctx) }

// User loads one account.
func (s *Service) User(ctx context.Context, id string) (*StoredUser, error) {
	return s.repo.UserByID(ctx, id)
}

// Tokens lists one user's API tokens (digests are not returned by the repo
// projection used for a list).
func (s *Service) Tokens(ctx context.Context, userID string) ([]*StoredToken, error) {
	return s.repo.ListAPITokens(ctx, userID)
}

// SessionsFor lists one user's sessions, for the admin inspector.
func (s *Service) SessionsFor(ctx context.Context, userID string) ([]*StoredSession, error) {
	return s.repo.ListUserSessions(ctx, userID)
}

// Prune applies retention. Login calls it opportunistically: a service that
// only pruned on a timer would still be growing the session table on an
// appliance that sees one login a week.
func (s *Service) Prune(ctx context.Context) (PruneReport, error) {
	return s.repo.PruneExpired(ctx, s.Now(), s.sessions.IdleTimeout, s.sessions.AuditRetention, s.sessions.PruneBatch)
}

// CheckRole answers an authorization decision for a principal: the shortfall
// error carries the role the caller needed, which becomes the 403 detail.
func CheckRole(id *Identity, need Access) error {
	if need.IsPublic() {
		return nil
	}
	if id.Authenticated() && need.Allows(id.Role) {
		return nil
	}
	return ErrNotPermitted
}

// ── audit helpers ───────────────────────────────────────────────────────────

// loginKeys are the two independent buckets one authentication attempt spends
// (Login explains why both exist).
func loginKeys(username, remoteAddr string) (ipKey, userKey string) {
	return "ip:" + clientIP(remoteAddr), "user:" + strings.ToLower(strings.TrimSpace(username))
}

// spend consumes one attempt from key, returning the wait the limiter demands or
// 0 when the attempt may proceed.
func (s *Service) spend(key string) time.Duration {
	allowed, wait := s.limiter.Allow(key)
	if allowed {
		return 0
	}
	return wait
}

// AttemptAllowed is the limiter for the other credential-guessing route, first
// run setup. docs/spec/08 lists rate limiting as a control and docs/spec/14
// names it under Authorization; an unrecorded throttling would be
// indistinguishable from a service that stopped seeing the attacker, so the
// refusal is audited here.
func (s *Service) AttemptAllowed(ctx context.Context, username, remoteAddr string) error {
	ipKey, userKey := loginKeys(username, remoteAddr)
	if wait := s.spend(ipKey); wait > 0 {
		s.audit(ctx, "setup.rate_limited", "", remoteAddr, map[string]any{"username": username, "scope": "address"})
		return &RateLimited{RetryAfter: wait}
	}
	if wait := s.spend(userKey); wait > 0 {
		s.audit(ctx, "setup.rate_limited", "", remoteAddr, map[string]any{"username": username, "scope": "account"})
		return &RateLimited{RetryAfter: wait}
	}
	return nil
}

// AuditEvent records one auth decision on behalf of the HTTP layer: an access
// denial and a CSRF rejection are authentication-authorization events, and the
// code that answers them is middleware rather than the service. It never
// receives a password, a token or a hash.
func (s *Service) AuditEvent(ctx context.Context, event, userID, remoteAddr string, detail map[string]any) {
	s.audit(ctx, event, userID, remoteAddr, detail)
}

// audit writes one auth event. It never receives a password or a raw token: the
// detail map is built from usernames, ids, roles and counts.
//
// A failure to audit is logged and swallowed. The alternative — failing a
// successful login because an audit insert hit a locked database — would turn a
// bookkeeping fault into an outage; docs/spec/14's audit expectation is served
// by the log line and by the fact that the same condition will be reported by
// readiness. Privileged *writes* (token creation, disable) surface the error to
// the caller through the operation that carried them.
func (s *Service) audit(ctx context.Context, event, userID, remoteAddr string, detail map[string]any) {
	raw, err := json.Marshal(detail)
	if err != nil {
		s.log.Error("auth: could not encode an audit detail", "event", event, "error", err)
		return
	}
	if err := s.repo.RecordAuthAudit(ctx, AuditRecord{
		OccurredAt: s.Now(),
		UserID:     userID,
		Event:      event,
		RemoteAddr: remoteAddr,
		Detail:     raw,
	}); err != nil {
		s.log.Error("auth: audit event not recorded",
			"event", event, "user_id", userID, "remote_addr", remoteAddr, "error", err)
	}
}

// key identifies a principal inside an audit detail without a token.
func (i *Identity) key() string {
	if i == nil || i.User == nil {
		return ""
	}
	return i.User.ID
}

func clientIP(remoteAddr string) string {
	if remoteAddr == "" {
		return "unknown"
	}
	// The bucket key is the transport address, never a forwarded header (see
	// IsLoopbackRemote for why that distinction is load-bearing here too).
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// schemeIsHTTPS reads the already-trust-gated forwarded scheme.
func schemeIsHTTPS(proto string) bool {
	return strings.EqualFold(strings.TrimSpace(proto), "https")
}

// httpHeaderNames are named here so internal/api does not invent its own
// spellings of the CSRF contract.
const (
	// HeaderCSRFToken is the request header carrying the session's CSRF token.
	HeaderCSRFToken = "X-CSRF-Token"
	// CookieSession is the HttpOnly session cookie.
	CookieSession = "gn_session"
	// CookieCSRF is the non-HttpOnly twin: a browser must be able to read it to
	// put it in a header, and reading it grants nothing on its own — the header
	// is only accepted when it hashes to the session's csrf_hash, so this is a
	// session-bound token, not the weaker double-submit cookie.
	CookieCSRF = "gn_csrf"
)

// UnsafeMethod reports whether a method may change state and therefore needs
// CSRF protection under cookie auth. GET/HEAD/OPTIONS are, per RFC 9110, the
// defined safe methods; everything else — including unknown extension methods
// — is treated as unsafe, which is the fail-closed direction.
func UnsafeMethod(method string) bool {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
