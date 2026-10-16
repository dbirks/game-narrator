package auth

import (
	"net/http"
	"strings"
	"testing"
)

func TestRoleOrdering(t *testing.T) {
	ordered := Ordered()
	want := []Role{RoleViewer, RoleCurator, RoleOperator, RoleDeveloper, RoleAdmin}
	if len(ordered) != len(want) {
		t.Fatalf("Ordered() = %v, want %v", ordered, want)
	}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("Ordered()[%d] = %s, want %s", i, ordered[i], want[i])
		}
		if i > 0 && !ordered[i].AtLeast(ordered[i-1]) {
			t.Fatalf("%s must satisfy %s", ordered[i], ordered[i-1])
		}
		if i > 0 && ordered[i-1].AtLeast(ordered[i]) {
			t.Fatalf("%s must not satisfy the higher %s", ordered[i-1], ordered[i])
		}
	}
	// RoleNone is not a role a principal holds; it is what an unauthenticated
	// request carries, and it satisfies only a public grant.
	if RoleNone.AtLeast(RoleViewer) {
		t.Fatal("RoleNone satisfied a viewer grant")
	}
	if !RoleNone.AtLeast(RoleNone) {
		t.Fatal("RoleNone did not satisfy a public grant")
	}
	for _, r := range ordered {
		if !r.Valid() {
			t.Fatalf("%s must be a valid role", r)
		}
		if got, err := ParseRole(r.String()); err != nil || got != r {
			t.Fatalf("ParseRole(%q) = %v, %v", r.String(), got, err)
		}
	}
	if _, err := ParseRole("superuser"); err == nil {
		t.Fatal("ParseRole accepted a role outside docs/spec/08's set")
	}
	if _, err := ParseRole(""); err == nil {
		t.Fatal("ParseRole accepted the empty role")
	}
	if _, err := ParseRole("none"); err == nil {
		t.Fatal("ParseRole accepted the unauthenticated pseudo-role as a grantable one")
	}
	// Case-insensitive on the wire, because the CLI and a hand-written config
	// both get to say --role=ADMIN.
	if got, err := ParseRole("Operator"); err != nil || got != RoleOperator {
		t.Fatalf("ParseRole(Operator) = %v, %v", got, err)
	}
	for _, r := range Ordered() {
		if GrantFor[r] == "" {
			t.Fatalf("role %s has no grant text for the ADR/UI legend", r)
		}
	}
}

