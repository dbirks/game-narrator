package auth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/store"
)

// fakeClock lets expiry be tested at second scale. docs/spec/08's 12 h idle and
// 7 d absolute deadlines are acceptance criteria; a test that slept to reach them
// would not be run, and one that asserted nothing would be theatre.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time          { return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.now = c.now.Add(d) }

func newTestService(t *testing.T, opts ...Option) (*Service, *fakeRepo, *fakeClock) {
	t.Helper()
	repo := newFakeRepo()
	clock := &fakeClock{now: time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)}
	base := []Option{
		WithClock(clock.Now),
		// Cheap KDF: these tests are about the state machine, and password_test.go
		// pins the production cost. 1000 iterations is ~0.4 ms instead of ~240 ms.
		WithPasswordCost(1000),
		WithLimiter(NewLimiter(1000, time.Minute, clock.Now)),
	}
	svc := NewService(repo, append(base, opts...)...)
	return svc, repo, clock
}

func TestSetupCreatesTheFirstAdminFromLoopbackOnly(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newTestService(t)

	required, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !required {
		t.Fatal("a service with no users must report setup_required")
	}

	// Off-loopback: refused, and nothing was created.
	if _, err := svc.Setup(ctx, "admin", testPassword, "192.168.1.20:52000", false); !errors.Is(err, ErrSetupNotLocal) {
		t.Fatalf("remote Setup error = %v, want ErrSetupNotLocal", err)
	}
	required, _ = svc.Status(ctx)
	if !required {
		t.Fatal("a refused remote setup still created an admin")
	}
	if ev := repo.auditEvents(); len(ev) == 0 || ev[len(ev)-1] != "setup.refused" {
		t.Fatalf("a refused setup was not audited: %v", ev)
	}

	user, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:52000", true)
	if err != nil {
		t.Fatalf("loopback Setup: %v", err)
	}
	if user.Role != RoleAdmin.String() {
		t.Fatalf("first user role = %q, want admin", user.Role)
	}
	if required, _ := svc.Status(ctx); required {
		t.Fatal("setup_required was still true after the first admin")
	}

	// Once claimed, the endpoint is closed — forever, and including from the
	// machine itself (docs/spec/14 "set admin on first run" is a bootstrap, not a
	// recovery path; `admin create` is the recovery path).
	if _, err := svc.Setup(ctx, "second", testPassword, "127.0.0.1:52000", true); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("second Setup error = %v, want ErrAlreadyInitialized", err)
	}
	// A disabled admin keeps the endpoint closed: otherwise disabling the account
	// you just made would hand the appliance back to whoever is listening.
	if _, err := svc.SetUserDisabled(ctx, nil, user.ID, true, "127.0.0.1:52000"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Setup(ctx, "attacker", testPassword, "127.0.0.1:52000", true); !errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("Setup after disabling the only admin = %v, want ErrAlreadyInitialized", err)
	}
}

func TestSetupRejectsWeakCredentials(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", "short", "127.0.0.1:1", true); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("Setup with a 5-character password = %v, want ErrWeakPassword", err)
	}
	if _, err := svc.Setup(ctx, "a!", testPassword, "127.0.0.1:1", true); !errors.Is(err, ErrInvalidUsername) {
		t.Fatalf("Setup with a bad username = %v, want ErrInvalidUsername", err)
	}
	// The password must not appear in an error string, however the failure is
	// phrased (AGENTS.md: never log passwords).
	_, err := svc.Setup(ctx, "admin", strings.Repeat("x", 300), "127.0.0.1:1", true)
	if err == nil || strings.Contains(err.Error(), strings.Repeat("x", 40)) {
		t.Fatalf("the long password was echoed back in %v", err)
	}
}

