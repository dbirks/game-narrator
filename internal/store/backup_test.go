package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBackupProducesAnIndependentUsableCopy is the docs/spec/02 requirement
// that backups go through the SQLite backup API: the snapshot opens on its own,
// passes integrity_check, carries the data, and the live store keeps serving.
func TestBackupProducesAnIndependentUsableCopy(t *testing.T) {
	s := migratedStore(t)
	ctx := testCtx(t)

	insertGame(t, s.db, "game-1", "test-game")
	insertEdition(t, s.db, "ed-1", "game-1")
	insertProfile(t, s.db, "p-1", "ed-1", 1, "published", nil)
	insertSource(t, s.db, "src-1", "p-1", "user_file", shaA)
	insertBlob(t, s.db, shaA, 2048)

	dest := filepath.Join(t.TempDir(), "snapshot.sqlite")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The live store is still usable after the snapshot — an online backup, not
	// a shutdown-and-copy.
	insertGame(t, s.db, "game-2", "added-after-backup")

	backup := openPath(t, dest)
	if err := backup.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck on the backup: %v", err)
	}
	if v, err := backup.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("backup SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
	for table, want := range map[string]int{
		"games":        1, // game-2 landed after the snapshot was taken
		"editions":     1,
		"profiles":     1,
		"sources":      1,
		"source_blobs": 1,
	} {
		if n := countRows(t, backup.db, table); n != want {
			t.Errorf("backup %s rows = %d, want %d", table, n, want)
		}
	}
	if n := countRows(t, s.db, "games"); n != 2 {
		t.Errorf("live games rows = %d, want 2", n)
	}

	// A WAL database that is later read through the backup must still be
	// consistent with foreign keys enforced.
	if _, err := backup.db.Exec(`INSERT INTO editions (id, game_id, region, language, revision)
		VALUES ('ed-bad', 'missing', 'EU', 'de', 'rev0')`); err == nil {
		t.Error("backup copy does not enforce foreign keys")
	}
}

func TestBackupRejectsEmptyDestination(t *testing.T) {
	s := migratedStore(t)
	if err := s.Backup(testCtx(t), ""); err == nil {
		t.Fatal("Backup(\"\") = nil error, want failure")
	}
}

func TestRestoreRejectsSillyArguments(t *testing.T) {
	ctx := testCtx(t)
	dir := t.TempDir()
	db := filepath.Join(dir, FileName)
	if err := Restore(ctx, "", db); err == nil {
		t.Fatal("Restore with no source = nil error, want failure")
	}
	if err := Restore(ctx, db, db); err == nil {
		t.Fatal("Restore onto itself = nil error, want failure")
	}
	if err := Restore(ctx, filepath.Join(dir, "nope.sqlite"), filepath.Join(dir, "dest.sqlite")); err == nil {
		t.Fatal("Restore from a missing backup = nil error, want failure")
	}

	// A file that is not a SQLite database must not be usable as a backup.
	junk := filepath.Join(dir, "junk.sqlite")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not a database ", 900)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, junk, filepath.Join(dir, "dest-junk.sqlite")); err == nil {
		t.Fatal("Restore from a non-database file = nil error, want failure")
	}

	// An empty, unmigrated database is not a Game Narrator backup either.
	freshPath := filepath.Join(dir, "fresh.sqlite")
	fresh := openPath(t, freshPath)
	if err := fresh.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, freshPath, filepath.Join(dir, "dest-fresh.sqlite")); err == nil {
		t.Fatal("Restore from an unmigrated database = nil error, want failure")
	}
}

// TestIntegrityCheckOnHealthyDB: the check must pass, so a passing gate means
// something.
func TestIntegrityCheckOnHealthyDB(t *testing.T) {
	s := migratedStore(t)
	insertGame(t, s.db, "game-1", "test-game")
	if err := s.IntegrityCheck(testCtx(t)); err != nil {
		t.Fatalf("IntegrityCheck on a healthy database: %v", err)
	}
}

// TestCorruptThenRestore is the acceptance scenario in docs/spec/02
// "Migration and backup acceptance": back up, simulate corruption, prove it is
// detected, restore from the backup, and prove the data is really there.
func TestCorruptThenRestore(t *testing.T) {
	ctx := testCtx(t)
	dir := t.TempDir()
	livePath := filepath.Join(dir, FileName)
	backupPath := filepath.Join(dir, "backup.sqlite")
	corruptPath := filepath.Join(dir, "corrupt.sqlite")

	// 1. Write a game row and take an online backup.
	s := openPath(t, livePath)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	insertGame(t, s.db, "game-keep", "keep-me")
	mustExec(t, s.db, `UPDATE games SET title = 'Wind Waker (synthetic)' WHERE id = 'game-keep'`)
	if err := s.Backup(ctx, backupPath); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	// Close so the copy below is a complete, quiesced database.
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 2. Simulate corruption by overwriting the first 4096 bytes (page 1, the
	//    database header) of a closed copy of the live database with zeros.
	live, err := os.ReadFile(livePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(livePath + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected the WAL to be checkpointed and removed on close, stat: %v", err)
	}
	corrupt := make([]byte, len(live))
	copy(corrupt, live)
	if len(corrupt) < 8192 {
		t.Fatalf("database is only %d bytes; too small to corrupt page 1 meaningfully", len(corrupt))
	}
	zeros := bytes.Repeat([]byte{0}, 4096)
	copy(corrupt[:4096], zeros)
	if err := os.WriteFile(corruptPath, corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	corruptStore, openErr := Open(ctx, corruptPath)
	if openErr != nil {
		t.Logf("Open detected the corruption: %v", openErr)
	} else {
		// If the driver tolerates a zeroed header on open, integrity_check must
		// still see it; either detection path satisfies the acceptance clause.
		if err := corruptStore.IntegrityCheck(ctx); err == nil {
			corruptStore.Close()
			t.Fatal("zeroing the database header went undetected by both Open and IntegrityCheck")
		} else {
			t.Logf("IntegrityCheck detected the corruption: %v", err)
		}
		if err := corruptStore.Close(); err != nil {
			t.Fatalf("Close after corruption probe: %v", err)
		}
	}

	// 3. Restore: replace the damaged file from the backup and reopen it.
	if err := Restore(ctx, backupPath, corruptPath); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	restored := openPath(t, corruptPath)
	if err := restored.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck after restore: %v", err)
	}
	if v, err := restored.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("restored SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
	var title string
	if err := restored.db.QueryRow(`SELECT title FROM games WHERE id = 'game-keep'`).Scan(&title); err != nil {
		t.Fatalf("restored row missing: %v", err)
	}
	if title != "Wind Waker (synthetic)" {
		t.Fatalf("restored title = %q, want the value written before the backup", title)
	}
	if n := countRows(t, restored.db, "schema_migrations"); n != LatestSchemaVersion() {
		t.Fatalf("restored schema_migrations rows = %d, want %d", n, LatestSchemaVersion())
	}

	// 4. Restoring must not consume or mutate the backup: it is the only copy.
	source := openPath(t, backupPath)
	if err := source.IntegrityCheck(ctx); err != nil {
		t.Fatalf("backup damaged by a restore: %v", err)
	}
	if n := countRows(t, source.db, "games"); n != 1 {
		t.Fatalf("backup games rows = %d, want 1", n)
	}
}
