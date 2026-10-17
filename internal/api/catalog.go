package api

// /api/v1/games, /api/v1/games/{id}/editions — the title-library surface of
// docs/spec/08 "APIs" (Milestone 1, ADR-0017).
//
// Three conventions the whole catalog family shares, stated once here because
// every handler in catalog.go, profiles.go, messages.go and importexport.go
// depends on them:
//
//   - Request bodies are bounded (1 MiB for JSON; only the import routes are
//     larger) and decoded with DisallowUnknownFields. A client that sends
//     `{"titel": …}` gets a 400 naming the field it got wrong, not a silently
//     empty title — the same rule internal/api/auth.go applies to credentials.
//   - Lists are cursor-paginated with a hard upper bound (docs/spec/08
//     "pagination with cursor and hard upper bounds"). A limit outside 1..200
//     is refused rather than clamped, because a clamped limit makes a client's
//     "give me everything" look successful while it silently received a page.
//   - Domain validation errors come back as problem+json carrying every problem
//     at once (games.Validate already joins them), so a review screen can show
//     all of what is wrong in one round trip (docs/spec/03).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// maxJSONBodyBytes bounds every JSON request body in this family. The largest
// legitimate payload is a speech-text edit, and messages.speech_text is capped
// at 8 000 characters by games.Message.Validate.
const maxJSONBodyBytes = 1 << 20

// maxUploadBytes bounds one import upload (docs/spec/14 "upload sizes"). The
// 64 MiB ceiling is above every realistic script (docs/spec/16's 10 000-message
// benchmark) and below anything that could fill the appliance's disk by accident.
const maxUploadBytes = 64 << 20

// maxFormValueBytes bounds one non-file multipart field. Format names, rights
// states and column names are short strings; an unbounded field would let an
// upload smuggle a megabyte of "format" into the provenance row.
const maxFormValueBytes = 1 << 10

// maxListLimit is the hard pagination ceiling this API accepts, equal to
// store.MaxListLimit, and asserted equal by
// TestPaginationIsBoundedAndCursorStable: an API ceiling that drifted from the
// repository's would let a client page past a bound the store then silently clamps.
const maxListLimit = 200

// defaultListLimit is what an omitted ?limit= means.
const defaultListLimit = 50

// Error codes of docs/spec/08 "Error contract examples" that this package
// answers. They travel in the problem document's `code` member so a client can
// branch on the contract rather than on prose.
const (
	codeNotFound              = "RESOURCE_NOT_FOUND"
	codeValidationFailed      = "VALIDATION_FAILED"
	codeImmutable             = "PROFILE_IMMUTABLE"
	codeRevisionConflict      = "PROFILE_REVISION_CONFLICT"
	codeRightsReview          = "CONTENT_RIGHTS_REVIEW_REQUIRED"
	codeUnknownEdition        = "UNKNOWN_GAME_EDITION"
	codeJobRequiresInput      = "JOB_REQUIRES_INPUT"
	codeIdempotencyKeyMissing = "IDEMPOTENCY_KEY_REQUIRED"
	codeIdempotencyConflict   = "IDEMPOTENCY_KEY_REUSED"
	codeConflict              = "RESOURCE_CONFLICT"
	codeTooLarge              = "UPLOAD_TOO_LARGE"
	codeInvalidParameter      = "INVALID_PARAMETER"
	codeTransitionInvalid     = "PROFILE_TRANSITION_INVALID"
)

// nowStamp is the single timestamp form for writes this package owns. games.
// Validate and the schema's strftime CHECKs both require RFC 3339 UTC.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// ── shared request plumbing ────────────────────────────────────────────────

// decodeJSONBody reads a bounded JSON object into dst. Unknown fields are
// refused (see the file comment); a body that is not a single JSON object is a
// 400, and an oversized one is a 413.
func (s *Server) decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any, max int64) bool {
	if max <= 0 {
		max = maxJSONBodyBytes
	}
	body := http.MaxBytesReader(w, r.Body, max)
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			s.writeCodeProblem(w, r, http.StatusBadRequest, "missing-body", codeInvalidParameter,
				"Bad Request", "a JSON request body is required")
		case errors.As(err, &maxErr):
			s.writeCodeProblem(w, r, http.StatusRequestEntityTooLarge, "body-too-large", codeTooLarge,
				"Request Entity Too Large",
				fmt.Sprintf("the request body must be at most %d bytes", max))
		default:
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-json", codeInvalidParameter,
				"Bad Request",
				fmt.Sprintf("the request body must be a JSON object with exactly the documented fields: %v", err))
		}
		return false
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "trailing-json", codeInvalidParameter,
			"Bad Request", "exactly one JSON object may be sent per request")
		return false
	}
	return true
}

