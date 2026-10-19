package api

// The managed replay clip store (ADR-0073): POST /api/v1/replay/clips uploads
// a tar/zip of a replay layout, installs it under a managed directory inside
// the data directory that the session runtime always treats as a replay root,
// and records its provenance (the archive's content hash, committed to the
// asset store); GET lists the store; DELETE removes one clip.
//
// Why the product path exists at all (the evidence that motivated it). The
// 30-minute sustained run on the real UNO Q could only replay a clip by
// appending GAME_NARRATOR_REPLAY_ROOTS to /etc/game-narrator/env and
// restarting the service — the env file that also carries the gateway token.
// A clip is user content with a product surface for its own sake; the env
// file is for operators, and the two must not be the same step.
//
// Why the role is developer (docs/spec/08 "Developer mode can display
// sensitive gameplay text and provider traces; require explicit role"). A
// clip is a recording of gameplay frames: the same evidence class as the
// dev-trace store, the fixtures and the provider traces, which the scope
// table keeps behind the developer tier. The surface is therefore
// /api/v1/replay/* at developer, not an operator convenience and not an
// admin act — an admin always passes (the role order), which is how the
// device scripts reach it with the admin token they already mint.
//
// Why the archive is committed to the asset store before it is extracted.
// The clip's provenance is the archive's content hash (AGENTS.md rule 5:
// user content is provenance-aware), and a hash that exists in the store is
// a claim an auditor can re-verify; the extracted directory is a cache of
// those bytes for the runtime. Nothing here logs an absolute clip path —
// the index and the responses carry the id, the hash and the counts only.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/clips"
)

// clipMaxArchiveBytes bounds the upload the appliance will accept: a 10 Hz
// recording of a long session of 640x480 PNG frames is the large real case,
// and a browser upload that needs more than this is a clip to record at a
// lower frame rate, not a request the appliance serves by streaming forever.
const clipMaxArchiveBytes = 512 << 20

// clipNameMaxBytes bounds the optional display-name form field.
const clipNameMaxBytes = 256

// WithClips wires the managed clip store behind the /api/v1/replay/clips
// routes (ADR-0073). dir is the managed directory inside the data directory
// (the serve configuration passes <data dir>/replay-clips); as is the asset
// store the uploaded archives are committed to for provenance. An unwired
// half answers 503 rather than an empty store: a list that pretended "no
// clips" for an unconfigured store would hide the configuration, and an
// upload with nowhere durable to go would 202 into nothing.
func WithClips(as assets.Store, dir string) Option {
	return func(s *Server) {
		s.clipsAssets = as
		s.clipsStoreDir = strings.TrimSpace(dir)
	}
}

// clipsReady is the honest-degradation gate for the clip surface: the store's
// directory is the clip-specific configuration fact, and the asset store is
// its provenance half. A 503 that named only the asset store would point the
// operator at a subsystem that may be wired and working (the imports).
func (s *Server) clipsReady(w http.ResponseWriter, r *http.Request) bool {
	if s.clipsStoreDir == "" {
		s.unavailable(w, r, "clips-unavailable",
			"the managed clip store is not configured in this process; the serve wiring did not pass a clip directory")
		return false
	}
	if s.clipsAssets == nil {
		s.unavailable(w, r, "asset-store-unavailable",
			"no asset store is configured, so a clip upload has no durable home for its provenance")
		return false
	}
	return true
}

// clipDTO is one clip as the API answers it: the id a session selects by, the
// display name, the install-time summary and the provenance. No path of any
// kind — the directory the id resolves to is server state (docs/spec/14
// "Filesystem": the API never answers with a host path).
type clipDTO struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Frames        int       `json:"frames"`
	ArchiveSHA256 string    `json:"archive_sha256"`
	ArchiveSize   int64     `json:"archive_size"`
	ArchiveFormat string    `json:"archive_format"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by,omitempty"`
	// Missing is true when the index has the clip but its directory is gone
	// (a removed state, not an error): the session answers source-missing,
	// and the delete answers the cleanup.
	Missing bool `json:"missing,omitempty"`
}

