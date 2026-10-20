package store

// Transactional scaffolding for the repositories in this package.
//
// Why `BEGIN IMMEDIATE` instead of database/sql's BeginTx. A deferred
// transaction (what `sql.DB.BeginTx` issues) takes no lock until its first
// statement, so two concurrent writers can both "succeed" at beginning, read
// the same profile row, both decide their `If-Match` token is valid, and then
// one of them loses at COMMIT — where SQLite reports the loser as SQLITE_BUSY
// and the whole unit of work is gone with no useful error to show the client.
// docs/spec/08 names the error the API must return instead
// (`PROFILE_REVISION_CONFLICT`), and docs/spec/02 "Transaction workflows" makes
// publication a compare-and-swap, which is only honest if the compare and the
// swap happen while nobody else can interleave. BEGIN IMMEDIATE takes the write
// lock up front, so writers serialize on the busy timeout (5 s, store.go) and
// every transaction sees the committed result of the one before it. The
// consequence is stated plainly: one writer at a time per database, which is
// SQLite's own model and is plenty for a single-appliance server.
//
// Because `database/sql` only tracks transactions it opened itself, the lock is
// held by pinning one *sql.Conn for the whole transaction: every statement of a
// unit of work provably runs on the connection that holds the write lock, and
// nothing else can be handed that connection.

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// Typed errors the repositories return. The API layer maps these to wire codes
// (docs/spec/08 "Error codes"): ErrNotFound → 404 RESOURCE_NOT_FOUND,
// ErrConflict → 409, ErrRevisionConflict → 409 PROFILE_REVISION_CONFLICT,
// ErrImmutable → 409 (derive a revision instead). They wrap nothing else, so
// errors.Is is the contract and the message text is not.
var (
	// ErrNotFound means the row a reference names does not exist.
	ErrNotFound = errors.New("store: not found")
	// ErrConflict means the write contradicts a uniqueness or referential
	// constraint: a duplicate slug, a message whose source belongs to another
	// profile, an edition that is not where its parent says it is.
	ErrConflict = errors.New("store: conflict")
	// ErrImmutable means the target revision is published or archived, so the
	// edit is refused and a new revision has to be derived instead
	// (docs/spec/02 "Transaction workflows"; docs/spec/12 "no published edit
	// without valid revision token").
	ErrImmutable = errors.New("store: immutable")
	// ErrRevisionConflict means the caller's expected row version is not the
	// stored one: somebody else wrote first.
	ErrRevisionConflict = errors.New("store: revision conflict")
	// ErrNoSpace means SQLite reported SQLITE_FULL: the database file cannot
	// grow (disk full, or a PRAGMA max_page_count limit). The transaction was
	// rolled back and nothing was written. Callers must treat this as a
	// capacity problem they can act on — refuse the write, evict safe transient
	// data and retry — never as a server fault (docs/spec/13 "Disk full").
	ErrNoSpace = errors.New("store: no space")
	// ErrInvalidTransition means a status move the lifecycle forbids, e.g.
	// draft → published, which never goes through review (games.ProfileStatus.
	// CanTransition is the single source of that table).
	ErrInvalidTransition = errors.New("store: invalid status transition")
	// ErrUnpublishable is returned (wrapped inside *PublishError) when the
	// publication gate refuses a revision. It is a distinct sentinel from
	// ErrValidation in internal/games because it is a *set* of findings about
	// rows that are each individually well-formed — a message with an unknown
	// rights status is a valid row and an unpublishable profile.
	ErrUnpublishable = errors.New("store: profile is not publishable")
	// ErrInvalidCursor means a pagination cursor this store did not issue, or
	// which names a row shape the query cannot order by. docs/spec/08 makes
	// cursors opaque, so a client that mangles one is a 400 rather than an
	// internal error.
	ErrInvalidCursor = errors.New("store: invalid cursor")

	// The library lifecycle sentinels (ADR-0107, gap G14). Each one is a 409
	// with its own wire code at the API layer, so the caller can tell the four
	// refusals apart instead of recovering from a generic conflict.
	// ErrProfileAlreadyDeleted means a soft delete was asked for a revision
	// that carries the mark; the row exists (that is what retention is for),
	// so the answer is "already deleted", not "not found".
	ErrProfileAlreadyDeleted = errors.New("store: profile is already deleted")
	// ErrProfileHasPublicationHistory means the revision has a publish_actions
	// row — published, archived or even a gate refusal. 0004's RESTRICT on
	// publish_actions.profile_id exists precisely so such a revision cannot be
	// deleted out from under its own trail, and the soft delete obeys the same
	// rule instead of leaning on the foreign key's opaque error: the retirement
	// path for a shipped revision is archive, not delete.
	ErrProfileHasPublicationHistory = errors.New("store: profile has publication history")
	// ErrProfileHasDerivedRevisions means another revision descends from this
	// one (parent_revision); deleting a parent first would orphan the lineage
	// the 0001 composite key refuses to write.
	ErrProfileHasDerivedRevisions = errors.New("store: profile has derived revisions")
	// ErrProfileNotDeleted means a purge was asked for a revision that was
	// never soft-deleted: purging is the second half of a two-step act, and
	// the first step is the mark.
	ErrProfileNotDeleted = errors.New("store: profile is not deleted")
	// ErrRetentionNotElapsed means the soft-delete mark is inside its retention
	// window; the purge must wait until purge_after, when the mistake has had
	// its bounded time to be noticed.
	ErrRetentionNotElapsed = errors.New("store: retention window has not elapsed")
)

