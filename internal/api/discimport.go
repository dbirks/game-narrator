package api

// /api/v1/import/disc/images and /api/v1/profiles/{id}/import/disc (FR-04,
// ADR-0066): the user imports a game's script from their own disc image
// through the web UI.
//
// Why the directory is allowlisted rather than path-taking (docs/spec/14
// "Filesystem/import", AGENTS.md rule 5). The operator places the image in
// the directory named by GAME_NARRATOR_IMPORT_DIR; the API reads only regular
// files directly in that directory, and a request names the image by its
// basename. Anything path-shaped is refused with 400 and the join is verified
// to stay inside the directory, so a crafted name cannot reach /etc/passwd.
// The image path is never logged and never stored — the job payload and the
// source row carry a basename and content hashes only.
//
// Why a job instead of a request. A real disc takes minutes to hash and parse,
// so the request does the two bounded things — land/verify the bytes (upload
// shape) or verify the named image exists (dir shape) — and answers 202 with
// a durable job of kind import.disc, exactly as import.file does (ADR-0017).
// Progress and the report live at GET /api/v1/jobs/{id} and on the event
// stream as job.* frames.
//
// Why the job reuses the file-import machinery. The extraction yields a BMG,
// and a BMG is one more source format: profileio.ImportFile stages it into the
// asset store, hashes it, reconciles provenance by content hash (a re-import
// of the same disc is a no-op, docs/spec/02) and writes the messages in one
// transaction. The disc side of the job is only: open the image in place
// (never copy it), let the adapter that owns the disc verify it is its game
// and extract the script, hand the bytes to the importer with the provenance
// overrides.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/gcdisc"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/profileio"
	"github.com/dbirks/game-narrator/internal/store"
)

// JobKindImportDisc is the queue name this route enqueues and
// cmd/game-narrator registers a handler for, next to import.file.
const JobKindImportDisc = "import.disc"

const (
	// discImportMaxImageBytes bounds a disc image this appliance will read:
	// a full DVD-sized CISO (4.7 GiB) plus headroom for a container.
	discImportMaxImageBytes = 8 << 30

	// The two request shapes the 202 answers about.
	discSourceDir    = "dir"
	discSourceUpload = "upload"

	discImportJobPriority = 20
	discImportMaxAttempts = 3
)

