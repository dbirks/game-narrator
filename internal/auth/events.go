package auth

import (
	"strings"

	"github.com/dbirks/game-narrator/internal/events"
)

// Event-stream confidentiality (docs/spec/08 "Event stream reliability":
// "Partial data redacted when not in developer mode"; docs/spec/14 "Privacy":
// "developer diagnostics can reveal game content").
//
// The rule the SSE route enforces: a principal below developer must never
// receive an event whose payload carries recognised game text or a provider
// trace. Everything else on the stream — job lifecycle, session state, speech
// timings, health changes — is operational and is what a viewer page needs to
// paint anything at all.
//
// It is expressed as a prefix deny-list rather than a per-name allow-list
// because the registry is open: internal/events.Register is called by whichever
// subsystem first needs a name, so a milestone after this one can add
// decision.context_compressed or provider.trace_upload without touching this
// file, and it inherits the gate. A new developer-only *family* must be added
// here deliberately — and that is the property worth having: widening the
// stream is a visible edit to a security table, while adding a name to a
// feature package is not.
var developerOnlyEventPrefixes = []string{
	// docs/spec/06's SystemOne decision envelope: candidate lines, ranked
	// choices and the raw provider request/response correlation. This is the
	// game's text and somebody else's inference traffic. The whole family is
	// gated, so decision.requested, decision.resolved and any later
	// decision.* inherit the rule.
	"decision.",
	// Provider traffic traces arrive as their own family when the runtime task
	// registers them; claimed now so they cannot be born public.
	"provider.trace.",
}

// DeveloperOnlyEvent reports whether an event name may only reach a developer
// or admin stream.
func DeveloperOnlyEvent(name string) bool {
	for _, prefix := range developerOnlyEventPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	// The two names docs/spec/14's acceptance test names, matched exactly so the
	// deny-list does not depend on the trailing-dot convention above.
	switch name {
	case events.DecisionRequested, events.DecisionResolved:
		return true
	}
	return false
}

// CanSeeEvent reports whether an identity may receive a named event. GET
// /api/v1/events requires viewer, and this is the second, narrower filter
// applied to whatever that subscription then carries.
func CanSeeEvent(id *Identity, name string) bool {
	if !DeveloperOnlyEvent(name) {
		return true
	}
	return id.Authenticated() && id.Role.AtLeast(RoleDeveloper)
}