// Pagination bounds from docs/spec/08 "Conventions" ("pagination with cursor and
// hard upper bounds"). The caps exist so a client bug — or a hostile integration
// token — cannot ask the 4 GB appliance for the whole dialogue graph in one
// response.
const (
	// MaxListLimit bounds every cursor-paginated list.
	MaxListLimit = 200
	// MaxSearchLimit bounds message search, whose hits carry full rows.
	MaxSearchLimit = 100
	// DefaultListLimit applies when a caller passes a non-positive limit.
	DefaultListLimit = 50
	// minTrigramQuery is the shortest phrase the FTS5 trigram tokenizer can
	// match; shorter text takes the LIKE prefix path (docs/spec/02 "SQLite
	// schema principles": "Substrng queries need at least three characters,
	// which the app enforces, not the schema").
	minTrigramQuery = 3
)

// txKey is the context key under which an open Tx is carried so nested calls
// join it instead of asking the pool for a second connection. An unexported
// empty struct cannot be collided with by another package.
type txKey struct{}

// Tx is an open transaction on one pinned connection. It is used by the
// repository methods in this package; callers get one only through
// Store.InTx.
//
// A Tx is NOT safe for concurrent use as a unit of work — SQLite serializes
// statements on one connection, so concurrent calls interleave inside the same
// transaction rather than running in parallel, and an error from one aborts the
// other's intent. Treat a Tx as belonging to the goroutine that opened it.
type Tx struct {
	conn *sql.Conn
	// savepoints numbers nested units so a failing inner unit can be rolled
	// back without discarding the outer one's completed work.
	savepoints atomic.Int32
}

