package api

// The curator's speaker-link action (ADR-0070). An import of a source that
// attributes lines to opaque speaker ids (the Wind Waker BMG's per-message
// attribute byte) creates one proposed placeholder character per id, with its
// lines proposed. This endpoint records the curator's identification of one
// of those placeholders with a real character, in one transaction:
//
//	POST /api/v1/profiles/{id}/characters/{cid}/speaker-link
//	     {"target_character_id": "…"}
//
//   - re-points every line of {cid} at the target (confirmed at 1.0, the
//     source-field evidence kept and the acting curator added as a reviewer
//     entry);
//   - records one `manual` speaker_assertion per re-pointed line (the audit
//     trail separates the dump's claim from the curator's identification,
//     docs/spec/02);
//   - folds the placeholder's name (the opaque key) into the target's
//     aliases, so a re-import binds the key straight to the target;
//   - retires the placeholder, whose identity is now the target's.
//
// The write is draft-only (the store's content write refuses an immutable
// revision regardless) and carries the profile's row version as If-Match,
// the same token the message edit requires (ADR-0015).

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

type speakerLinkRequest struct {
	TargetCharacterID string `json:"target_character_id"`
	// Name is the character the placeholder's speaker is, by name (ADR 0134):
	// the existing link when the roster already holds a non-retired row with
	// that exact name, or a new confirmed character created in the same
	// transaction when it does not. One of name / target_character_id is
	// required; when both are given they must name the same row.
	Name string `json:"name,omitempty"`
}

// speakerLinkResponse is the transaction's outcome, counts only: which row
// was identified, with which one, and how many lines moved. TargetName and
// Created are the name half of the answer (ADR 0134): which name the target
// carries, and whether the link created it.
type speakerLinkResponse struct {
	ProfileID         string `json:"profile_id"`
	SourceCharacterID string `json:"source_character_id"`
	TargetCharacterID string `json:"target_character_id"`
	RepointedMessages int    `json:"repointed_messages"`
	TargetName        string `json:"target_name,omitempty"`
	Created           bool   `json:"created,omitempty"`
}

// errNameMismatch is the 400 the name half of the request earns when both
// halves were given and they name two different rows (ADR 0134): the caller
// said two different things, and neither half is the answer.
var errNameMismatch = errors.New("speaker-link: name does not carry the target character")

// errSelfLink is the 400 the self-link earns whichever half named it: the
// id half said so outright, the name half resolved to the placeholder's own
// row. The refusal is the same either way — a placeholder is never its own
// identification.
var errSelfLink = errors.New("speaker-link: the placeholder names itself")

