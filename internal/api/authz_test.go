package api

// The role matrix, the scope-table/route drift check, admin account management
// and the developer-only event filter (docs/spec/08 "API version and user
// authentication", docs/spec/14 "Authorization" + "Security acceptance tests",
// ADR-0016).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// routeCase is one row of the matrix: a route as a caller addresses it, and the
// role the scope table says it needs. Placeholder rows are deliberate —
// docs/spec/08 lists routes a later milestone registers, and the point of putting
// them in the table *now* is that they cannot be born unauthenticated.
type routeCase struct {
	method, path string
	want         auth.Role
	// body is the payload an unsafe method needs; `{}` is enough for the routes
	// whose *authorization* is under test here (a rejected request never reaches
	// the handler's own validation, and an accepted one failing validation later
	// is still proof the gate opened).
	body string
}

func matrixCases() []routeCase {
	id := games.NewID()
	return []routeCase{
		// viewer: the library and the operational reads (docs/spec/08)
		{http.MethodGet, "/api/v1/events", auth.RoleViewer, ""},
		{http.MethodGet, "/api/v1/jobs", auth.RoleViewer, ""},
		{http.MethodGet, "/api/v1/jobs/" + id, auth.RoleViewer, ""},
		{http.MethodGet, "/api/v1/games", auth.RoleViewer, ""},
		{http.MethodGet, "/api/v1/games/demo/profiles", auth.RoleViewer, ""},
		{http.MethodGet, "/api/v1/auth/me", auth.RoleViewer, ""},
		{http.MethodPost, "/api/v1/auth/logout", auth.RoleViewer, ""},
		{http.MethodPost, "/api/v1/auth/password", auth.RoleViewer, `{}`},

		// operator: sessions, capture, audio, job control
		{http.MethodPost, "/api/v1/jobs/" + id + "/cancel", auth.RoleOperator, ""},
		{http.MethodPost, "/api/v1/jobs/" + id + "/retry", auth.RoleOperator, ""},
		{http.MethodGet, "/api/v1/capture/devices", auth.RoleOperator, ""},
		{http.MethodGet, "/api/v1/capture/preview", auth.RoleOperator, ""},
		{http.MethodPost, "/api/v1/sessions", auth.RoleOperator, `{}`},
		{http.MethodPost, "/api/v1/sessions/" + id + "/stop", auth.RoleOperator, ""},
		// The player's live controls (ADR-0075): pause/resume are idempotent
		// operator writes; settings is the mid-session policy. All three sit
		// under the session family's operator grant, and the matrix proves a
		// viewer's click on the pause button is refused at the gate.
		{http.MethodPost, "/api/v1/sessions/" + id + "/pause", auth.RoleOperator, ""},
		{http.MethodPost, "/api/v1/sessions/" + id + "/resume", auth.RoleOperator, ""},
		{http.MethodPut, "/api/v1/sessions/" + id + "/settings", auth.RoleOperator, `{}`},
		{http.MethodPost, "/api/v1/voices/" + id + "/register", auth.RoleOperator, ""},

		// curator: import, publish, voices, onboarding
		{http.MethodPost, "/api/v1/games", auth.RoleCurator, `{}`},
		// Library lifecycle (ADR-0107, gap G14): the curator's five operations.
		// The matrix proves a viewer's click on any of them is refused at the
		// gate; the admin-only purge sits in the admin block below, and its
		// explicit scope-table grant precedes the curator wildcard so the
		// boundary is in the table, not the handler.
		{http.MethodPost, "/api/v1/profiles/" + id + "/duplicate", auth.RoleCurator, ""},
		{http.MethodPost, "/api/v1/profiles/" + id + "/clone", auth.RoleCurator, `{}`},
		{http.MethodDelete, "/api/v1/profiles/" + id, auth.RoleCurator, ""},
		{http.MethodPost, "/api/v1/games/" + id + "/disable", auth.RoleCurator, ""},
		{http.MethodPost, "/api/v1/games/" + id + "/enable", auth.RoleCurator, ""},

		{http.MethodPost, "/api/v1/games/demo/profiles", auth.RoleCurator, `{}`},
		{http.MethodPost, "/api/v1/games/demo/profiles/rev1/publish", auth.RoleCurator, `{}`},
		{http.MethodPost, "/api/v1/games/demo/import", auth.RoleCurator, `{}`},
		{http.MethodPost, "/api/v1/voices/auditions", auth.RoleCurator, `{}`},
		{http.MethodPost, "/api/v1/voices/" + id + "/approve", auth.RoleCurator, ""},
		{http.MethodGet, "/api/v1/voices/" + id + "/registrations", auth.RoleCurator, ""},
		{http.MethodPost, "/api/v1/onboarding/runs", auth.RoleCurator, `{}`},
		{http.MethodPost, "/api/v1/corrections", auth.RoleCurator, `{}`},
		{http.MethodGet, "/api/v1/corrections", auth.RoleCurator, ""},

		// developer: docs/spec/14's "dev screenshot" is exactly this row
		{http.MethodGet, "/api/v1/dev/events", auth.RoleDeveloper, ""},
		{http.MethodGet, "/api/v1/dev/screenshots", auth.RoleDeveloper, ""},
		{http.MethodGet, "/api/v1/dev/traces/" + id, auth.RoleDeveloper, ""},
		{http.MethodPost, "/api/v1/dev/fixtures", auth.RoleDeveloper, `{}`},
		{http.MethodGet, "/api/v1/dev/fixtures", auth.RoleDeveloper, ""},
		{http.MethodPost, "/api/v1/dev/test-runs", auth.RoleDeveloper, `{}`},

		// The managed replay clip store (ADR-0073): gameplay recording, so the
		// developer tier; the matrix proves the operator cannot install one.
		{http.MethodPost, "/api/v1/replay/clips", auth.RoleDeveloper, `{}`},
		{http.MethodGet, "/api/v1/replay/clips", auth.RoleDeveloper, ""},
		{http.MethodDelete, "/api/v1/replay/clips/" + id, auth.RoleDeveloper, ""},

		// admin: users, tokens, config
		{http.MethodGet, "/api/v1/users", auth.RoleAdmin, ""},
		// The purge (ADR-0107): the one profile write the curator wildcard must
		// not swallow — the scope table keeps the boundary in the table.
		{http.MethodPost, "/api/v1/profiles/" + id + "/purge", auth.RoleAdmin, ""},

		{http.MethodPost, "/api/v1/users", auth.RoleAdmin, `{}`},
		{http.MethodPost, "/api/v1/users/" + id + "/disable", auth.RoleAdmin, ""},
		{http.MethodPost, "/api/v1/users/" + id + "/enable", auth.RoleAdmin, ""},
		{http.MethodPost, "/api/v1/users/" + id + "/password", auth.RoleAdmin, `{}`},
		{http.MethodGet, "/api/v1/users/" + id + "/tokens", auth.RoleAdmin, ""},
		{http.MethodPost, "/api/v1/users/" + id + "/tokens", auth.RoleAdmin, `{}`},
		{http.MethodPost, "/api/v1/users/" + id + "/tokens/" + games.NewID() + "/revoke", auth.RoleAdmin, ""},
		{http.MethodGet, "/api/v1/config", auth.RoleAdmin, ""},
		{http.MethodPut, "/api/v1/config", auth.RoleAdmin, `{}`},

		// unknown: fail closed (docs/spec/14 — the route map is not public
		// furniture, and an ungranted route must not default to open)
		{http.MethodGet, "/api/v1/not-a-route", auth.RoleAdmin, ""},
	}
}

