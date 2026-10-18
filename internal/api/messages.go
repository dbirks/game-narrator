package api

// /api/v1/profiles/{id}/messages… — the dialogue browser and the one hand-typed
// edit the curator makes (docs/spec/08 "GET/PATCH /api/v1/profiles/:id/messages/:id":
// "original vs speech text edits, immutable source refs"; docs/spec/02 "Editing
// the editable half of a message").
//
// The rule this file exists to enforce is a single sentence: `original_text` is
// never written through this API. docs/spec/02 "Normalization and markup" makes
// it the fingerprint of what the source said — the thing an occurrence is
// matched against and an audit compares to — while `speech_text` is the curator's
// rendering of it. internal/store.UpdateMessageSpeechText is the only write
// either can perform, and it takes a field list, not a row: there is no path
// from this handler to the original column even by accident. The 409 that a
// PATCH on a published revision answers is the same rule seen from the other
// side: content belongs to a draft (ADR-0015).
//
// If-Match here is the *profile's* row version, not a per-message token. The
// store's one-write-one-bump discipline (docs/spec/12 "one row version bump per
// batch") means the revision is the unit of concurrency; a per-message ETag would
// imply that two messages could be edited concurrently without disagreeing,
// which the row version exists to make impossible.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// maxSearchLimit mirrors store.MaxSearchLimit: a search hit carries a whole
// message row, so the bound is tighter than a list's (docs/spec/16's 10 000-message
// benchmark is the reason this is a hard ceiling rather than a default).
const maxSearchLimit = 100

type messageDTO struct {
	ID                    string          `json:"id"`
	ProfileID             string          `json:"profile_id"`
	SourceID              string          `json:"source_id,omitempty"`
	SourceMessageID       string          `json:"source_message_id,omitempty"`
	OriginalText          string          `json:"original_text"`
	Markup                json.RawMessage `json:"markup"`
	NormalizedText        string          `json:"normalized_text"`
	SpeechText            string          `json:"speech_text"`
	Language              string          `json:"language,omitempty"`
	TextKind              string          `json:"text_kind"`
	SceneID               string          `json:"scene_id,omitempty"`
	SpeakerID             string          `json:"speaker_id,omitempty"`
	AttributionStatus     string          `json:"attribution_status"`
	AttributionConfidence float64         `json:"attribution_confidence"`
	AttributionEvidence   json.RawMessage `json:"attribution_evidence"`
	Flags                 json.RawMessage `json:"flags"`
	Revision              int             `json:"revision"`
	CreatedAt             string          `json:"created_at"`
	UpdatedAt             string          `json:"updated_at"`
}

func toMessageDTO(m games.Message) messageDTO {
	return messageDTO{
		ID:                    m.ID,
		ProfileID:             m.ProfileID,
		SourceID:              m.SourceID,
		SourceMessageID:       m.SourceMessageID,
		OriginalText:          m.OriginalText,
		Markup:                defaultJSONArray(m.MarkupJSON),
		NormalizedText:        m.NormalizedText,
		SpeechText:            m.SpeechText,
		Language:              m.Language,
		TextKind:              string(m.TextKind),
		SceneID:               m.SceneID,
		SpeakerID:             m.SpeakerID,
		AttributionStatus:     string(m.AttributionStatus),
		AttributionConfidence: m.AttributionConfidence,
		AttributionEvidence:   defaultJSONArray(m.AttributionEvidence),
		Flags:                 defaultJSONObject(m.Flags),
		Revision:              m.Revision,
		CreatedAt:             m.CreatedAt,
		UpdatedAt:             m.UpdatedAt,
	}
}

type messagePageDTO struct {
	MessageID       string          `json:"message_id"`
	Ordinal         int             `json:"ordinal"`
	SourcePageID    string          `json:"source_page_id,omitempty"`
	PageText        string          `json:"page_text"`
	VisiblePrefixes json.RawMessage `json:"visible_prefixes"`
	DisplayControls json.RawMessage `json:"display_controls"`
	TimingJSON      json.RawMessage `json:"timing_json"`
}

func toMessagePageDTO(p games.MessagePage) messagePageDTO {
	return messagePageDTO{
		MessageID:       p.MessageID,
		Ordinal:         p.Ordinal,
		SourcePageID:    p.SourcePageID,
		PageText:        p.PageText,
		VisiblePrefixes: defaultJSONArray(p.VisiblePrefixes),
		DisplayControls: defaultJSONArray(p.DisplayControls),
		TimingJSON:      defaultJSONObject(p.TimingJSON),
	}
}

