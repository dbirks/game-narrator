package api

// /api/v1/editions/{id}/profiles, /api/v1/profiles/{id}… — the revision
// lifecycle of docs/spec/02 "Transaction workflows" on the wire (Milestone 1,
// ADR-0017, ADR-0015).
//
// The one design decision worth stating at the top: an ETag here is the
// profile's `row_version`, quoted and decimal, and nothing else. It is not a
// digest of the body (the body has no stable byte order across a Go map-free
// encoder, and a digest would make every unrelated column write look like a new
// token), and it is not the manifest digest (that one names *content* and only
// exists once a revision has been published). internal/store bumps row_version
// in the same statement as every mutation, so "the token you read is the token
// you write with or you are told you lost the race" is a storage fact this
// endpoint inherits rather than a rule it enforces twice.
//
// Which writes need If-Match, and which do not, is a table, not a judgement:
//
//	POST /profiles/{id}/status     required   a lifecycle move is a compare-and-swap
//	POST /profiles/{id}/publish    required   docs/spec/12 "no published edit without a valid revision token"
//	PATCH  …/messages/{mid}        required   two editors, one speech line
//	POST /profiles/{id}/revisions  not required   it creates a new row; a stale token cannot corrupt anything
//	GET    everything              n/a
//
// A required token that is absent is 428 (Precondition Required); a token that
// does not match is 412. Both carry PROFILE_REVISION_CONFLICT, because the
// client's recovery is the same: re-read, then decide.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// ── DTOs ───────────────────────────────────────────────────────────────────

type profileDTO struct {
	ID                string `json:"id"`
	EditionID         string `json:"edition_id"`
	RevisionNum       int    `json:"revision_num"`
	ParentRevision    *int   `json:"parent_revision"`
	Status            string `json:"status"`
	ManifestDigest    string `json:"manifest_digest"`
	PublishedAt       string `json:"published_at,omitempty"`
	PreferredVoiceID  string `json:"preferred_voice_id,omitempty"`
	PublicationAuthor string `json:"publication_author,omitempty"`
	// NarratorFallback is the per-profile narrator fallback (ADR-0101). Always
	// present — the flag is on by default and "absent" would mean two things
	// (never set, off) that the store cannot tell apart — so the field never
	// omits.
	NarratorFallback bool     `json:"narrator_fallback"`
	NextStates       []string `json:"next_states"`
}

func toProfileDTO(p games.Profile) profileDTO {
	next := p.Status.NextStates()
	out := profileDTO{
		ID:                p.ID,
		EditionID:         p.EditionID,
		RevisionNum:       p.RevisionNum,
		ParentRevision:    p.ParentRevision,
		Status:            string(p.Status),
		ManifestDigest:    p.ManifestDigest,
		PublishedAt:       p.PublishedAt,
		PreferredVoiceID:  p.PreferredVoiceID,
		PublicationAuthor: p.PublicationAuthor,
		NarratorFallback:  p.NarratorFallback,
		NextStates:        make([]string, 0, len(next)),
	}
	if out.NextStates == nil {
		out.NextStates = []string{}
	}
	for _, s := range next {
		out.NextStates = append(out.NextStates, string(s))
	}
	return out
}

type characterDTO struct {
	ID                 string          `json:"id"`
	ProfileID          string          `json:"profile_id"`
	Name               string          `json:"name"`
	Aliases            json.RawMessage `json:"aliases"`
	VisualDescriptors  json.RawMessage `json:"visual_descriptors"`
	PersonalitySummary string          `json:"personality_summary"`
	Role               string          `json:"role"`
	VerifiedSourceRefs json.RawMessage `json:"verified_source_refs"`
	PortraitAssetID    string          `json:"portrait_asset_id,omitempty"`
	Status             string          `json:"status"`
	CreatedAt          string          `json:"created_at"`
}

