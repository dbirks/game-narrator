package api

// /api/v1/auth and /api/v1/users — credentials, sessions and account management
// (docs/spec/08 "API version and user authentication", docs/spec/14
// "Authorization", docs/spec/10 "Security defaults for LAN deployment",
// ADR-0016).
//
// What lives here and what lives in internal/auth: this file turns an HTTP
// request into a call and a result back into a status code; every rule about
// *what counts as a valid credential* lives in auth.Service, so that the CLI
// (`admin create`, `admin token`), a future config route and the API cannot
// drift into disagreeing about it.
//
// Response shapes are deliberately small. Nothing here echoes a hash, a stored
// digest or a secret that was not just minted for this very response: the raw
// session token appears in one Set-Cookie header, the CSRF token in one body and
// one non-HttpOnly cookie, and an API token in exactly one response body — the
// call that created it (docs/spec/14 "no raw key" applies to our own
// credentials, not only to provider keys).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// maxAuthBodyBytes bounds every credential request body. A login payload is a
// username plus a ≤256-byte password, so 8 KiC is already generous; without a
// cap, the one route that runs PBKDF2 (docs/spec/14 "Abuse and resource
// controls": the appliance's CPU is the scarce resource on a 4 GB target) would
// happily hash a 1 GiB "password" — a free denial-of-service that needs no
// credentials at all.
const maxAuthBodyBytes = 8 << 10

// userDTO is the wire shape of an account. There is no field that could hold a
// password hash, which is a stronger guarantee than clearing one after the fact.
type userDTO struct {
	ID                string `json:"id"`
	Username          string `json:"username"`
	Role              string `json:"role"`
	Disabled          bool   `json:"disabled"`
	CreatedAt         string `json:"created_at"`
	PasswordChangedAt string `json:"password_changed_at"`
}

func toUserDTO(u *store.User) userDTO {
	if u == nil {
		return userDTO{}
	}
	return userDTO{
		ID:                u.ID,
		Username:          u.Username,
		Role:              u.Role,
		Disabled:          u.Disabled,
		CreatedAt:         fmtTime(u.CreatedAt),
		PasswordChangedAt: fmtTime(u.PasswordChangedAt),
	}
}

