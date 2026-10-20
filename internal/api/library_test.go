package api

// ADR-0107 (gap G14) HTTP coverage: duplicate, clone, disable/enable, delete
// and purge over real routes against a real migrated store, with every 2xx
// validated against the OpenAPI document (the same contract clause the catalog
// flows get). The store-level facts (the transactions, the cascade, the
// retention guard, the provenance rows) are proven in
// internal/store/repo_library_test.go; what this file proves is the surface:
// the shapes, the roles, the hiding of deleted revisions, the session guards,
// and the restart guarantee.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"sync/atomic"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// newLifecycleFixture creates the game/edition/draft trio the lifecycle tests
// act on, through the API the way the UI would.
func newLifecycleFixture(t *testing.T, e *catalogEnv, slug string) (gameID, editionID, profileID string) {
	t.Helper()
	role := auth.RoleCurator
	g := e.createGame(role, slug, slug, "life-"+slug)
	ed := e.createEdition(role, g["id"].(string), "US", "en", "rev0", "life-"+slug+"-ed")
	draft, _ := e.createDraft(role, ed["id"].(string))
	return g["id"].(string), ed["id"].(string), draft["id"].(string)
}

// backdatePurgeAfter moves a profile's purge_after into the past through a
// second connection on the same file: the store reads the column, it does not
// own the clock, so the test plays the week the retention window needs.
func backdatePurgeAfter(t *testing.T, path, profileID string) {
	t.Helper()
	db, err := sql.Open("sqlite", path+"?_busy_timeout=5000&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open backdate connection: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("UPDATE profiles SET purge_after = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), profileID)
	if err != nil {
		t.Fatalf("backdate purge_after: %v", err)
	}
}

// requireProblemType asserts the status and the problem's urn slug — the
// machine-readable identity the slugs of ADR-0107's refusals exist to carry.
func requireProblemType(t *testing.T, rec *httptest.ResponseRecorder, wantStatus int, wantSlug string) {
	t.Helper()
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d (body %s)", rec.Code, wantStatus, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "urn:game-narrator:error:"+wantSlug) {
		t.Fatalf("problem type lacks urn:game-narrator:error:%s (body %s)", wantSlug, rec.Body.String())
	}
}

// restartTokenSeq names the tokens the restart test mints (token names are
// unique per account; the counter keeps the two boots from colliding).
var restartTokenSeq atomic.Int64