func toCharacterDTO(c games.Character) characterDTO {
	return characterDTO{
		ID:                 c.ID,
		ProfileID:          c.ProfileID,
		Name:               c.Name,
		Aliases:            defaultJSONArray(c.Aliases),
		VisualDescriptors:  defaultJSONArray(c.VisualDescriptors),
		PersonalitySummary: c.PersonalitySummary,
		Role:               c.Role,
		VerifiedSourceRefs: defaultJSONArray(c.VerifiedSourceRefs),
		PortraitAssetID:    c.PortraitAssetID,
		Status:             string(c.Status),
		CreatedAt:          c.CreatedAt,
	}
}

type regionDTO struct {
	ID           string          `json:"id"`
	ProfileID    string          `json:"profile_id"`
	SceneID      string          `json:"scene_id,omitempty"`
	Label        string          `json:"label"`
	RegionType   string          `json:"region_type"`
	DetectMethod string          `json:"detect_method"`
	RectX        float64         `json:"rect_x"`
	RectY        float64         `json:"rect_y"`
	RectW        float64         `json:"rect_w"`
	RectH        float64         `json:"rect_h"`
	MinAspect    *float64        `json:"min_aspect"`
	MaxAspect    *float64        `json:"max_aspect"`
	MinWidthPx   *int            `json:"min_width_px"`
	MinHeightPx  *int            `json:"min_height_px"`
	OCRSettings  json.RawMessage `json:"ocr_settings"`
	MaskAssetIDs json.RawMessage `json:"mask_asset_ids"`
	Reliability  *float64        `json:"reliability"`
	Revision     int             `json:"revision"`
	UpdatedAt    string          `json:"updated_at"`
}

func toRegionDTO(rg games.Region) regionDTO {
	return regionDTO{
		ID:           rg.ID,
		ProfileID:    rg.ProfileID,
		SceneID:      rg.SceneID,
		Label:        rg.Label,
		RegionType:   string(rg.RegionType),
		DetectMethod: string(rg.DetectMethod),
		RectX:        rg.RectX,
		RectY:        rg.RectY,
		RectW:        rg.RectW,
		RectH:        rg.RectH,
		MinAspect:    rg.MinAspect,
		MaxAspect:    rg.MaxAspect,
		MinWidthPx:   rg.MinWidthPx,
		MinHeightPx:  rg.MinHeightPx,
		OCRSettings:  defaultJSONObject(rg.OCRSettings),
		MaskAssetIDs: defaultJSONArray(rg.MaskAssetIDs),
		Reliability:  rg.Reliability,
		Revision:     rg.Revision,
		UpdatedAt:    rg.UpdatedAt,
	}
}

type publishActionDTO struct {
	ID                  string   `json:"id"`
	EditionID           string   `json:"edition_id"`
	ProfileID           string   `json:"profile_id"`
	SupersededProfileID string   `json:"superseded_profile_id,omitempty"`
	Action              string   `json:"action"`
	Result              string   `json:"result"`
	Actor               string   `json:"actor"`
	ManifestDigest      string   `json:"manifest_digest,omitempty"`
	ExpectedRowVersion  int64    `json:"expected_row_version,omitempty"`
	ResultRowVersion    int64    `json:"result_row_version"`
	Problems            []string `json:"problems"`
	Detail              string   `json:"detail,omitempty"`
	CreatedAt           string   `json:"created_at"`
}

func toPublishActionDTO(a store.PublishAction) publishActionDTO {
	problems := a.Problems
	if problems == nil {
		problems = []string{}
	}
	return publishActionDTO{
		ID:                  a.ID,
		EditionID:           a.EditionID,
		ProfileID:           a.ProfileID,
		SupersededProfileID: a.SupersededProfileID,
		Action:              a.Action,
		Result:              a.Result,
		Actor:               a.Actor,
		ManifestDigest:      a.ManifestDigest,
		ExpectedRowVersion:  a.ExpectedRowVersion,
		ResultRowVersion:    a.ResultRowVersion,
		Problems:            problems,
		Detail:              a.Detail,
		CreatedAt:           a.CreatedAt,
	}
}