// handleSpeakerLink serves POST /api/v1/profiles/{id}/characters/{cid}/speaker-link.
func (s *Server) handleSpeakerLink(w http.ResponseWriter, r *http.Request) {
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
	var req speakerLinkRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if req.TargetCharacterID == "" && name == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-field", codeInvalidParameter,
			"Bad Request", "target_character_id or name is required")
		return
	}
	if name != "" && len(name) > 200 {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-parameter", codeInvalidParameter,
			"Bad Request", "name must be at most 200 characters")
		return
	}
	target := req.TargetCharacterID
	if target != "" && !games.ValidID(target) {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-parameter", codeInvalidParameter,
			"Bad Request", "target_character_id must be a character identifier")
		return
	}
	if target == cid && name == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "self-link", codeInvalidParameter,
			"Bad Request", "a placeholder cannot be linked to itself; the link names a different character")
		return
	}
	// Draft-only, stated plainly: the store's content write would refuse an
	// immutable revision anyway, but a review revision is frozen for curation,
	// and the recovery (derive a draft revision) is the same sentence.
	if p.Status != games.StatusDraft {
		s.writeCodeProblem(w, r, http.StatusConflict, "draft-only", codeConflict, "Conflict",
			fmt.Sprintf("revision %d is %s; the link may only be recorded on a draft — derive a revision and link there", p.RevisionNum, p.Status))
		return
	}
	if _, ok := s.ifMatchRowVersion(w, r, version); !ok {
		return
	}

	actor := ""
	if id := IdentityFromContext(r.Context()); id != nil {
		actor = id.Username()
	}
	if actor == "" {
		actor = "unknown"
	}

	var resp speakerLinkResponse
	err := s.store.InTx(r.Context(), func(tx *store.Tx) error {
		const op = "link speaker"
		ctx := store.WithTx(r.Context(), tx)
		chars, err := s.store.ListCharacters(ctx, p.ID)
		if err != nil {
			return err
		}
		// The name half (ADR 0134), resolved before the rows are pulled: with
		// no target id, the name resolves to the roster's non-retired row
		// with that exact name — the existing link when one exists — or to a
		// new confirmed character created in this same transaction when it
		// does not (the curation decision IS the confirmation, so the row
		// lands confirmed, not proposed). When both halves were given, they
		// must name the same row: the caller said two different things. A
		// target the roster does not have falls through to the 404
		// twoCharacters answers, the same as before ADR 0134.
		if name != "" {
			if target == "" {
				for _, c := range chars {
					if c.Name == name && c.Status != games.CharacterRetired {
						target = c.ID
						break
					}
				}
				if target == "" {
					stamp := time.Now().UTC().Format(time.RFC3339)
					created := games.Character{
						ID:        games.NewID(),
						ProfileID: p.ID,
						Name:      name,
						Status:    games.CharacterConfirmed,
						CreatedAt: stamp,
					}
					if err := created.Validate(); err != nil {
						return err
					}
					if err := s.store.PutCharacters(ctx, p.ID, []games.Character{created}); err != nil {
						return err
					}
					resp.Created = true
					target = created.ID
					chars = append(chars, created)
				}
			} else {
				for _, c := range chars {
					if c.ID == target && c.Name != name {
						return fmt.Errorf("store: %s: %w", op, errNameMismatch)
					}
				}
			}
		}
		if target == cid {
			// A self-link, whichever half named it: the id half said so
			// outright, the name half resolved to the placeholder's own row.
			return fmt.Errorf("store: %s: the placeholder names itself: %w", op, errSelfLink)
		}
		source, targetRow, err := twoCharacters(chars, cid, target)
		if err != nil {
			return err
		}
		if source.Status == games.CharacterRetired {
			return fmt.Errorf("store: %s: the placeholder is already retired: %w", op, store.ErrConflict)
		}
		if targetRow.Status == games.CharacterRetired {
			return fmt.Errorf("store: %s: the target is retired; link a live character: %w", op, store.ErrConflict)
		}
		// Every line that currently points at the placeholder, pages of it:
		// the placeholder of a whole disc import can hold hundreds of lines.
		lines := []games.Message{}
		cursor := ""
		for {
			page, next, err := s.store.ListMessages(ctx, p.ID, store.MessageFilter{SpeakerID: cid}, cursor, store.MaxListLimit)
			if err != nil {
				return err
			}
			lines = append(lines, page...)
			if next == "" {
				break
			}
			cursor = next
		}
		stamp := time.Now().UTC().Format(time.RFC3339)
		updates := make([]games.Message, 0, len(lines))
		assertions := make([]games.SpeakerAssertion, 0, len(lines))
		for _, m := range lines {
			m.SpeakerID = target
			m.AttributionStatus = games.AttributionConfirmed
			m.AttributionConfidence = 1.0
			m.AttributionEvidence = withReviewerEntry(m.AttributionEvidence, actor)
			m.UpdatedAt = stamp
			if err := m.Validate(); err != nil {
				return err
			}
			updates = append(updates, m)
			assertions = append(assertions, games.SpeakerAssertion{
				ID:          speakerLinkAssertionID(m.ID, target, games.AssertionManual),
				ProfileID:   p.ID,
				MessageID:   m.ID,
				CharacterID: target,
				Method:      games.AssertionManual,
				AssertedAt:  stamp,
				Evidence:    manualAssertionEvidence(cid, actor),
				Posterior:   1.0,
				Reviewer:    actor,
			})
		}
		if err := s.store.PutMessages(ctx, p.ID, updates, nil); err != nil {
			return err
		}
		if len(assertions) > 0 {
			if err := s.store.PutSpeakerAssertions(ctx, p.ID, assertions); err != nil {
				return err
			}
		}
		// Fold the placeholder's name and spellings into the target's
		// aliases: a re-import's roster match keys on the folded alias, so
		// the key binds to the target from the next import on.
		extra := append([]string{source.Name}, jsonStringSlice(source.Aliases)...)
		targetRow.Aliases = mergeAliases(targetRow.Aliases, extra...)
		if err := targetRow.Validate(); err != nil {
			return err
		}
		source.Status = games.CharacterRetired
		if err := source.Validate(); err != nil {
			return err
		}
		if err := s.store.PutCharacters(ctx, p.ID, []games.Character{targetRow, source}); err != nil {
			return err
		}
		resp = speakerLinkResponse{
			ProfileID:         p.ID,
			SourceCharacterID: source.ID,
			TargetCharacterID: targetRow.ID,
			RepointedMessages: len(updates),
			TargetName:        targetRow.Name,
			Created:           resp.Created,
		}
		return nil
	})
	if err != nil {
		s.speakerLinkProblem(w, r, err, p.ID, cid, target)
		return
	}
	_, fresh, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	s.auditCatalogWrite(r, "catalog.speaker.link", map[string]any{
		"profile_id":          p.ID,
		"source_character_id": cid,
		"target_character_id": resp.TargetCharacterID, "repointed_messages": resp.RepointedMessages,
		"target_name": resp.TargetName, "created": resp.Created,
	})
	writeJSON(w, http.StatusOK, resp)
}

