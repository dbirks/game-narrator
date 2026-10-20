package api

// /api/v1/profiles/{id}/import, /api/v1/profiles/{id}/export and
// /api/v1/editions/{id}/profiles/import-manifest (docs/spec/08 "staged
// source/profiles" + "manifest export with rights filters"; ADR-0017).
//
// Why an upload answers 202 with a job id rather than a parse report. Importing
// is the one content operation whose duration is not bounded by the request:
// profileio.ImportFile streams a blob through an adapter and writes it in
// batches, and docs/spec/16's benchmark is 10 000 messages on a 4 GB appliance.
// Answering 200 would mean either holding the request open until the work
// finished (a browser timeout, and a client that cannot safely retry) or
// answering early with a summary the work has not earned. So the HTTP layer does
// the two things that must be synchronous — land the bytes, hash them, make them
// durable — and hands back the resource that reports the rest
// (`/api/v1/jobs/{id}`, whose state and result are also on the event stream as
// `job.*` frames). The upload's content hash travels in the job
// payload, so a worker that resumes after a crash re-reads the same bytes the API
// verified (docs/spec/02 "Import resume by content hash").
//
// Why the bytes are stored here and not by the job: an upload that was never
// persisted is an upload a client can re-send forever. Committing first means
// provenance exists whether or not a parser ever runs, and the 413 is answered
// from a byte count rather than discovered as a full disk.
//
// Export's default is the rights-filtered `.gnprofile.json` — source pointers,
// and text withheld from any source whose rights position is unresolved
// (docs/spec/14 "copyright"). The `.gnpack` form carries the bytes themselves and
// so needs `bytes=true` *and* `acknowledge_copyright=true`; profileio refuses one
// without both, and this route never supplies the acknowledgement from a default.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/profileio"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKindImportFile is the queue name this route enqueues and cmd/game-narrator
// registers a handler for.
const JobKindImportFile = "import.file"

// ImportPayload is one queued import. It names a blob, not a file: the bytes are
// already in the asset store under SHA256 when the row is written, which is what
// makes the job safe to retry on another worker or after a restart.
type ImportPayload struct {
	ProfileID   string            `json:"profile_id"`
	SHA256      string            `json:"sha256"`
	Size        int64             `json:"size"`
	MediaType   string            `json:"media_type,omitempty"`
	FileName    string            `json:"file_name"`
	Format      string            `json:"format,omitempty"`
	Rights      string            `json:"rights,omitempty"`
	Trust       float64           `json:"trust,omitempty"`
	Language    string            `json:"language,omitempty"`
	DefaultKind string            `json:"default_kind,omitempty"`
	Columns     adapter.ColumnMap `json:"columns,omitempty"`
	Actor       string            `json:"actor,omitempty"`
	RequestID   string            `json:"request_id,omitempty"`
}

// importParams is the transport-independent half of an import request: what a
// multipart form field and a query parameter both mean.
type importParams struct {
	FileName    string
	Format      string
	Rights      string
	Trust       float64
	Language    string
	DefaultKind string
	Columns     adapter.ColumnMap
}

