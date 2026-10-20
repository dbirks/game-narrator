package api

// Tests for the auth HTTP surface: first-run setup, login/session/CSRF/roles over
// the wire, and user/token administration (docs/spec/08 "API version and user
// authentication", docs/spec/14 "Authorization" and "Security acceptance tests",
// ADR-0016).
//
// Everything runs against a real migrated SQLite credential store. A login test
// that passed against an in-memory map would prove nothing about the digest
// columns, the cascade revocations or the CHECK constraints that make "stolen old
// session cookie invalidated" a fact rather than a claim (AGENTS.md "no
// simulation theater"); the store-level behaviour has its own tests in
// internal/store/auth_store_test.go, and internal/auth's service tests use a fake
// Repo on purpose — they are testing the rules, not the storage.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// testPassword clears the 12-character floor in internal/auth. It appears in test
// requests and is asserted absent from every response body and audit row.
const testPassword = "correct horse battery staple"

var (
	testBuild = bootstrap.BuildInfo{
		Version: "v0.0.1-test", Commit: "deadbee", GoVersion: "go1.27.2",
		GOOS: "linux", GOARCH: "arm64",
	}
	testStarted   = time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)
	discardLogger = slog.New(slog.NewJSONHandler(io.Discard, nil))
)

// testAuth is the fixture: a real store, an auth.Service on a controllable clock,
// and one account plus one API token per role. The store is a private copy of
// the package's shared migrated template (fixture_test.go, issue #15) rather
// than a per-test re-migration. Tests that need the *unclaimed*
// appliance build their own store — see TestSetupFlowClaimsTheApplianceOnce.
type testAuth struct {
	t       testing.TB
	st      *store.Store
	repo    *store.AuthStore
	svc     *auth.Service
	tokens  map[auth.Role]string
	now     time.Time
	limiter *auth.Limiter
}

func newTestAuth(t testing.TB, extra ...auth.Option) *testAuth {
	t.Helper()
	ctx := context.Background()
	// Shared migrated template (fixture_test.go, issue #15): the 16-migration
	// run happens once per package in TestMain; this test gets the same private
	// migrated store it always did, via a file copy instead of a re-migration.
	st := openTemplateStore(t)

	a := &testAuth{t: t, st: st, repo: store.NewAuthStore(st),
		now: time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)}
	// A roomy limiter by default: only the 429 test installs a small bucket, so
	// no other assertion can depend on incidental timing.
	a.limiter = auth.NewLimiter(10_000, time.Minute, a.clock())
	a.svc = auth.NewService(a.repo, append([]auth.Option{
		auth.WithClock(a.clock()),
		// Cheap KDF (see internal/auth/service_test.go for the same choice): the
		// production cost is pinned in internal/auth's password test, and a suite
		// that hashed at 600k iterations would spend minutes in login for no
		// additional coverage of the routes.
		auth.WithPasswordCost(1000),
		auth.WithLimiter(a.limiter),
		auth.WithLogger(discardLogger)}, extra...)...)

	if _, err := a.svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	a.tokens = map[auth.Role]string{
		auth.RoleAdmin: a.mintToken(a.mustUser("admin").ID, "test-admin", auth.RoleAdmin),
	}
	for _, role := range []auth.Role{auth.RoleViewer, auth.RoleCurator, auth.RoleOperator, auth.RoleDeveloper} {
		u, err := a.svc.CreateUser(ctx, role.String(), testPassword, role, nil, "test-fixture")
		if err != nil {
			t.Fatalf("seed %s account: %v", role, err)
		}
		a.tokens[role] = a.mintToken(u.ID, "test-"+role.String(), role)
	}
	return a
}

func (a *testAuth) clock() func() time.Time {
	return func() time.Time { return a.now }
}

func (a *testAuth) mintToken(userID, name string, role auth.Role) string {
	a.t.Helper()
	raw, _, err := a.svc.CreateToken(context.Background(), nil, auth.CreateTokenParams{
		UserID: userID, Name: name, Role: role,
	}, "127.0.0.1:1")
	if err != nil {
		a.t.Fatalf("mint a %s token: %v", role, err)
	}
	return raw
}

func (a *testAuth) mustUser(name string) *store.User {
	a.t.Helper()
	u, err := a.repo.UserByUsername(context.Background(), name)
	if err != nil {
		a.t.Fatalf("account %q: %v", name, err)
	}
	return u
}

// bearer returns a live API token held by an account of that role.
func (a *testAuth) bearer(role auth.Role) string { return a.tokens[role] }

// advance moves the service clock, which is how a test reaches the 12-hour idle
// timeout without sleeping through it.
func (a *testAuth) advance(d time.Duration) { a.now = a.now.Add(d) }

// handler is the server as cmd/game-narrator wires it. Requests carry exactly the
// credentials a test put on them.
func (a *testAuth) handler(opts ...Option) http.Handler {
	return New(testBuild, testStarted, discardLogger,
		append([]Option{WithAuth(a.svc, a.repo)}, opts...)...).Handler()
}

