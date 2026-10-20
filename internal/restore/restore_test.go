package restore

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/store"
)

// The tests here are hermetic: real SQLite files in t.TempDir(), the real
// store's backup/restore machinery, the real migration engine. They are not
// the live-process walk (that is the Playwright restore project's job); they
// are the bookkeeping, the validation gates and the recovery paths.

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// openLiveStore opens and migrates a fresh store at the data directory's
// conventional database path — the runServe sequence Apply expects.
func openLiveStore(t *testing.T, dataDir string) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(dataDir))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// backupOf closes nothing (the caller owns st) and takes an online backup of
// st's database to dest — the API's POST /admin/backup machinery.
func backupOf(t *testing.T, st *store.Store, dest string) {
	t.Helper()
	if err := st.Backup(context.Background(), dest); err != nil {
		t.Fatalf("backup to %s: %v", dest, err)
	}
}

// planFromStaged builds a plan for a staged backup and the given pre-restore
// path, with real hashes.
func planFromStaged(t *testing.T, staged, pre string) Plan {
	t.Helper()
	sha, bytes, err := SHA256File(staged)
	if err != nil {
		t.Fatalf("hash staged: %v", err)
	}
	preSHA, preBytes, err := SHA256File(pre)
	if err != nil {
		t.Fatalf("hash pre-restore: %v", err)
	}
	v, err := ValidateBackup(context.Background(), staged)
	if err != nil {
		t.Fatalf("validate staged: %v", err)
	}
	return Plan{
		StagedBackup:        staged,
		StagedSHA256:        sha,
		StagedBytes:         bytes,
		BackupSchemaVersion: v,
		BinaryLatestSchema:  store.LatestSchemaVersion(),
		PreRestoreBackup:    pre,
		PreRestoreSHA256:    preSHA,
		PreRestoreBytes:     preBytes,
		RequestedAt:         time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC),
		Actor:               "admin",
		RequestID:           "test-req-1",
	}
}

// tamperChecksum corrupts one applied migration's recorded checksum in the
// file at path, the way a backup from a different binary lineage would
// present itself (TestMigrateRefusesTamperedChecksum's condition, on a file).
func tamperChecksum(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s raw: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1"); err != nil {
		t.Fatalf("tamper: %v", err)
	}
}

// ── ValidateBackup ────────────────────────────────────────────────────────────

func TestValidateBackupRejectsForeignFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// (a) A plain text file.
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("not a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateBackup(ctx, text); err == nil {
		t.Error("a text file must not validate as a backup")
	}

	// (b) A real SQLite file that is not a Game Narrator database (no
	//     schema_migrations table).
	empty := filepath.Join(dir, "empty.sqlite")
	es, err := store.Open(ctx, empty)
	if err != nil {
		t.Fatalf("open blank db: %v", err)
	}
	_ = es.Close()
	if _, err := ValidateBackup(ctx, empty); err == nil {
		t.Error("an un-migrated SQLite file must not validate as a backup")
	} else if !strings.Contains(err.Error(), "schema_migrations") {
		t.Errorf("the refusal must name the missing marker: %v", err)
	}
}

func TestValidateBackupAcceptsARealBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openLiveStore(t, dir)
	backup := filepath.Join(dir, "backups", "test.sqlite")
	backupOf(t, st, backup)
	v, err := ValidateBackup(ctx, backup)
	if err != nil {
		t.Fatalf("a real backup must validate: %v", err)
	}
	if v != store.LatestSchemaVersion() {
		t.Errorf("backup schema version = %d, want the build's %d", v, store.LatestSchemaVersion())
	}
}

// ── Apply: the no-op and the full walk ───────────────────────────────────────

func TestApplyNoPlanIsANoOp(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := openLiveStore(t, dir)
	out, outcome, err := Apply(ctx, dir, st, testLog)
	if err != nil {
		t.Fatalf("Apply with no plan: %v", err)
	}
	if outcome != nil {
		t.Errorf("no plan, no outcome: got %+v", outcome)
	}
	if out != st {
		t.Error("no plan: the live store must be returned unchanged")
	}
}

