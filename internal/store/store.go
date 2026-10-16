// Package store owns Game Narrator's SQLite persistence: opening the database
// with the pragmas required by docs/spec/02-data-and-storage.md, applying
// versioned embedded migrations, and online backup/restore.
//
// The driver is modernc.org/sqlite (pure Go, CGO_ENABLED=0 compatible) as
// recorded in docs/adr/0008-sqlite-driver-and-migrations.md.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// FileName is the conventional database file name inside the data directory
// (docs/spec/02: the durable asset root is the data dir, the catalogue lives
// beside it).
const FileName = "game-narrator.sqlite"

// driverName is the name modernc.org/sqlite registers itself under.
const driverName = "sqlite"

// Runtime pragma requirements from docs/spec/02 "SQLite schema principles":
// foreign keys ON, WAL, a busy timeout, and durable-enough writes without the
// cost of FULL on a 4 GB appliance.
const (
	requiredForeignKeys             = 1
	requiredJournalMode             = "wal"
	requiredBusyTimeout             = 5000
	requiredSynchronous             = 1 // NORMAL
	maxOpenConns                    = 8
	maxIdleConns                    = 4
	dirPerm             os.FileMode = 0o700
)

// DBPath returns the database file inside a data directory.
func DBPath(dataDir string) string {
	return filepath.Join(dataDir, FileName)
}

// Store is an open SQLite database plus the migrations embedded in this
// binary. It is safe for concurrent use; the underlying connection pool is
// bounded so a stalled writer cannot exhaust file descriptors on the target
// hardware (AGENTS.md "Keep runtime predictable").
type Store struct {
	db   *sql.DB
	path string
}

// dsn builds the connection string. The pragmas are DSN parameters rather than
// statements run once after opening, because foreign_keys, busy_timeout and
// synchronous are per-connection settings in SQLite: putting them here means
// every connection database/sql later opens is configured the same way.
func dsn(path string) string {
	q := url.Values{}
	q.Set("_foreign_keys", strconv.Itoa(requiredForeignKeys))
	q.Set("_journal_mode", strings.ToUpper(requiredJournalMode))
	q.Set("_busy_timeout", strconv.Itoa(requiredBusyTimeout))
	// NORMAL is the name of the level whose numeric pragma value is
	// requiredSynchronous; the read-back check in verifyPragmas is what proves
	// the two agree.
	q.Set("_synchronous", "NORMAL")
	return path + "?" + q.Encode()
}

// Open creates the parent directory (0700) if needed and opens path with the
// required pragmas, verifying each one is actually in effect before returning.
// A database file that SQLite cannot read (truncated, zeroed header, not a
// database at all) fails here rather than at the first query.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: Open requires a database path")
	}
	if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}

	db, err := sql.Open(driverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)

	s := &Store{db: db, path: path}
	if err := s.verifyPragmas(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ensurePrivateDir creates dir (and missing parents) with 0700. SQLite files
// hold user-curated profile data, so the directory is not world-readable
// (docs/spec/02 "Asset store contract": "Directory 0700").
func ensurePrivateDir(dir string) error {
	if dir == "" {
		dir = "."
	}
	_, statErr := os.Stat(dir)
	creating := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !creating {
		return fmt.Errorf("store: inspect %s: %w", dir, statErr)
	}
	if err := os.MkdirAll(dir, dirPerm); err != nil {
		return fmt.Errorf("store: create %s: %w", dir, err)
	}
	if creating {
		// MkdirAll masks the mode with the process umask; the 0700 requirement
		// is explicit, so set it once the directory exists.
		if err := os.Chmod(dir, dirPerm); err != nil {
			return fmt.Errorf("store: chmod %s: %w", dir, err)
		}
	}
	return nil
}

// verifyPragmas reads each required pragma back from one dedicated connection.
// Any pragma the server refuses to apply (a read-only file, a database opened
// by an older non-WAL build with an incompatible setting, a locked file) makes
// Open fail loudly instead of running with weaker durability than the spec
// requires.
func (s *Store) verifyPragmas(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: connect %s: %w", s.path, err)
	}
	defer conn.Close()

	checks := []struct {
		pragma string
		want   string
	}{
		{"foreign_keys", fmt.Sprint(requiredForeignKeys)},
		{"journal_mode", requiredJournalMode},
		{"busy_timeout", fmt.Sprint(requiredBusyTimeout)},
		{"synchronous", fmt.Sprint(requiredSynchronous)},
	}
	for _, c := range checks {
		var got string
		if err := conn.QueryRowContext(ctx, "PRAGMA "+c.pragma).Scan(&got); err != nil {
			return fmt.Errorf("store: read pragma %s on %s: %w", c.pragma, s.path, err)
		}
		if got != c.want {
			return fmt.Errorf("store: pragma %s on %s is %q, want %q", c.pragma, s.path, got, c.want)
		}
	}
	return nil
}

// Path returns the database file this store was opened from.
func (s *Store) Path() string { return s.path }

// SQLiteVersion reports the bundled SQLite library version, for diagnostics
// and evidence manifests.
func (s *Store) SQLiteVersion(ctx context.Context) (string, error) {
	var v string
	if err := s.db.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v); err != nil {
		return "", fmt.Errorf("store: sqlite_version: %w", err)
	}
	return v, nil
}

// Close releases the connection pool. Backup and Restore require the database
// to be closed first.
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return fmt.Errorf("store: close %s: %w", s.path, err)
	}
	return nil
}
