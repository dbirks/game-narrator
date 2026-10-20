package api

// The restore half of the backup/restore pair (ADR-0090, docs/spec/09
// /settings "backup/export", gap G6). A restore is a process-lifecycle act —
// the live database file must be closed, replaced and reopened — so the API
// never restores in place. This surface validates the upload, takes the
// automatic pre-restore backup, writes the durable plan (internal/restore)
// and answers; the process then restarts and the boot applies the plan
// before the listener comes up (cmd/game-narrator's runServe calls
// restore.Apply). The post-restart GET reports the recorded outcome, so the
// UI shows success or failure — never silence.
//
// Admin-only by the scope table: the upload replaces the appliance's whole
// catalogue, and the response and the result file carry hash/size facts the
// operator must be able to check.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/restore"
	"github.com/dbirks/game-narrator/internal/store"
)

// maxRestoreUploadBytes bounds one restore upload. The database holds the
// curated catalogue (text and pointers; the binary content lives in the asset
// store beside it, never in SQLite — docs/spec/02), so an appliance database
// is small; the bound exists so a mis-pointed multi-GB file cannot fill the
// data partition through the API. 256 MiB is generous for years of curation.
const maxRestoreUploadBytes = 256 << 20

// restoreBodyLimit bounds the whole request body before the form parser
// touches it. Multipart framing adds the boundary plus per-part headers
// around the file part (a few hundred bytes), so the body bound is the part
// bound plus a framing margin; the part-level bound in stageRestoreUpload is
// the second line of defense. Without the body bound the parser would buffer
// an unbounded upload to disk before any limit could be checked.
const restoreBodyLimit = maxRestoreUploadBytes + 4096

// errRestoreBodyTooLarge is the sentinel the bounded body reports once its
// budget is spent; the handler maps it to the 413.
var errRestoreBodyTooLarge = errors.New("the upload exceeds the restore size limit")

// boundedBody fails once its budget is spent, so an over-limit upload cannot
// fill the data partition through the form parser.
type boundedBody struct {
	r    io.Reader
	left int64
}

func (b *boundedBody) Read(p []byte) (int, error) {
	if b.left <= 0 {
		return 0, errRestoreBodyTooLarge
	}
	if int64(len(p)) > b.left {
		p = p[:b.left]
	}
	n, err := b.r.Read(p)
	b.left -= int64(n)
	if err == nil && b.left <= 0 {
		err = errRestoreBodyTooLarge
	}
	return n, err
}

// Close delegates to the wrapped body; the bound has already done its job by
// then.
func (b *boundedBody) Close() error {
	closer, ok := b.r.(io.Closer)
	if !ok {
		return nil
	}
	return closer.Close()
}

// restoreField is the multipart field name of the uploaded backup.
const restoreField = "backup"

// restoreScheduledView is the 202 answer of POST /api/v1/admin/restore: the
// plan is durable on disk and the appliance is about to restart. The fields
// are what the operator reviews before the restart — the backup's identity
// (hash, size, schema version) and the pre-restore safety net's name.
type restoreScheduledView struct {
	Status              string `json:"status"` // "restart_scheduled"
	Note                string `json:"note"`
	PreRestoreBackup    string `json:"pre_restore_backup"`
	StagedSHA256        string `json:"staged_sha256"`
	StagedBytes         int64  `json:"staged_bytes"`
	BackupSchemaVersion int    `json:"backup_schema_version"`
	BinaryLatestSchema  int    `json:"binary_latest_schema"`
}

// restorePlanView is the plan's operator-facing projection (base names only:
// absolute paths are host state and stay server-side, docs/spec/08).
type restorePlanView struct {
	StagedSHA256        string `json:"staged_sha256"`
	StagedBytes         int64  `json:"staged_bytes"`
	BackupSchemaVersion int    `json:"backup_schema_version"`
	BinaryLatestSchema  int    `json:"binary_latest_schema"`
	PreRestoreBackup    string `json:"pre_restore_backup"`
	RequestedAt         string `json:"requested_at"`
	Actor               string `json:"actor"`
	RequestID           string `json:"request_id"`
}

// restoreStatusView is the GET /api/v1/admin/restore answer: what the last
// restore did (or that one is pending). The UI polls this after the restart.
type restoreStatusView struct {
	// Status is "none" (no restore ever ran and none is pending),
	// "pending" (a plan is waiting for the next boot), "applied" or "failed".
	Status string `json:"status"`
	// Plan is set when Status is pending or was the last attempt's input.
	Plan *restorePlanView `json:"plan,omitempty"`
	// The recorded outcome of the last completed attempt (applied or failed).
	At                      string   `json:"at,omitempty"`
	Error                   string   `json:"error,omitempty"`
	RecoveredFromPreRestore bool     `json:"recovered_from_pre_restore,omitempty"`
	SchemaVersionBefore     int      `json:"schema_version_before,omitempty"`
	SchemaVersionAfter      int      `json:"schema_version_after,omitempty"`
	MigrationsApplied       []string `json:"migrations_applied,omitempty"`
}

