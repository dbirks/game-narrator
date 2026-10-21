package api

// The /library screen's per-game read (ADR-0140, docs/design/plum-peach 2a):
// the actionable revision plus the counts the shelf tiles and list rows
// render. Every step below is an HTTP call as a specific role, and every 2xx
// body is validated against the OpenAPI contract on the way through
// (contract clause (c) rides e.send).

import (
	"net/http"
	"testing"

	"github.com/dbirks/game-narrator/internal/auth"
)

// TestGameLibraryView walks the read the screen asks of every game: the
// honest zero view for a bare game, the draft stand-in before a publication,
// the published revision with its counts after an import and a publish, and
// the pick rule when a second edition publishes.
func TestGameLibraryView(t *testing.T) {
	e := newCatalogEnv(t)
	curator, viewer := auth.RoleCurator, auth.RoleViewer

	// 1. A game with nothing under it answers the zero view, not a 404: the
	//    shelf renders its "no profile yet" chip from the empty members.
	g1 := e.createGame(curator, "library-bare", "Library Bare", "lib-bare")
	gameID, _ := g1["id"].(string)
	view := e.mustOK(viewer, "/api/v1/games/"+gameID+"/library")
	if _, present := view["profile_id"]; present {
		t.Errorf("a bare game carries a profile member: %v", view)
	}
	for _, key := range []string{"lines", "characters", "voiced"} {
		if view[key] != float64(0) {
			t.Errorf("a bare game read %s = %v, want 0", key, view[key])
		}
	}

	// 2. An edition with only a draft: the draft stands in, its counts zero.
	ed := e.createEdition(curator, gameID, "us", "en", "rev0", "lib-edition")
	editionID, _ := ed["id"].(string)
	draft, _ := e.createDraft(curator, editionID)
	draftID, _ := draft["id"].(string)
	view = e.mustOK(viewer, "/api/v1/games/"+gameID+"/library")
	if view["profile_id"] != draftID || view["profile_status"] != "draft" || view["edition_id"] != editionID {
		t.Fatalf("the draft stands in for a game with nothing published: %v", view)
	}

	// 3. Import the synthetic script (40 lines, five named speakers) and
	//    publish: the counts follow the revision and the status flips.
	e.importCSV(curator, draftID)
	e.requireStatus(e.post(curator, "/api/v1/profiles/"+draftID+"/status", `{"to":"review"}`,
		"If-Match", e.etag(draftID)), http.StatusOK, "draft → review")
	e.requireStatus(e.post(curator, "/api/v1/profiles/"+draftID+"/publish", "",
		"If-Match", e.etag(draftID)), http.StatusOK, "publish the draft")

	// The read also answers by slug, the way the tile links carry it.
	view = e.mustOK(viewer, "/api/v1/games/library-bare/library")
	if view["profile_id"] != draftID || view["profile_status"] != "published" {
		t.Fatalf("after the publication the read says %v", view)
	}
	if view["lines"] != float64(40) {
		t.Errorf("lines = %v, want the fixture's 40", view["lines"])
	}
	if view["characters"] != float64(5) {
		t.Errorf("characters = %v, want the fixture's five named speakers", view["characters"])
	}
	if view["voiced"] != float64(0) {
		t.Errorf("voiced = %v, want 0 (no voice revision exists yet)", view["voiced"])
	}
	if view["published_at"] == "" {
		t.Errorf("a published pick carries its publication time: %v", view)
	}

	// 4. A second edition publishes too: the pick stays on a publication and
	//    the read is deterministic (the later publication, or the later
	//    edition id when the clock does not move).
	ed2 := e.createEdition(curator, gameID, "eu", "en", "rev0", "lib-edition-2")
	ed2ID, _ := ed2["id"].(string)
	draft2, _ := e.createDraft(curator, ed2ID)
	draft2ID, _ := draft2["id"].(string)
	e.importCSV(curator, draft2ID)
	e.requireStatus(e.post(curator, "/api/v1/profiles/"+draft2ID+"/status", `{"to":"review"}`,
		"If-Match", e.etag(draft2ID)), http.StatusOK, "draft 2 → review")
	e.requireStatus(e.post(curator, "/api/v1/profiles/"+draft2ID+"/publish", "",
		"If-Match", e.etag(draft2ID)), http.StatusOK, "publish the second edition")

	view = e.mustOK(viewer, "/api/v1/games/"+gameID+"/library")
	if view["profile_id"] != draft2ID {
		t.Errorf("the pick followed the newer publication: profile_id = %v, want %s", view["profile_id"], draft2ID)
	}
	if view["profile_status"] != "published" || view["lines"] != float64(40) {
		t.Errorf("the picked revision reads %v", view)
	}

	// 5. The errors the read can make: a game that does not exist.
	requireProblem(t, e.get(viewer, "/api/v1/games/does-not-exist/library"),
		http.StatusNotFound, codeNotFound)

	// 6. The role gate: the scope table grants the route to viewer; an
	//    unauthenticated call is refused like every other catalogue read.
	if rec := e.get(auth.RoleNone, "/api/v1/games/"+gameID+"/library"); rec.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated read = %d, want 401", rec.Code)
	}
}
