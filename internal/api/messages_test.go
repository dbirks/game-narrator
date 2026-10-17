package api

// The dialogue surface: the browser's reads, the search, and the one hand-typed
// edit a curator makes (docs/spec/08 "GET/PATCH /api/v1/profiles/:id/messages/:id":
// "original vs speech text edits, immutable source refs"; docs/spec/02 "Editing the
// editable half of a message").

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// firstLine returns a message of the revision with the given kind, as both the
// DTO and the repository row it came from.
func firstLine(t *testing.T, e *catalogEnv, profileID, kind string) (map[string]any, games.Message) {
	t.Helper()
	target := "/api/v1/profiles/" + profileID + "/messages?limit=1"
	if kind != "" {
		target += "&kind=" + kind
	}
	list := mustList(e.mustOK(auth.RoleViewer, target)["messages"])
	if len(list) == 0 {
		t.Fatalf("the revision has no %q lines", kind)
	}
	dto, _ := list[0].(map[string]any)
	row, _, err := e.st.GetMessage(e.ctx, profileID, dto["id"].(string))
	if err != nil {
		t.Fatalf("GetMessage: %v", err)
	}
	return dto, row
}

// TestPatchSpeechTextNeverTouchesTheSourceText is the file's reason to exist:
// speech_text is the curator's rendering, original_text is the fingerprint of what
// the source said, and there is no request shape that changes the second one.
func TestPatchSpeechTextNeverTouchesTheSourceText(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, etag := newDraft(t, e, "speech", "speech")
	dto, row := firstLine(t, e, profileID, "dialogue")
	mid := dto["id"].(string)
	target := "/api/v1/profiles/" + profileID + "/messages/" + mid

	const rewrite = "Spoken the way the narrator should say it."
	got := e.requireStatus(e.patch(auth.RoleCurator, target,
		fmt.Sprintf(`{"speech_text":%q}`, rewrite), "If-Match", etag),
		http.StatusOK, "PATCH …/messages/{mid}")
	msg, _ := got["message"].(map[string]any)
	if msg["speech_text"] != rewrite {
		t.Fatalf("speech_text = %v", msg["speech_text"])
	}
	if msg["original_text"] != row.OriginalText {
		t.Errorf("original_text changed: %q → %v", row.OriginalText, msg["original_text"])
	}
	if msg["normalized_text"] != row.NormalizedText {
		t.Errorf("normalized_text changed: %q → %v", row.NormalizedText, msg["normalized_text"])
	}
	if msg["revision"].(float64) <= float64(row.Revision) {
		t.Errorf("the per-message revision did not move: %v after %d", msg["revision"], row.Revision)
	}
	if _, ok := got["pages"].([]any); !ok {
		t.Error("a PATCH response came back without the pages member")
	}

	// Asking to change the source text is refused *by name*, before the write: the
	// honest answer to "you asked to change something this API will not change".
	overreach := e.patch(auth.RoleCurator, target,
		`{"speech_text":"fine","original_text":"rewritten history"}`, "If-Match", e.etag(profileID))
	requireProblem(t, overreach, http.StatusBadRequest, codeInvalidParameter)
	if !strings.Contains(overreach.Body.String(), "original_text") {
		t.Errorf("the refusal did not name the field it refused: %s", overreach.Body.String())
	}
	if after, _, err := e.st.GetMessage(e.ctx, profileID, mid); err != nil {
		t.Fatalf("the refused write lost the row: %v", err)
	} else if after.OriginalText != row.OriginalText || after.SpeechText != rewrite {
		t.Errorf("the refused write changed the stored row: %+v", after)
	}

	// An empty speech_text is a *valid* edit: silence is a rendering choice, and
	// docs/spec/02 leaves it to the curator.
	quiet := e.requireStatus(e.patch(auth.RoleCurator, target, `{"speech_text":""}`,
		"If-Match", e.etag(profileID)), http.StatusOK, "clearing speech_text")
	quietMsg, _ := quiet["message"].(map[string]any)
	if quietMsg["speech_text"] != "" {
		t.Errorf("clearing speech_text gave %q", quietMsg["speech_text"])
	}
	if quietMsg["original_text"] != row.OriginalText {
		t.Error("clearing the rendering changed the source text")
	}

	// The domain bound on the field is enforced with the domain's own finding, not
	// a truncation: 8 000 characters is what games.Message.Validate allows.
	tooLong := `{"speech_text":"` + strings.Repeat("long", 2001) + `"}`
	long := e.patch(auth.RoleCurator, target, tooLong, "If-Match", e.etag(profileID))
	problem := requireProblem(t, long, http.StatusUnprocessableEntity, codeValidationFailed)
	if len(problem.Findings) == 0 || !strings.Contains(problem.Findings[0], "speech") {
		t.Errorf("the length refusal carried no finding naming the field: %v", problem.Findings)
	}
	// A body bigger than the request ceiling is a 413 rather than a slow 422.
	huge := `{"speech_text":"` + strings.Repeat("x", maxJSONBodyBytes) + `"}`
	requireProblem(t, e.patch(auth.RoleCurator, target, huge, "If-Match", e.etag(profileID)),
		http.StatusRequestEntityTooLarge, codeTooLarge)

	// And a message of a *different* revision is not addressable through this one,
	// even though both rows exist (ADR-0015: content belongs to a revision).
	sibling, _ := e.createDraft(auth.RoleCurator, mustEditionOf(t, e, profileID))
	requireProblem(t, e.get(auth.RoleViewer,
		"/api/v1/profiles/"+sibling["id"].(string)+"/messages/"+mid), http.StatusNotFound, codeNotFound)
	requireProblem(t, e.patch(auth.RoleCurator,
		"/api/v1/profiles/"+sibling["id"].(string)+"/messages/"+mid, `{"speech_text":"x"}`,
		"If-Match", `"1"`), http.StatusNotFound, codeNotFound)
}

