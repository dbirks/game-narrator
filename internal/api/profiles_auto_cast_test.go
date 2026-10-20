package api

// The auto-cast toggle on the wire (ADR-0109, UX-03): the per-profile dial
// that decides whether a voice audition run created for one of the profile's
// characters auto-approves its take once the ASR QA read-back is not failed.
// The token contract is the profile-write one (428 absent, 412 stale) even
// though the store write does not bump the row version: the If-Match answers
// "was this dial set against the revision you last read", and the 200 answers
// with the unchanged token.

import (
	"net/http"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

func TestAutoCastToggle(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "auto-cast", "autocast")
	target := "/api/v1/profiles/" + profileID + "/auto-cast"

	// The dial defaults on: the profile read says so before anyone touches it
	// (the product default — a fresh profile casts).
	first := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID)
	if first["auto_cast"] != true {
		t.Fatalf("the fresh draft's auto_cast = %v, want true (the migration default)", first["auto_cast"])
	}

	// The token contract: absent is 428, and the body's missing field is 400
	// with the field named — a dial set to an absence would silently stop the
	// cast.
	requireProblem(t, e.post(auth.RoleCurator, target, `{"enabled":false}`),
		http.StatusPreconditionRequired, codeRevisionConflict)
	requireProblem(t, e.post(auth.RoleCurator, target, `{}`, "If-Match", etag),
		http.StatusBadRequest, codeInvalidParameter)
	requireProblem(t, e.post(auth.RoleCurator, target, `{"enabled":"off"}`, "If-Match", etag),
		http.StatusBadRequest, codeInvalidParameter)

	// Off, with the current token: 200, the DTO answers with the new value,
	// and the ETag does not move (the write is not a content write).
	rec := e.post(auth.RoleCurator, target, `{"enabled":false}`, "If-Match", etag)
	body := e.requireStatus(rec, http.StatusOK, "auto-cast off")
	if body["auto_cast"] != false {
		t.Fatalf("the 200 answers auto_cast = %v, want false", body["auto_cast"])
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("the ETag moved %s -> %s: the dial is an editorial setting, not content", etag, got)
	}
	// The store read agrees with the wire answer.
	p, _, err := e.st.GetProfile(e.ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if p.AutoCast {
		t.Fatalf("the store still holds auto_cast on after the toggle")
	}

	// A stale token is 412, and the 412 carries the current one for recovery —
	// which is the same token, because the write moved nothing.
	requireProblem(t, e.post(auth.RoleCurator, target, `{"enabled":true}`, "If-Match", `"9001"`),
		http.StatusPreconditionFailed, codeRevisionConflict)

	// Back on, with the token the toggle left in place.
	rec = e.post(auth.RoleCurator, target, `{"enabled":true}`, "If-Match", etag)
	body = e.requireStatus(rec, http.StatusOK, "auto-cast on")
	if body["auto_cast"] != true {
		t.Fatalf("the 200 answers auto_cast = %v, want true", body["auto_cast"])
	}
	if got := rec.Header().Get("ETag"); got != etag {
		t.Fatalf("the ETag moved to %s after the second toggle, want %s", got, etag)
	}

	// A revision cut from the opted-out profile inherits the dial: the cast
	// a curator turned off does not come back on the next revision.
	if err := e.st.SetProfileAutoCast(e.ctx, profileID, false); err != nil {
		t.Fatalf("set off before snapshot: %v", err)
	}
	child, err := e.st.NewRevision(e.ctx, profileID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if child.AutoCast {
		t.Fatalf("the snapshot's auto_cast = on, want the parent's off inherited")
	}
}