func TestLoginMintsSessionAndVerifiesIt(t *testing.T) {
	ctx := context.Background()
	svc, repo, clock := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}

	grant, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1", UserAgent: "curl/8"})
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if grant.Token == "" || grant.CSRF == "" {
		t.Fatal("a successful login returned no credential")
	}
	if len(grant.Token) != 43 {
		t.Fatalf("session token length = %d", len(grant.Token))
	}
	if grant.ExpiresAt.Sub(grant.CreatedAt) != DefaultSessionPolicy().AbsoluteTTL {
		t.Fatalf("absolute lifetime = %s, want %s", grant.ExpiresAt.Sub(grant.CreatedAt), DefaultSessionPolicy().AbsoluteTTL)
	}
	// The stored rows are digests.
	if _, err := repo.SessionByHash(ctx, grant.Token); !errors.Is(err, ErrNoSuchUser) {
		t.Fatalf("looking a session up by its raw token succeeded — the store must hold digests only: %v", err)
	}

	id, err := svc.VerifySession(ctx, grant.Token)
	if err != nil {
		t.Fatalf("VerifySession: %v", err)
	}
	if id.Role != RoleAdmin || id.User.Username != "admin" || !id.IsCookie() {
		t.Fatalf("identity = %+v", id)
	}
	if id.Session.CSRFHash != DigestSecret(grant.CSRF) {
		t.Fatal("the CSRF token handed to the client does not hash to the session's stored digest")
	}
	// Cookie auth must not claim to be a token.
	if id.Token != nil {
		t.Fatal("a session identity carried an api token")
	}

	// A second login is a second, independent session.
	other, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:2"})
	if err != nil {
		t.Fatal(err)
	}
	if other.Token == grant.Token {
		t.Fatal("two logins minted the same session token")
	}
	if err := svc.Logout(ctx, id, "127.0.0.1:1"); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	if _, err := svc.VerifySession(ctx, grant.Token); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the logged-out cookie still verified: %v", err)
	}
	if _, err := svc.VerifySession(ctx, other.Token); err != nil {
		t.Fatalf("logout revoked a different session: %v", err)
	}
	if _, err := svc.VerifySession(ctx, "unknown-cookie-value"); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("a never-issued cookie = %v, want ErrUnknownSession", err)
	}
	if _, err := svc.VerifySession(ctx, ""); !errors.Is(err, ErrUnknownSession) {
		t.Fatalf("an empty cookie = %v, want ErrUnknownSession", err)
	}

	// Idle touching: the session clock moves, so the deadline is when the *last
	// activity* happened + 12 h, not login + 12 h.
	clock.Advance(time.Hour)
	if _, err := svc.VerifySession(ctx, other.Token); err != nil {
		t.Fatal(err)
	}
	sess, err := repo.SessionByHash(ctx, DigestSecret(other.Token))
	if err != nil {
		t.Fatal(err)
	}
	if !sess.LastSeenAt.Equal(clock.now) {
		t.Fatalf("last_seen_at = %s, want the request time %s", sess.LastSeenAt, clock.now)
	}
}

func TestSessionIdleAndAbsoluteExpiry(t *testing.T) {
	ctx := context.Background()
	svc, _, clock := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	grant, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}

	// Idle: activity keeps it alive, and each accepted request *is* the new anchor
	// — that is what distinguishes a 12 h idle timeout from a 12 h lease.
	clock.Advance(11*time.Hour + 59*time.Minute)
	if _, err := svc.VerifySession(ctx, grant.Token); err != nil {
		t.Fatalf("session died 1 minute early: %v", err)
	}
	clock.Advance(11*time.Hour + 58*time.Minute)
	if _, err := svc.VerifySession(ctx, grant.Token); err != nil {
		t.Fatalf("the idle window did not slide with the last request: %v", err)
	}
	clock.Advance(12*time.Hour + time.Minute) // past the window from the last touch
	if _, err := svc.VerifySession(ctx, grant.Token); !errors.Is(err, ErrSessionIdle) {
		t.Fatalf("idle session = %v, want ErrSessionIdle", err)
	}

	// Absolute: touching it cannot extend past 7 days from creation.
	grant2, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	for hour := 1; hour < 24*7; hour++ {
		clock.Advance(time.Hour)
		if _, err := svc.VerifySession(ctx, grant2.Token); err != nil {
			t.Fatalf("session died on hour %d of the %d-day window: %v", hour, 7, err)
		}
	}
	clock.Advance(2 * time.Hour)
	if _, err := svc.VerifySession(ctx, grant2.Token); !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("session past 7 days = %v, want ErrSessionExpired", err)
	}
}