// limitParam parses ?limit= against a hard ceiling. An omitted limit means
// defaultListLimit; anything else outside 1..max is a 400 naming the bound, so
// a client asking for a million rows learns the bound instead of silently
// getting 50 (docs/spec/08 "hard upper bounds").
func (s *Server) limitParam(w http.ResponseWriter, r *http.Request, max int) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return defaultListLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > max {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-limit", codeInvalidParameter,
			"Bad Request", fmt.Sprintf("limit must be an integer between 1 and %d", max))
		return 0, false
	}
	return n, true
}

// catalogRequired answers 503 when no catalogue is wired: an endpoint that
// returned an empty game list from a process with no database would tell the UI
// the library is empty (docs/spec/13 "no generic success on an unprobed
// service").
func (s *Server) catalogRequired(w http.ResponseWriter, r *http.Request) bool {
	if s.store == nil {
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "catalog-unavailable", "SUBSYSTEM_UNAVAILABLE",
			"Service Unavailable",
			"no catalogue database is configured in this process")
		return false
	}
	return true
}

// idFromPath resolves a {name} path segment to a ULID. A malformed id answers
// 404, the same as "no such row", so a scanner cannot use this route to learn
// which id shapes the service issues (the rule handleJobGet follows).
func (s *Server) idFromPath(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	id := strings.TrimSpace(r.PathValue(name))
	if !games.ValidID(id) {
		s.writeCodeProblem(w, r, http.StatusNotFound, "not-found", codeNotFound, "Not Found",
			"no resource has that identifier")
		return "", false
	}
	return id, true
}

// storeProblem maps a repository error onto the docs/spec/08 error contract.
// `what` names the resource in the 404 detail only; internal error text (which
// can name a table or a constraint) goes to the log with the correlation id.
func (s *Server) storeProblem(w http.ResponseWriter, r *http.Request, err error, op, what string) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		s.writeCodeProblem(w, r, http.StatusNotFound, "not-found", codeNotFound, "Not Found",
			fmt.Sprintf("no %s has that identifier", what))
	case errors.Is(err, store.ErrRevisionConflict):
		s.writeCodeProblem(w, r, http.StatusPreconditionFailed, "revision-conflict", codeRevisionConflict,
			"Precondition Failed",
			"the If-Match token is not the current revision token; re-read the revision and retry")
	case errors.Is(err, store.ErrImmutable):
		s.writeCodeProblem(w, r, http.StatusConflict, "immutable-revision", codeImmutable, "Conflict",
			fmt.Sprintf("this %s is published or archived and cannot be edited; derive a new revision instead",
				what))
	case errors.Is(err, store.ErrInvalidTransition):
		s.writeCodeProblem(w, r, http.StatusConflict, "invalid-transition", codeTransitionInvalid, "Conflict",
			err.Error())
	case errors.Is(err, store.ErrUnpublishable):
		// Reached only by callers that do not special-case *store.PublishError;
		// the publish handler answers it as 422 with the finding list.
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "unpublishable", codeValidationFailed,
			"Unprocessable Entity", err.Error())
	case errors.Is(err, store.ErrInvalidCursor):
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-cursor", codeInvalidParameter, "Bad Request",
			"cursor is not a pagination token issued by this service")
	case errors.Is(err, games.ErrValidation):
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
			"Unprocessable Entity", err.Error())
	case errors.Is(err, store.ErrConflict):
		s.writeCodeProblem(w, r, http.StatusConflict, "resource-conflict", codeConflict, "Conflict",
			err.Error())
	default:
		s.log.Warn("catalog operation failed",
			"request_id", RequestIDFromContext(r.Context()), "op", op, "error", err)
		s.writeCodeProblem(w, r, http.StatusServiceUnavailable, "catalog-unavailable", "SUBSYSTEM_UNAVAILABLE",
			"Service Unavailable", "the catalogue could not perform this operation")
	}
}

// writeCodeProblem emits an RFC 9457 document carrying docs/spec/08's `code`
// member. middleware.go owns the base problem shape and is not modified by this
// package, so the code and the optional finding list are added here.
func (s *Server) writeCodeProblem(w http.ResponseWriter, r *http.Request, status int, slug, code, title, detail string) {
	s.writeCodeProblemFindings(w, r, status, slug, code, title, detail, nil)
}