// TestLibraryDuplicateCloneAndDisable walks the three curator operations to a
// real 2xx each (so the OpenAPI contract is validated, not just routed) and
// asserts the fact each operation promises: the duplicate stays in its game as
// the next draft revision, the clone opens a new game with a matching edition,
// a fresh draft and the provenance link, and the disable switch moves the
// game's flag while the catalogue keeps serving it.
func TestLibraryDuplicateCloneAndDisable(t *testing.T) {
	e := newCatalogEnv(t)
	role := auth.RoleCurator
	gid, eid, pid := newLifecycleFixture(t, e, "lifecycle-tide")

	// duplicate: no body, the source is the path.
	rec := e.post(role, "/api/v1/profiles/"+pid+"/duplicate", "")
	created := e.requireStatus(rec, http.StatusCreated, "POST /profiles/{id}/duplicate")
	dupID := created["id"].(string)
	if dupID == pid {
		t.Fatalf("the duplicate got the source's id %s", pid)
	}
	if created["edition_id"] != eid {
		t.Errorf("duplicate edition_id = %v, want %s (a duplicate stays in its game)", created["edition_id"], eid)
	}
	if created["revision_num"] != float64(2) || created["status"] != "draft" {
		t.Errorf("duplicate = revision %v %v, want revision 2 draft", created["revision_num"], created["status"])
	}
	if created["parent_revision"] != float64(1) {
		t.Errorf("duplicate parent_revision = %v, want 1 (the revision it duplicated)", created["parent_revision"])
	}
	if got := rec.Header().Get("Location"); got != "/api/v1/profiles/"+dupID {
		t.Errorf("duplicate Location = %q, want the new draft's path", got)
	}
	// The source is untouched by the act: still revision 1.
	if src := e.mustOK(auth.RoleViewer, "/api/v1/profiles/"+pid); src["revision_num"] != float64(1) {
		t.Errorf("source revision_num after the duplicate = %v, want 1", src["revision_num"])
	}
	// Refusals: a missing source is a 404, not a 500.
	rec = e.post(role, "/api/v1/profiles/"+games.NewID()+"/duplicate", "")
	requireProblem(t, rec, http.StatusNotFound, codeNotFound)

	// clone: the optional body takes the caller's title and slug.
	rec = e.post(role, "/api/v1/profiles/"+pid+"/clone", `{"title":"Cloned Tide","slug":"cloned-tide"}`)
	clone := e.requireStatus(rec, http.StatusCreated, "POST /profiles/{id}/clone")
	cg := clone["game"].(map[string]any)
	ce := clone["edition"].(map[string]any)
	cp := clone["profile"].(map[string]any)
	link := clone["link"].(map[string]any)
	cloneGameID, cloneEditionID, cloneProfileID := cg["id"].(string), ce["id"].(string), cp["id"].(string)
	if cloneGameID == gid || cloneGameID == "" {
		t.Fatalf("clone game id = %q, want a new game (distinct from %s)", cloneGameID, gid)
	}
	if cg["slug"] != "cloned-tide" {
		t.Errorf("clone game slug = %v, want the caller's slug", cg["slug"])
	}
	if ce["game_id"] != cloneGameID || ce["region"] != "US" || ce["language"] != "en" {
		t.Errorf("clone edition = game %v region %v language %v, want the source binding under the new game",
			ce["game_id"], ce["region"], ce["language"])
	}
	if cp["edition_id"] != cloneEditionID || cp["revision_num"] != float64(1) || cp["status"] != "draft" {
		t.Errorf("clone profile = edition %v revision %v %v, want a fresh draft revision 1",
			cp["edition_id"], cp["revision_num"], cp["status"])
	}
	if cp["parent_revision"] != nil {
		t.Errorf("clone parent_revision = %v, want null (the draft descends from the clone, not from a revision number)", cp["parent_revision"])
	}
	if link["source_profile_id"] != pid || link["target_profile_id"] != cloneProfileID ||
		link["source_game_id"] != gid || link["target_game_id"] != cloneGameID {
		t.Errorf("clone link = %v, want the provenance between the source revision and the clone", link)
	}
	if cb, _ := link["created_by"].(string); cb == "" {
		t.Errorf("clone link created_by is empty: the provenance row must name its actor")
	}
	// The clone is navigable the way the library navigates anything: the game
	// reads and its edition lists the fresh draft.
	if g := e.mustOK(auth.RoleViewer, "/api/v1/games/"+cloneGameID); g["slug"] != "cloned-tide" {
		t.Errorf("cloned game read back = %v, want the clone", g)
	}
	if list := e.mustOK(auth.RoleViewer, "/api/v1/editions/"+cloneEditionID+"/profiles"); !profileListContains(list, cloneProfileID) {
		t.Errorf("the clone's edition does not list its draft %v", list)
	}

	// disable: the game's flag, re-read from the store for the body.
	rec = e.post(role, "/api/v1/games/"+gid+"/disable", "")
	disabled := e.requireStatus(rec, http.StatusOK, "POST /games/{id}/disable")
	if disabled["disabled"] != true {
		t.Errorf("disable response disabled = %v, want true", disabled["disabled"])
	}
	// The library keeps the game (with the flag) — hidden from Play, not
	// hidden from the curator who must be able to re-enable it.
	row := gameListContains(t, e.mustOK(auth.RoleViewer, "/api/v1/games"), gid)
	if row["disabled"] != true {
		t.Errorf("library list row = %v, want disabled true (the flag the Play screen will render)", row)
	}
	// And the catalogue writes on it keep working: the profiles stay editable.
	etag := e.etag(pid)
	if rec := e.post(role, "/api/v1/profiles/"+pid+"/status", `{"to":"review"}`, "If-Match", etag); rec.Code != http.StatusOK {
		t.Errorf("status write on a disabled game's profile = %d (body %s), want 200 (profiles stay editable)", rec.Code, rec.Body.String())
	}
	// A disabled game is not hidden from reads: the direct game read still 200s.
	if rec := e.get(auth.RoleViewer, "/api/v1/games/"+gid); rec.Code != http.StatusOK {
		t.Errorf("GET of a disabled game = %d, want 200", rec.Code)
	}
	// Re-enable flips the flag back.
	rec = e.post(role, "/api/v1/games/"+gid+"/enable", "")
	enabled := e.requireStatus(rec, http.StatusOK, "POST /games/{id}/enable")
	if enabled["disabled"] != false {
		t.Errorf("enable response disabled = %v, want false", enabled["disabled"])
	}
	// Refusals: a missing game is a 404 on both halves.
	rec = e.post(role, "/api/v1/games/"+games.NewID()+"/disable", "")
	requireProblem(t, rec, http.StatusNotFound, codeNotFound)
	rec = e.post(role, "/api/v1/games/"+games.NewID()+"/enable", "")
	requireProblem(t, rec, http.StatusNotFound, codeNotFound)
}