func TestMatchPattern(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"/api/v1/jobs", "/api/v1/jobs", true},
		{"/api/v1/jobs/{id}", "/api/v1/jobs/01ABC", true},
		{"/api/v1/jobs/{id}", "/api/v1/jobs/01ABC/cancel", false},
		{"/api/v1/jobs/{id}/cancel", "/api/v1/jobs/01ABC/cancel", true},
		{"/api/v1/jobs/{id}/cancel", "/api/v1/jobs/cancel", false},
		{"/api/v1/dev/{rest...}", "/api/v1/dev/screenshots", true},
		{"/api/v1/dev/{rest...}", "/api/v1/dev/traces/01ABC/raw", true},
		{"/api/v1/dev/{rest...}", "/api/v1/dev", false},
		{"/api/v1/dev/{rest...}", "/api/v1/voices", false},
		// A trailing slash is canonicalized away by both ServeMux and splitPath,
		// so the pattern claims it too — the middleware then routes the request
		// and the 405/404 fallback gives the answer.
		{"/api/v1/users", "/api/v1/users/", true},
		{"/api/v1/auth/{rest...}", "/api/v1/auth/me", true},
		{"/api/v1/games/{id}/profiles", "/api/v1/games/g1/profiles", true},
		// A pattern must not match a sibling prefix: "/api/v1/jobs" is not
		// "/api/v1/jobsomething".
		{"/api/v1/jobs/{rest...}", "/api/v1/jobby", false},
	}
	for _, tc := range cases {
		if got := MatchPattern(tc.pattern, tc.path); got != tc.want {
			t.Errorf("MatchPattern(%q, %q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestDefaultPolicyGrantsDocsSpec08Routes(t *testing.T) {
	// The table is docs/spec/08 "API version and user authentication" applied to
	// docs/spec/08 "APIs", and the assertions name the docs/spec/14 acceptance
	// test: "unauthorized viewer tries dev screenshot, voice registry and
	// profile publish → denied".
	cases := []struct {
		method, path string
		want         Role
	}{
		// public: health, system info, and the three auth entry points
		{http.MethodGet, "/api/v1/health/live", RoleNone},
		{http.MethodGet, "/api/v1/health/ready", RoleNone},
		{http.MethodGet, "/api/v1/system/info", RoleNone},
		{http.MethodGet, "/api/v1/auth/status", RoleNone},
		{http.MethodPost, "/api/v1/auth/login", RoleNone},
		{http.MethodPost, "/api/v1/auth/setup", RoleNone},

		// viewer: reads
		{http.MethodGet, "/api/v1/events", RoleViewer},
		{http.MethodGet, "/api/v1/jobs", RoleViewer},
		{http.MethodGet, "/api/v1/jobs/01ABC", RoleViewer},
		{http.MethodGet, "/api/v1/games", RoleViewer},
		{http.MethodGet, "/api/v1/games/g1/profiles", RoleViewer},
		{http.MethodGet, "/api/v1/profiles/p1/messages/m1", RoleViewer},
		{http.MethodGet, "/api/v1/voices", RoleViewer},
		// docs/spec/14 "unauthorized viewer tries … voice registry … → denied"
		{http.MethodGet, "/api/v1/voices/01ABC/registrations", RoleCurator},
		{http.MethodGet, "/api/v1/auth/me", RoleViewer},
		{http.MethodPost, "/api/v1/auth/logout", RoleViewer},

		// operator: control of capture, audio, sessions and the queue
		{http.MethodPost, "/api/v1/jobs/01ABC/cancel", RoleOperator},
		{http.MethodPost, "/api/v1/jobs/01ABC/retry", RoleOperator},
		{http.MethodGet, "/api/v1/capture/devices", RoleOperator},
		{http.MethodGet, "/api/v1/capture/preview", RoleOperator},
		{http.MethodPost, "/api/v1/sessions", RoleOperator},
		{http.MethodPost, "/api/v1/sessions/01ABC/stop", RoleOperator},
		{http.MethodPost, "/api/v1/voices/01ABC/register", RoleOperator},

		// curator: content and voice curation
		{http.MethodPost, "/api/v1/games", RoleCurator},
		{http.MethodPost, "/api/v1/games/g1/profiles", RoleCurator},
		{http.MethodPost, "/api/v1/games/g1/profiles/rev/publish", RoleCurator},
		{http.MethodPost, "/api/v1/games/g1/import", RoleCurator},
		{http.MethodPost, "/api/v1/voices/auditions", RoleCurator},
		{http.MethodPost, "/api/v1/voices/01ABC/approve", RoleCurator},
		{http.MethodPost, "/api/v1/onboarding/runs", RoleCurator},
		// developer: docs/spec/08's dev inspection surface, including routes
		// that do not exist yet — the prefix claim is what stops them being born
		// public (docs/spec/10: emulator/test endpoints must never be live without auth)
		{http.MethodGet, "/api/v1/dev/events", RoleDeveloper},
		{http.MethodGet, "/api/v1/dev/screenshots", RoleDeveloper},
		{http.MethodGet, "/api/v1/dev/traces/01ABC", RoleDeveloper},
		{http.MethodPost, "/api/v1/dev/fixtures", RoleDeveloper},
		{http.MethodPost, "/api/v1/dev/test-runs", RoleDeveloper},

		// admin: users, tokens, config
		{http.MethodGet, "/api/v1/users", RoleAdmin},
		{http.MethodPost, "/api/v1/users", RoleAdmin},
		{http.MethodPost, "/api/v1/users/01ABC/disable", RoleAdmin},
		{http.MethodPost, "/api/v1/users/01ABC/tokens", RoleAdmin},
		{http.MethodPost, "/api/v1/users/01ABC/tokens/01TOK/revoke", RoleAdmin},
		{http.MethodGet, "/api/v1/config", RoleAdmin},
		{http.MethodPut, "/api/v1/config", RoleAdmin},

		// unknown: fail closed
		{http.MethodGet, "/api/v1/nope", RoleAdmin},
		{http.MethodPost, "/api/v1/whatever", RoleAdmin},
		{http.MethodGet, "/api/v1/", RoleAdmin},
	}
	p := DefaultPolicy()
	for _, tc := range cases {
		got, known := p.Require(tc.method, tc.path)
		if got.Min != tc.want {
			t.Errorf("%s %s: required role = %s, want %s", tc.method, tc.path, got.Min, tc.want)
		}
		if tc.path != "/api/v1/nope" && tc.path != "/api/v1/whatever" && tc.path != "/api/v1/" && !known {
			t.Errorf("%s %s: no grant matched, so it fell through to the fail-closed default", tc.method, tc.path)
		}
	}
}

// TestUnknownRouteRequiresAdmin is the fail-closed default stated on its own:
// a route that nobody wrote a grant for is privileged, not public.
func TestUnknownRouteRequiresAdmin(t *testing.T) {
	p := DefaultPolicy()
	for _, path := range []string{"/api/v1/", "/api/v1", "/api/v1/sealed", "/api/v1/sessions-next"} {
		acc, known := p.Require(http.MethodGet, path)
		if acc.Min != RoleAdmin {
			t.Errorf("%s: required %s, want admin", path, acc.Min)
		}
		if known && path != "/api/v1/sessions-next" {
			t.Errorf("%s unexpectedly matched a grant (%s)", path, acc)
		}
	}
	// A nil Policy (a Server built without one) must fail the same way.
	var nilPolicy *Policy
	if acc, _ := nilPolicy.Require(http.MethodGet, "/api/v1/jobs"); acc.Min != RoleAdmin {
		t.Errorf("nil policy required %s, want admin", acc.Min)
	}
}

func TestGrantsAreExposedInOrder(t *testing.T) {
	grants := DefaultPolicy().Grants()
	if len(grants) < 20 {
		t.Fatalf("DefaultPolicy has %d grants, which is fewer than docs/spec/08's route table", len(grants))
	}
	for _, g := range grants {
		if g.Pattern == "" {
			t.Fatal("a grant with no pattern would match nothing and protect nothing")
		}
		if g.Method != "" && g.Method != http.MethodGet && g.Method != http.MethodPost &&
			g.Method != http.MethodPut && g.Method != http.MethodPatch && g.Method != http.MethodDelete {
			t.Errorf("grant %s %s has an unexpected method", g.Method, g.Pattern)
		}
	}
}

func TestIsLoopbackRemote(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"127.65.43.21:1", true}, // all of 127.0.0.0/8 is local
		{"[::1]:8080", true},
		{"::1:1234", false},
		{"192.168.1.20:8080", false},
		{"10.0.0.5:1", false},
		{"169.254.9.9:1", false},
		{"8.8.8.8:53", false},
		{"::ffff:127.0.0.1:1", false}, // malformed: SplitHostPort fails, ParseIP fails
		{"", false},
		{"not-an-address", false},
	}
	for _, tc := range cases {
		if got := IsLoopbackRemote(tc.addr); got != tc.want {
			t.Errorf("IsLoopbackRemote(%q) = %v, want %v", tc.addr, got, tc.want)
		}
	}
}