// handleAdminRestore answers POST /api/v1/admin/restore (admin role): the
// safe restore flow — staged upload, validation (format, schema marker,
// integrity, forward-migration headroom), automatic pre-restore backup,
// durable plan, audit row, answer, restart.
func (s *Server) handleAdminRestore(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	if s.store == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "store-unavailable",
			"Store Unavailable", "no catalogue store is wired into this process; a restore needs one")
		return
	}
	if s.restarter == nil {
		s.writeProblem(w, r, http.StatusServiceUnavailable, "restart-unavailable",
			"Restart Unavailable",
			"this process cannot restart in place to apply a restore; use the CLI's restore procedure instead")
		return
	}
	dataDir := s.settingsDataDir()
	if dataDir == "" {
		s.writeProblem(w, r, http.StatusInternalServerError, "data-dir-unknown",
			"Internal Error", "the data directory could not be resolved; a restore needs a durable home for its plan")
		return
	}

	// 1. The upload: one multipart file part, streamed to a private temp file
	//    in the restore work directory while its size is bounded. The body
	//    itself is bounded first: the form parser must not buffer an
	//    unbounded upload before the limit can be checked.
	if r.Body != nil {
		r.Body = &boundedBody{r: r.Body, left: restoreBodyLimit}
	}
	filePart, err := restoreFilePart(r)
	if err != nil {
		if errors.Is(err, errRestoreBodyTooLarge) {
			s.writeProblem(w, r, http.StatusRequestEntityTooLarge, "restore-upload-too-large",
				"Payload Too Large",
				fmt.Sprintf("a restore upload is limited to %d bytes", maxRestoreUploadBytes))
			return
		}
		s.writeProblem(w, r, http.StatusBadRequest, "restore-upload-malformed",
			"Bad Request", err.Error())
		return
	}
	staged, stagedSHA, stagedBytes, err := stageRestoreUpload(ctx, dataDir, filePart)
	if err != nil {
		if isUploadSizeError(err) {
			s.writeProblem(w, r, http.StatusRequestEntityTooLarge, "restore-upload-too-large",
				"Payload Too Large",
				fmt.Sprintf("a restore upload is limited to %d bytes", maxRestoreUploadBytes))
			return
		}
		s.writeProblem(w, r, http.StatusBadRequest, "restore-upload-invalid",
			"Bad Request", "reading the uploaded backup failed: "+err.Error())
		return
	}

	// 2. Validation: the file must be a migrated Game Narrator SQLite backup
	//    that passes integrity_check, and its schema must be at or below this
	//    build's migrations (forward-only: the boot migrates it up, never down).
	schemaVersion, verr := restore.ValidateBackup(ctx, staged)
	if verr != nil {
		_ = os.Remove(staged)
		s.writeProblem(w, r, http.StatusUnprocessableEntity, "backup-invalid",
			"Unprocessable Backup",
			"the uploaded file is not a restorable Game Narrator backup: "+verr.Error())
		return
	}
	latest := store.LatestSchemaVersion()
	if schemaVersion > latest {
		_ = os.Remove(staged)
		s.writeProblem(w, r, http.StatusUnprocessableEntity, "backup-newer-than-binary",
			"Unprocessable Backup",
			fmt.Sprintf("the backup is at schema version %d but this build migrates to %d: upgrade the binary before restoring",
				schemaVersion, latest))
		return
	}

	// 3. The automatic pre-restore backup: the operator's safety net, taken
	//    from the live store through the online-backup API (the live
	//    connection keeps serving until the restart).
	preStamp := time.Now().UTC().Format("20060102T150405Z")
	prePath := filepath.Join(restore.BackupsDir(dataDir), "pre-restore-"+preStamp+".sqlite")
	if err := s.store.Backup(ctx, prePath); err != nil {
		_ = os.Remove(staged)
		s.writeProblem(w, r, http.StatusInternalServerError, "pre-restore-backup-failed",
			"Pre-Restore Backup Failed",
			"the automatic pre-restore backup failed, so no restore is scheduled: "+err.Error())
		return
	}
	preSHA, preBytes, err := restore.SHA256File(prePath)
	if err != nil {
		_ = os.Remove(staged)
		s.writeProblem(w, r, http.StatusInternalServerError, "pre-restore-backup-invalid",
			"Pre-Restore Backup Failed",
			"the automatic pre-restore backup could not be hashed: "+err.Error())
		return
	}

	// 4. The durable plan: what the boot will apply. Written last, so a
	//    process that dies between staging and planning leaves no pending
	//    restore (only a scratch file the next request overwrites).
	id := IdentityFromContext(ctx)
	actor := "admin"
	if id != nil {
		actor = id.Username()
	}
	plan := restore.Plan{
		StagedBackup:        staged,
		StagedSHA256:        stagedSHA,
		StagedBytes:         stagedBytes,
		BackupSchemaVersion: schemaVersion,
		BinaryLatestSchema:  latest,
		PreRestoreBackup:    prePath,
		PreRestoreSHA256:    preSHA,
		PreRestoreBytes:     preBytes,
		RequestedAt:         time.Now().UTC(),
		Actor:               actor,
		RequestID:           RequestIDFromContext(ctx),
	}
	if err := restore.WritePlan(dataDir, plan); err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "restore-plan-failed",
			"Internal Error", "the restore plan could not be written durably; no restart was scheduled: "+err.Error())
		return
	}

	// 5. The audit row (best-effort: the plan file is the durable record;
	//    this row makes the scheduling visible in the event stream before the
	//    restart — a successful restore replaces the database and with it
	//    this row, which the applied row then supersedes).
	if _, err := store.NewEventStore(s.store).Append(ctx, events.Envelope{
		Name: restore.EventScheduled,
		Data: mustJSON(map[string]any{
			"actor":                 actor,
			"request_id":            plan.RequestID,
			"staged_sha256":         stagedSHA,
			"staged_bytes":          stagedBytes,
			"backup_schema_version": schemaVersion,
			"pre_restore":           filepath.Base(prePath),
			"pre_restore_sha256":    preSHA,
		}),
	}); err != nil {
		s.log.Warn("restore: the scheduled audit event could not be appended", "error", err)
	}
	s.log.Info("restore: scheduled",
		"actor", actor, "staged_sha256", stagedSHA, "pre_restore", filepath.Base(prePath),
		"backup_schema", schemaVersion, "binary_latest_schema", latest)

	// 6. Answer, then hand the process to the restarter. The response goes
	//    out first: the kernel delivers it after the socket closes, and the
	//    restarter carries the flush grace (cmd wiring). The restarter runs
	//    the process's normal graceful shutdown; under the documented
	//    deployment the supervisor brings the replacement up and its boot
	//    applies the plan. If the process is not restarted, the plan stays
	//    pending and the status route says so.
	writeJSON(w, http.StatusAccepted, restoreScheduledView{
		Status:              "restart_scheduled",
		Note:                "the restore is scheduled and the appliance is shutting down to apply it: under a supervisor (the systemd unit) it is back within seconds, otherwise restart it by hand — GET /api/v1/admin/restore reports whether the restore was applied or failed",
		PreRestoreBackup:    filepath.Base(prePath),
		StagedSHA256:        stagedSHA,
		StagedBytes:         stagedBytes,
		BackupSchemaVersion: schemaVersion,
		BinaryLatestSchema:  latest,
	})
	restart := s.restarter
	go func() {
		if err := restart(); err != nil {
			s.log.Error("restore: the restart failed; the plan stays pending and will apply on the next restart",
				"error", err)
		}
	}()
}

