package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
)

// backupPageStep bounds how many pages one sqlite3_backup_step copies. Copying
// everything in one step (-1) holds a read lock on the source for the whole
// database; stepping keeps the pauses short enough for a running service
// (https://sqlite.org/backup.html).
const backupPageStep = 256

// maxReportedProblems caps how much of an integrity_check report is put into
// an error message.
const maxReportedProblems = 5

// backupSource is the driver capability this package needs from a connection:
// modernc.org/sqlite exposes the online backup API on its driver connection,
// reachable through database/sql's Conn.Raw.
type backupSource interface {
	NewBackup(dstUri string) (*sqlite.Backup, error)
}

// Backup writes a consistent snapshot of the live database to destPath using
// SQLite's online backup API — deliberately not a file copy, which would miss
// or split WAL content (docs/spec/02 "SQLite schema principles"). The store may
// stay open and keep serving while the backup runs.
//
// An existing destPath is replaced: the destination is a backup target, and
// re-running a backup overwriting the previous snapshot is the expected
// behaviour.
func (s *Store) Backup(ctx context.Context, destPath string) error {
	if destPath == "" {
		return errors.New("store: Backup requires a destination path")
	}
	if err := ensurePrivateDir(filepath.Dir(destPath)); err != nil {
		return err
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: backup %s: %w", s.path, err)
	}
	defer conn.Close()

	if err := copyViaBackupAPI(ctx, conn, destPath); err != nil {
		return err
	}
	return nil
}

// Restore rebuilds destPath from a backup file, again through the online backup
// API with the roles reversed (the backup is the source database).
//
// destPath must not be open: callers Close the Store, Restore, then Open the
// restored file. The existing destination and its -wal/-shm sidecars are
// removed first, because the point of restoring is that the file being replaced
// is untrusted (docs/spec/02 "Migration and backup acceptance").
func Restore(ctx context.Context, backupPath, destPath string) error {
	if backupPath == "" || destPath == "" {
		return errors.New("store: Restore requires both a backup path and a destination path")
	}
	if filepath.Clean(backupPath) == filepath.Clean(destPath) {
		return errors.New("store: Restore source and destination are the same file")
	}
	// The source must exist and be a real database. SQLite creates a missing
	// destination lazily, so without this check a typo in a backup path would
	// "succeed" by producing an empty database over the user's data.
	srcInfo, err := os.Stat(backupPath)
	if err != nil {
		return fmt.Errorf("store: restore from %s: %w", backupPath, err)
	}
	if srcInfo.IsDir() {
		return fmt.Errorf("store: restore from %s: source is a directory", backupPath)
	}
	if err := ensurePrivateDir(filepath.Dir(destPath)); err != nil {
		return err
	}

	for _, name := range []string{destPath, destPath + "-wal", destPath + "-shm"} {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("store: restore: clear %s: %w", name, err)
		}
	}

	// Opened read-only: a restore must never modify the backup it is copying
	// from, even if the copy half-fails.
	db, err := sql.Open(driverName, backupPath+"?_query_only=1&_busy_timeout=5000")
	if err != nil {
		return fmt.Errorf("store: restore from %s: %w", backupPath, err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: restore from %s: %w", backupPath, err)
	}
	defer conn.Close()

	// Prove the source really is a Game Narrator database before clearing the
	// destination: a non-database file or an un-migrated snapshot must abort the
	// restore rather than replace the destination with something empty.
	var catalogTables int
	if err := conn.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", schemaMigrationsTable,
	).Scan(&catalogTables); err != nil {
		return fmt.Errorf("store: restore from %s: %w", backupPath, err)
	}
	if catalogTables == 0 {
		return fmt.Errorf("store: restore from %s: no %s table, this file is not a migrated Game Narrator backup",
			backupPath, schemaMigrationsTable)
	}

	if err := copyViaBackupAPI(ctx, conn, destPath); err != nil {
		return fmt.Errorf("store: restore from %s to %s: %w", backupPath, destPath, err)
	}
	return nil
}

// copyViaBackupAPI copies every page reachable from conn into the database at
// destPath.
func copyViaBackupAPI(ctx context.Context, conn *sql.Conn, destPath string) error {
	return conn.Raw(func(dc any) error {
		src, ok := dc.(backupSource)
		if !ok {
			return fmt.Errorf("store: driver connection %T does not expose the online backup API", dc)
		}
		bck, err := src.NewBackup(destPath)
		if err != nil {
			return fmt.Errorf("store: initialize backup to %s: %w", destPath, err)
		}
		finished := false
		defer func() {
			if !finished {
				_ = bck.Finish()
			}
		}()

		for {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("store: backup to %s aborted: %w", destPath, err)
			}
			more, err := bck.Step(backupPageStep)
			if err != nil {
				return fmt.Errorf("store: copy pages to %s: %w", destPath, err)
			}
			if !more {
				break
			}
		}

		finished = true
		if err := bck.Finish(); err != nil {
			return fmt.Errorf("store: finalize backup to %s: %w", destPath, err)
		}
		return nil
	})
}

// IntegrityCheck runs PRAGMA integrity_check and fails unless the database
// reports "ok". The check runs on a dedicated fresh connection, not the
// shared pool — see Store.integrityCheckConn (fts_repair.go) for why a pooled
// connection can report FTS5 damage the file does not have.
func (s *Store) IntegrityCheck(ctx context.Context) error {
	problems, err := s.IntegrityProblems(ctx)
	if err != nil {
		return err
	}
	if len(problems) > 0 {
		shown := problems
		suffix := ""
		if len(shown) > maxReportedProblems {
			suffix = fmt.Sprintf(" (and %d more)", len(shown)-maxReportedProblems)
			shown = shown[:maxReportedProblems]
		}
		return fmt.Errorf("store: integrity_check on %s reported %d problem(s): %s%s",
			s.path, len(problems), strings.Join(shown, "; "), suffix)
	}
	return nil
}

// SchemaMigrationsPresent reports whether the database carries the
// schema_migrations table — the marker of a migrated Game Narrator database.
// Restore refuses a source without it (a non-database file or an un-migrated
// snapshot must abort the restore rather than replace the destination with
// something empty), and the API's restore validation asks the same question
// before a restart is scheduled (internal/restore.ValidateBackup).
func (s *Store) SchemaMigrationsPresent(ctx context.Context) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		schemaMigrationsTable).Scan(&count); err != nil {
		return false, fmt.Errorf("store: checking %s: %w", schemaMigrationsTable, err)
	}
	return count > 0, nil
}
