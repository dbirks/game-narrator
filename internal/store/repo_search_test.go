package store

// Retrieval tests for SearchMessages (docs/spec/02 "SQLite schema principles":
// "FTS5 index on normalized message text … trigram"; docs/spec/05's candidate
// ranking; docs/spec/08's hard upper bounds). The texts here are invented for the
// test — no game script (AGENTS.md rule 5).

import (
	"fmt"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// searchFixture loads one revision with the given lines and returns it. Each line
// is stored with the fixture's own normalization (lowercase, trailing punctuation
// trimmed), which is what the trigram index sees.
func searchFixture(t *testing.T, lines ...string) (*repoFixture, string) {
	t.Helper()
	f := newRepoFixture(t)
	msgs := make([]games.Message, 0, len(lines))
	for _, line := range lines {
		msgs = append(msgs, f.message(line))
	}
	if err := f.s.PutMessages(f.ctx, f.profile.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}
	return f, f.profile.ID
}

func hitIDs(hits []MessageHit) []string {
	out := make([]string, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.Message.ID)
	}
	return out
}

// TestSearchMessagesFindsSubstrings is the reason docs/spec/02 asks for trigrams
// rather than the default unicode61 tokenizer: the runtime query is a noisy OCR
// fragment, not a word a user typed on purpose.
func TestSearchMessagesFindsSubstrings(t *testing.T) {
	f, id := searchFixture(t,
		"Hello there, traveller!",
		"Open the door and enter.",
		"A sign on the wall lists the fares.",
	)
	ctx := f.ctx

	cases := []struct {
		query string
		want  int
	}{
		{"ello", 1},   // mid-word, not a word
		{"the", 3},    // a substring of all three, none of them a word match
		{"IGN on", 1}, // spans a space, and is typed in the wrong case
		{"zebra", 0},  // a miss is a miss, not an error
	}
	for _, c := range cases {
		hits, err := f.s.SearchMessages(ctx, id, c.query, 20)
		if err != nil {
			t.Fatalf("SearchMessages(%q): %v", c.query, err)
		}
		if len(hits) != c.want {
			t.Errorf("SearchMessages(%q) = %d hits %v, want %d", c.query, len(hits), hitIDs(hits), c.want)
		}
		for _, h := range hits {
			// Every hit must genuinely contain the query, case-folded: an index
			// that returns rows the user did not ask for is worse than none.
			if !strings.Contains(strings.ToLower(h.Message.NormalizedText), strings.ToLower(c.query)) &&
				!strings.Contains(strings.ToLower(h.Message.SpeechText), strings.ToLower(c.query)) {
				t.Errorf("hit %q does not contain %q", h.Message.NormalizedText, c.query)
			}
		}
	}

	// bm25's "best first" direction is preserved, not rescaled (MessageHit.Score's
	// doc): the rows come back ordered by the score SQLite computed.
	hits, err := f.s.SearchMessages(ctx, id, "the", 20)
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i-1].Score > hits[i].Score {
			t.Errorf("hits are not ranked best-first: %v before %v", hits[i-1].Score, hits[i].Score)
		}
	}
}