type tokenDTO struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Role       string `json:"role"`
	CreatedAt  string `json:"created_at"`
	LastUsedAt string `json:"last_used_at,omitempty"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	RevokedAt  string `json:"revoked_at,omitempty"`
}

func toTokenDTO(t *store.APIToken) tokenDTO {
	return tokenDTO{
		ID:         t.ID,
		Name:       t.Name,
		Role:       t.Role,
		CreatedAt:  fmtTime(t.CreatedAt),
		LastUsedAt: fmtTime(t.LastUsedAt),
		ExpiresAt:  fmtTime(t.ExpiresAt),
		RevokedAt:  fmtTime(t.RevokedAt),
	}
}

// authRequired reports whether the credential routes can work at all. Every
// handler below answers 503 rather than pretending: a status endpoint that
// answered `{"setup_required":false}` without a database behind it would tell
// the UI that the appliance is configured when nothing has been written
// (docs/spec/13 "no generic success on an unprobed service").
func (s *Server) authRequired(w http.ResponseWriter, r *http.Request) bool {
	if s.authSvc == nil || s.users == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "auth-unavailable",
			"Authentication Unavailable",
			"no credential store is configured in this process")
		return false
	}
	return true
}

// ── GET /api/v1/auth/status ─────────────────────────────────────────────────

type authStatusResponse struct {
	// SetupRequired is true until an admin exists. The UI's first-run wizard and
	// `doctor` both key on it (docs/spec/14 "local-first, set admin on first
	// run").
	SetupRequired bool `json:"setup_required"`
	// LoopbackSetup reports whether *this* caller could run setup, which is a
	// property of where the request came from, not of the account state. The
	// wizard needs it to say "finish this from the machine itself" instead of
	// posting a form that will 403.
	LoopbackSetup bool `json:"loopback_setup"`
}

// handleAuthStatus is public. It reports whether the appliance is unclaimed and
// nothing else — no usernames, no counts, no roles.
func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	required, err := s.authSvc.Status(r.Context())
	if err != nil {
		s.authFault(w, r, "status", err)
		return
	}
	writeJSON(w, http.StatusOK, authStatusResponse{
		SetupRequired: required,
		LoopbackSetup: auth.IsLoopbackRemote(s.clientAddr(r)),
	})
}

// ── POST /api/v1/auth/setup ─────────────────────────────────────────────────

type setupRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleAuthSetup creates the first admin, and only from the machine itself
// (docs/spec/10 "Bind localhost on initial boot and expose LAN via explicit
// configuration"; docs/spec/14 "set admin on first run").
//
// The loopback test is the transport peer address and nothing else. X-Forwarded-
// For is not consulted: any host on the LAN can put an arbitrary value in it, so
// trusting it would mean the endpoint that mints the top-level credential is
// reachable by whoever reads that header first. A deployment behind a reverse
// proxy therefore claims its admin over a loopback connection to the proxy, or
// uses `game-narrator admin create`.
func (s *Server) handleAuthSetup(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	var req setupRequest
	if !s.decodeAuthBody(w, r, &req) {
		return
	}
	if err := s.authSvc.AttemptAllowed(r.Context(), req.Username, s.clientAddr(r)); err != nil {
		s.authError(w, r, err)
		return
	}
	// Deliberately *after* the rate limiter has taken a token: the loopback rule
	// is the answer we owe a remote caller even when they are being throttled,
	// and answering 429 first would hint that the endpoint would otherwise have
	// accepted them.
	if !auth.IsLoopbackRemote(s.clientAddr(r)) {
		s.writeProblem(w, r, http.StatusForbidden, "setup-not-local", "Forbidden",
			"first-run setup is only accepted from the local machine (127.0.0.0/8 or ::1); create an account with `game-narrator admin create`")
		return
	}
	user, err := s.authSvc.Setup(r.Context(), req.Username, req.Password, s.clientAddr(r), true)
	if err != nil {
		s.authError(w, r, err)
		return
	}
	s.log.Info("auth: first admin created over http", "username", user.Username)
	writeJSON(w, http.StatusCreated, map[string]any{
		"user": toUserDTO(user),
		// The account exists; no session was minted. Forcing a login means the
		// wizard cannot hold a cookie it never authenticated for, and the
		// password the operator just typed is proven known before any
		// privileged state is reachable.
		"message": "admin created; sign in with POST /api/v1/auth/login",
	})
}

// ── POST /api/v1/auth/login ─────────────────────────────────────────────────

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	User      userDTO `json:"user"`
	CSRFToken string  `json:"csrf_token"`
	ExpiresAt string  `json:"expires_at"`
	// IdleExpiresAt is the deadline the *next* activity must precede; the UI
	// shows it as a countdown rather than discovering the 12 h rule from a 401.
	IdleExpiresAt string `json:"idle_expires_at"`
}

// handleLogin mints a session. Credential failures are answered by authError;
// the uniform 401 there is what stops this being a username oracle.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	var req loginRequest
	if !s.decodeAuthBody(w, r, &req) {
		return
	}
	// A body that omits a credential entirely is a malformed request, not a
	// failed one: answering 401 would tell a client with a serialization bug that
	// its credentials were wrong. An *incorrect* password stays a 401.
	if req.Username == "" || req.Password == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "missing-credential-field", "Bad Request",
			"both username and password must be present and non-empty in the request body")
		return
	}
	grant, err := s.authSvc.Login(r.Context(), auth.LoginParams{
		Username:       req.Username,
		Password:       req.Password,
		UserAgent:      r.UserAgent(),
		RemoteAddr:     s.clientAddr(r),
		ForwardedProto: s.forwardedProto(r),
	})
	if err != nil {
		// A rejected login must not leave a half-established cookie behind.
		s.clearSessionCookies(w)
		s.authError(w, r, err)
		return
	}
	// Opportunistic retention: an appliance that sees one login a week would
	// otherwise keep expired sessions forever (docs/spec/02 bounds the
	// operational store). Failure here is not fatal and is logged by the store.
	if rep, err := s.authSvc.Prune(r.Context()); err != nil {
		s.log.Warn("auth: session retention sweep failed", "request_id", RequestIDFromContext(r.Context()), "error", err)
	} else if rep.Sessions+rep.Tokens+rep.Audit > 0 {
		s.log.Info("auth: retention sweep removed expired credentials",
			"sessions", rep.Sessions, "tokens", rep.Tokens, "audit", rep.Audit)
	}

	s.setSessionCookies(w, r, grant)
	policy := s.authSvc.Sessions()
	writeJSON(w, http.StatusOK, loginResponse{
		User:          toUserDTO(grant.User),
		CSRFToken:     grant.CSRF,
		ExpiresAt:     fmtTime(grant.ExpiresAt),
		IdleExpiresAt: fmtTime(grant.CreatedAt.Add(policy.IdleTimeout)),
	})
}

// ── POST /api/v1/auth/logout ────────────────────────────────────────────────

// handleLogout revokes the calling session and clears both cookies. Under cookie
// auth it is an unsafe method, so the CSRF middleware has already required the
// header — a cross-site page cannot log a user out (nor, more importantly,
// cannot use a logged-out session as a probe).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	id := IdentityFromContext(r.Context())
	if !id.IsCookie() {
		// A bearer credential has no session to end: the client drops the token.
		// Revoking it is an admin action on /api/v1/users/{id}/tokens, and this
		// says so instead of returning 200 for something it did not do.
		s.writeProblem(w, r, http.StatusConflict, "logout-needs-session", "Conflict",
			"an api token is not a session; revoke the token instead of logging out")
		return
	}
	if err := s.authSvc.Logout(r.Context(), id, s.clientAddr(r)); err != nil {
		s.authError(w, r, err)
		return
	}
	s.clearSessionCookies(w)
	w.WriteHeader(http.StatusNoContent)
}

// ── GET /api/v1/auth/me ─────────────────────────────────────────────────────

type meResponse struct {
	User      userDTO `json:"user"`
	CSRFToken string  `json:"csrf_token,omitempty"`
	// AuthMethod is "cookie" or "token": the difference explains whether CSRF
	// applies and is what the UI uses to decide whether to send the header at
	// all (docs/spec/08 "Conventions").
	AuthMethod string `json:"auth_method"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	// Grants is the role table this principal sits inside, so an operator can
	// see what their role means without reading source (docs/spec/14 wants
	// role ACLs to be reviewable).
	Grants map[string]string `json:"grants,omitempty"`
}