// InTx runs fn inside one transaction and commits if fn returns nil.
//
// Two behaviours matter to callers:
//
//  1. The transaction is started with BEGIN IMMEDIATE, so a concurrent writer
//     blocks here (up to the busy timeout) instead of interleaving. A write that
//     reads a row version and acts on it is therefore a genuine compare-and-swap.
//  2. If ctx already carries an open Tx — i.e. InTx is called from inside
//     another InTx — fn joins that transaction inside a SAVEPOINT and the outer
//     call still decides commit or rollback. Joining rather than opening a second
//     connection is deliberate: the pool is capped (store.go maxOpenConns), and a
//     nested call that took a second connection would deadlock against an
//     exclusively held write lock instead of doing useful work.
//
// fn must not run network calls: it holds the database's single write lock, so a
// provider round trip inside a transaction stalls every other writer on the
// appliance (AGENTS.md "Keep runtime predictable"). Nothing in this package makes
// a network call, and nothing that takes a *Tx may start one.
func (s *Store) InTx(ctx context.Context, fn func(*Tx) error) (err error) {
	if fn == nil {
		return errors.New("store: InTx requires a function")
	}
	if outer := TxFrom(ctx); outer != nil {
		return outer.nest(ctx, fn)
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		// SQLITE_BUSY here means the busy timeout expired while another writer
		// held the lock. It is reported as itself: it is retryable, and mapping
		// it to ErrConflict would tell a client its data is wrong when the real
		// answer is "try again".
		return fmt.Errorf("store: begin immediate: %w", err)
	}

	tx := &Tx{conn: conn}
	committed := false
	defer func() {
		if committed {
			return
		}
		// The rollback must run even when ctx is already canceled — a canceled
		// context is the common reason a transaction is being abandoned, and an
		// abandoned transaction would leave the write lock held until this
		// connection is closed.
		if _, rbErr := conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK"); rbErr != nil {
			if err == nil {
				err = fmt.Errorf("store: rollback after panic: %w", rbErr)
			}
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	committed = true
	return nil
}

// TxFrom returns the transaction carried by ctx, if any. Repository methods use
// it to run on the caller's transaction when one is open and on their own
// otherwise, which is what makes a multi-step workflow (import → publish)
// composable without duplicating the API.
func TxFrom(ctx context.Context) *Tx {
	tx, _ := ctx.Value(txKey{}).(*Tx)
	return tx
}

// WithTx returns ctx carrying tx, so a nested Store.InTx call joins it.
func WithTx(ctx context.Context, tx *Tx) context.Context {
	return context.WithValue(ctx, txKey{}, tx)
}

// nest runs fn inside a SAVEPOINT of this transaction.
func (t *Tx) nest(ctx context.Context, fn func(*Tx) error) error {
	name := fmt.Sprintf("gn_sp_%d", t.savepoints.Add(1))
	if _, err := t.conn.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return fmt.Errorf("store: savepoint: %w", err)
	}
	if err := fn(t); err != nil {
		// Rolling back to the savepoint undoes only this unit; the outer
		// transaction is still usable, which is what lets a caller try a
		// different write after a rejected one.
		if _, rbErr := t.conn.ExecContext(context.WithoutCancel(ctx), "ROLLBACK TO "+name); rbErr != nil {
			return errors.Join(err, fmt.Errorf("store: release savepoint %s: %w", name, rbErr))
		}
		if _, relErr := t.conn.ExecContext(context.WithoutCancel(ctx), "RELEASE "+name); relErr != nil {
			return errors.Join(err, fmt.Errorf("store: release savepoint %s: %w", name, relErr))
		}
		return err
	}
	if _, err := t.conn.ExecContext(ctx, "RELEASE "+name); err != nil {
		return fmt.Errorf("store: release savepoint %s: %w", name, err)
	}
	return nil
}

// Context returns a context carrying this transaction, for calling the exported
// Store methods from inside an InTx callback without opening a second
// transaction.
func (t *Tx) Context(ctx context.Context) context.Context { return WithTx(ctx, t) }

// Exec runs a statement inside the transaction.
func (t *Tx) Exec(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return t.conn.ExecContext(ctx, query, args...)
}

// QueryRow runs a row-returning query inside the transaction.
func (t *Tx) QueryRow(ctx context.Context, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// Query runs a query inside the transaction. The caller must close the rows.
func (t *Tx) Query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return t.conn.QueryContext(ctx, query, args...)
}

// exec is the write path used by every repository mutation: it maps SQLite's
// constraint codes onto the typed errors before a caller can mistake them for a
// server fault.
func (t *Tx) exec(ctx context.Context, op, query string, args ...any) (sql.Result, error) {
	res, err := t.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return nil, mapWriteError(op, err)
	}
	return res, nil
}

// queryRow is the read path inside a transaction: sql.ErrNoRows becomes
// ErrNotFound, because "no such profile" is an answer, not a failure.
func (t *Tx) queryRow(ctx context.Context, op, query string, args ...any) *sql.Row {
	return t.conn.QueryRowContext(ctx, query, args...)
}

// query is the row-set read path inside a transaction.
func (t *Tx) query(ctx context.Context, op, query string, args ...any) (*sql.Rows, error) {
	rows, err := t.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapReadError(op, err)
	}
	return rows, nil
}