type clipListResponse struct {
	Clips []clipDTO `json:"clips"`
}

type clipUploadResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Frames        int       `json:"frames"`
	ArchiveSHA256 string    `json:"archive_sha256"`
	ArchiveSize   int64     `json:"archive_size"`
	ArchiveFormat string    `json:"archive_format"`
	CreatedAt     time.Time `json:"created_at"`
	CreatedBy     string    `json:"created_by,omitempty"`
	Detail        string    `json:"detail"`
}

// ── GET /api/v1/replay/clips ────────────────────────────────────────────────

// handleClipsList answers GET /api/v1/replay/clips. The index is the
// provenance table and the directories are the truth for existence, so the
// list reconciles the two on the read: a directory the index lost is adopted
// (validated, with empty provenance), and an index row whose directory is
// gone is listed as missing rather than dropped — an honest list shows the
// appliance's state, including its losses.
func (s *Server) handleClipsList(w http.ResponseWriter, r *http.Request) {
	if !s.clipsReady(w, r) {
		return
	}
	idx, err := s.reconciledClipsIndex()
	if err != nil {
		s.clipIndexProblem(w, r, err)
		return
	}
	out := clipListResponse{Clips: []clipDTO{}}
	for _, rec := range idx.Records {
		dto := clipDTO{
			ID: rec.ID, Name: rec.Name, Frames: rec.Frames,
			ArchiveSHA256: rec.SHA256, ArchiveSize: rec.Size,
			ArchiveFormat: rec.Format, CreatedAt: rec.CreatedAt, CreatedBy: rec.CreatedBy,
		}
		if _, statErr := os.Stat(filepath.Join(s.clipsStoreDir, rec.ID)); errors.Is(statErr, os.ErrNotExist) {
			dto.Missing = true
			dto.Frames = 0
		}
		out.Clips = append(out.Clips, dto)
	}
	writeJSON(w, http.StatusOK, out)
}

// reconcileClipsIndex is Load-or-rebuild plus the adoption pass. A corrupt
// index is the caller's problem to report (clipIndexProblem); the adoption
// writes a reconciled index only when it changed something, so a healthy
// list never rewrites the file.
func (s *Server) reconciledClipsIndex() (clips.Index, error) {
	idx, err := clips.Load(s.clipsStoreDir)
	if err != nil {
		return clips.Index{}, err
	}
	entries, err := os.ReadDir(s.clipsStoreDir)
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return clips.Index{}, err
	}
	known := map[string]bool{}
	for _, rec := range idx.Records {
		known[rec.ID] = true
	}
	changed := false
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".staging-") {
			continue
		}
		if known[e.Name()] {
			continue
		}
		// A directory without provenance: adopt it only when it is a
		// genuinely runnable clip. A directory that fails validation is not
		// a clip at all — it lists nowhere and the operator removes it by
		// hand (the delete route is id-scoped and would refuse a non-clip).
		clip, verr := clips.ValidateDir(filepath.Join(s.clipsStoreDir, e.Name()))
		if verr != nil {
			continue
		}
		var createdAt time.Time
		if st, serr := os.Stat(filepath.Join(s.clipsStoreDir, e.Name())); serr == nil {
			createdAt = st.ModTime().UTC()
		} else {
			createdAt = time.Now().UTC()
		}
		idx.Records = append(idx.Records, clips.Record{
			ID:        e.Name(),
			Frames:    clip.Frames,
			CreatedAt: createdAt,
			CreatedBy: "recovered",
			Format:    "unknown", // no uploaded archive, so no container to name
		})
		changed = true
	}
	if changed {
		if err := clips.Save(s.clipsStoreDir, idx); err != nil {
			return clips.Index{}, err
		}
	}
	return idx, nil
}