// DiscImportPayload is one queued disc import. It names a basename (dir
// shape) or a content hash (upload shape) — never a path — so a worker that
// resumes after a crash does the same work the request verified.
type DiscImportPayload struct {
	ProfileID string `json:"profile_id"`
	Source    string `json:"source"`
	// ImageName is the dir shape: the basename inside ImportDir.
	ImageName string `json:"image_name,omitempty"`
	// ImportDir is the allowlisted directory the basename was verified
	// against at enqueue time; the job re-verifies it against its own
	// resolution and never trusts the payload on its own.
	ImportDir string `json:"import_dir,omitempty"`
	// SHA256 is the upload shape: the image bytes' hash in the asset store.
	SHA256    string `json:"sha256,omitempty"`
	Actor     string `json:"actor,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

// discImportReport is the result_json of a succeeded import.disc job. It
// reuses profileio.ImportReport's counter names — the UI's jobsApi.importReport
// reads messages_inserted — and adds the disc-side provenance the review
// screen needs: the image's own hash, the disc's game id and region.
type discImportReport struct {
	profileio.ImportReport

	ImageSHA256 string `json:"image_sha256"`
	GameID      string `json:"game_id"`
	DiscRegion  string `json:"disc_region"`
}

// discImportReady is the honest-degradation gate: the disc import leans on
// the store, the asset store, the adapter registry and the durable queue,
// and an unwired dependency answers 503 rather than an empty list.
func (s *Server) discImportReady(w http.ResponseWriter, r *http.Request) bool {
	if s.store == nil || s.assets == nil || s.adapters == nil || s.jobQueue == nil {
		s.unavailable(w, r, "disc-import-unavailable",
			"the disc import needs the store, the asset store, the adapter registry and the job queue; this build has not wired them")
		return false
	}
	return true
}

// WithDiscImport wires the admin-configured allowlisted directory behind the
// disc-import surface (ADR-0066). Empty means the images route answers 200
// with configured:false (a state, not a fault — the overview panel queries it
// on every draft, on every appliance) and the dir shape of the import answers
// 503; the upload shape is the only import path in that case.
// The store, asset, adapter and queue dependencies are the Server's own
// (WithStore, WithImport, WithJobs).
func WithDiscImport(importDir string) Option {
	return func(s *Server) { s.discImportDir = strings.TrimSpace(importDir) }
}

// discExtractor finds the adapter that can read a disc image, by the
// optional capability the registry already searches by assertion. A build
// without one reports that nothing can read a disc (503 at the route, a
// permanent job error) rather than pointing a byte parser at an image it
// was not told about.
func discExtractor(reg *adapter.Registry) adapter.Adapter {
	for _, m := range reg.Adapters() {
		a, ok := reg.Get(m.ID)
		if !ok {
			continue
		}
		if _, ok := a.(adapter.DiscExtractor); ok {
			return a
		}
	}
	return nil
}

// ── GET /api/v1/import/disc/images ──────────────────────────────────────────

// discImageEntry is one regular file in the allowlisted directory. Basename
// and size only: the directory itself is operator configuration and the API
// never answers with a path (AGENTS.md rule 5).
type discImageEntry struct {
	Name     string `json:"name"`
	Size     int64  `json:"size_bytes"`
	Modified string `json:"modified_at"`
}

type discImagesResponse struct {
	Configured bool             `json:"configured"`
	Adapter    string           `json:"adapter,omitempty"`
	Images     []discImageEntry `json:"images"`
}

// handleDiscImportImages serves GET /api/v1/import/disc/images: what a
// curator can import from the allowlisted directory, before choosing a
// revision.
func (s *Server) handleDiscImportImages(w http.ResponseWriter, r *http.Request) {
	if !s.discImportReady(w, r) {
		return
	}
	resp := discImagesResponse{Images: []discImageEntry{}}
	if ext := discExtractor(s.adapters); ext != nil {
		m := ext.Manifest()
		resp.Adapter = m.ID + "@" + m.Version
	}
	if s.discImportDir == "" {
		// An unconfigured directory is a state, not a fault: this query runs
		// every time the overview renders a draft, on every appliance, and a
		// 503 here would log a browser-console error for a normal
		// configuration. The configured flag is the answer; the dir *action*
		// (POST /profiles/{id}/import/disc) still answers 503, where an error
		// is what the requesting act asked for.
		writeJSON(w, http.StatusOK, resp)
		return
	}
	images, err := listDiscImages(s.discImportDir)
	if err != nil {
		s.unavailable(w, r, "import-dir-unreadable",
			"the import directory is not readable; the operator must be able to list it")
		return
	}
	resp.Configured = true
	resp.Images = images
	writeJSON(w, http.StatusOK, resp)
}

// listDiscImages lists the regular files directly in dir, following
// operator-placed symlinks (a symlinked image is how the image reaches the
// allowlist on a machine that keeps it elsewhere). Unlistable entries are
// skipped rather than guessed at: the list answers "what can be imported".
func listDiscImages(dir string) ([]discImageEntry, error) {
	dirEntries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := make([]discImageEntry, 0, len(dirEntries))
	for _, e := range dirEntries {
		if e.IsDir() {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out = append(out, discImageEntry{
			Name:     e.Name(),
			Size:     info.Size(),
			Modified: info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ── POST /api/v1/profiles/{id}/import/disc ──────────────────────────────────

// discImportRequest is the dir shape's body. The query string carries the same
// field for clients that do not send JSON.
type discImportRequest struct {
	Image string `json:"image"`
}

// discImportAccepted is the 202 body: the job that will do the work, shaped
// the way import.file answers so the UI polls one resource for both.
type discImportAccepted struct {
	JobID     string `json:"job_id"`
	JobKind   string `json:"job_kind"`
	ProfileID string `json:"profile_id"`
	Source    string `json:"source"`
	Image     string `json:"image,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
	State     string `json:"state"`
	Detail    string `json:"detail"`
}

