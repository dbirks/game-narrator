// Package restore implements the safe half of the backup/restore pair that
// the store owns (internal/store/backup.go): the durable, restart-surviving
// bookkeeping of a restore requested through the UI (docs/spec/09 /settings
// "backup/export", gap G6).
//
// A restore is a process-lifecycle act — the live database file must be
// closed, replaced and reopened, and every component that holds the store's
// handle must start against the new file. The API therefore never restores
// in place. It stages and validates the upload, takes the automatic
// pre-restore backup, writes the plan this package owns, answers and
// restarts the process; at boot, before the listener comes up, Apply runs the
// plan so that a process which cannot complete the restore never claims to be
// serving on data it has already destroyed. The result is persisted where the
// UI reads it after the restart, and the durable event log carries the audit
// rows (docs/spec/13: the operator must see success or failure, not silence).
package restore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/store"
)

// The layout inside the durable data directory. Everything here is 0600/0700
// private state: a plan names a backup file that is, in effect, a copy of the
// operator's catalogue.
const (
	backupsDirName = "backups" // automatic pre-restore snapshots (kept for the operator)
	workDirName    = "restore" // staged.sqlite + plan.json + result.json
	stagedFileName = "staged.sqlite"
	planFileName   = "plan.json"
	resultFileName = "result.json"
)

// Outcome statuses recorded in the result file and answered by the status
// route (the UI renders them verbatim). None and Pending are the two states
// that live in the plan's absence or presence rather than in a result file.
const (
	StatusNone    = "none"
	StatusPending = "pending"
	StatusApplied = "applied"
	StatusFailed  = "failed"
)

// Event names on the durable log (docs/spec/01's dotted vocabulary). They are
// registered at init, the runtime/signal.go seam: the boot application runs
// before any HTTP listener exists, so the names must be available without any
// wiring.
const (
	EventScheduled = "admin.restore.scheduled"
	EventApplied   = "admin.restore.applied"
	EventFailed    = "admin.restore.failed"
)

func init() {
	if err := events.Register(EventScheduled, EventApplied, EventFailed); err != nil {
		panic(fmt.Sprintf("restore: %v", err))
	}
}

// Plan is the restart-surviving request to apply one staged backup to the
// live database. It is written by the API handler after validation and the
// pre-restore backup, read exactly once at boot by Apply, and removed on
// success. A plan is a one-shot: if boot finds one, it means the previous
// restart is the one that was supposed to apply it.
type Plan struct {
	// StagedBackup is the absolute path of the validated upload.
	StagedBackup string `json:"staged_backup"`
	// StagedSHA256 is its hash: boot must not apply a file the plan did not
	// describe (a swapped or truncated file would replace the live database
	// with something the operator never reviewed).
	StagedSHA256 string `json:"staged_sha256"`
	StagedBytes  int64  `json:"staged_bytes"`
	// BackupSchemaVersion is the schema the staged backup is at (validated
	// before the plan was written).
	BackupSchemaVersion int `json:"backup_schema_version"`
	// BinaryLatestSchema is the newest schema this build's migrations reach.
	// A backup above it was refused at request time; the guard repeats at
	// boot because the plan is durable state and the binary is not (a
	// downgraded binary must not try forward migrations it does not have).
	BinaryLatestSchema int `json:"binary_latest_schema"`
	// PreRestoreBackup is the automatic snapshot taken immediately before the
	// swap (the recovery source if the restore itself fails).
	PreRestoreBackup string `json:"pre_restore_backup"`
	PreRestoreSHA256 string `json:"pre_restore_sha256"`
	PreRestoreBytes  int64  `json:"pre_restore_bytes"`
	// RequestedAt / Actor / RequestID are the audit fields: who asked, when,
	// and the correlation id the HTTP answer carried.
	RequestedAt time.Time `json:"requested_at"`
	Actor       string    `json:"actor"`
	RequestID   string    `json:"request_id"`
}

// Result is the recorded outcome of the last restore attempt, persisted in the
// data directory so the UI can show it after the restart. It outlives the
// process that applied it: "the restore happened, here is what it did" must
// be readable by the next boot, not only by the one that ran it.
type Result struct {
	Status string    `json:"status"` // StatusApplied | StatusFailed
	At     time.Time `json:"at"`
	// Plan is the plan the attempt ran (or refused), for the UI's "what was
	// restored from where" line.
	Plan  Plan   `json:"plan"`
	Error string `json:"error,omitempty"`
	// The schema journey: what the live database was at, what the restore
	// left it at, and the forward migrations that ran in between.
	SchemaVersionBefore int      `json:"schema_version_before,omitempty"`
	SchemaVersionAfter  int      `json:"schema_version_after,omitempty"`
	MigrationsApplied   []string `json:"migrations_applied,omitempty"`
	// RecoveredFromPreRestore is set on a failed attempt that still has the
	// appliance running on the pre-restore snapshot.
	RecoveredFromPreRestore bool `json:"recovered_from_pre_restore,omitempty"`
}