// adminHandler presents the test admin's token on any request that carried no
// credential of its own. It is the narrowest possible edit to a route test that
// predates the scope table, standing in for the browser session a real UI holds.
// The auth tests below never use it: they choose their own principal.
func (a *testAuth) adminHandler(opts ...Option) http.Handler {
	inner := a.handler(opts...)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			if _, err := r.Cookie(auth.CookieSession); err != nil {
				r.Header.Set("Authorization", "Bearer "+a.bearer(auth.RoleAdmin))
			}
		}
		inner.ServeHTTP(w, r)
	})
}

// ── request helpers ─────────────────────────────────────────────────────────

// jsonRequest builds a request with a JSON body. RemoteAddr stays httptest's
// non-loopback default on purpose: first-run setup must be asked for loopback
// explicitly rather than getting it by accident.
func jsonRequest(method, target, body string) *http.Request {
	r := httptest.NewRequest(method, target, strings.NewReader(body))
	r.Header.Set("Content-Type", ContentTypeJSON)
	return r
}

func getReq(target string) *http.Request { return httptest.NewRequest(http.MethodGet, target, nil) }

func (a *testAuth) as(role auth.Role, r *http.Request) *http.Request {
	r.Header.Set("Authorization", "Bearer "+a.bearer(role))
	return r
}