// handleMe answers "who am I, and what is my CSRF token".
//
// The CSRF token is *echoed*, not issued: the only places it is minted are
// login and setup, and it is delivered in a non-HttpOnly cookie precisely so a
// page can read it and put it in a header. Returning it here is the convenience
// that keeps a reload from looking like a logout; it is recomputed only from
// what the request already carried, and verified against the session's stored
// digest first, so a request whose cookie jar was tampered with gets "" rather
// than a token.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	id := IdentityFromContext(r.Context())
	out := meResponse{User: toUserDTO(id.User), AuthMethod: id.Method, ExpiresAt: fmtTime(id.ExpiresAt)}
	if id.Method == auth.MethodCookie && id.Session != nil {
		if c, err := r.Cookie(auth.CookieCSRF); err == nil && c.Value != "" &&
			auth.ConstantTimeEqualHex(auth.DigestSecret(c.Value), id.Session.CSRFHash) {
			out.CSRFToken = c.Value
		}
		out.Grants = map[string]string{"role": id.Role.String(), "grant": auth.GrantFor[id.Role]}
	}
	writeJSON(w, http.StatusOK, out)
}

// ── POST /api/v1/auth/password ──────────────────────────────────────────────

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword proves docs/spec/14's "stolen old session cookie
// invalidated": the change revokes every session *and* every API token of this
// user, including the one making the call, which is why the response clears the
// cookies and says re-authentication is required.
//
// Keeping the caller's own session alive would be the convenient choice and the
// wrong one: the reason an operator rotates a password is that they think the
// old one — and anything derived from it or sitting beside it — is burned.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	id := IdentityFromContext(r.Context())
	var req changePasswordRequest
	if !s.decodeAuthBody(w, r, &req) {
		return
	}
	rep, err := s.authSvc.ChangePassword(r.Context(), id, req.CurrentPassword, req.NewPassword, s.clientAddr(r))
	if err != nil {
		s.authError(w, r, err)
		return
	}
	s.clearSessionCookies(w)
	writeJSON(w, http.StatusOK, map[string]any{
		"reauth_required":     true,
		"sessions_revoked":    rep.Sessions,
		"tokens_revoked":      rep.Tokens,
		"password_changed_at": fmtTime(s.authSvc.Now()),
	})
}