// mapWriteError turns a driver error into one of the typed errors above.
//
// The dispatch uses SQLite's *extended* result code, which the driver reports
// through *sqlite.Error: the primary code says only SQLITE_CONSTRAINT (19) and
// would let a CHECK violation be reported to a client as "your row collides with
// an existing one", which is false and unhelpful. Codes are masked with 0xff
// before comparison so an extended value never misses its primary class.
func mapWriteError(op string, err error) error {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	code, primary := se.Code(), se.Code()&0xff
	if primary == sqlite3.SQLITE_FULL {
		// A full disk is not the caller's data fault and not a server bug: it
		// is a capacity condition with a well-defined recovery (evict transient
		// cache, then retry), so it gets its own typed error instead of leaking
		// as an opaque driver message.
		return fmt.Errorf("store: %s: %w: %s", op, ErrNoSpace, se.Error())
	}
	if primary != sqlite3.SQLITE_CONSTRAINT {
		return fmt.Errorf("store: %s: %w", op, err)
	}
	detail := se.Error()
	switch code {
	case sqlite3.SQLITE_CONSTRAINT_UNIQUE,
		sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY,
		sqlite3.SQLITE_CONSTRAINT_ROWID,
		// A RESTRICT violation is reported as SQLITE_CONSTRAINT_TRIGGER: SQLite
		// implements RESTRICT as an implicit trigger. It is still "another row
		// refers to this one", i.e. a conflict, not a missing row.
		sqlite3.SQLITE_CONSTRAINT_TRIGGER:
		return fmt.Errorf("store: %s: %w: %s", op, ErrConflict, detail)
	case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
		// docs/spec/02 keeps referential integrity as the backstop behind the
		// repositories' own checks, so this path is reachable only by a race or a
		// caller that bypassed Validate. It is a conflict with existing data, not
		// a server bug, and is reported as one.
		return fmt.Errorf("store: %s: %w: foreign key: %s", op, ErrConflict, detail)
	default:
		// CHECK and NOT NULL violations are left unmapped on purpose: every one
		// of them corresponds to a rule games.Validate already enforces, so
		// reaching one means Go and the schema disagree. That is a defect and it
		// must look like a defect (500), not like a client error (409).
		return fmt.Errorf("store: %s: schema constraint violated, this indicates a bug: %w", op, err)
	}
}

// mapReadError labels read failures. "No such row" becomes ErrNotFound — a
// missing profile is an answer, not a server fault, and docs/spec/08 wants a 404
// rather than a 500 for it. A read that fails because the database is locked is
// left as the driver's error: it is retryable and must not be dressed up as
// "not found".
func mapReadError(op string, err error) error {
	var se *sqlite.Error
	if errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_FULL {
		// A read that needs the file to grow (WAL growth) meets the same
		// capacity wall as a write; report it the same way.
		return fmt.Errorf("store: no space: %w: %s", ErrNoSpace, se.Error())
	}
	switch {
	case err == nil:
		return nil
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("store: %s: %w", op, ErrNotFound)
	default:
		return fmt.Errorf("store: %s: %w", op, err)
	}
}

// ── read/write plumbing shared by every repository ─────────────────────────

// scanner is the shape of both *sql.Row and *sql.Rows, so one scan function
// serves a single-row get and a paged list without duplicating column order in
// two places (which is how a get and a list drift apart and a UI starts showing
// different text for the same message).
type scanner interface {
	Scan(dest ...any) error
}

// queryable is the read surface a repository body needs. A read runs on a pooled
// connection when it stands alone and on the caller's transaction when one is
// open, and the code that reads is identical either way.
type queryable interface {
	queryRow(ctx context.Context, op, query string, args ...any) *sql.Row
	query(ctx context.Context, op, query string, args ...any) (*sql.Rows, error)
}

// dbReader adapts the pool to queryable for reads outside a transaction.
type dbReader struct{ db *sql.DB }

func (r dbReader) queryRow(ctx context.Context, _, query string, args ...any) *sql.Row {
	return r.db.QueryRowContext(ctx, query, args...)
}

func (r dbReader) query(ctx context.Context, op, query string, args ...any) (*sql.Rows, error) {
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, mapReadError(op, err)
	}
	return rows, nil
}

// inRead runs fn with a reader, joining an open transaction when ctx carries one
// so a read inside a workflow sees that workflow's own uncommitted writes.
func (s *Store) inRead(ctx context.Context, fn func(context.Context, queryable) error) error {
	if tx := TxFrom(ctx); tx != nil {
		return fn(ctx, tx)
	}
	return fn(ctx, dbReader{s.db})
}