// TestLibraryDeleteAndPurge is the two-step delete end to end: the receipt,
// the hiding (list, direct read, writes), the role gate on the purge, the
// retention refusal, and — after the window — the purge's report.
func TestLibraryDeleteAndPurge(t *testing.T) {
	e := newCatalogEnv(t)
	role := auth.RoleCurator
	gid, eid, pid := newLifecycleFixture(t, e, "delete-tide")

	// The purge gate: admin only, before the window even matters.
	rec := e.post(role, "/api/v1/profiles/"+pid+"/purge", "")
	requireProblemType(t, rec, http.StatusForbidden, "forbidden")
	rec = e.post(auth.RoleViewer, "/api/v1/profiles/"+pid+"/purge", "")
	requireProblemType(t, rec, http.StatusForbidden, "forbidden")

	// delete: the soft mark, with its receipt.
	rec = e.send(role, http.MethodDelete, "/api/v1/profiles/"+pid, nil)
	receipt := e.requireStatus(rec, http.StatusOK, "DELETE /profiles/{id}")
	deletedAt, _ := receipt["deleted_at"].(string)
	purgeAfter, _ := receipt["purge_after"].(string)
	if deletedAt == "" || purgeAfter == "" {
		t.Fatalf("delete receipt lacks the mark: %v", receipt)
	}
	// The mark is present: a second delete names it.
	rec = e.send(role, http.MethodDelete, "/api/v1/profiles/"+pid, nil)
	requireProblemType(t, rec, http.StatusConflict, "profile-already-deleted")
	// A missing profile's delete is a 404, like every other read.
	rec = e.send(role, http.MethodDelete, "/api/v1/profiles/"+games.NewID(), nil)
	requireProblem(t, rec, http.StatusNotFound, codeNotFound)

	// Hidden from the library list.
	if list := e.mustOK(auth.RoleViewer, "/api/v1/editions/"+eid+"/profiles"); profileListContains(list, pid) {
		t.Errorf("deleted profile is still listed: %v", list)
	}
	// Hidden from the direct read (the ADR's 404 for the marked row).
	rec = e.get(auth.RoleViewer, "/api/v1/profiles/"+pid)
	requireProblemType(t, rec, http.StatusNotFound, "profile-deleted")
	// And off the editing surfaces: a write answers the mark, not the edit.
	rec = e.post(role, "/api/v1/profiles/"+pid+"/status", `{"to":"review"}`, "If-Match", `"1"`)
	requireProblemType(t, rec, http.StatusConflict, "profile-deleted")

	// Inside the retention window the purge refuses and names the window.
	rec = e.post(auth.RoleAdmin, "/api/v1/profiles/"+pid+"/purge", "")
	requireProblem(t, rec, http.StatusConflict, "RETENTION_NOT_ELAPSED")
	// A purge of a live (unmarked) profile names the two-step act.
	_, _, pid2 := newLifecycleFixture(t, e, "purge-livemiss")
	rec = e.post(auth.RoleAdmin, "/api/v1/profiles/"+pid2+"/purge", "")
	requireProblemType(t, rec, http.StatusConflict, "profile-not-deleted")

	// The window closes: the purge removes the row, its content and its cache
	// entries, and reports what it did.
	backdatePurgeAfter(t, e.st.Path(), pid)
	rec = e.post(auth.RoleAdmin, "/api/v1/profiles/"+pid+"/purge", "")
	report := e.requireStatus(rec, http.StatusOK, "POST /profiles/{id}/purge")
	if report["profile_id"] != pid {
		t.Errorf("purge report profile_id = %v, want %s", report["profile_id"], pid)
	}
	if report["cache_entries"] != float64(0) {
		t.Errorf("purge report cache_entries = %v, want 0 (the draft never synthesized)", report["cache_entries"])
	}
	if blobs, ok := report["deleted_blobs"].([]any); !ok || len(blobs) != 0 {
		t.Errorf("purge report deleted_blobs = %v, want an empty list (no blobs were named)", report["deleted_blobs"])
	}

	// The purge is a removal: the row is gone from the store's own reads, so
	// every surface 404s with the ordinary not-found, and the edition stands.
	if _, _, err := e.st.GetProfile(e.ctx, pid); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("store read of the purged profile = %v, want ErrNotFound", err)
	}
	if rec := e.get(auth.RoleViewer, "/api/v1/profiles/"+pid); rec.Code != http.StatusNotFound {
		t.Errorf("purged profile read = %d, want 404", rec.Code)
	}
	if list := e.mustOK(auth.RoleViewer, "/api/v1/editions/"+eid+"/profiles"); profileListContains(list, pid) {
		t.Errorf("purged profile is listed: %v", list)
	}
	if g := e.mustOK(auth.RoleViewer, "/api/v1/games/"+gid); g["id"] != gid {
		t.Errorf("game after the purge = %v, want it standing", g)
	}
	// Deleting again finds nothing: after the purge the act is simply done.
	rec = e.send(role, http.MethodDelete, "/api/v1/profiles/"+pid, nil)
	requireProblem(t, rec, http.StatusNotFound, codeNotFound)
}

