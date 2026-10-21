package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationsDir is the embedded path holding the numbered .sql files.
const migrationsDir = "migrations"

// migrationFilePattern is the mandatory file naming: four digits, an
// underscore, a slug, ".sql".
var migrationFilePattern = regexp.MustCompile(`^([0-9]{4})_([a-z0-9_]+)\.sql$`)

// schemaMigrationsTable records which migrations this database has applied.
// The checksum makes the applied history tamper-evident: an embedded file that
// no longer hashes to what a database recorded means the binary and the data
// disagree about what "version N" means, which is unrecoverable guesswork
// rather than a migration.
const schemaMigrationsTable = "schema_migrations"

const schemaMigrationsDDL = `CREATE TABLE IF NOT EXISTS ` + schemaMigrationsTable + ` (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	checksum   TEXT NOT NULL,
	applied_at TEXT NOT NULL
)`

// ErrChecksumMismatch is returned when an already-applied migration's embedded
// file no longer hashes to the checksum recorded in the database.
var ErrChecksumMismatch = errors.New("checksum mismatch")

// loadMigrations is the embedded-migration loader Migrate uses. It is a seam:
// the embedded set is dense by construction, so the wrapped error path (a
// loader failure naming the database, issue #31) is exercised through this
// pointer rather than by corrupting the build's own migrations.
var loadMigrations = Migrations

// Migration is one embedded, numbered SQL file.
type Migration struct {
	Version int
	// Name is the file base name, e.g. "0001_init.sql".
	Name string
	// SQL is the verbatim file content.
	SQL string
	// Checksum is the lowercase hex SHA-256 of SQL.
	Checksum string
}

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, migrationsDir)
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q does not match NNNN_name.sql", e.Name())
		}
		version, err := strconv.Atoi(m[1])
		if err != nil {
			return nil, fmt.Errorf("migration file %q has an unreadable version: %w", e.Name(), err)
		}
		raw, err := migrationFS.ReadFile(migrationsDir + "/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, Migration{
			Version:  version,
			Name:     e.Name(),
			SQL:      string(raw),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	// The version sequence must be dense and start at 1. A missing number is
	// always an accident (a forgotten file, a bad rebase) and would silently
	// change what "schema version 7" means between builds.
	for i, mig := range out {
		if mig.Version != i+1 {
			if i == 0 {
				return nil, fmt.Errorf("first embedded migration is version %d (%s), want 1", mig.Version, mig.Name)
			}
			return nil, fmt.Errorf("embedded migration sequence has a gap: %s (version %d) follows version %d",
				mig.Name, mig.Version, out[i-1].Version)
		}
	}
	return out, nil
}

// LatestSchemaVersion returns the version a fully migrated database is at — the
// highest embedded migration version, or 0 if this build embeds none. It exists so
// callers (the CLI's own reporting, and every test that checks a migration ran)
// can state the requirement this build actually ships instead of hardcoding a
// number that changes with every migration file and then reports a correct
// database as corrupt.
func LatestSchemaVersion() int {
	migrations, err := Migrations()
	if err != nil || len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].Version
}

// MigrationNames returns the embedded migration file names in version order.
func MigrationNames() []string {
	migrations, err := Migrations()
	if err != nil {
		return nil
	}
	out := make([]string, len(migrations))
	for i, mig := range migrations {
		out[i] = mig.Name
	}
	return out
}

// appliedMigration is a schema_migrations row.
type appliedMigration struct {
	version  int
	name     string
	checksum string
}

// migrationError names the database file a migration failure concerns, so a
// process that opened the wrong data directory says which database it was
// (issue #31: neither the startup log nor the error named the file). The %w
// keeps sentinels such as ErrChecksumMismatch matchable with errors.Is.
func (s *Store) migrationError(err error) error {
	return fmt.Errorf("store: %s: %w", s.path, err)
}

