package api

// The restore route's handler tests (ADR-0090, docs/spec/09 /settings "backup
// and restore"). The restart seam is faked (the test process must not re-exec
// itself); the boot's swap is driven by internal/restore.Apply directly, which
// is exactly what the restarted process runs at start. Coverage owed: the
// admin gate, the validation refusals (malformed, foreign, newer-than-binary,
// oversize), the happy path (202 with the plan's evidence, the pre-restore
// copy, the restart call) and the post-restart status the page renders.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/restore"
	"github.com/dbirks/game-narrator/internal/store"
)

// restoreMultipart builds a request carrying the file part "backup".
func restoreMultipart(t *testing.T, filename string, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile(restoreField, filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/restore", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return req
}

// gnBackup returns a fresh backup of a live migrated store. The store is a
// private copy of the package's shared migrated template (fixture_test.go,
// issue #15), so the backup's contents are unchanged: a migrated, empty store.
func gnBackup(t *testing.T) []byte {
	t.Helper()
	st := openTemplateStore(t)
	backup := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := st.Backup(context.Background(), backup); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(backup)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// gnBackupAtVersion returns a valid GN backup whose schema_migrations claims
// the given version (a row past this build's migrations), for the
// newer-than-binary refusal.
func gnBackupAtVersion(t *testing.T, version int) []byte {
	t.Helper()
	raw := gnBackup(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "backup.sqlite")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// The name is arbitrary: the handler compares versions, not checksums.
	if _, err := db.Exec("INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		version, "future/0000-fake.sql", "0000", time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	// Close before reading: the backup's header claims WAL mode, so the
	// transaction sits in a -wal sidecar until a clean close checkpoints it
	// into the main file.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// TestAdminRestoreRequiresAdmin pins the scope table's grants: every role
// below admin is refused on both routes with a 403 problem.
func TestAdminRestoreRequiresAdmin(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	for _, tc := range []struct{ method, target string }{
		{http.MethodPost, "/api/v1/admin/restore"},
		{http.MethodGet, "/api/v1/admin/restore"},
	} {
		for _, role := range []auth.Role{auth.RoleViewer, auth.RoleCurator, auth.RoleOperator, auth.RoleDeveloper} {
			req := httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{}`))
			rec := doReq(t, h, a.as(role, req))
			if rec.Code != http.StatusForbidden {
				t.Errorf("%s %s as %s = %d, want 403 (body %s)",
					tc.method, tc.target, role, rec.Code, rec.Body.String())
			}
		}
	}
}

// TestAdminRestoreNeedsStoreAndRestarter: without either seam the route
// answers 503 and names what is missing — never a silent no-op.
func TestAdminRestoreNeedsStoreAndRestarter(t *testing.T) {
	a := newTestAuth(t)

	// No store wired at all.
	hNoStore := a.handler(WithWebAssets(uiBuild()), WithRestarter(func() error { return nil }))
	rec := doReq(t, hNoStore, a.as(auth.RoleAdmin,
		restoreMultipart(t, "b.sqlite", gnBackup(t))))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without a store = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}

	// A store, but the process cannot restart in place.
	hNoRestart := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	rec = doReq(t, hNoRestart, a.as(auth.RoleAdmin,
		restoreMultipart(t, "b.sqlite", gnBackup(t))))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without a restarter = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
}

// TestAdminRestoreRefusals pins the validation refusals: each one is a
// problem with the documented code, and none of them touches the store or
// schedules a restart.
func TestAdminRestoreRefusals(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st),
		WithRestarter(func() error { t.Error("a refused restore must not restart the process"); return nil }))

	// Not multipart at all.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/restore", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", ContentTypeJSON)
	rec := doReq(t, h, a.as(auth.RoleAdmin, req))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("malformed upload = %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}

	// A real SQLite file that is not a Game Narrator backup.
	plainDB := filepath.Join(t.TempDir(), "plain.sqlite")
	if err := os.WriteFile(plainDB, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", plainDB)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("CREATE TABLE t (x INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	plainRaw, err := os.ReadFile(plainDB)
	if err != nil {
		t.Fatal(err)
	}
	rec = doReq(t, h, a.as(auth.RoleAdmin, restoreMultipart(t, "plain.sqlite", plainRaw)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("foreign database = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	} else if !strings.Contains(rec.Body.String(), "backup-invalid") {
		t.Errorf("foreign database must carry the backup-invalid code: %s", rec.Body.String())
	}

	// A Game Narrator backup whose schema is newer than this binary.
	latest := store.LatestSchemaVersion()
	rec = doReq(t, h, a.as(auth.RoleAdmin, restoreMultipart(t, "newer.sqlite", gnBackupAtVersion(t, latest+7))))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("newer backup = %d, want 422 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "backup-newer-than-binary") {
		t.Errorf("newer backup must carry the backup-newer-than-binary code: %s", rec.Body.String())
	}
}

// TestAdminRestoreHappyPath is the G6 flow at the API's altitude: upload →
// validate → pre-restore copy → plan → 202 → (fake) restart → the boot's swap
// → the post-restart status. The assertions are the operator's: the 202 names
// the backup's hash and the safety copy; the restart was requested; after the
// swap the store is the backup's contents; the status route reports applied
// with the evidence.
func TestAdminRestoreHappyPath(t *testing.T) {
	a := newTestAuth(t)
	ctx := context.Background()

	// The live store gains a user, then is backed up; the backup predates the
	// later-created user, so a correct restore removes that user.
	differentUser := "restored-away"
	u, err := a.svc.CreateUser(ctx, differentUser, testPassword, auth.RoleOperator, nil, "test-fixture")
	if err != nil {
		t.Fatal(err)
	}
	_ = u
	liveBackup := gnBackup(t) // a store with no such user
	_ = liveBackup

	// Wire the process: the store the page serves, and the restart seam the
	// handler hands the process to. The seam records its call instead of
	// re-executing.
	restarted := make(chan struct{}, 1)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st),
		WithRestarter(func() error {
			select {
			case restarted <- struct{}{}:
			default:
			}
			return nil
		}))

	rec := doReq(t, h, a.as(auth.RoleAdmin, restoreMultipart(t, "curated.sqlite", liveBackup)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("restore = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}
	var scheduled struct {
		Status              string `json:"status"`
		PreRestoreBackup    string `json:"pre_restore_backup"`
		StagedSHA256        string `json:"staged_sha256"`
		StagedBytes         int64  `json:"staged_bytes"`
		BackupSchemaVersion int    `json:"backup_schema_version"`
		BinaryLatestSchema  int    `json:"binary_latest_schema"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &scheduled); err != nil {
		t.Fatal(err)
	}
	if scheduled.Status != "restart_scheduled" {
		t.Errorf("status = %q, want restart_scheduled", scheduled.Status)
	}
	wantSHA := sha256.Sum256(liveBackup)
	if scheduled.StagedSHA256 != hex.EncodeToString(wantSHA[:]) {
		t.Errorf("staged_sha256 = %s, want the uploaded file's hash %s",
			scheduled.StagedSHA256, hex.EncodeToString(wantSHA[:]))
	}
	if scheduled.PreRestoreBackup == "" || !strings.HasSuffix(scheduled.PreRestoreBackup, ".sqlite") {
		t.Errorf("pre_restore_backup = %q, want the safety copy's name", scheduled.PreRestoreBackup)
	}
	if scheduled.BinaryLatestSchema != store.LatestSchemaVersion() {
		t.Errorf("binary_latest_schema = %d, want %d", scheduled.BinaryLatestSchema, store.LatestSchemaVersion())
	}

	// The restart seam was called after the answer.
	select {
	case <-restarted:
	case <-time.After(time.Second):
		t.Fatal("the restarter was not called after the 202")
	}

	// The pre-restore copy exists and is a real SQLite file of the live store.
	dataDir := filepath.Dir(a.st.Path())
	pre := filepath.Join(restore.BackupsDir(dataDir), scheduled.PreRestoreBackup)
	if st, err := os.Stat(pre); err != nil || st.Size() == 0 {
		t.Fatalf("the pre-restore copy is missing or empty: %v", err)
	}

	// The plan is durable before the restart (what the boot will apply).
	plan, pending, err := restore.LoadPlan(dataDir)
	if err != nil || !pending {
		t.Fatalf("the plan is not pending after the 202: %v", err)
	}
	if plan.StagedSHA256 != scheduled.StagedSHA256 {
		t.Errorf("plan hash = %s, want the 202's %s", plan.StagedSHA256, scheduled.StagedSHA256)
	}

	// Simulate the restarted process's boot: the old process is gone (its
	// store is closed and its WAL flushed — which is why the test closes a.st
	// here, not in cleanup), then the new process opens the live store and
	// applies the pending plan. This is exactly cmd/game-narrator's runServe
	// step.
	if err := a.st.Close(); err != nil {
		t.Fatal(err)
	}
	fresh, err := store.Open(ctx, a.st.Path())
	if err != nil {
		t.Fatal(err)
	}
	applied, result, err := restore.Apply(ctx, dataDir, fresh, discardLogger)
	if err != nil {
		t.Fatalf("the boot's swap failed: %v", err)
	}
	if result == nil || result.Status != restore.StatusApplied {
		t.Fatalf("swap result = %+v, want applied", result)
	}
	defer func() { _ = applied.Close() }()

	// The swap is real: the different user the live store gained after the
	// backup is gone, and the store is at this binary's schema. The check
	// runs against the swapped store — the old one is the dead process's.
	if _, err := store.NewAuthStore(applied).UserByUsername(ctx, differentUser); err == nil {
		t.Error("the post-backup user survived the restore; the swap installed the wrong file")
	}
	if v, err := applied.SchemaVersion(ctx); err != nil || v != store.LatestSchemaVersion() {
		t.Errorf("post-restore schema = %d (%v), want %d", v, err, store.LatestSchemaVersion())
	}

	// The post-restart status the settings page renders: applied, with the
	// evidence. The restored catalogue is a fresh store — the old admin is
	// gone with it — so the restarted process's first act (seeding the
	// account) happens here, and the check runs on the new process's auth
	// stack over the swapped store.
	repo2 := store.NewAuthStore(applied)
	svc2 := auth.NewService(repo2, auth.WithPasswordCost(1000), auth.WithLogger(discardLogger))
	if _, err := svc2.Setup(ctx, "admin", testPassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	admin2, err := repo2.UserByUsername(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	tok, _, err := svc2.CreateToken(ctx, nil, auth.CreateTokenParams{UserID: admin2.ID, Name: "restarted-admin", Role: auth.RoleAdmin}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	h2 := New(testBuild, testStarted, discardLogger,
		WithAuth(svc2, repo2), WithWebAssets(uiBuild()), WithStore(applied)).Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/restore", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = doReq(t, h2, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	body := settingsBody(t, rec)
	if body["status"] != "applied" {
		t.Fatalf("status = %v, want applied (body %s)", body["status"], rec.Body.String())
	}
	if at, _ := body["at"].(string); at == "" {
		t.Error("the applied answer must carry when the swap finished")
	}
}

// TestAdminRestoreOversizeUpload is bounded mid-stream: the copy stops at the
// limit, the answer is 413 with the limit named, and no plan is written. The
// body is a generator, not a 256 MiB allocation.
func TestAdminRestoreOversizeUpload(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st),
		WithRestarter(func() error { t.Error("an oversize upload must not restart"); return nil }))

	boundary := "restore-boundary"
	var header bytes.Buffer
	fmt.Fprintf(&header, "--%s\r\nContent-Disposition: form-data; name=%q; filename=%q\r\nContent-Type: application/octet-stream\r\n\r\n",
		boundary, restoreField, "big.sqlite")
	// Past the body bound (not merely the part bound): the framing margin is
	// for headers, and a part that alone exceeds the body budget must be
	// refused by the bounded body before the parser can buffer it.
	overflow := int64(restoreBodyLimit) + 1
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/restore",
		io.MultiReader(&header, &repeatReader{remaining: overflow}))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

	rec := doReq(t, h, a.as(auth.RoleAdmin, req))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize upload = %d, want 413 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), fmt.Sprintf("%d bytes", maxRestoreUploadBytes)) {
		t.Errorf("the 413 must name the limit: %s", rec.Body.String())
	}

	// Nothing was scheduled.
	dataDir := filepath.Dir(a.st.Path())
	if _, pending, _ := restore.LoadPlan(dataDir); pending {
		t.Error("an oversize upload left a pending plan behind")
	}
}

// repeatReader emits a fixed byte until its budget is spent.
type repeatReader struct {
	remaining int64
}

func (r *repeatReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	r.remaining -= n
	for i := int64(0); i < n; i++ {
		p[i] = 'a'
	}
	return int(n), nil
}

// TestAdminRestoreStatusStates pins the four states of the post-restart route:
// none (a fresh data directory), pending (a plan the restart has not applied),
// failed (the boot's recorded outcome) — applied is covered by the happy path.
func TestAdminRestoreStatusStates(t *testing.T) {
	a := newTestAuth(t)
	h := a.handler(WithWebAssets(uiBuild()), WithStore(a.st))
	dataDir := filepath.Dir(a.st.Path())

	get := func() map[string]any {
		rec := doReq(t, h, a.as(auth.RoleAdmin, httptest.NewRequest(http.MethodGet, "/api/v1/admin/restore", nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		return settingsBody(t, rec)
	}

	// None.
	if body := get(); body["status"] != "none" {
		t.Errorf("fresh state = %v, want none (body %v)", body["status"], body)
	}

	// Pending: a plan on disk the boot has not yet applied.
	backup := gnBackup(t)
	staged := restore.StagedPath(dataDir)
	if err := os.MkdirAll(restore.WorkDir(dataDir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(staged, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	sha, bytes, err := restore.SHA256File(staged)
	if err != nil {
		t.Fatal(err)
	}
	plan := restore.Plan{
		StagedBackup:        staged,
		StagedSHA256:        sha,
		StagedBytes:         bytes,
		BackupSchemaVersion: store.LatestSchemaVersion(),
		BinaryLatestSchema:  store.LatestSchemaVersion(),
		PreRestoreBackup:    filepath.Join(restore.BackupsDir(dataDir), "pre-restore-test.sqlite"),
		RequestedAt:         time.Now().UTC(),
		Actor:               "admin",
		RequestID:           "test-request",
	}
	if err := restore.WritePlan(dataDir, plan); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restore.ClearPlan(dataDir) })

	body := get()
	if body["status"] != "pending" {
		t.Fatalf("pending state = %v, want pending (body %v)", body["status"], body)
	}
	planView, _ := body["plan"].(map[string]any)
	if planView == nil || planView["staged_sha256"] != sha {
		t.Errorf("pending plan view = %v, want the plan's hash %s", planView, sha)
	}

	// Failed: the boot attempted the plan and it did not work. Apply clears
	// the one-shot plan after attempting it, so the post-boot state is the
	// result without the plan — reproduce exactly that.
	if err := restore.WriteResult(dataDir, restore.Result{
		Status: restore.StatusFailed,
		At:     time.Now().UTC(),
		Error:  "the restored backup failed to open after the swap",
		Plan:   plan,
	}); err != nil {
		t.Fatal(err)
	}
	if err := restore.ClearPlan(dataDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(restore.ResultPath(dataDir)) })

	body = get()
	if body["status"] != "failed" {
		t.Fatalf("failed state = %v, want failed (body %v)", body["status"], body)
	}
	if body["error"] != "the restored backup failed to open after the swap" {
		t.Errorf("the failure must carry the boot's recorded reason: %v", body["error"])
	}
}