func TestApplyRestoresTheStagedBackup(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	staged := StagedPath(dir)
	pre := filepath.Join(BackupsDir(dir), "pre-restore-test.sqlite")

	// State v1: what the staged backup will carry.
	st := openLiveStore(t, dir)
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "original", MasterVolume: 0.5}); err != nil {
		t.Fatalf("seed v1: %v", err)
	}
	backupOf(t, st, staged)
	// State v2: what the live database will have when the restart happens.
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "mutated", MasterVolume: 0.9}); err != nil {
		t.Fatalf("seed v2: %v", err)
	}
	backupOf(t, st, pre) // the automatic pre-restore snapshot, state v2
	if err := st.Close(); err != nil {
		t.Fatalf("close before the swap: %v", err)
	}
	// The store is now owned by Apply (it closes and reopens it).

	plan := planFromStaged(t, staged, pre)
	if err := WritePlan(dir, plan); err != nil {
		t.Fatalf("write plan: %v", err)
	}

	// The restarted process: a fresh open of the same (v2) database.
	st2 := openLiveStore(t, dir)
	out, outcome, err := Apply(ctx, dir, st2, testLog)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if outcome == nil || outcome.Status != StatusApplied {
		t.Fatalf("outcome = %+v, want applied", outcome)
	}
	// The v1 state is back: the restore replaced v2.
	got, ok, err := out.GetAudioSettings(ctx)
	if err != nil || !ok || got.Device != "original" || got.MasterVolume != 0.5 {
		t.Errorf("restored state = %+v ok=%v err=%v, want the v1 seed", got, ok, err)
	}
	t.Cleanup(func() { _ = out.Close() }) // Apply's store is live now; the test owns it
	// The audit: the durable log carries the applied row.
	evs, err := store.NewEventStore(out).After(ctx, 0, events.Filter{Names: []string{EventApplied}}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("audit events = %d (err %v), want exactly one %s row", len(evs), err, EventApplied)
	}
	// The one-shot files are gone; the pre-restore snapshot is kept.
	if _, err := os.Stat(PlanPath(dir)); !os.IsNotExist(err) {
		t.Error("the plan file must be cleared after a successful apply")
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Error("the staged file must be removed after a successful apply")
	}
	if _, err := os.Stat(pre); err != nil {
		t.Errorf("the pre-restore backup is the operator's safety net and must survive: %v", err)
	}
	// The result is persisted for the post-restart UI.
	res, ok, err := LoadResult(dir)
	if err != nil || !ok || res.Status != StatusApplied || res.Plan.RequestID != "test-req-1" {
		t.Errorf("result = %+v ok=%v err=%v", res, ok, err)
	}
	if res.SchemaVersionBefore != store.LatestSchemaVersion() {
		t.Errorf("schema_before = %d, want the live version", res.SchemaVersionBefore)
	}
	// The pre-restore backup itself is a readable backup (the recovery source).
	if _, err := ValidateBackup(ctx, pre); err != nil {
		t.Errorf("the pre-restore backup must stay a valid backup: %v", err)
	}
}

// ── Apply: the refusal paths (live database untouched) ───────────────────────

func TestApplyRefusesAStagedFileThatNoLongerMatchesThePlan(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	staged := StagedPath(dir)
	pre := filepath.Join(BackupsDir(dir), "pre-restore-test.sqlite")

	st := openLiveStore(t, dir)
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "live", MasterVolume: 0.3}); err != nil {
		t.Fatal(err)
	}
	backupOf(t, st, staged)
	backupOf(t, st, pre)
	plan := planFromStaged(t, staged, pre)
	if err := WritePlan(dir, plan); err != nil {
		t.Fatal(err)
	}
	// The file on disk is no longer what the plan described.
	if err := os.WriteFile(staged, []byte("swapped"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, outcome, err := Apply(ctx, dir, st, testLog)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if outcome == nil || outcome.Status != StatusFailed || !strings.Contains(outcome.Error, "matches the recorded plan") {
		t.Fatalf("outcome = %+v, want a refusal naming the hash mismatch", outcome)
	}
	// The live database was not touched: v-live is intact.
	got, ok, err := out.GetAudioSettings(ctx)
	if err != nil || !ok || got.Device != "live" {
		t.Errorf("live state = %+v ok=%v err=%v, want the untouched row", got, ok, err)
	}
	// And the plan is still pending: the refusal is a recorded fact the
	// operator can see (and re-request), not a silent drop.
	if _, pending, err := LoadPlan(dir); err != nil || !pending {
		t.Errorf("the refused plan must stay pending (pending=%v err=%v)", pending, err)
	}
}

func TestApplyRefusesABackupNewerThanTheBinary(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	staged := StagedPath(dir)
	pre := filepath.Join(BackupsDir(dir), "pre-restore-test.sqlite")

	st := openLiveStore(t, dir)
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "live"}); err != nil {
		t.Fatal(err)
	}
	backupOf(t, st, staged)
	backupOf(t, st, pre)

	// The staged backup claims a schema version this build cannot migrate to
	// (a backup from a future binary lineage).
	db, err := sql.Open("sqlite", staged)
	if err != nil {
		t.Fatal(err)
	}
	next := store.LatestSchemaVersion() + 1
	if _, err := db.Exec("INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, '9999_future.sql', 'x', '2026-01-01T00:00:00Z')", next); err != nil {
		t.Fatalf("plant the future version row: %v", err)
	}
	_ = db.Close()

	plan := planFromStaged(t, staged, pre) // re-validates: now reports next
	if plan.BackupSchemaVersion != next {
		t.Fatalf("setup: validation should read version %d, got %d", next, plan.BackupSchemaVersion)
	}
	if err := WritePlan(dir, plan); err != nil {
		t.Fatal(err)
	}

	out, outcome, err := Apply(ctx, dir, st, testLog)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if outcome == nil || outcome.Status != StatusFailed || !strings.Contains(outcome.Error, "upgrade the binary") {
		t.Fatalf("outcome = %+v, want the version-guard refusal", outcome)
	}
	if _, ok, _ := out.GetAudioSettings(ctx); !ok {
		t.Error("the live database must be untouched by a refused restore")
	}
}

