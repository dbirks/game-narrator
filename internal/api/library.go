package api

// Profile library lifecycle (ADR-0107, gap G14): duplicate, clone, disable,
// delete and purge. The store owns the state transitions (one transaction each,
// with the guards); this file turns them into routes — the role per route is
// the scope table's business, the audit line is the auth trail's, and the
// session guard is the one check that lives here because the runtime manager
// does.
//
// The shapes, from the ADR:
//
//   - duplicate: POST /profiles/{id}/duplicate — no body, 201 + the new draft
//     (same edition), audited catalog.profile.duplicate.
//   - clone:     POST /profiles/{id}/clone — optional {title, slug}, 201 +
//     the new game/edition/draft plus the provenance link, audited
//     catalog.profile.clone.
//   - disable:   POST /games/{id}/disable (and …/enable) — the game's flag,
//     200 + the game, audited catalog.game.disable/enable.
//   - delete:    DELETE /profiles/{id} — the soft mark, 200 + the marked
//     profile (the receipt), audited catalog.profile.delete.
//   - purge:     POST /profiles/{id}/purge — admin, 200 + the purge report,
//     audited catalog.profile.purge.
//
// A deleted revision is hidden from the surfaces a user acts on: the list
// (the store filters it), GET /profiles/{id} (404, handleProfileGet) and every
// profile-scoped write (409 profile-deleted via the guards in the write
// handlers). The row itself stays readable by the store's own repositories —
// the retention window and the purge need it — until the purge removes it.

import (
	"errors"
	"net/http"
	"strings"

	"github.com/dbirks/game-narrator/internal/store"
)

// rejectIfProfileDeleted answers 409 profile-deleted for a profile-scoped WRITE
// on a soft-deleted revision. The mark means the revision is off the surfaces
// (ADR-0107): its content is kept for the retention window and support, not
// edited — an edit of a hidden row would bump its ETag, re-touch its FTS
// entries and make the purge's work pointless, all for a row nobody can see.
// The read handlers do not use it (they 404 instead, the ADR's "hidden"
// reading), and the delete/purge routes read the mark themselves: refusing
// them here would make the two-step act impossible.
func (s *Server) rejectIfProfileDeleted(w http.ResponseWriter, r *http.Request, deletedAt string) bool {
	if deletedAt == "" {
		return true
	}
	s.writeCodeProblem(w, r, http.StatusConflict, "profile-deleted", codeConflict, "Conflict",
		"this profile revision is deleted (purge_after "+deletedAt+"); the mark keeps it off the editing surfaces for the retention window — purge it when the window closes")
	return false
}

// rejectIfSessionUsesProfile refuses a delete or purge while this process's
// runtime has an active session (starting/running/stopping) pinned to the
// profile. In practice a session can only pin a published revision — which the
// publication-history guard already keeps undeletable — so this is the
// defense-in-depth half of the pair: the session-side statement of the same
// rule, at the layer that actually knows session state (ADR-0107). A process
// with no runtime wired has no session to protect, so it passes.
func (s *Server) rejectIfSessionUsesProfile(w http.ResponseWriter, r *http.Request, profileID string) bool {
	if s.sessions == nil {
		return true
	}
	info, ok := s.sessions.Active()
	if !ok || info.ProfileID != profileID {
		return true
	}
	s.writeCodeProblem(w, r, http.StatusConflict, "profile-in-use", codeConflict, "Conflict",
		"session "+info.ID+" is running on this profile ("+info.State+"); stop the session before deleting or purging the profile it uses")
	return false
}

// ── duplicate ──────────────────────────────────────────────────────────────

// handleProfileDuplicate serves POST /api/v1/profiles/{id}/duplicate: the
// lifecycle verb FR-02 names, over the store primitive POST …/revisions uses
// (ADR-0107). No body: the source is the path, and the copy is a draft of the
// same edition with the source's content snapshot. The audit line is
// catalog.profile.duplicate, not catalog.profile.revision — same store work,
// distinct intent, distinguishable trail.
func (s *Server) handleProfileDuplicate(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	source, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, source.DeletedAt) {
		return
	}
	if !s.rejectBody(w, r) {
		return
	}
	p, err := s.store.NewRevision(r.Context(), source.ID)
	if err != nil {
		s.storeProblem(w, r, err, "duplicate profile", "profile")
		return
	}
	_, version, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get duplicated profile", "profile")
		return
	}
	setETag(w, version)
	s.auditCatalogWrite(r, "catalog.profile.duplicate", map[string]any{
		"source_profile_id": source.ID, "profile_id": p.ID, "revision_num": p.RevisionNum,
	})
	w.Header().Set("Location", "/api/v1/profiles/"+p.ID)
	writeJSON(w, http.StatusCreated, toProfileDTO(p))
}