// ── /api/v1/users (admin) ───────────────────────────────────────────────────

func (s *Server) handleUsersList(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	users, err := s.authSvc.Users(r.Context())
	if err != nil {
		s.authFault(w, r, "list users", err)
		return
	}
	out := make([]userDTO, 0, len(users))
	for _, u := range users {
		out = append(out, toUserDTO(u))
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": out})
}

type createUserRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *Server) handleUsersCreate(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	var req createUserRequest
	if !s.decodeAuthBody(w, r, &req) {
		return
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-role", "Bad Request", err.Error())
		return
	}
	user, err := s.authSvc.CreateUser(r.Context(), req.Username, req.Password, role,
		IdentityFromContext(r.Context()), s.clientAddr(r))
	if err != nil {
		s.authError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"user": toUserDTO(user)})
}

// userIDFromPath resolves {id}; a malformed id answers 404, the same as "no such
// user", so the endpoint cannot be used to enumerate ids (the rule
// handleJobGet follows for the same reason).
func (s *Server) userIDFromPath(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := strings.TrimSpace(r.PathValue("id"))
	if !games.ValidID(id) {
		s.writeProblem(w, r, http.StatusNotFound, "user-not-found", "Not Found",
			"no user has that identifier")
		return "", false
	}
	return id, true
}

func (s *Server) handleUserDisable(w http.ResponseWriter, r *http.Request) {
	s.setUserDisabled(w, r, true)
}

func (s *Server) handleUserEnable(w http.ResponseWriter, r *http.Request) {
	s.setUserDisabled(w, r, false)
}

// setUserDisabled withdraws or restores a principal. Disabling revokes every
// session and API token in the same transaction that sets the flag
// (store.AuthStore.SetUserDisabled), which is what makes "the operator clicked
// disable" and "the stolen cookie stops working" one event rather than two that
// might both happen.
func (s *Server) setUserDisabled(w http.ResponseWriter, r *http.Request, disabled bool) {
	if !s.authRequired(w, r) {
		return
	}
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	target, err := s.authSvc.User(r.Context(), id)
	if err != nil {
		s.userError(w, r, err)
		return
	}
	actor := IdentityFromContext(r.Context())
	if disabled && actor.User != nil && actor.User.ID == target.ID {
		// Self-disable would lock the appliance out of its own admin account
		// with no credential left to undo it through the API. The CLI remains
		// the deliberate path, because it is a local act.
		s.writeProblem(w, r, http.StatusConflict, "cannot-disable-self", "Conflict",
			"an admin may not disable the account they are signed in with; use `game-narrator admin` from the machine itself")
		return
	}
	rep, err := s.authSvc.SetUserDisabled(r.Context(), actor, target.ID, disabled, s.clientAddr(r))
	if err != nil {
		s.userError(w, r, err)
		return
	}
	fresh, err := s.authSvc.User(r.Context(), target.ID)
	if err != nil {
		s.userError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":             toUserDTO(fresh),
		"sessions_revoked": rep.Sessions,
		"tokens_revoked":   rep.Tokens,
	})
}

type setPasswordRequest struct {
	Password string `json:"password"`
}

// handleUserPasswordReset is the admin reset. It revokes everything for that
// user for the same reason a self-service change does: a reset is what you do
// when you believe the old credential is compromised.
func (s *Server) handleUserPasswordReset(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req setPasswordRequest
	if !s.decodeAuthBody(w, r, &req) {
		return
	}
	rep, err := s.authSvc.AdminSetPassword(r.Context(), IdentityFromContext(r.Context()),
		id, req.Password, s.clientAddr(r))
	if err != nil {
		s.authError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"reauth_required":  true,
		"sessions_revoked": rep.Sessions,
		"tokens_revoked":   rep.Tokens,
	})
}