func (s *Server) clipIndexProblem(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, clips.ErrCorruptIndex) {
		s.log.Error("clip index is corrupt",
			"request_id", RequestIDFromContext(r.Context()), "error", err)
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-index-corrupt", "CLIP_INDEX_CORRUPT",
			"Internal Server Error",
			"the clip index cannot be read; the store's directories are intact and deletions still work — repair or replace the index file and retry")
		return
	}
	s.log.Error("listing the clip store failed",
		"request_id", RequestIDFromContext(r.Context()), "error", err)
	s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-store-unreadable", "INTERNAL",
		"Internal Server Error", "the clip store could not be listed; see the server log for this request_id")
}

// ── POST /api/v1/replay/clips ───────────────────────────────────────────────

// handleClipsUpload answers POST /api/v1/replay/clips. The body is
// multipart/form-data with a file part carrying the archive (tar or zip) and
// an optional name field for the clip id. The order of the work is the
// security order: the bytes land durably under their hash first (provenance
// exists whatever the extraction then does), the extraction is confined to a
// staging directory, the layout is validated, and only then is the clip
// moved into place under its id and the index written. Every earlier failure
// removes the staging and answers 400/409/413 — a clip that did not install
// leaves no directory and no index row.
func (s *Server) handleClipsUpload(w http.ResponseWriter, r *http.Request) {
	if !s.clipsReady(w, r) {
		return
	}
	base, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if base != "multipart/form-data" {
		s.writeCodeProblem(w, r, http.StatusUnsupportedMediaType, "clip-upload-needs-multipart", codeInvalidParameter,
			"Unsupported Media Type",
			"the upload is multipart/form-data with a file part carrying the archive (tar or zip)")
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-upload-malformed", codeInvalidParameter,
			"Bad Request", "the body is not readable as multipart/form-data: "+err.Error())
		return
	}
	var (
		fileName   string
		fileFound  bool
		nameField  string
		nameTooBig bool
		committed  assets.Info
	)
	ctx := r.Context()
	// The file part must be staged while it is the current part: the multipart
	// reader discards an unread part's remainder the moment NextPart advances,
	// so reading it after the loop would stage zero bytes and install an empty
	// clip — exactly the shape that answers "neither tar nor zip".
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-upload-malformed", codeInvalidParameter,
				"Bad Request", "the multipart body is malformed: "+perr.Error())
			return
		}
		if part.FileName() == "" {
			if part.FormName() == "name" {
				raw, rerr := readCapped(part, clipNameMaxBytes)
				part.Close()
				if rerr != nil {
					nameTooBig = true
					continue
				}
				nameField = string(raw)
			} else {
				part.Close()
			}
			continue
		}
		if fileFound {
			part.Close()
			s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-upload-one-file", codeInvalidParameter,
				"Bad Request", "one archive per upload request")
			return
		}
		// 1. Land the bytes under their hash: provenance before interpretation.
		fileFound = true
		fileName = safeBaseName(part.FileName())
		id, info, serr := s.clipsAssets.PutStaged(ctx, io.LimitReader(part, clipMaxArchiveBytes+1), "")
		part.Close()
		if serr != nil {
			s.clipUploadStoreProblem(w, r, serr)
			return
		}
		if info.Size > clipMaxArchiveBytes {
			_ = s.clipsAssets.Abort(context.WithoutCancel(ctx), id)
			s.writeCodeProblem(w, r, http.StatusRequestEntityTooLarge, "clip-too-large", codeTooLarge,
				"Request Entity Too Large",
				fmt.Sprintf("a clip archive is limited to %d bytes", clipMaxArchiveBytes))
			return
		}
		committedInfo, cerr := s.clipsAssets.Commit(ctx, id)
		if cerr != nil {
			_ = s.clipsAssets.Abort(context.WithoutCancel(ctx), id)
			s.clipUploadStoreProblem(w, r, cerr)
			return
		}
		committed = committedInfo
		// No break: the loop keeps going so a second file part is the refused
		// "one archive per upload request", not silently ignored trailing bytes.
	}
	if !fileFound {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-archive-required", codeJobRequiresInput,
			"Bad Request", "the upload needs a file part carrying the archive (a tar or zip of the replay layout)")
		return
	}
	if nameTooBig {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-name-too-long", codeInvalidParameter,
			"Bad Request", fmt.Sprintf("the name field is limited to %d bytes", clipNameMaxBytes))
		return
	}
	if err := s.clipsAssets.Verify(ctx, committed.SHA256); err != nil {
		s.log.Warn("clip archive failed verification after commit",
			"request_id", RequestIDFromContext(r.Context()), "error", err)
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-verify-failed", "INTERNAL",
			"Internal Server Error", "the stored archive did not re-hash; try the upload again")
		return
	}

	// 2. Extract into a fresh staging directory, confined.
	seedBytes := make([]byte, 8)
	if _, err := rand.Read(seedBytes); err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-staging-fault", "INTERNAL",
			"Internal Server Error", "a staging name could not be generated; retry")
		return
	}
	staging, err := clips.StagingDir(s.clipsStoreDir, hex.EncodeToString(seedBytes))
	if err != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-staging-fault", "INTERNAL",
			"Internal Server Error", "the staging directory could not be named; retry")
		return
	}
	defer os.RemoveAll(staging) // a no-op on the success path's rename

	blob, _, err := s.clipsAssets.Get(ctx, committed.SHA256)
	if err != nil {
		s.clipUploadStoreProblem(w, r, err)
		return
	}
	format, rest, derr := clips.DetectFormat(blob)
	if derr != nil {
		blob.Close()
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-archive-unreadable", codeInvalidParameter,
			"Bad Request", "the archive could not be read: "+derr.Error())
		return
	}
	if format == clips.FormatUnknown {
		blob.Close()
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-archive-format", codeInvalidParameter,
			"Bad Request", "the archive is neither tar nor zip (gzip-wrapped tar is not admitted)")
		return
	}
	extract, err := clips.Extract(ctx, rest, format, staging, clips.DefaultLimits())
	blob.Close() // extraction has consumed the stream; the header peek is replayed from memory
	if err != nil {
		s.clipInvalid(w, r, err)
		return
	}

	// 3. Validate the layout where the extraction put it.
	layoutDir := staging
	if extract.TopDir != "" {
		layoutDir = filepath.Join(staging, extract.TopDir)
	}
	clip, err := clips.ValidateDir(layoutDir)
	if err != nil {
		s.clipInvalid(w, r, err)
		return
	}

	// 4. Choose the id: the caller's name, the archive's top-level directory,
	//    the uploaded filename, or a hash-derived fallback — in that order.
	//    A collision is a 409, never a silent rename: the id is the session's
	//    selection key, and a surprise id is how a clip gets replaced by the
	//    wrong recording.
	clipID, idErr := chooseClipID(nameField, extract.TopDir, fileName, committed.SHA256)
	if idErr != nil {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-name-invalid", codeInvalidParameter,
			"Bad Request", idErr.Error())
		return
	}
	if _, err := os.Stat(filepath.Join(s.clipsStoreDir, clipID)); err == nil {
		s.writeCodeProblem(w, r, http.StatusConflict, "clip-exists", codeConflict,
			"Conflict",
			fmt.Sprintf("a clip with id %q is already installed; upload with a different name or delete it first", clipID))
		return
	}

	// 5. Move the layout into place and record the provenance. The rename is
	//    within one filesystem (both under the data directory), so it is the
	//    atomic half; the index write follows, and a crash between them is
	//    repaired by the list's adoption pass.
	final := filepath.Join(s.clipsStoreDir, clipID)
	if err := os.MkdirAll(s.clipsStoreDir, 0o700); err != nil {
		s.clipStoreIOProblem(w, r, err, "creating the clip store")
		return
	}
	if err := os.Rename(layoutDir, final); err != nil {
		s.clipStoreIOProblem(w, r, err, "installing the clip")
		return
	}
	actor := ""
	if who := IdentityFromContext(r.Context()); who != nil && who.User != nil {
		actor = who.User.Username
	}
	rec := clips.Record{
		ID:        clipID,
		Name:      strings.TrimSpace(nameField),
		SHA256:    committed.SHA256,
		Size:      committed.Size,
		Frames:    clip.Frames,
		Format:    format.String(),
		CreatedAt: time.Now().UTC(),
		CreatedBy: actor,
	}
	if err := clips.Update(s.clipsStoreDir, func(idx *clips.Index) error {
		if _, exists := idx.Find(clipID); exists {
			// A concurrent install won the id between the stat above and now;
			// single-writer makes this unlikely, and the safe answer is the
			// same 409 the stat would have given.
			os.RemoveAll(final)
			return errClipExists
		}
		idx.Records = append(idx.Records, rec)
		return nil
	}); err != nil {
		if errors.Is(err, errClipExists) {
			s.writeCodeProblem(w, r, http.StatusConflict, "clip-exists", codeConflict,
				"Conflict", fmt.Sprintf("a clip with id %q is already installed", clipID))
			return
		}
		s.log.Error("recording the clip provenance failed",
			"request_id", RequestIDFromContext(r.Context()), "error", err)
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-index-write-failed", "INTERNAL",
			"Internal Server Error", "the clip installed but its provenance could not be recorded; see the server log")
		return
	}
	s.auditCatalogWrite(r, "clips.create", map[string]any{
		"id": clipID, "sha256": committed.SHA256, "bytes": committed.Size,
		"frames": clip.Frames, "format": format.String(),
	})
	resp := clipUploadResponse{
		ID: clipID, Name: strings.TrimSpace(nameField), Frames: clip.Frames,
		ArchiveSHA256: committed.SHA256, ArchiveSize: committed.Size,
		ArchiveFormat: format.String(), CreatedAt: rec.CreatedAt, CreatedBy: actor,
		Detail: fmt.Sprintf(
			"the clip is installed under the managed store and its archive is in the asset store under sha256:%s; start a session with frame_source {\"kind\":\"replay\",\"clip_id\":%q}",
			truncHash(committed.SHA256), clipID),
	}
	writeJSON(w, http.StatusCreated, resp)
}