// Migrate applies every embedded migration the database has not yet applied,
// each in its own transaction, and returns their file names in the order they
// were applied. Running it again is a no-op.
//
// It refuses to do anything at all when the recorded history and the embedded
// files disagree (ErrChecksumMismatch, or a recorded version this binary has
// no file for): continuing would build assumptions on a schema the code has
// never seen.
func (s *Store) Migrate(ctx context.Context) ([]string, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, s.migrationError(err)
	}

	if _, err := s.db.ExecContext(ctx, schemaMigrationsDDL); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", schemaMigrationsTable, err)
	}
	applied, err := loadApplied(ctx, s.db)
	if err != nil {
		return nil, err
	}

	embedded := make(map[int]Migration, len(migrations))
	for _, mig := range migrations {
		embedded[mig.Version] = mig
	}
	for _, rec := range applied {
		if _, ok := embedded[rec.version]; !ok {
			return nil, s.migrationError(fmt.Errorf("database records schema version %d (%s) but this build has no such migration (max %d); refusing to continue with an older binary",
				rec.version, rec.name, migrations[len(migrations)-1].Version))
		}
	}

	var didApply []string
	for _, mig := range migrations {
		if rec, ok := applied[mig.Version]; ok {
			if rec.checksum != mig.Checksum {
				return didApply, s.migrationError(fmt.Errorf("migration %d (%s): %w: database recorded %s, embedded file hashes %s; applied migrations are immutable, restore a backup or repair the build",
					mig.Version, mig.Name, ErrChecksumMismatch, rec.checksum, mig.Checksum))
			}
			continue
		}
		if err := applyMigration(ctx, s.db, mig); err != nil {
			return didApply, s.migrationError(err)
		}
		didApply = append(didApply, mig.Name)
	}
	return didApply, nil
}

func loadApplied(ctx context.Context, db *sql.DB) (map[int]appliedMigration, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT version, name, checksum FROM "+schemaMigrationsTable+" ORDER BY version")
	if err != nil {
		return nil, fmt.Errorf("store: read %s: %w", schemaMigrationsTable, err)
	}
	defer rows.Close()

	out := make(map[int]appliedMigration)
	for rows.Next() {
		var rec appliedMigration
		if err := rows.Scan(&rec.version, &rec.name, &rec.checksum); err != nil {
			return nil, fmt.Errorf("store: scan %s: %w", schemaMigrationsTable, err)
		}
		if _, dup := out[rec.version]; dup {
			return nil, fmt.Errorf("store: %s lists version %d twice; the migration history is corrupt",
				schemaMigrationsTable, rec.version)
		}
		out[rec.version] = rec
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: read %s: %w", schemaMigrationsTable, err)
	}
	return out, nil
}

// applyMigration runs one file in a single transaction: either every statement
// and its bookkeeping row land, or nothing does.
func applyMigration(ctx context.Context, db *sql.DB, mig Migration) error {
	statements, err := splitSQL(mig.SQL)
	if err != nil {
		return fmt.Errorf("store: migration %s: %w", mig.Name, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration %s: %w", mig.Name, err)
	}
	defer func() {
		// Rollback after Commit is a no-op; this is the interrupted-migration
		// path (docs/spec/02 "Migration and backup acceptance").
		_ = tx.Rollback()
	}()

	for _, stmt := range statements {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("store: migration %s: %w", mig.Name, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO "+schemaMigrationsTable+" (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)",
		mig.Version, mig.Name, mig.Checksum, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return fmt.Errorf("store: migration %s: record version: %w", mig.Name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migration %s: %w", mig.Name, err)
	}
	return nil
}

// SchemaVersion returns the highest applied migration version, or 0 for a
// database that has not been migrated yet.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var exists int
	if err := s.db.QueryRowContext(ctx,
		"SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?",
		schemaMigrationsTable).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: look for %s: %w", schemaMigrationsTable, err)
	}
	if exists == 0 {
		return 0, nil
	}

	var version sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		"SELECT max(version) FROM "+schemaMigrationsTable).Scan(&version); err != nil {
		return 0, fmt.Errorf("store: read schema version: %w", err)
	}
	if !version.Valid {
		return 0, nil
	}
	return int(version.Int64), nil
}