// streamProbeTimeout bounds how long the matrix waits on a route that streams
// until its client goes away. GET /api/v1/events answers with 200 and then
// pumps frames forever, and the authorization question is fully answered by the
// status line and headers, so the probe reads those and lets the deadline end
// the stream — never a blocking read of a live event stream.
const streamProbeTimeout = 200 * time.Millisecond

// streamsUntilDisconnect reports the one shape in the matrix that would hang a
// ResponseRecorder: a registered GET that keeps writing.
func streamsUntilDisconnect(method, path string) bool {
	return method == http.MethodGet && path == eventsPath
}

// doMatrixReq is doReq plus the streaming carve-out.
func doMatrixReq(t *testing.T, tc routeCase, h http.Handler, r *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	if !streamsUntilDisconnect(tc.method, tc.path) {
		return doReq(t, h, r)
	}
	ctx, cancel := context.WithTimeout(r.Context(), streamProbeTimeout)
	t.Cleanup(cancel)
	defer cancel()
	return doReq(t, h, r.WithContext(ctx))
}

// gateDenials are the problem slugs the authorization middleware itself
// answers. Status alone cannot identify a denial: POST /api/v1/auth/password
// rejects a body with no current password as invalid-credentials, and that 401
// is proof the gate *opened* — the request reached the handler. So the matrix
// classifies by problem type, which only the middleware emits.
var gateDenials = map[string]bool{
	"unauthenticated":   true,
	"invalid-token":     true,
	"session-revoked":   true,
	"session-expired":   true,
	"session-idle":      true,
	"forbidden":         true,
	"csrf-check-failed": true,
	"origin-mismatch":   true,
}

