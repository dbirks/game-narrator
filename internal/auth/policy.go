package auth

import (
	"net"
	"net/http"
	"strings"
)

// Grant ties one route pattern to the minimum role that may call it. It is the
// row type of the scope table docs/spec/08 "Token/session scopes" implies and
// docs/spec/14 "role ACL" requires: the mapping is data, reviewed in one place,
// instead of a call wrapped differently in every handler file.
type Grant struct {
	// Method is the HTTP method, or "" to mean "any method".
	Method string
	// Pattern is a route pattern in net/http ServeMux syntax: "/api/v1/jobs/{id}"
	// matches one path segment for {id}; a trailing "/{rest...}" (or a final
	// segment ending in "...") matches everything below a prefix.
	Pattern string
	// Access is the requirement. Build it with Require(role) or Public().
	Access Access
}

// Policy resolves (method, path) to an Access. The zero Policy is not useful;
// use DefaultPolicy.
//
// Resolution is ordered and first-match-wins over rules whose method is either
// the request's method or "" (any). Order is therefore part of the contract —
// most specific first — which is written out in DefaultPolicy rather than
// derived from a specificity score, because a score silently reorders grants
// when somebody adds a pattern and that is precisely how a developer-only route
// ends up readable by a viewer.
//
// A path that matches nothing requires admin. Unknown-is-privileged is the only
// default that fails closed: a route added without a grant is then loudly
// inaccessible to everybody but an admin (who can fix the table) instead of
// quietly public (docs/spec/14 "Authorization").
type Policy struct {
	grants []Grant
}

// NewPolicy builds a Policy over the given order-sensitive grants.
func NewPolicy(grants ...Grant) *Policy { return &Policy{grants: grants} }