// ── clone ───────────────────────────────────────────────────────────────────

// cloneRequest is the optional body of POST …/clone. Empty (or absent) takes
// the ADR defaults: the source game's title, and <source-slug>-copy suffixed
// -2, -3, … until the slug is free.
type cloneRequest struct {
	Title string `json:"title,omitempty"`
	Slug  string `json:"slug,omitempty"`
}

// cloneLinkDTO is the provenance row the clone wrote (ADR-0107: "the audit
// trail must be queryable in both directions" — this is the forward one;
// CloneLinksForProfile is the backward read the UI's "cloned from" card uses).
type cloneLinkDTO struct {
	ID              string `json:"id"`
	SourceProfileID string `json:"source_profile_id"`
	TargetProfileID string `json:"target_profile_id"`
	SourceEditionID string `json:"source_edition_id"`
	TargetEditionID string `json:"target_edition_id"`
	SourceGameID    string `json:"source_game_id"`
	TargetGameID    string `json:"target_game_id"`
	CreatedBy       string `json:"created_by"`
	CreatedAt       string `json:"created_at"`
}

// cloneResponse is the 201 body: the four rows the one transaction created,
// so the client's navigation (game page → edition → draft) starts from ids it
// holds rather than re-reading them.
type cloneResponse struct {
	Game    gameDTO      `json:"game"`
	Edition editionDTO   `json:"edition"`
	Profile profileDTO   `json:"profile"`
	Link    cloneLinkDTO `json:"link"`
}

// handleProfileClone serves POST /api/v1/profiles/{id}/clone (ADR-0107). The
// source revision must exist and not be deleted (the store refuses a hidden
// source); everything else — the new game, the edition copy, the draft
// revision 1 with the content snapshot, the provenance link — is one store
// transaction, so the 201 either carries all four or does not happen.
func (s *Server) handleProfileClone(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	source, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, source.DeletedAt) {
		return
	}
	var req cloneRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Slug = strings.TrimSpace(req.Slug)

	res, err := s.store.CloneProfile(r.Context(), source.ID, req.Title, req.Slug, s.auditActor(r))
	if err != nil {
		s.storeProblem(w, r, err, "clone profile", "profile")
		return
	}
	_, version, err := s.store.GetProfile(r.Context(), res.Profile.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get cloned profile", "profile")
		return
	}
	setETag(w, version)
	s.auditCatalogWrite(r, "catalog.profile.clone", map[string]any{
		"source_profile_id": source.ID, "source_game_id": sourceGameID(res),
		"game_id": res.Game.ID, "edition_id": res.Edition.ID,
		"profile_id": res.Profile.ID, "link_id": res.Link.ID,
	})
	w.Header().Set("Location", "/api/v1/profiles/"+res.Profile.ID)
	writeJSON(w, http.StatusCreated, cloneResponse{
		Game:    toGameDTO(res.Game),
		Edition: toEditionDTO(res.Edition),
		Profile: toProfileDTO(res.Profile),
		Link: cloneLinkDTO{
			ID:              res.Link.ID,
			SourceProfileID: res.Link.SourceProfileID,
			TargetProfileID: res.Link.TargetProfileID,
			SourceEditionID: res.Link.SourceEditionID,
			TargetEditionID: res.Link.TargetEditionID,
			SourceGameID:    res.Link.SourceGameID,
			TargetGameID:    res.Link.TargetGameID,
			CreatedBy:       res.Link.CreatedBy,
			CreatedAt:       res.Link.CreatedAt,
		},
	})
}

// sourceGameID recovers the source game id from the clone result for the audit
// line: the store carries it on the link row, and the audit detail wants it
// next to the target's.
func sourceGameID(res store.CloneResult) string { return res.Link.SourceGameID }

// auditActor names the caller for the clone_links row (the created_by the
// provenance card shows): the caller's username when authenticated, the
// empty string otherwise, which the store records as its unknown-actor value.
func (s *Server) auditActor(r *http.Request) string {
	return IdentityFromContext(r.Context()).Username()
}

// ── disable / enable ────────────────────────────────────────────────────────