// ── /api/v1/users/{id}/tokens (admin) ───────────────────────────────────────

func (s *Server) handleTokensList(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	id, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	tokens, err := s.authSvc.Tokens(r.Context(), id)
	if err != nil {
		s.userError(w, r, err)
		return
	}
	out := make([]tokenDTO, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, toTokenDTO(t))
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": out})
}

type createTokenRequest struct {
	Name string `json:"name"`
	Role string `json:"role"`
	// ExpiresInDays is optional; 0 (absent) means no expiry, which the CLI also
	// defaults to. The choice is explicit in the request rather than implicit in
	// a server-side default so a long-lived credential is always somebody's
	// deliberate decision (docs/spec/14 "Abuse and resource controls").
	ExpiresInDays int `json:"expires_in_days"`
}

func (s *Server) handleTokensCreate(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	userID, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	var req createTokenRequest
	if !s.decodeAuthBody(w, r, &req) {
		return
	}
	role, err := auth.ParseRole(req.Role)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-role", "Bad Request", err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		s.writeProblem(w, r, http.StatusBadRequest, "missing-name", "Bad Request",
			"name is required: it is the only way to tell two tokens apart before revoking one")
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-expiry", "Bad Request",
			"expires_in_days must be between 0 (no expiry) and 3650")
		return
	}
	raw, tok, err := s.authSvc.CreateToken(r.Context(), IdentityFromContext(r.Context()),
		auth.CreateTokenParams{
			UserID: userID,
			Name:   strings.TrimSpace(req.Name),
			Role:   role,
			TTL:    time.Duration(req.ExpiresInDays) * 24 * time.Hour,
		}, s.clientAddr(r))
	if err != nil {
		s.authError(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		// The only time the raw value is ever sent. It is not in the list
		// endpoint, not in the audit row, and not recoverable from the database.
		"token": raw,
		"id":    tok.ID,
		"role":  tok.Role,
		"note":  "shown once; store it as a secret and revoke it if it is lost",
	})
}

func (s *Server) handleTokenRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.authRequired(w, r) {
		return
	}
	userID, ok := s.userIDFromPath(w, r)
	if !ok {
		return
	}
	tokenID := strings.TrimSpace(r.PathValue("tid"))
	if !games.ValidID(tokenID) {
		s.writeProblem(w, r, http.StatusNotFound, "token-not-found", "Not Found",
			"no api token has that identifier")
		return
	}
	if err := s.authSvc.RevokeToken(r.Context(), IdentityFromContext(r.Context()),
		userID, tokenID, s.clientAddr(r)); err != nil {
		s.userError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revoked": true, "id": tokenID})
}

// ── cookies ────────────────────────────────────────────────────────────────

// setSessionCookies writes the pair behind a new session.
//
// gn_session is HttpOnly: a cross-site script or a sloppy template that leaks an
// XSS cannot read it, which is why the CSRF token may live in a readable cookie
// at all — the readable value grants nothing by itself, it is only accepted when
// it hashes to what the *HttpOnly* session's row stores (docs/spec/08 "Browser
// session secured with same-site httpOnly cookie").
//
// SameSite=Strict is the primary CSRF defence; the token and the Origin check
// behind it are for the clients and proxies that do not honour it.
func (s *Server) setSessionCookies(w http.ResponseWriter, r *http.Request, grant *auth.SessionGrant) {
	secure := s.cookieSecure(r)
	maxAge := int(time.Until(grant.ExpiresAt).Seconds())
	if maxAge < 1 {
		maxAge = 1
	}
	for _, c := range []*http.Cookie{
		{Name: auth.CookieSession, Value: grant.Token, HttpOnly: true},
		{Name: auth.CookieCSRF, Value: grant.CSRF, HttpOnly: false},
	} {
		c.Path = "/"
		c.Secure = secure
		c.SameSite = http.SameSiteStrictMode
		c.MaxAge = maxAge
		http.SetCookie(w, c)
	}
	// A response that carries an authentication credential must not be
	// cacheable, by a shared LAN cache or by the browser's back-forward cache
	// alike (docs/spec/14 "Trust zones").
	w.Header().Add("Vary", "Cookie")
}

