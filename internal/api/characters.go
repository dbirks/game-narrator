package api

// The roster's direct write surface (GitHub #42, ADR-0180): before this, the
// only character writers a curator could reach over HTTP were speaker-link
// (a new row with exactly name + status, nothing else) and the agent-gated
// tools, so a row created by a link kept an empty role and an empty voice
// brief forever — the Cast page had no brief to show, analyze_character had
// no editorial prose, and the roster's role column read blank.
//
//	POST   /api/v1/profiles/{id}/characters         mint one roster row
//	PUT    /api/v1/profiles/{id}/characters/{cid}   update one row in place
//	DELETE /api/v1/profiles/{id}/characters/{cid}   retire one row, when unreferenced
//
// All three are draft-only (the store's content write refuses an immutable
// revision regardless) and carry the profile's row version as If-Match, the
// same token speaker-link and the message edit enforce (ADR-0015): 428 when
// absent, 412 when stale. The writes are operator-tier (ADR-0180): they touch
// every roster editorial field at once, including the voice brief the speech
// pipeline grounds on, and the scope table keeps that boundary in the table,
// not in the handler. DELETE retires, it does not drop the row — the schema
// keeps retired rows for history (the voice and correction rows that may
// still name them), so "delete" in the store's vocabulary is the status
// move speaker-link makes to a placeholder's own row.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// errWriteStopped is the sentinel the shared precondition block returns when
// it has already answered the request: the handler stops, it did not touch
// the store.
var errWriteStopped = errors.New("write stopped")

// characterWriteRequest is the body both writes take. Every field is a
// pointer so "absent" (keep the row's value) is distinct from "empty"
// (clear it): POST applies them to a new row, PUT to the existing one.
// The arrays travel as raw JSON, the way the store carries them —
// aliases and visual_descriptors are string arrays the importer folds,
// and verified_source_refs is the row's own source citations.
type characterWriteRequest struct {
	Name               *string          `json:"name,omitempty"`
	Aliases            *json.RawMessage `json:"aliases,omitempty"`
	Role               *string          `json:"role,omitempty"`
	PersonalitySummary *string          `json:"personality_summary,omitempty"`
	VisualDescriptors  *json.RawMessage `json:"visual_descriptors,omitempty"`
	VerifiedSourceRefs *json.RawMessage `json:"verified_source_refs,omitempty"`
	PortraitAssetID    *string          `json:"portrait_asset_id,omitempty"`
	Status             *string          `json:"status,omitempty"`
}

// noneSet is the 400's condition: a PUT that changes nothing is a read, and a
// POST that names nothing has nothing to mint.
func (b characterWriteRequest) noneSet() bool {
	return b.Name == nil && b.Aliases == nil && b.Role == nil &&
		b.PersonalitySummary == nil && b.VisualDescriptors == nil &&
		b.VerifiedSourceRefs == nil && b.PortraitAssetID == nil && b.Status == nil
}

// characterDeleteReceipt is DELETE's 200 body: which row left the roster, and
// where it sits now (retired — the roster's own answer to a review refusal).
type characterDeleteReceipt struct {
	ProfileID   string `json:"profile_id"`
	CharacterID string `json:"character_id"`
	Status      string `json:"status"`
}

// errCharacterReferenced is the 409 DELETE earns while another row still
// points at the character: a line that speaks it, an attribution that claims
// it, or a voice profile keyed by its name (the voice surface's own identity).
var errCharacterReferenced = errors.New("character is referenced")