// ── ETag / If-Match ────────────────────────────────────────────────────────

// etagFor renders the token the wire carries. Quoted, decimal, strong.
func etagFor(rowVersion int64) string {
	return strconv.Quote(strconv.FormatInt(rowVersion, 10))
}

// parseETagToken reverses etagFor. An unparseable token is reported as such
// rather than guessed at: a client that sent `W/"3"` (weak) or `*` must be told
// the precondition cannot be satisfied, not silently matched.
func parseETagToken(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "W/") {
		return 0, false // weak validators cannot satisfy If-Match (RFC 9110 8.8.3)
	}
	unquoted, err := strconv.Unquote(raw)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(unquoted, 10, 64)
	if err != nil || v < 1 {
		return 0, false
	}
	return v, true
}

// ifMatchRowVersion reads the required precondition header. It answers the
// problem itself and reports whether the handler may proceed.
//
// `*` is refused on purpose. It is a legitimate If-Match value meaning "whatever
// is there now", and on an immutable-by-default publication it is exactly the
// assumption that loses an edit: the whole point of the token is that the caller
// saw the revision it is changing.
func (s *Server) ifMatchRowVersion(w http.ResponseWriter, r *http.Request, current int64) (int64, bool) {
	raw := r.Header.Get("If-Match")
	if raw == "" {
		s.writeCodeProblem(w, r, http.StatusPreconditionRequired, "if-match-required", codeRevisionConflict,
			"Precondition Required",
			"this write requires an If-Match header carrying the revision's current ETag; GET the revision first")
		return 0, false
	}
	if strings.TrimSpace(raw) == "*" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "if-match-wildcard", codeRevisionConflict,
			"Bad Request",
			"If-Match: * is not accepted for profile writes: the token must name the revision this call was based on")
		return 0, false
	}
	// A list of validators is allowed (RFC 9110 8.8.3.1); any match satisfies.
	found := false
	var chosen int64
	for _, part := range strings.Split(raw, ",") {
		v, ok := parseETagToken(part)
		if !ok {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-etag", codeInvalidParameter,
				"Bad Request",
				fmt.Sprintf("If-Match carries %q, which is not a strong quoted-decimal ETag like %s",
					part, etagFor(current)))
			return 0, false
		}
		if v == current {
			found, chosen = true, v
		}
	}
	if !found {
		w.Header().Set("ETag", etagFor(current))
		s.writeCodeProblem(w, r, http.StatusPreconditionFailed, "revision-conflict", codeRevisionConflict,
			"Precondition Failed",
			fmt.Sprintf("If-Match does not name the current revision token (this revision is at %s); re-read and re-apply",
				etagFor(current)))
		return 0, false
	}
	return chosen, true
}

// setETag writes the token header on every successful profile-scoped response,
// so a client can cache the decision "this is the row I am editing" without a
// second round trip.
func setETag(w http.ResponseWriter, rowVersion int64) {
	w.Header().Set("ETag", etagFor(rowVersion))
}

// ── handlers ───────────────────────────────────────────────────────────────

// handleProfileListForEdition serves GET /api/v1/editions/{id}/profiles: every
// revision of one edition, lowest first.
func (s *Server) handleProfileListForEdition(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	edition, ok := s.resolveEdition(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	profiles, err := s.store.ListProfiles(r.Context(), edition.ID)
	if err != nil {
		s.storeProblem(w, r, err, "list profiles", "profile")
		return
	}
	out := make([]profileDTO, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, toProfileDTO(p))
	}
	writeList(w, "profiles", out, "")
}