// splitSQL cuts a migration file into individual statements on semicolons that
// are not inside a string, a quoted identifier or a comment.
//
// CREATE TRIGGER is the one compound statement SQLite allows in a migration and
// it carries its own semicolons ("BEGIN stmt; stmt; END;"), so its body is
// tracked: while a trigger body is open a semicolon separates the body's
// statements and does not terminate the CREATE TRIGGER. ADR-0008 flagged this
// as the case that would require extending the splitter; 0002_content.sql needs
// the FTS5 sync triggers, so that is what this does. Recognising the body needs
// a keyword at a time, not a byte at a time, hence the word scanner below: it
// only ever answers "is this statement a trigger, and is its END here yet?" and
// never tries to parse SQL.
//
// Remaining limitation, deliberately narrow: only BEGIN/END (with CASE/END
// nested inside the body counted separately) are tracked. A future compound
// statement with another nesting keyword needs the same treatment, and
// TestSplitSQLRejectsAnUnterminatedTriggerBody shows a trigger whose END is
// missing fails loudly (the whole rest of the file becomes one invalid
// statement, the migration transaction rolls back) instead of half-applying.
func splitSQL(script string) ([]string, error) {
	const (
		stateCode = iota
		stateLineComment
		stateBlockComment
		stateString
		stateQuotedIdent
		stateBacktick
	)

	var (
		out   []string
		cur   strings.Builder
		state = stateCode
		code  bool
	)

	// Trigger-body state, reset with every statement.
	var (
		// leadWord is how far the current statement's leading keyword run has
		// got towards "CREATE [TEMP|TEMPORARY] TRIGGER": 0 = nothing seen, 1 =
		// CREATE, 2 = CREATE TEMP[ORARY], 3 = trigger confirmed, -1 = some other
		// statement.
		leadWord  int
		bodyOpen  bool
		caseDepth int
		word      []byte
	)
	// endWord feeds one completed keyword to the trigger tracker.
	endWord := func() {
		if len(word) == 0 {
			return
		}
		w := strings.ToUpper(string(word))
		word = word[:0]

		switch {
		case bodyOpen:
			// Inside a body, END closes a CASE expression first; the END that
			// does not is the end of the trigger itself.
			switch w {
			case "CASE":
				caseDepth++
			case "END":
				if caseDepth > 0 {
					caseDepth--
				} else {
					bodyOpen = false
				}
			}
		case leadWord == 3:
			// Confirmed trigger, body not yet open: BEGIN starts it.
			if w == "BEGIN" {
				bodyOpen = true
			}
		case leadWord == 0:
			if w == "CREATE" {
				leadWord = 1
			} else {
				leadWord = -1
			}
		case leadWord == 1:
			switch w {
			case "TRIGGER":
				leadWord = 3
			case "TEMP", "TEMPORARY":
				leadWord = 2
			default:
				leadWord = -1
			}
		case leadWord == 2:
			if w == "TRIGGER" {
				leadWord = 3
			} else {
				leadWord = -1
			}
		}
	}

	flush := func() {
		stmt := strings.TrimSpace(cur.String())
		if code && stmt != "" {
			out = append(out, stmt)
		}
		cur.Reset()
		code = false
		leadWord, bodyOpen, caseDepth = 0, false, 0
	}

	for i := 0; i < len(script); i++ {
		c := script[i]
		var next byte
		if i+1 < len(script) {
			next = script[i+1]
		}

		switch state {
		case stateLineComment:
			cur.WriteByte(c)
			if c == '\n' {
				state = stateCode
			}
			continue
		case stateBlockComment:
			cur.WriteByte(c)
			if c == '*' && next == '/' {
				cur.WriteByte(next)
				i++
				state = stateCode
			}
			continue
		case stateString, stateQuotedIdent, stateBacktick:
			closer := byte('\'')
			switch state {
			case stateQuotedIdent:
				closer = '"'
			case stateBacktick:
				closer = '`'
			}
			cur.WriteByte(c)
			if c == closer {
				// Doubled closer is an escaped literal, not the end.
				if next == closer {
					cur.WriteByte(next)
					i++
					continue
				}
				state = stateCode
			}
			continue
		}

		// stateCode
		switch {
		case c == '-' && next == '-':
			endWord()
			cur.WriteByte(c)
			cur.WriteByte(next)
			i++
			state = stateLineComment
		case c == '/' && next == '*':
			endWord()
			cur.WriteByte(c)
			cur.WriteByte(next)
			i++
			state = stateBlockComment
		case c == '\'':
			endWord()
			code = true
			cur.WriteByte(c)
			state = stateString
		case c == '"':
			endWord()
			code = true
			cur.WriteByte(c)
			state = stateQuotedIdent
		case c == '`':
			endWord()
			code = true
			cur.WriteByte(c)
			state = stateBacktick
		case c == ';':
			endWord()
			if bodyOpen {
				// A semicolon between statements inside a trigger body. The
				// CREATE TRIGGER is not finished until its END.
				code = true
				cur.WriteByte(c)
				continue
			}
			flush()
		default:
			if !isSpaceByte(c) {
				code = true
			}
			if isWordByte(c) {
				word = append(word, c)
			} else {
				endWord()
			}
			cur.WriteByte(c)
		}
	}

	switch state {
	case stateString, stateQuotedIdent, stateBacktick:
		return nil, errors.New("unterminated quoted literal")
	case stateBlockComment:
		return nil, errors.New("unterminated block comment")
	}
	endWord()
	flush()
	if len(out) == 0 {
		return nil, errors.New("migration contains no statements")
	}
	return out, nil
}

// isWordByte reports whether c can appear in a bare SQL keyword or identifier.
// SQLite also allows '$' and '#' inside identifiers, so they count as word bytes
// here: treating them as separators could otherwise split a keyword-looking
// identifier into pieces that match BEGIN or END by accident.
func isWordByte(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		return true
	case c == '_' || c == '$' || c == '#':
		return true
	}
	return false
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', '\f', '\v':
		return true
	}
	return false
}