func gateDenial(rec *httptest.ResponseRecorder) (problem, bool) {
	var doc problem
	if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
		return doc, false
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		return doc, false
	}
	slug, prefixed := strings.CutPrefix(doc.Type, "urn:game-narrator:error:")
	return doc, prefixed && gateDenials[slug]
}

// TestRoleMatrixOverEveryRoute walks every route at every role. Below the grant
// the answer must be 401/403 with a problem document; at or above it the request
// must reach the route — which is asserted as "not a 401/403" rather than a
// specific status, because most of these routes belong to later milestones and
// their own answer (404/503) is not what is under test here.
//
// This is docs/spec/14's "unauthorized viewer tries dev screenshot, voice
// registry and profile publish → denied" stated for the whole surface instead of
// three examples, so a grant written too low (or a route added without one) fails
// a test rather than shipping.
func TestRoleMatrixOverEveryRoute(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithJobs(store.NewJobsStore(a.st)), WithEvents(events.New()))

	denials := map[auth.Role]int{}
	for _, tc := range matrixCases() {
		for _, role := range auth.Ordered() {
			r := jsonRequest(tc.method, tc.path, tc.body)
			rec := doMatrixReq(t, tc, h, a.as(role, r))
			doc, denied := gateDenial(rec)
			if role.AtLeast(tc.want) {
				if denied {
					t.Errorf("%s %s as %s: %d, want the gate to open (body %s)",
						tc.method, tc.path, role, rec.Code, rec.Body.String())
				}
				continue
			}
			if !denied {
				t.Errorf("%s %s as %s: %d, want a denial — docs/spec/14 requires an unauthorized caller be denied",
					tc.method, tc.path, role, rec.Code)
				continue
			}
			denials[role]++
			requireContentType(t, rec, ContentTypeProblem)
			if doc.RequestID == "" || doc.RequestID != rec.Header().Get(HeaderRequestID) {
				t.Errorf("%s %s as %s: the denial carries no usable correlation id", tc.method, tc.path, role)
			}
			// A 403 must name the role to ask for; a 401 must not name any
			// account at all.
			if rec.Code == http.StatusForbidden && !strings.Contains(doc.Detail, tc.want.String()) {
				t.Errorf("%s %s as %s: the 403 does not name the required role (%s)", tc.method, tc.path, role, doc.Detail)
			}
			if strings.Contains(doc.Detail, testPassword) || strings.Contains(doc.Detail, "pbkdf2") {
				t.Errorf("%s %s as %s: the denial leaked credential material: %s", tc.method, tc.path, role, doc.Detail)
			}
		}
	}
	// A role that denied nothing proves the table never crosses its boundary.
	for _, role := range auth.Ordered() {
		if role == auth.RoleAdmin {
			continue
		}
		if denials[role] == 0 {
			t.Errorf("role %s was never denied a route: the matrix does not test its ceiling", role)
		}
	}
	if denials[auth.RoleAdmin] != 0 {
		t.Errorf("an admin was denied %d routes: admin is the top of docs/spec/08's order and must reach everything", denials[auth.RoleAdmin])
	}
}

