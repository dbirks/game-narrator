package store

// Pagination, filtering and idempotent-write tests for the content repository
// (docs/spec/08 "Conventions": "cursor pagination with hard bounds"; docs/spec/02
// "Transaction workflows": a re-run import must "reconcile, not duplicate").

import (
	"errors"
	"fmt"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestPaginateMessagesWalksEveryLineOnce is docs/spec/12's pagination property
// exercised at the size the dialogue browser actually faces: 1 000 lines of one
// revision, read in pages of 200. Keyset cursors (id > last id) must produce every
// row exactly once, in id order, with no gap where a page boundary fell and no
// repeat — an OFFSET-based page would show the repeat as soon as a writer inserted
// a row mid-walk, which the second half of this test does.
func TestPaginateMessagesWalksEveryLineOnce(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	const total = 1000
	msgs := make([]games.Message, 0, total)
	want := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		m := f.message(fmt.Sprintf("Synthetic line %04d of the long walk", i))
		msgs = append(msgs, m)
		want[m.ID] = true
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages(1000): %v", err)
	}

	const pageSize = 200
	seen := make(map[string]bool, total)
	var (
		cursor string
		pages  int
	)
	for {
		page, next, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, cursor, pageSize)
		if err != nil {
			t.Fatalf("ListMessages page %d: %v", pages, err)
		}
		if len(page) > pageSize {
			t.Fatalf("page %d returned %d rows, the limit is %d", pages, len(page), pageSize)
		}
		for i, m := range page {
			if i > 0 && page[i-1].ID >= m.ID {
				t.Fatalf("page %d is not ordered by id at offset %d", pages, i)
			}
			if seen[m.ID] {
				t.Fatalf("message %s came back twice (page %d)", m.ID, pages)
			}
			seen[m.ID] = true
		}
		pages++
		if next == "" {
			break
		}
		if pages > total/pageSize+2 {
			t.Fatalf("the walk did not terminate after %d pages", pages)
		}
		cursor = next
	}
	if pages != total/pageSize {
		t.Errorf("walked %d pages of %d, want %d", pages, pageSize, total/pageSize)
	}
	if len(seen) != total {
		t.Errorf("the walk read %d of %d messages", len(seen), total)
	}
	for id := range want {
		if !seen[id] {
			t.Fatalf("message %s was never returned: a page boundary skipped it", id)
		}
	}

	// A concurrent insert while a curator is mid-walk must not shift the window.
	// Resume from the first page's cursor after writing a row whose id sorts
	// *before* the cursor (ULIDs sort by time, so the new row is the largest id:
	// the point is only that pages after the cursor stay what they were).
	first, cursor, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, []games.Message{f.message("Written after the first page")}, nil); err != nil {
		t.Fatal(err)
	}
	second, _, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, cursor, 200)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 200 {
		t.Fatalf("second page = %d rows after a concurrent insert, want 200", len(second))
	}
	for _, m := range first {
		for _, n := range second {
			if m.ID == n.ID {
				t.Fatalf("message %s appears on both pages", m.ID)
			}
		}
	}
}

// TestPaginateBoundsAndCursors covers the two ways a client gets pagination wrong:
// asking for more than the hard bound, and handing back a cursor it invented
// (docs/spec/08 makes cursors opaque, so a mangled one is a 400, not a scan).
func TestPaginateBoundsAndCursors(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	msgs := make([]games.Message, 0, 300)
	for i := 0; i < 300; i++ {
		msgs = append(msgs, f.message(fmt.Sprintf("Line %03d for the bounds check", i)))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, msgs, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		limit int
		want  int
	}{
		{"default for zero", 0, DefaultListLimit},
		{"default for negative", -7, DefaultListLimit},
		{"exact", 120, 120},
		{"clamped to the hard bound", MaxListLimit + 1, MaxListLimit},
		{"clamped from an absurd request", 1_000_000, MaxListLimit},
	}
	for _, c := range cases {
		page, next, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, "", c.limit)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(page) != c.want {
			t.Errorf("%s: limit %d returned %d rows, want %d", c.name, c.limit, len(page), c.want)
		}
		if c.want < 300 && next == "" {
			t.Errorf("%s: no next cursor for a page of %d of 300 rows", c.name, len(page))
		}
	}

	if _, _, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, "not-a-cursor", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Errorf("garbage cursor = %v, want ErrInvalidCursor", err)
	}
	// A well-formed cursor naming a row of another table is still just a resume
	// point: it returns whatever sorts after it, which for an id past the end is
	// nothing. The id below is a syntactically valid ULID no clock can produce.
	if page, next, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, encodeCursor("7ZZZZZZZZZZZZZZZZZZZZZZZZZ"), 10); err != nil || len(page) != 0 || next != "" {
		t.Errorf("past-the-end cursor = %d rows, next %q, %v; want an empty page", len(page), next, err)
	}
}