// messageHitDTO is one search result. `bm25` is SQLite's own score, lower
// first: the store passes it through unmodified rather than rescaling it, and so
// does this API, because a number that has been through two transformations can
// be compared with nothing (internal/store/repo_search.go makes the same
// argument). A short query that took the prefix-scan path scores 0, which is the
// store's honest "this was not ranked" value.
type messageHitDTO struct {
	Message messageDTO `json:"message"`
	BM25    float64    `json:"bm25"`
}

// messageDetailResponse is what one message reads as: the row plus its display
// pages, because docs/spec/02 insists a page is not a message and the browser
// needs both to render a textbox advance.
type messageDetailResponse struct {
	Message messageDTO       `json:"message"`
	Pages   []messagePageDTO `json:"pages"`
}

// handleMessagesList serves GET /api/v1/profiles/{id}/messages with the three
// filters docs/spec/09's dialogue browser has and cursor pagination.
func (s *Server) handleMessagesList(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	limit, ok := s.limitParam(w, r, maxListLimit)
	if !ok {
		return
	}
	q := r.URL.Query()
	filter, perr := s.messageFilter(q.Get("kind"))
	if perr != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-kind", codeInvalidParameter,
			"Bad Request", perr.Error())
		return
	}
	filter.SceneID = strings.TrimSpace(q.Get("scene"))
	filter.SpeakerID = strings.TrimSpace(q.Get("speaker"))
	for name, value := range map[string]string{"scene": filter.SceneID, "speaker": filter.SpeakerID} {
		if value != "" && !games.ValidID(value) {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-"+name, codeInvalidParameter,
				"Bad Request", name+" must be a profile-scoped identifier")
			return
		}
	}

	msgs, next, err := s.store.ListMessages(r.Context(), p.ID, filter, q.Get("cursor"), limit)
	if err != nil {
		s.storeProblem(w, r, err, "list messages", "message")
		return
	}
	out := make([]messageDTO, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, toMessageDTO(m))
	}
	// The revision token rides along on a list read: the dialogue browser edits
	// from the list, and forcing a GET /profiles/{id} per row to obtain the ETag
	// would be one round trip per keystroke.
	body := map[string]any{"messages": out, "profile_etag": etagFor(version)}
	if next != "" {
		body["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, body)
}

// messageFilter parses the ?kind= filter. An unknown text kind is refused rather
// than dropped: a browser that filtered by "dialgue" and got the unfiltered list
// would show the curator content they asked not to see.
func (s *Server) messageFilter(kind string) (store.MessageFilter, error) {
	var f store.MessageFilter
	if kind == "" {
		return f, nil
	}
	parsed, err := games.ParseTextKind(kind)
	if err != nil {
		return f, err
	}
	f.TextKind = parsed
	return f, nil
}

// handleMessageSearch serves GET /api/v1/profiles/{id}/messages/search?q=.
// The score is SQLite's bm25 passed through unmodified: lower is better, and
// rescaling it would invite comparisons across queries or SQLite versions that
// the number cannot support (internal/store/repo_search.go argues this at length).
func (s *Server) handleMessageSearch(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	query := r.URL.Query().Get("q")
	if strings.TrimSpace(query) == "" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-query", codeInvalidParameter,
			"Bad Request",
			"q is required: an empty search is refused rather than turned into a full listing")
		return
	}
	limit, ok := s.limitParam(w, r, maxSearchLimit)
	if !ok {
		return
	}
	hits, err := s.store.SearchMessages(r.Context(), p.ID, query, limit)
	if err != nil {
		s.storeProblem(w, r, err, "search messages", "message")
		return
	}
	out := make([]messageHitDTO, 0, len(hits))
	for _, h := range hits {
		out = append(out, messageHitDTO{Message: toMessageDTO(h.Message), BM25: h.Score})
	}
	setETag(w, version)
	writeJSON(w, http.StatusOK, map[string]any{"query": query, "hits": out})
}

// handleMessageGet serves GET /api/v1/profiles/{id}/messages/{mid} with pages.
func (s *Server) handleMessageGet(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	mid, ok := s.idFromPath(w, r, "mid")
	if !ok {
		return
	}
	m, pages, err := s.store.GetMessage(r.Context(), p.ID, mid)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeCodeProblem(w, r, http.StatusNotFound, "message-not-found", codeNotFound, "Not Found",
				"no message of this profile revision has that identifier")
			return
		}
		s.storeProblem(w, r, err, "get message", "message")
		return
	}
	out := messageDetailResponse{Message: toMessageDTO(m), Pages: make([]messagePageDTO, 0, len(pages))}
	for _, pg := range pages {
		out.Pages = append(out.Pages, toMessagePageDTO(pg))
	}
	setETag(w, version)
	writeJSON(w, http.StatusOK, out)
}