func TestUnsafeMethod(t *testing.T) {
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions, "get"} {
		if UnsafeMethod(m) {
			t.Errorf("%s was treated as unsafe", m)
		}
	}
	// Everything else — including an extension method nobody defined — is
	// unsafe. That is the fail-closed half of the CSRF rule.
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete, "PROPPATCH", "WEIRD"} {
		if !UnsafeMethod(m) {
			t.Errorf("%s was treated as safe", m)
		}
	}
}

func TestOriginAllowed(t *testing.T) {
	cases := []struct {
		name                  string
		origin, referer, host string
		want                  bool
	}{
		{"neither present", "", "", "127.0.0.1:8080", true},
		{"matching origin", "http://127.0.0.1:8080", "", "127.0.0.1:8080", true},
		{"cross-site origin", "http://evil.example", "", "127.0.0.1:8080", false},
		{"origin port differs", "http://127.0.0.1:9999", "", "127.0.0.1:8080", false},
		{"referer only, matching", "", "http://127.0.0.1:8080/games/g1", "127.0.0.1:8080", true},
		{"referer matches, scheme differs", "", "https://evil.example/x", "evil.example", true},
		// The port is part of the origin: a page on :8080 is a different
		// principal than the app on :8081, and r.Host carries the port either
		// way, so a mismatch is refused rather than forgiven.
		{"referer cross-port", "", "http://evil.example:8080/", "evil.example", false},
		{"origin wins over referer", "http://evil.example", "http://127.0.0.1:8080/", "127.0.0.1:8080", false},
		{"unparsable origin", "not-a-url", "", "127.0.0.1:8080", false},
		{"default port stripped", "http://narrator.local", "narrator.local:80", "narrator.local", true},
		{"case-insensitive host", "http://NARRATOR.local", "", "narrator.local", true},
	}
	for _, tc := range cases {
		if got := OriginAllowed(tc.origin, tc.referer, tc.host); got != tc.want {
			t.Errorf("%s: OriginAllowed(%q,%q,%q) = %v, want %v", tc.name, tc.origin, tc.referer, tc.host, got, tc.want)
		}
	}
}

