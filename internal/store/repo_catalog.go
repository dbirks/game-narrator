package store

// Catalog repository: games and editions (docs/spec/02 "Entity catalog" tables
// `games` and `editions`).
//
// These two tables are the only ones with no profile revision behind them, so
// they carry no row version: docs/spec/08 puts ETag/If-Match on *profile
// revisions*, and a game's title or an edition's locale is metadata an operator
// may correct in place. Losing a concurrent edit here is a nuisance the developer
// UI shows; losing one on curated dialogue content is a corrupted published
// revision, which is why the versioning starts at profiles.

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/dbirks/game-narrator/internal/games"
)

// gameColumns is the read/write column list, in the order scanGameRow expects.
const gameColumns = "id, slug, title, description, platform, user_tags, created_at"

// editionColumns likewise. The published revision of an edition is not folded in:
// it is per-edition state answered by PublishedProfile, and games.Edition has no
// field for a pointer this schema does not store (see PublishedProfile for why the
// status column is the single representation of it).
const editionColumns = "id, game_id, region, language, revision, source_identity, display_aspect, locale, compatibility_notes"

// CreateGame inserts a title family. A repeated slug is ErrConflict rather than
// a silent no-op: games.slug is the public identity in /api/v1/games/:slug paths,
// so two callers asking for the same slug means a collision to resolve, not an
// idempotent retry — an idempotent create carries the same id, which is what
// Idempotency-Key is for at the API layer.
func (s *Store) CreateGame(ctx context.Context, g games.Game) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		return tx.createGame(ctx, g)
	})
}

func (tx *Tx) createGame(ctx context.Context, g games.Game) error {
	const op = "create game"
	if err := g.Validate(); err != nil {
		return err
	}
	_, err := tx.exec(ctx, op,
		`INSERT INTO games (id, slug, title, description, platform, user_tags, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		g.ID, g.Slug, g.Title, g.Description, g.Platform, orDefault(g.UserTags, "[]"), g.CreatedAt)
	return err
}

// GetGame loads one title family by id.
func (s *Store) GetGame(ctx context.Context, id string) (games.Game, error) {
	var out games.Game
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = scanGameRow(q.queryRow(ctx, opGetGame,
			"SELECT "+gameColumns+" FROM games WHERE id = ?", id), opGetGame)
		return err
	})
	return out, err
}

// Read-path operation names. One constant per query keeps a mislabelled error —
// which sends an operator to the wrong table — from being introduced by
// copy-paste.
const (
	opGetGame        = "get game"
	opGetGameBySlug  = "get game by slug"
	opListGames      = "list games"
	opGetEdition     = "get edition"
	opListEditions   = "list editions"
	opGetProfile     = "get profile"
	opListMessages   = "list messages"
	opGetMessage     = "get message"
	opListPages      = "list message pages"
	opSearchMessages = "search messages"
)

// GetGameBySlug resolves the slug the web UI puts in a URL. docs/spec/08's
// /games/:id accepts either a ULID or a slug, so the handler tries id first and
// falls back here; both report a miss as ErrNotFound so a client cannot tell
// which lookup answered.
func (s *Store) GetGameBySlug(ctx context.Context, slug string) (games.Game, error) {
	var out games.Game
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = scanGameRow(q.queryRow(ctx, opGetGameBySlug,
			"SELECT "+gameColumns+" FROM games WHERE slug = ?", slug), opGetGameBySlug)
		return err
	})
	return out, err
}

// ListGames returns one page of title families in id order plus the cursor for
// the next page (empty when the walk is finished).
//
// Ordering by id rather than created_at: ids are ULIDs, so they encode creation
// time (internal/games/ids.go) and are unique — a keyset pagination cursor built
// on a second-resolution timestamp would either skip or repeat a row for the two
// titles created in the same second, which is exactly the "no duplicates or gaps"
// property docs/spec/12 requires of pagination.
func (s *Store) ListGames(ctx context.Context, cursor string, limit int) ([]games.Game, string, error) {
	limit = clampLimit(limit, MaxListLimit)
	after, err := decodeCursor(cursor)
	if err != nil {
		return nil, "", err
	}

	var (
		out  []games.Game
		next string
	)
	err = s.inRead(ctx, func(ctx context.Context, q queryable) error {
		// One row more than the page: if it comes back there is a next page, and
		// its predecessor is the cursor. Cheaper and race-free compared to a
		// COUNT(*) over a table concurrent writers are adding to.
		rows, err := q.query(ctx, opListGames,
			"SELECT "+gameColumns+" FROM games WHERE id > ? ORDER BY id LIMIT ?", after, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()

		for len(out) <= limit && rows.Next() {
			g, err := scanGameRow(rows, opListGames)
			if err != nil {
				return err
			}
			out = append(out, g)
		}
		if len(out) > limit {
			next = encodeCursor(out[limit-1].ID)
			out = out[:limit]
		}
		return rows.Err()
	})
	if err != nil {
		return nil, "", err
	}
	return out, next, nil
}

// UpdateGame replaces a title family's metadata by id. A missing id is
// ErrNotFound: an UPDATE that matched no rows is not a success, and reporting it
// as one is how a UI ends up showing a save that never happened.
func (s *Store) UpdateGame(ctx context.Context, g games.Game) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "update game"
		if err := g.Validate(); err != nil {
			return err
		}
		res, err := tx.exec(ctx, op,
			`UPDATE games SET slug = ?, title = ?, description = ?, platform = ?, user_tags = ?
			 WHERE id = ?`,
			g.Slug, g.Title, g.Description, g.Platform, orDefault(g.UserTags, "[]"), g.ID)
		if err != nil {
			return err
		}
		return existsAfterWrite(ctx, tx, op, res, "games", g.ID)
	})
}

// CreateEdition adds a region/language/revision binding under a game. The game
// must exist: the FK refuses otherwise and mapWriteError reports it as
// ErrConflict (docs/spec/02 keeps the constraint as the authority, and
// editions_identity_idx is what makes a re-import reconcile rather than
// duplicate).
func (s *Store) CreateEdition(ctx context.Context, e games.Edition) error {
	return s.inWrite(ctx, func(ctx context.Context, tx *Tx) error {
		const op = "create edition"
		if err := e.Validate(); err != nil {
			return err
		}
		_, err := tx.exec(ctx, op,
			`INSERT INTO editions (id, game_id, region, language, revision, source_identity, display_aspect, locale, compatibility_notes)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, e.GameID, e.Region, e.Language, e.Revision, nullStr(e.SourceIdentity),
			nullFloat(e.DisplayAspect), e.Locale, e.CompatibilityNotes)
		return err
	})
}