// TestPaginateGamesWalksEveryRow covers ListGames, whose rows are the library
// screen: same keyset rule, same hard bound, and the cursor must survive the walk
// of a table that has no content rows at all.
func TestPaginateGamesWalksEveryRow(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	const total = 25
	for i := 0; i < total; i++ {
		g := games.Game{
			ID:        games.NewID(),
			Slug:      fmt.Sprintf("synthetic-title-%02d", i),
			Title:     fmt.Sprintf("Synthetic Title %02d", i),
			CreatedAt: nowRFC3339(),
		}
		if err := f.s.CreateGame(ctx, g); err != nil {
			t.Fatalf("CreateGame %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	cursor := ""
	for pages := 1; ; pages++ {
		page, next, err := f.s.ListGames(ctx, cursor, 10)
		if err != nil {
			t.Fatalf("ListGames: %v", err)
		}
		if len(page) > 10 {
			t.Fatalf("page of %d rows exceeds the limit", len(page))
		}
		for _, g := range page {
			if seen[g.ID] {
				t.Fatalf("game %s returned twice", g.ID)
			}
			seen[g.ID] = true
		}
		if next == "" {
			break
		}
		if pages > total {
			t.Fatal("the game walk did not terminate")
		}
		cursor = next
	}
	if len(seen) != total+1 {
		t.Errorf("walked %d games, want %d", len(seen), total+1)
	}
	if !seen[f.game.ID] {
		t.Error("the fixture's own game was never returned")
	}

	// The slug lookup answers the same miss code as an unknown id, so a client
	// cannot tell which lookup ran.
	if _, err := f.s.GetGameBySlug(ctx, "no-such-title"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetGameBySlug of a missing slug = %v, want ErrNotFound", err)
	}
}

// TestListMessagesFiltersNarrowOneRevision covers the MessageFilter fields
// docs/spec/09's dialogue browser drives: content class, scene and speaker, each
// backed by one of the composite indexes 0002_content.sql creates.
func TestListMessagesFiltersNarrowOneRevision(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	dock := f.scene("Dock")
	plaza := f.scene("Plaza")
	sailor := f.character("Sailor")
	keeper := f.character("Gatekeeper")
	if err := f.s.PutScenes(ctx, f.profile.ID, []games.Scene{dock, plaza}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutCharacters(ctx, f.profile.ID, []games.Character{sailor, keeper}); err != nil {
		t.Fatal(err)
	}

	msgs := make([]games.Message, 0, 10)
	for i := 0; i < 4; i++ {
		m := f.message(fmt.Sprintf("Dock line %d", i))
		m.SceneID, m.SpeakerID = dock.ID, sailor.ID
		m.AttributionStatus = games.AttributionConfirmed
		msgs = append(msgs, m)
	}
	for i := 0; i < 3; i++ {
		m := f.message(fmt.Sprintf("Plaza line %d", i))
		m.SceneID, m.SpeakerID = plaza.ID, keeper.ID
		m.AttributionStatus = games.AttributionConfirmed
		msgs = append(msgs, m)
	}
	for i := 0; i < 3; i++ {
		m := f.message(fmt.Sprintf("Carved sign %d", i))
		m.SceneID = plaza.ID
		m.TextKind = games.TextSign
		msgs = append(msgs, m)
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, msgs, nil); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		f    MessageFilter
		want int
	}{
		{"everything", MessageFilter{}, 10},
		{"by kind", MessageFilter{TextKind: games.TextSign}, 3},
		{"by scene", MessageFilter{SceneID: plaza.ID}, 6},
		{"by speaker", MessageFilter{SpeakerID: sailor.ID}, 4},
		{"scene and kind", MessageFilter{SceneID: plaza.ID, TextKind: games.TextSign}, 3},
		{"scene and speaker", MessageFilter{SceneID: plaza.ID, SpeakerID: keeper.ID}, 3},
		{"a scene nobody speaks in", MessageFilter{SceneID: dock.ID, SpeakerID: keeper.ID}, 0},
	}
	for _, c := range cases {
		var n int
		cursor := ""
		for {
			page, next, err := f.s.ListMessages(ctx, f.profile.ID, c.f, cursor, 2)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			n += len(page)
			if next == "" {
				break
			}
			cursor = next
			if n > len(msgs) {
				t.Fatalf("%s: filtered walk returned more rows than exist", c.name)
			}
		}
		if n != c.want {
			t.Errorf("%s: filter %+v matched %d rows, want %d", c.name, c.f, n, c.want)
		}
	}

	// A second revision of the same edition must not join the list: the profile
	// predicate is unconditional, and if it were ever dropped the dialogue browser
	// would show two revisions merged. The snapshot copies all ten lines.
	snapshot, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	page, next, err := f.s.ListMessages(ctx, f.profile.ID, MessageFilter{}, "", MaxListLimit)
	if err != nil || next != "" || len(page) != len(msgs) {
		t.Errorf("parent list after a sibling revision exists = %d rows, next %q, %v; want %d",
			len(page), next, err, len(msgs))
	}
	childPage, _, err := f.s.ListMessages(ctx, snapshot.ID, MessageFilter{}, "", MaxListLimit)
	if err != nil || len(childPage) != len(msgs) {
		t.Fatalf("snapshot list = %d rows, %v; want %d", len(childPage), err, len(msgs))
	}
	// Rows are compared by content, not by position: the snapshot's ids are new,
	// so both revisions are ordered by ids minted at different instants and the
	// ordinal positions do not correspond.
	texts := func(ms []games.Message) map[string]int {
		out := map[string]int{}
		for _, m := range ms {
			out[m.NormalizedText]++
		}
		return out
	}
	parentTexts, childTexts := texts(page), texts(childPage)
	for text, n := range parentTexts {
		if childTexts[text] != n {
			t.Errorf("snapshot holds %d copies of %q, the parent holds %d", childTexts[text], text, n)
		}
		delete(childTexts, text)
	}
	for text := range childTexts {
		t.Errorf("snapshot lists %q, which the parent does not have", text)
	}
	parentIDs := map[string]bool{}
	for _, m := range page {
		parentIDs[m.ID] = true
	}
	for _, m := range childPage {
		if parentIDs[m.ID] {
			t.Errorf("the snapshot's list returned the parent's message %s", m.ID)
		}
	}
}

// TestPutMessagesReconcilesInsteadOfDuplicating is the import-resume rule
// (docs/spec/02): a parser that replays the same batch after a crash must end with
// the same rows, not a second copy of every line. The per-message revision counter
// is what tells a reader the row was rewritten.
func TestPutMessagesReconcilesInsteadOfDuplicating(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	b := f.bundle(3)
	b.put(f)
	before, err := f.s.GetProfileContent(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Replay the identical batch.
	if err := f.s.PutMessages(ctx, f.profile.ID, b.Messages, b.Pages); err != nil {
		t.Fatalf("replaying the import: %v", err)
	}
	after, err := f.s.GetProfileContent(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Messages) != len(before.Messages) {
		t.Fatalf("replay left %d messages, want %d", len(after.Messages), len(before.Messages))
	}
	if len(after.Pages) != len(before.Pages) {
		t.Fatalf("replay left %d pages, want %d", len(after.Pages), len(before.Pages))
	}
	for i := range before.Messages {
		if after.Messages[i].Revision != before.Messages[i].Revision+1 {
			t.Errorf("message %s revision = %d after a replay, want %d: the counter is server-owned",
				after.Messages[i].ID, after.Messages[i].Revision, before.Messages[i].Revision+1)
		}
	}
	// The caller's own value for that counter is ignored rather than trusted.
	stuffed := b.Messages[0]
	stuffed.Revision = 99
	if err := f.s.PutMessages(ctx, f.profile.ID, []games.Message{stuffed}, nil); err != nil {
		t.Fatal(err)
	}
	m, _, err := f.s.GetMessage(ctx, f.profile.ID, stuffed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.Revision != after.Messages[0].Revision+1 {
		t.Errorf("message revision = %d, want %d: a caller cannot set its own counter",
			m.Revision, after.Messages[0].Revision+1)
	}
}

// TestProfileMessageCountCountsPerRevision is the Library's "N lines" column
// (ADR-0140) at the store level: an indexed COUNT scoped to one revision —
// the dialogue browser's length is never a page walk, and a revision's lines
// are never another revision's.
func TestProfileMessageCountCountsPerRevision(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	second, err := f.s.CreateDraftProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile(second): %v", err)
	}
	other := f.editionWithRegion("EU")
	third, err := f.s.CreateDraftProfile(ctx, other.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile(third): %v", err)
	}

	msgs := make([]games.Message, 0, 3)
	for i := 0; i < 3; i++ {
		msgs = append(msgs, f.message(fmt.Sprintf("Counted line %d of the first revision", i)))
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, msgs, nil); err != nil {
		t.Fatalf("PutMessages(first): %v", err)
	}
	one := f.message("The single line of the second revision")
	one.ProfileID = second.ID
	if err := f.s.PutMessages(ctx, second.ID, []games.Message{one}, nil); err != nil {
		t.Fatalf("PutMessages(second): %v", err)
	}

	if got, err := f.s.ProfileMessageCount(ctx, f.profile.ID); err != nil || got != 3 {
		t.Fatalf("first revision counted %d (%v), want 3", got, err)
	}
	if got, err := f.s.ProfileMessageCount(ctx, second.ID); err != nil || got != 1 {
		t.Fatalf("second revision counted %d (%v), want 1", got, err)
	}
	// An empty profile is an honest zero, not an error: the shelf renders
	// its count column from it.
	if got, err := f.s.ProfileMessageCount(ctx, third.ID); err != nil || got != 0 {
		t.Fatalf("an empty profile counted %d (%v), want 0", got, err)
	}
}
