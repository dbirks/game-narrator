package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
)

// problem is an RFC 9457 application/problem+json document. field order is
// fixed by the spec text in docs/spec/08: type, title, status, detail,
// instance, request_id.
type problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail"`
	Instance  string `json:"instance"`
	RequestID string `json:"request_id"`
}

// newRequestID returns 16 random bytes as lowercase hex. crypto/rand.Read is
// documented to always succeed; on a hypothetical failure the caller still
// gets a non-empty, distinguishable value.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unavailable"
	}
	return hex.EncodeToString(b[:])
}

// requestIDPattern bounds a client-supplied correlation id: 1-64 characters of
// a conservative safe set. Anything else is ignored and a fresh id is
// generated, so a hostile client cannot use the header to inject log content,
// inflate response headers, or forge another request's correlation key.
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// acceptRequestID returns id when it is safe to echo back, else "".
func acceptRequestID(id string) string {
	if requestIDPattern.MatchString(id) {
		return id
	}
	return ""
}

// withRequestID attaches a correlation id to every request and response, and
// logs the completed request with it. Later milestones reuse request_id as the
// correlation key in event traces and provider call logs.
func withRequestID(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := acceptRequestID(r.Header.Get(HeaderRequestID))
		if id == "" {
			id = newRequestID()
		}
		w.Header().Set(HeaderRequestID, id)
		ctx := contextWithRequestID(r.Context(), id)

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r.WithContext(ctx))

		log.Info("http request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", float64(time.Since(start).Microseconds())/1000,
			"bytes", rec.bytes,
		)
	})
}

func contextWithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// statusRecorder captures the response status and size for logging.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

// Flush keeps http.Flusher available for streaming responses (SSE).
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ── authorization middleware (docs/spec/08 "API version and user
// authentication", docs/spec/14 "Authorization", ADR-0016) ───────────────────

// HeaderCSRFToken re-exports auth.HeaderCSRFToken so handlers, tests and the
// OpenAPI task all spell the header the same way once.
const HeaderCSRFToken = auth.HeaderCSRFToken

// IdentityFromContext returns the principal authorize attached to this request,
// or nil for a public route. Handlers use it for the actor field of an audit
// row and for the developer/event-scope decisions.
func IdentityFromContext(ctx context.Context) *auth.Identity {
	id, _ := ctx.Value(identityKey).(*auth.Identity)
	return id
}

func contextWithIdentity(ctx context.Context, id *auth.Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// authorize is the single enforcement point for every /api/ route: it resolves
// the grant from the scope table, establishes the principal from a bearer token
// or a session cookie, checks the role, and applies the CSRF rule to
// cookie-authenticated unsafe methods.
//
// Three properties are deliberate:
//
//   - Non-API paths pass through untouched. The embedded SPA must load before
//     anybody logs in (docs/spec/09), and the login form is part of it; the
//     session cookie is what makes its *data* calls privileged, not the page.
//   - A path that matches no grant requires admin. routes is passed in (and
//     also held on the Server) purely so a test can prove every registered
//     route has a grant; the middleware itself consults the policy, because the
//     safe answer to "which role owns a path nobody wrote down?" must not
//     depend on whether a handler was registered yet.
//   - Authorization is answered before routing. An unauthenticated probe of an
//     unknown path gets 401, not a 404 that confirms the path is unknown —
//     route discovery is the first step of an attack on any service
//     (docs/spec/14 "Trust zones").
func (s *Server) authorize(routes routeTable, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, apiRoot) {
			next.ServeHTTP(w, r)
			return
		}

		need, known := s.policy.Require(r.Method, r.URL.Path)
		if need.IsPublic() {
			next.ServeHTTP(w, r)
			return
		}
		if !known {
			// No grant named this verb on this path. When the path itself is
			// public for some other verb, the router's 405 is the honest answer
			// (docs/spec/08); when the path is unknown entirely, the fail-closed
			// admin default applies and the probe learns nothing.
			if pathAccess, pathKnown := s.policy.RequireAnyMethod(r.URL.Path); pathKnown && pathAccess.IsPublic() {
				next.ServeHTTP(w, r)
				return
			}
		}
		if s.authSvc == nil {
			// Honest degraded mode, and the same shape as every other unwired
			// subsystem: 503, not 200 and not an accidental 401 that suggests the
			// caller's credentials are wrong.
			s.writeProblem(w, r, http.StatusServiceUnavailable, "auth-unavailable",
				"Authentication Unavailable",
				"no credential store is configured in this process; run `game-narrator admin create` and start serve with the database it wrote")
			return
		}

		id, err := s.identify(r)
		if err != nil {
			s.unauthenticated(w, r, err)
			return
		}
		if !need.Allows(id.Role) {
			// 403, not 401: the credentials were good, the scope was not, and the
			// distinction is what tells a UI to show "ask an operator" instead of
			// a login form that will fail the same way.
			s.forbidden(w, r, need, id, known)
			return
		}
		if id.IsCookie() && auth.UnsafeMethod(r.Method) {
			if err := s.authSvc.CheckCSRF(id, r.Header.Get(HeaderCSRFToken),
				r.Header.Get("Origin"), r.Header.Get("Referer"), r.Host); err != nil {
				s.csrfRejected(w, r, id, err)
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(contextWithIdentity(r.Context(), id)))
	})
}