// importUploadResponse is the 202 body. The job resource answers progress and
// result; this answers "what did you just do with my file".
type importUploadResponse struct {
	JobID     string `json:"job_id"`
	JobKind   string `json:"job_kind"`
	ProfileID string `json:"profile_id"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}

// importRequired answers 503 when an import could not possibly run: no asset
// store means nowhere to put the bytes, no registry means nothing to read them
// with, and no queue means the parse would have to happen on the request path
// (docs/spec/13: never report a subsystem as working that was not wired).
func (s *Server) importRequired(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case s.store == nil:
		s.unavailable(w, r, "catalog-unavailable", "no catalogue database is configured in this process")
	case s.assets == nil:
		s.unavailable(w, r, "asset-store-unavailable",
			"no asset store is configured, so an upload has nowhere durable to go")
	case s.adapters == nil || s.adapters.Len() == 0:
		s.unavailable(w, r, "no-adapters",
			"no source adapters are linked into this build, so nothing could read an uploaded script")
	case s.jobQueue == nil:
		s.unavailable(w, r, "jobs-unavailable",
			"no durable job queue is configured, so an import would have to run on the request path")
	default:
		return true
	}
	return false
}

func (s *Server) unavailable(w http.ResponseWriter, r *http.Request, slug, detail string) {
	s.writeCodeProblem(w, r, http.StatusServiceUnavailable, slug, "SUBSYSTEM_UNAVAILABLE",
		"Service Unavailable", detail)
}

// handleImportFile serves POST /api/v1/profiles/{id}/import.
//
// Two request shapes are accepted because the two clients that matter differ: a
// browser sends multipart/form-data (a `file` part plus the mapping fields), and
// a script pipes the bytes with a Content-Type and puts the mapping in the query.
// Both land in the same job payload.
func (s *Server) handleImportFile(w http.ResponseWriter, r *http.Request) {
	if !s.importRequired(w, r) {
		return
	}
	p, _, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	if !s.rejectIfProfileDeleted(w, r, p.DeletedAt) {
		return
	}
	if p.IsImmutable() {
		s.writeCodeProblem(w, r, http.StatusConflict, "immutable-revision", codeImmutable, "Conflict",
			fmt.Sprintf("revision %d is %s and frozen: content only ever enters a draft, so derive one with POST /api/v1/profiles/%s/revisions",
				p.RevisionNum, p.Status, p.ID))
		return
	}

	base, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var (
		params importParams
		stored assets.Info
		err    error
	)
	switch {
	case base == "multipart/form-data":
		params, stored, err = s.readMultipartImport(r)
	case base == "":
		s.writeCodeProblem(w, r, http.StatusUnsupportedMediaType, "missing-content-type", codeInvalidParameter,
			"Unsupported Media Type",
			"send multipart/form-data with a file part, or the raw bytes with a Content-Type and the mapping in the query string")
		return
	default:
		params, stored, err = s.readRawImport(r)
	}
	if err != nil {
		s.importError(w, r, err)
		return
	}
	if params, err = s.validateImportParams(params); err != nil {
		s.importError(w, r, err)
		return
	}

	payload := ImportPayload{
		ProfileID:   p.ID,
		SHA256:      stored.SHA256,
		Size:        stored.Size,
		MediaType:   stored.MediaType,
		FileName:    params.FileName,
		Format:      params.Format,
		Rights:      params.Rights,
		Trust:       params.Trust,
		Language:    params.Language,
		DefaultKind: params.DefaultKind,
		Columns:     params.Columns,
		RequestID:   RequestIDFromContext(r.Context()),
	}
	if who := IdentityFromContext(r.Context()); who != nil {
		payload.Actor = who.Username()
	}
	raw, merr := json.Marshal(payload)
	if merr != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", "INTERNAL",
			"Internal Server Error", "the import request could not be queued")
		return
	}
	// An upload's Idempotency-Key is honoured by the queue rather than by this
	// process's memory. The durable `jobs.idempotency_key` column is unique, and a
	// retried POST after a dropped 202 is exactly the case whose response cannot be
	// reconstructed locally — an import may still be running minutes later, so the
	// answer is the job's identity rather than a remembered body (ADR-0017). The
	// key is scoped to principal + revision, so two curators who both choose "1"
	// cannot read each other's job.
	idemKey, ok := s.importIdempotencyKey(w, r, JobKindImportFile, p.ID)
	if !ok {
		return
	}
	job, created, qerr := s.jobQueue.Enqueue(r.Context(), store.EnqueueParams{
		Kind:           JobKindImportFile,
		Payload:        raw,
		Priority:       20,
		MaxAttempts:    3,
		TraceID:        RequestIDFromContext(r.Context()),
		IdempotencyKey: idemKey,
	})
	if qerr != nil {
		// The bytes are durable and the job is not: saying 202 anyway would let
		// the UI believe an import is coming that never arrives.
		s.log.Warn("import enqueue failed", "request_id", RequestIDFromContext(r.Context()),
			"sha256", stored.SHA256, "error", qerr)
		s.unavailable(w, r, "jobs-unavailable",
			"the upload was stored but could not be queued; retry the import")
		return
	}
	if !created && !sameQueuedImport(job.Payload, payload) {
		// One key, two different files: the same refusal the in-memory store makes
		// for a keyed create. Honouring it would answer 202 about an upload the
		// caller never sent and silently discard the bytes in this request.
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "idempotency-key-reused",
			codeIdempotencyConflict, "Unprocessable Entity",
			"this Idempotency-Key already queued a different upload for this revision; a key identifies one request, not one slot")
		return
	}
	s.auditCatalogWrite(r, "catalog.import.enqueue", map[string]any{
		"job_id": job.ID, "profile_id": p.ID, "sha256": stored.SHA256, "bytes": stored.Size,
	})
	resp := importUploadResponse{
		JobID:     job.ID,
		JobKind:   job.Kind,
		ProfileID: p.ID,
		SHA256:    stored.SHA256,
		Size:      stored.Size,
		MediaType: stored.MediaType,
		State:     job.State,
		Detail: fmt.Sprintf("the bytes are in the asset store under sha256:%s; follow /api/v1/jobs/%s for progress and the parse report",
			stored.SHA256, job.ID),
	}
	if !created {
		resp.Detail = "these bytes are already queued for this revision (the queue de-duplicates on Idempotency-Key); " + resp.Detail
	}
	w.Header().Set("Location", "/api/v1/jobs/"+job.ID)
	writeJSON(w, http.StatusAccepted, resp)
}

// ── reading the two request shapes ─────────────────────────────────────────

// stageAndCommit streams a reader into the asset store and returns the committed
// blob. The staging copy is aborted on every failure path, including the size
// check, so a retried upload does not accumulate staging detritus (docs/spec/02
// "atomic temp+fsync+rename", docs/spec/14 "memory/time limits").
func (s *Server) stageAndCommit(ctx context.Context, body io.Reader) (assets.Info, error) {
	id, info, err := s.assets.PutStaged(ctx, io.LimitReader(body, maxUploadBytes+1), "")
	if err != nil {
		return assets.Info{}, err
	}
	if info.Size > maxUploadBytes {
		_ = s.assets.Abort(context.WithoutCancel(ctx), id)
		return assets.Info{}, errTooLarge{}
	}
	committed, err := s.assets.Commit(ctx, id)
	if err != nil {
		_ = s.assets.Abort(context.WithoutCancel(ctx), id)
		return assets.Info{}, err
	}
	if err := s.assets.Verify(ctx, committed.SHA256); err != nil {
		return assets.Info{}, err
	}
	return committed, nil
}

// errTooLarge is the size refusal, mapped to 413 by importError.
type errTooLarge struct{}

func (errTooLarge) Error() string { return "upload exceeds the size limit" }

// readRawImport streams a non-multipart body into the asset store. The body is
// never buffered whole: a 200 MB script costs the copy buffer, not the file.
func (s *Server) readRawImport(r *http.Request) (importParams, assets.Info, error) {
	q := r.URL.Query()
	params := importParams{
		FileName:    firstNonEmpty(q.Get("file_name"), q.Get("filename"), "upload"),
		Format:      strings.TrimSpace(q.Get("format")),
		Rights:      strings.TrimSpace(q.Get("rights")),
		Language:    strings.TrimSpace(q.Get("language")),
		DefaultKind: strings.TrimSpace(q.Get("default_kind")),
		Columns:     columnMapFromValues(q.Get),
	}
	if raw := q.Get("trust"); raw != "" {
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || v > 1 {
			return params, assets.Info{}, errParam{field: "trust", detail: "trust must be a number between 0 and 1"}
		}
		params.Trust = v
	}
	// The caller's Content-Type is not recorded: the asset store sniffs the media
	// type from the bytes, so a header cannot mislabel what provenance will say the
	// file is (ADR-0014).
	info, err := s.stageAndCommit(r.Context(), r.Body)
	return params, info, err
}

// readMultipartImport walks the form parts once: the file is streamed into the
// asset store as it arrives, and every other field is read under its own byte
// bound, so a "format" field cannot be a megabyte of padding.
func (s *Server) readMultipartImport(r *http.Request) (importParams, assets.Info, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return importParams{}, assets.Info{}, errParam{field: "body", detail: "the body is not readable as multipart/form-data: " + err.Error()}
	}
	params := importParams{}
	fields := map[string]string{}
	var (
		info  assets.Info
		found bool
	)
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return params, assets.Info{}, errParam{field: "body", detail: "the multipart body is malformed: " + perr.Error()}
		}
		name := part.FormName()
		if name == "" {
			part.Close()
			continue
		}
		if part.FileName() == "" {
			buf, rerr := readCapped(part, maxFormValueBytes)
			part.Close()
			if rerr != nil {
				return params, assets.Info{}, errParam{field: name, detail: fmt.Sprintf(
					"a form field is limited to %d bytes", maxFormValueBytes)}
			}
			fields[name] = string(buf)
			continue
		}
		if found {
			part.Close()
			return params, assets.Info{}, errParam{field: "file",
				detail: "one file per import request: a source row names exactly one set of bytes"}
		}
		found = true
		params.FileName = safeBaseName(part.FileName())
		info, err = s.stageAndCommit(r.Context(), part)
		part.Close()
		if err != nil {
			return params, assets.Info{}, err
		}
	}
	if !found {
		return params, assets.Info{}, errParam{field: "file", code: codeJobRequiresInput,
			detail: `the import needs a file part named "file": a job with no bytes to read can only fail`}
	}
	get := func(k string) string { return fields[k] }
	params.Format = strings.TrimSpace(get("format"))
	params.Rights = strings.TrimSpace(get("rights"))
	params.Language = strings.TrimSpace(get("language"))
	params.DefaultKind = strings.TrimSpace(get("default_kind"))
	params.Columns = columnMapFromValues(get)
	if raw := get("trust"); raw != "" {
		v, cerr := strconv.ParseFloat(raw, 64)
		if cerr != nil || v < 0 || v > 1 {
			return params, assets.Info{}, errParam{field: "trust", detail: "trust must be a number between 0 and 1"}
		}
		params.Trust = v
	}
	return params, info, nil
}

func readCapped(r io.Reader, cap int64) ([]byte, error) {
	buf, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, err
	}
	if int64(len(buf)) > cap {
		return nil, errors.New("field exceeds its cap")
	}
	return buf, nil
}

// safeBaseName reduces an uploaded filename to what provenance can use. It is
// never opened (profileio derives a file:// label from it and the bytes live under
// their hash), but a traversal-shaped name in an audit row is still a name a
// human will misread (docs/spec/14 "Filesystem/import").
func safeBaseName(name string) string {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(name), "\\", "/"))
	if base == "." || base == "/" || base == "" {
		return "upload"
	}
	return base
}

func columnMapFromValues(get func(string) string) adapter.ColumnMap {
	return adapter.ColumnMap{
		Text:    strings.TrimSpace(get("column_text")),
		ID:      strings.TrimSpace(get("column_id")),
		Speaker: strings.TrimSpace(get("column_speaker")),
		Kind:    strings.TrimSpace(get("column_kind")),
		Scene:   strings.TrimSpace(get("column_scene")),
		Page:    strings.TrimSpace(get("column_page")),
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// validateImportParams applies the rules that need the registry or the domain
// vocabulary — the two things a streaming reader cannot have decided mid-part.
func (s *Server) validateImportParams(in importParams) (importParams, error) {
	out := in
	if out.Format != "" && out.Format != adapter.FormatAuto {
		supported := false
		for _, m := range s.adapters.Adapters() {
			if m.SupportsFormat(out.Format) {
				supported = true
				break
			}
		}
		if !supported {
			return out, errParam{field: "format", detail: fmt.Sprintf(
				"no adapter linked into this build reads format %q; known formats: %s",
				out.Format, strings.Join(s.supportedFormats(), ", "))}
		}
	}
	if out.DefaultKind != "" {
		if _, err := games.ParseTextKind(out.DefaultKind); err != nil {
			return out, errParam{field: "default_kind", detail: err.Error()}
		}
	}
	if len(out.Language) > 32 {
		return out, errParam{field: "language", detail: "a BCP-47 tag is at most 32 characters"}
	}
	out.Rights = strings.ToLower(strings.TrimSpace(out.Rights))
	switch {
	case out.Rights == "":
		// docs/spec/01 invariant 6: a file the user handed over is
		// user_supplied. The default is the truthful answer, not a guess.
		out.Rights = string(games.RightsUserSupplied)
	case out.Rights == string(games.RightsUnknown):
		return out, errParam{field: "rights", code: codeJobRequiresInput, detail: "rights=unknown is refused: a file you were given is user_supplied and licensed material is licensed — record the answer rather than storing the question, which would only resurface as a publish refusal"}
	default:
		parsed, err := games.ParseRightsStatus(out.Rights)
		if err != nil {
			return out, errParam{field: "rights", detail: err.Error()}
		}
		out.Rights = string(parsed)
	}
	if out.Trust == 0 {
		out.Trust = 1
	}
	return out, nil
}

func (s *Server) supportedFormats() []string {
	var out []string
	for _, m := range s.adapters.Adapters() {
		out = append(out, m.SupportedFormats...)
	}
	if len(out) == 0 {
		return []string{"(none)"}
	}
	return out
}

// importError maps an import failure onto the docs/spec/08 contract. A named
// parameter problem is 422 carrying the field: the onboarding screen has to show
// *which* mapping to fix (docs/spec/03).
func (s *Server) importError(w http.ResponseWriter, r *http.Request, err error) {
	var ep errParam
	if errors.As(err, &ep) {
		code := ep.code
		if code == "" {
			code = codeValidationFailed
		}
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-import", code,
			"Unprocessable Entity", ep.field+" is not usable", []string{ep.detail})
		return
	}
	var tooLarge errTooLarge
	switch {
	case errors.As(err, &tooLarge):
		s.writeCodeProblem(w, r, http.StatusRequestEntityTooLarge, "upload-too-large", codeTooLarge,
			"Request Entity Too Large",
			fmt.Sprintf("an import is limited to %d bytes", maxUploadBytes))
	case errors.Is(err, assets.ErrTooLarge):
		s.writeCodeProblem(w, r, http.StatusRequestEntityTooLarge, "upload-too-large", codeTooLarge,
			"Request Entity Too Large",
			fmt.Sprintf("an import is limited to %d bytes", maxUploadBytes))
	case errors.Is(err, assets.ErrUnsafePath), errors.Is(err, assets.ErrInvalidHash):
		s.writeCodeProblem(w, r, http.StatusBadRequest, "unusable-upload", codeInvalidParameter,
			"Bad Request", "the asset store refused the upload")
	default:
		s.log.Warn("import upload failed", "request_id", RequestIDFromContext(r.Context()), "error", err)
		s.unavailable(w, r, "asset-store-unavailable", "the upload could not be stored")
	}
}

// errParam is a named-parameter import failure.
type errParam struct {
	field  string
	code   string
	detail string
}

func (e errParam) Error() string { return e.field + ": " + e.detail }

// importIdempotencyKey turns the caller's Idempotency-Key into the queue's
// durable one: kind + principal + revision + key. The prefix is what keeps two
// unrelated routes that happen to receive the key "1" from answering each
// other's request, and the principal is what stops one curator's retry being
// attributed to another's job (ADR-0017). The kind is a parameter because the
// disc import reuses the same discipline with its own kind (ADR-0066).
func (s *Server) importIdempotencyKey(w http.ResponseWriter, r *http.Request, kind, profileID string) (string, bool) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if key == "" {
		return "", true
	}
	if len(key) > maxIdempotencyKeyLen {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "idempotency-key-invalid", codeInvalidParameter,
			"Bad Request",
			"Idempotency-Key must be at most "+strconv.Itoa(maxIdempotencyKeyLen)+" characters")
		return "", false
	}
	user := "anonymous"
	if id := IdentityFromContext(r.Context()); id != nil && id.User != nil {
		user = id.User.ID
	}
	return kind + "\x00" + user + "\x00" + profileID + "\x00" + key, true
}

// sameQueuedImport decides whether the job an idempotent retry landed on is the
// same upload: the content address is the identity of the bytes, and the profile
// is the identity of the target, so those two being equal means the caller is
// looking at their own earlier request rather than somebody else's.
func sameQueuedImport(stored json.RawMessage, want ImportPayload) bool {
	if len(stored) == 0 {
		return false
	}
	var queued ImportPayload
	if err := json.Unmarshal(stored, &queued); err != nil {
		return false
	}
	return queued.SHA256 == want.SHA256 && queued.ProfileID == want.ProfileID
}

// ── the import.file job handler ────────────────────────────────────────────

// ImportFileJobHandler is the jobs.Handler for JobKindImportFile. It lives in
// this package because the payload it decodes is the payload this package writes:
// keeping the two halves in one file is what stops a field rename on one side
// becoming a job that fails at parse time three seconds later.
//
// The bytes are re-read from the asset store and verified against their own
// address before a parser sees them (docs/spec/02 "verify on read"): the job is
// durable and may run on a worker that started after the request that created it,
// so the hash — not the fact that somebody once uploaded something — is what
// proves what is being parsed. A failure the same retry would reproduce is marked
// permanent, which is what stops a bad file burning its whole retry budget
// (docs/spec/12 "dropped endpoint vs malformed response").
func ImportFileJobHandler(st *store.Store, as assets.Store, reg *adapter.Registry) jobs.Handler {
	return func(ctx context.Context, job jobs.Job, progress jobs.ProgressFn) (json.RawMessage, error) {
		var p ImportPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return nil, jobs.Permanent(fmt.Errorf("import.file payload is unreadable: %w", err))
		}
		if !games.ValidID(p.ProfileID) || !assets.ValidHash(p.SHA256) {
			return nil, jobs.Permanent(fmt.Errorf(
				"import.file payload names profile %q and sha256 %q, neither of which is usable",
				p.ProfileID, truncHash(p.SHA256)))
		}
		profile, _, err := st.GetProfile(ctx, p.ProfileID)
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("import.file profile %s: %w", p.ProfileID, err))
		}
		if profile.IsImmutable() {
			return nil, jobs.Permanent(fmt.Errorf(
				"import.file: revision %d is %s and content may only enter a draft; derive a revision and import into that: %w",
				profile.RevisionNum, profile.Status, profileio.ErrNotDraft))
		}
		progress(2, "reading the stored upload")
		blob, _, err := as.Get(ctx, p.SHA256)
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("import.file: the queued bytes (sha256 %s) are not in the asset store: %w",
				truncHash(p.SHA256), err))
		}
		defer blob.Close()

		report, ierr := profileio.ImportFile(ctx, profileio.Deps{Store: st, Assets: as, Adapters: reg},
			profileio.ImportRequest{
				ProfileID: p.ProfileID,
				FileName:  p.FileName,
				Reader:    blob,
				Format:    p.Format,
				Rights:    games.RightsStatus(p.Rights),
				Trust:     p.Trust,
				Parse: adapter.ParseOptions{
					Format:      p.Format,
					ColumnMap:   p.Columns,
					DefaultKind: games.TextKind(p.DefaultKind),
					Language:    p.Language,
				},
			})
		if ierr != nil {
			if permanentImportError(ierr) {
				return nil, jobs.Permanent(ierr)
			}
			return nil, ierr
		}
		progress(100, fmt.Sprintf("imported %d messages from source %s", report.Stats.Imported, report.SourceID))
		raw, merr := json.Marshal(report)
		if merr != nil {
			return nil, fmt.Errorf("import.file: the parse report could not be encoded: %w", merr)
		}
		return raw, nil
	}
}

// permanentImportError decides retryability. A dropped endpoint is retried and a
// malformed file is refused once; the two are told apart by which sentinel the
// failure wrapped.
//
// The JSON decoder cases are here because the generic adapter wraps its tokeniser
// errors rather than raising a sentinel of its own, and a decode failure against
// *these* bytes can never be transient: the blob is content-addressed, was
// verified against that address before the parser saw it, and cannot have changed
// under a retry. Re-running it would reproduce the identical failure, which is the
// definition this function is supposed to implement.
func permanentImportError(err error) bool {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	return errors.Is(err, profileio.ErrInvalid) ||
		errors.Is(err, profileio.ErrNoAdapter) ||
		errors.Is(err, profileio.ErrNotDraft) ||
		errors.Is(err, adapter.ErrNoMessages) ||
		errors.Is(err, adapter.ErrInvalidOptions) ||
		errors.Is(err, adapter.ErrFormatNotSupported) ||
		errors.Is(err, games.ErrValidation) ||
		errors.Is(err, store.ErrImmutable) ||
		errors.Is(err, store.ErrNotFound) ||
		errors.As(err, &syntax) ||
		errors.As(err, &typeErr)
}

func truncHash(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	if s == "" {
		return "(empty)"
	}
	return s
}

// ── export ─────────────────────────────────────────────────────────────────

// handleProfileExport serves GET /api/v1/profiles/{id}/export.
func (s *Server) handleProfileExport(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	p, version, ok := s.resolveProfile(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	q := r.URL.Query()
	wantBytes := truthy(q.Get("bytes"))
	opts := profileio.ExportOptions{
		IncludeSourceBytes:   wantBytes,
		AcknowledgeCopyright: truthy(q.Get("acknowledge_copyright")),
	}
	if wantBytes {
		if !opts.AcknowledgeCopyright {
			s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "copyright-acknowledgement-required",
				codeRightsReview, "Unprocessable Entity",
				"exporting the source bytes needs an explicit acknowledgement",
				[]string{"re-send with ?bytes=true&acknowledge_copyright=true; the default export carries source metadata and withholds text whose rights are unresolved"})
			return
		}
		if s.assets == nil {
			s.unavailable(w, r, "asset-store-unavailable", "a .gnpack needs the asset store to read blobs from")
			return
		}
	}

	// The manifest is rendered into memory before the first byte is written: a
	// failure after Content-Disposition and 200 could only be reported as a
	// truncated download, which is the worst way to discover a refusal
	// (docs/spec/08's error contract assumes a problem document is reachable).
	if !wantBytes {
		m, err := profileio.Export(r.Context(), s.store, p.ID, opts)
		if err != nil {
			s.storeProblem(w, r, err, "export profile", "profile")
			return
		}
		encoded, eerr := encodeManifest(m)
		if eerr != nil {
			s.log.Warn("export encode failed", "request_id", RequestIDFromContext(r.Context()), "error", eerr)
			s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", "INTERNAL",
				"Internal Server Error", "the exported profile could not be encoded")
			return
		}
		setETag(w, version)
		w.Header().Set("Content-Type", ContentTypeJSON)
		w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename(p.ID, false)+`"`)
		w.Header().Set("X-Profile-Sha256", m.Digest)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(encoded)
		s.auditCatalogWrite(r, "catalog.profile.export", map[string]any{
			"profile_id": p.ID, "bytes": false, "size": len(encoded),
		})
		return
	}

	var buf bytes.Buffer
	m, err := profileio.ExportBundle(r.Context(),
		profileio.Deps{Store: s.store, Assets: s.assets, Adapters: s.adapters}, p.ID, opts, &buf)
	if err != nil {
		s.storeProblem(w, r, err, "export bundle", "profile")
		return
	}
	setETag(w, version)
	written := buf.Len()
	w.Header().Set("Content-Type", "application/x-tar")
	w.Header().Set("Content-Disposition", `attachment; filename="`+exportFilename(p.ID, true)+`"`)
	w.Header().Set("X-Profile-Sha256", m.Digest)
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = buf.WriteTo(w)
	s.auditCatalogWrite(r, "catalog.profile.export", map[string]any{
		"profile_id": p.ID, "bytes": true, "size": written,
	})
}

