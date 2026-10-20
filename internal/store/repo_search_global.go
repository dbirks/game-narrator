package store

// Global retrieval for the command-palette search (ADR-0098, UX-02 ⌘K).
//
// The palette searches across the whole catalogue rather than one profile
// revision, so these methods differ from SearchMessages in exactly two ways:
// the content tables are scoped to the latest revision of each edition (one
// revision per edition, or the same line would surface once per revision of
// every edition), and the rows carry the game they belong to so the API layer
// can build the result's subtitle and route without a second lookup.
//
// Every query still runs on the store's safe inRead path (ADR-0062 / cfd936f:
// the FTS5 structure cache is per-connection and a handler that opened its own
// connection would reintroduce the stale-view failure class), and user text
// reaches the database only as the two spellings SearchMessages already
// trusts: ftsPhrase for the trigram index and a wildcard-escaped LIKE literal
// for the name scans.
//
// Match classes (the store's contract for a name field):
//
//	MatchExact     the stored field equals the query (case-insensitive)
//	MatchPrefix    the stored field begins with the query
//	MatchContains  the stored field contains the query
//
// The API layer turns the class into the palette's score; the store never
// mixes a class with a bm25 number, which the two cannot be compared to.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games"
)

// opSearchGlobal names the reads below in the store's operation log, next to
// opSearchMessages in repo_search.go.
const opSearchGlobal = "search global"

// The match classes, in the store's documented order. They are the integer
// tiers the API layer scores from; the values are part of the contract this
// file's tests pin, not implementation detail.
const (
	MatchContains = 1
	MatchPrefix   = 2
	MatchExact    = 3
)

// GameSearchHit is one title family whose name matched the query.
type GameSearchHit struct {
	Game       games.Game
	MatchClass int
}

// CharacterSearchHit is one character (of the latest revision of its
// edition) whose name matched, with the game its palette row links to.
type CharacterSearchHit struct {
	Character  games.Character
	GameID     string
	GameTitle  string
	MatchClass int
}

// LineSearchHit is one dialogue line (of the latest revision of its edition)
// that the trigram index matched, with the game its palette row links to.
// BM25 is bm25(messages_fts) — lower is better, passed through unmodified,
// the same convention SearchMessages' hits carry. The prefix path (short
// queries) cannot rank and reports 0, the honest this-was-not-ranked value.
type LineSearchHit struct {
	Message   games.Message
	GameID    string
	GameTitle string
	BM25      float64
}

// latestRevisionClause scopes a join on profiles to the latest revision of
// the row's edition. profiles_edition_revision_idx is UNIQUE on
// (edition_id, revision_num), so the MAX subquery is an index range read, and
// the join itself keeps exactly one profile row per edition.
const latestRevisionClause = " AND p.revision_num = (SELECT MAX(p2.revision_num) FROM profiles p2 WHERE p2.edition_id = p.edition_id)"

// findGamesGlobalSQL matches a query against the title field of every game.
// The games table is one row per title family — a handful on an appliance —
// so a LIKE scan is the honest retrieval; the CASE computes the match class
// from the same literals the WHERE uses, so a row that matched cannot score
// a class it does not earn.
const findGamesGlobalSQL = `SELECT ` + gameColumns + `,
	CASE
		WHEN lower(title) = ? THEN 3
		WHEN lower(title) LIKE ? ESCAPE '\' THEN 2
		ELSE 1
	END
	FROM games
	WHERE lower(title) LIKE ? ESCAPE '\'
	ORDER BY 8 DESC, created_at DESC, id
	LIMIT ?`

// findCharactersGlobalSQL is the same shape across the catalogue: the name
// field of the latest revision of every edition. Every characters column is
// prefixed because games carries id and created_at too, and an unqualified
// SELECT column that both tables hold is an ambiguity error, not a guess.
var findCharactersGlobalSQL = `SELECT ` + addPrefix(characterColumns, "c.") + `, e.game_id, g.title,
	CASE
		WHEN lower(c.name) = ? THEN 3
		WHEN lower(c.name) LIKE ? ESCAPE '\' THEN 2
		ELSE 1
	END
	FROM characters c
	JOIN profiles p ON p.id = c.profile_id` + latestRevisionClause + `
	JOIN editions e ON e.id = p.edition_id
	JOIN games g ON g.id = e.game_id
	WHERE lower(c.name) LIKE ? ESCAPE '\'
	ORDER BY 14 DESC, c.created_at DESC, c.id
	LIMIT ?`

// findLinesFTSSQL is the trigram path: the existing messages_fts index,
// phrase-escaped, scoped to the latest revision of each edition. ORDER BY
// score ASC is bm25's best-first direction, as in SearchMessages' own join.
var findLinesFTSSQL = fmt.Sprintf(`SELECT bm25(messages_fts) AS score, %s, e.game_id, g.title
	FROM messages_fts
	JOIN messages m ON m.rowid = messages_fts.rowid
	JOIN profiles p ON p.id = m.profile_id`+latestRevisionClause+`
	JOIN editions e ON e.id = p.edition_id
	JOIN games g ON g.id = e.game_id
	WHERE messages_fts MATCH ?
	ORDER BY score ASC
	LIMIT ?`, addPrefix(messageColumns, "m."))