// handleDiscImport serves POST /api/v1/profiles/{id}/import/disc. Two request
// shapes, one 202 body (ADR-0066):
//
//   - dir shape: {"image": "<basename>"} (or ?image=<name>) — the image lives
//     in the allowlisted directory and is read in place by the job;
//   - upload shape: multipart with a `file` part carrying the raw image —
//     streamed through PutStaged/Commit under maxUploadBytes, so a GB-scale
//     real disc is the directory shape's job by design.
func (s *Server) handleDiscImport(w http.ResponseWriter, r *http.Request) {
	if !s.discImportReady(w, r) {
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
		s.writeCodeProblem(w, r, http.StatusConflict, "revision-immutable", "CONFLICT",
			"Conflict",
			fmt.Sprintf("revision %d is %s; content may only enter a draft — derive a revision and import into that", p.RevisionNum, p.Status))
		return
	}
	if discExtractor(s.adapters) == nil {
		s.unavailable(w, r, "no-disc-adapter",
			"no disc-capable adapter is linked into this build; nothing can read a disc image")
		return
	}

	var payload DiscImportPayload
	payload.ProfileID = p.ID
	if id := IdentityFromContext(r.Context()); id != nil && id.User != nil {
		payload.Actor = id.User.Username
	}
	payload.RequestID = RequestIDFromContext(r.Context())

	base, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if base == "multipart/form-data" {
		info, err := s.readDiscUpload(r)
		if err != nil {
			s.importError(w, r, err)
			return
		}
		payload.Source = discSourceUpload
		payload.SHA256 = info.SHA256
	} else {
		name := strings.TrimSpace(r.URL.Query().Get("image"))
		if name == "" {
			var req discImportRequest
			if !s.decodeJSONBody(w, r, &req, maxJSONBodyBytes) {
				return
			}
			name = strings.TrimSpace(req.Image)
		}
		if name == "" {
			s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "image-required", codeJobRequiresInput,
				"Unprocessable Entity",
				"the request needs an image: a basename of a file in the import directory (JSON body {\"image\": …} or ?image=…), or a multipart file part")
			return
		}
		if s.discImportDir == "" {
			s.unavailable(w, r, "import-dir-unavailable",
				"the import directory is not configured; place disc images in the directory the operator names with GAME_NARRATOR_IMPORT_DIR")
			return
		}
		resolved, err := safeDiscImagePath(s.discImportDir, name)
		if err != nil {
			s.writeCodeProblem(w, r, http.StatusBadRequest, "invalid-image-name", codeInvalidParameter,
				"Bad Request",
				"the image must be the plain name of a regular file directly in the import directory: "+err.Error())
			return
		}
		payload.Source = discSourceDir
		payload.ImageName = name
		payload.ImportDir = s.discImportDir
		_ = resolved
	}

	// The idempotency key is honoured durably (the jobs.idempotency_key column),
	// scoped to kind + principal + revision, the same discipline the file
	// import applies (ADR-0017).
	idemKey, ok := s.importIdempotencyKey(w, r, JobKindImportDisc, p.ID)
	if !ok {
		return
	}
	raw, merr := json.Marshal(payload)
	if merr != nil {
		s.writeCodeProblem(w, r, http.StatusInternalServerError, "encode-fault", "INTERNAL",
			"Internal Server Error", "the disc import request could not be queued")
		return
	}
	job, created, qerr := s.jobQueue.Enqueue(r.Context(), store.EnqueueParams{
		Kind:           JobKindImportDisc,
		Payload:        raw,
		Priority:       discImportJobPriority,
		MaxAttempts:    discImportMaxAttempts,
		TraceID:        RequestIDFromContext(r.Context()),
		IdempotencyKey: idemKey,
	})
	if qerr != nil {
		s.log.Warn("disc import enqueue failed",
			"request_id", RequestIDFromContext(r.Context()), "error", qerr)
		s.unavailable(w, r, "jobs-unavailable",
			"the image was verified but could not be queued; retry the import")
		return
	}
	if !created && !sameQueuedDiscImport(job.Payload, payload) {
		s.writeCodeProblem(w, r, http.StatusUnprocessableEntity, "idempotency-key-reused",
			codeIdempotencyConflict, "Unprocessable Entity",
			"this Idempotency-Key already queued a different disc import for this revision; a key identifies one request, not one slot")
		return
	}
	s.auditCatalogWrite(r, "catalog.import.disc.enqueue", map[string]any{
		"job_id": job.ID, "profile_id": p.ID, "source": payload.Source,
		"image": payload.ImageName, "sha256": payload.SHA256,
	})
	resp := discImportAccepted{
		JobID:     job.ID,
		JobKind:   job.Kind,
		ProfileID: p.ID,
		Source:    payload.Source,
		Image:     payload.ImageName,
		SHA256:    payload.SHA256,
		State:     job.State,
	}
	if payload.Source == discSourceUpload {
		resp.Detail = fmt.Sprintf(
			"the image bytes are in the asset store under sha256:%s; follow /api/v1/jobs/%s for progress and the report",
			payload.SHA256, job.ID)
	} else {
		resp.Detail = fmt.Sprintf(
			"the image is read in place from the import directory (never copied); follow /api/v1/jobs/%s for progress and the report — a GB-scale disc can take minutes",
			job.ID)
	}
	if !created {
		resp.Detail += " (this Idempotency-Key had already queued that import)"
	}
	writeJSON(w, http.StatusAccepted, resp)
}