// TestEveryRegisteredRouteIsCoveredByTheScopeTable is the drift detector. A route
// in Handler() with no grant is not broken — it is silently *admin-only*, which
// is the safe direction but will confuse the UI the moment it ships, so it fails
// a test here instead.
func TestEveryRegisteredRouteIsCoveredByTheScopeTable(t *testing.T) {
	a := newTestAuth(t)
	s := New(testBuild, testStarted, discardLogger, WithWebAssets(uiBuild()), WithAuth(a.svc, a.repo))
	s.Handler()
	if len(s.routes) == 0 {
		t.Fatal("no routes were recorded, so this check would pass vacuously")
	}
	for _, rt := range s.routes {
		acc, known := auth.DefaultPolicy().Require(rt.method, rt.path)
		if !known {
			t.Errorf("route %s %s has no grant in auth.DefaultPolicy: it requires admin by the fail-closed default",
				rt.method, rt.path)
			continue
		}
		// Sanity on the direction of each grant, so a typo in the table (jobs
		// readable only by admin, users readable by viewer) cannot land as a
		// "passing" test elsewhere.
		switch {
		case strings.HasPrefix(rt.path, "/api/v1/onboarding/"):
			// The onboarding surface is curator-end-to-end (ADR-0028): its
			// pause/resume/cancel act on one run's own attempt, never on the
			// queue at large, so they are run curation, not job control. The
			// queue itself stays operator-gated at /api/v1/jobs.
			if acc.Min != auth.RoleCurator {
				t.Errorf("%s %s granted to %s, want curator (the onboarding surface is curator-end-to-end)", rt.method, rt.path, acc.Min)
			}
		case strings.HasPrefix(rt.path, "/api/v1/replay/"):
			// The managed clip store is developer (ADR-0073): a clip is a
			// recording of gameplay frames — the same sensitive-evidence tier
			// as the dev traces — and installing one changes what the runtime
			// can replay, which is a developer act, not an operator convenience.
			if acc.Min != auth.RoleDeveloper {
				t.Errorf("%s %s granted to %s, want developer (a clip is gameplay recording, docs/spec/08)", rt.method, rt.path, acc.Min)
			}
		case rt.path == "/api/v1/jobs" || rt.path == "/api/v1/jobs/{id}" || rt.path == "/api/v1/events":
			if acc.Min != auth.RoleViewer {
				t.Errorf("%s %s granted to %s, want viewer", rt.method, rt.path, acc.Min)
			}
		case strings.HasSuffix(rt.path, "/cancel") || strings.HasSuffix(rt.path, "/retry"):
			if acc.Min != auth.RoleOperator {
				t.Errorf("%s %s granted to %s, want operator (job control is not curation)", rt.method, rt.path, acc.Min)
			}
		case strings.HasPrefix(rt.path, "/api/v1/users"):
			if acc.Min != auth.RoleAdmin {
				t.Errorf("%s %s granted to %s, want admin", rt.method, rt.path, acc.Min)
			}
		case rt.path == "/api/v1/health/live", rt.path == "/api/v1/health/ready",
			rt.path == "/api/v1/system/info", rt.path == "/api/v1/auth/status",
			rt.method == http.MethodPost && (rt.path == "/api/v1/auth/login" || rt.path == "/api/v1/auth/setup"):
			if !acc.IsPublic() {
				t.Errorf("%s %s is not public, but docs/spec/08 requires it before anybody can sign in", rt.method, rt.path)
			}
		}
	}
}