// doReq dispatches one request through a handler and returns the recorder.
func doReq(t *testing.T, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// login performs a cookie login and returns the Cookie header to send back plus
// the CSRF token from the body.
func login(t *testing.T, h http.Handler, username, password string) (cookies, csrf string) {
	t.Helper()
	rec := doReq(t, h, jsonRequest(http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login as %s = %d, want 200 (body %s)", username, rec.Code, rec.Body.String())
	}
	var body struct {
		CSRFToken string `json:"csrf_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("login body: %v (%s)", err, rec.Body.String())
	}
	if body.CSRFToken == "" {
		t.Fatal("the login response carried no CSRF token")
	}
	return cookieHeader(rec.Result().Cookies()), body.CSRFToken
}

func cookieHeader(cs []*http.Cookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}

// cookieValue pulls one cookie's value out of a Cookie header.
func cookieValue(header, name string) string {
	for _, part := range strings.Split(header, ";") {
		if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok && k == name {
			return v
		}
	}
	return ""
}

func withCookie(r *http.Request, header string) *http.Request {
	r.Header.Set("Cookie", header)
	return r
}

func decode(t *testing.T, rec *httptest.ResponseRecorder, dst any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), dst); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, rec.Body.String())
	}
}

// authedGet asks for a route as a role and returns the recorder.
func (a *testAuth) get(t *testing.T, h http.Handler, role auth.Role, target string) *httptest.ResponseRecorder {
	t.Helper()
	return doReq(t, h, a.as(role, getReq(target)))
}

// ── public surface ─────────────────────────────────────────────────────────

// TestPublicRoutesAnswerWithoutCredentials pins docs/spec/08's minimum public
// set — and that nothing wider leaked in. The event stream and the job queue are
// the two routes an earlier milestone served unauthenticated on the
// localhost-bind assumption; docs/spec/14 no longer allows that.
func TestPublicRoutesAnswerWithoutCredentials(t *testing.T) {
	h := newTestAuth(t).handler(WithWebAssets(uiBuild()))
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/health/live"},
		{http.MethodGet, "/api/v1/health/ready"},
		{http.MethodGet, "/api/v1/system/info"},
		{http.MethodGet, "/api/v1/auth/status"},
	} {
		rec := doReq(t, h, httptest.NewRequest(tc.method, tc.target, nil))
		if rec.Code >= 400 {
			t.Errorf("%s %s = %d without credentials, want a public answer (body %s)",
				tc.method, tc.target, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, "/api/v1/events"},
		{http.MethodGet, "/api/v1/jobs"},
		{http.MethodGet, "/api/v1/auth/me"},
		{http.MethodGet, "/api/v1/users"},
		{http.MethodGet, "/api/v1/dev/screenshots"},
		{http.MethodPost, "/api/v1/sessions"},
	} {
		rec := doReq(t, h, jsonRequest(tc.method, tc.target, `{}`))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s = %d without credentials, want 401 (body %s)", tc.method, tc.target, rec.Code, rec.Body.String())
			continue
		}
		requireContentType(t, rec, ContentTypeProblem)
		if id := rec.Header().Get(HeaderRequestID); id == "" {
			t.Errorf("%s %s: a 401 with no correlation id cannot be traced (docs/spec/08)", tc.method, tc.target)
		}
	}
	// The UI stays reachable: the login form ships inside the SPA.
	for _, target := range []string{"/", "/developer", "/login"} {
		if rec := doReq(t, h, getReq(target)); rec.Code != http.StatusOK {
			t.Errorf("UI path %s = %d, want 200 — the SPA must load before anybody signs in", target, rec.Code)
		}
	}
	// A wrong method on a public route is still 405, not 401.
	if rec := doReq(t, h, jsonRequest(http.MethodPost, "/api/v1/health/live", `{}`)); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /api/v1/health/live = %d, want 405", rec.Code)
	}
}

// TestNonPublicRoutesRefuseWhenNoCredentialStoreIsWired: the answer to "nobody
// configured auth" is 503 — not an open door, and not a 401 that blames the
// caller for the operator's missing setup.
func TestNonPublicRoutesRefuseWhenNoCredentialStoreIsWired(t *testing.T) {
	h := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild())).Handler()
	for _, target := range []string{"/api/v1/jobs", "/api/v1/events", "/api/v1/users"} {
		rec := doReq(t, h, getReq(target))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503 (body %s)", target, rec.Code, rec.Body.String())
			continue
		}
		requireContentType(t, rec, ContentTypeProblem)
		if !strings.Contains(rec.Body.String(), "auth-unavailable") {
			t.Errorf("%s does not name the cause: %s", target, rec.Body.String())
		}
	}
	if rec := doReq(t, h, getReq("/api/v1/health/live")); rec.Code != http.StatusOK {
		t.Errorf("/api/v1/health/live = %d, want 200: a monitoring probe must not be held hostage to the credential question", rec.Code)
	}
}

// ── first run ──────────────────────────────────────────────────────────────

// TestSetupFlowClaimsTheApplianceOnce is docs/spec/14 "local-first, set admin on
// first run" over HTTP, including the spoofed-header case docs/spec/10's
// "Bind localhost on initial boot" exists to prevent.
func TestSetupFlowClaimsTheApplianceOnce(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	repo := store.NewAuthStore(st)
	h := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild()),
		WithAuth(auth.NewService(repo,
			auth.WithPasswordCost(1000),
			auth.WithLimiter(auth.NewLimiter(20, time.Hour, time.Now)),
			auth.WithLogger(discardLogger)), repo)).Handler()

	rec := doReq(t, h, getReq("/api/v1/auth/status"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (body %s)", rec.Code, rec.Body.String())
	}
	var status struct {
		SetupRequired bool `json:"setup_required"`
		LoopbackSetup bool `json:"loopback_setup"`
	}
	decode(t, rec, &status)
	if !status.SetupRequired {
		t.Fatalf("a database with no users reported %+v, want setup_required true", status)
	}

	// From a LAN address: refused, and nothing was created.
	rec = doReq(t, h, withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/setup",
		fmt.Sprintf(`{"username":"remote-admin","password":%q}`, testPassword)), "192.168.37.9:44130"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("setup from a LAN address = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	if has, _ := repo.HasRole(ctx, store.RoleAdmin); has {
		t.Fatal("setup from a non-loopback address created an admin")
	}

	// X-Forwarded-For claiming 127.0.0.1 must not buy an admin account.
	rec = doReq(t, h, withLoopbackSpoofed(jsonRequest(http.MethodPost, "/api/v1/auth/setup",
		fmt.Sprintf(`{"username":"spoofed","password":%q}`, testPassword))))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("setup with a spoofed X-Forwarded-For = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if has, _ := repo.HasRole(ctx, store.RoleAdmin); has {
		t.Fatal("a spoofed forwarded header created an admin: the guard is reading a header")
	}

	// Loopback: claimed, once.
	rec = doReq(t, h, withLoopback(jsonRequest(http.MethodPost, "/api/v1/auth/setup",
		fmt.Sprintf(`{"username":"admin","password":%q}`, testPassword))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("loopback setup = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); strings.Contains(body, testPassword) || strings.Contains(body, "pbkdf2") {
		t.Fatalf("the setup response carried credential material: %s", body)
	}
	if len(rec.Result().Cookies()) != 0 {
		t.Errorf("setup set cookies (%v): creating an account is not authenticating for one", rec.Result().Cookies())
	}

	rec = doReq(t, h, withLoopback(jsonRequest(http.MethodPost, "/api/v1/auth/setup",
		fmt.Sprintf(`{"username":"second","password":%q}`, testPassword))))
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup = %d, want 409", rec.Code)
	}
	decode(t, doReq(t, h, getReq("/api/v1/auth/status")), &status)
	if status.SetupRequired {
		t.Fatal("status still reports setup_required after the first admin exists")
	}

	// Every privileged route is now reachable only with a credential, and the
	// cycle closes: setup → login → me → CSRF-protected logout → dead cookie.
	if rec := doReq(t, h, getReq("/api/v1/jobs")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/jobs right after setup = %d, want 401", rec.Code)
	}
	cookies, csrfToken := login(t, h, "admin", testPassword)
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies)); rec.Code != http.StatusOK {
		t.Fatalf("me after login = %d (body %s)", rec.Code, rec.Body.String())
	}
	// The state-changing call without the CSRF header is refused...
	if rec := doReq(t, h, withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/logout", ""), cookies)); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without a CSRF header = %d, want 403", rec.Code)
	}
	// ...and honoured with it.
	r := withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/logout", ""), cookies)
	r.Header.Set(auth.HeaderCSRFToken, csrfToken)
	if rec := doReq(t, h, r); rec.Code != http.StatusNoContent {
		t.Fatalf("logout with the CSRF header = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d, want 401", rec.Code)
	}
}

func withLoopback(r *http.Request) *http.Request {
	r.RemoteAddr = "127.0.0.1:53213"
	return r
}

func withRemote(r *http.Request, addr string) *http.Request {
	r.RemoteAddr = addr
	return r
}

func withLoopbackSpoofed(r *http.Request) *http.Request {
	r.RemoteAddr = "203.0.113.7:5555"
	r.Header.Set("X-Forwarded-For", "127.0.0.1")
	r.Header.Set("X-Real-IP", "127.0.0.1")
	return r
}

// TestSetupRefusalOrder proves the location rule answers before the throttle
// would: a 429 to a remote caller hints the endpoint was otherwise open to them.
func TestSetupRefusalOrder(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	rec := doReq(t, h, withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/setup",
		fmt.Sprintf(`{"username":"someone","password":%q}`, testPassword)), "10.1.2.3:9999"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("remote setup = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "local machine") {
		t.Errorf("the 403 does not explain the loopback rule: %s", rec.Body.String())
	}
}

// ── login, me, logout ──────────────────────────────────────────────────────

func TestLoginMeLogoutRoundTrip(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()

	rec := doReq(t, h, jsonRequest(http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"username":"viewer","password":%q}`, testPassword)))
	if rec.Code != http.StatusOK {
		t.Fatalf("login = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var body struct {
		User      map[string]any `json:"user"`
		CSRFToken string         `json:"csrf_token"`
		ExpiresAt string         `json:"expires_at"`
	}
	decode(t, rec, &body)
	if body.User["role"] != "viewer" || body.User["id"] == "" {
		t.Fatalf("login user = %v", body.User)
	}
	if _, err := time.Parse(time.RFC3339, body.ExpiresAt); err != nil {
		t.Errorf("expires_at %q is not RFC3339: %v", body.ExpiresAt, err)
	}
	if _, ok := body.User["password_hash"]; ok {
		t.Error("the user payload carries a password hash")
	}

	cookies := rec.Result().Cookies()
	var session, csrf *http.Cookie
	for _, c := range cookies {
		switch c.Name {
		case auth.CookieSession:
			session = c
		case auth.CookieCSRF:
			csrf = c
		}
	}
	if session == nil {
		t.Fatal("no gn_session cookie was set")
	}
	if !session.HttpOnly {
		t.Error("gn_session is not HttpOnly: a script could read the session")
	}
	if session.SameSite != http.SameSiteStrictMode {
		t.Errorf("gn_session SameSite = %v, want Strict", session.SameSite)
	}
	if session.Path != "/" {
		t.Errorf("gn_session Path = %q, want /", session.Path)
	}
	if session.Secure {
		t.Error("gn_session was marked Secure over plain HTTP: the browser would drop it and login would look broken")
	}
	if csrf == nil {
		t.Fatal("no gn_csrf cookie was set — the SPA cannot build the header without reading it")
	}
	if csrf.HttpOnly {
		t.Error("gn_csrf is HttpOnly, so no page script can send it and every unsafe method would fail")
	}
	if csrf.Value != body.CSRFToken {
		t.Error("the CSRF cookie and the CSRF body disagree")
	}
	// The raw session value must not be recoverable from what the server sent
	// twice (belt and braces: it is only ever in one Set-Cookie header).
	if strings.Contains(fmt.Sprint(csrf), session.Value) {
		t.Error("the CSRF cookie embeds the session token")
	}

	cookie := cookieHeader(cookies)
	rec = doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookie))
	if rec.Code != http.StatusOK {
		t.Fatalf("me = %d (body %s)", rec.Code, rec.Body.String())
	}
	var me struct {
		User       map[string]any `json:"user"`
		CSRFToken  string         `json:"csrf_token"`
		AuthMethod string         `json:"auth_method"`
	}
	decode(t, rec, &me)
	if me.User["username"] != "viewer" || me.AuthMethod != auth.MethodCookie {
		t.Fatalf("me = %+v", me)
	}
	if me.CSRFToken != body.CSRFToken {
		t.Error("/auth/me did not echo the session's CSRF token — the DoD flow and the SPA both read it from here")
	}
	if strings.Contains(rec.Body.String(), testPassword) || strings.Contains(rec.Body.String(), "pbkdf2") {
		t.Fatalf("/auth/me carried credential material: %s", rec.Body.String())
	}

	// Logout without CSRF is refused; with it, succeeds and kills the cookie.
	if rec := doReq(t, h, withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/logout", ""), cookie)); rec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF = %d, want 403", rec.Code)
	}
	r := withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/logout", ""), cookie)
	r.Header.Set(auth.HeaderCSRFToken, body.CSRFToken)
	r.Header.Set("Origin", "http://"+r.Host)
	rec = doReq(t, h, r)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.MaxAge >= 0 {
			t.Errorf("logout sent cookie %s without expiring it (Max-Age=%d)", c.Name, c.MaxAge)
		}
	}
	rec = doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookie))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("me after logout = %d, want 401 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	if !strings.Contains(rec.Body.String(), "revoked") {
		t.Errorf("the 401 after logout does not say why: %s", rec.Body.String())
	}
	// A second logout of the same cookie is refused: there is no session left to
	// authenticate the request at all.
	r2 := withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/logout", ""), cookie)
	r2.Header.Set(auth.HeaderCSRFToken, body.CSRFToken)
	if rec := doReq(t, h, r2); rec.Code != http.StatusUnauthorized {
		t.Errorf("second logout = %d, want 401", rec.Code)
	}
}