// readDiscUpload streams the file part of the upload shape into the asset
// store under the shared upload cap. The name is never opened: nothing is
// read from an uploaded name (docs/spec/14).
func (s *Server) readDiscUpload(r *http.Request) (assets.Info, error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return assets.Info{}, errParam{field: "body",
			detail: "the body is not readable as multipart/form-data: " + err.Error()}
	}
	for {
		part, perr := mr.NextPart()
		if errors.Is(perr, io.EOF) {
			break
		}
		if perr != nil {
			return assets.Info{}, errParam{field: "body",
				detail: "the multipart body is malformed: " + perr.Error()}
		}
		if part.FileName() == "" {
			part.Close()
			continue
		}
		info, err := s.stageAndCommit(r.Context(), part)
		part.Close()
		if err != nil {
			return assets.Info{}, err
		}
		return info, nil
	}
	return assets.Info{}, errParam{field: "file", code: codeJobRequiresInput,
		detail: "the upload needs a file part carrying the disc image (CISO or plain GCM): a job with no bytes to read can only fail"}
}

// sameQueuedDiscImport answers the idempotent-retry question the way the file
// import answers it: is this the import the key already queued? The shape is
// identity — same target, same source, same bytes or same basename.
func sameQueuedDiscImport(stored json.RawMessage, want DiscImportPayload) bool {
	var queued DiscImportPayload
	if len(stored) == 0 || json.Unmarshal(stored, &queued) != nil {
		return false
	}
	switch want.Source {
	case discSourceUpload:
		return queued.Source == discSourceUpload && queued.SHA256 == want.SHA256 && queued.ProfileID == want.ProfileID
	default:
		return queued.Source == discSourceDir && queued.ImageName == want.ImageName && queued.ImportDir == want.ImportDir && queued.ProfileID == want.ProfileID
	}
}

// ── the allowlisted path ────────────────────────────────────────────────────

