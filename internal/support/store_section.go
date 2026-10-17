package support

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"

	"github.com/dbirks/game-narrator/internal/store"
)

// sqliteDriverName is the name modernc.org/sqlite registers itself under
// (the same driver and open style as internal/store, ADR 0008).
const sqliteDriverName = "sqlite"

// queryOnlyDSN opens a database strictly read-only: a support bundle must
// never write to the user's catalogue, not even to take a write lock.
func queryOnlyDSN(path string) string {
	return path + "?_query_only=1&_busy_timeout=5000"
}

// migrationRow is one applied migration: identity and timestamp only.
type migrationRow struct {
	Version   int    `json:"version"`
	Name      string `json:"name"`
	AppliedAt string `json:"applied_at"`
}

// storeDoc is the store section of the bundle: schema version, the applied
// migration list and per-table row counts. Counts only — docs/spec/13
// forbids shipping catalogue content, and the database itself never enters
// the archive.
type storeDoc struct {
	SchemaVersion int              `json:"schema_version"`
	Migrations    []migrationRow   `json:"migrations"`
	TableCounts   map[string]int64 `json:"table_counts"`
	Note          string           `json:"note"`
}

// buildStore opens the catalogue strictly read-only and reads just the
// metadata a support engineer needs: which schema is in place, which
// migrations were applied, and how large each table is.
func buildStore(ctx context.Context, dataDir string) ([]byte, error) {
	dbPath := store.DBPath(dataDir)
	if _, err := os.Stat(dbPath); err != nil {
		return nil, fmt.Errorf("database %s not found: run 'game-narrator migrate --data-dir DIR' first", filepath.Base(dbPath))
	}
	db, err := sql.Open(sqliteDriverName, queryOnlyDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("support: open store read-only: %w", err)
	}
	defer db.Close()

	var hasMigrations int
	if err := db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'").Scan(&hasMigrations); err != nil {
		return nil, fmt.Errorf("support: inspect schema_migrations: %w", err)
	}
	if hasMigrations == 0 {
		return nil, fmt.Errorf("database is not migrated: run 'game-narrator migrate --data-dir DIR' first")
	}

	var version int
	if err := db.QueryRowContext(ctx, "SELECT coalesce(max(version), 0) FROM schema_migrations").Scan(&version); err != nil {
		return nil, fmt.Errorf("support: read schema version: %w", err)
	}

	rows, err := db.QueryContext(ctx, "SELECT version, name, applied_at FROM schema_migrations ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("support: read applied migrations: %w", err)
	}
	migs := make([]migrationRow, 0, 16)
	for rows.Next() {
		var m migrationRow
		if err := rows.Scan(&m.Version, &m.Name, &m.AppliedAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("support: scan migration row: %w", err)
		}
		migs = append(migs, m)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("support: read applied migrations: %w", err)
	}
	rows.Close()

	tables, err := db.QueryContext(ctx,
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("support: list tables: %w", err)
	}
	counts := make(map[string]int64)
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			tables.Close()
			return nil, fmt.Errorf("support: scan table name: %w", err)
		}
		var n int64
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM "`+strings.ReplaceAll(name, `"`, `""`)+`"`).Scan(&n); err != nil {
			tables.Close()
			return nil, fmt.Errorf("support: count rows in %s: %w", name, err)
		}
		counts[name] = n
	}
	if err := tables.Err(); err != nil {
		tables.Close()
		return nil, fmt.Errorf("support: list tables: %w", err)
	}
	tables.Close()

	doc := storeDoc{
		SchemaVersion: version,
		Migrations:    migs,
		TableCounts:   counts,
		Note:          "metadata only: schema version, applied migrations and per-table row counts. The database and its rows are never copied into a bundle (docs/spec/13).",
	}
	return marshalDoc(doc)
}