// TestPasswordChangeRevokesEverythingIs docs/spec/14's "stolen old session
// cookie invalidated", asserted for every credential the account holds.
func TestPasswordChangeRevokesEverything(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newTestService(t)
	admin, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true)
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.VerifySession(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	rawToken, _, err := svc.CreateToken(ctx, id, CreateTokenParams{UserID: admin.ID, Name: "ci", Role: RoleAdmin}, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if _, err := svc.VerifyToken(ctx, rawToken); err != nil {
		t.Fatalf("the new token does not work: %v", err)
	}

	rep, err := svc.ChangePassword(ctx, id, testPassword, "an entirely new passphrase", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("ChangePassword: %v", err)
	}
	if rep.Sessions != 1 || rep.Tokens != 1 {
		t.Fatalf("revoked %+v, want one session and one token", rep)
	}
	if _, err := svc.VerifySession(ctx, session.Token); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("the old cookie survived a password change: %v", err)
	}
	if _, err := svc.VerifyToken(ctx, rawToken); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("the old api token survived a password change: %v", err)
	}
	// And the new password works, once.
	if _, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("the old password still logged in: %v", err)
	}
	if _, err := svc.Login(ctx, LoginParams{Username: "admin", Password: "an entirely new passphrase", RemoteAddr: "127.0.0.1:1"}); err != nil {
		t.Fatalf("the new password did not log in: %v", err)
	}

	// The audit trail records the act and its reach — read here, before the
	// follow-up logins below add newer rows of their own.
	var last store.AuthAuditParams
	for _, a := range repo.audits {
		if a.Event == "password.change" {
			last = a
		}
	}
	if last.Event != "password.change" {
		t.Fatalf("audits = %v, want a password.change row", repo.auditEvents())
	}
	if !strings.Contains(string(last.Detail), "\"sessions_revoked\":1") ||
		!strings.Contains(string(last.Detail), "\"tokens_revoked\":1") {
		t.Fatalf("the password.change row does not record what it revoked: %s", last.Detail)
	}
	for _, a := range repo.audits {
		blob := string(a.Detail) + a.UserID + a.Event
		if strings.Contains(blob, "entirely new passphrase") || strings.Contains(blob, testPassword) {
			t.Fatalf("audit row %s contains a password: %s", a.Event, a.Detail)
		}
	}
}

func TestChangePasswordRequiresTheCurrentOne(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	grant, _ := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	id, err := svc.VerifySession(ctx, grant.Token)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChangePassword(ctx, id, "wrong current password", "an entirely new passphrase", "127.0.0.1:1"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("ChangePassword with a wrong current password = %v", err)
	}
	// The session is still alive: a failed change must not be a lockout.
	if _, err := svc.VerifySession(ctx, grant.Token); err != nil {
		t.Fatalf("a failed password change killed the session: %v", err)
	}
	// A too-short new password is refused before anything is written.
	if _, err := svc.ChangePassword(ctx, id, testPassword, "short", "127.0.0.1:1"); !errors.Is(err, ErrWeakPassword) {
		t.Fatalf("ChangePassword with a weak new password = %v", err)
	}
	// Admin reset needs no old password and revokes everything too.
	rep, err := svc.AdminSetPassword(ctx, id, id.User.ID, "rotated by the admin", "127.0.0.1:1")
	if err != nil {
		t.Fatalf("AdminSetPassword: %v", err)
	}
	if rep.Sessions != 1 {
		t.Fatalf("admin reset revoked %+v, want the one live session", rep)
	}
	if _, err := svc.VerifySession(ctx, grant.Token); err == nil {
		t.Fatal("the session survived an admin password reset")
	}
}