// safeDiscImagePath resolves a client-supplied basename inside the
// allowlisted directory and answers with the path only if it names a regular
// file that stays in the directory (docs/spec/14 "Filesystem/import",
// AGENTS.md rule 5). The refusal strings name the rule, not the path: the
// error can be shown to the caller and logged without leaking the operator's
// directory.
func safeDiscImagePath(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	switch {
	case name == "":
		return "", errors.New("the name is empty")
	case name == "." || name == "..":
		return "", errors.New("the name must name a file, not a directory")
	case strings.ContainsAny(name, "/\\\x00"):
		return "", errors.New("the name must not contain a path separator")
	case len(name) > 255:
		return "", errors.New("the name is too long")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", errors.New("the name carries a control character")
		}
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return "", errors.New("the import directory could not be resolved")
	}
	resolved := filepath.Join(absDir, name)
	rel, err := filepath.Rel(absDir, resolved)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("the name resolves outside the import directory")
	}
	info, err := os.Stat(resolved) // follows the operator's symlink, by design
	if err != nil {
		return "", errors.New("no regular file with that name is in the import directory")
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("that name is not a regular file")
	}
	if info.Size() > discImportMaxImageBytes {
		return "", fmt.Errorf("the image is %d bytes; this appliance reads at most %d", info.Size(), discImportMaxImageBytes)
	}
	return resolved, nil
}

