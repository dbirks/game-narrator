package api

// The correction labels (0002_content.sql's `corrections`) and the regression
// fixtures they are (ADR-0052). Three routes:
//
//   - GET  /api/v1/corrections     the curator's label list (curator);
//   - POST /api/v1/corrections     record a correction explicitly — the
//     inspector's "text incorrect / speaker incorrect / …" actions. It records
//     a label only; it never applies a fix (docs/spec/08 "user fixes become
//     labels");
//   - GET  /api/v1/dev/fixtures    the developer's browsing of the same rows as
//     the re-run inputs (docs/spec/08 "fixture recording and browsing",
//     docs/spec/09 "save a fixture and inspect a test report").
//
// The editor's own path — PATCH /api/v1/profiles/{id}/messages/{mid} — records
// its correction inside the message write's own transaction (see
// handleMessagePatch, ADR-0052), so "the correction landed" and "the label
// landed" are one fact.

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// correctionsListLimit is the hard ceiling of the list endpoints; the store
// clamps to the same bound (AGENTS.md "bounded queues").
const correctionsListLimit = 500

// correctionDTO is one row of the corrections table as the API serves it.
type correctionDTO struct {
	ID          string `json:"id"`
	ProfileID   string `json:"profile_id"`
	Kind        string `json:"kind"`
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	MessageID   string `json:"message_id,omitempty"`
	BeforeValue string `json:"before_value"`
	AfterValue  string `json:"after_value"`
	Reason      string `json:"reason,omitempty"`
	Evidence    []any  `json:"evidence"`
	Actor       string `json:"actor"`
	Status      string `json:"status"`
	Reviewer    string `json:"reviewer,omitempty"`
	CreatedAt   string `json:"created_at"`
	ReviewedAt  string `json:"reviewed_at,omitempty"`
}

func toCorrectionDTO(c games.Correction) correctionDTO {
	evidence := []any{}
	if len(c.Evidence) > 0 {
		if err := json.Unmarshal(c.Evidence, &evidence); err != nil {
			evidence = []any{string(c.Evidence)}
		}
	}
	return correctionDTO{
		ID:          c.ID,
		ProfileID:   c.ProfileID,
		Kind:        c.Kind.String(),
		TargetType:  c.TargetType.String(),
		TargetID:    c.TargetID,
		MessageID:   c.MessageID,
		BeforeValue: c.BeforeValue,
		AfterValue:  c.AfterValue,
		Reason:      c.Reason,
		Evidence:    evidence,
		Actor:       c.Actor,
		Status:      c.Status.String(),
		Reviewer:    c.Reviewer,
		CreatedAt:   c.CreatedAt,
		ReviewedAt:  c.ReviewedAt,
	}
}

// correctionsFilter parses the shared query of both list endpoints: an unknown
// kind is a 400 rather than a silently dropped filter (the same rule the
// messages list applies), and a named profile that the store does not have is a
// 404 from the read itself (ListCorrections enforces it), the way every other
// profile-scoped read answers — the filter names a revision, it does not
// invent one.
func (s *Server) correctionsFilter(w http.ResponseWriter, r *http.Request) (store.CorrectionFilter, bool) {
	q := r.URL.Query()
	limit, ok := s.limitParam(w, r, correctionsListLimit)
	if !ok {
		return store.CorrectionFilter{}, false
	}
	f := store.CorrectionFilter{Limit: limit}
	if profileID := strings.TrimSpace(q.Get("profile_id")); profileID != "" {
		if !games.ValidID(profileID) {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-profile", codeInvalidParameter,
				"Bad Request", "profile_id must be a profile identifier")
			return f, false
		}
		f.ProfileID = profileID
	}
	if kind := strings.TrimSpace(q.Get("kind")); kind != "" {
		parsed, err := games.ParseCorrectionKind(kind)
		if err != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-kind", codeInvalidParameter,
				"Bad Request", "kind must be one of: "+correctionKindList())
			return f, false
		}
		f.Kind = parsed.String()
	}
	return f, true
}

// correctionKindList renders the domain's own vocabulary, never a hand-typed
// copy (the enum test asserts the document against the same tables).
func correctionKindList() string {
	kinds := []games.CorrectionKind{
		games.CorrectionSpeaker, games.CorrectionText, games.CorrectionSpeechText,
		games.CorrectionScene, games.CorrectionRegion, games.CorrectionClassification,
		games.CorrectionEdge, games.CorrectionOther,
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k.String())
	}
	return strings.Join(out, ", ")
}