// GetEdition loads one edition by id.
func (s *Store) GetEdition(ctx context.Context, id string) (games.Edition, error) {
	var out games.Edition
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var err error
		out, err = scanEditionRow(q.queryRow(ctx, opGetEdition,
			"SELECT "+editionColumns+" FROM editions WHERE id = ?", id), opGetEdition)
		return err
	})
	return out, err
}

// ListEditions returns every edition of a game in id order. It does not include
// the published revision: that is per-edition state and reading it for a list of
// N editions means N joins the caller usually does not need. Use
// PublishedProfile(editionID) for the one edition a session is about to start.
func (s *Store) ListEditions(ctx context.Context, gameID string) ([]games.Edition, error) {
	var out []games.Edition
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, opListEditions,
			"SELECT "+editionColumns+" FROM editions WHERE game_id = ? ORDER BY id", gameID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEditionRow(rows, opListEditions)
			if err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// ── scanners ────────────────────────────────────────────────────────────────

func scanGameRow(r scanner, op string) (games.Game, error) {
	var (
		g    games.Game
		tags []byte
	)
	if err := r.Scan(&g.ID, &g.Slug, &g.Title, &g.Description, &g.Platform, &tags, &g.CreatedAt); err != nil {
		return g, mapReadError(op, err)
	}
	if len(tags) > 0 {
		g.UserTags = json.RawMessage(append([]byte(nil), tags...))
	}
	return g, nil
}

func scanEditionRow(r scanner, op string) (games.Edition, error) {
	var (
		e      games.Edition
		src    sql.NullString
		aspect sql.NullFloat64
	)
	if err := r.Scan(&e.ID, &e.GameID, &e.Region, &e.Language, &e.Revision,
		&src, &aspect, &e.Locale, &e.CompatibilityNotes); err != nil {
		return e, mapReadError(op, err)
	}
	e.SourceIdentity = src.String
	e.DisplayAspect = optFloat(aspect)
	return e, nil
}

// existsAfterWrite converts "this UPDATE matched nothing" into ErrNotFound.
// Rows-affected is a sound existence signal for an UPDATE: writing identical
// values back still counts as one row matched, so only a missing key reports 0.
func existsAfterWrite(ctx context.Context, tx *Tx, op string, res sql.Result, table, id string) error {
	n, err := res.RowsAffected()
	if err != nil {
		// A driver that cannot report rows affected cannot prove existence
		// either; ask the table directly instead of guessing.
		var one int
		if qErr := tx.queryRow(ctx, op, "SELECT 1 FROM "+table+" WHERE id = ?", id).Scan(&one); qErr != nil {
			return mapReadError(op, qErr)
		}
		return nil
	}
	if n == 0 {
		return notFound(op, table+" "+id)
	}
	return nil
}