// hashDiscImage streams the dir-shape image through a SHA-256 in place — the
// provenance value the coverage note records (AGENTS.md rule 6) — without
// buffering the image.
func hashDiscImage(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ── the import.disc job handler ─────────────────────────────────────────────

// ImportDiscJobHandler is the jobs.Handler for JobKindImportDisc. It lives in
// this package for the reason ImportFileJobHandler gives: the payload it
// decodes is the payload this package writes. The work, in order:
//
//  1. Re-verify the request's invariants against the worker's own view: the
//     profile is still a draft, the dir-shape path still resolves inside the
//     allowlisted directory, the upload's bytes are still under their hash.
//     A payload is data, not trust (AGENTS.md rule 6).
//  2. Open the image — in place for the dir shape (never copied), from the
//     asset store for the upload — and let the disc-capable adapter verify
//     the game id and extract the message script.
//  3. Hand the script to profileio.ImportFile with the provenance overrides:
//     it stages and hashes the BMG, reuses the source row when the same bytes
//     were imported before, binds speakers and writes the messages in one
//     transaction. The report is the job's result.
//
// Refusals that a retry would reproduce (a bad payload, an immutable
// revision, a game id the adapter does not own, an image that left the
// directory) are marked permanent so they do not burn the attempt budget
// (docs/spec/12). A dropped read of the image or the store is retried.
func ImportDiscJobHandler(st *store.Store, as assets.Store, reg *adapter.Registry) jobs.Handler {
	return func(ctx context.Context, job jobs.Job, progress jobs.ProgressFn) (json.RawMessage, error) {
		var p DiscImportPayload
		if err := json.Unmarshal(job.Payload, &p); err != nil {
			return nil, jobs.Permanent(fmt.Errorf("import.disc payload is unreadable: %w", err))
		}
		if !games.ValidID(p.ProfileID) {
			return nil, jobs.Permanent(fmt.Errorf("import.disc payload names profile %q, which is not a usable id", p.ProfileID))
		}
		switch p.Source {
		case discSourceDir:
			if p.ImportDir == "" || p.ImageName == "" {
				return nil, jobs.Permanent(fmt.Errorf("import.disc dir-shape payload is missing its directory or image name"))
			}
		case discSourceUpload:
			if !assets.ValidHash(p.SHA256) {
				return nil, jobs.Permanent(fmt.Errorf("import.disc upload-shape payload names no usable content hash"))
			}
		default:
			return nil, jobs.Permanent(fmt.Errorf("import.disc payload source %q is neither dir nor upload", p.Source))
		}

		profile, _, err := st.GetProfile(ctx, p.ProfileID)
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("import.disc profile %s: %w", p.ProfileID, err))
		}
		if profile.IsImmutable() {
			return nil, jobs.Permanent(fmt.Errorf(
				"import.disc: revision %d is %s and content may only enter a draft; derive a revision and import into that",
				profile.RevisionNum, profile.Status))
		}
		edition, err := st.GetEdition(ctx, profile.EditionID)
		if err != nil {
			return nil, jobs.Permanent(fmt.Errorf("import.disc: the profile's edition %s: %w", profile.EditionID, err))
		}
		ext, ok := discExtractor(reg).(adapter.DiscExtractor)
		if !ok {
			return nil, jobs.Permanent(fmt.Errorf("import.disc: no disc-capable adapter is linked into this build"))
		}

		progress(5, "opening the disc image")
		var (
			d        *gcdisc.Disc
			imageSHA string
			gameID   string
		)
		switch p.Source {
		case discSourceDir:
			resolved, err := safeDiscImagePath(p.ImportDir, p.ImageName)
			if err != nil {
				return nil, jobs.Permanent(fmt.Errorf("import.disc dir shape: %w", err))
			}
			d, err = gcdisc.Open(resolved)
			if err != nil {
				return nil, jobs.Permanent(fmt.Errorf("import.disc: opening the image: %w", err))
			}
			// The image's own hash is provenance (AGENTS.md rule 6), recorded
			// streamed in place — the path itself goes nowhere.
			imageSHA, err = hashDiscImage(resolved)
			if err != nil {
				d.Close()
				return nil, fmt.Errorf("import.disc: hashing the image in place: %w", err)
			}
			gameID = d.Header().GameID
		case discSourceUpload:
			blob, _, err := as.Get(ctx, p.SHA256)
			if err != nil {
				return nil, jobs.Permanent(fmt.Errorf("import.disc: the queued image (sha256 %s) is not in the asset store: %w",
					truncHash(p.SHA256), err))
			}
			data, rerr := io.ReadAll(blob)
			blob.Close()
			if rerr != nil {
				return nil, fmt.Errorf("import.disc: reading the stored image: %w", rerr)
			}
			d, err = gcdisc.OpenReader(bytes.NewReader(data), int64(len(data)))
			if err != nil {
				return nil, jobs.Permanent(fmt.Errorf("import.disc: the stored image is not a readable disc: %w", err))
			}
			imageSHA = p.SHA256
			gameID = d.Header().GameID
		}
		defer d.Close()

		progress(15, "verifying the disc and extracting the message script")
		script, scriptName, gameID, err := ext.ExtractDiscSource(ctx, d)
		if err != nil {
			// A wrong game id is a refusal, not a fault: the retry would read
			// the same image and hear the same no.
			return nil, jobs.Permanent(fmt.Errorf("import.disc: %w", err))
		}

		progress(30, "importing the extracted script")
		actor := p.Actor
		if actor == "" {
			actor = "unattributed"
		}
		report, ierr := profileio.ImportFile(ctx, profileio.Deps{Store: st, Assets: as, Adapters: reg},
			profileio.ImportRequest{
				ProfileID:  p.ProfileID,
				FileName:   scriptName,
				Reader:     bytes.NewReader(script),
				Format:     "bmg",
				Rights:     games.RightsUserSupplied,
				Trust:      1,
				SourceURI:  "gcdisc://" + gameID + "/res/Msg/bmgres.arc",
				SourceKind: games.SourceGameExtract,
				CoverageSuffix: fmt.Sprintf(
					"; disc image sha256 %s; game id %s; region %s; imported by %s",
					imageSHA, gameID, gcdisc.RegionOfGameID(gameID), actor),
				Parse: adapter.ParseOptions{Language: edition.Language},
			})
		if ierr != nil {
			if permanentImportError(ierr) {
				return nil, jobs.Permanent(ierr)
			}
			return nil, ierr
		}
		progress(100, fmt.Sprintf("imported %d messages from the disc script", report.Stats.Imported))

		out := discImportReport{
			ImportReport: report,
			ImageSHA256:  imageSHA,
			GameID:       gameID,
			DiscRegion:   gcdisc.RegionOfGameID(gameID),
		}
		raw, merr := json.Marshal(out)
		if merr != nil {
			return nil, fmt.Errorf("import.disc: the report could not be encoded: %w", merr)
		}
		return raw, nil
	}
}