// handleCorrectionsList serves GET /api/v1/corrections.
func (s *Server) handleCorrectionsList(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	f, ok := s.correctionsFilter(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListCorrections(r.Context(), f)
	if err != nil {
		s.storeProblem(w, r, err, "list corrections", "correction")
		return
	}
	out := make([]correctionDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, toCorrectionDTO(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// handleDevFixturesList serves GET /api/v1/dev/fixtures: the developer's
// browsing of the recorded fixtures. Same rows, same shape — the documented
// difference is the reading (ADR-0052), not the data.
func (s *Server) handleDevFixturesList(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	f, ok := s.correctionsFilter(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListCorrections(r.Context(), f)
	if err != nil {
		s.storeProblem(w, r, err, "list fixtures", "fixture")
		return
	}
	out := make([]correctionDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, toCorrectionDTO(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

// createCorrectionRequest is the explicit-recording surface: a label for a fix
// the caller made (or is proposing). It carries no status and no reviewer:
// explicit labels land as `proposed`, and the review lifecycle belongs to the
// reviewer milestone, not to the recorder (games.Correction's validation
// forbids a proposed row from carrying a reviewer anyway).
type createCorrectionRequest struct {
	ProfileID   string          `json:"profile_id"`
	Kind        string          `json:"kind"`
	TargetType  string          `json:"target_type"`
	TargetID    string          `json:"target_id"`
	MessageID   string          `json:"message_id,omitempty"`
	BeforeValue string          `json:"before_value"`
	AfterValue  string          `json:"after_value,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	Evidence    json.RawMessage `json:"evidence,omitempty"`
}

// handleCreateCorrection serves POST /api/v1/corrections.
func (s *Server) handleCreateCorrection(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	var req createCorrectionRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	p, _, ok := s.resolveProfile(w, r, req.ProfileID)
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, p.DeletedAt) {
		return
	}
	kind, err := games.ParseCorrectionKind(strings.TrimSpace(req.Kind))
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-kind", codeInvalidParameter,
			"Bad Request", "kind must be one of: "+correctionKindList())
		return
	}
	target, err := games.ParseCorrectionTarget(strings.TrimSpace(req.TargetType))
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-target-type", codeInvalidParameter,
			"Bad Request", "target_type must name one of the content tables the correction labels")
		return
	}
	if strings.TrimSpace(req.TargetID) == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-target", codeInvalidParameter,
			"Bad Request", "target_id names the row the correction is about and is required")
		return
	}
	messageID := strings.TrimSpace(req.MessageID)
	if target == games.TargetMessage {
		// A message-targeted label is a label *about a message row*: the row must
		// exist on this revision, and the loose pointer must agree with the index
		// column (games.Correction's validation makes the two one value).
		if messageID == "" {
			messageID = strings.TrimSpace(req.TargetID)
		}
		if messageID != strings.TrimSpace(req.TargetID) {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "target-mismatch", codeInvalidParameter,
				"Bad Request", "a message correction's message_id must name the same row as target_id")
			return
		}
		if _, _, err := s.store.GetMessage(r.Context(), p.ID, messageID); err != nil {
			s.storeProblem(w, r, err, "resolve correction target", "message")
			return
		}
	}
	identity := IdentityFromContext(r.Context())
	c := games.Correction{
		ID:          games.NewID(),
		ProfileID:   p.ID,
		Kind:        kind,
		TargetType:  target,
		TargetID:    strings.TrimSpace(req.TargetID),
		MessageID:   messageID,
		BeforeValue: req.BeforeValue,
		AfterValue:  req.AfterValue,
		Reason:      strings.TrimSpace(req.Reason),
		Evidence:    orEvidence(req.Evidence),
		Actor:       identity.Username(),
		Status:      games.CorrectionProposed,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
	if err := c.Validate(); err != nil {
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-correction",
			codeValidationFailed, "Unprocessable Entity", "the correction does not satisfy its constraints",
			validationFindings(err))
		return
	}
	if err := s.store.PutCorrection(r.Context(), c); err != nil {
		s.storeProblem(w, r, err, "record correction", "correction")
		return
	}
	s.auditCatalogWrite(r, "catalog.correction.recorded", map[string]any{
		"correction_id": c.ID, "profile_id": p.ID, "kind": c.Kind, "target_id": c.TargetID,
	})
	writeJSON(w, http.StatusCreated, map[string]any{"correction": toCorrectionDTO(c)})
}

// orEvidence normalises the evidence member to the column's default when the
// caller sends nothing (the column is NOT NULL DEFAULT '[]').
func orEvidence(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("[]")
	}
	return raw
}