// DefaultPolicy is the appliance's scope table: the roles of docs/spec/08
// "API version and user authentication" applied to the routes of
// docs/spec/08 "APIs". It covers routes this build has registered *and* the
// ones a later milestone registers, so an endpoint arriving later cannot be
// born unauthenticated by omission; internal/api's tests assert the two sets
// agree for everything that is live (TestEveryRegisteredRouteIsCovered).
func DefaultPolicy() *Policy {
	return NewPolicy(
		// ── public: what a monitoring system and a browser need before login ──
		// docs/spec/08 makes health and system info unauthenticated; they carry
		// no game content, no paths and no secrets. Nothing else here is public:
		// docs/spec/14 forbids "unauthenticated screenshot/audio", and an
		// unauthenticated event or job stream leaks gameplay text.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/health/live", Access: Public()},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/health/ready", Access: Public()},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/health/{rest...}", Access: Public()},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/system/info", Access: Public()},
		// The host vitals of the diagnostics page (ADR-0087): CPU, RAM,
		// storage and thermal state — the system-info class of read (host
		// capacity, no game content, no paths, no secrets), public for the same
		// reason /diagnostics is a public page: a stalled appliance must be
		// inspectable before an account exists.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/diagnostics/host", Access: Public()},

		// The auth endpoints are public by necessity — but only the three that
		// cannot be guarded by the thing they produce. setup is additionally
		// loopback-only inside its handler (docs/spec/10 "Security defaults":
		// "Bind localhost on initial boot"), which is a transport check, not a
		// role, so it stays a Public grant.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/auth/status", Access: Public()},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/auth/login", Access: Public()},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/auth/setup", Access: Public()},

		// ── developer and admin surfaces, before the generic prefixes ──
		// docs/spec/08: "Developer mode can display sensitive gameplay text and
		// provider traces; require explicit role." docs/spec/10: emulator and
		// test-harness endpoints are dev-mode-only and must never be live on
		// UNO Q without auth. This is the rule that makes /api/v1/dev/* mean
		// something for routes that do not exist yet.
		Grant{Method: "", Pattern: "/api/v1/dev/{rest...}", Access: Require(RoleDeveloper)},
		Grant{Method: "", Pattern: "/api/v1/dev", Access: Require(RoleDeveloper)},

		// The managed replay clip store (ADR-0073). A clip is a recording of
		// gameplay frames — the same sensitive-evidence class as the dev-trace
		// store the /api/v1/dev prefix keeps behind the developer tier — and
		// installing or removing one changes what the runtime can replay, so
		// all three verbs sit at developer, not operator. The admin role
		// passes by the role order, which is how the on-device scripts reach
		// this surface with the admin token they already mint.
		Grant{Method: "", Pattern: "/api/v1/replay/{rest...}", Access: Require(RoleDeveloper)},

		// config, users and sealed provider credentials are admin (docs/spec/08
		// scopes; docs/spec/14 "Agent cannot update admin/gateway/secret config
		// … without role-protected explicit action").
		// The /settings page is the same tier: it renders the effective
		// configuration (model endpoints, storage, security, import policies,
		// audio sinks) and its backup download copies the appliance's whole
		// state, so a kiosk viewer or a session operator must not read either
		// (docs/spec/09 "/settings"; docs/spec/14 "Authorization").
		Grant{Method: http.MethodGet, Pattern: "/api/v1/settings", Access: Require(RoleAdmin)},
		Grant{Method: http.MethodPut, Pattern: "/api/v1/settings", Access: Require(RoleAdmin)},
		// The one runtime-mutable setting (ADR-0074): the audio output
		// selection and master volume persist in the store, and the env
		// variables keep their override precedence. It is admin, like the
		// settings page it feeds: choosing where speech plays is choosing a
		// physical output on the appliance.
		Grant{Method: http.MethodPut, Pattern: "/api/v1/settings/audio", Access: Require(RoleAdmin)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/admin/backup", Access: Require(RoleAdmin)},
		// The restore half of the backup/restore pair (ADR-0090): the upload
		// replaces the appliance's whole catalogue and the request restarts the
		// process, so it is admin, exactly like the backup download it consumes.
		// GET is the post-restart status read of the same action.
		Grant{Method: http.MethodPost, Pattern: "/api/v1/admin/restore", Access: Require(RoleAdmin)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/admin/restore", Access: Require(RoleAdmin)},
		// The update check's manual re-check (ADR-0088): admin, like the
		// settings page whose section it feeds (it also triggers a network
		// request on the appliance's behalf, which a kiosk viewer must not do).
		Grant{Method: http.MethodPost, Pattern: "/api/v1/settings/updates/refresh", Access: Require(RoleAdmin)},
		Grant{Method: "", Pattern: "/api/v1/config", Access: Require(RoleAdmin)},
		Grant{Method: "", Pattern: "/api/v1/config/{rest...}", Access: Require(RoleAdmin)},
		Grant{Method: "", Pattern: "/api/v1/secrets/{rest...}", Access: Require(RoleAdmin)},
		Grant{Method: "", Pattern: "/api/v1/users", Access: Require(RoleAdmin)},
		Grant{Method: "", Pattern: "/api/v1/users/{rest...}", Access: Require(RoleAdmin)},
		Grant{Method: "", Pattern: "/api/v1/tokens/{rest...}", Access: Require(RoleAdmin)},

		// ── operator: control of the running appliance ──
		// Session lifecycle, capture and audio are the "act on the physical
		// world" tier (docs/spec/10: no unauthenticated capture preview or TTS
		// abuse). Reads of the same resources stay lower.
		//
		// Reading one session's state is a viewer read (ADR-0024): the snapshot
		// names profile, mode, counters and terminal state — no gameplay text,
		// which stays on the developer-only event pages and dev-trace assets.
		// It must sit before the generic operator rule below: Require scans in
		// table order and the first matching grant wins, so a later viewer rule
		// would be dead code that reads as if it applied.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/sessions/{id}", Access: Require(RoleViewer)},
		Grant{Method: "", Pattern: "/api/v1/sessions", Access: Require(RoleOperator)},
		Grant{Method: "", Pattern: "/api/v1/sessions/{rest...}", Access: Require(RoleOperator)},
		Grant{Method: "", Pattern: "/api/v1/capture/{rest...}", Access: Require(RoleOperator)},
		Grant{Method: "", Pattern: "/api/v1/audio/{rest...}", Access: Require(RoleOperator)},
		Grant{Method: "", Pattern: "/api/v1/frame-sources/{rest...}", Access: Require(RoleOperator)},
		// The context-aware assistant (ADR-0099): the dock is an operator
		// surface. A curator can read the catalogue but may not drive the
		// agent (every dock action is a mutation or a developer-tier evidence
		// read); a developer or admin inherits operator and up. The whole
		// prefix is operator — the per-tool tiers (get_trace at developer)
		// are the tools' own MinRole's business, enforced in the loop.
		Grant{Method: "", Pattern: "/api/v1/assistant/{rest...}", Access: Require(RoleOperator)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/jobs/{id}/cancel", Access: Require(RoleOperator)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/jobs/{id}/retry", Access: Require(RoleOperator)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/diagnostics/run", Access: Require(RoleOperator)},
		// ── curator: content and voice curation, import, publication ──
		// docs/spec/08's publish/import/audition/approve/onboarding routes;
		// docs/spec/14's rights review makes publication a deliberate,
		// role-gated act rather than something a kiosk viewer can trigger.
		//
		// The voice *registry* is curator-and-above on reads as well: docs/spec/14's
		// acceptance test names "voice registry" as something a viewer must be
		// denied, and docs/spec/07 explains why — replica registration state names
		// upstream endpoints and cloned-voice identifiers, which are not library
		// data. Which voices exist at all is a read (a viewer page has to label the
		// voice it is about to play), so the list stays at viewer.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/voices", Access: Require(RoleViewer)},
		// The generic kind voices (issue #51, ADR-0185): reading the kind
		// coverage is a read like the voice list; the create-missing run is
		// curation — it starts design work and takes land for the operator.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/voices/kinds", Access: Require(RoleViewer)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/voices/kinds/create-missing", Access: Require(RoleCurator)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/voices/{id}", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/voices/{id}/registrations", Access: Require(RoleCurator)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/voices/{id}/register", Access: Require(RoleOperator)},
		Grant{Method: "", Pattern: "/api/v1/voices/{id}/approve", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/voices/{id}/preview", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/voices/{id}/auditions", Access: Require(RoleCurator)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/voices/auditions", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/voices/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/voices", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/onboarding/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/corrections", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/corrections/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/import/{rest...}", Access: Require(RoleCurator)},

		// games/profiles: reads are viewer, writes are curator. Listed read-first
		// because method-disjoint rules never compete. The collection POST is
		// spelled out rather than inherited from /api/v1/games/{rest...}, which
		// — like net/http's own {name...} — claims one or more segments and so
		// does not cover /api/v1/games itself (docs/spec/08 "POST /api/v1/games":
		// adding a game to the library is curation).
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games", Access: Require(RoleViewer)},
		Grant{Method: http.MethodPost, Pattern: "/api/v1/games", Access: Require(RoleCurator)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games/{id}", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games/{id}/profiles", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games/{id}/download", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games/{id}/export", Access: Require(RoleCurator)},
		// The catalog reads of ADR-0017, spelled out because the generic
		// /api/v1/games/{rest...} and /api/v1/profiles/{rest...} rules below are
		// *curator* rules — correct for writes, wrong for the library browser a
		// viewer is (docs/spec/09): a viewer page has to be able to read the
		// library, an edition's revisions and a revision's dialogue.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games/{id}/editions", Access: Require(RoleViewer)},
		// The /library screen's per-game read (ADR-0140): the shelf is the
		// kiosk's front door, so the actionable revision and the counts are
		// viewer reads like the rest of the library browser, spelled out
		// before the curator wildcard below.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/games/{id}/library", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/editions/{id}/profiles", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/editions/{id}/published", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/messages", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/messages/search", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/publish-actions", Access: Require(RoleViewer)},
		// An export is viewer-readable because its default form is the
		// rights-filtered manifest (docs/spec/02 "Profile portability"): text
		// derived from a source whose rights position is unresolved is withheld
		// by profileio itself, and the .gnpack form that carries the bytes needs
		// the copyright acknowledgement the route refuses to supply on a
		// caller's behalf.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/export", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/characters", Access: Require(RoleViewer)},
		// The cast audit (ADR-0171): read-only, so a viewer may see the
		// expected-vs-measured voice class per roster row.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/cast/audit", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/regions", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/graph", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/profiles/{id}/messages/{mid}", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/jobs", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/jobs/{id}", Access: Require(RoleViewer)},
		// The redacted log tail (ADR-0087): system logs can carry provider
		// request detail, so they sit at the same tier as the dev-trace
		// evidence route (docs/spec/14); the concrete path must grant before
		// the /diagnostics/{id} template below, which would answer viewer.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/diagnostics/logs", Access: Require(RoleDeveloper)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/diagnostics/{id}", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/events", Access: Require(RoleViewer)},
		// The global status bar's read (docs/design/plum-peach "Global
		// chrome"): the bar is on every screen, kiosk included, so the
		// snapshot and its push stream are viewer reads like the session
		// state they summarise. The push stream carries no gameplay text —
		// a status string, counters and health levels — which is what keeps
		// it viewer-tier rather than the developer-tier event stream it is
		// carried on.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/status", Access: Require(RoleViewer)},
		Grant{Method: http.MethodGet, Pattern: "/api/v1/status/stream", Access: Require(RoleViewer)},
		// The ⌘K command-palette search (ADR-0098, docs/design/plum-peach
		// UX-02): the palette sits on every screen, kiosk included, so the
		// route itself is a viewer read like the status bar. The narrowing
		// happens per result inside the handler, not in this table: trace rows
		// are produced only for developer and above (the developer-evidence
		// tier, docs/spec/08), a settings section only for the role that may
		// open the page it links to, and each action verb only for the role it
		// names.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/search", Access: Require(RoleViewer)},
		// The contract is how a client learns to authenticate, so it cannot sit
		// behind the credential it describes. It carries no game content, no
		// paths and no secrets, and docs/spec/09's generated TypeScript client
		// is built from this file (docs/spec/08 "Conventions", ADR-0017). This is
		// the one ADR-0016 grant Milestone 1 widened; the fail-closed default for
		// an *unknown* path is untouched.
		Grant{Method: http.MethodGet, Pattern: "/api/v1/openapi.json", Access: Public()},

		// Anything else under a content prefix — including every write to a game,
		// edition, profile, source or publication route that a later milestone
		// registers — is curator. The /api/v1/editions prefix joins games and
		// profiles here for the same reason: an edition's revisions are curated
		// content, and the readable half of it is enumerated above.
		// The purge (ADR-0107) is the one profile write the curator wildcard
		// must NOT swallow: it is admin, and the concrete grant sits before
		// the wildcard so first-match keeps the boundary in the table, not in
		// the handler.
		Grant{Method: http.MethodPost, Pattern: "/api/v1/profiles/{id}/purge", Access: Require(RoleAdmin)},
		// The roster's direct writes (GitHub #42, ADR-0180): they touch every
		// editorial field at once — including the voice brief the speech
		// pipeline grounds on — so they sit a tier above the curator wildcard
		// that the same prefix would otherwise answer with. The GET stays
		// viewer, listed above.
		Grant{Method: http.MethodPost, Pattern: "/api/v1/profiles/{id}/characters", Access: Require(RoleOperator)},
		Grant{Method: http.MethodPut, Pattern: "/api/v1/profiles/{id}/characters/{cid}", Access: Require(RoleOperator)},
		Grant{Method: http.MethodDelete, Pattern: "/api/v1/profiles/{id}/characters/{cid}", Access: Require(RoleOperator)},
		// The speech-cache prune (ADR-0159, issue #49): removes the transient
		// cache entries no current published profile revision can hit. It is a
		// content-tier maintenance verb like pre-synthesis, so curator — the
		// role that published the revision the dead entries belonged to.
		Grant{Method: http.MethodPost, Pattern: "/api/v1/speech/cache/prune", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/games/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/editions/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/profiles/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/sources/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/scenes/{rest...}", Access: Require(RoleCurator)},
		Grant{Method: "", Pattern: "/api/v1/jobs/{rest...}", Access: Require(RoleOperator)},

		// Any authenticated principal may reach the auth self-service routes
		// (me, logout, password). It sits after the public auth grants so it
		// never overrides them, and catches "/api/v1/auth/anything-new" closed to
		// the lowest real role rather than to admin.
		Grant{Method: "", Pattern: "/api/v1/auth/{rest...}", Access: Require(RoleViewer)},

		// The durable queue's remaining routes (POST /api/v1/jobs, whenever the
		// policy question is answered) are operator, via the jobs prefix above.
	)
}

// Require resolves the requirement for one request. The second result reports
// whether a table entry matched at all; false means the caller is looking at the
// fail-closed default (admin) and internal/api surfaces that as the reason in
// the 403 detail.
func (p *Policy) Require(method, path string) (Access, bool) {
	if p == nil {
		return Require(RoleAdmin), false
	}
	for _, g := range p.grants {
		if g.Method != "" && !strings.EqualFold(g.Method, method) {
			continue
		}
		if MatchPattern(g.Pattern, path) {
			return g.Access, true
		}
	}
	return Access{
		Min:    RoleAdmin,
		Reason: "no grant in the scope table matches this path, so it requires admin (fail-closed default)",
	}, false
}

// RequireAnyMethod resolves a path ignoring the verb, reporting the *most
// permissive* grant that names it. authorize uses it for one purpose: a request
// whose method is not offered on a public path (POST /api/v1/health/live) must be
// answered 405 by the router, as docs/spec/08's error contract requires, rather
// than 401 by a fail-closed default that would leak nothing but confuse the
// caller. Nothing reaches a handler either way — no method+path pattern is
// registered for the pair — so widening the answer here cannot widen access.
func (p *Policy) RequireAnyMethod(path string) (Access, bool) {
	if p == nil {
		return Access{}, false
	}
	var out Access
	found := false
	for _, g := range p.grants {
		if !MatchPattern(g.Pattern, path) {
			continue
		}
		if !found || g.Access.Min < out.Min {
			out, found = g.Access, true
		}
	}
	return out, found
}

// Grants exposes the table, in order, for the developer UI, `doctor` and the
// matrix test that walks every route at every role.
func (p *Policy) Grants() []Grant {
	if p == nil {
		return nil
	}
	out := make([]Grant, len(p.grants))
	copy(out, p.grants)
	return out
}

// MatchPattern reports whether path is claimed by a route pattern in ServeMux
// syntax. It is exported because internal/api's 405 fallback and its
// route-coverage test need the same notion of "does this pattern claim that
// path?" as the policy, and two implementations of that question is how a
// pattern stops being protected by the grant written for it.
func MatchPattern(pattern, path string) bool {
	if pattern == path {
		return true
	}
	psegs := splitPath(pattern)
	dsegs := splitPath(path)

	// Go's syntax for a multi-segment wildcard is a final segment of the form
	// {name...} — the dots are inside the braces, so testing for a trailing
	// "..." alone would silently turn every deep pattern into a single-segment
	// one and leave everything below it unmatched (i.e. ungranted, i.e. admin-
	// only, which fails closed but breaks the route it was written for).
	deep := len(psegs) > 0 && isDeepWildcard(psegs[len(psegs)-1])
	if deep {
		psegs = psegs[:len(psegs)-1]
		// One or more segments, exactly as net/http documents {name...}: the
		// prefix alone is not a match, so "/api/v1/dev/{rest...}" claims
		// /api/v1/dev/traces but not /api/v1/dev, which needs its own grant.
		if len(dsegs) <= len(psegs) {
			return false
		}
	} else if len(dsegs) != len(psegs) {
		return false
	}

	for i, seg := range psegs {
		if !matchSegment(seg, dsegs[i]) {
			return false
		}
	}
	return true
}

func isDeepWildcard(seg string) bool {
	return strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "...}")
}

func splitPath(p string) []string {
	return strings.Split(strings.Trim(p, "/"), "/")
}

func matchSegment(pattern, segment string) bool {
	if strings.HasPrefix(pattern, "{") {
		// {id} matches exactly one segment; {rest...} was already stripped by
		// the caller, so anything else brace-shaped is a single-segment wildcard.
		return true
	}
	return pattern == segment
}

// IsLoopbackRemote reports whether the peer address of a request is the machine
// itself: 127.0.0.0/8 or ::1 (docs/spec/10 "Bind localhost on initial boot").
//
// It reads only the transport peer address. X-Forwarded-For and friends are
// attacker-supplied by default, and first-run setup is the one request in the
// system whose outcome is "somebody now owns this appliance": trusting a header
// to decide it would let any LAN host answer the setup form (docs/spec/14).
func IsLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	host = strings.Trim(host, "[]")
	if zone := strings.IndexByte(host, '%'); zone >= 0 {
		host = host[:zone]
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	// IsLoopback covers 127.0.0.0/8 and ::1, and also reports true for an
	// IPv4-mapped IPv6 loopback, which is what a dual-stack listener hands a
	// local client. It deliberately answers false for ::ffff:192.168.x.x.
	return ip.IsLoopback()
}