// TestLoginFailuresAreUniformAndCannotEnumerateAccounts: the response for
// "no such user" must be indistinguishable from "wrong password" — the
// precondition for a targeted guess (docs/spec/14).
func TestLoginFailuresAreUniformAndCannotEnumerateAccounts(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	got := map[string]string{}
	for name, tc := range map[string][2]string{
		"wrong-password":  {"viewer", "not the password at all"},
		"unknown-account": {"nosuchuser", testPassword},
	} {
		rec := doReq(t, h, jsonRequest(http.MethodPost, "/api/v1/auth/login",
			fmt.Sprintf(`{"username":%q,"password":%q}`, tc[0], tc[1])))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%s: login = %d, want 401 (body %s)", name, rec.Code, rec.Body.String())
		}
		requireContentType(t, rec, ContentTypeProblem)
		if strings.Contains(rec.Body.String(), tc[0]) || strings.Contains(rec.Body.String(), tc[1]) {
			t.Errorf("%s: the response echoes the submitted credential: %s", name, rec.Body.String())
		}
		for _, c := range rec.Result().Cookies() {
			if c.MaxAge > 0 {
				t.Errorf("%s: a failed login issued a live cookie %s", name, c.Name)
			}
		}
		// The correlation id differs per request by design (docs/spec/08 wants it
		// on every response), so compare the parts a client could learn from.
		var doc problem
		decode(t, rec, &doc)
		got[name] = fmt.Sprintf("%s|%d|%s|%s", doc.Type, doc.Status, doc.Detail, doc.Instance)
	}
	if got["wrong-password"] != got["unknown-account"] {
		t.Errorf("the two failures answer differently, which is a username oracle:\n%s\n%s",
			got["wrong-password"], got["unknown-account"])
	}

	// Malformed bodies are the caller's schema error (400), not a credential
	// failure: a client that misspells a field must be told, or it will read its
	// own typo as an attack on the account.
	for name, body := range map[string]string{
		"missing-field": `{"username":"viewer"}`,
		"unknown-field": fmt.Sprintf(`{"username":"viewer","passwd":%q}`, testPassword),
		// An omitted or empty credential is a malformed request, not a failed one:
		// answering 401 would tell a client with a serialization bug that its
		// credentials were wrong.
		"empty-username": `{"username":"","password":"` + testPassword + `"}`,
		"empty-password": `{"username":"viewer","password":""}`,
		"not-json":       `username=viewer`,
		"array":          `["viewer"]`,
		"trailing-doc":   fmt.Sprintf(`{"username":"viewer","password":%q} {"username":"x"}`, testPassword),
	} {
		want := http.StatusBadRequest
		rec := doReq(t, h, jsonRequest(http.MethodPost, "/api/v1/auth/login", body))
		if rec.Code != want {
			t.Errorf("%s: = %d, want %d (body %s)", name, rec.Code, want, rec.Body.String())
		}
	}
	// An oversized body is refused before the KDF ever sees it: the login route is
	// the one place that runs PBKDF2, so an uncapped body is a free DoS
	// (docs/spec/14 "Abuse and resource controls").
	huge := jsonRequest(http.MethodPost, "/api/v1/auth/login",
		`{"username":"viewer","password":"`+strings.Repeat("a", 1<<20)+`"}`)
	if rec := doReq(t, h, huge); rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a 1 MiB login body = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestSessionCookieSecureFlagFollowsTheRealScheme is docs/spec/10 "HTTPS for