var errClipExists = errors.New("api: clip id exists")

// chooseClipID applies the id precedence and the id contract (ADR-0073).
// The generated fallback is stable for the same bytes: a retried upload of
// the same archive with no name lands on the same id — and, if that id was
// installed by the attempt it retried, on the 409 that tells it so.
func chooseClipID(name, topDir, fileName, sha256 string) (string, error) {
	if name != "" {
		id, err := clips.NormalizeName(name)
		if err != nil {
			return "", fmt.Errorf("the name %q does not yield a clip id (lowercase letters, digits, dash, dot and underscore; at most %d characters)", name, clips.MaxIDLen)
		}
		return id, nil
	}
	if clips.ValidID(topDir) {
		return topDir, nil
	}
	if base := strings.TrimSuffix(fileName, filepath.Ext(fileName)); base != "" {
		if id, err := clips.NormalizeName(base); err == nil {
			return id, nil
		}
	}
	if len(sha256) >= 12 {
		return "clip-" + sha256[:12], nil
	}
	return "", errors.New("no usable clip id could be derived; supply a name field")
}

func (s *Server) clipInvalid(w http.ResponseWriter, r *http.Request, err error) {
	s.writeCodeProblemFindings(w, r, http.StatusBadRequest, "clip-invalid", codeValidationFailed,
		"Bad Request", "the archive is not a usable replay clip", []string{err.Error()})
}