// File locations (pure, so tests and the support bundle can name them).
func BackupsDir(dataDir string) string { return filepath.Join(dataDir, backupsDirName) }
func WorkDir(dataDir string) string    { return filepath.Join(dataDir, workDirName) }
func StagedPath(dataDir string) string { return filepath.Join(WorkDir(dataDir), stagedFileName) }
func PlanPath(dataDir string) string   { return filepath.Join(WorkDir(dataDir), planFileName) }
func ResultPath(dataDir string) string { return filepath.Join(WorkDir(dataDir), resultFileName) }

// SHA256File hashes a file and returns the hex digest and its byte count.
func SHA256File(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, fmt.Errorf("hashing %s: %w", filepath.Base(path), err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// writePrivateFile writes raw to path with 0600 permissions atomically
// (temp file + rename in the same directory).
func writePrivateFile(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("restore: create %s: %w", filepath.Dir(path), err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// WritePlan persists a pending plan (0600, atomic).
func WritePlan(dataDir string, p Plan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return writePrivateFile(PlanPath(dataDir), raw)
}

// LoadPlan reads the pending plan; ok is false when there is none.
func LoadPlan(dataDir string) (Plan, bool, error) {
	var p Plan
	raw, err := os.ReadFile(PlanPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return p, false, nil
	}
	if err != nil {
		return p, false, fmt.Errorf("restore: read plan: %w", err)
	}
	if jerr := json.Unmarshal(raw, &p); jerr != nil {
		return p, false, fmt.Errorf("restore: plan is corrupt: %w", jerr)
	}
	return p, true, nil
}

// ClearPlan removes a pending plan (absent plans are not an error).
func ClearPlan(dataDir string) error {
	err := os.Remove(PlanPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// WriteResult persists the last outcome (0600, atomic).
func WriteResult(dataDir string, r Result) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return writePrivateFile(ResultPath(dataDir), raw)
}

// LoadResult reads the last outcome; ok is false when a restore never ran.
func LoadResult(dataDir string) (Result, bool, error) {
	var r Result
	raw, err := os.ReadFile(ResultPath(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return r, false, nil
	}
	if err != nil {
		return r, false, fmt.Errorf("restore: read result: %w", err)
	}
	if jerr := json.Unmarshal(raw, &r); jerr != nil {
		return r, false, fmt.Errorf("restore: result is corrupt: %w", jerr)
	}
	return r, true, nil
}

// ValidateBackup proves a file can be the source of a restore:
//
//   - it opens as a SQLite database (the store's pragmas verify in effect);
//   - it carries the schema_migrations table, i.e. it is a migrated Game
//     Narrator database and not a database of some other product;
//   - PRAGMA integrity_check reports ok.
//
// It returns the schema version the file is at. The file is opened and closed
// by this call; its WAL sidecars (if any) are cleaned up by the close.
func ValidateBackup(ctx context.Context, path string) (int, error) {
	st, err := store.Open(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("restore: %s is not an openable SQLite database: %w", filepath.Base(path), err)
	}
	defer st.Close()

	// The schema_migrations table is the format marker: Restore itself refuses
	// a file without it, so validation must not pass a file the swap would
	// refuse (the operator would lose the restart for nothing).
	haveMigrations, err := st.SchemaMigrationsPresent(ctx)
	if err != nil {
		return 0, err
	}
	if !haveMigrations {
		return 0, fmt.Errorf("restore: %s has no schema_migrations table: it is not a Game Narrator backup",
			filepath.Base(path))
	}
	if err := st.IntegrityCheck(ctx); err != nil {
		return 0, fmt.Errorf("restore: %s fails integrity_check: %w", filepath.Base(path), err)
	}
	v, err := st.SchemaVersion(ctx)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// Apply runs the pending plan, if there is one. It is called at boot, before
// the listener, with the freshly opened (and migrated) live store:
//
//   - no plan: the store is returned unchanged and outcome is nil;
//   - plan present: the live store is closed, the staged backup is copied over
//     the live file (store.Restore's online-backup API with the roles
//     reversed), the file is reopened and forward-migrated, and the outcome
//     is recorded to disk and the durable event log;
//   - any failure after the live file was touched recovers the live database
//     from the automatic pre-restore snapshot, records the failure and
//     returns a running store, so a bad restore degrades to "still on the
//     pre-restore state" rather than to a dead appliance. The only hard
//     failure is one where even the pre-restore recovery cannot open.
//
// The returned store is the one the process should run with.
func Apply(ctx context.Context, dataDir string, st *store.Store, log *slog.Logger) (*store.Store, *Result, error) {
	plan, pending, err := LoadPlan(dataDir)
	if err != nil {
		// A corrupt or unreadable plan must not block the appliance, but it
		// must not vanish either: log it and let the support bundle carry it.
		log.Error("restore: pending plan is unreadable; keeping it for the support bundle; continuing on the current database",
			"error", err)
		return st, nil, nil
	}
	if !pending {
		return st, nil, nil
	}

	log.Info("restore: applying the pending plan",
		"staged", filepath.Base(plan.StagedBackup),
		"staged_sha256", plan.StagedSHA256[:12],
		"backup_schema", plan.BackupSchemaVersion,
		"pre_restore", filepath.Base(plan.PreRestoreBackup))

	live := store.DBPath(dataDir)
	fail := func(format string, a ...any) (*store.Store, *Result, error) {
		outcome := &Result{
			Status: StatusFailed,
			At:     time.Now().UTC(),
			Plan:   plan,
			Error:  fmt.Sprintf(format, a...),
		}
		recordOutcome(ctx, dataDir, st, outcome, log)
		return st, outcome, nil
	}

	// 1. The staged file must be exactly what the plan describes.
	sha, bytes, herr := SHA256File(plan.StagedBackup)
	if herr != nil || sha != plan.StagedSHA256 || bytes != plan.StagedBytes {
		return fail("the staged backup no longer matches the recorded plan (hash %s… / %d bytes): refusing to touch the live database",
			trunc(sha, 12), bytes)
	}

	// 2. Re-validate the staged file and re-check the version guard. The API
	//    handler did both before writing the plan; the plan is durable state
	//    the restart carries, and boot does not trust it blindly.
	backupVersion, verr := ValidateBackup(ctx, plan.StagedBackup)
	if verr != nil {
		return fail("the staged backup failed re-validation at boot: %v", verr)
	}
	if backupVersion > store.LatestSchemaVersion() {
		return fail("the staged backup is at schema version %d but this build migrates to %d: upgrade the binary before restoring",
			backupVersion, store.LatestSchemaVersion())
	}

	before, err := st.SchemaVersion(ctx)
	if err != nil {
		return fail("reading the current schema version: %v", err)
	}

	// 3. The swap. Closing the live store is what makes store.Restore legal
	//    (the destination must not be open).
	if cerr := st.Close(); cerr != nil {
		log.Warn("restore: closing the live store before the swap", "error", cerr)
	}
	rerr := store.Restore(ctx, plan.StagedBackup, live)

	// 3a. A failed swap recovers from the pre-restore snapshot.
	if rerr != nil {
		log.Error("restore: the swap failed; recovering the live database from the pre-restore backup", "error", rerr)
		if perr := store.Restore(ctx, plan.PreRestoreBackup, live); perr != nil {
			return nil, nil, fmt.Errorf("restore: the swap failed (%v) and the pre-restore recovery failed too (%v): the data directory needs manual attention (see docs/release.md)", rerr, perr)
		}
		recovered, oerr := store.Open(ctx, live)
		if oerr != nil {
			return nil, nil, fmt.Errorf("restore: the pre-restore backup could not be opened after recovery: %w", oerr)
		}
		if _, merr := recovered.Migrate(ctx); merr != nil {
			_ = recovered.Close()
			return nil, nil, fmt.Errorf("restore: migrating the recovered database: %w", merr)
		}
		outcome := &Result{
			Status:                  StatusFailed,
			At:                      time.Now().UTC(),
			Plan:                    plan,
			Error:                   fmt.Sprintf("the swap failed: %v", rerr),
			SchemaVersionBefore:     before,
			RecoveredFromPreRestore: true,
		}
		recordOutcome(ctx, dataDir, recovered, outcome, log)
		// The plan is one-shot: it has been attempted. The failure is the
		// recorded fact; the operator retries by requesting a new restore.
		_ = ClearPlan(dataDir)
		return recovered, outcome, nil
	}

	// 3b. Success path: open and forward-migrate the restored file.
	ns, oerr := store.Open(ctx, live)
	if oerr != nil {
		// The restored file will not open: recover the pre-restore state.
		if perr := store.Restore(ctx, plan.PreRestoreBackup, live); perr != nil {
			return nil, nil, fmt.Errorf("restore: the restored database could not be opened (%v) and the pre-restore recovery failed too (%v)", oerr, perr)
		}
		recovered, rerr2 := store.Open(ctx, live)
		if rerr2 != nil {
			return nil, nil, fmt.Errorf("restore: the pre-restore backup could not be opened after recovery: %w", rerr2)
		}
		if _, merr := recovered.Migrate(ctx); merr != nil {
			_ = recovered.Close()
			return nil, nil, fmt.Errorf("restore: migrating the recovered database: %w", merr)
		}
		outcome := &Result{
			Status:                  StatusFailed,
			At:                      time.Now().UTC(),
			Plan:                    plan,
			Error:                   fmt.Sprintf("the restored database could not be opened: %v", oerr),
			SchemaVersionBefore:     before,
			RecoveredFromPreRestore: true,
		}
		recordOutcome(ctx, dataDir, recovered, outcome, log)
		_ = ClearPlan(dataDir)
		return recovered, outcome, nil
	}

	applied, merr := ns.Migrate(ctx)
	if merr != nil {
		_ = ns.Close()
		if perr := store.Restore(ctx, plan.PreRestoreBackup, live); perr != nil {
			return nil, nil, fmt.Errorf("restore: forward migration failed (%v) and the pre-restore recovery failed too (%v)", merr, perr)
		}
		recovered, rerr2 := store.Open(ctx, live)
		if rerr2 != nil {
			return nil, nil, fmt.Errorf("restore: the pre-restore backup could not be opened after recovery: %w", rerr2)
		}
		if _, mm := recovered.Migrate(ctx); mm != nil {
			_ = recovered.Close()
			return nil, nil, fmt.Errorf("restore: migrating the recovered database: %w", mm)
		}
		outcome := &Result{
			Status:                  StatusFailed,
			At:                      time.Now().UTC(),
			Plan:                    plan,
			Error:                   fmt.Sprintf("forward migration of the restored database failed: %v", merr),
			SchemaVersionBefore:     before,
			RecoveredFromPreRestore: true,
		}
		recordOutcome(ctx, dataDir, recovered, outcome, log)
		_ = ClearPlan(dataDir)
		return recovered, outcome, nil
	}

	after, verr2 := ns.SchemaVersion(ctx)
	if verr2 != nil {
		after = 0
	}

	// 4. Success: record, clear the one-shot files, and publish the audit row.
	outcome := &Result{
		Status:              StatusApplied,
		At:                  time.Now().UTC(),
		Plan:                plan,
		SchemaVersionBefore: before,
		SchemaVersionAfter:  after,
		MigrationsApplied:   applied,
	}
	if err := WriteResult(dataDir, *outcome); err != nil {
		log.Error("restore: recording the result", "error", err)
	}
	if err := ClearPlan(dataDir); err != nil {
		log.Warn("restore: clearing the plan file", "error", err)
	}
	removeStaged(plan.StagedBackup)
	publishEvent(ctx, ns, EventApplied, outcome, log)
	log.Info("restore applied",
		"schema_before", before, "schema_after", after,
		"migrations", len(applied), "at", outcome.At.UTC())
	return ns, outcome, nil
}

// removeStaged drops the staged file and its WAL sidecars after a successful
// apply (the live database is a separate file now; the staged copy is done).
func removeStaged(path string) {
	for _, name := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(name)
	}
}

// recordOutcome writes the result file and, when a store is available, the
// durable event row. Both are best-effort on the failure paths: the outcome
// must land on disk even if the event append fails, and a missing store
// (a fail before the live store was closed) still gets its file.
func recordOutcome(ctx context.Context, dataDir string, st *store.Store, outcome *Result, log *slog.Logger) {
	if err := WriteResult(dataDir, *outcome); err != nil {
		log.Error("restore: recording the result", "error", err)
	}
	if st != nil {
		publishEvent(ctx, st, EventFailed, outcome, log)
	}
}

// publishEvent appends the audit row to the durable event log (the hub is not
// involved: at boot the hub may not exist yet, and the event store is the
// durable half anyway). Data carries only what an operator needs: no game
// text, no absolute paths (base names only), no credentials.
func publishEvent(ctx context.Context, st *store.Store, name string, outcome *Result, log *slog.Logger) {
	data, err := json.Marshal(map[string]any{
		"status":                     outcome.Status,
		"at":                         outcome.At.UTC().Format(time.RFC3339),
		"error":                      outcome.Error,
		"actor":                      outcome.Plan.Actor,
		"request_id":                 outcome.Plan.RequestID,
		"staged":                     filepath.Base(outcome.Plan.StagedBackup),
		"staged_sha256":              outcome.Plan.StagedSHA256,
		"pre_restore":                filepath.Base(outcome.Plan.PreRestoreBackup),
		"schema_before":              outcome.SchemaVersionBefore,
		"schema_after":               outcome.SchemaVersionAfter,
		"migrations_applied":         outcome.MigrationsApplied,
		"recovered_from_pre_restore": outcome.RecoveredFromPreRestore,
	})
	if err != nil {
		log.Error("restore: encoding the audit event", "error", err)
		return
	}
	if _, err := store.NewEventStore(st).Append(ctx, events.Envelope{
		Name: name,
		Data: data,
	}); err != nil {
		log.Warn("restore: the audit event could not be appended to the durable log", "event", name, "error", err)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