// clearSessionCookies expires both cookies. Written on every path that voids a
// session — logout, password change, and a 401 naming an expired or revoked
// cookie — so a browser stops resending a credential the server has already
// declared dead instead of discovering it again on the next request.
func (s *Server) clearSessionCookies(w http.ResponseWriter) {
	for _, name := range []string{auth.CookieSession, auth.CookieCSRF} {
		http.SetCookie(w, &http.Cookie{
			Name:     name,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			Expires:  time.Unix(0, 0).UTC(),
			HttpOnly: name == auth.CookieSession,
			SameSite: http.SameSiteStrictMode,
		})
	}
}

// ── request/response plumbing ──────────────────────────────────────────────

// decodeAuthBody reads a bounded, strictly-typed JSON body. Unknown fields are
// rejected rather than ignored: a client that sends `{"passwd": "..."}` must be
// told the field name is wrong, because silently reading an empty password and
// answering "invalid credentials" turns a typo into a suspected attack.
func (s *Server) decodeAuthBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	body := http.MaxBytesReader(w, r.Body, maxAuthBodyBytes)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			s.writeProblem(w, r, http.StatusBadRequest, "missing-body", "Bad Request",
				"a JSON request body is required")
		case errors.As(err, &maxErr):
			s.writeProblem(w, r, http.StatusRequestEntityTooLarge, "body-too-large", "Request Entity Too Large",
				fmt.Sprintf("an authentication request body must be at most %d bytes", maxAuthBodyBytes))
		default:
			s.writeProblem(w, r, http.StatusBadRequest, "invalid-json", "Bad Request",
				fmt.Sprintf("the request body must be a JSON object with exactly the documented fields: %v", err))
		}
		return false
	}
	// A trailing document means the client concatenated requests; honouring the
	// first and ignoring the second is a request-smuggling shape, so it is
	// refused at the one place that mints credentials.
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		s.writeProblem(w, r, http.StatusBadRequest, "trailing-json", "Bad Request",
			"exactly one JSON object may be sent per request")
		return false
	}
	return true
}

// authError maps an auth failure onto the docs/spec/08 error contract. Detail
// text is deliberately coarse: these responses are generated before (and
// sometimes instead of) any account being established, so they must not confirm
// which usernames exist or how close a guess came.
func (s *Server) authError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		s.writeProblem(w, r, http.StatusUnauthorized, "invalid-credentials", "Unauthorized",
			"the username or password is not valid for this account")
	case errors.Is(err, auth.ErrWeakPassword):
		s.writeProblem(w, r, http.StatusBadRequest, "weak-password", "Bad Request",
			fmt.Sprintf("%v (minimum %d characters)", err, auth.MinPasswordLen))
	case errors.Is(err, auth.ErrInvalidUsername):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-username", "Bad Request", err.Error())
	case errors.Is(err, auth.ErrRoleTooHigh):
		s.writeProblem(w, r, http.StatusBadRequest, "role-too-high", "Bad Request", err.Error())
	case errors.Is(err, auth.ErrAlreadyInitialized):
		s.writeProblem(w, r, http.StatusConflict, "already-initialized", "Conflict",
			"an admin account already exists; create further accounts through POST /api/v1/users")
	case errors.Is(err, auth.ErrSetupNotLocal):
		s.writeProblem(w, r, http.StatusForbidden, "setup-not-local", "Forbidden", err.Error())
	case errors.Is(err, auth.ErrNotPermitted):
		s.writeProblem(w, r, http.StatusForbidden, "forbidden", "Forbidden",
			"this action requires a session with a sufficient role")
	case errors.Is(err, auth.ErrUnknownSession),
		errors.Is(err, auth.ErrSessionRevoked),
		errors.Is(err, auth.ErrSessionExpired),
		errors.Is(err, auth.ErrSessionIdle):
		s.unauthenticated(w, r, err)
	case errors.Is(err, auth.ErrUnknownToken):
		s.writeProblem(w, r, http.StatusUnauthorized, "invalid-token", "Unauthorized",
			"the api token is unknown, revoked or expired")
	case errors.Is(err, store.ErrNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "user-not-found", "Not Found",
			"no user or token has that identifier")
	case errors.Is(err, store.ErrConflict):
		s.writeProblem(w, r, http.StatusConflict, "account-conflict", "Conflict",
			"an account with that username already exists")
	case errors.Is(err, store.ErrInvalidRole):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-role", "Bad Request", err.Error())
	default:
		var rl *auth.RateLimited
		if errors.As(err, &rl) {
			secs := rl.RetryAfterSeconds()
			w.Header().Set("Retry-After", strconv.Itoa(secs))
			// no-store on a rate-limit answer: a shared cache that stored a 429
			// would keep answering it after the bucket refilled, extending the
			// lockout for everyone behind that cache.
			w.Header().Set("Cache-Control", "no-store")
			s.writeProblem(w, r, http.StatusTooManyRequests, "too-many-attempts", "Too Many Requests",
				fmt.Sprintf("too many authentication attempts for this address or account; retry after %d seconds", secs))
			return
		}
		s.authFault(w, r, "credential operation", err)
	}
}