// LAN": the Secure attribute must track TLS or a *trusted* proxy's forwarded
// scheme, and nothing a client can invent.
func TestSessionCookieSecureFlagFollowsTheRealScheme(t *testing.T) {
	a := newTestAuth(t)
	body := fmt.Sprintf(`{"username":"viewer","password":%q}`, testPassword)
	const addr = "192.0.2.7:44000"

	rec := doReq(t, a.handler(), withHeader(withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/login", body), addr),
		"X-Forwarded-Proto", "https"))
	if sessionCookie(t, rec).Secure {
		t.Error("an untrusted X-Forwarded-Proto set the Secure flag: any LAN host could then break logins (or, read the other way, the flag proves nothing)")
	}

	hProxied := a.handler(WithTrustedProxy("192.0.2.0/24"))
	rec = doReq(t, hProxied, withHeader(withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/login", body), addr),
		"X-Forwarded-Proto", "https"))
	if c := sessionCookie(t, rec); !c.Secure {
		t.Error("a trusted proxy's X-Forwarded-Proto did not set Secure: the session would travel in the clear after TLS termination")
	}
	// A different address in the same /24 is trusted too; one outside it is not.
	rec = doReq(t, hProxied, withHeader(withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/login", body), "192.0.2.99:1"),
		"X-Forwarded-Proto", "https"))
	if !sessionCookie(t, rec).Secure {
		t.Error("an address inside the trusted CIDR was not trusted")
	}
	rec = doReq(t, hProxied, withHeader(withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/login", body), "198.51.100.9:1"),
		"X-Forwarded-Proto", "https"))
	if sessionCookie(t, rec).Secure {
		t.Error("an address outside the trusted CIDR set Secure")
	}

	// TLS on the connection needs no header at all.
	srv := httptest.NewTLSServer(a.handler())
	defer srv.Close()
	resp, err := srv.Client().Post(srv.URL+"/api/v1/auth/login", ContentTypeJSON, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("TLS login = %d", resp.StatusCode)
	}
	var found bool
	for _, c := range resp.Cookies() {
		if c.Name == auth.CookieSession {
			found = true
			if !c.Secure {
				t.Error("a session cookie over TLS was not marked Secure")
			}
		}
	}
	if !found {
		t.Error("no session cookie over TLS")
	}
}

