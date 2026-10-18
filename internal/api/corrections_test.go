package api

// The correction labels and the regression fixtures they are (ADR-0052,
// docs/spec/08 "user fixes become labels", docs/spec/16 M5 "correction
// produces regression fixture"). The flows use the real router and the real
// store; every 2xx body is validated against the documented operation by the
// catalog environment's send helpers (contract clause (c)).

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// patchSpeechText is the editor's own save, the correction the ADR names.
func patchSpeechText(t *testing.T, e *catalogEnv, profileID, mid, text, etag string) map[string]any {
	t.Helper()
	target := "/api/v1/profiles/" + profileID + "/messages/" + mid
	return e.requireStatus(e.patch(auth.RoleCurator, target,
		fmt.Sprintf(`{"speech_text":%q}`, text), "If-Match", etag),
		http.StatusOK, "PATCH …/messages/{mid}")
}

// TestEditorCorrectionProducesTheRegressionFixture is the M5 acceptance on the
// API side: a message correction in the editor lands a correction row with the
// before/after texts, and both read views serve it.
func TestEditorCorrectionProducesTheRegressionFixture(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "correction-walk", "corr")

	dto, row := firstLine(t, e, profileID, "dialogue")
	mid := dto["id"].(string)

	const corrected = "The harbour bell rang twice, and no one looked up."
	before := row.SpeechText
	if before == corrected {
		t.Fatalf("fixture coincidence: the line already reads %q", corrected)
	}
	patchSpeechText(t, e, profileID, mid, corrected, etag)

	// The curator's view: the label is there, newest first, with the fixture's
	// before/after pair and the acting curator as actor.
	got := e.mustOK(auth.RoleCurator, "/api/v1/corrections?profile_id="+profileID)
	items := mustList(got["items"])
	if len(items) != 1 {
		t.Fatalf("the list has %d rows, want the one recorded by the save", len(items))
	}
	c, _ := items[0].(map[string]any)
	if c["kind"] != "speech_text" {
		t.Errorf("kind = %v, want speech_text", c["kind"])
	}
	if c["target_type"] != "message" || c["target_id"] != mid || c["message_id"] != mid {
		t.Errorf("target = %v / %v / %v, want the corrected message row", c["target_type"], c["target_id"], c["message_id"])
	}
	if c["before_value"] != before {
		t.Errorf("before_value = %q, want the text the editor replaced", c["before_value"])
	}
	if c["after_value"] != corrected {
		t.Errorf("after_value = %q, want the corrected text", c["after_value"])
	}
	if c["status"] != "accepted" {
		t.Errorf("status = %v, want accepted (the fix is applied by the same save)", c["status"])
	}
	if c["actor"] != "curator" || c["reviewer"] != "curator" {
		t.Errorf("actor/reviewer = %v / %v, want the acting curator", c["actor"], c["reviewer"])
	}

	// The developer's fixture view: the same row — the reading is the
	// difference, not the data.
	fixtures := e.mustOK(auth.RoleDeveloper, "/api/v1/dev/fixtures?profile_id="+profileID)
	fitems := mustList(fixtures["items"])
	if len(fitems) != 1 {
		t.Fatalf("the fixture list has %d rows, want the one the save recorded", len(fitems))
	}
	fc, _ := fitems[0].(map[string]any)
	if fc["id"] != c["id"] || fc["before_value"] != before || fc["after_value"] != corrected {
		t.Errorf("the fixture view disagrees with the label list:\n%v\n%v", fc, c)
	}

	// The store agrees with the wire, outside the page: the row is a real
	// corrections row, not a view the API invented.
	stored, err := e.st.ListCorrections(e.ctx, store.CorrectionFilter{ProfileID: profileID})
	if err != nil {
		t.Fatalf("ListCorrections: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("the store holds %d rows, want 1", len(stored))
	}
	if stored[0].ID != c["id"] {
		t.Errorf("store row %s is not the wire row %v", stored[0].ID, c["id"])
	}
}

// TestEditorNoOpSaveRecordsNothing: a save that leaves the text unchanged is
// not a correction, and an unchanged save must not grow the label queue.
func TestEditorNoOpSaveRecordsNothing(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "correction-noop", "noop")
	dto, row := firstLine(t, e, profileID, "dialogue")

	patchSpeechText(t, e, profileID, dto["id"].(string), row.SpeechText, etag)
	got := e.mustOK(auth.RoleCurator, "/api/v1/corrections?profile_id="+profileID)
	if items := mustList(got["items"]); len(items) != 0 {
		t.Fatalf("an unchanged save recorded %d correction(s)", len(items))
	}
}