// TestSearchMessagesStaysInsideTheRevision proves the profile filter is applied
// with the match rather than after it: a curator searching one revision must never
// see another revision's lines, and a runtime candidate query must not be able to
// read across revisions.
func TestSearchMessagesStaysInsideTheRevision(t *testing.T) {
	f, first := searchFixture(t, "A distinctive lighthouse beacon")
	second, err := f.s.NewRevision(f.ctx, first)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	// The snapshot carries the parent's line, and one of its own.
	dst, err := f.s.GetProfileContent(f.ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	extra := f.message("A second beacon lit only in the draft")
	extra.ProfileID = second.ID
	if err := f.s.PutMessages(f.ctx, second.ID, append(dst.Messages, extra), nil); err != nil {
		t.Fatalf("PutMessages on the snapshot: %v", err)
	}

	for _, tc := range []struct {
		profile string
		want    int
	}{
		{first, 1}, // the published-side revision has one beacon line
		{second.ID, 2},
	} {
		hits, err := f.s.SearchMessages(f.ctx, tc.profile, "beacon", 20)
		if err != nil {
			t.Fatalf("SearchMessages(%s): %v", tc.profile, err)
		}
		if len(hits) != tc.want {
			t.Errorf("revision %s: %d hits %v, want %d", tc.profile, len(hits), hitIDs(hits), tc.want)
		}
		for _, h := range hits {
			if h.Message.ProfileID != tc.profile {
				t.Errorf("hit from profile %s leaked into the search of %s", h.Message.ProfileID, tc.profile)
			}
		}
	}
}

// TestSearchMessagesTreatsSyntaxAsText is the FTS5 injection guard. The match
// string is *parsed*, so an unquoted OR widens the query, a bare * becomes a
// prefix operator, NEAR( opens a proximity expression and an unmatched " breaks the
// statement. None of those may happen: user text is text to find.
func TestSearchMessagesTreatsSyntaxAsText(t *testing.T) {
	f, id := searchFixture(t,
		`The NEAR( oracle spoke in trigrams`,
		"An ordinary star * mark sits here",
		"Or alternatively the word or appears in for and before",
		`A quotation "mark" closes nothing`,
		"Nothing to see in this line at all",
	)
	ctx := f.ctx

	// Each of these is FTS5 syntax if it reaches the parser. A query that errors,
	// or that returns rows not containing the text, is the vulnerability.
	hostile := []string{
		`"`,     // unterminated string literal
		`*`,     // prefix operator
		`NEAR(`, // proximity operator, unterminated
		`oracle NEAR tr`,
		`OR`,       // boolean operator (also the prefix path below)
		`trigram*`, // prefix operator glued to text
		`"oracle" OR "star"`,
		`{brace} [brack] (paren)`,
		`-ordinary`, // column filter / NOT prefix in some dialects
		`AND`,
		`\`,
		"two words with a\nnewline",
		`oracle""spoke`,
	}
	for _, q := range hostile {
		hits, err := f.s.SearchMessages(ctx, id, q, 50)
		if err != nil {
			t.Errorf("SearchMessages(%q) failed: %v", q, err)
			continue
		}
		// Whatever comes back must contain the query as literal text, modulo case.
		// A search for `NEAR(` may legitimately find the line that holds "NEAR(",
		// and may not find the whole table.
		needle := strings.ToLower(strings.ReplaceAll(q, `""`, `"`))
		// Queries whose only characters are syntax have no trigram to look up, so
		// they either match nothing (FTS path) or match by prefix (short path).
		for _, h := range hits {
			text := strings.ToLower(h.Message.NormalizedText)
			speech := strings.ToLower(h.Message.SpeechText)
			if !strings.Contains(text, needle) && !strings.Contains(speech, needle) {
				t.Errorf("SearchMessages(%q) returned %q, which does not contain the text",
					q, h.Message.NormalizedText)
			}
		}
	}

	// The positive control, so the loop above cannot pass by matching nothing:
	// text that *is* in the revision still found it.
	for _, q := range []string{"NEAR(", "star *", `"mark"`, "for and"} {
		hits, err := f.s.SearchMessages(ctx, id, q, 50)
		if err != nil {
			t.Fatalf("SearchMessages(%q): %v", q, err)
		}
		if len(hits) == 0 {
			t.Errorf("SearchMessages(%q) found nothing, but the revision contains it", q)
		}
	}
}

// TestSearchMessagesShortQueryUsesPrefixPath covers the documented fallback: below
// three runes the trigram index has nothing to look up, and an operator typing into
// a filter box expects completions rather than an empty list.
func TestSearchMessagesShortQueryUsesPrefixPath(t *testing.T) {
	f, id := searchFixture(t,
		"Herald of the dawn",
		"Her voice carried over the water",
		"The current pulls south",
	)
	ctx := f.ctx

	// Two runes, which is below the trigram floor: prefix path, ordered by
	// normalized_text, scored 0 because a prefix scan has ranked nothing and a fake
	// score would invite comparison with bm25. The search is case-insensitive for
	// ASCII, which is what an operator typing a capital expects.
	hits, err := f.s.SearchMessages(ctx, id, "He", 20)
	if err != nil {
		t.Fatalf("prefix search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("prefix hits = %v, want the two lines starting with He", hitIDs(hits))
	}
	// Three runes crosses the floor and takes the FTS path instead, which is the
	// only branch that produces a real score.
	if hits, err := f.s.SearchMessages(ctx, id, "Her", 20); err != nil {
		t.Fatalf("trigram search: %v", err)
	} else if len(hits) != 2 {
		t.Fatalf("trigram hits = %v, want the two Her lines", hitIDs(hits))
	} else if hits[0].Score == 0 && hits[1].Score == 0 {
		t.Error("a 3-rune query took the prefix path; the trigram floor is wrong")
	}
	for _, h := range hits {
		if h.Score != 0 {
			t.Errorf("prefix hit %q carries score %v, want 0 (nothing was ranked)", h.Message.NormalizedText, h.Score)
		}
	}

	// One rune, and LIKE's own wildcards typed by a user who means the characters:
	// the escape is explicit, so % must not become "everything".
	if _, err := f.s.SearchMessages(ctx, id, "T", 20); err != nil {
		t.Errorf("single-rune search: %v", err)
	}
	if hits, err := f.s.SearchMessages(ctx, id, "%", 20); err != nil {
		t.Errorf("percent search: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("a literal %% query matched %v; the LIKE wildcard leaked", hitIDs(hits))
	}
	if hits, err := f.s.SearchMessages(ctx, id, "_", 20); err != nil {
		t.Errorf("underscore search: %v", err)
	} else if len(hits) != 0 {
		t.Errorf("a literal _ query matched %v; the LIKE wildcard leaked", hitIDs(hits))
	}
}

// TestSearchMessagesClampsLimit is docs/spec/08's "hard upper bounds" on the one
// list whose rows carry full text: a client asking for a million hits gets 100.
func TestSearchMessagesClampsLimit(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	const lines = 130
	msgs := make([]games.Message, 0, lines)
	for i := 0; i < lines; i++ {
		m := f.message(fmt.Sprintf("Lighthouse %03d keeps the current lit", i))
		m.ProfileID = f.profile.ID
		msgs = append(msgs, m)
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, msgs, nil); err != nil {
		t.Fatal(err)
	}

	for _, limit := range []int{0, -5, 1_000_000} {
		hits, err := f.s.SearchMessages(ctx, f.profile.ID, "lighthouse", limit)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		want := clampLimit(limit, MaxSearchLimit)
		if len(hits) != want {
			t.Errorf("limit %d returned %d hits, want the clamped %d", limit, len(hits), want)
		}
	}
	if _, err := f.s.SearchMessages(ctx, f.profile.ID, "   ", 10); err == nil {
		t.Error("a blank query was accepted; it silently means \"every message\"")
	}
}