func TestValidateUsername(t *testing.T) {
	ok := []string{"admin", "ci.harness", "game-operator", "ops+deploy", "dave@local", "abc"}
	for _, name := range ok {
		if err := ValidateUsername(name); err != nil {
			t.Errorf("ValidateUsername(%q) = %v, want acceptance", name, err)
		}
	}
	bad := []string{"", "ab", "ad min", "admin!", "_admin", "admin_", "ad\vm in", strings.Repeat("a", 65), "ünïcode"}
	for _, name := range bad {
		if err := ValidateUsername(name); err == nil {
			t.Errorf("ValidateUsername(%q) was accepted; the schema's CHECK and NOCASE folding would disagree about it", trunc(name, 20))
		}
	}
}

func TestDeveloperOnlyEvents(t *testing.T) {
	for _, name := range []string{
		"decision.requested", "decision.resolved",
		"decision.candidates_ranked", // a later milestone's name inherits the gate
		"provider.trace.request",
	} {
		if !DeveloperOnlyEvent(name) {
			t.Errorf("%s must be developer-only (docs/spec/08: partial data redacted when not in developer mode)", name)
		}
	}
	for _, name := range []string{
		"job.started", "job.succeeded", "session.stopped", "speech.started",
		"events.resync_required", "system.heartbeat",
	} {
		if DeveloperOnlyEvent(name) {
			t.Errorf("%s must stay on a viewer's stream: without it the UI cannot show progress at all", name)
		}
	}
	// And the filter itself, at each role.
	viewer := &Identity{User: &StoredUser{ID: "u", Username: "v"}, Role: RoleViewer, Method: MethodCookie}
	dev := &Identity{User: &StoredUser{ID: "d", Username: "d"}, Role: RoleDeveloper, Method: MethodCookie}
	if CanSeeEvent(viewer, "decision.resolved") {
		t.Error("a viewer could see a decision event")
	}
	if !CanSeeEvent(dev, "decision.resolved") {
		t.Error("a developer could not see a decision event")
	}
	if !CanSeeEvent(viewer, "job.started") {
		t.Error("a viewer could not see an operational event")
	}
	if CanSeeEvent(nil, "decision.resolved") {
		t.Error("a nil identity could see a decision event")
	}
}