func (s *Server) clipUploadStoreProblem(w http.ResponseWriter, r *http.Request, err error) {
	var tooLarge errTooLarge
	switch {
	case errors.As(err, &tooLarge), errors.Is(err, assets.ErrTooLarge):
		s.writeCodeProblem(w, r, http.StatusRequestEntityTooLarge, "clip-too-large", codeTooLarge,
			"Request Entity Too Large",
			fmt.Sprintf("a clip archive is limited to %d bytes", clipMaxArchiveBytes))
	case errors.Is(err, assets.ErrUnsafePath), errors.Is(err, assets.ErrInvalidHash):
		s.writeCodeProblem(w, r, http.StatusBadRequest, "clip-unusable-archive", codeInvalidParameter,
			"Bad Request", "the asset store refused the archive")
	default:
		s.log.Warn("clip upload failed in the asset store",
			"request_id", RequestIDFromContext(r.Context()), "error", err)
		s.unavailable(w, r, "asset-store-unavailable", "the archive could not be stored")
	}
}

func (s *Server) clipStoreIOProblem(w http.ResponseWriter, r *http.Request, err error, what string) {
	s.log.Error("clip store I/O failed", "what", what,
		"request_id", RequestIDFromContext(r.Context()), "error", err)
	s.writeCodeProblem(w, r, http.StatusInternalServerError, "clip-store-io", "INTERNAL",
		"Internal Server Error", what+" failed; see the server log for this request_id")
}