// handleCreateCharacter serves POST /api/v1/profiles/{id}/characters.
func (s *Server) handleCreateCharacter(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, p.DeletedAt) {
		return
	}
	var req characterWriteRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	if req.Name == nil || strings.TrimSpace(*req.Name) == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-field", codeInvalidParameter,
			"Bad Request", "name is required")
		return
	}
	if err := s.characterWriteCommon(w, r, p, version, req); err != nil {
		return
	}
	char := s.buildNewCharacter(p.ID, req)
	if err := char.Validate(); err != nil {
		s.characterWriteProblem(w, r, err)
		return
	}
	if err := s.store.InTx(r.Context(), func(tx *store.Tx) error {
		ctx := store.WithTx(r.Context(), tx)
		return s.store.PutCharacters(ctx, p.ID, []games.Character{char})
	}); err != nil {
		s.characterWriteProblem(w, r, err)
		return
	}
	s.characterWriteReceipt(w, r, p, char, "catalog.character.create",
		map[string]any{"character_id": char.ID, "name": char.Name, "role": char.Role,
			"personality_summary_set": char.PersonalitySummary != ""})
	writeJSON(w, http.StatusCreated, toCharacterDTO(char))
}

// handleUpdateCharacter serves PUT /api/v1/profiles/{id}/characters/{cid}.
func (s *Server) handleUpdateCharacter(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, p.DeletedAt) {
		return
	}
	cid, ok := s.idFromPath(w, r, "cid")
	if !ok {
		return
	}
	var req characterWriteRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	if req.noneSet() {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "empty-body", codeInvalidParameter,
			"Bad Request", "at least one field must be set")
		return
	}
	if err := s.characterWriteCommon(w, r, p, version, req); err != nil {
		return
	}
	var out games.Character
	err := s.store.InTx(r.Context(), func(tx *store.Tx) error {
		ctx := store.WithTx(r.Context(), tx)
		chars, err := s.store.ListCharacters(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, c := range chars {
			if c.ID == cid {
				out = c
				break
			}
		}
		if out.ID == "" {
			return fmt.Errorf("store: update character: no character %s in this revision: %w", cid, store.ErrNotFound)
		}
		applyCharacterWrite(&out, req)
		if err := out.Validate(); err != nil {
			return err
		}
		return s.store.PutCharacters(ctx, p.ID, []games.Character{out})
	})
	if err != nil {
		s.characterWriteProblem(w, r, err)
		return
	}
	s.characterWriteReceipt(w, r, p, out, "catalog.character.update",
		map[string]any{"character_id": out.ID, "name": out.Name, "role": out.Role,
			"personality_summary_set": out.PersonalitySummary != ""})
	writeJSON(w, http.StatusOK, toCharacterDTO(out))
}