func withHeader(r *http.Request, name, value string) *http.Request {
	r.Header.Set(name, value)
	return r
}

func sessionCookie(t *testing.T, rec *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, c := range rec.Result().Cookies() {
		if c.Name == auth.CookieSession {
			return c
		}
	}
	t.Fatalf("no %s cookie was set (%v)", auth.CookieSession, rec.Result().Cookies())
	return nil
}

// ── CSRF ───────────────────────────────────────────────────────────────────

// TestCSRFMatrix is docs/spec/08 "State-changing calls require CSRF protection
// under cookie auth" as a table. The expected pass-through status is the *route's
// own* answer (503, because no job store is wired into this fixture), which is
// what proves the gate let the request through rather than a handler inventing a
// success.
func TestCSRFMatrix(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	cookies, token := login(t, h, "operator", testPassword)
	const host = "192.0.2.10:8080"

	for _, tc := range []struct {
		name   string
		token  string
		origin string
		want   int
	}{
		{"correct token", token, "http://" + host, http.StatusServiceUnavailable},
		{"correct token, no origin header", token, "", http.StatusServiceUnavailable},
		{"missing token", "", "http://" + host, http.StatusForbidden},
		{"wrong token", "forged-forged-forged-forged", "http://" + host, http.StatusForbidden},
		{"cross-site origin", token, "http://evil.example", http.StatusForbidden},
		{"cross-port origin", token, "http://" + host + "1", http.StatusForbidden},
	} {
		r := withCookie(jsonRequest(http.MethodPost, "/api/v1/jobs/"+games.NewID()+"/cancel", ""), cookies)
		r.Host = host
		if tc.token != "" {
			r.Header.Set(auth.HeaderCSRFToken, tc.token)
		}
		if tc.origin != "" {
			r.Header.Set("Origin", tc.origin)
		}
		rec := doReq(t, h, r)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d (body %s)", tc.name, rec.Code, tc.want, rec.Body.String())
		}
		if tc.want == http.StatusForbidden {
			requireContentType(t, rec, ContentTypeProblem)
		}
	}

	// A token lifted from *another session of the same account* must fail: the
	// comparison is against this session's stored digest, which is what makes
	// this a session-bound token rather than the weaker double-submit cookie.
	_, otherToken := login(t, h, "operator", testPassword)
	r := withCookie(jsonRequest(http.MethodPost, "/api/v1/jobs/"+games.NewID()+"/cancel", ""), cookies)
	r.Header.Set(auth.HeaderCSRFToken, otherToken)
	if rec := doReq(t, h, r); rec.Code != http.StatusForbidden {
		t.Errorf("another session's CSRF token was accepted: %d (body %s)", rec.Code, rec.Body.String())
	}

	// Safe methods never need it (a CSRF-protected GET would break the back
	// button, and RFC 9110 says it must not have side effects).
	if rec := doReq(t, h, withCookie(getReq("/api/v1/jobs"), cookies)); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("GET under cookie auth without a CSRF token = %d, want the route's own 503", rec.Code)
	}

	// The rejections are audited: "who tried this" is the next question an
	// operator asks, and docs/spec/14's acceptance test is about evidence.
	rows, err := a.repo.ListAuthAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.Event] = true
	}
	if !seen["csrf.rejected"] {
		t.Errorf("CSRF rejections were not audited: %v", eventSet(seen))
	}
}

func eventSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBearerRequestsAreExemptFromCSRF pins the other half of the rule: CSRF
// applies "under cookie auth" — an Authorization header cannot be attached by a
// foreign page, so a token requirement would be ceremony.
func TestBearerRequestsAreExemptFromCSRF(t *testing.T) {
	a := newTestAuth(t)
	js := store.NewJobsStore(a.st)
	h := a.handler(WithJobs(js))
	id := seedJob(t, js, "system.noop").ID

	rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/jobs/"+id+"/cancel", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("bearer POST with no CSRF header = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	r := a.as(auth.RoleOperator, jsonRequest(http.MethodPost, "/api/v1/jobs/"+id+"/retry", ""))
	r.Header.Set("Origin", "http://evil.example")
	r.Header.Set(auth.HeaderCSRFToken, "nonsense")
	if rec := doReq(t, h, r); rec.Code == http.StatusForbidden {
		t.Errorf("a bearer request was CSRF-rejected: %s", rec.Body.String())
	}

	rec = doReq(t, h, a.as(auth.RoleViewer, getReq("/api/v1/auth/me")))
	var me struct {
		AuthMethod string `json:"auth_method"`
		CSRFToken  string `json:"csrf_token"`
	}
	decode(t, rec, &me)
	if me.AuthMethod != auth.MethodToken {
		t.Errorf("auth_method = %q, want %q", me.AuthMethod, auth.MethodToken)
	}
	if me.CSRFToken != "" {
		t.Error("a token-authenticated /auth/me handed back a CSRF token that can never be used")
	}
}