// TestCreateCorrectionRecordsLabelsAndRefusesTheUnsafe says what the explicit
// surface is for: a label only, applied to nothing, validated against the
// revision it names.
func TestCreateCorrectionRecordsLabelsAndRefusesTheUnsafe(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "correction-post", "post")
	dto, _ := firstLine(t, e, profileID, "dialogue")
	mid := dto["id"].(string)

	// A message target with a real row: recorded as proposed, actor the caller.
	rec := e.requireStatus(e.post(auth.RoleCurator, "/api/v1/corrections", fmt.Sprintf(`{
		"profile_id": %q,
		"kind": "text",
		"target_type": "message",
		"target_id": %q,
		"before_value": "what the source said",
		"after_value": "what it should say",
		"reason": "the source dump mangled the punctuation",
		"evidence": ["frame:01M56S0J7PWYJ8D8PCKHGDXJBR00"]
	}`, profileID, mid)), http.StatusCreated, "POST /api/v1/corrections")
	c, _ := rec["correction"].(map[string]any)
	if c["status"] != "proposed" {
		t.Errorf("explicit recording = %v, want proposed (the recorder does not review)", c["status"])
	}
	if c["actor"] != "curator" {
		t.Errorf("actor = %v, want the caller", c["actor"])
	}
	if reviewer, present := c["reviewer"]; present && reviewer != "" {
		t.Errorf("a proposed row carries a reviewer: %v", reviewer)
	}
	ev := mustList(c["evidence"])
	if len(ev) != 1 {
		t.Errorf("evidence = %v, want the one frame id", ev)
	} else if !strings.Contains(ev[0].(string), "frame:") {
		t.Errorf("evidence lost its content: %v", ev)
	}

	// Unknown kind: a 400 naming the vocabulary, nothing recorded.
	bad := e.post(auth.RoleCurator, "/api/v1/corrections", fmt.Sprintf(`{
		"profile_id": %q, "kind": "vibes", "target_type": "message", "target_id": %q, "before_value": "x"
	}`, profileID, mid))
	requireProblem(t, bad, http.StatusBadRequest, codeInvalidParameter)

	// A message target on a row the revision does not have: a 404.
	missing := e.post(auth.RoleCurator, "/api/v1/corrections", fmt.Sprintf(`{
		"profile_id": %q, "kind": "text", "target_type": "message", "target_id": %q, "before_value": "x"
	}`, profileID, games.NewID()))
	requireProblem(t, missing, http.StatusNotFound, codeNotFound)

	// message_id that disagrees with target_id: a 400, not a silent label.
	mismatch := e.post(auth.RoleCurator, "/api/v1/corrections", fmt.Sprintf(`{
		"profile_id": %q, "kind": "text", "target_type": "message", "target_id": %q,
		"message_id": %q, "before_value": "x"
	}`, profileID, mid, games.NewID()))
	requireProblem(t, mismatch, http.StatusBadRequest, codeInvalidParameter)

	// A viewer may not record labels (docs/spec/08's scope table).
	denied := e.post(auth.RoleViewer, "/api/v1/corrections", fmt.Sprintf(`{
		"profile_id": %q, "kind": "text", "target_type": "message", "target_id": %q, "before_value": "x"
	}`, profileID, mid))
	if denied.Code != http.StatusForbidden && denied.Code != http.StatusUnauthorized {
		t.Errorf("a viewer recorded a label: %d", denied.Code)
	}
}

// TestCorrectionsListRefusesBadFilters: an unknown kind is refused rather than
// dropped, and the limit bound is the API's own, named in the answer.
func TestCorrectionsListRefusesBadFilters(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "correction-list", "list")
	_, row := firstLine(t, e, profileID, "dialogue")
	if err := e.st.PutCorrection(e.ctx, games.Correction{
		ID:          games.NewID(),
		ProfileID:   profileID,
		Kind:        games.CorrectionSpeechText,
		TargetType:  games.TargetMessage,
		TargetID:    row.ID,
		MessageID:   row.ID,
		BeforeValue: "a", AfterValue: "b", Actor: "list-tester",
		Status: games.CorrectionAccepted, Reviewer: "list-tester",
		CreatedAt:  time.Now().UTC().Format(time.RFC3339),
		ReviewedAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("PutCorrection: %v", err)
	}

	// The valid spelling of the filters answers with the list, not a refusal.
	ok := e.mustOK(auth.RoleCurator, "/api/v1/corrections?profile_id="+profileID+"&kind=speech_text&limit=50")
	if items := mustList(ok["items"]); len(items) != 1 {
		t.Fatalf("the valid filters returned %d rows, want the one stored row", len(items))
	}

	badKind := e.get(auth.RoleCurator, "/api/v1/corrections?kind=not-a-kind")
	requireProblem(t, badKind, http.StatusBadRequest, codeInvalidParameter)

	badLimit := e.get(auth.RoleCurator, "/api/v1/corrections?limit=501")
	requireProblem(t, badLimit, http.StatusBadRequest, codeInvalidParameter)

	// The dev view refuses the same bad filters at the developer tier.
	badDev := e.get(auth.RoleDeveloper, "/api/v1/dev/fixtures?kind=not-a-kind")
	requireProblem(t, badDev, http.StatusBadRequest, codeInvalidParameter)

	// An unknown profile id is a 404 the profile read itself answers.
	unknown := e.get(auth.RoleCurator, "/api/v1/corrections?profile_id="+games.NewID())
	requireProblem(t, unknown, http.StatusNotFound, codeNotFound)
}