// TestUnknownRouteFailsClosedToAdmin states the default on its own terms: the
// fail-closed answer is *admin*, and the fail-open alternative (public until
// somebody says otherwise) is what docs/spec/14 is written against.
func TestUnknownRouteFailsClosedToAdmin(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	for _, role := range auth.Ordered()[:4] {
		if rec := a.get(t, h, role, "/api/v1/a-route-nobody-wrote"); rec.Code != http.StatusForbidden {
			t.Errorf("%s on an ungranted path = %d, want 403", role, rec.Code)
		}
	}
	// An admin reaches it, and gets the router's honest 404: the default
	// elevates the requirement, it does not invent a handler.
	rec := a.get(t, h, auth.RoleAdmin, "/api/v1/a-route-nobody-wrote")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("admin on an ungranted path = %d, want 404 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	// And the policy itself says so, without a server in the way.
	acc, known := auth.DefaultPolicy().Require(http.MethodGet, "/api/v1/a-route-nobody-wrote")
	if known || acc.Min != auth.RoleAdmin {
		t.Errorf("DefaultPolicy on an unknown path = %s (known %v), want admin and not known", acc, known)
	}
}

// ── admin account management ───────────────────────────────────────────────

func TestAdminUserRoutesAreAdminOnlyAndCarryNoHashes(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()

	for _, role := range auth.Ordered()[:4] {
		rec := a.get(t, h, role, "/api/v1/users")
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s reading /api/v1/users = %d, want 403 (docs/spec/08: users are admin)", role, rec.Code)
		}
	}
	rec := a.get(t, h, auth.RoleAdmin, "/api/v1/users")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin reading /api/v1/users = %d (body %s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// docs/spec/14's "no raw key in the frontend bundle" applies to our own
	// credential material: an admin reading the account list has no need for a
	// hash, and the DTO has no field that could hold one.
	for _, forbidden := range []string{"pbkdf2", "password_hash", "token_hash", testPassword} {
		if strings.Contains(body, forbidden) {
			t.Errorf("the user list carried %q: %s", forbidden, body)
		}
	}
	// Field names, not just substrings: `password_changed_at` is a legitimate
	// timestamp and must not be mistaken for a leak, while a key literally named
	// `password` (or anything holding a digest) must never appear. A substring
	// scan alone would either drown in that false positive or miss a renamed one.
	var raw struct {
		Users []map[string]any `json:"users"`
	}
	decode(t, rec, &raw)
	for _, u := range raw.Users {
		for key := range u {
			switch key {
			case "id", "username", "role", "disabled", "created_at", "password_changed_at":
			default:
				t.Errorf("the user list carries an unexpected field %q: %s", key, body)
			}
		}
	}
	var listed struct {
		Users []userDTO `json:"users"`
	}
	decode(t, rec, &listed)
	if len(listed.Users) != 5 {
		t.Fatalf("listed %d accounts, want the seeded five", len(listed.Users))
	}
	seen := map[string]string{}
	for _, u := range listed.Users {
		seen[u.Username] = u.Role
	}
	for _, role := range auth.Ordered() {
		if seen[role.String()] != role.String() {
			t.Errorf("account %s missing or mis-roled in the list: %v", role, seen)
		}
	}
}

func TestAdminCreatesAccountMintsTokenOnceAndWithdraws(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	ctx := context.Background()

	// Create.
	rec := doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost, "/api/v1/users",
		`{"username":"operator2","password":"`+testPassword+`","role":"operator"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create account = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var created struct {
		User userDTO `json:"user"`
	}
	decode(t, rec, &created)
	if created.User.Role != "operator" || created.User.ID == "" {
		t.Fatalf("created = %+v", created.User)
	}
	if created.User.PasswordChangedAt == "" {
		t.Error("a new account must record when its credential was set")
	}

	// Case-flipped duplicate is the same conflict the schema enforces.
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost, "/api/v1/users",
		`{"username":"OPERATOR2","password":"`+testPassword+`","role":"viewer"}`)))
	if rec.Code != http.StatusConflict {
		t.Errorf("duplicate username (case-flipped) = %d, want 409 — the NOCASE index must be visible to the API", rec.Code)
	}
	// An unknown role is a 400, not a silent downgrade.
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost, "/api/v1/users",
		`{"username":"sneaky","password":"`+testPassword+`","role":"supreme"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("role supreme = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	// A short password is a 400 quoting the floor.
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost, "/api/v1/users",
		`{"username":"weakling","password":"short","role":"viewer"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "12") {
		t.Fatalf("weak password = %d (%s), want 400 naming the minimum", rec.Code, rec.Body.String())
	}
	// A curator may not create accounts at all, whatever it tries to name.
	rec = doReq(t, h, a.as(auth.RoleCurator, jsonRequest(http.MethodPost, "/api/v1/users",
		`{"username":"selfelevated","password":"`+testPassword+`","role":"admin"}`)))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("curator creating an account = %d, want 403", rec.Code)
	}

	// Mint a token: printed once, stored as a digest, usable immediately.
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/tokens", `{"name":"deploy","role":"operator"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create token = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	var minted struct {
		Token string `json:"token"`
		ID    string `json:"id"`
	}
	decode(t, rec, &minted)
	if !strings.HasPrefix(minted.Token, auth.APITokenPrefix) {
		t.Fatalf("issued token %q is not in the documented gn_ form", minted.Token)
	}
	// It works, and as its own role. Deliberately an assignment to the shared
	// recorder, not an `if rec :=` shadow: the read of `me` below has to be the
	// read of *this* response.
	if rec = doReq(t, h, withBearer(getReq("/api/v1/auth/me"), minted.Token)); rec.Code != http.StatusOK {
		t.Fatalf("the minted token does not authenticate: %d (body %s)", rec.Code, rec.Body.String())
	}
	var me struct {
		User userDTO `json:"user"`
	}
	decode(t, rec, &me)
	if me.User.Username != "operator2" {
		t.Fatalf("token authenticated as %q (body %s)", me.User.Username, rec.Body.String())
	}
	// A second read of the list must not carry the secret or its digest.
	rec = a.get(t, h, auth.RoleAdmin, "/api/v1/users/"+created.User.ID+"/tokens")
	if rec.Code != http.StatusOK {
		t.Fatalf("list tokens = %d (body %s)", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), minted.Token) || strings.Contains(rec.Body.String(), auth.DigestSecret(minted.Token)) {
		t.Fatalf("the token list carried the secret or its digest: %s", rec.Body.String())
	}
	// Asking for a role above the owner is refused (docs/spec/08 "role (≤ owner's
	// role)"): the escalation path an operator must not have.
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/tokens", `{"name":"oops","role":"admin"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("operator-owned token minted as admin = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	// Disable the account: its sessions and tokens die with it.
	session, _ := login(t, h, "operator2", testPassword)
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), session)); rec.Code != http.StatusOK {
		t.Fatalf("the new account cannot sign in: %d", rec.Code)
	}
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/disable", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var disabled struct {
		User     userDTO `json:"user"`
		Sessions float64 `json:"sessions_revoked"`
		Tokens   float64 `json:"tokens_revoked"`
	}
	decode(t, rec, &disabled)
	if !disabled.User.Disabled {
		t.Error("the response does not report the account as disabled")
	}
	if disabled.Sessions < 1 || disabled.Tokens < 1 {
		t.Errorf("disable revoked %+v, want both the session and the token counted", disabled)
	}
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), session)); rec.Code != http.StatusUnauthorized {
		t.Errorf("a disabled account's cookie still works: %d", rec.Code)
	}
	if rec := doReq(t, h, withBearer(getReq("/api/v1/auth/me"), minted.Token)); rec.Code != http.StatusUnauthorized {
		t.Errorf("a disabled account's token still works: %d", rec.Code)
	}

	// Revoke the token explicitly, then confirm it is dead even after the account
	// is re-enabled: revocation is permanent, which is what makes "revoke the
	// leaked one" an action with an effect.
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/tokens/"+minted.ID+"/revoke", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke token = %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/enable", ""))); rec.Code != http.StatusOK {
		t.Fatalf("enable = %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, withBearer(getReq("/api/v1/auth/me"), minted.Token)); rec.Code != http.StatusUnauthorized {
		t.Errorf("a revoked token came back to life when the account was re-enabled: %d", rec.Code)
	}

	// An admin reset revokes the account's sessions (docs/spec/14).
	third, _ := login(t, h, "operator2", testPassword)
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/password", `{"password":"a replaced passphrase"}`)))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin reset = %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), third)); rec.Code != http.StatusUnauthorized {
		t.Errorf("an admin password reset left the old session alive: %d", rec.Code)
	}
	if _, _ = login(t, h, "operator2", "a replaced passphrase"); false {
		t.Fatal("the replaced password does not work")
	}

	// Nobody but an admin may withdraw a principal, and the withdraw is audited.
	if rec := doReq(t, h, a.as(auth.RoleOperator, jsonRequest(http.MethodPost,
		"/api/v1/users/"+created.User.ID+"/disable", ""))); rec.Code != http.StatusForbidden {
		t.Errorf("an operator disabled an account: %d", rec.Code)
	}
	rows, err := a.repo.ListAuthAudit(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.Event] = true
	}
	for _, want := range []string{"user.create", "token.create", "user.disable", "password.admin_reset", "access.denied"} {
		if !got[want] {
			t.Errorf("auth_audit is missing %q: %v — docs/spec/14 wants these decisions recorded", want, keysOf(got))
		}
	}
	for _, row := range rows {
		blob := string(row.Detail) + row.UserID + row.RemoteAddr
		if strings.Contains(blob, testPassword) || strings.Contains(blob, "pbkdf2") {
			t.Fatalf("an audit row carries credential material (%s): %s", row.Event, row.Detail)
		}
	}
}