// findLinesPrefixSQL is the short-query path (<3 runes): the normalized-text
// prefix scan, same scope and same honest 0 score as SearchMessages' prefix
// SQL — a prefix scan does not rank, and claiming 1.0 would let a caller
// compare a not-ranked number with a real bm25 value.
var findLinesPrefixSQL = fmt.Sprintf(`SELECT 0 AS score, %s, e.game_id, g.title
	FROM messages m
	JOIN profiles p ON p.id = m.profile_id`+latestRevisionClause+`
	JOIN editions e ON e.id = p.edition_id
	JOIN games g ON g.id = e.game_id
	WHERE m.normalized_text LIKE ? ESCAPE '\'
	ORDER BY m.normalized_text
	LIMIT ?`, addPrefix(messageColumns, "m."))

// recentCharactersGlobalSQL and recentLinesGlobalSQL answer the palette's
// empty-query "recents" mode: no match class exists to order by, so the
// creation order alone decides (created_at is second-resolution, so id —
// monotonic within a millisecond, ADR-0080 — breaks the ties).
var recentCharactersGlobalSQL = `SELECT ` + addPrefix(characterColumns, "c.") + `, e.game_id, g.title
	FROM characters c
	JOIN profiles p ON p.id = c.profile_id` + latestRevisionClause + `
	JOIN editions e ON e.id = p.edition_id
	JOIN games g ON g.id = e.game_id
	ORDER BY c.created_at DESC, c.id DESC
	LIMIT ?`

var recentLinesGlobalSQL = fmt.Sprintf(`SELECT 0 AS score, %s, e.game_id, g.title
	FROM messages m
	JOIN profiles p ON p.id = m.profile_id`+latestRevisionClause+`
	JOIN editions e ON e.id = p.edition_id
	JOIN games g ON g.id = e.game_id
	ORDER BY m.created_at DESC, m.id DESC
	LIMIT ?`, addPrefix(messageColumns, "m."))

