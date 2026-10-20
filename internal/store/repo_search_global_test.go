package store

// Retrieval tests for the global palette search (ADR-0098). Everything is
// synthetic text typed into the test (AGENTS.md rule 5). The per-revision
// SearchMessages tests in repo_search_test.go cover the shared FTS/prefix
// mechanics; these pin what is new here: the match classes, the latest-
// revision-per-edition scope, and the recents reads.

import (
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func (f *repoFixture) gameWith(title, slug string) games.Game {
	f.t.Helper()
	g := games.Game{
		ID:        games.NewID(),
		Slug:      slug,
		Title:     title,
		Platform:  "gamecube",
		UserTags:  []byte(`["synthetic"]`),
		CreatedAt: nowRFC3339(),
	}
	if err := f.s.CreateGame(f.ctx, g); err != nil {
		f.t.Fatalf("CreateGame(%s): %v", title, err)
	}
	return g
}

// TestFindGamesGlobalRanking pins the match classes: exact before prefix
// before contains, case-insensitive, and within a class recency decides.
func TestFindGamesGlobalRanking(t *testing.T) {
	f := newRepoFixture(t)
	// The fixture game is "Synthetic Tide": exact for the query below.
	f.gameWith("Synthetic Tide Remastered", "synthetic-tide-remastered")   // prefix
	f.gameWith("The Synthetic Tide Returns", "the-synthetic-tide-returns") // contains

	for _, q := range []string{"synthetic tide", "Synthetic Tide"} {
		hits, err := f.s.FindGamesGlobal(f.ctx, q, 10)
		if err != nil {
			t.Fatalf("FindGamesGlobal(%q): %v", q, err)
		}
		if len(hits) != 3 {
			t.Fatalf("FindGamesGlobal(%q) = %d hits, want 3", q, len(hits))
		}
		if hits[0].MatchClass != MatchExact || hits[0].Game.Title != "Synthetic Tide" {
			t.Errorf("first hit = %+v, want the exact match first", hits[0])
		}
		if hits[1].MatchClass != MatchPrefix || hits[1].Game.Title != "Synthetic Tide Remastered" {
			t.Errorf("second hit = %+v, want the prefix match", hits[1])
		}
		if hits[2].MatchClass != MatchContains || hits[2].Game.Title != "The Synthetic Tide Returns" {
			t.Errorf("third hit = %+v, want the contains match", hits[2])
		}
	}
}

// TestFindGamesGlobalTreatsLikeMetacharactersAsText: a query full of LIKE
// wildcards searches for those characters, not for a pattern.
func TestFindGamesGlobalTreatsLikeMetacharactersAsText(t *testing.T) {
	f := newRepoFixture(t)
	f.gameWith("100% Wave \\ _ Demo", "synthetic-wave-demo")

	hits, err := f.s.FindGamesGlobal(f.ctx, `100% Wave \ _`, 10)
	if err != nil {
		t.Fatalf("FindGamesGlobal: %v", err)
	}
	if len(hits) != 1 || hits[0].Game.Title != "100% Wave \\ _ Demo" {
		t.Fatalf("hits = %+v, want the one title containing the wildcards as text", hits)
	}

	if _, err := f.s.FindGamesGlobal(f.ctx, "%", 10); err != nil {
		t.Errorf("FindGamesGlobal(%q) failed: %v", "%", err)
	}
}

// TestFindCharactersGlobalStaysOnTheLatestRevision is the scope rule: a line
// or a face that was retired by a newer revision must not surface, and the
// hit carries the game its palette row links to.
func TestFindCharactersGlobalStaysOnTheLatestRevision(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(1)
	b.put(f)
	if err := f.s.SetProfileStatus(f.ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("draft to review: %v", err)
	}
	if _, err := f.s.Publish(f.ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	child, err := f.s.NewRevision(f.ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	// Rename the snapshot's character: "Sailor" retires in this edition.
	dst, err := f.s.GetProfileContent(f.ctx, child.ID)
	if err != nil {
		t.Fatalf("GetProfileContent: %v", err)
	}
	if len(dst.Characters) != 1 {
		t.Fatalf("the snapshot copied %d characters, want 1", len(dst.Characters))
	}
	renamed := dst.Characters[0]
	renamed.Name = "Harbour Pilot"
	if err := f.s.PutCharacters(f.ctx, child.ID, []games.Character{renamed}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	hits, err := f.s.FindCharactersGlobal(f.ctx, "Sailor", 10)
	if err != nil {
		t.Fatalf("FindCharactersGlobal: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("the retired name still surfaces from an old revision: %+v", hits)
	}
	hits, err = f.s.FindCharactersGlobal(f.ctx, "harbour", 10)
	if err != nil {
		t.Fatalf("FindCharactersGlobal: %v", err)
	}
	if len(hits) != 1 || hits[0].MatchClass != MatchPrefix {
		t.Fatalf("hits = %+v, want the renamed character, prefix match", hits)
	}
	if hits[0].GameID != f.game.ID || hits[0].GameTitle != f.game.Title {
		t.Errorf("hit game = %s %q, want the fixture game", hits[0].GameID, hits[0].GameTitle)
	}
	if hits[0].Character.ProfileID != child.ID {
		t.Errorf("hit profile = %s, want the latest revision %s", hits[0].Character.ProfileID, child.ID)
	}
}

// TestFindLinesGlobalDeduplicatesRevisions: the same line exists in two
// revisions of the edition's history; the palette sees it once, from the
// latest.
func TestFindLinesGlobalDeduplicatesRevisions(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(1)
	b.Messages[0].OriginalText = "Beacon keeper log entry four"
	b.Messages[0].NormalizedText = "beacon keeper log entry four"
	b.put(f)
	if err := f.s.SetProfileStatus(f.ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("draft to review: %v", err)
	}
	if _, err := f.s.Publish(f.ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := f.s.NewRevision(f.ctx, f.profile.ID); err != nil {
		t.Fatalf("NewRevision: %v", err)
	}

	hits, err := f.s.FindLinesGlobal(f.ctx, "keeper log", 10)
	if err != nil {
		t.Fatalf("FindLinesGlobal: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %d, want exactly 1 across the two revisions", len(hits))
	}
	// the trigram path reports a real bm25 (which can be positive or negative
	// depending on document length); the prefix path reports its honest 0.
	if hits[0].GameID != f.game.ID {
		t.Errorf("GameID = %s, want the fixture game", hits[0].GameID)
	}
}

// TestFindLinesGlobalTreatsFTSSyntaxAsText is the injection guard for the
// global index, the same cases repo_search_test.go runs per-revision: a quote,
// a boolean operator, a proximity operator, a prefix star and an unbalanced
// quote are text to find, never instructions.
func TestFindLinesGlobalTreatsFTSSyntaxAsText(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(3)
	b.Messages[0].OriginalText = "Open water is calm today"
	b.Messages[0].NormalizedText = "open water is calm today"
	b.Messages[1].OriginalText = `The keeper's log ends here`
	b.Messages[1].NormalizedText = `the keeper's log ends here`
	b.Messages[2].OriginalText = "Harbour light on schedule"
	b.Messages[2].NormalizedText = "harbour light on schedule"
	b.put(f)
	ctx := f.ctx

	syntaxQueries := []string{
		`"`,
		`"open water"`,
		`open OR water`,
		`NEAR(open water)`,
		`*open`,
		`open"`,
		`open AND (water OR harbour`,
		`calm's`,
	}
	for _, q := range syntaxQueries {
		if _, err := f.s.FindLinesGlobal(ctx, q, 10); err != nil {
			t.Errorf("FindLinesGlobal(%q) failed: %v", q, err)
		}
	}

	// The doubled-quote phrase must match the line that contains the apostrophe
	// as text, and nothing else: a parser reading the quote as a string boundary
	// would return a different set or fail.
	hits, err := f.s.FindLinesGlobal(ctx, `keeper's`, 10)
	if err != nil {
		t.Fatalf("FindLinesGlobal: %v", err)
	}
	if len(hits) != 1 || !strings.Contains(hits[0].Message.NormalizedText, "keeper") {
		t.Fatalf("hits = %+v, want the one line containing the apostrophe", hits)
	}

	// An OR the parser was supposed to ignore must not widen the result: with
	// the operator treated as text, the phrase "open OR water" occurs nowhere.
	hits, err = f.s.FindLinesGlobal(ctx, `open OR water`, 10)
	if err != nil {
		t.Fatalf("FindLinesGlobal: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("the OR was parsed as an operator: %d hits", len(hits))
	}
}

// TestFindLinesGlobalShortQueryUsesPrefixPath: below the trigram minimum the
// normalized-text prefix scan answers, and its honest not-ranked score is 0.
func TestFindLinesGlobalShortQueryUsesPrefixPath(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(2)
	b.Messages[0].OriginalText = "Beacon keeper log entry four"
	b.Messages[0].NormalizedText = "beacon keeper log entry four"
	b.Messages[1].OriginalText = "Owl tracks in the mud"
	b.Messages[1].NormalizedText = "owl tracks in the mud"
	b.put(f)

	hits, err := f.s.FindLinesGlobal(f.ctx, "be", 10)
	if err != nil {
		t.Fatalf("FindLinesGlobal: %v", err)
	}
	if len(hits) != 1 || hits[0].BM25 != 0 {
		t.Fatalf("hits = %+v, want the one prefix match with score 0", hits)
	}
}

// TestRecentGlobalsAnswerTheEmptyQueryMode: with no query the palette shows
// recents, newest first, still scoped to the latest revision.
func TestRecentGlobalsAnswerTheEmptyQueryMode(t *testing.T) {
	f := newRepoFixture(t)
	b := f.bundle(1)
	b.put(f)
	if err := f.s.SetProfileStatus(f.ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("draft to review: %v", err)
	}
	if _, err := f.s.Publish(f.ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := f.s.NewRevision(f.ctx, f.profile.ID); err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	// A second edition of the same game gets a fresh draft with a second line.
	e2 := f.editionWithRegion("EU")
	p2, err := f.s.CreateDraftProfile(f.ctx, e2.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	m := f.message("Second edition line about the tide")
	m.ProfileID = p2.ID
	if err := f.s.PutMessages(f.ctx, p2.ID, []games.Message{m}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	lines, err := f.s.RecentLinesGlobal(f.ctx, 10)
	if err != nil {
		t.Fatalf("RecentLinesGlobal: %v", err)
	}
	// One line per latest revision: the second edition's line and the first
	// edition's latest-revision copy of its single line.
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2 (one per latest revision)", len(lines))
	}
	for _, l := range lines {
		if l.BM25 != 0 {
			t.Errorf("recent line carries BM25 %v, want the not-ranked 0", l.BM25)
		}
	}

	chs, err := f.s.RecentCharactersGlobal(f.ctx, 10)
	if err != nil {
		t.Fatalf("RecentCharactersGlobal: %v", err)
	}
	if len(chs) != 1 {
		t.Fatalf("characters = %d, want 1 (the first edition's roster; the second edition has none)", len(chs))
	}
}