func withBearer(r *http.Request, token string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+token)
	return r
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestAdminCannotDisableItselfThroughTheAPI closes the one self-inflicted
// lockout the routes could cause: an admin is the only credential in a fresh
// appliance, and withdrawing it mid-session leaves no way back through the API.
func TestAdminCannotDisableItselfThroughTheAPI(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler()
	admin := a.mustUser("admin")
	cookies, token := login(t, h, "admin", testPassword)

	r := withCookie(jsonRequest(http.MethodPost, "/api/v1/users/"+admin.ID+"/disable", ""), cookies)
	r.Header.Set(auth.HeaderCSRFToken, token)
	rec := doReq(t, h, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("self-disable = %d, want 409 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "admin") {
		t.Errorf("the 409 does not point at the CLI path that can do it deliberately: %s", rec.Body.String())
	}
	// Disabling *somebody else* is fine, and the same account stays usable.
	other := a.mustUser("viewer")
	rec = doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost, "/api/v1/users/"+other.ID+"/disable", "")))
	if rec.Code != http.StatusOK {
		t.Fatalf("disabling another account = %d (body %s)", rec.Code, rec.Body.String())
	}
	if rec := doReq(t, h, withCookie(getReq("/api/v1/auth/me"), cookies)); rec.Code != http.StatusOK {
		t.Errorf("disabling a colleague broke the admin's own session: %d", rec.Code)
	}
	id := games.NewID()
	if _, err := a.repo.UserByID(context.Background(), id); err == nil {
		t.Fatal("the fixture issued an id that already belongs to an account")
	}
	for _, bad := range []string{"not-an-id", "01SHORT"} {
		rec := doReq(t, h, a.as(auth.RoleAdmin, jsonRequest(http.MethodPost, "/api/v1/users/"+bad+"/disable", "")))
		if rec.Code != http.StatusNotFound {
			t.Errorf("malformed user id %q = %d, want 404 — the same answer as \"no such account\", so the route cannot probe id shapes", bad, rec.Code)
		}
	}
}