// TestBearerTokenRespectsItsOwnRole: a viewer token cannot do operator things,
// whatever the account behind it may do (docs/spec/08 "scopes", docs/spec/14
// "role ACL").
func TestBearerTokenRespectsItsOwnRole(t *testing.T) {
	a := newTestAuth(t)
	js := store.NewJobsStore(a.st)
	h := a.handler(WithJobs(js))
	id := seedJob(t, js, "system.noop").ID

	for _, tc := range []struct {
		role auth.Role
		want int
	}{
		{auth.RoleViewer, http.StatusForbidden},
		{auth.RoleCurator, http.StatusForbidden},
		{auth.RoleOperator, http.StatusOK},
		{auth.RoleDeveloper, http.StatusOK},
		{auth.RoleAdmin, http.StatusOK},
	} {
		// cancel is the write whose success does not depend on queue state (a
		// queued job is retryable only after it has failed).
		rec := doReq(t, h, a.as(tc.role, jsonRequest(http.MethodPost, "/api/v1/jobs/"+id+"/cancel", "")))
		if rec.Code != tc.want {
			t.Errorf("%s token retrying a job = %d, want %d (body %s)", tc.role, rec.Code, tc.want, rec.Body.String())
		}
		if tc.want == http.StatusForbidden {
			requireContentType(t, rec, ContentTypeProblem)
			if !strings.Contains(rec.Body.String(), "operator") {
				t.Errorf("the 403 does not name the role to ask for: %s", rec.Body.String())
			}
			if id2 := rec.Header().Get(HeaderRequestID); id2 == "" {
				t.Errorf("a 403 with no correlation id gives the user nothing to report")
			}
		}
	}
}

func TestUnknownAndForeignTokensAreRejected(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	for _, raw := range []string{"gn_NOTAREALTOKENATALL", "gn_", "gn_!!!", strings.Repeat("x", 40)} {
		r := withHeader(getReq("/api/v1/jobs"), "Authorization", "Bearer "+raw)
		if rec := doReq(t, h, r); rec.Code != http.StatusUnauthorized {
			t.Errorf("token %q = %d, want 401 (body %s)", raw[:min(20, len(raw))], rec.Code, rec.Body.String())
		}
	}
	// A non-Bearer scheme (Basic, or a stray provider key) is not an
	// authentication attempt against this API at all.
	for _, scheme := range []string{"Basic Z3Vlc3Q6Z3Vlc3Q=", "Token gn_ABCDEFGHIJ", "gn_ABCDEFGHIJ"} {
		r := withHeader(getReq("/api/v1/jobs"), "Authorization", scheme)
		if rec := doReq(t, h, r); rec.Code != http.StatusUnauthorized {
			t.Errorf("Authorization %q = %d, want 401", scheme, rec.Code)
		}
	}
}

// TestPasswordChangeOverHTTPInvalidatesTheOldCookie is docs/spec/14's stolen
// cookie acceptance test on the wire, including the CSRF gate in front of it.
func TestPasswordChangeOverHTTPInvalidatesTheOldCookie(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	cookies, token := login(t, h, "curator", testPassword)

	newPassword := "a longer replacement phrase"
	body := fmt.Sprintf(`{"current_password":%q,"new_password":%q}`, testPassword, newPassword)

	// Without CSRF: refused, so a foreign page cannot rotate a session it cannot
	// even read.
	if rec := doReq(t, h, withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/password", body), cookies)); rec.Code != http.StatusForbidden {
		t.Fatalf("password change without CSRF = %d, want 403", rec.Code)
	}
	r := withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/password", body), cookies)
	r.Header.Set(auth.HeaderCSRFToken, token)
	rec := doReq(t, h, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("password change = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var out map[string]any
	decode(t, rec, &out)
	if out["reauth_required"] != true {
		t.Errorf("response = %v, want reauth_required so the SPA shows a login form", out)
	}
	if n, ok := out["sessions_revoked"].(float64); !ok || n < 1 {
		t.Errorf("response = %v, want the number of revoked sessions as evidence", out)
	}
	if strings.Contains(rec.Body.String(), newPassword) {
		t.Error("the response echoed the new password")
	}
	// The stolen-cookie case: the old cookie is dead.
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old cookie after a password change = %d, want 401 — docs/spec/14's acceptance test", rec.Code)
	}
	// The old password no longer authenticates; the new one does.
	if rec := doReq(t, h, jsonRequest(http.MethodPost, "/api/v1/auth/login",
		fmt.Sprintf(`{"username":"curator","password":%q}`, testPassword))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old password after a change = %d, want 401", rec.Code)
	}
	fresh, _ := login(t, h, "curator", newPassword)
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), fresh)); rec.Code != http.StatusOK {
		t.Fatalf("the new password did not produce a working session: %d", rec.Code)
	}
	// A weak replacement is refused with the rule quoted, before anything moves.
	cookies2, token2 := login(t, h, "viewer", testPassword)
	r = withCookie(jsonRequest(http.MethodPost, "/api/v1/auth/password",
		fmt.Sprintf(`{"current_password":%q,"new_password":"short"}`, testPassword)), cookies2)
	r.Header.Set(auth.HeaderCSRFToken, token2)
	rec = doReq(t, h, r)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "12") {
		t.Fatalf("weak replacement password = %d (%s), want 400 naming the minimum length", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies2)); rec.Code != http.StatusOK {
		t.Fatalf("a refused password change killed the session: %d", rec.Code)
	}
}