func (s *Server) writeCodeProblemFindings(w http.ResponseWriter, r *http.Request, status int, slug, code, title, detail string, findings []string) {
	p := codedProblem{
		Type:      "urn:game-narrator:error:" + slug,
		Title:     title,
		Status:    status,
		Detail:    detail,
		Instance:  r.URL.Path,
		RequestID: RequestIDFromContext(r.Context()),
		Code:      code,
		Findings:  findings,
	}
	if p.RequestID == "" {
		p.RequestID = newRequestID()
		w.Header().Set(HeaderRequestID, p.RequestID)
	}
	w.Header().Set("Content-Type", ContentTypeProblem)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(p); err != nil {
		s.log.Error("failed to encode problem response",
			"status", status, "request_id", p.RequestID, "error", err)
	}
}

// codedProblem is problem (middleware.go) plus the two members this milestone's
// contract adds: the stable `code`, and the `problems` finding list the publish
// gate and the importer report with (docs/spec/03 wants every problem at once).
type codedProblem struct {
	Type      string   `json:"type"`
	Title     string   `json:"title"`
	Status    int      `json:"status"`
	Detail    string   `json:"detail"`
	Instance  string   `json:"instance"`
	RequestID string   `json:"request_id"`
	Code      string   `json:"code"`
	Findings  []string `json:"problems,omitempty"`
}

// publishFindings splits a store.PublishError into (code, findings). The rights
// finding has its own docs/spec/08 code because it is the one refusal a curator
// cannot fix by editing text.
func publishFindings(pe *store.PublishError) (string, []string) {
	code := codeValidationFailed
	for _, p := range pe.Problems {
		if strings.Contains(p, "rights_status") {
			code = codeRightsReview
			break
		}
	}
	findings := pe.Problems
	if findings == nil {
		findings = []string{}
	}
	return code, findings
}

// ── DTOs ───────────────────────────────────────────────────────────────────

type gameDTO struct {
	ID          string          `json:"id"`
	Slug        string          `json:"slug"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Platform    string          `json:"platform"`
	UserTags    json.RawMessage `json:"user_tags"`
	CreatedAt   string          `json:"created_at"`
}

func toGameDTO(g games.Game) gameDTO {
	return gameDTO{
		ID:          g.ID,
		Slug:        g.Slug,
		Title:       g.Title,
		Description: g.Description,
		Platform:    g.Platform,
		UserTags:    defaultJSONArray(g.UserTags),
		CreatedAt:   g.CreatedAt,
	}
}

type editionDTO struct {
	ID                 string   `json:"id"`
	GameID             string   `json:"game_id"`
	Region             string   `json:"region"`
	Language           string   `json:"language"`
	Revision           string   `json:"revision"`
	SourceIdentity     string   `json:"source_identity"`
	DisplayAspect      *float64 `json:"display_aspect"`
	Locale             string   `json:"locale"`
	CompatibilityNotes string   `json:"compatibility_notes"`
}

func toEditionDTO(e games.Edition) editionDTO {
	return editionDTO{
		ID:                 e.ID,
		GameID:             e.GameID,
		Region:             e.Region,
		Language:           e.Language,
		Revision:           e.Revision,
		SourceIdentity:     e.SourceIdentity,
		DisplayAspect:      e.DisplayAspect,
		Locale:             e.Locale,
		CompatibilityNotes: e.CompatibilityNotes,
	}
}

// writeList is the shape every paginated list shares: the collection under a
// named key plus the cursor that continues it (absent when the walk is done).
func writeList[T any](w http.ResponseWriter, key string, items []T, next string) {
	if items == nil {
		items = []T{}
	}
	out := map[string]any{key: items}
	if next != "" {
		out["next_cursor"] = next
	}
	writeJSON(w, http.StatusOK, out)
}

// defaultJSONArray keeps a JSON column that arrived empty from a database row
// rendering as [] rather than null: the schema defaults these columns to "[]"
// and a generated client typed as string[] must never receive null.
func defaultJSONArray(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("[]")
	}
	return raw
}

func defaultJSONObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

// ── /api/v1/games ──────────────────────────────────────────────────────────

// handleGamesList serves GET /api/v1/games?cursor=&limit=.
func (s *Server) handleGamesList(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	limit, ok := s.limitParam(w, r, maxListLimit)
	if !ok {
		return
	}
	if r.URL.Query().Get("q") != "" {
		// Library-wide text search is not implemented; answering an empty list
		// would read as "no game matches" rather than "this build does not do
		// that" (docs/spec/13).
		s.writeCodeProblem(w, r, http.StatusNotImplemented, "search-not-supported", "NOT_SUPPORTED",
			"Not Implemented", "games are not full-text searchable; message search is per revision")
		return
	}
	got, next, err := s.store.ListGames(r.Context(), r.URL.Query().Get("cursor"), limit)
	if err != nil {
		s.storeProblem(w, r, err, "list games", "game")
		return
	}
	out := make([]gameDTO, 0, len(got))
	for _, g := range got {
		out = append(out, toGameDTO(g))
	}
	writeList(w, "games", out, next)
}

// createGameRequest is the POST body. Every field games.Game carries except the
// server-owned ones (id, created_at).
type createGameRequest struct {
	Slug        string          `json:"slug"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Platform    string          `json:"platform"`
	UserTags    json.RawMessage `json:"user_tags"`
}