// handleDeleteCharacter serves DELETE /api/v1/profiles/{id}/characters/{cid}:
// the roster's delete, which in the schema's vocabulary is the retirement —
// the row stays for history, the name leaves the roster. It earns a 409
// while anything still points at the row: a line that speaks it, an
// attribution that claims it, or a voice profile keyed by its name.
func (s *Server) handleDeleteCharacter(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, p.DeletedAt) {
		return
	}
	cid, ok := s.idFromPath(w, r, "cid")
	if !ok {
		return
	}
	if p.Status != games.StatusDraft {
		s.writeCodeProblem(w, r, http.StatusConflict, "draft-only", codeConflict, "Conflict",
			fmt.Sprintf("revision %d is %s; the roster may only be edited on a draft — derive a revision and edit there", p.RevisionNum, p.Status))
		return
	}
	if _, ok := s.ifMatchRowVersion(w, r, version); !ok {
		return
	}

	var out games.Character
	err := s.store.InTx(r.Context(), func(tx *store.Tx) error {
		ctx := store.WithTx(r.Context(), tx)
		chars, err := s.store.ListCharacters(ctx, p.ID)
		if err != nil {
			return err
		}
		for _, c := range chars {
			if c.ID == cid {
				out = c
				break
			}
		}
		if out.ID == "" {
			return fmt.Errorf("store: delete character: no character %s in this revision: %w", cid, store.ErrNotFound)
		}
		if out.Status == games.CharacterRetired {
			return fmt.Errorf("store: delete character: %s: already retired: %w", cid, store.ErrConflict)
		}
		// The reference walk: a line that speaks the row, an attribution that
		// claims it, a voice keyed by its name. Each is the surface that would
		// dangle if the row left the roster outright.
		referenced := []string{}
		lines := 0
		cursor := ""
		for {
			page, next, err := s.store.ListMessages(ctx, p.ID, store.MessageFilter{SpeakerID: cid}, cursor, store.MaxListLimit)
			if err != nil {
				return err
			}
			lines += len(page)
			if next == "" {
				break
			}
			cursor = next
		}
		if lines > 0 {
			referenced = append(referenced, fmt.Sprintf("%d line(s)", lines))
		}
		asserts, err := s.store.ListSpeakerAssertions(ctx, p.ID)
		if err != nil {
			return err
		}
		claims := 0
		for _, a := range asserts {
			if a.CharacterID == cid {
				claims++
			}
		}
		if claims > 0 {
			referenced = append(referenced, fmt.Sprintf("%d attribution(s)", claims))
		}
		voices, err := s.store.ListVoiceProfiles(ctx, out.Name)
		if err != nil {
			return err
		}
		if len(voices) > 0 {
			referenced = append(referenced, fmt.Sprintf("%d voice profile(s)", len(voices)))
		}
		if len(referenced) > 0 {
			return fmt.Errorf("store: delete character: %s: referenced by %s: %w", cid, strings.Join(referenced, ", "), errCharacterReferenced)
		}
		out.Status = games.CharacterRetired
		if err := out.Validate(); err != nil {
			return err
		}
		return s.store.PutCharacters(ctx, p.ID, []games.Character{out})
	})
	if err != nil {
		switch {
		case errors.Is(err, errCharacterReferenced):
			s.writeCodeProblem(w, r, http.StatusConflict, "character-referenced", codeConflict, "Conflict",
				"the character still carries lines, attributions or a voice; clear or re-point them before it can leave the roster")
		case errors.Is(err, store.ErrNotFound):
			s.writeCodeProblem(w, r, http.StatusNotFound, "character-not-found", codeNotFound, "Not Found",
				"no character of this profile revision has that identifier")
		default:
			s.characterWriteProblem(w, r, err)
		}
		return
	}
	_, fresh, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	s.auditCatalogWrite(r, "catalog.character.delete", map[string]any{
		"profile_id": p.ID, "character_id": out.ID, "name": out.Name,
	})
	writeJSON(w, http.StatusOK, characterDeleteReceipt{
		ProfileID:   p.ID,
		CharacterID: out.ID,
		Status:      string(out.Status),
	})
}

// characterWriteCommon runs the shared preconditions of the two mutating
// writes and answers them itself; it returns nil when the handler should
// proceed. The 404 it cannot earn (an unknown {cid}) belongs to the write's
// own transaction, not here.
func (s *Server) characterWriteCommon(w http.ResponseWriter, r *http.Request, p games.Profile, version int64, req characterWriteRequest) error {
	if p.Status != games.StatusDraft {
		s.writeCodeProblem(w, r, http.StatusConflict, "draft-only", codeConflict, "Conflict",
			fmt.Sprintf("revision %d is %s; the roster may only be edited on a draft — derive a revision and edit there", p.RevisionNum, p.Status))
		return errWriteStopped
	}
	if _, ok := s.ifMatchRowVersion(w, r, version); !ok {
		return errWriteStopped
	}
	if req.Status != nil {
		st, err := games.ParseCharacterStatus(*req.Status)
		if err != nil {
			s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-status", codeValidationFailed,
				"Unprocessable Entity", "status must be one of proposed, confirmed or retired")
			return errWriteStopped
		}
		req.Status = new(string(st))
	}
	return nil
}

// characterWriteReceipt is the two writes' shared tail: the new ETag and the
// audit event, the event name supplied by the caller (create and update are
// separate audit facts).
func (s *Server) characterWriteReceipt(w http.ResponseWriter, r *http.Request, p games.Profile, char games.Character, event string, detail map[string]any) {
	_, fresh, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	s.auditCatalogWrite(r, event, detail)
}