// TestIdleAndAbsoluteSessionExpiry moves the fixture's clock through both
// deadlines (docs/spec/08's 12 h idle / 7 d absolute).
func TestIdleAndAbsoluteSessionExpiry(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	cookies, _ := login(t, h, "viewer", testPassword)

	// Activity keeps it alive: 11h59m after the last touch the session still
	// works, and that request itself becomes the new idle anchor — the idle
	// window slides, which is what "12 h idle" means as opposed to a 12 h lease.
	a.advance(11*time.Hour + 59*time.Minute)
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies)); rec.Code != http.StatusOK {
		t.Fatalf("session died before its idle window: %d (body %s)", rec.Code, rec.Body.String())
	}
	a.advance(11*time.Hour + 58*time.Minute)
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies)); rec.Code != http.StatusOK {
		t.Fatalf("the idle window did not slide with the last request: %d (body %s)", rec.Code, rec.Body.String())
	}
	// Silence past the window from the *last* touch is the deadline.
	a.advance(12*time.Hour + time.Minute)
	rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("an idle session was not rejected: %d (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "idle") {
		t.Errorf("the 401 does not name the idle deadline: %s", rec.Body.String())
	}

	// Absolute: touching it hourly cannot extend a session past 7 days.
	a2 := newTestAuth(t)
	h2 := a2.handler()
	cookies2, _ := login(t, h2, "viewer", testPassword)
	for hour := 1; hour < 24*7; hour++ {
		a2.advance(time.Hour)
		if rec := doReq(t, h2, withCookie(getReq("/api/v1/auth/me"), cookies2)); rec.Code != http.StatusOK {
			t.Fatalf("session died in hour %d of the 7-day window: %d (%s)", hour, rec.Code, rec.Body.String())
		}
	}
	a2.advance(time.Hour)
	rec = doReq(t, h2, withCookie(getReq("/api/v1/auth/me"), cookies2))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a session past its absolute deadline was accepted: %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Errorf("the 401 does not name the absolute deadline: %s", rec.Body.String())
	}
}

// ── rate limiting ─────────────────────────────────────────────────────────

// TestLoginRateLimitAnswers429WithRetryAfter is docs/spec/08's rate-limit rule
// and docs/spec/14's resource control on the one endpoint that can be used to
// guess a password.
func TestLoginRateLimitAnswers429WithRetryAfter(t *testing.T) {
	a := newTestAuth(t, auth.WithLimiter(auth.NewLimiter(3, 15*time.Minute, nil)))
	h := a.handler()

	var last *httptest.ResponseRecorder
	for i := 0; i < 6; i++ {
		r := withRemote(jsonRequest(http.MethodPost, "/api/v1/auth/login",
			`{"username":"viewer","password":"guessing the password here"}`), fmt.Sprintf("192.0.2.%d:40000", 10+i))
		last = doReq(t, h, r)
		if last.Code == http.StatusTooManyRequests {
			break
		}
	}
	if last.Code != http.StatusTooManyRequests {
		t.Fatalf("six failed logins never tripped the limiter, last = %d (%s)", last.Code, last.Body.String())
	}
	requireContentType(t, last, ContentTypeProblem)
	if !strings.Contains(last.Body.String(), "too-many-attempts") {
		t.Errorf("the 429 does not name the error code: %s", last.Body.String())
	}
	retry := last.Header().Get("Retry-After")
	if retry == "" {
		t.Fatal("a 429 with no Retry-After tells clients to invent a backoff (docs/spec/08's error contract)")
	}
	secs, err := strconv.Atoi(retry)
	if err != nil || secs <= 0 || secs > 15*60 {
		t.Errorf("Retry-After = %q, want a positive number of seconds inside the 15-minute window", retry)
	}
	if cc := last.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a 429 is cacheable (%q): a shared LAN cache would extend the lockout for everybody", cc)
	}
	// The failed attempts were audited, without the attempted password.
	rows, err := a.repo.ListAuthAudit(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	var failures, throttled int
	for _, row := range rows {
		blob := string(row.Detail) + row.UserID + row.RemoteAddr
		if strings.Contains(blob, "guessing the password") || strings.Contains(blob, testPassword) {
			t.Fatalf("an audit row contains a password: %s / %s", row.Event, row.Detail)
		}
		switch row.Event {
		case "login.failure":
			failures++
		case "login.rate_limited":
			throttled++
		}
	}
	if failures == 0 || throttled == 0 {
		t.Errorf("audit rows: %d login.failure, %d login.rate_limited — both are required evidence", failures, throttled)
	}
}