// TestDisabledUserLosesEveryCredential is the other half of "instantly
// withdraw a principal" (docs/spec/14 role ACL).
func TestDisabledUserLosesEveryCredential(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	curator, err := svc.CreateUser(ctx, "curator", testPassword, RoleCurator, nil, "cli")
	if err != nil {
		t.Fatal(err)
	}
	session, err := svc.Login(ctx, LoginParams{Username: "curator", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.VerifySession(ctx, session.Token)
	if err != nil {
		t.Fatal(err)
	}
	rawToken, tok, err := svc.CreateToken(ctx, id, CreateTokenParams{UserID: curator.ID, Name: "import-bot", Role: RoleCurator}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetUserDisabled(ctx, nil, curator.ID, true, "127.0.0.1:1"); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := svc.VerifySession(ctx, session.Token); !errors.Is(err, ErrSessionRevoked) {
		t.Fatalf("a disabled user's cookie still worked: %v", err)
	}
	if _, err := svc.VerifyToken(ctx, rawToken); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("a disabled user's token still worked: %v", err)
	}
	if _, err := svc.Login(ctx, LoginParams{Username: "curator", Password: testPassword, RemoteAddr: "127.0.0.1:1"}); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("a disabled user could log in: %v", err)
	}
	if ev := repo.auditEvents(); !contains(ev, "user.disable") {
		t.Fatalf("the disable was not audited: %v", ev)
	}

	// Re-enabling does not resurrect the revoked credentials.
	if _, err := svc.SetUserDisabled(ctx, nil, curator.ID, false, "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifySession(ctx, session.Token); err == nil {
		t.Fatal("re-enabling the user re-enabled their old session")
	}
	if _, err := svc.VerifyToken(ctx, rawToken); err == nil {
		t.Fatal("re-enabling the user re-enabled their old token")
	}
	fresh, err := svc.Login(ctx, LoginParams{Username: "curator", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("an re-enabled user cannot log in: %v", err)
	}
	if _, err := svc.VerifySession(ctx, fresh.Token); err != nil {
		t.Fatalf("the new session does not work: %v", err)
	}
	_ = tok
}

func TestAPITokenRoleCeilingAndExpiry(t *testing.T) {
	ctx := context.Background()
	svc, _, clock := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	adminGrant, _ := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	admin, err := svc.VerifySession(ctx, adminGrant.Token)
	if err != nil {
		t.Fatal(err)
	}

	// A token may not exceed its owner.
	if _, _, err := svc.CreateToken(ctx, admin, CreateTokenParams{UserID: admin.User.ID, Name: "x", Role: RoleAdmin + 1}, "127.0.0.1:1"); !errors.Is(err, ErrRoleTooHigh) {
		t.Fatalf("an out-of-range role = %v, want ErrRoleTooHigh", err)
	}
	// A viewer cannot mint an operator token, even through the service API.
	viewerUser, err := svc.CreateUser(ctx, "viewer", testPassword, RoleViewer, admin, "cli")
	if err != nil {
		t.Fatal(err)
	}
	viewerGrant, _ := svc.Login(ctx, LoginParams{Username: "viewer", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	viewer, _ := svc.VerifySession(ctx, viewerGrant.Token)
	if _, _, err := svc.CreateToken(ctx, viewer, CreateTokenParams{UserID: viewerUser.ID, Name: "escalation", Role: RoleOperator}, "127.0.0.1:1"); !errors.Is(err, ErrRoleTooHigh) {
		t.Fatalf("a viewer minted an operator token: %v", err)
	}
	// Same-role is fine.
	raw, tok, err := svc.CreateToken(ctx, viewer, CreateTokenParams{UserID: viewerUser.ID, Name: "readonly", Role: RoleViewer}, "127.0.0.1:1")
	if err != nil {
		t.Fatalf("CreateToken: %v", err)
	}
	if !strings.HasPrefix(raw, APITokenPrefix) {
		t.Fatalf("token %q is not in the documented gn_ form", raw)
	}
	id, err := svc.VerifyToken(ctx, raw)
	if err != nil {
		t.Fatalf("VerifyToken: %v", err)
	}
	if id.Role != RoleViewer || id.Method != MethodToken || id.IsCookie() {
		t.Fatalf("token identity = %+v", id)
	}
	// A token is exempt from CSRF by construction: there is no session, so there
	// is no CheckCSRF to pass.
	if err := svc.CheckCSRF(id, "", "", "", "127.0.0.1:1"); err != nil {
		t.Fatalf("CheckCSRF applied to a bearer identity: %v", err)
	}

	// The effective role follows the owner down, immediately.
	if _, err := svc.SetUserDisabled(ctx, admin, viewerUser.ID, true, "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.VerifyToken(ctx, raw); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("a disabled owner's token still authenticated: %v", err)
	}
	if err := svc.RevokeToken(ctx, admin, viewerUser.ID, tok.ID, "127.0.0.1:1"); err != nil {
		t.Fatalf("RevokeToken: %v", err)
	}

	// Expiry.
	expiring, _, err := svc.CreateToken(ctx, admin, CreateTokenParams{UserID: admin.User.ID, Name: "temp", Role: RoleOperator, TTL: time.Hour}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(2 * time.Hour)
	if _, err := svc.VerifyToken(ctx, expiring); !errors.Is(err, ErrUnknownToken) {
		t.Fatalf("an expired token authenticated: %v", err)
	}
	// A malformed or foreign bearer value never reaches the store.
	for _, bad := range []string{"", "sk-abcdefghij0123456789", "gn_!!!", "Bearer gn_ABCD"} {
		if _, err := svc.VerifyToken(ctx, bad); !errors.Is(err, ErrUnknownToken) {
			t.Fatalf("VerifyToken(%q) = %v, want ErrUnknownToken", trunc(bad, 20), err)
		}
	}
}

func TestLogoutOfATokenIdentityIsRefused(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	admin, _ := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	id, _ := svc.VerifySession(ctx, admin.Token)
	raw, _, err := svc.CreateToken(ctx, id, CreateTokenParams{UserID: id.User.ID, Name: "cli", Role: RoleAdmin}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	tokID, err := svc.VerifyToken(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, tokID, "127.0.0.1:1"); !errors.Is(err, ErrNotPermitted) {
		t.Fatalf("Logout on a token identity = %v, want ErrNotPermitted", err)
	}
	// It is still usable: a refused logout must not have revoked anything.
	if _, err := svc.VerifyToken(ctx, raw); err != nil {
		t.Fatalf("the refused logout revoked the token: %v", err)
	}
}

func TestCSRFCheck(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	grant, _ := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	id, err := svc.VerifySession(ctx, grant.Token)
	if err != nil {
		t.Fatal(err)
	}
	host := "127.0.0.1:8080"

	// Correct token, same origin.
	if err := svc.CheckCSRF(id, grant.CSRF, "http://"+host, "", host); err != nil {
		t.Fatalf("a correct CSRF token was rejected: %v", err)
	}
	// Missing.
	if err := svc.CheckCSRF(id, "", "http://"+host, "", host); !errors.Is(err, ErrInvalidCSRF) {
		t.Fatalf("a missing CSRF token = %v, want ErrInvalidCSRF", err)
	}
	// Wrong (guessed / stolen from another session).
	if err := svc.CheckCSRF(id, "not-the-token", "http://"+host, "", host); !errors.Is(err, ErrInvalidCSRF) {
		t.Fatalf("a wrong CSRF token = %v, want ErrInvalidCSRF", err)
	}
	other, _ := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:2"})
	if err := svc.CheckCSRF(id, other.CSRF, "http://"+host, "", host); !errors.Is(err, ErrInvalidCSRF) {
		t.Fatal("another session's CSRF token was accepted — the token must be bound to the session, not merely present (double-submit alone is not enough)")
	}
	// Cross-origin, with a valid token: the second layer fires.
	if err := svc.CheckCSRF(id, grant.CSRF, "http://evil.example", "", host); !errors.Is(err, ErrInvalidOrigin) {
		t.Fatalf("a cross-origin request = %v, want ErrInvalidOrigin", err)
	}
	// No Origin/Referer at all (curl, a CLI): the token is enough.
	if err := svc.CheckCSRF(id, grant.CSRF, "", "", host); err != nil {
		t.Fatalf("a non-browser request with a correct token was rejected: %v", err)
	}
}

func TestLoginRateLimitsAndAuditsFailuresWithoutThePassword(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()
	clock := &fakeClock{now: time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)}
	svc := NewService(repo,
		WithClock(clock.Now),
		WithPasswordCost(1000),
		WithLimiter(NewLimiter(3, 15*time.Minute, clock.Now)),
	)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}

	// Fill the bucket with failures: capacity 3 per 15 minutes.
	for i := 0; i < 3; i++ {
		_, err := svc.Login(ctx, LoginParams{Username: "admin", Password: "guessing the password here", RemoteAddr: "192.0.2.5:40000"})
		if !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("attempt %d = %v, want ErrInvalidCredentials", i+1, err)
		}
	}
	// Bucket empty. The *right* password is now refused too, and by the address
	// key: a throttled correct login is annoying and correct, because the
	// alternative is telling an attacker they may keep trying.
	var rl *RateLimited
	_, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "192.0.2.5:40000"})
	if !errors.As(err, &rl) {
		t.Fatalf("correct password on an exhausted address bucket = %v, want *RateLimited", err)
	}
	// A different address, same account: the *account* bucket is what stops this,
	// which is the anti-distribution property (docs/spec/14).
	_, err = svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "192.0.2.6:40000"})
	if !errors.As(err, &rl) {
		t.Fatalf("correct password from a fresh address on an exhausted account bucket = %v, want *RateLimited", err)
	}
	if rl.RetryAfterSeconds() <= 0 {
		t.Fatalf("Retry-After = %d, want a positive number of seconds", rl.RetryAfterSeconds())
	}

	// After the window refills, a correct login works and clears both buckets, so
	// the user who mistyped three times is not punished for it afterwards.
	clock.Advance(16 * time.Minute)
	if _, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "192.0.2.5:40000"}); err != nil {
		t.Fatalf("a correct login after the window was refused: %v", err)
	}
	for i := 0; i < 2; i++ {
		if _, err := svc.Login(ctx, LoginParams{Username: "admin", Password: "guessing the password here", RemoteAddr: "192.0.2.5:40000"}); !errors.Is(err, ErrInvalidCredentials) {
			t.Fatalf("post-success attempt %d was throttled instead of answered normally: %v", i+1, err)
		}
	}

	// Audit: failures recorded, no credential material anywhere in them.
	events := repo.auditEvents()
	if !contains(events, "login.failure") || !contains(events, "login.rate_limited") {
		t.Fatalf("audit events = %v, want login.failure and login.rate_limited", events)
	}
	for _, a := range repo.audits {
		blob := string(a.Detail) + a.UserID + a.RemoteAddr + a.Event
		if strings.Contains(blob, "guessing the password") || strings.Contains(blob, testPassword) {
			t.Fatalf("an audit row contains a password: %s / %s", a.Event, a.Detail)
		}
	}
}