// twoCharacters pulls both rows out of the roster. Either being absent is the
// same 404 the rest of the profile reads answer with.
func twoCharacters(chars []games.Character, sourceID, targetID string) (source, target games.Character, err error) {
	for _, c := range chars {
		switch c.ID {
		case sourceID:
			source = c
		case targetID:
			target = c
		}
	}
	if source.ID == "" {
		err = fmt.Errorf("store: link speaker: no character %s in this revision: %w", sourceID, store.ErrNotFound)
	} else if target.ID == "" {
		err = fmt.Errorf("store: link speaker: no character %s in this revision: %w", targetID, store.ErrNotFound)
	}
	return source, target, err
}

// withReviewerEntry keeps the evidence the line already carries (the
// source-field claim, byte for byte) and appends the curator's. The array is
// rebuilt, never mutated in place.
func withReviewerEntry(existing json.RawMessage, actor string) json.RawMessage {
	var entries []map[string]any
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &entries)
	}
	entries = append(entries, map[string]any{
		"method":    "manual",
		"reviewer":  actor,
		"posterior": 1.0,
	})
	raw, err := json.Marshal(entries)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// manualAssertionEvidence is the speaker_assertions.evidence of the curator's
// identification: which placeholder was identified, and by whom. No game
// content leaves the revision (AGENTS.md rule 5) — the ids are the revision's
// own identifiers.
func manualAssertionEvidence(placeholderID, actor string) json.RawMessage {
	rec := map[string]string{"kind": "manual", "ref": placeholderID}
	if actor != "" {
		rec["excerpt"] = "reviewer " + actor
	}
	raw, err := json.Marshal([]map[string]string{rec})
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// speakerLinkAssertionID derives the assertion's id from the (message,
// character, method) triple the same way profileio's import derives its own
// assertion ids: a content hash, stable across re-runs, so linking the same
// line to the same target twice is an upsert, not a second row. The
// derivation is restated here rather than imported because profileio keeps
// it private and the api package must not reach into import internals.
func speakerLinkAssertionID(messageID, characterID string, method games.AssertionMethod) string {
	h := sha256.Sum256([]byte(messageID + "\x00" + characterID + "\x00" + string(method)))
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	var buf [17]byte
	copy(buf[:], h[:])
	buf[0] &= 0x3f
	out := make([]byte, 26)
	for i := range out {
		var field byte
		for bit := 0; bit < 5; bit++ {
			pos := i*5 + bit
			if buf[pos/8]&(1<<uint(7-(pos%8))) != 0 {
				field |= 1 << uint(4-bit)
			}
		}
		out[i] = crockford[field]
	}
	return string(out)
}

// mergeAliases unions the target's aliases with the placeholder's name and
// aliases, keeping the target's order and skipping exact duplicates: the
// importer folds before it matches, so plain dedup is all the re-import
// needs.
func mergeAliases(existing json.RawMessage, add ...string) json.RawMessage {
	var cur []string
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &cur)
	}
	seen := make(map[string]bool, len(cur))
	for _, s := range cur {
		seen[s] = true
	}
	for _, s := range add {
		if s == "" || seen[s] {
			continue
		}
		cur = append(cur, s)
		seen[s] = true
	}
	raw, err := json.Marshal(cur)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// jsonStringSlice reads a JSON string array the way the store stores
// aliases; a corrupt payload degrades to the empty list rather than an
// error, because the row already passed the schema's json_valid check.
func jsonStringSlice(raw json.RawMessage) []string {
	var out []string
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// speakerLinkProblem maps the transaction's errors onto the wire contract:
// unknown rows are 404s (the reads answered that way too), the two retired
// states and a moved row version are conflicts, and a validation failure of a
// row this handler itself built is a 422 with the findings.
func (s *Server) speakerLinkProblem(w http.ResponseWriter, r *http.Request, err error, profileID, sourceID, targetID string) {
	switch {
	case errors.Is(err, errNameMismatch):
		s.writeCodeProblem(w, r, http.StatusBadRequest, "name-mismatch", codeInvalidParameter,
			"Bad Request", "name does not carry the character the target_character_id names; give one half, or make the two agree")
	case errors.Is(err, errSelfLink):
		s.writeCodeProblem(w, r, http.StatusBadRequest, "self-link", codeInvalidParameter,
			"Bad Request", "a placeholder cannot be linked to itself; the link names a different character")
	case errors.Is(err, store.ErrNotFound):
		s.writeCodeProblem(w, r, http.StatusNotFound, "character-not-found", codeNotFound, "Not Found",
			"no character of this profile revision has that identifier")
	case errors.Is(err, store.ErrRevisionConflict):
		s.writeCodeProblem(w, r, http.StatusPreconditionFailed, "revision-conflict", codeRevisionConflict,
			"Precondition Failed", "the revision moved while the link was being recorded; re-read and re-apply")
	case errors.Is(err, store.ErrConflict):
		s.writeCodeProblem(w, r, http.StatusConflict, "link-conflict", codeConflict, "Conflict",
			"the link cannot be recorded on the revision it was based on")
	case errors.Is(err, games.ErrValidation):
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
			"Unprocessable Entity", "a row the link produced does not satisfy its constraints",
			validationFindings(err))
	default:
		s.storeProblem(w, r, err, "link speaker", "character")
	}
}