// identify resolves the principal. An Authorization: Bearer header wins over a
// cookie and is never fallback-repaired by one: a caller that presented a
// credential and was refused must not silently be served as a different
// principal because a browser also had a session cookie in the jar.
func (s *Server) identify(r *http.Request) (*auth.Identity, error) {
	if raw := bearerToken(r); raw != "" {
		return s.authSvc.VerifyToken(r.Context(), raw)
	}
	c, err := r.Cookie(auth.CookieSession)
	if err != nil || c.Value == "" {
		return nil, auth.ErrUnknownSession
	}
	return s.authSvc.VerifySession(r.Context(), c.Value)
}

// bearerToken extracts the `Authorization: Bearer gn_…` credential.
func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if h == "" {
		return ""
	}
	scheme, rest, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

// unauthenticated answers 401 with the reason a client can act on, and clears a
// cookie the server has just declared dead so the browser stops resending it.
func (s *Server) unauthenticated(w http.ResponseWriter, r *http.Request, err error) {
	slug, detail := "unauthenticated", "a session cookie or an API token is required"
	switch {
	case errors.Is(err, auth.ErrSessionRevoked):
		slug, detail = "session-revoked", "this session was revoked — sign in again"
	case errors.Is(err, auth.ErrSessionExpired):
		slug, detail = "session-expired", "this session passed its absolute deadline — sign in again"
	case errors.Is(err, auth.ErrSessionIdle):
		slug, detail = "session-idle", "this session was idle too long — sign in again"
	case errors.Is(err, auth.ErrUnknownToken):
		slug, detail = "invalid-token", "the api token is unknown, revoked or expired"
	case errors.Is(err, auth.ErrInternal):
		s.log.Error("auth: credential check failed internally",
			"request_id", RequestIDFromContext(r.Context()), "error", err)
		s.writeProblem(w, r, http.StatusInternalServerError, "auth-fault",
			"Internal Server Error",
			"the credential store could not answer; no authorization decision was made")
		return
	}
	if slug != "unauthenticated" && slug != "invalid-token" {
		s.clearSessionCookies(w)
	}
	w.Header().Set("Cache-Control", "no-store")
	s.writeProblem(w, r, http.StatusUnauthorized, slug, "Unauthorized", detail)
}

// forbidden answers 403 and records the attempt: an unauthorized caller trying
// privileged routes is exactly the signal docs/spec/14's acceptance tests look
// for, and it has to be visible without reproducing the request.
func (s *Server) forbidden(w http.ResponseWriter, r *http.Request, need auth.Access, id *auth.Identity, known bool) {
	reason := need.Reason
	if reason == "" {
		reason = "the scope table grants this route to " + need.Min.String() + " and above"
	}
	if !known {
		reason = "no grant matches this path, so the fail-closed default requires admin"
	}
	s.log.Warn("auth: access denied",
		"request_id", RequestIDFromContext(r.Context()),
		"user", id.Username(), "role", id.Role, "method", r.Method, "path", r.URL.Path,
		"required", need.Min,
	)
	s.auditDeny(r, id, need)
	w.Header().Set("Cache-Control", "no-store")
	s.writeProblem(w, r, http.StatusForbidden, "forbidden", "Forbidden",
		fmt.Sprintf("role %s may not call %s %s: %s", id.Role, r.Method, r.URL.Path, reason))
}

// csrfRejected answers the CSRF rule. It is a 403 problem+json and an audit
// event, never a silent pass: a browser extension or a stale SPA build hitting
// this is the signal that the Origin/Referer fallback is doing real work.
func (s *Server) csrfRejected(w http.ResponseWriter, r *http.Request, id *auth.Identity, err error) {
	slug, detail := "csrf-check-failed", err.Error()
	if errors.Is(err, auth.ErrInvalidOrigin) {
		slug, detail = "origin-mismatch",
			"the Origin or Referer header does not match this host"
	}
	s.log.Warn("auth: csrf rejection",
		"request_id", RequestIDFromContext(r.Context()),
		"user", id.Username(), "method", r.Method, "path", r.URL.Path, "reason", slug)
	s.auditEvent(r.Context(), "csrf.rejected", id, s.clientAddr(r), map[string]any{
		"method": r.Method, "path": r.URL.Path, "reason": slug,
	})
	w.Header().Set("Cache-Control", "no-store")
	s.writeProblem(w, r, http.StatusForbidden, slug, "Forbidden", detail)
}

// clientAddr returns the transport peer address. Nothing in the authorization
// path reads X-Forwarded-For: it is a client-supplied string, and the two
// decisions that use this value (rate-limit bucketing and loopback-only setup)
// are both the ones docs/spec/14 and docs/spec/10 name as unspoofable.
func (s *Server) clientAddr(r *http.Request) string {
	if r.RemoteAddr == "" {
		return "unknown"
	}
	return r.RemoteAddr
}

// cookieSecure decides the Secure flag: TLS on this connection, or an
// X-Forwarded-Proto hint from an address this deployment declared a trusted
// proxy. Anything else is treated as plain HTTP, which is the honest answer for
// the default localhost bind and the only one that cannot be forged by the
// caller (docs/spec/10 "HTTPS for LAN", docs/spec/14 "Authorization").
func (s *Server) cookieSecure(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if !strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https") {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		return false
	}
	for _, block := range s.trustedProxies {
		if block.Contains(ip) {
			return true
		}
	}
	return false
}

// forwardedProto returns the scheme hint only when it may be believed, for the
// cookie decision inside auth.Service.Login.
func (s *Server) forwardedProto(r *http.Request) string {
	if s.cookieSecure(r) {
		return "https"
	}
	return ""
}