// inWrite runs fn inside a transaction and puts it on ctx, so a Store method
// called from the body of another Store method joins the same transaction instead
// of waiting forever for a write lock this goroutine already holds. The pool is
// capped at maxOpenConns, so without this the obvious composition — Publish
// calling PutMessages — would deadlock rather than work.
func (s *Store) inWrite(ctx context.Context, fn func(context.Context, *Tx) error) error {
	return s.InTx(ctx, func(tx *Tx) error { return fn(WithTx(ctx, tx), tx) })
}

// notFound reports a missing row for op.
func notFound(op, what string) error {
	return fmt.Errorf("store: %s: %s: %w", op, what, ErrNotFound)
}

// immutable reports a refused edit of a frozen revision.
func immutable(op string, status games.ProfileStatus, id string) error {
	return fmt.Errorf("store: %s: profile %s is %s and cannot be edited, derive a new revision with NewRevision: %w",
		op, id, status, ErrImmutable)
}

// revisionConflict reports a stale If-Match token.
func revisionConflict(op, id string, want, have int64) error {
	return fmt.Errorf("store: %s: profile %s row version is %d, the caller expected %d: %w",
		op, id, have, want, ErrRevisionConflict)
}

// ── cursor pagination ───────────────────────────────────────────────────────

// Cursor is an opaque pagination token. docs/spec/08 requires cursors to be
// opaque, so the payload (the last id seen) is base64url-encoded and never
// documented as a stable format: a client that parses it is relying on something
// this package may change.
func encodeCursor(id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodeCursor resolves a cursor to the row id to resume after. An empty cursor
// means "start at the beginning". Anything else must decode to a well-formed
// identifier, because the comparison is `id > ?` and a garbage cursor would
// otherwise silently return an arbitrary page.
func decodeCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", fmt.Errorf("store: decode cursor: %w", ErrInvalidCursor)
	}
	id := string(raw)
	if !games.ValidID(id) {
		return "", fmt.Errorf("store: decode cursor: %w", ErrInvalidCursor)
	}
	return id, nil
}

// clampLimit applies the [1, max] bounds and the default for a non-positive
// request, per docs/spec/08's "hard upper bounds".
func clampLimit(limit, max int) int {
	switch {
	case limit <= 0:
		return min(DefaultListLimit, max)
	case limit > max:
		return max
	default:
		return limit
	}
}

// ── value helpers ───────────────────────────────────────────────────────────

// nowStamp is the single timestamp formatter for writes. UTC and RFC 3339 are
// what the schema's strftime CHECKs and games.TimestampFormat both require, and
// one helper keeps a local-time zone with a non-zero offset (which SQLite sorts
// chronologically wrong) out of the database entirely.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// nullStr maps the empty Go string onto SQL NULL. Every nullable TEXT column is
// empty in the domain type, and writing ” would make "no source" and "a source
// whose id is the empty string" indistinguishable — and the partial UNIQUE index
// on messages(source_message_id IS NOT NULL) depends on real NULL.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// orDefault renders an empty JSON column as the schema's own default, so a
// caller that never touched the field writes the same bytes as one that sent []
// explicitly.
func orDefault(raw json.RawMessage, fallback string) string {
	if trimmed := strings.TrimSpace(string(raw)); trimmed != "" && json.Valid(raw) {
		return trimmed
	}
	return fallback
}

// nullFloat and nullInt render a nil *T as SQL NULL for the nullable numeric
// columns (regions.min_aspect, messages at the app layer, …).
func nullFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}

func nullInt(v *int) any {
	if v == nil {
		return nil
	}
	return *v
}

// optText reads a nullable TEXT column into a Go string.
func optText(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case []byte:
		return string(t)
	}
	return fmt.Sprint(v)
}

// optFloat reads a nullable REAL into a *float64, nil for absent.
func optFloat(v sql.NullFloat64) *float64 {
	if !v.Valid {
		return nil
	}
	out := v.Float64
	return &out
}

// optInt reads a nullable INTEGER into a *int, nil for absent.
func optInt(v sql.NullInt64) *int {
	if !v.Valid {
		return nil
	}
	out := int(v.Int64)
	return &out
}