// patchMessageRequest is the editable half of a message, and only that.
// `original_text` is not a field of this request; a client that sends it gets a
// 400 from DisallowUnknownFields, which is the honest answer to "you asked to
// change something this API will not change".
type patchMessageRequest struct {
	SpeechText *string `json:"speech_text"`
}

// handleMessagePatch serves PATCH /api/v1/profiles/{id}/messages/{mid} with
// If-Match, and returns the new token so the editor's next save is possible.
func (s *Server) handleMessagePatch(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	mid, ok := s.idFromPath(w, r, "mid")
	if !ok {
		return
	}
	var req patchMessageRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	if req.SpeechText == nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-field", codeInvalidParameter,
			"Bad Request", "speech_text is the only field this endpoint edits, and it is required")
		return
	}
	expected, ok := s.ifMatchRowVersion(w, r, version)
	if !ok {
		return
	}
	// The correction label (ADR-0052): a speech-text save is a user fix, and
	// the fix is the regression fixture of its target — what was wrong
	// (before), what it should be (after, the assertion a re-run checks) and
	// who decided (actor). The before-value is read here and the write's own
	// if-match check makes the read consistent with the update: a concurrent
	// edit moves the row version and the write refuses below. A save that
	// changes nothing is not a correction, so it records nothing. A save that
	// clears the rendering is still a valid edit (silence is a rendering
	// choice, docs/spec/02), but it records no label: an accepted correction
	// must carry the assertion a re-run checks, and an empty one asserts
	// nothing.
	before, _, err := s.store.GetMessage(r.Context(), p.ID, mid)
	if err != nil {
		s.storeProblem(w, r, err, "read message", "message")
		return
	}
	var correction *games.Correction
	if *req.SpeechText != before.SpeechText && *req.SpeechText != "" {
		identity := IdentityFromContext(r.Context())
		correction = &games.Correction{
			ID:          games.NewID(),
			ProfileID:   p.ID,
			Kind:        games.CorrectionSpeechText,
			TargetType:  games.TargetMessage,
			TargetID:    mid,
			MessageID:   mid,
			BeforeValue: before.SpeechText,
			AfterValue:  *req.SpeechText,
			Reason:      "speech text corrected in the dialogue editor",
			Evidence:    json.RawMessage("[]"),
			Actor:       identity.Username(),
			Status:      games.CorrectionAccepted, // the fix is applied by this very save
			Reviewer:    identity.Username(),
			CreatedAt:   time.Now().UTC().Format(time.RFC3339),
			ReviewedAt:  time.Now().UTC().Format(time.RFC3339),
		}
	}
	// The message write and the correction record join one transaction: the
	// store composes on the caller's transaction (the same way import →
	// publish does), so "the edit landed" and "the fixture landed" are one
	// fact and a crash between them cannot happen.
	var updated games.Message
	err = s.store.InTx(r.Context(), func(tx *store.Tx) error {
		ctx := store.WithTx(r.Context(), tx)
		m, err := s.store.UpdateMessageSpeechText(ctx, p.ID, mid, *req.SpeechText, expected)
		if err != nil {
			return err
		}
		if correction != nil {
			if err := s.store.PutCorrection(ctx, *correction); err != nil {
				return err
			}
		}
		updated = m
		return nil
	})
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeCodeProblem(w, r, http.StatusNotFound, "message-not-found", codeNotFound, "Not Found",
				"no message of this profile revision has that identifier")
			return
		}
		if errors.Is(err, games.ErrValidation) {
			s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
				"Unprocessable Entity", "the edited message does not satisfy its constraints",
				validationFindings(err))
			return
		}
		s.profileWriteProblem(w, r, err, "update message speech text")
		return
	}
	_, fresh, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	audit := map[string]any{"profile_id": p.ID, "message_id": updated.ID, "revision": updated.Revision}
	if correction != nil {
		audit["correction_id"] = correction.ID
	}
	s.auditCatalogWrite(r, "catalog.message.edit", audit)
	writeJSON(w, http.StatusOK, messageDetailResponse{
		Message: toMessageDTO(updated),
		Pages:   []messagePageDTO{},
	})
}

// validationFindings splits a games.Validate error (errors.Join of one problem
// per field) into the list a review screen renders. An unjoined error still
// produces one finding, so a caller never receives an empty list with a 422.
func validationFindings(err error) []string {
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return []string{err.Error()}
	}
	out := make([]string, 0, 4)
	for _, part := range joined.Unwrap() {
		if part != nil {
			out = append(out, part.Error())
		}
	}
	if len(out) == 0 {
		out = append(out, err.Error())
	}
	return out
}
