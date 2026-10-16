package store

// Message retrieval (docs/spec/02 "SQLite schema principles": "FTS5 index on
// normalized message text/aliases, trigram … separate memory-efficient prefix
// index"; docs/spec/05 uses both paths at runtime).
//
// Two paths, chosen by the shape of the query, because they answer different
// questions and only one of them is safe to hand user text to:
//
//   - ≥3 runes: FTS5 `messages_fts` MATCH, ranked by bm25. The trigram tokenizer
//     indexes every 3-character window, so a noisy OCR fragment finds the line it
//     came from even when the words are mis-spaced or partly wrong (the
//     docs/spec/05 candidate-ranking case). Below three characters there is no
//     trigram to look up and the index simply has nothing for that input.
//   - <3 runes: the (profile_id, normalized_text) index with a LIKE prefix. An
//     operator typing "he" into the message editor expects completions, not an
//     empty result set.
//
// FTS5 query syntax is a real injection surface: the match string is *parsed*, so
// a bare `OR`, a `*` prefix operator, `NEAR(a b)` or an unbalanced `"` either
// changes what the search means or makes the statement fail. Every query therefore
// goes through ftsPhrase, which wraps it as a double-quoted phrase literal with
// internal quotes doubled — the one spelling under which FTS5 reads the input as
// text to find rather than as instructions.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games"
)

// MessageHit is one search result with its ranking.
type MessageHit struct {
	Message games.Message
	// Score is bm25(messages_fts) as SQLite computed it: lower is better, the
	// direction SQLite's own ORDER BY rank uses and the opposite of a naive
	// "relevance" number. It is passed through unmodified rather than rescaled,
	// because a rescaled score invites comparing it across queries or across
	// SQLite versions, which it does not support.
	Score float64
}

// SearchMessages finds messages of one revision whose text contains the query.
//
// The revision filter is applied *with* the match rather than afterwards:
// filtering in Go would mean materialising every revision's hits to keep a few,
// and docs/spec/16's 10 000-message search benchmark is precisely the case where
// that costs the appliance its memory (AGENTS.md "Keep runtime predictable").
//
// An empty or whitespace-only query is refused rather than treated as "match
// everything": a search box that silently turns into a full listing is how an
// operator pages through 50 000 rows by accident.
func (s *Store) SearchMessages(ctx context.Context, profileID, query string, limit int) ([]MessageHit, error) {
	limit = clampLimit(limit, MaxSearchLimit)
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("store: search messages: query is required")
	}

	var out []MessageHit
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		var (
			rows *sql.Rows
			err  error
		)
		if utf8.RuneCountInString(query) >= minTrigramQuery {
			rows, err = q.query(ctx, opSearchMessages, searchFTSSQL, ftsPhrase(query), profileID, limit)
		} else {
			rows, err = q.query(ctx, opSearchMessages, searchPrefixSQL, likePrefix(query), profileID, limit)
		}
		if err != nil {
			return err
		}
		defer rows.Close()

		for rows.Next() {
			var score float64
			m, err := scanMessage(rows, &score)
			if err != nil {
				return err
			}
			out = append(out, MessageHit{Message: m, Score: score})
		}
		return rows.Err()
	})
	return out, err
}

// searchFTSSQL joins the FTS index back to the messages it indexes (external
// content: the text is stored once — 0002_content.sql) and constrains the match to
// one revision. ORDER BY score ASC is bm25's "best first" direction.
var searchFTSSQL = fmt.Sprintf(`SELECT bm25(messages_fts) AS score, %s
	FROM messages_fts
	JOIN messages m ON m.rowid = messages_fts.rowid
	WHERE messages_fts MATCH ? AND m.profile_id = ?
	ORDER BY score ASC
	LIMIT ?`, addPrefix(messageColumns, "m."))

// searchPrefixSQL is the short-query path: a prefix scan of the composite index
// docs/spec/02 requires on (profile_id, normalized_text). Its score column is the
// constant 0 — there is no ranking in a prefix scan, and pretending otherwise
// (e.g. by claiming 1.0) would let a caller compare a number that means "we did
// not rank this" with a real bm25 value. ESCAPE '\' is explicit so a user typing
// % or _ searches for those characters.
var searchPrefixSQL = fmt.Sprintf(`SELECT 0 AS score, %s
	FROM messages m
	WHERE m.normalized_text LIKE ? ESCAPE '\' AND m.profile_id = ?
	ORDER BY m.normalized_text
	LIMIT ?`, addPrefix(messageColumns, "m."))

// ftsPhrase renders arbitrary user text as an FTS5 phrase literal.
//
// Doubling every `"` and wrapping the whole string is exactly FTS5's
// string-literal rule, so the tokenizer receives characters to find and never
// operators: `*` stays a star, `OR` stays two letters inside a phrase, `NEAR(`
// cannot open a proximity query, and an unmatched `"` cannot break the parse.
// Newlines and tabs survive as ordinary characters, which means such a query
// returns nothing rather than failing — the honest answer for text that cannot
// occur in a normalized message.
func ftsPhrase(query string) string {
	return `"` + strings.ReplaceAll(query, `"`, `""`) + `"`
}

// likePrefix escapes the LIKE wildcards and appends the prefix operator. The
// escape character itself is escaped first, or escaping the others would escape
// its own escape character.
func likePrefix(query string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query) + "%"
}
