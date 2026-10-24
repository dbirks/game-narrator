package api

// The roster's direct write surface (GitHub #42, ADR-0180): mint a row,
// update one row's editorial fields, retire one unreferenced row. The
// assertions read the rows back through the API (the wire) and through the
// store (the database), so the two cannot disagree. Every 2xx here is
// recorded for the contract gate and validated against the operation that
// documents it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
)

// putCharacterSend performs one PUT on the wire with the If-Match token the
// caller holds.
func putCharacterSend(t *testing.T, e *catalogEnv, profileID, cid string, body string, kv ...string) *httptest.ResponseRecorder {
	t.Helper()
	target := "/api/v1/profiles/" + profileID + "/characters/" + cid
	return e.send(auth.RoleOperator, http.MethodPut, target, strings.NewReader(body), kv...)
}

func TestCharacterCreateAndUpdate(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "character-api", "ca")

	// ── create: a row with the full editorial half ──────────────────────
	body := `{
		"name": "Harbormaster",
		"role": "narrator",
		"personality_summary": "A calm, weathered voice; speaks slowly and never raises it.",
		"aliases": ["harbourmaster"],
		"visual_descriptors": ["an elderly woman", "about seventy years old"]
	}`
	rec := e.send(auth.RoleOperator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/characters", strings.NewReader(body),
		"If-Match", e.etag(profileID))
	created := e.requireStatus(rec, http.StatusCreated, "POST …/characters")
	cid, _ := created["id"].(string)
	if cid == "" {
		t.Fatalf("the created row carries no id: %v", created)
	}
	if created["name"] != "Harbormaster" || created["role"] != "narrator" ||
		created["status"] != "proposed" {
		t.Errorf("created row = %v, want the named row, its role and the proposed default", created)
	}

	// The roster agrees, through the read the UI uses.
	roster := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/characters")
	found := mustFindCharacter(t, roster, cid)
	if found["personality_summary"] != "A calm, weathered voice; speaks slowly and never raises it." {
		t.Errorf("the roster's voice brief = %v, want the created brief", found["personality_summary"])
	}

	// ── update: the role moves, the brief stays, the name stays ──────────
	rec = putCharacterSend(t, e, profileID, cid, `{"role":"lead"}`, "If-Match", e.etag(profileID))
	updated := e.requireStatus(rec, http.StatusOK, "PUT …/characters/{cid}")
	if updated["role"] != "lead" {
		t.Errorf("updated role = %v, want lead", updated["role"])
	}
	if updated["name"] != "Harbormaster" || updated["personality_summary"] == "" {
		t.Errorf("the update must keep the fields it did not name: name=%v brief=%v",
			updated["name"], updated["personality_summary"])
	}

	// ── the refusals ─────────────────────────────────────────────────────
	// A PUT that changes nothing is a read, not a write.
	rec = putCharacterSend(t, e, profileID, cid, `{}`, "If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusBadRequest, "PUT an empty body")
	// POST without a name has nothing to mint.
	rec = e.send(auth.RoleOperator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/characters", strings.NewReader(`{}`),
		"If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusBadRequest, "POST without a name")
	// No If-Match is 428; a stale token is 412, the same pair the link keeps.
	rec = putCharacterSend(t, e, profileID, cid, `{"role":"minor"}`)
	e.requireStatus(rec, http.StatusPreconditionRequired, "PUT without If-Match")
	// "1" is a well-formed token this revision long outgrew: stale, the 412.
	rec = putCharacterSend(t, e, profileID, cid, `{"role":"minor"}`, "If-Match", "\"1\"")
	e.requireStatus(rec, http.StatusPreconditionFailed, "PUT with a stale token")
	// The role the write names must be one the schema carries.
	rec = putCharacterSend(t, e, profileID, cid, `{"status":"famous"}`, "If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusUnprocessableEntity, "PUT an unknown status")
	// Operator-tier: the curator the wildcard would otherwise admit is refused.
	rec = e.send(auth.RoleCurator, http.MethodPut,
		"/api/v1/profiles/"+profileID+"/characters/"+cid, strings.NewReader(`{"role":"minor"}`),
		"If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusForbidden, "PUT as a curator")
	rec = e.send(auth.RoleCurator, http.MethodDelete,
		"/api/v1/profiles/"+profileID+"/characters/"+cid, nil)
	e.requireStatus(rec, http.StatusForbidden, "DELETE as a curator")
	// Draft-only: a review revision is frozen for curation.
	if err := e.st.SetProfileStatus(e.ctx, profileID, games.StatusReview, mustAtoi(strings.Trim(e.etag(profileID), `"`))); err != nil {
		t.Fatalf("SetProfileStatus: %v", err)
	}
	rec = putCharacterSend(t, e, profileID, cid, `{"role":"minor"}`, "If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusConflict, "PUT on a review revision")
}

func TestCharacterDeleteRetiresWhenUnreferenced(t *testing.T) {

	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "character-del", "cd")

	body := `{"name": "Old Ferryman", "role": "minor"}`
	rec := e.send(auth.RoleOperator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/characters", strings.NewReader(body),
		"If-Match", e.etag(profileID))
	created := e.requireStatus(rec, http.StatusCreated, "POST …/characters")
	cid, _ := created["id"].(string)

	// The row holds no line, no attribution and no voice: it leaves the
	// roster, and the receipt says where it sits.
	del := func() *httptest.ResponseRecorder {
		return e.send(auth.RoleOperator, http.MethodDelete,
			"/api/v1/profiles/"+profileID+"/characters/"+cid, nil, "If-Match", e.etag(profileID))
	}
	rec = del()
	receipt := e.requireStatus(rec, http.StatusOK, "DELETE …/characters/{cid}")
	if receipt["status"] != "retired" || receipt["character_id"] != cid {
		t.Errorf("receipt = %v, want the retired row's id", receipt)
	}

	// The roster still carries it (the row stays for history), retired.
	roster := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/characters")
	found := mustFindCharacter(t, roster, cid)
	if found["status"] != "retired" {
		t.Errorf("the deleted row's status = %v, want retired", found["status"])
	}

	// Deleting it again is a conflict: it is already retired.
	rec = del()
	e.requireStatus(rec, http.StatusConflict, "DELETE an already-retired row")
	// A row the revision does not have is the 404 the reads answer.
	rec = e.send(auth.RoleOperator, http.MethodDelete,
		"/api/v1/profiles/"+profileID+"/characters/"+games.NewID(), nil, "If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusNotFound, "DELETE an unknown character")
	// The PUT of the same unknown row earns the same 404.
	rec = putCharacterSend(t, e, profileID, games.NewID(), `{"role":"minor"}`, "If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusNotFound, "PUT an unknown character")
}

func TestCharacterDeleteRefusesReferencedRows(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "character-ref", "cr")

	body := `{"name": "The Ferry", "role": "supporting"}`
	rec := e.send(auth.RoleOperator, http.MethodPost,
		"/api/v1/profiles/"+profileID+"/characters", strings.NewReader(body),
		"If-Match", e.etag(profileID))
	created := e.requireStatus(rec, http.StatusCreated, "POST …/characters")
	cid, _ := created["id"].(string)

	// A line that speaks the row keeps it: the DELETE answers 409 and the
	// row is still live.
	line := games.Message{
		ID:                    games.NewID(),
		ProfileID:             profileID,
		OriginalText:          "invented line held by the row",
		MarkupJSON:            json.RawMessage(`[]`),
		NormalizedText:        "invented line held by the row",
		SpeechText:            "invented line held by the row",
		Language:              "en",
		TextKind:              games.TextDialogue,
		SpeakerID:             cid,
		AttributionStatus:     games.AttributionProposed,
		AttributionConfidence: 1.0,
		AttributionEvidence:   json.RawMessage(`[]`),
		Flags:                 json.RawMessage(`{}`),
		Revision:              1,
		CreatedAt:             nowStamp(),
		UpdatedAt:             nowStamp(),
	}
	if err := e.st.PutMessages(e.ctx, profileID, []games.Message{line}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	rec = e.send(auth.RoleOperator, http.MethodDelete,
		"/api/v1/profiles/"+profileID+"/characters/"+cid, nil, "If-Match", e.etag(profileID))
	e.requireStatus(rec, http.StatusConflict, "DELETE a row with lines")

	roster := e.mustOK(auth.RoleCurator, "/api/v1/profiles/"+profileID+"/characters")
	found := mustFindCharacter(t, roster, cid)
	if found["status"] != "proposed" {
		t.Errorf("the refused delete must not move the row: status = %v", found["status"])
	}
}

// mustFindCharacter pulls one row out of the roster list the GET carries.
func mustFindCharacter(t *testing.T, roster map[string]any, cid string) map[string]any {
	t.Helper()
	list, ok := roster["characters"].([]any)
	if !ok {
		t.Fatalf("the roster has no character list: %v", roster)
	}
	for _, item := range list {
		row, _ := item.(map[string]any)
		if row["id"] == cid {
			return row
		}
	}
	t.Fatalf("the roster has no row with id %s", cid)
	return nil
}