// ── developer-only events ─────────────────────────────────────────────────

// TestEventStreamRedactsDeveloperEvents is docs/spec/08 "Partial data redacted
// when not in developer mode" on the one channel that carries game text, and
// docs/spec/14 "developer diagnostics can reveal game content": below developer
// the decision family is absent from the stream — in replay as well as live —
// while the operational events a page needs to paint anything survive.
func TestEventStreamRedactsDeveloperEvents(t *testing.T) {
	a := newTestAuth(t)
	hub := events.New(events.WithHeartbeat(20 * time.Millisecond))
	srv := httptest.NewServer(a.handler(WithEvents(hub)))
	t.Cleanup(srv.Close)

	// Published *before* any subscription, so what arrives is replayed history:
	// a filter that only inspected live events would pass a naive test and leak
	// on every reconnect.
	for _, name := range []string{
		events.MessageCommitted, events.SpeechStarted,
		events.DecisionRequested, events.DecisionResolved,
	} {
		publishTestEvent(t, hub, name, "")
	}
	secret := "the captain's last line was not for the player"
	if _, err := hub.Publish(context.Background(), events.Envelope{
		Name: events.DecisionResolved, TraceID: "trace-redaction",
		Data: json.RawMessage(`{"candidate_text":"` + secret + `"}`),
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		role auth.Role
		seen bool
	}{
		{auth.RoleViewer, false},
		{auth.RoleCurator, false},
		{auth.RoleOperator, false},
		{auth.RoleDeveloper, true},
		{auth.RoleAdmin, true},
	} {
		text := readStream(t, srv, a.bearer(tc.role), 600*time.Millisecond)
		hasDecision := strings.Contains(text, "event: "+events.DecisionRequested) ||
			strings.Contains(text, "event: "+events.DecisionResolved)
		if hasDecision != tc.seen {
			t.Errorf("%s stream decision events present = %v, want %v\n%s", tc.role, hasDecision, tc.seen, trimForTest(text))
		}
		if !tc.seen && strings.Contains(text, secret) {
			t.Errorf("%s stream leaked decision payload text: %s", tc.role, trimForTest(text))
		}
		// The operational half must not be collateral damage: a stream with only
		// decision events stripped and nothing left is a blank UI.
		for _, want := range []string{"event: " + events.MessageCommitted, "event: " + events.SpeechStarted} {
			if !strings.Contains(text, want) {
				t.Errorf("%s stream is missing %s: %s", tc.role, want, trimForTest(text))
			}
		}
	}

	// A live event published after the stream opened is filtered by the same rule
	// (the replay-only filter is the failure mode this guards).
	live := doReqStream(t, srv, a.bearer(auth.RoleViewer), 2*time.Second)
	defer live.Close()
	go func() {
		_, _ = hub.Publish(context.Background(), events.Envelope{
			Name: events.DecisionRequested, TraceID: "trace-live",
			Data: json.RawMessage(`{"note":"live decision payload"}`),
		})
		_, _ = hub.Publish(context.Background(), events.Envelope{
			Name: events.MessageCommitted, TraceID: "trace-live", Data: json.RawMessage(`{"message_id":"m1"}`),
		})
	}()
	text := live.readUntilDeadline()
	if strings.Contains(text, "live decision payload") || strings.Contains(text, "event: "+events.DecisionRequested) {
		t.Errorf("a live decision event reached a viewer's stream: %s", trimForTest(text))
	}
	if !strings.Contains(text, "event: "+events.MessageCommitted) {
		t.Errorf("the live viewer stream never delivered the operational event: %s", trimForTest(text))
	}
}

// rawStream is an open event stream plus the deadline that ends it. Reading it is
// the only way to tell "the stream filtered this event out" from "the stream has
// not delivered anything yet": a test that only inspected the bytes present at
// connect time would pass a filter that never worked at all.
type rawStream struct {
	body   io.ReadCloser
	cancel context.CancelFunc
}

func (s *rawStream) Close() { s.body.Close(); s.cancel() }

// readUntilDeadline drains frames until the stream's deadline turns Read into an
// error, which is how "nothing more is coming" is observed without sleeping a
// fixed interval.
func (s *rawStream) readUntilDeadline() string {
	buf := make([]byte, 8192)
	var out strings.Builder
	for {
		n, err := s.body.Read(buf)
		out.Write(buf[:n])
		if err != nil || out.Len() > 16384 {
			return out.String()
		}
	}
}

// doReqStream opens an event stream with one credential.
func doReqStream(t *testing.T, srv *httptest.Server, bearer string, window time.Duration) *rawStream {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+eventsPath+"?last_event_id=0", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		resp.Body.Close()
		t.Fatalf("events with that credential = %d (%s)", resp.StatusCode, body)
	}
	return &rawStream{body: resp.Body, cancel: cancel}
}

func readStream(t *testing.T, srv *httptest.Server, bearer string, window time.Duration) string {
	t.Helper()
	s := doReqStream(t, srv, bearer, window)
	defer s.Close()
	return s.readUntilDeadline()
}

func trimForTest(s string) string {
	if len(s) > 1200 {
		return s[:1200] + "…"
	}
	return s
}