// encodeManifest renders a manifest exactly as profileio.WriteManifest writes it,
// so the response body is byte-identical to the file a CLI export produces: one
// canonical form, two transports.
func encodeManifest(m profileio.Manifest) ([]byte, error) {
	var buf bytes.Buffer
	if err := profileio.WriteManifest(&buf, m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// handleImportManifest serves POST /api/v1/editions/{id}/profiles/import-manifest:
// a `.gnprofile.json` in, a brand new draft out, never an overwrite (ADR-0015).
// The response names the sources whose bytes are not on this appliance, which is
// the difference between "imported" and "imported and re-parseable" (docs/spec/02
// "Profile portability").
func (s *Server) handleImportManifest(w http.ResponseWriter, r *http.Request) {
	if !s.catalogRequired(w, r) {
		return
	}
	edition, ok := s.resolveEdition(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	deps := profileio.Deps{Store: s.store, Assets: s.assets, Adapters: s.adapters}
	body := io.LimitReader(r.Body, maxUploadBytes+1)
	report, err := profileio.ImportManifest(r.Context(), deps, body, profileio.ImportManifestOptions{
		TargetEditionID: edition.ID,
		Force:           truthy(r.URL.Query().Get("force")),
	})
	if err != nil {
		s.importManifestError(w, r, err)
		return
	}
	profile, version, gerr := s.store.GetProfile(r.Context(), report.ProfileID)
	if gerr != nil {
		s.storeProblem(w, r, gerr, "get imported profile", "profile")
		return
	}
	needing := orEmptyList(report.MissingBlobs)
	present := orEmptyList(report.PresentBlobs)
	s.auditCatalogWrite(r, "catalog.profile.import-manifest", map[string]any{
		"profile_id": report.ProfileID, "edition_id": edition.ID, "digest": report.Digest,
	})
	setETag(w, version)
	w.Header().Set("Location", "/api/v1/profiles/"+report.ProfileID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"profile":               toProfileDTO(profile),
		"sources_needing_bytes": needing,
		"report": map[string]any{
			"profile_id":         report.ProfileID,
			"edition_id":         report.EditionID,
			"game_id":            report.GameID,
			"created_catalog":    report.CreatedCatalog,
			"messages_written":   report.MessagesWritten,
			"messages_withheld":  report.MessagesWithheld,
			"sources_registered": report.SourcesRegistered,
			"missing_blobs":      needing,
			"present_blobs":      present,
			"digest":             report.Digest,
			"preferred_voice_id": report.PreferredVoiceID,
			// The voice section's own report (ADR-0101): what the import wrote,
			// what it skipped because the store already held the id, what it
			// withheld because the reference bytes are not on this appliance
			// (re-import the pack to carry them), and what it could not place
			// because the (character, revision) slot is taken.
			"voices_written":     report.VoicesWritten,
			"voices_skipped":     report.VoicesSkipped,
			"voice_refs_missing": orEmptyList(report.VoiceRefsMissing),
			"voices_conflicts":   orEmptyList(report.VoicesConflicts),
		},
	})
}

// importManifestError is the refusal table for a re-import, in profileio's own
// order: malformed, then unverifiable, then incompatible. The earlier a failure
// is caught, the less it has written.
func (s *Server) importManifestError(w http.ResponseWriter, r *http.Request, err error) {
	findings := validationFindings(err)
	switch {
	case errors.Is(err, profileio.ErrDigestMismatch):
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "digest-mismatch", codeValidationFailed,
			"Unprocessable Entity",
			"the manifest does not hash to its own manifest_digest: it was edited or truncated after export", findings)
	case errors.Is(err, profileio.ErrSchemaValidation):
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "schema-invalid", codeValidationFailed,
			"Unprocessable Entity", "the manifest does not satisfy the .gnprofile.json schema", findings)
	case errors.Is(err, profileio.ErrIncompatibleEdition):
		s.writeCodeProblemFindings(w, r, http.StatusConflict, "incompatible-edition", codeUnknownEdition, "Conflict",
			"this manifest's edition does not match the target edition; re-send with ?force=true to import anyway", findings)
	case errors.Is(err, profileio.ErrDanglingRef):
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "dangling-reference", codeValidationFailed,
			"Unprocessable Entity", "the manifest references a row it does not contain", findings)
	case errors.Is(err, profileio.ErrInvalid):
		s.writeCodeProblemFindings(w, r, http.StatusUnprocessableEntity, "invalid-manifest", codeValidationFailed,
			"Unprocessable Entity", "the manifest could not be read", findings)
	default:
		s.storeProblem(w, r, err, "import manifest", "profile")
	}
}

func orEmptyList(in []string) []string {
	if in == nil {
		return []string{}
	}
	return in
}

// exportFilename builds a download name from the revision id alone. The profile
// id is a ULID, and nothing user-supplied reaches a header: a filename taken from
// an upload is how a response header ends up carrying a CRLF or a path (RFC 6266
// wants a quoted string; docs/spec/14 wants no attacker string in a header).
func exportFilename(profileID string, pack bool) string {
	var b strings.Builder
	for _, ch := range profileID {
		if (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' {
			b.WriteRune(ch)
		}
	}
	stem := b.String()
	if stem == "" {
		stem = "profile"
	}
	if pack {
		return "gn-" + stem + ".gnpack"
	}
	return "gn-" + stem + ".gnprofile.json"
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}