// handleProfileDraftCreate serves POST /api/v1/editions/{id}/profiles: a new
// draft lineage (revision 1 of a fresh chain, or the next free number when the
// edition already has one — internal/store/repo_profiles.go owns that rule).
func (s *Server) handleProfileDraftCreate(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	edition, ok := s.resolveEdition(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	// The body is optional and must be empty-or-{}; reading and rejecting it is
	// what stops a mistyped {"revision": 4} being silently ignored — the
	// revision number is server-owned (docs/spec/02).
	if !s.rejectBody(w, r) {
		return
	}
	p, err := s.store.CreateDraftProfile(r.Context(), edition.ID)
	if err != nil {
		s.storeProblem(w, r, err, "create draft profile", "profile")
		return
	}
	_, version, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get created profile", "profile")
		return
	}
	setETag(w, version)
	s.auditCatalogWrite(r, "catalog.profile.draft", map[string]any{
		"edition_id": edition.ID, "profile_id": p.ID,
	})
	w.Header().Set("Location", "/api/v1/profiles/"+p.ID)
	writeJSON(w, http.StatusCreated, toProfileDTO(p))
}

// rejectBody accepts an empty body or `{}` and refuses anything else. A create
// that takes no input must not be a place where a caller can smuggle fields.
func (s *Server) rejectBody(w http.ResponseWriter, r *http.Request) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBodyBytes))
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-json", codeInvalidParameter,
			"Bad Request", "this endpoint takes no request body")
		return false
	}
	if strings.TrimSpace(string(raw)) != "{}" {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "unexpected-body", codeInvalidParameter,
			"Bad Request", "this endpoint takes no request body fields")
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "trailing-json", codeInvalidParameter,
			"Bad Request", "exactly one JSON document may be sent per request")
		return false
	}
	return true
}

// handleProfileGet serves GET /api/v1/profiles/{id}.
func (s *Server) handleProfileGet(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	setETag(w, version)
	writeJSON(w, http.StatusOK, toProfileDTO(p))
}

func (s *Server) resolveProfile(w http.ResponseWriter, r *http.Request, id string) (games.Profile, int64, bool) {
	if !games.ValidID(id) {
		s.writeCodeProblem(w, r, http.StatusNotFound, "profile-not-found", codeNotFound, "Not Found",
			"no profile revision has that identifier")
		return games.Profile{}, 0, false
	}
	p, version, err := s.store.GetProfile(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return games.Profile{}, 0, false
	}
	return p, version, true
}

// handlePublishedProfile serves GET /api/v1/editions/{id}/published — the one
// question a session start asks (docs/spec/13: refuse to start rather than
// improvise, which is why "nothing published" is a 404 and not a null body).
func (s *Server) handlePublishedProfile(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	edition, ok := s.resolveEdition(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	p, version, err := s.store.PublishedProfile(r.Context(), edition.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeCodeProblem(w, r, http.StatusNotFound, "not-published", codeNotFound, "Not Found",
				"this edition has no published revision; a session must refuse to start rather than choose one")
			return
		}
		s.storeProblem(w, r, err, "get published profile", "profile")
		return
	}
	setETag(w, version)
	writeJSON(w, http.StatusOK, toProfileDTO(p))
}

// handleProfileRevision serves POST /api/v1/profiles/{id}/revisions: copy this
// revision's content into a fresh draft (ADR-0015). No If-Match: the parent may
// be immutable, which is precisely the case this call exists for, and deriving
// a snapshot cannot corrupt the row it read.
func (s *Server) handleProfileRevision(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	parent, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectBody(w, r) {
		return
	}
	p, err := s.store.NewRevision(r.Context(), parent.ID)
	if err != nil {
		s.storeProblem(w, r, err, "new profile revision", "profile")
		return
	}
	_, version, err := s.store.GetProfile(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "get created profile", "profile")
		return
	}
	setETag(w, version)
	s.auditCatalogWrite(r, "catalog.profile.revision", map[string]any{
		"parent_profile_id": parent.ID, "profile_id": p.ID, "revision_num": p.RevisionNum,
	})
	w.Header().Set("Location", "/api/v1/profiles/"+p.ID)
	writeJSON(w, http.StatusCreated, toProfileDTO(p))
}