// handleGameDisable and handleGameEnable serve POST /api/v1/games/{id}/
// {disable,enable}: the title family's switch (ADR-0107). A disabled game is
// hidden from Play — session start refuses its profiles with 409 GAME_DISABLED
// — while every catalogue write on it keeps working and the game stays in the
// library list with disabled: true. Both are curator, under the scope table's
// games grant, and each writes its own audit line.
func (s *Server) handleGameDisable(w http.ResponseWriter, r *http.Request, disabled bool) {
	if !s.catalogRequired(w, r) {
		return
	}
	game, ok := s.resolveGame(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectBody(w, r) {
		return
	}
	if err := s.store.SetGameDisabled(r.Context(), game.ID, disabled); err != nil {
		s.storeProblem(w, r, err, "set game disabled", "game")
		return
	}
	// Re-read so the 200 body is the row as stored, not the request's claim:
	// the flag the store holds is what the Play screen will render.
	fresh, err := s.store.GetGame(r.Context(), game.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get game", "game")
		return
	}
	event := "catalog.game.enable"
	detail := "the game is re-enabled and playable again"
	if disabled {
		event = "catalog.game.disable"
		detail = "the game is hidden from Play and narration; its profiles stay editable"
	}
	s.auditCatalogWrite(r, event, map[string]any{
		"game_id": game.ID, "slug": game.Slug, "disabled": disabled, "detail": detail,
	})
	writeJSON(w, http.StatusOK, toGameDTO(fresh))
}

// handleGameDisableRoute and handleGameEnableRoute are the two fixed halves
// of the game switch (ADR-0107); the shared body carries the store write,
// the re-read and the audit line.
func (s *Server) handleGameDisableRoute(w http.ResponseWriter, r *http.Request) {
	s.handleGameDisable(w, r, true)
}

func (s *Server) handleGameEnableRoute(w http.ResponseWriter, r *http.Request) {
	s.handleGameDisable(w, r, false)
}

// ── delete / purge ────────────────────────────────────────────────────────

// handleProfileDelete serves DELETE /api/v1/profiles/{id}: the soft mark
// (deleted_at + purge_after, one compare-and-swap, ADR-0107). The 200 body is
// the marked profile — the receipt the operator's UI shows ("deleted, purge
// after …") — with its deleted_at and purge_after now filled in. Refusals are
// the store's named 409s (publication history, derived revisions, already
// deleted) plus the API's session guard.
func (s *Server) handleProfileDelete(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.rejectIfSessionUsesProfile(w, r, id) {
		return
	}
	p, err := s.store.SoftDeleteProfile(r.Context(), id)
	if err != nil {
		s.profileLifecycleProblem(w, r, err, "delete profile")
		return
	}
	s.auditCatalogWrite(r, "catalog.profile.delete", map[string]any{
		"profile_id": p.ID, "edition_id": p.EditionID, "revision_num": p.RevisionNum,
		"purge_after": p.PurgeAfter,
	})
	writeJSON(w, http.StatusOK, toProfileDTO(p))
}

// purgeReportDTO is the 200 body of the purge: what the transaction removed,
// so the admin's UI and the audit line agree on the scope of the act.
type purgeReportDTO struct {
	ProfileID    string   `json:"profile_id"`
	CacheEntries int      `json:"cache_entries"`
	DeletedBlobs []string `json:"deleted_blobs"`
}

// handleProfilePurge serves POST /api/v1/profiles/{id}/purge: the admin half
// of the two-step delete (ADR-0107). It re-checks the mark and the retention
// window, re-runs the soft-delete guards, and then — one store transaction —
// drops the profile's speech-cache rows and the profile row itself (the
// content cascades, the FTS triggers follow). After the commit this handler
// finishes the ADR-0041 half: every hash the purge dropped is re-checked
// (catalogue reference AND surviving cache rows) before its bytes go, so a
// shared blob survives the act.
func (s *Server) handleProfilePurge(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	id := r.PathValue("id")
	if !s.rejectIfSessionUsesProfile(w, r, id) {
		return
	}
	report, err := s.store.PurgeProfile(r.Context(), id)
	if err != nil {
		s.profileLifecycleProblem(w, r, err, "purge profile")
		return
	}

	// The blob cleanup runs after the commit, against the post-commit state:
	// the reference check sees the content rows already gone, and the cache
	// count sees the cache rows already dropped. Either count nonzero keeps
	// the bytes — the discipline ADR-0041 established for eviction, applied
	// to the purge.
	deleted := s.deleteUnreferencedBlobs(r, append(append([]string{}, report.CacheBlobs...), report.ReferencedBlobs...))
	if deleted == nil {
		// The purge removed no blob bytes (every hash stayed referenced or the
		// asset store is unwired): the report's deleted_blobs is an empty list,
		// not null — the schema the operator's UI parses is the contract.
		deleted = []string{}
	}
	s.auditCatalogWrite(r, "catalog.profile.purge", map[string]any{
		"profile_id":    id,
		"cache_entries": report.CacheEntries,
		"deleted_blobs": deleted,
	})
	writeJSON(w, http.StatusOK, purgeReportDTO{
		ProfileID:    id,
		CacheEntries: report.CacheEntries,
		DeletedBlobs: deleted,
	})
}

// deleteUnreferencedBlobs removes the hashes nothing names any more and
// returns the ones it actually removed. The reference check is the store's
// AssetReferenced — the catalogue-side half (ADR-0041), which is why an
// unwired store (an impossible half: the purge ran through the same store)
// and a nil asset store (the unwired half) both make the purge report its
// index work and skip the bytes: the blobs then wait for eviction's own
// reference-before-delete pass, which is the honest degradation rather than
// a delete the purge could not prove safe.
func (s *Server) deleteUnreferencedBlobs(r *http.Request, hashes []string) []string {
	var out []string
	if s.assets == nil || s.store == nil {
		return out
	}
	seen := map[string]struct{}{}
	for _, h := range hashes {
		if h == "" {
			continue
		}
		if _, dup := seen[h]; dup {
			continue
		}
		seen[h] = struct{}{}
		referenced, err := s.store.AssetReferenced(r.Context(), h)
		if err != nil || referenced {
			continue // a failed check is not a "no reference" (ADR-0041)
		}
		n, err := s.store.SpeechCacheRowCountByHash(r.Context(), h)
		if err != nil || n > 0 {
			continue // the cache still addresses it; the count is the cache-side reference
		}
		if err := s.assets.Delete(r.Context(), h); err != nil {
			// Already gone: the row said one thing the filesystem says
			// another, and the row is the one that is about to be gone too.
			s.log.Warn("purge: deleting unreferenced blob",
				"request_id", RequestIDFromContext(r.Context()), "hash", h[:16], "error", err.Error())
			continue
		}
		out = append(out, h)
	}
	return out
}

// profileLifecycleProblem maps the lifecycle sentinels onto their documented
// 409s (ADR-0107's consequence list). Everything else falls through to the
// catalog mapping: a lifecycle error that is not one of the named guards is a
// store fault, and the catalog's 503 sentence is the honest report for it.
func (s *Server) profileLifecycleProblem(w http.ResponseWriter, r *http.Request, err error, op string) {
	switch {
	case errors.Is(err, store.ErrProfileAlreadyDeleted):
		s.writeCodeProblem(w, r, http.StatusConflict, "profile-already-deleted", codeConflict, "Conflict",
			"this profile is already deleted; the mark's purge_after is the retention deadline, and the purge is the other half of the act")
	case errors.Is(err, store.ErrProfileHasPublicationHistory):
		s.writeCodeProblem(w, r, http.StatusConflict, "profile-has-publication-history", codeConflict, "Conflict",
			"this revision has publication history in publish_actions; the audit trail outlives the revision (0004), so a shipped revision retires via archive, not delete")
	case errors.Is(err, store.ErrProfileHasDerivedRevisions):
		s.writeCodeProblem(w, r, http.StatusConflict, "profile-has-derived-revisions", codeConflict, "Conflict",
			"another revision descends from this one (parent_revision); delete the derived revisions first")
	case errors.Is(err, store.ErrProfileNotDeleted):
		s.writeCodeProblem(w, r, http.StatusConflict, "profile-not-deleted", codeConflict, "Conflict",
			"this profile is not soft-deleted; purging is the second half of a two-step act, and DELETE /api/v1/profiles/{id} is the first")
	case errors.Is(err, store.ErrRetentionNotElapsed):
		s.writeCodeProblem(w, r, http.StatusConflict, "retention-not-elapsed", "RETENTION_NOT_ELAPSED", "Conflict",
			"the retention window has not closed; the purge_after on the delete receipt is the deadline")
	default:
		s.storeProblem(w, r, err, op, "profile")
	}
}