// FindGamesGlobal returns the games whose title contains query, ranked exact
// match first, then prefix, then contains, and within a class most recently
// added first.
func (s *Store) FindGamesGlobal(ctx context.Context, query string, limit int) ([]GameSearchHit, error) {
	limit = clampLimit(limit, MaxSearchLimit)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: find games: query is required")
	}
	term := likeTerm(query)
	var out []GameSearchHit
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, opSearchGlobal, findGamesGlobalSQL,
			term, term+"%", "%"+term+"%", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h GameSearchHit
			var cls int
			var tags []byte
			if err := rows.Scan(&h.Game.ID, &h.Game.Slug, &h.Game.Title, &h.Game.Description,
				&h.Game.Platform, &tags, &h.Game.CreatedAt, &cls); err != nil {
				return mapReadError(opSearchGlobal, err)
			}
			if len(tags) > 0 {
				h.Game.UserTags = json.RawMessage(append([]byte(nil), tags...))
			}
			h.MatchClass = cls
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// FindCharactersGlobal returns the characters (of the latest revision of each
// edition) whose name contains query, ranked by match class then recency.
func (s *Store) FindCharactersGlobal(ctx context.Context, query string, limit int) ([]CharacterSearchHit, error) {
	limit = clampLimit(limit, MaxSearchLimit)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: find characters: query is required")
	}
	term := likeTerm(query)
	var out []CharacterSearchHit
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, opSearchGlobal, findCharactersGlobalSQL,
			term, term+"%", "%"+term+"%", limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h CharacterSearchHit
			var (
				c         games.Character
				cls       int
				aliases   []byte
				descript  []byte
				refs      []byte
				portrait  string
				status    string
				gameID    string
				gameTitle string
			)
			if err := rows.Scan(&c.ID, &c.ProfileID, &c.Name, &aliases, &descript,
				&c.PersonalitySummary, &c.Role, &refs, &portrait, &status,
				&c.CreatedAt, &gameID, &gameTitle, &cls); err != nil {
				return mapReadError(opSearchGlobal, err)
			}
			c.Aliases = rawJSON(aliases)
			c.VisualDescriptors = rawJSON(descript)
			c.VerifiedSourceRefs = rawJSON(refs)
			c.PortraitAssetID = portrait
			c.Status = games.CharacterStatus(status)
			h = CharacterSearchHit{Character: c, GameID: gameID, GameTitle: gameTitle, MatchClass: cls}
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// FindLinesGlobal returns the lines of the latest revision of each edition
// that match query: the trigram FTS index for queries of at least three
// runes, the normalized-text prefix scan below that. Both paths are scoped as
// FindCharactersGlobal's is, and the FTS path's order is bm25's.
func (s *Store) FindLinesGlobal(ctx context.Context, query string, limit int) ([]LineSearchHit, error) {
	limit = clampLimit(limit, MaxSearchLimit)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: find lines: query is required")
	}
	var out []LineSearchHit
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var (
			rows *sql.Rows
			err  error
		)
		if utf8.RuneCountInString(query) >= minTrigramQuery {
			rows, err = q.query(ctx, opSearchGlobal, findLinesFTSSQL, ftsPhrase(query), limit)
		} else {
			rows, err = q.query(ctx, opSearchGlobal, findLinesPrefixSQL, likePrefix(query), limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				h LineSearchHit
				m games.Message
			)
			if err := scanGlobalLine(rows, &h, &m); err != nil {
				return err
			}
			h.Message = m
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// RecentCharactersGlobal answers the palette's empty-query mode: the most
// recently added characters of the latest revision of each edition.
func (s *Store) RecentCharactersGlobal(ctx context.Context, limit int) ([]CharacterSearchHit, error) {
	limit = clampLimit(limit, MaxSearchLimit)
	var out []CharacterSearchHit
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, opSearchGlobal, recentCharactersGlobalSQL, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var h CharacterSearchHit
			var (
				c        games.Character
				aliases  []byte
				descript []byte
				refs     []byte
				portrait string
				status   string
			)
			if err := rows.Scan(&c.ID, &c.ProfileID, &c.Name, &aliases, &descript,
				&c.PersonalitySummary, &c.Role, &refs, &portrait, &status,
				&c.CreatedAt, &h.GameID, &h.GameTitle); err != nil {
				return mapReadError(opSearchGlobal, err)
			}
			c.Aliases = rawJSON(aliases)
			c.VisualDescriptors = rawJSON(descript)
			c.VerifiedSourceRefs = rawJSON(refs)
			c.PortraitAssetID = portrait
			c.Status = games.CharacterStatus(status)
			h.Character = c
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// RecentLinesGlobal answers the palette's empty-query mode: the most recently
// added lines of the latest revision of each edition.
func (s *Store) RecentLinesGlobal(ctx context.Context, limit int) ([]LineSearchHit, error) {
	limit = clampLimit(limit, MaxSearchLimit)
	var out []LineSearchHit
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		rows, err := q.query(ctx, opSearchGlobal, recentLinesGlobalSQL, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				h LineSearchHit
				m games.Message
			)
			if err := scanGlobalLine(rows, &h, &m); err != nil {
				return err
			}
			h.Message = m
			out = append(out, h)
		}
		return rows.Err()
	})
	return out, err
}

// scanGlobalLine reads one row shaped as `<score>, <messageColumns>, game_id,
// game_title`. The message columns are scanned with the same field mapping
// scanMessage uses, so the two cannot drift apart about what a message holds.
func scanGlobalLine(r scanner, h *LineSearchHit, m *games.Message) error {
	var (
		score       float64
		source      sql.NullString
		srcMsgID    sql.NullString
		markup      []byte
		scene       sql.NullString
		speaker     sql.NullString
		kind        string
		attribution string
		evidence    []byte
		flags       []byte
	)
	dest := []any{&score, &m.ID, &m.ProfileID, &source, &srcMsgID, &m.OriginalText, &markup,
		&m.NormalizedText, &m.SpeechText, &m.Language, &kind, &scene, &speaker, &attribution,
		&m.AttributionConfidence, &evidence, &flags, &m.Revision, &m.CreatedAt, &m.UpdatedAt,
		&h.GameID, &h.GameTitle}
	if err := r.Scan(dest...); err != nil {
		return mapReadError(opSearchGlobal, err)
	}
	m.SourceID = source.String
	m.SourceMessageID = srcMsgID.String
	m.MarkupJSON = rawJSON(markup)
	m.SceneID = scene.String
	m.SpeakerID = speaker.String
	m.TextKind = games.TextKind(kind)
	m.AttributionStatus = games.AttributionStatus(attribution)
	m.AttributionEvidence = rawJSON(evidence)
	m.Flags = rawJSON(flags)
	h.BM25 = score
	return nil
}

// likeTerm returns query lower-cased and LIKE-wildcard-escaped, for use as a
// literal in `lower(field) LIKE ? ESCAPE '\'`. A user's % or _ searches for
// that character, and the backslash escape itself is escaped first.
func likeTerm(s string) string {
	return strings.ToLower(wildcardEscape(s))
}

// wildcardEscape escapes the LIKE metacharacters in s for an ESCAPE '\\' match:
// the escape character itself first, or escaping the others would escape its
// own escape character (likePrefix in repo_search.go is the same rule plus
// the prefix operator).
func wildcardEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// rawJSON copies a scanned JSON column into a RawMessage without aliasing the
// scan buffer.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	return json.RawMessage(append([]byte(nil), b...))
}
