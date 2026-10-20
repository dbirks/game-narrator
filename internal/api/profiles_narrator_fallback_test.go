package api

// The narrator-fallback toggle on the wire (issue #11, ADR-0101): the
// per-profile dial that decides whether a line whose speaker has no approved
// voice is spoken by the narrator voice. The token contract is the
// profile-write one (428 absent, 412 stale) even though the store write does
// not bump the row version: the If-Match answers "was this dial set against
// the revision you last read", and the 200 answers with the unchanged token.

import (
	"net/http"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

func TestNarratorFallbackToggle(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "narrator-fallback", "narrfb")
	target := "/api/v1/profiles/" + profileID + "/narrator-fallback"

	// The dial defaults on: the profile read says so before anyone touches it.
	first := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID)
	if first["narrator_fallback"] != true {
		t.Fatalf("the fresh draft's narrator_fallback = %v, want true (the migration default)", first["narrator_fallback"])
	}

	// The token contract: absent is 428, and the body's missing field is 400
	// with the field named — a dial set to an absence would silently turn the
	// narrator off.
	requireProblem(t, e.post(auth.RoleCurator, target, `{"enabled":false}`),
		http.StatusPreconditionRequired, codeRevisionConflict)
	requireProblem(t, e.post(auth.RoleCurator, target, `{}`, "If-Match", etag),
		http.StatusBadRequest, codeInvalidParameter)
	requireProblem(t, e.post(auth.RoleCurator, target, `{"enabled":"on"}`, "If-Match", etag),
		http.StatusBadRequest, codeInvalidParameter)

	// Off, with the current token: 200, the DTO answers with the new value,
	// and the ETag does not move (the write is not a content write).
	rec := e.post(auth.RoleCurator, target, `{"enabled":false}`, "If-Match", etag)
	body := e.requireStatus(rec, http.StatusOK, "narrator-fallback off")
	if body["narrator_fallback"] != false {
		t.Fatalf("the 200 answers narrator_fallback = %v, want false", body["narrator_fallback"])
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("the ETag moved %s -> %s: the dial is an editorial setting, not content", etag, got)
	}
	// The store read agrees with the wire answer.
	p, _, err := e.st.GetProfile(e.ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p.NarratorFallback {
		t.Fatalf("the store still holds narrator_fallback on after the toggle")
	}

	// A stale token is 412, and the 412 carries the current one for recovery —
	// which is the same token, because the write moved nothing.
	requireProblem(t, e.post(auth.RoleCurator, target, `{"enabled":true}`, "If-Match", `"9001"`),
		http.StatusPreconditionFailed, codeRevisionConflict)

	// Back on, with the token the toggle left in place.
	rec = e.post(auth.RoleCurator, target, `{"enabled":true}`, "If-Match", etag)
	body = e.requireStatus(rec, http.StatusOK, "narrator-fallback on")
	if body["narrator_fallback"] != true {
		t.Fatalf("the 200 answers narrator_fallback = %v, want true", body["narrator_fallback"])
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("the ETag moved to %s after the second toggle, want %s", got, etag)
	}
}