// handleGameCreate serves POST /api/v1/games, which requires an
// Idempotency-Key (docs/spec/08 "Conventions"). A retried POST with the same
// key and the same body replays the stored 201; the same key with a different
// body is a 422, because two different bodies under one key is a client bug
// that silently creating a second title would hide.
func (s *Server) handleGameCreate(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	var req createGameRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	body, err := json.Marshal(req)
	if err != nil { // a struct of strings and RawMessage is always marshalable
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", "INTERNAL",
			"Internal Server Error", "the request could not be prepared for storage")
		return
	}
	s.idempotentCreate(w, r, "POST /api/v1/games", string(body), func() (int, any) {
		g := games.Game{
			ID:          games.NewID(),
			Slug:        strings.TrimSpace(req.Slug),
			Title:       strings.TrimSpace(req.Title),
			Description: req.Description,
			Platform:    strings.TrimSpace(req.Platform),
			UserTags:    defaultJSONArray(req.UserTags),
			CreatedAt:   nowStamp(),
		}
		if err := g.Validate(); err != nil {
			s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
				"Unprocessable Entity", err.Error())
			return 0, nil
		}
		if err := s.store.CreateGame(r.Context(), g); err != nil {
			s.storeProblem(w, r, err, "create game", "game")
			return 0, nil
		}
		s.auditCatalogWrite(r, "catalog.game.create", map[string]any{"game_id": g.ID, "slug": g.Slug})
		return http.StatusCreated, toGameDTO(g)
	})
}

// resolveGame loads a game addressed by id or slug (docs/spec/08's /games/:id
// path is documented for both, and the web UI puts the slug in the URL). Both
// misses answer the same 404, so the endpoint cannot be used to tell which
// lookup failed.
func (s *Server) resolveGame(w http.ResponseWriter, r *http.Request, key string) (games.Game, bool) {
	key = strings.TrimSpace(key)
	if games.ValidID(key) {
		g, err := s.store.GetGame(r.Context(), key)
		if err == nil {
			return g, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.storeProblem(w, r, err, "get game", "game")
			return g, false
		}
	}
	if key != "" {
		g, err := s.store.GetGameBySlug(r.Context(), key)
		if err == nil {
			return g, true
		}
		if !errors.Is(err, store.ErrNotFound) {
			s.storeProblem(w, r, err, "get game by slug", "game")
			return g, false
		}
	}
	s.writeCodeProblem(w, r, http.StatusNotFound, "game-not-found", codeNotFound, "Not Found",
		"no game has that id or slug")
	return games.Game{}, false
}

// handleGameGet serves GET /api/v1/games/{id}.
func (s *Server) handleGameGet(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	g, ok := s.resolveGame(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, toGameDTO(g))
}

// patchGameRequest is a partial game update: an omitted field is left alone,
// which is why the strings are pointers rather than bare values (docs/spec/08
// "PATCH" semantics; ADR-0017).
type patchGameRequest struct {
	Slug        *string         `json:"slug"`
	Title       *string         `json:"title"`
	Description *string         `json:"description"`
	Platform    *string         `json:"platform"`
	UserTags    json.RawMessage `json:"user_tags"`
}

// handleGamePatch serves PATCH /api/v1/games/{id}. Games carry no row version
// (docs/spec/08 puts ETag/If-Match on *profile revisions*, and
// internal/store/repo_catalog.go explains why a lost title edit is a nuisance
// while a lost dialogue edit is corruption), so no If-Match is required here.
func (s *Server) handleGamePatch(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	g, ok := s.resolveGame(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req patchGameRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	if req.Slug != nil {
		g.Slug = strings.TrimSpace(*req.Slug)
	}
	if req.Title != nil {
		g.Title = strings.TrimSpace(*req.Title)
	}
	if req.Description != nil {
		g.Description = *req.Description
	}
	if req.Platform != nil {
		g.Platform = strings.TrimSpace(*req.Platform)
	}
	if len(req.UserTags) > 0 {
		g.UserTags = defaultJSONArray(req.UserTags)
	}
	if err := g.Validate(); err != nil {
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
			"Unprocessable Entity", err.Error())
		return
	}
	if err := s.store.UpdateGame(r.Context(), g); err != nil {
		s.storeProblem(w, r, err, "update game", "game")
		return
	}
	s.auditCatalogWrite(r, "catalog.game.update", map[string]any{"game_id": g.ID})
	writeJSON(w, http.StatusOK, toGameDTO(g))
}