// TestPasswordHashIsUpgradedOnLogin is the parameter-migration path: a row
// written under an older (cheaper) cost must become a current-cost row without an
// operator doing anything, and without invalidating the session it just minted.
func TestPasswordHashIsUpgradedOnLogin(t *testing.T) {
	ctx := context.Background()
	repo := newFakeRepo()
	clock := &fakeClock{now: time.Date(2026, time.October, 16, 3, 4, 5, 0, time.UTC)}
	weak := NewService(repo, WithClock(clock.Now), WithPasswordCost(1000))
	strong := NewService(repo, WithClock(clock.Now), WithPasswordCost(5000))

	if _, err := weak.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	stored, err := repo.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	before, err := ParamsFor(stored.PasswordHash)
	if err != nil {
		t.Fatal(err)
	}
	if before.Iterations != 1000 {
		t.Fatalf("seed hash was written at %d iterations", before.Iterations)
	}

	grant, err := strong.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"})
	if err != nil {
		t.Fatalf("login against a weaker stored hash: %v", err)
	}
	after, err := repo.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := ParamsFor(after.PasswordHash)
	if err != nil {
		t.Fatalf("the rehashed value is not a valid envelope: %v", err)
	}
	if upgraded.Iterations != 5000 {
		t.Fatalf("stored iterations = %d after login at cost 5000, want the upgrade", upgraded.Iterations)
	}
	if after.PasswordChangedAt.Equal(stored.PasswordChangedAt) == false {
		// password_changed_at moves with the hash (same UPDATE); what must NOT move
		// is any session — asserted next — so record which is which.
		_ = after.PasswordChangedAt
	}
	// The upgrade must not revoke the login that earned it...
	if _, err := strong.VerifySession(ctx, grant.Token); err != nil {
		t.Fatalf("a parameter upgrade killed the session: %v", err)
	}
	// ...while the password still verifies at the new cost.
	if ok, err := VerifyPassword(testPassword, after.PasswordHash); err != nil || !ok {
		t.Fatalf("the upgraded hash does not verify: %v %v", ok, err)
	}
	// And a wrong password is still wrong at the new cost.
	if ok, _ := VerifyPassword("not the password at all", after.PasswordHash); ok {
		t.Fatal("the upgraded hash accepted a wrong password")
	}
	// An upgrade failure must not block the login it was meant to improve.
	repo.failOn("RotatePassword")
	if _, err := strong.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:2"}); err != nil {
		t.Fatalf("a failed hash upgrade blocked the login: %v", err)
	}
}