// authFault is the 500 path: the error text (which may name a table, a file or a
// constraint) goes to the log with the correlation id, and the client gets a
// sentence with no detail in it. docs/spec/08's error contract asks for a
// recoverable action and a correlation id, "not raw stack trace".
func (s *Server) authFault(w http.ResponseWriter, r *http.Request, op string, err error) {
	s.log.Error("auth: operation failed",
		"request_id", RequestIDFromContext(r.Context()), "op", op, "error", err)
	s.writeProblem(w, r, http.StatusInternalServerError, "auth-fault", "Internal Server Error",
		"the credential store could not complete this request; check the server log with the request id")
}

// userError maps a user/token route failure.
func (s *Server) userError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeProblem(w, r, http.StatusNotFound, "user-not-found", "Not Found",
			"no user or token has that identifier")
	case errors.Is(err, auth.ErrInvalidUsername):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-username", "Bad Request", err.Error())
	default:
		s.authError(w, r, err)
	}
}

// auditDeny records an access denial. A privileged write by a principal without
// the role is the event docs/spec/14's acceptance tests are written against, so
// it is audited rather than merely logged.
func (s *Server) auditDeny(r *http.Request, id *auth.Identity, need auth.Access) {
	if s.authSvc == nil {
		return
	}
	s.authSvc.AuditEvent(r.Context(), "access.denied", identityUserID(id), s.clientAddr(r), map[string]any{
		"method":   r.Method,
		"path":     r.URL.Path,
		"role":     id.Role.String(),
		"required": need.Min.String(),
		"via":      id.Method,
	})
}

// auditEvent is the middleware's audit entry point (CSRF rejections).
func (s *Server) auditEvent(ctx context.Context, event string, id *auth.Identity, addr string, detail map[string]any) {
	if s.authSvc == nil {
		return
	}
	s.authSvc.AuditEvent(ctx, event, identityUserID(id), addr, detail)
}

// identityUserID is the audit-row subject: "" when no principal was
// established, which is exactly the shape of a rejection at the door.
func identityUserID(id *auth.Identity) string {
	if id == nil || id.User == nil {
		return ""
	}
	return id.User.ID
}

// ── developer-only event filtering (docs/spec/08 "Partial data redacted when
// not in developer mode") ────────────────────────────────────────────────────

// maxSSEFrameBuffer bounds the bytes held while waiting for a frame boundary.
// encodeSSEFrame writes one whole frame per call, so in practice this buffer
// holds nothing; the cap exists so a handler that ever wrote partial frames
// cannot turn one slow developer-only stream into unbounded memory.
const maxSSEFrameBuffer = 1 << 20