// handleAdminRestoreStatus answers GET /api/v1/admin/restore (admin role):
// the recorded state of the last restore — pending (a plan file exists),
// applied or failed — read from the data directory, so it is answerable after
// the restart by the process that just applied it.
func (s *Server) handleAdminRestoreStatus(w http.ResponseWriter, r *http.Request) {
	dataDir := s.settingsDataDir()
	if dataDir == "" {
		s.writeProblem(w, r, http.StatusInternalServerError, "data-dir-unknown",
			"Internal Error", "the data directory could not be resolved")
		return
	}
	plan, pending, err := restore.LoadPlan(dataDir)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "restore-plan-unreadable",
			"Internal Error", "the pending restore plan is unreadable: "+err.Error())
		return
	}
	result, hasResult, err := restore.LoadResult(dataDir)
	if err != nil {
		s.writeProblem(w, r, http.StatusInternalServerError, "restore-result-unreadable",
			"Internal Error", "the restore result is unreadable: "+err.Error())
		return
	}

	view := restoreStatusView{Status: restore.StatusNone}
	if pending {
		view.Status = restore.StatusPending
		view.Plan = &restorePlanView{
			StagedSHA256:        plan.StagedSHA256,
			StagedBytes:         plan.StagedBytes,
			BackupSchemaVersion: plan.BackupSchemaVersion,
			BinaryLatestSchema:  plan.BinaryLatestSchema,
			PreRestoreBackup:    filepath.Base(plan.PreRestoreBackup),
			RequestedAt:         plan.RequestedAt.UTC().Format(time.RFC3339),
			Actor:               plan.Actor,
			RequestID:           plan.RequestID,
		}
	}
	if hasResult {
		if view.Status == restore.StatusNone {
			view.Status = result.Status
		}
		view.At = result.At.UTC().Format(time.RFC3339)
		view.Error = result.Error
		view.RecoveredFromPreRestore = result.RecoveredFromPreRestore
		view.SchemaVersionBefore = result.SchemaVersionBefore
		view.SchemaVersionAfter = result.SchemaVersionAfter
		view.MigrationsApplied = result.MigrationsApplied
		if view.Plan == nil {
			view.Plan = &restorePlanView{
				StagedSHA256:        result.Plan.StagedSHA256,
				StagedBytes:         result.Plan.StagedBytes,
				BackupSchemaVersion: result.Plan.BackupSchemaVersion,
				BinaryLatestSchema:  result.Plan.BinaryLatestSchema,
				PreRestoreBackup:    filepath.Base(result.Plan.PreRestoreBackup),
				RequestedAt:         result.Plan.RequestedAt.UTC().Format(time.RFC3339),
				Actor:               result.Plan.Actor,
				RequestID:           result.Plan.RequestID,
			}
		}
	}
	writeJSON(w, http.StatusOK, view)
}