// ── /api/v1/games/{id}/editions ────────────────────────────────────────────

// handleEditionList serves GET /api/v1/games/{id}/editions. Unpaginated: a
// title has a handful of editions (region x language x disc revision), and
// internal/store.ListEditions is the whole table slice for that game.
func (s *Server) handleEditionList(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	g, ok := s.resolveGame(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	editions, err := s.store.ListEditions(r.Context(), g.ID)
	if err != nil {
		s.storeProblem(w, r, err, "list editions", "edition")
		return
	}
	out := make([]editionDTO, 0, len(editions))
	for _, e := range editions {
		out = append(out, toEditionDTO(e))
	}
	writeList(w, "editions", out, "")
}

type createEditionRequest struct {
	Region             string   `json:"region"`
	Language           string   `json:"language"`
	Revision           string   `json:"revision"`
	SourceIdentity     string   `json:"source_identity"`
	DisplayAspect      *float64 `json:"display_aspect"`
	Locale             string   `json:"locale"`
	CompatibilityNotes string   `json:"compatibility_notes"`
}

// handleEditionCreate serves POST /api/v1/games/{id}/editions. The unique
// identity index (game, region, language, revision) is what makes a re-import
// of the same disc reconcile instead of duplicating, so a collision is a 409
// naming the existing edition rather than a second row.
func (s *Server) handleEditionCreate(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	g, ok := s.resolveGame(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	var req createEditionRequest
	if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
		return
	}
	body, _ := json.Marshal(req)
	s.idempotentCreate(w, r, "POST /api/v1/games/"+g.ID+"/editions", string(body), func() (int, any) {
		e := games.Edition{
			ID:                 games.NewID(),
			GameID:             g.ID,
			Region:             strings.TrimSpace(req.Region),
			Language:           strings.TrimSpace(req.Language),
			Revision:           strings.TrimSpace(req.Revision),
			SourceIdentity:     strings.TrimSpace(req.SourceIdentity),
			DisplayAspect:      req.DisplayAspect,
			Locale:             strings.TrimSpace(req.Locale),
			CompatibilityNotes: req.CompatibilityNotes,
		}
		if err := e.Validate(); err != nil {
			s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "invalid-resource", codeValidationFailed,
				"Unprocessable Entity", err.Error())
			return 0, nil
		}
		if err := s.store.CreateEdition(r.Context(), e); err != nil {
			s.storeProblem(w, r, err, "create edition", "edition")
			return 0, nil
		}
		s.auditCatalogWrite(r, "catalog.edition.create", map[string]any{
			"game_id": g.ID, "edition_id": e.ID,
		})
		return http.StatusCreated, toEditionDTO(e)
	})
}

// resolveEdition loads one edition by id. A miss answers 404 with
// UNKNOWN_GAME_EDITION, docs/spec/08's code for a title/edition the appliance
// cannot act on.
func (s *Server) resolveEdition(w http.ResponseWriter, r *http.Request, id string) (games.Edition, bool) {
	if !games.ValidID(id) {
		s.writeCodeProblem(w, r, http.StatusNotFound, "unknown-edition", codeUnknownEdition, "Not Found",
			"no edition has that identifier")
		return games.Edition{}, false
	}
	e, err := s.store.GetEdition(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeCodeProblem(w, r, http.StatusNotFound, "unknown-edition", codeUnknownEdition, "Not Found",
				"no edition has that identifier")
			return e, false
		}
		s.storeProblem(w, r, err, "get edition", "edition")
		return e, false
	}
	return e, true
}

// auditCatalogWrite records a catalog mutation into the auth audit trail when a
// credential store is wired: docs/spec/14 wants curation decisions
// attributable, and the audit table is the one place that already is.
func (s *Server) auditCatalogWrite(r *http.Request, event string, detail map[string]any) {
	if s.authSvc == nil {
		return
	}
	id := IdentityFromContext(r.Context())
	if id == nil {
		return
	}
	s.authSvc.AuditEvent(context.WithoutCancel(r.Context()), event, id.User.ID, s.clientAddr(r), detail)
}