// developerScopedEvents wraps the SSE handler so a principal below developer
// never receives a decision.* or provider.trace frame — in replay or live.
//
// The filter is applied at the frame boundary on the way out rather than by
// rewriting the client's ?names= query into an allow-list, for two reasons. An
// allow-list would have to enumerate the event vocabulary at subscribe time, so
// a name registered by a later subsystem would silently never reach a viewer;
// and internal/events bounds a filter's name list, so the viewer stream would
// break the day somebody registers the 65th event. A deny-list over the frames
// this stream actually carries has neither failure mode, and it covers replayed
// history exactly as well as live events.
func (s *Server) developerScopedEvents(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := IdentityFromContext(r.Context())
		if id.Authenticated() && id.Role.AtLeast(auth.RoleDeveloper) {
			next(w, r)
			return
		}
		fw := &eventScopeWriter{ResponseWriter: w, log: s.log}
		next(fw, r)
		fw.drain()
		if fw.hidden > 0 {
			// The count is logged, the names are not: a stream that hides game
			// text must not leak it into a log line either (docs/spec/13 log
			// hygiene).
			s.log.Debug("sse stream suppressed developer-only events",
				"request_id", RequestIDFromContext(r.Context()),
				"role", id.Role.String(), "hidden", fw.hidden)
		}
	}
}

// eventScopeWriter filters SSE frames for a non-developer subscriber.
type eventScopeWriter struct {
	http.ResponseWriter
	log     *slog.Logger
	scoped  bool
	pending []byte
	hidden  int
}

// sseFrameSeparator is the record terminator defined by the SSE wire format;
// encodeSSEFrame emits exactly one record per Write, ending here.
var sseFrameSeparator = []byte("\n\n")

func (w *eventScopeWriter) WriteHeader(code int) {
	// Only an actual event stream is worth frame-splitting. A 503 problem+json
	// (no hub wired) or a 400 must pass through byte-for-byte, so the filter can
	// never eat an error the client needs to see.
	w.scoped = code == http.StatusOK &&
		strings.HasPrefix(w.Header().Get("Content-Type"), ContentTypeEventStream)
	w.ResponseWriter.WriteHeader(code)
}

func (w *eventScopeWriter) Write(b []byte) (int, error) {
	if !w.scoped {
		return w.ResponseWriter.Write(b)
	}
	w.pending = append(w.pending, b...)
	for {
		i := bytes.Index(w.pending, sseFrameSeparator)
		if i < 0 {
			break
		}
		frame := w.pending[:i+len(sseFrameSeparator)]
		w.pending = w.pending[i+len(sseFrameSeparator):]
		if auth.DeveloperOnlyEvent(sseEventName(frame)) {
			w.hidden++
			continue
		}
		if _, err := w.ResponseWriter.Write(frame); err != nil {
			return len(b), err
		}
	}
	if len(w.pending) > maxSSEFrameBuffer {
		// No frame boundary in 1 MiB means the writer is not emitting SSE
		// records. Deliver it rather than buffer without end, and say so: a
		// viewer could see one unparsed frame, which is recoverable, whereas a
		// silently growing buffer is an outage.
		w.log.Debug("sse frame filter crossed its buffer bound; passing bytes through",
			"bytes", len(w.pending))
		if _, err := w.ResponseWriter.Write(w.pending); err != nil {
			return len(b), err
		}
		w.pending = nil
	}
	return len(b), nil
}

func (w *eventScopeWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// drain emits whatever is left when the handler returns. In the normal case that
// is nothing; if it is not, the tail cannot be attributed to an event name, and
// dropping it would be a silent hole in a stream the client believes is
// contiguous (docs/spec/08 reserves events.resync_required for exactly that).
func (w *eventScopeWriter) drain() {
	if len(w.pending) == 0 {
		return
	}
	_, _ = w.ResponseWriter.Write(w.pending)
	w.pending = nil
	w.Flush()
}

// sseEventName reads the `event:` field of one SSE record. The field is line
// oriented and the data payload is compacted JSON (encodeSSEFrame), so the first
// match is the event name and no game text can look like one.
func sseEventName(frame []byte) string {
	for len(frame) > 0 {
		line, rest, _ := bytes.Cut(frame, []byte("\n"))
		frame = rest
		if bytes.HasPrefix(line, []byte("event:")) {
			return string(bytes.TrimSpace(line[len("event:"):]))
		}
	}
	return ""
}