func TestServiceReportsInternalFaultsAsFaults(t *testing.T) {
	ctx := context.Background()
	svc, repo, _ := newTestService(t)
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	repo.failOn("CreateUser")
	if _, err := svc.CreateUser(ctx, "second", testPassword, RoleViewer, nil, "cli"); errors.Is(err, ErrInvalidCredentials) || errors.Is(err, ErrAlreadyInitialized) {
		t.Fatalf("a storage fault was reported as a credential answer: %v", err)
	} else if err == nil {
		t.Fatal("a storage fault was swallowed")
	}
	repo.failOn("InsertSession")
	if _, err := svc.Login(ctx, LoginParams{Username: "admin", Password: testPassword, RemoteAddr: "127.0.0.1:1"}); !errors.Is(err, ErrInternal) {
		t.Fatalf("Login with a failing session write = %v, want ErrInternal", err)
	}
}

func TestCheckRoleAndAccess(t *testing.T) {
	viewer := &Identity{User: &StoredUser{ID: "v"}, Role: RoleViewer, Method: MethodCookie}
	anon := &Identity{}
	for _, tc := range []struct {
		name string
		id   *Identity
		need Access
		want error
	}{
		{"public for anonymous", anon, Public(), nil},
		{"viewer route for anonymous", anon, Require(RoleViewer), ErrNotPermitted},
		{"viewer route for viewer", viewer, Require(RoleViewer), nil},
		{"viewer route for curator", &Identity{User: &StoredUser{}, Role: RoleCurator}, Require(RoleViewer), nil},
		{"admin route for viewer", viewer, Require(RoleAdmin), ErrNotPermitted},
		{"admin route for admin", &Identity{User: &StoredUser{}, Role: RoleAdmin}, Require(RoleAdmin), nil},
	} {
		if err := CheckRole(tc.id, tc.need); !errors.Is(err, tc.want) {
			t.Errorf("%s: CheckRole = %v, want %v", tc.name, err, tc.want)
		}
	}
	if !(Access{}).IsPublic() {
		t.Error("the zero Access must be public-free... wait: RoleNone means public; grants are explicit")
	}
	if (Public()).Min != RoleNone {
		t.Error("Public() must require nothing")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