// ── Apply: the swap ran but the restored file is unusable ────────────────────

func TestApplyRecoversFromPreRestoreWhenTheRestoredFileFailsMigration(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	staged := StagedPath(dir)
	pre := filepath.Join(BackupsDir(dir), "pre-restore-test.sqlite")

	st := openLiveStore(t, dir)
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "pre-restore-state", MasterVolume: 0.7}); err != nil {
		t.Fatal(err)
	}
	backupOf(t, st, staged)
	backupOf(t, st, pre)
	// The staged backup's migration history no longer matches this build:
	// the swap succeeds, the open succeeds, the forward migration refuses.
	tamperChecksum(t, staged)
	plan := planFromStaged(t, staged, pre)
	if err := WritePlan(dir, plan); err != nil {
		t.Fatal(err)
	}

	out, outcome, err := Apply(ctx, dir, st, testLog)
	if err != nil {
		t.Fatalf("Apply: the failure must be recovered, not fatal: %v", err)
	}
	t.Cleanup(func() { _ = out.Close() })
	if outcome == nil || outcome.Status != StatusFailed || !outcome.RecoveredFromPreRestore {
		t.Fatalf("outcome = %+v, want a failure recovered from the pre-restore backup", outcome)
	}
	if !strings.Contains(outcome.Error, "migration") && !strings.Contains(outcome.Error, "checksum") {
		t.Errorf("the failure must name the migration problem: %q", outcome.Error)
	}
	// The appliance is running on the pre-restore state.
	got, ok, err := out.GetAudioSettings(ctx)
	if err != nil || !ok || got.Device != "pre-restore-state" {
		t.Errorf("recovered state = %+v ok=%v err=%v, want the pre-restore seed", got, ok, err)
	}
	// The audit says failed, and the plan is consumed (one-shot).
	evs, err := store.NewEventStore(out).After(ctx, 0, events.Filter{Names: []string{EventFailed}}, 10)
	if err != nil || len(evs) != 1 {
		t.Fatalf("audit events = %d (err %v), want exactly one %s row", len(evs), err, EventFailed)
	}
	if _, pending, _ := LoadPlan(dir); pending {
		t.Error("an attempted plan is one-shot: it must be cleared after a failed attempt")
	}
	res, ok, err := LoadResult(dir)
	if err != nil || !ok || res.Status != StatusFailed || !res.RecoveredFromPreRestore {
		t.Errorf("result = %+v ok=%v err=%v", res, ok, err)
	}
}

// ── file discipline ───────────────────────────────────────────────────────────

func TestPlanAndResultFilesArePrivate(t *testing.T) {
	dir := t.TempDir()
	plan := Plan{
		StagedBackup: "/x/staged.sqlite", StagedSHA256: strings.Repeat("ab", 32),
		PreRestoreBackup: "/x/pre.sqlite", RequestedAt: time.Now().UTC(), Actor: "admin",
	}
	if err := WritePlan(dir, plan); err != nil {
		t.Fatalf("WritePlan: %v", err)
	}
	round, ok, err := LoadPlan(dir)
	if err != nil || !ok || round.Actor != plan.Actor {
		t.Fatalf("LoadPlan = %+v ok=%v err=%v", round, ok, err)
	}
	fi, err := os.Stat(PlanPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("plan permissions = %v, want 0600", fi.Mode().Perm())
	}
	if err := WriteResult(dir, Result{Status: StatusApplied, At: time.Now().UTC(), Plan: plan}); err != nil {
		t.Fatalf("WriteResult: %v", err)
	}
	fir, err := os.Stat(ResultPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fir.Mode().Perm() != 0o600 {
		t.Errorf("result permissions = %v, want 0600", fir.Mode().Perm())
	}
	// The working directory is 0700 (it holds the staged copy of the data).
	fid, err := os.Stat(WorkDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	if fid.Mode().Perm() != 0o700 {
		t.Errorf("work dir permissions = %v, want 0700", fid.Mode().Perm())
	}
}

func TestApplyIgnoresACorruptPlanWithoutBlocking(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.MkdirAll(WorkDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PlanPath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	st := openLiveStore(t, dir)
	if err := st.SetAudioSettings(ctx, store.AudioSettings{Device: "still-here"}); err != nil {
		t.Fatal(err)
	}
	out, outcome, err := Apply(ctx, dir, st, testLog)
	if err != nil {
		t.Fatalf("a corrupt plan must not stop the appliance: %v", err)
	}
	if outcome != nil {
		t.Errorf("a corrupt plan yields no outcome, only a log line: %+v", outcome)
	}
	if _, ok, _ := out.GetAudioSettings(ctx); !ok {
		t.Error("the live store must keep serving after a corrupt plan is found")
	}
	// The plan is kept for the support bundle, not deleted.
	if _, err := os.Stat(PlanPath(dir)); err != nil {
		t.Error("the corrupt plan must survive for the support bundle")
	}
}