// ── DELETE /api/v1/replay/clips/{id} ────────────────────────────────────────

// handleClipsDelete answers DELETE /api/v1/replay/clips/{id}. The id is
// validated against the clip id contract before any filesystem access, and
// the removal is RemoveAll on the joined path — the join cannot escape the
// store because the id cannot contain a separator. A clip that is running in
// an active session is not refused: the session's open files keep reading
// (POSIX unlinks the directory entry, not the bytes), and a session that
// ends mid-clip settles with the source-missing the list already advertises
// for the removed state. The index row is removed best-effort afterwards;
// the deletion of the directory is the act, and the list's reconciliation
// keeps the two in step whatever happened between.
func (s *Server) handleClipsDelete(w http.ResponseWriter, r *http.Request) {
	if !s.clipsReady(w, r) {
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if !clips.ValidID(id) {
		s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-clip-id", codeInvalidParameter,
			"Bad Request", "the clip id in the path is not a valid clip id (a lowercase slug; never a path)")
		return
	}
	path := filepath.Join(s.clipsStoreDir, id)
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		// A row without a directory is still deletable: the cleanup is the
		// point. Nothing else on this path is a "not found".
		if _, rowExists := clipRowExists(s.clipsStoreDir, id); !rowExists {
			s.writeCodeProblem(w, r, http.StatusNotFound, "clip-not-found", codeNotFound,
				"Not Found", fmt.Sprintf("no clip with id %q is installed on this appliance", id))
			return
		}
	} else if err != nil {
		s.clipStoreIOProblem(w, r, err, "inspecting the clip")
		return
	} else if st.IsDir() {
		if err := os.RemoveAll(path); err != nil {
			s.clipStoreIOProblem(w, r, err, "removing the clip directory")
			return
		}
	} else {
		// A regular file where a clip directory should be is not a clip;
		// refusing to remove it is the only answer that cannot delete
		// somebody else's file by a name collision.
		s.writeCodeProblem(w, r, http.StatusConflict, "clip-not-a-directory", codeConflict,
			"Conflict", "the store entry with that id is not a clip directory; resolve it by hand")
		return
	}
	_ = clips.Update(s.clipsStoreDir, func(idx *clips.Index) error {
		out := idx.Records[:0]
		for _, rec := range idx.Records {
			if rec.ID != id {
				out = append(out, rec)
			}
		}
		idx.Records = out
		return nil
	})
	s.auditCatalogWrite(r, "clips.delete", map[string]any{"id": id})
	w.WriteHeader(http.StatusNoContent)
}

// clipRowExists reports the index's view of id, tolerating an unreadable
// index (a corrupt index must not block the directory's deletion).
func clipRowExists(root, id string) (clips.Record, bool) {
	idx, err := clips.Load(root)
	if err != nil {
		return clips.Record{}, false
	}
	rec, ok := idx.Find(id)
	return rec, ok
}