// buildNewCharacter mints a row the POST body names: the id and the timestamp
// are the service's, the status defaults to proposed (the row is unaudited
// until a curator confirms it, the same default stage_character carries), and
// the arrays default to the empty list the schema itself carries.
func (s *Server) buildNewCharacter(profileID string, req characterWriteRequest) games.Character {
	stamp := time.Now().UTC().Format(time.RFC3339)
	status := games.CharacterProposed
	if req.Status != nil {
		status = games.CharacterStatus(*req.Status)
	}
	char := games.Character{
		ID:                 games.NewID(),
		ProfileID:          profileID,
		Name:               strings.TrimSpace(*req.Name),
		Aliases:            orEmptyArray(req.Aliases),
		VisualDescriptors:  orEmptyArray(req.VisualDescriptors),
		PersonalitySummary: derefOrEmpty(req.PersonalitySummary),
		Role:               derefOrEmpty(req.Role),
		VerifiedSourceRefs: orEmptyArray(req.VerifiedSourceRefs),
		PortraitAssetID:    derefOrEmpty(req.PortraitAssetID),
		Status:             status,
		CreatedAt:          stamp,
	}
	return char
}

// applyCharacterWrite overlays the fields the PUT named onto the row; the
// rest keeps their stored values.
func applyCharacterWrite(char *games.Character, req characterWriteRequest) {
	if req.Name != nil {
		char.Name = strings.TrimSpace(*req.Name)
	}
	if req.Aliases != nil {
		char.Aliases = orEmptyArray(req.Aliases)
	}
	if req.Role != nil {
		char.Role = *req.Role
	}
	if req.PersonalitySummary != nil {
		char.PersonalitySummary = *req.PersonalitySummary
	}
	if req.VisualDescriptors != nil {
		char.VisualDescriptors = orEmptyArray(req.VisualDescriptors)
	}
	if req.VerifiedSourceRefs != nil {
		char.VerifiedSourceRefs = orEmptyArray(req.VerifiedSourceRefs)
	}
	if req.PortraitAssetID != nil {
		char.PortraitAssetID = *req.PortraitAssetID
	}
	if req.Status != nil {
		char.Status = games.CharacterStatus(*req.Status)
	}
}

// characterWriteProblem maps the write's errors onto the wire contract: an
// unknown row is the 404 the roster reads answer, a moved row version is the
// 412 pair, a row the write produced that fails its own constraints is a 422
// with the findings, and the rest is the store's ordinary problem shape.
func (s *Server) characterWriteProblem(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeCodeProblem(w, r, http.StatusNotFound, "character-not-found", codeNotFound, "Not Found",
			"no character of this profile revision has that identifier")
	case errors.Is(err, store.ErrRevisionConflict):
		s.writeCodeProblem(w, r, http.StatusPreconditionFailed, "revision-conflict", codeRevisionConflict,
			"Precondition Failed", "the revision moved while the write was being recorded; re-read and re-apply")
	case errors.Is(err, store.ErrConflict):
		s.writeCodeProblem(w, r, http.StatusConflict, "character-conflict", codeConflict, "Conflict",
			"the write cannot be recorded on the revision it was based on")
	case errors.Is(err, games.ErrValidation):
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
			"Unprocessable Entity", "the row does not satisfy its constraints",
			validationFindings(err))
	default:
		s.storeProblem(w, r, err, "write character", "character")
	}
}

// orEmptyArray reads a raw-JSON array pointer the way the store stores the
// roster's array fields: nil means the empty list, and a non-empty payload is
// carried through as-is (the row's Validate checks it).
func orEmptyArray(raw *json.RawMessage) json.RawMessage {
	if raw == nil {
		return json.RawMessage("[]")
	}
	if len(*raw) == 0 {
		return json.RawMessage("[]")
	}
	return *raw
}

// derefOrEmpty reads a string pointer the same way: nil keeps "".
func derefOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
