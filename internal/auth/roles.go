// Package auth owns Game Narrator's authentication and authorization domain:
// the ordered role set, password hashing, session and API-token credentials,
// the CSRF rule, the route→role scope table and the login rate limiter
// (docs/spec/08-api-and-events.md "API version and user authentication",
// docs/spec/14-security-and-content.md "Mandatory controls: Authorization",
// ADR-0016).
//
// It deliberately does not contain SQL. Persistence lives in
// internal/store.AuthStore, which this package reaches through the Repo
// interface: that keeps the credential rules (what a role means, when a session
// dies, what a CSRF failure looks like) testable without a database, and keeps
// the database free of policy it would otherwise duplicate and drift from.
//
// Two rules are absolute in this package and its callers:
//
//   - A raw credential (password, session token, API token, CSRF token) is
//     never stored, never returned by a lookup, and never logged. Types are
//     shaped so there is nowhere to put one: store rows carry digests, and
//     the raw values this package mints are returned once, by the call that
//     creates them.
//   - Every path that is not explicitly granted a role requires admin. An
//     unrecognised route is not "public until somebody says otherwise"
//     (Policy.Required, docs/spec/14).
package auth

import (
	"fmt"
	"strings"
)

// Role is one of the five scopes of docs/spec/08 "API version and user
// authentication". The zero value is RoleNone ("no principal established"),
// which is not a role a user can hold — it is what an unauthenticated request
// carries, so `identity.Role.AtLeast(min)` is a complete authorization check
// with no separate nil case.
type Role int

// The ordered role set: viewer < curator < operator < developer < admin. The
// numeric order is the grant order (docs/spec/08), so AtLeast is a comparison
// and not a lookup table.
const (
	RoleNone Role = iota
	RoleViewer
	RoleCurator
	RoleOperator
	RoleDeveloper
	RoleAdmin
)

// RoleNames maps each role onto the wire/schema spelling used by
// 0006_auth.sql's CHECK enum and docs/spec/08.
var RoleNames = map[Role]string{
	RoleNone:      "none",
	RoleViewer:    "viewer",
	RoleCurator:   "curator",
	RoleOperator:  "operator",
	RoleDeveloper: "developer",
	RoleAdmin:     "admin",
}

// GrantFor is the one-paragraph statement of what a role may do. It is exported
// for the ADR, `doctor`, and the developer UI's own role legend, so the prose
// an operator reads is generated from the same table the middleware enforces.
var GrantFor = map[Role]string{
	RoleViewer:    "read the library, profiles, jobs and non-developer events",
	RoleCurator:   "additionally edit/import/publish profiles and manage voices",
	RoleOperator:  "additionally control sessions, capture and audio; cancel and retry jobs",
	RoleDeveloper: "additionally read /api/v1/dev/*, raw provider traces and developer-only events",
	RoleAdmin:     "additionally manage users, API tokens and configuration",
}

// String returns the wire spelling of the role.
func (r Role) String() string {
	if name, ok := RoleNames[r]; ok {
		return name
	}
	return fmt.Sprintf("role(%d)", int(r))
}

// ParseRole reads a wire role name. An unknown name is an error rather than
// RoleNone: a caller that mistyped "devloper" must be told, because silently
// downgrading it to "no privileges" turns a configuration bug into an
// inexplicable 403 later, and silently accepting it would put an unenforced
// string in the database.
func ParseRole(name string) (Role, error) {
	for role, wire := range RoleNames {
		if role != RoleNone && strings.EqualFold(name, wire) {
			return role, nil
		}
	}
	return RoleNone, fmt.Errorf("auth: %q is not a role (viewer, curator, operator, developer, admin)", name)
}

// MustParseRole is ParseRole for compile-time-constant names (grants, tests).
func MustParseRole(name string) Role {
	r, err := ParseRole(name)
	if err != nil {
		panic(err)
	}
	return r
}

// AtLeast reports whether r grants at least min. RoleNone satisfies only
// RoleNone, so an unauthenticated request passes a public check and nothing
// else.
func (r Role) AtLeast(min Role) bool { return r >= min }

// Valid reports whether r is a role a user or token can hold.
func (r Role) Valid() bool { return r >= RoleViewer && r <= RoleAdmin }

// Ordered returns the roles from lowest to highest, for tables and tests.
func Ordered() []Role {
	return []Role{RoleViewer, RoleCurator, RoleOperator, RoleDeveloper, RoleAdmin}
}

// Access is the authorization requirement attached to one route.
type Access struct {
	// Min is the lowest role that may call the route. RoleNone marks a public
	// route (docs/spec/08 "Conventions" + "no unauthenticated screenshot/audio"
	// in docs/spec/14 constrain which routes may be public: health, system
	// info, and the auth endpoints themselves).
	Min Role
	// Reason names why the route sits where it does, for the developer UI and
	// for a 403 detail that tells an operator what to ask for instead of
	// guessing.
	Reason string
}

// Require is the per-route grant used by the scope table: the route needs at
// least min.
func Require(min Role) Access { return Access{Min: min} }

// Public marks a route that needs no principal.
func Public() Access { return Access{Min: RoleNone} }

// IsPublic reports whether the route answers unauthenticated.
func (a Access) IsPublic() bool { return a.Min == RoleNone }

// Allows reports whether role satisfies the requirement.
func (a Access) Allows(role Role) bool { return role.AtLeast(a.Min) }

func (a Access) String() string {
	if a.IsPublic() {
		return "public"
	}
	return "role>=" + a.Min.String()
}