// ── upload plumbing ───────────────────────────────────────────────────────────

type uploadSizeError struct{ limit int64 }

func (e uploadSizeError) Error() string {
	return fmt.Sprintf("the upload is %d bytes over the limit", e.limit)
}

// isUploadSizeError reports whether err (or anything it wraps) is the
// streaming bound being exceeded.
func isUploadSizeError(err error) bool {
	var sizeErr uploadSizeError
	return errors.As(err, &sizeErr)
}

// restoreFilePart pulls the one file part out of the upload: a field named
// "backup" wins; with no such field, a lone file part of any name is accepted
// (the UI names it "backup"; a curl user may not).
func restoreFilePart(r *http.Request) (*multipart.FileHeader, error) {
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		// %w keeps the bounded body's sentinel reachable for errors.Is:
		// an over-limit upload must answer 413, not a generic 400.
		return nil, fmt.Errorf("the upload is not a valid multipart form: %w", err)
	}
	form := r.MultipartForm
	if form == nil || len(form.File) == 0 {
		return nil, fmt.Errorf("the upload must be a multipart form with a file field named %q", restoreField)
	}
	if parts := form.File[restoreField]; len(parts) > 0 {
		return parts[0], nil
	}
	if len(form.File) == 1 {
		for _, parts := range form.File {
			if len(parts) == 1 {
				return parts[0], nil
			}
		}
	}
	return nil, fmt.Errorf("the upload must carry exactly one file field (named %q)", restoreField)
}

// stageRestoreUpload streams the part to a private file in the restore work
// directory, enforcing maxRestoreUploadBytes, and returns the staged path,
// its SHA-256 and its size. A failed staging leaves no plan-visible state.
func stageRestoreUpload(ctx context.Context, dataDir string, part *multipart.FileHeader) (string, string, int64, error) {
	if err := ctx.Err(); err != nil {
		return "", "", 0, err
	}
	if err := os.MkdirAll(restore.WorkDir(dataDir), 0o700); err != nil {
		return "", "", 0, err
	}
	tmp, err := os.CreateTemp(restore.WorkDir(dataDir), ".staged-*")
	if err != nil {
		return "", "", 0, err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	src, err := part.Open()
	if err != nil {
		tmp.Close()
		return "", "", 0, err
	}
	defer src.Close()

	// The bound is enforced mid-stream: a part longer than the limit fails
	// the copy and the temp file is dropped, so no over-limit state is ever
	// staged for the plan to name.
	bounded := io.LimitReader(src, maxRestoreUploadBytes+1)
	n, err := io.Copy(tmp, bounded)
	if err != nil {
		tmp.Close()
		return "", "", 0, err
	}
	if err := tmp.Close(); err != nil {
		return "", "", 0, err
	}
	if n > maxRestoreUploadBytes {
		return "", "", 0, uploadSizeError{limit: n - maxRestoreUploadBytes}
	}
	staged := restore.StagedPath(dataDir)
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return "", "", 0, err
	}
	if err := os.Rename(tmpName, staged); err != nil {
		return "", "", 0, err
	}
	sha, bytes, err := restore.SHA256File(staged)
	if err != nil {
		_ = os.Remove(staged)
		return "", "", 0, err
	}
	return staged, sha, bytes, nil
}

// mustJSON is the handler's small "this must marshal or the handler is
// broken" helper for event payloads.
func mustJSON(v any) json.RawMessage {
	raw, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return raw
}