// TestMessageListFiltersAndSearchBound covers the dialogue browser's three
// filters and the tighter ceiling on search (docs/spec/16's 10 000-message
// benchmark is why a hit, which carries a whole row, is bounded below a list).
func TestMessageListFiltersAndSearchBound(t *testing.T) {
	e := newCatalogEnv(t)
	_, _, profileID, _ := newDraft(t, e, "filters", "filters")

	for _, kind := range []string{"dialogue", "sign", "item", "system"} {
		list := mustList(e.mustOK(auth.RoleViewer,
			"/api/v1/profiles/"+profileID+"/messages?kind="+kind)["messages"])
		if len(list) == 0 {
			t.Errorf("the fixture has no %q lines, so this filter is untested", kind)
			continue
		}
		for _, raw := range list {
			if m, _ := raw.(map[string]any); m["text_kind"] != kind {
				t.Fatalf("kind=%s returned a %v", kind, m["text_kind"])
			}
		}
	}
	// A filter that matches nothing is an empty page, not an error, and not the
	// unfiltered list.
	none := mustList(e.mustOK(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages?kind=tutorial")["messages"])
	if len(none) != 0 {
		t.Errorf("kind=tutorial returned %d lines, want none", len(none))
	}

	// scene and speaker are addressed by id, so a label is refused rather than
	// quietly matching nothing (docs/spec/13: an empty list would claim the
	// revision has no such scene).
	requireProblem(t, e.get(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages?scene=Harbour"), http.StatusBadRequest, codeInvalidParameter)
	requireProblem(t, e.get(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages?speaker=Marla"), http.StatusBadRequest, codeInvalidParameter)
	scenes, err := e.st.ListScenes(e.ctx, profileID)
	if err != nil {
		t.Fatalf("ListScenes: %v", err)
	}
	if len(scenes) == 0 {
		t.Skip("this build's adapter writes no scene rows, so the scene filter has nothing to select")
	}
	filtered := mustList(e.mustOK(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages?scene="+scenes[0].ID)["messages"])
	if len(filtered) == 0 {
		t.Errorf("scene=%s selected nothing although the scene row exists", scenes[0].ID)
	}
	for _, raw := range filtered {
		if m, _ := raw.(map[string]any); m["scene_id"] != scenes[0].ID {
			t.Errorf("the scene filter returned a line from another scene: %v", m["scene_id"])
		}
	}
	// An id that is well formed but unknown is the empty page the label could not
	// be, and says so with the same shape.
	if empty := mustList(e.mustOK(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages?scene="+games.NewID())["messages"]); len(empty) != 0 {
		t.Errorf("an unknown scene returned %d lines, want none", len(empty))
	}

	// Search: an empty q is refused instead of becoming a listing, and the ceiling
	// is store.MaxSearchLimit rather than the list's.
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages/search?q="),
		http.StatusBadRequest, codeInvalidParameter)
	requireProblem(t, e.get(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages/search?q=%20%20"),
		http.StatusBadRequest, codeInvalidParameter)
	requireProblem(t, e.get(auth.RoleViewer,
		fmt.Sprintf("/api/v1/profiles/%s/messages/search?q=the&limit=%d", profileID, store.MaxSearchLimit+1)),
		http.StatusBadRequest, codeInvalidParameter)

	hits := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+profileID+"/messages/search?q=harbour&limit=5")
	list := mustList(hits["hits"])
	if len(list) == 0 {
		t.Fatalf("a search for a word in the script found nothing: %s", trimJSON(hits))
	}
	if len(list) > 5 {
		t.Fatalf("limit=5 returned %d hits", len(list))
	}
	if hits["query"] != "harbour" {
		t.Errorf("the response echoed query %v, so a client cannot tell which search this page answers", hits["query"])
	}
	for _, raw := range list {
		h, _ := raw.(map[string]any)
		if _, ok := h["bm25"].(float64); !ok {
			t.Errorf("a hit carries no numeric bm25: %v", h)
		}
		if m, _ := h["message"].(map[string]any); m["profile_id"] != profileID {
			t.Errorf("a hit came from another revision: %v", m["profile_id"])
		}
	}
	// The score is SQLite's own, passed through: lower is better, and a hit list is
	// ordered by it (internal/store/repo_search.go refuses to rescale it, and this
	// route must not either).
	if len(list) > 1 {
		first, _ := list[0].(map[string]any)
		last, _ := list[len(list)-1].(map[string]any)
		if first["bm25"].(float64) > last["bm25"].(float64) {
			t.Errorf("hits are not ordered by the score they carry: %v then %v", first["bm25"], last["bm25"])
		}
	}
	// A phrase that exists in no line is an empty hit list, not a 404.
	if miss := mustList(e.mustOK(auth.RoleViewer,
		"/api/v1/profiles/"+profileID+"/messages/search?q=zzzznothinghere")["hits"]); len(miss) != 0 {
		t.Errorf("a search for an absent word returned %d hits, want none", len(miss))
	}
}

func mustEditionOf(t *testing.T, e *catalogEnv, profileID string) string {
	t.Helper()
	p, _, err := e.st.GetProfile(e.ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	return p.EditionID
}