type profileStatusRequest struct {
	To string `json:"to"`
}

// handleProfileStatus serves POST /api/v1/profiles/{id}/status with If-Match.
// `to: published` is refused here on purpose — the store says so in terms
// (publishThroughPublishCall) because publication is the validated, digested,
// pointer-swapped, audited act, and a status flip cannot stand in for it.
func (s *Server) handleProfileStatus(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	_, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req profileStatusRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	to, err := games.ParseProfileStatus(strings.TrimSpace(req.To))
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-status", codeInvalidParameter,
			"Bad Request", err.Error())
		return
	}
	if to == games.StatusPublished {
		s.writeCodeProblem(w, r, http.StatusConflict, "publish-not-a-status-write", codeTransitionInvalid,
			"Conflict",
			"publication is not a status write; POST /api/v1/profiles/{id}/publish so the revision is validated, digested and audited")
		return
	}
	expected, ok := s.ifMatchRowVersion(w, r, version)
	if !ok {
		return
	}
	if err := s.store.SetProfileStatus(r.Context(), r.PathValue("id"), to, expected); err != nil {
		s.profileWriteProblem(w, r, err, "set profile status")
		return
	}
	p, fresh, err := s.store.GetProfile(r.Context(), r.PathValue("id"))
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	s.auditCatalogWrite(r, "catalog.profile.status", map[string]any{
		"profile_id": p.ID, "to": string(to),
	})
	writeJSON(w, http.StatusOK, toProfileDTO(p))
}

type narratorFallbackRequest struct {
	Enabled *bool `json:"enabled"`
}

// handleNarratorFallback serves POST /api/v1/profiles/{id}/narrator-fallback
// with If-Match (ADR-0101): the per-profile switch that decides whether a line
// whose speaker has no approved voice is spoken by the narrator voice.
//
// The token contract is the profile-write one (428 absent, 412 stale), even
// though the store write does not bump the row version: the If-Match still
// answers "was this dial set against the revision you last read", which is
// the guard a settings endpoint exists to give, and it costs the caller
// nothing the content writes do not already pay.
func (s *Server) handleNarratorFallback(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	prof, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	id := prof.ID
	var req narratorFallbackRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	// The body carries exactly one boolean, and its presence is asserted:
	// a missing enabled would decode to the zero value the caller never typed
	// — silently setting the dial off — so the 400 names the field, the same
	// missing-field answer the neighbouring profile writes give.
	if req.Enabled == nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-field", codeInvalidParameter,
			"Bad Request", "enabled is required: the dial is set to a value, not to an absence")
		return
	}
	if _, ok := s.ifMatchRowVersion(w, r, version); !ok {
		return
	}
	if err := s.store.SetProfileNarratorFallback(r.Context(), id, *req.Enabled); err != nil {
		s.profileWriteProblem(w, r, err, "set profile narrator fallback")
		return
	}
	p, fresh, err := s.store.GetProfile(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	s.auditCatalogWrite(r, "catalog.profile.narrator_fallback", map[string]any{
		"profile_id": p.ID, "enabled": p.NarratorFallback,
	})
	writeJSON(w, http.StatusOK, toProfileDTO(p))
}