// TestLibraryDeleteGuardsAtTheSurface is the two store guards the delete route
// answers with their named 409s: a revision with publication history retires
// via archive, not delete, and a parented revision deletes its children first.
func TestLibraryDeleteGuardsAtTheSurface(t *testing.T) {
	e := newCatalogEnv(t)
	role := auth.RoleCurator
	_, _, pid := newLifecycleFixture(t, e, "guard-tide")

	// Publication history: the guard counts publish_actions rows, so the test
	// writes one through the same raw access the store test uses (the real
	// publish flow is proven store-side, where it can actually ship).
	db, err := sql.Open("sqlite", e.st.Path()+"?_busy_timeout=5000&_foreign_keys=1")
	if err != nil {
		t.Fatalf("open guard connection: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO publish_actions (id, profile_id, edition_id, action, author, row_version, detail_json, occurred_at)
		 VALUES (?, ?, 'ed-guard', 'published', 'curator@local', 1, '{}', ?)`,
		games.NewID(), pid, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatalf("seed the history row: %v", err)
	}
	rec := e.send(role, http.MethodDelete, "/api/v1/profiles/"+pid, nil)
	requireProblemType(t, rec, http.StatusConflict, "profile-has-publication-history")

	// A parented revision: the duplicate is the child that makes the delete
	// name its children.
	_, _, parent := newLifecycleFixture(t, e, "guard-parent")
	if rec := e.post(role, "/api/v1/profiles/"+parent+"/duplicate", ""); rec.Code != http.StatusCreated {
		t.Fatalf("duplicate for the parent guard = %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	rec = e.send(role, http.MethodDelete, "/api/v1/profiles/"+parent, nil)
	requireProblemType(t, rec, http.StatusConflict, "profile-has-derived-revisions")
}

// TestDeletedProfileRefusesSessionStart is the session-side half of the
// delete: Play refuses a hidden revision with the runtime's 404 sentinel — a
// session started on it would resurrect what the operator removed.
func TestDeletedProfileRefusesSessionStart(t *testing.T) {
	e := newSessionEnv(t)
	clipDir := sessSyntheticDirAbs(t)

	// A fresh draft on the seeded edition, deleted through the store (the
	// API's delete route runs the same store call; the guard under test is
	// the runtime's, which reads the mark).
	prof, _, err := e.st.GetProfile(context.Background(), e.profileID)
	if err != nil {
		t.Fatalf("seeded profile: %v", err)
	}
	draft, err := e.st.CreateDraftProfile(context.Background(), prof.EditionID)
	if err != nil {
		t.Fatalf("draft: %v", err)
	}
	if _, err := e.st.SoftDeleteProfile(context.Background(), draft.ID); err != nil {
		t.Fatalf("soft delete the draft: %v", err)
	}

	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
		startPayload(draft.ID, "replay", clipDir, "conservative", false))
	assertSessionProblem(t, rec, http.StatusNotFound, "profile-deleted")
}

// TestDisabledGameRefusesSessionStart is the session-side half of the disable:
// the runtime's 409 sentinel, mapped to the session's problem document — and
// the enable is proven by the identical start succeeding with the flag back
// off, which is the switch, not the profile.
func TestDisabledGameRefusesSessionStart(t *testing.T) {
	e := newSessionEnv(t)
	clipDir := sessSyntheticDirAbs(t)

	prof, _, err := e.st.GetProfile(context.Background(), e.profileID)
	if err != nil {
		t.Fatalf("seeded profile: %v", err)
	}
	edition, err := e.st.GetEdition(context.Background(), prof.EditionID)
	if err != nil {
		t.Fatalf("seeded edition: %v", err)
	}

	// Disable the seeded game through the curator route.
	if rec := e.send(auth.RoleCurator, http.MethodPost, "/api/v1/games/"+edition.GameID+"/disable", ""); rec.Code != http.StatusOK {
		t.Fatalf("disable the seeded game = %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	// Play refuses it: the 409 the ADR documents.
	rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", false))
	assertSessionProblem(t, rec, http.StatusConflict, "game-disabled")

	// Re-enable and the identical start succeeds.
	if rec := e.send(auth.RoleCurator, http.MethodPost, "/api/v1/games/"+edition.GameID+"/enable", ""); rec.Code != http.StatusOK {
		t.Fatalf("re-enable = %d (body %s), want 200", rec.Code, rec.Body.String())
	}
	rec = e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", false))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start after re-enable = %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	started := decodeJSON(t, rec)
	id, _ := started["session_id"].(string)
	if id == "" {
		t.Fatalf("start after re-enable has no session_id: %s", rec.Body.String())
	}
	// And the session's own stop path stays clean while the game stands.
	if rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/stop", ""); rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("stop = %d (body %s), want 2xx", rec.Code, rec.Body.String())
	}
}

// TestDeleteRefusedWhileASessionUsesTheProfile is the session guard's positive
// half: with a live session pinned to the profile, the delete answers the
// guard (409 profile-in-use) before it would reach the store's history
// refusal — the session-side statement of the rule the store also guards.
// Stopping the session clears the guard, and the same delete then reaches the
// store's own refusal: proof the first 409 came from the session.
func TestDeleteRefusedWhileASessionUsesTheProfile(t *testing.T) {
	e := newSessionEnv(t)
	clipDir := sessSyntheticDirAbs(t)

	rec := e.send(auth.RoleDeveloper, http.MethodPost, "/api/v1/sessions",
		startPayload(e.profileID, "replay", clipDir, "conservative", true))
	if rec.Code != http.StatusCreated {
		t.Fatalf("start = %d (body %s), want 201", rec.Code, rec.Body.String())
	}
	id, _ := decodeJSON(t, rec)["session_id"].(string)
	// Pause so the session is deterministically still the active one: the
	// replay would otherwise race the delete to the end of its clip.
	if rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/pause", ""); rec.Code != http.StatusOK {
		t.Fatalf("pause = %d (body %s), want 200", rec.Code, rec.Body.String())
	}

	rec = e.send(auth.RoleCurator, http.MethodDelete, "/api/v1/profiles/"+e.profileID, "")
	requireProblemType(t, rec, http.StatusConflict, "profile-in-use")
	// The purge guard is the same session fact.
	rec = e.send(auth.RoleAdmin, http.MethodPost, "/api/v1/profiles/"+e.profileID+"/purge", "")
	requireProblemType(t, rec, http.StatusConflict, "profile-in-use")

	// Stop the session and the guard clears: the seeded profile's publication
	// history is now the refusal — the store's, not the session's.
	if rec := e.send(auth.RoleOperator, http.MethodPost, "/api/v1/sessions/"+id+"/stop", ""); rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("stop = %d (body %s), want 2xx", rec.Code, rec.Body.String())
	}
	rec = e.send(auth.RoleCurator, http.MethodDelete, "/api/v1/profiles/"+e.profileID, "")
	requireProblemType(t, rec, http.StatusConflict, "profile-has-publication-history")
}

// TestLibraryLifecycleSurvivesRestart is the AGENTS.md restart guarantee at
// the API seam: a delete and a disable, then a close-and-reopen of the same
// file with a fresh service and handler — the deleted profile stays hidden
// and the disabled game stays off Play.
func TestLibraryLifecycleSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	st1, h1, token := restartOpen(t, dir, ctx)
	gid, _, pid := restartNewFixture(t, h1, token, "restart-tide")

	// The delete candidate: a second draft of the same edition, deleted; and
	// the game, disabled. Both through the same store calls the routes run.
	prof, _, err := st1.GetProfile(ctx, pid)
	if err != nil {
		t.Fatalf("profile: %v", err)
	}
	draft2, err := st1.CreateDraftProfile(ctx, prof.EditionID)
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := st1.SoftDeleteProfile(ctx, draft2.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	if err := st1.SetGameDisabled(ctx, gid, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// The restart: a fresh open of the same file, a fresh service over the
	// same accounts, a fresh handler. The raw tokens authenticate again
	// because they are rows in the file that was just reopened.
	st2, h2, token2 := restartOpen(t, dir, ctx)

	// The delete survived: the direct read 404s and the list hides the row.
	if rec := reqAs(h2, token2, auth.RoleViewer, http.MethodGet, "/api/v1/profiles/"+draft2.ID, nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleted draft after restart = %d (body %s), want 404 (the mark survived the restart)", rec.Code, rec.Body.String())
	}
	list := bodyOK(t, reqAs(h2, token2, auth.RoleViewer, http.MethodGet,
		"/api/v1/editions/"+prof.EditionID+"/profiles", nil))
	if profileListContains(list, draft2.ID) {
		t.Errorf("deleted draft is listed again after the restart: %v", list)
	}
	// The first draft is untouched by the restart: same id, same status.
	if p, _, err := st2.GetProfile(ctx, pid); err != nil || p.Status != games.StatusDraft {
		t.Errorf("live profile after restart = %+v, %v; want it still a draft", p, err)
	}
	// The disable survived: the game reads back with the flag.
	g, err := st2.GetGame(ctx, gid)
	if err != nil || !g.Disabled {
		t.Errorf("game after restart = %+v, %v; want it still disabled", g, err)
	}
	// And the file is still writable: the re-enable lands on the restarted
	// store.
	if err := st2.SetGameDisabled(ctx, gid, false); err != nil {
		t.Errorf("re-enable after the restart: %v", err)
	}
}

// ── restart helpers ─────────────────────────────────────────────────────────

// restartOpen opens (or reopens) the store at dir, wires a fresh auth service
// over it and hands back a handler. On the first call it sets the appliance up
// and mints the tokens; on the restart the accounts are found in the file,
// not created.
func restartOpen(t *testing.T, dir string, ctx context.Context) (*store.Store, http.Handler, map[auth.Role]string) {
	t.Helper()
	st, err := store.Open(ctx, store.DBPath(dir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repo := store.NewAuthStore(st)
	svc := auth.NewService(repo,
		auth.WithPasswordCost(1000),
		auth.WithLimiter(auth.NewLimiter(10_000, time.Minute, func() time.Time { return time.Now() })),
		auth.WithLogger(discardLogger))
	if _, err := svc.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil && !errors.Is(err, auth.ErrAlreadyInitialized) {
		t.Fatalf("setup admin: %v", err)
	}
	tokens := map[auth.Role]string{}
	for _, role := range []auth.Role{auth.RoleAdmin, auth.RoleCurator, auth.RoleViewer} {
		name := role.String()
		u, err := repo.UserByUsername(ctx, name)
		if err != nil {
			u, err = svc.CreateUser(ctx, name, testPassword, role, nil, "restart-test")
			if err != nil {
				t.Fatalf("create %s: %v", name, err)
			}
		}
		restartTokenSeq.Add(1)
		raw, _, err := svc.CreateToken(ctx, nil, auth.CreateTokenParams{
			UserID: u.ID, Name: fmt.Sprintf("restart-token-%d", restartTokenSeq.Load()), Role: role,
		}, "127.0.0.1:1")
		if err != nil {
			t.Fatalf("mint %s token: %v", name, err)
		}
		tokens[role] = raw
	}
	h := New(testBuild, testStarted, discardLogger,
		WithAuth(svc, repo), WithStore(st)).Handler()
	return st, h, tokens
}

// restartNewFixture creates the game/edition/draft trio over a bare handler.
func restartNewFixture(t *testing.T, h http.Handler, token map[auth.Role]string, slug string) (string, string, string) {
	t.Helper()
	g := bodyCreated(t, reqAs(h, token, auth.RoleCurator, http.MethodPost, "/api/v1/games",
		strings.NewReader(fmt.Sprintf(`{"slug":%q,"title":%q,"platform":"gamecube-like"}`, slug, slug)),
		"Idempotency-Key", "rs-"+slug))
	ed := bodyCreated(t, reqAs(h, token, auth.RoleCurator, http.MethodPost,
		"/api/v1/games/"+g["id"].(string)+"/editions",
		strings.NewReader(`{"region":"US","language":"en","revision":"rev0","source_identity":"`+strings.Repeat("a", 64)+`","locale":"en-US"}`),
		"Idempotency-Key", "rs-"+slug+"-ed"))
	d := bodyCreated(t, reqAs(h, token, auth.RoleCurator, http.MethodPost,
		"/api/v1/editions/"+ed["id"].(string)+"/profiles", nil))
	return g["id"].(string), ed["id"].(string), d["id"].(string)
}

func reqAs(h http.Handler, token map[auth.Role]string, role auth.Role, method, target string, body io.Reader, kv ...string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	if body != nil {
		r.Header.Set("Content-Type", ContentTypeJSON)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		r.Header.Set(kv[i], kv[i+1])
	}
	if role != auth.RoleNone {
		r.Header.Set("Authorization", "Bearer "+token[role])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func bodyOK(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	return decodeJSON(t, rec)
}

func bodyCreated(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	return decodeJSON(t, rec)
}

// profileListContains answers "is this profile id in the edition's profile
// list": the list body is {profiles: [...], ...}.
func profileListContains(list map[string]any, profileID string) bool {
	items, _ := list["profiles"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["id"] == profileID {
			return true
		}
	}
	return false
}

// gameListContains finds one game row in the games list body.
func gameListContains(t *testing.T, list map[string]any, gameID string) map[string]any {
	t.Helper()
	items, _ := list["games"].([]any)
	for _, it := range items {
		m, _ := it.(map[string]any)
		if m["id"] == gameID {
			return m
		}
	}
	t.Fatalf("game %s is not in the list: %v", gameID, list)
	return nil
}

// sessSyntheticDirAbs resolves the synthetic replay clip the session tests
// replay (the same fixture the session manager's allowlist admits).
func sessSyntheticDirAbs(t *testing.T) string {
	t.Helper()
	abs, err := filepath.Abs(sessSyntheticDir)
	if err != nil {
		t.Fatalf("abs clip dir: %v", err)
	}
	return abs
}