// handleProfilePublish serves POST /api/v1/profiles/{id}/publish with If-Match.
// A gate refusal is 422 with every finding listed and, when the finding is the
// rights gate, docs/spec/08's CONTENT_RIGHTS_REVIEW_REQUIRED code.
func (s *Server) handleProfilePublish(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	id := r.PathValue("id")
	_, version, ok := s.resolveProfile(w, r, id)
	if !ok {
		return
	}
	if !s.rejectBody(w, r) {
		return
	}
	expected, ok := s.ifMatchRowVersion(w, r, version)
	if !ok {
		return
	}
	author := "unknown"
	if who := IdentityFromContext(r.Context()); who != nil {
		author = who.Username()
	}
	p, err := s.store.Publish(r.Context(), id, expected, author)
	if err != nil {
		var pe *store.PublishError
		if errors.As(err, &pe) {
			code, findings := publishFindings(pe)
			s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "unpublishable", code,
				"Unprocessable Entity",
				fmt.Sprintf("this revision is not publishable (%d finding(s)); fix them and publish again", len(findings)),
				findings)
			return
		}
		s.profileWriteProblem(w, r, err, "publish profile")
		return
	}
	_, fresh, err := s.store.GetProfile(r.Context(), id)
	if err != nil {
		s.storeProblem(w, r, err, "get profile", "profile")
		return
	}
	setETag(w, fresh)
	s.auditCatalogWrite(r, "catalog.profile.publish", map[string]any{
		"profile_id": p.ID, "manifest_digest": p.ManifestDigest,
	})
	writeJSON(w, http.StatusOK, toProfileDTO(p))
}

// profileWriteProblem adds the two lifecycle errors the generic mapping cannot
// name on its own: an immutable revision (derive one instead) and an illegal
// transition (which states are reachable is the useful answer).
func (s *Server) profileWriteProblem(w http.ResponseWriter, r *http.Request, err error, op string) {
	switch {
	case errors.Is(err, store.ErrImmutable):
		s.writeCodeProblem(w, r, http.StatusConflict, "immutable-revision", codeImmutable, "Conflict",
			"this revision is published or archived and frozen; derive a new revision with POST /api/v1/profiles/{id}/revisions")
	case errors.Is(err, store.ErrInvalidTransition):
		s.writeCodeProblem(w, r, http.StatusConflict, "invalid-transition", codeTransitionInvalid, "Conflict",
			err.Error())
	default:
		s.storeProblem(w, r, err, op, "profile")
	}
}

// handlePublishActions serves GET /api/v1/profiles/{id}/publish-actions: the
// edition's audit trail, newest first. It is scoped by the profile's edition
// because publish_actions is per edition (docs/spec/02) — the question a review
// screen asks is "what has shipped for this disc, and what was refused".
func (s *Server) handlePublishActions(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	limit, ok := s.limitParam(w, r, maxListLimit)
	if !ok {
		return
	}
	actions, err := s.store.PublishActions(r.Context(), p.EditionID, limit)
	if err != nil {
		s.storeProblem(w, r, err, "list publish actions", "profile")
		return
	}
	out := make([]publishActionDTO, 0, len(actions))
	for _, a := range actions {
		out = append(out, toPublishActionDTO(a))
	}
	writeList(w, "publish_actions", out, "")
}

// handleProfileCharacters serves GET /api/v1/profiles/{id}/characters.
func (s *Server) handleProfileCharacters(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	rows, err := s.store.ListCharacters(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "list characters", "character")
		return
	}
	out := make([]characterDTO, 0, len(rows))
	for _, c := range rows {
		out = append(out, toCharacterDTO(c))
	}
	writeList(w, "characters", out, "")
}

// handleProfileRegions serves GET /api/v1/profiles/{id}/regions.
func (s *Server) handleProfileRegions(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	rows, err := s.store.ListRegions(r.Context(), p.ID)
	if err != nil {
		s.storeProblem(w, r, err, "list regions", "region")
		return
	}
	out := make([]regionDTO, 0, len(rows))
	for _, rg := range rows {
		out = append(out, toRegionDTO(rg))
	}
	// The region-set PUT is If-Match guarded, so the read it feeds must hand
	// the editor the token (client.ts caches the ETag and sends it back); the
	// other profile-scoped reads do exactly this and 428s are how a missing
	// one surfaces otherwise.
	setETag(w, version)
	writeList(w, "regions", out, "")
}
