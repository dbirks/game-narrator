package store

// Tests for the three publication facts 0004_publication.sql moves into the
// catalogue where docs/spec/02 says they belong: `profiles.row_version` (the
// optimistic-locking counter behind every ETag), `editions.published_profile_id`
// (the pointer that says what an edition serves) and `publish_actions` (the audit
// trail). ADR-0015 records why they are columns and a table rather than rows of
// the pruned-by-age `events` log, and the last test here is exactly the regression
// that rejection was about: prune the log to nothing and the facts must not move.
//
// These are deliberately *storage* tests. The behaviours they pin are the ones an
// HTTP-layer test cannot reach — a write that bypasses the repositories, a
// database written by an older build, a retention job that removes every
// operational row — and each one is checked against the file, not against a mock.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// TestRowVersionIncrementsByExactlyOnePerWrite is the counter's contract: one
// accepted write, one increment, no more and no less.
//
// "No more" is what makes a 200-row import usable — a counter that moved per row
// would expire every other editor's token on the first paste (docs/spec/08's
// If-Match) — and "no less" is what makes it a lock rather than a decoration. The
// batch-level rule lives here rather than in each Put method's test because the
// bump happens in contentWrite, once per transaction, for all of them.
func TestRowVersionIncrementsByExactlyOnePerWrite(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(2)
	b.put(f)

	if got := f.version(); got != freshRowVersion+7 {
		t.Fatalf("row version after the 7-write fixture = %d, want %d", got, freshRowVersion+7)
	}

	extraChar := f.character("Second")
	steps := []struct {
		name string
		run  func() error
	}{
		{"PutSources", func() error {
			return f.s.PutSources(ctx, f.profile.ID, []games.Source{f.source("file:///curated/second.csv", shaB, games.RightsUserSupplied)})
		}},
		{"PutCharacters", func() error {
			return f.s.PutCharacters(ctx, f.profile.ID, []games.Character{extraChar})
		}},
		{"PutScenes", func() error {
			return f.s.PutScenes(ctx, f.profile.ID, []games.Scene{f.scene("Second Scene")})
		}},
		{"PutRegions", func() error {
			return f.s.PutRegions(ctx, f.profile.ID, []games.Region{f.region(b.Scene.ID)})
		}},
		{"PutMessages", func() error {
			return f.s.PutMessages(ctx, f.profile.ID, b.Messages, b.Pages)
		}},
		{"PutEdges", func() error {
			return f.s.PutEdges(ctx, f.profile.ID, []games.MessageEdge{f.edge(b.Messages[0].ID, b.Messages[1].ID)})
		}},
		{"PutSpeakerAssertions", func() error {
			return f.s.PutSpeakerAssertions(ctx, f.profile.ID, []games.SpeakerAssertion{f.assertion(b.Messages[1].ID, extraChar.ID)})
		}},
		{"UpdateMessageSpeechText", func() error {
			_, err := f.s.UpdateMessageSpeechText(ctx, f.profile.ID, b.Messages[0].ID, "Speak it differently.", f.version())
			return err
		}},
		{"SetProfileStatus", func() error {
			return f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version())
		}},
		{"Publish", func() error {
			_, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
			return err
		}},
	}
	for _, st := range steps {
		before := f.version()
		if err := st.run(); err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		if after := f.version(); after != before+1 {
			t.Fatalf("%s moved the row version %d → %d, want exactly one increment", st.name, before, after)
		}
	}

	// A *rejected* write must not move it either: docs/spec/08's
	// PROFILE_REVISION_CONFLICT is only actionable if the loser's view of the
	// counter is still the truth.
	before := f.version()
	if err := f.s.PutCharacters(ctx, f.profile.ID, []games.Character{f.character("Frozen")}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("PutCharacters on the published revision = %v, want ErrImmutable", err)
	}
	if got := f.version(); got != before {
		t.Fatalf("a refused write moved the row version to %d, want %d", got, before)
	}

	// Deriving a revision writes the *new* row, not the parent: an editor holding
	// the published revision's token must not find it spent by somebody else
	// starting the next release.
	child, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if got := f.version(); got != before {
		t.Fatalf("NewRevision moved the parent's row version to %d, want %d", got, before)
	}
	if got := mustVersion(t, f.s, ctx, child.ID); got != freshRowVersion {
		t.Fatalf("a fresh revision starts at row version %d, want %d (0004's DEFAULT: creation is the first write)", got, freshRowVersion)
	}
}

// TestStaleRowVersionIsRejectedOnEveryWritePath covers docs/spec/12's "no
// published edit without valid revision token" across the whole write surface.
//
// The paths split into two kinds, and both are asserted, because pretending they
// are the same kind is how a reviewer ends up trusting a lock that does not
// exist:
//
//  1. Writes that take the caller's token (SetProfileStatus, Publish,
//     UpdateMessageSpeechText) must refuse a stale one and change nothing.
//  2. Writes that take no token (the Put* batch methods) cannot be handed a stale
//     token by construction: they load the version and compare-and-swap it inside
//     the one BEGIN IMMEDIATE transaction that performs them. What that
//     construction owes the caller instead is that no accepted write is ever lost,
//     so the second half of this test runs concurrent writers and requires every
//     one of them to be counted.
func TestStaleRowVersionIsRejectedOnEveryWritePath(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)
	current := f.version()

	// 1. Every token-bearing write, for every stale value worth naming: never
	// issued, the version a row had at creation, one below and above the current
	// value, and a number from a client that invented one.
	for _, stale := range []int64{0, 1, current - 1, current + 1, current + 99} {
		if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, stale); !errors.Is(err, ErrRevisionConflict) {
			t.Errorf("SetProfileStatus with token %d (current %d) = %v, want ErrRevisionConflict", stale, current, err)
		}
		if _, err := f.s.Publish(ctx, f.profile.ID, stale, "curator@local"); !errors.Is(err, ErrRevisionConflict) {
			t.Errorf("Publish with token %d = %v, want ErrRevisionConflict", stale, err)
		}
		if _, err := f.s.UpdateMessageSpeechText(ctx, f.profile.ID, b.Messages[0].ID, "nope", stale); !errors.Is(err, ErrRevisionConflict) {
			t.Errorf("UpdateMessageSpeechText with token %d = %v, want ErrRevisionConflict", stale, err)
		}
		if got := f.version(); got != current {
			t.Fatalf("a refused write moved the row version to %d, want %d", got, current)
		}
	}
	if p, _, err := f.s.GetProfile(ctx, f.profile.ID); err != nil || p.Status != games.StatusDraft {
		t.Errorf("status after the refused writes = %+v, %v; want the revision still draft", p, err)
	}

	// 2a. The guard primitive every write funnels through, driven directly, so a
	// future write path that forgets to pass a token cannot silently become
	// unguarded: the guard itself refuses a stale compare.
	if err := f.s.InTx(ctx, func(tx *Tx) error {
		const op = "stale-guard test"
		have, err := rowVersion(ctx, tx, op, f.profile.ID)
		if err != nil {
			return err
		}
		if _, err := tx.bumpRowVersion(ctx, op, f.profile.ID, have+3); !errors.Is(err, ErrRevisionConflict) {
			return fmt.Errorf("bumpRowVersion with a stale token = %v, want ErrRevisionConflict", err)
		}
		if _, err := tx.guardedProfileUpdate(ctx, op, f.profile.ID, have+3, "status = 'review'"); !errors.Is(err, ErrRevisionConflict) {
			return fmt.Errorf("guardedProfileUpdate with a stale token = %v, want ErrRevisionConflict", err)
		}
		// A guard that misses because the row is *gone* says so, rather than
		// pretending the caller's token was wrong.
		if _, err := tx.bumpRowVersion(ctx, op, games.NewID(), 1); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("guard on a deleted revision = %v, want ErrNotFound", err)
		}
		// And inside one transaction the guard is still satisfied by the version
		// that transaction loaded — that is what Publish relies on.
		if _, err := tx.bumpRowVersion(ctx, op, f.profile.ID, have); err != nil {
			return err
		}
		return errors.New("roll the test transaction back")
	}); err == nil || !strings.Contains(err.Error(), "roll the test") {
		t.Fatalf("InTx = %v, want the sentinel error so the guard's bump is rolled back with it", err)
	}
	if got := f.version(); got != current {
		t.Fatalf("a rolled-back transaction left the row version at %d, want %d", got, current)
	}

	// 2b. The tokenless write paths: six concurrent writers, six increments. A lost
	// update would land below the expected value, which is precisely the failure
	// BEGIN IMMEDIATE exists to make impossible.
	const writers = 6
	var wg sync.WaitGroup
	errs := make([]error, writers)
	start := make(chan struct{})
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			errs[w] = f.s.PutCharacters(ctx, f.profile.ID, []games.Character{f.character(fmt.Sprintf("Writer %d", w))})
		}(w)
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent PutCharacters: %v", err)
		}
	}
	if got := f.version(); got != current+writers {
		t.Fatalf("row version after %d concurrent writes = %d, want %d (none lost)", writers, got, current+writers)
	}
}

// TestPartialUniqueIndexAllowsOnePublishedRevisionPerEdition proves the schema,
// not just the code, holds docs/spec/02's invariant "a session asks what is live
// and gets one answer". Every write here goes straight to the database, through
// none of the repositories.
func TestPartialUniqueIndexAllowsOnePublishedRevisionPerEdition(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatal(err)
	}

	// A second revision promoted by hand: refused, by the partial index.
	wantOnePublishedIndex(t, func() error {
		_, err := f.s.db.Exec(
			`INSERT INTO profiles (id, edition_id, revision_num, status, publication_author)
			 VALUES (?, ?, 2, 'published', 'hand')`, games.NewID(), f.edition.ID)
		return err
	}, "inserting a second published revision")
	second := games.NewID()
	mustExec(t, f.s.db,
		`INSERT INTO profiles (id, edition_id, revision_num, status, publication_author)
		 VALUES (?, ?, 2, 'draft', 'hand')`, second, f.edition.ID)
	wantOnePublishedIndex(t, func() error {
		_, err := f.s.db.Exec(`UPDATE profiles SET status = 'published' WHERE id = ?`, second)
		return err
	}, "promoting a second revision by raw SQL")

	// Partial, so every other revision of the same edition is untouched by it: two
	// drafts and an archived revision coexist as they always did.
	mustExec(t, f.s.db,
		`INSERT INTO profiles (id, edition_id, revision_num, status, publication_author)
		 VALUES (?, ?, 3, 'draft', 'hand'), (?, ?, 4, 'archived', 'hand')`,
		games.NewID(), f.edition.ID, games.NewID(), f.edition.ID)

	// And another edition may publish the same way — the constraint is per edition,
	// which is what makes the pointer's "one answer" a scoped question.
	other := f.editionWithRegion("EU")
	mustExec(t, f.s.db,
		`INSERT INTO profiles (id, edition_id, revision_num, status, publication_author)
		 VALUES (?, ?, 1, 'published', 'hand')`, games.NewID(), other.ID)

	// The repositories never trip it, because Publish retires the incumbent before
	// promoting the successor inside one transaction — and the pointer ends up
	// naming exactly the one published row.
	live, _, err := f.s.PublishedProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM profiles WHERE edition_id = ? AND status = 'published'`, f.edition.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("%d published revisions of one edition, want 1", n)
	}
	var pointer string
	if err := f.s.db.QueryRow(`SELECT published_profile_id FROM editions WHERE id = ?`, f.edition.ID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	if pointer != live.ID {
		t.Errorf("pointer = %s, PublishedProfile = %s: they must name the same revision", pointer, live.ID)
	}
}

// wantOnePublishedIndex asserts that a write failed *because of* the one-published-
// revision-per-edition index — which is the same constraint whether SQLite names the
// index or the columns it covers, and which must not be confused with some other
// constraint (a CHECK, a primary key) that would let an unrelated bug pass.
func wantOnePublishedIndex(t *testing.T, run func() error, what string) {
	t.Helper()
	err := run()
	if err == nil {
		t.Fatalf("%s succeeded, want profiles_one_published_idx to refuse it", what)
	}
	msg := err.Error()
	if !strings.Contains(msg, "profiles_one_published_idx") &&
		!strings.Contains(msg, "UNIQUE constraint failed: profiles.edition_id") {
		t.Fatalf("%s = %v, want the one-published-revision-per-edition unique index", what, msg)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		if code := se.Code() & 0xff; code != sqlite3.SQLITE_CONSTRAINT {
			t.Fatalf("%s failed with SQLite code %d, want a constraint violation", what, se.Code())
		}
	}
}

// TestRejectedPublishLeavesAnAuditRowAndChangesNothing is the half of the audit
// trail the publication transaction structurally cannot carry: docs/spec/03's
// review screen and docs/spec/16's evidence bundles have to answer "was this ever
// offered for publication, and why not", and the answer must come from the
// database rather than from log retention. It therefore has to survive the
// rollback of the attempt that produced it.
func TestRejectedPublishLeavesAnAuditRowAndChangesNothing(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// One finding, produced the way a bad import produces it: every line attributed
	// to a character that belongs to another revision, which the schema's foreign
	// keys happily accept and only the publication gate refuses (docs/spec/02).
	other := f.editionWithRegion("EU")
	otherProfile, err := f.s.CreateDraftProfile(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := f.character("Foreign")
	foreign.ProfileID = otherProfile.ID
	if err := f.s.PutCharacters(ctx, otherProfile.ID, []games.Character{foreign}); err != nil {
		t.Fatal(err)
	}
	b := f.bundle(2)
	for i := range b.Messages {
		b.Messages[i].SpeakerID = foreign.ID
	}
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	token := f.version()

	_, err = f.s.Publish(ctx, f.profile.ID, token, "curator@local")
	var pe *PublishError
	if !errors.As(err, &pe) {
		t.Fatalf("Publish = %v, want a *PublishError", err)
	}

	// Nothing changed: not the status, not the token, not the edition's pointer.
	p, rev, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != games.StatusReview || rev != token {
		t.Errorf("refused publication left status %q version %d, want %q %d", p.Status, rev, games.StatusReview, token)
	}
	if _, _, err := f.s.PublishedProfile(ctx, f.edition.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("the refused revision entered service anyway: %v", err)
	}

	// The refusal itself is recorded, in a transaction of its own, and it is the
	// only audit row so far.
	actions, err := f.s.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 {
		t.Fatalf("audit trail = %+v, want exactly the one rejection", actions)
	}
	a := actions[0]
	if a.Action != "rejected" || a.Result != "rejected" || a.ProfileID != f.profile.ID || a.Actor != "curator@local" {
		t.Errorf("rejection record = %+v, want a rejected publish of this revision by the curator who asked", a)
	}
	if a.ExpectedRowVersion != token || a.ResultRowVersion != token {
		t.Errorf("rejection record versions = (%d, %d), want (%d, %d): a refusal moved nothing",
			a.ExpectedRowVersion, a.ResultRowVersion, token, token)
	}
	if len(a.Problems) == 0 || !strings.Contains(strings.Join(a.Problems, " "), "speaker_id") {
		t.Errorf("rejection record problems = %v, want the gate's findings", a.Problems)
	}
	// The findings are in the file, not just in the Go value: that is what an
	// evidence bundle reads after the process is gone.
	var detail string
	if err := f.s.db.QueryRow(`SELECT detail_json FROM publish_actions WHERE profile_id = ?`, f.profile.ID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var d publishActionDetail
	if err := json.Unmarshal([]byte(detail), &d); err != nil {
		t.Fatalf("detail_json = %q: %v", detail, err)
	}
	if len(d.Problems) == 0 {
		t.Errorf("detail_json %q carries no problem list", detail)
	}

	// Fix the content and publish: the new record is appended, the refusal is
	// still there, and the history reads as a sequence of decisions.
	if err := f.s.PutMessages(ctx, f.profile.ID, restoreSpeakers(b, foreign.ID, b.Messages), b.Pages); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish after the repair: %v", err)
	}
	actions, err = f.s.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 2 || actions[0].Action != "publish" || actions[1].Action != "rejected" {
		t.Fatalf("audit trail = %+v, want the publication followed by the older rejection", actions)
	}
}

// TestUpgradeFromThePriorSchemaBackfillsThePointer is docs/spec/02's "prior
// database" acceptance case: a file written by the previous build, holding a
// published revision and no pointer column, must come up still serving that
// revision — not silently un-publish its live release.
func TestUpgradeFromThePriorSchemaBackfillsThePointer(t *testing.T) {
	s := openIn(t)
	ctx := testCtx(t)
	prior := priorSchemaVersion(t)
	migrateTo(t, s, prior)

	gameID, editionID, emptyID := games.NewID(), games.NewID(), games.NewID()
	liveID, oldID, draftID := games.NewID(), games.NewID(), games.NewID()
	insertGame(t, s.db, gameID, "synthetic-legacy")
	insertEdition(t, s.db, editionID, gameID)
	mustExec(t, s.db,
		`INSERT INTO editions (id, game_id, region, language, revision, locale) VALUES (?, ?, 'EU', 'en', 'rev0', 'en_US')`,
		emptyID, gameID)
	insertProfile(t, s.db, liveID, editionID, 1, "published", nil)
	insertProfile(t, s.db, oldID, editionID, 2, "archived", nil)
	insertProfile(t, s.db, draftID, emptyID, 1, "draft", nil)
	mustExec(t, s.db,
		`UPDATE profiles SET manifest_digest = ?, published_at = ?, publication_author = 'curator@local'
		 WHERE id = ?`, strings.Repeat("a", games.MaxSHA256Hex), nowRFC3339(), liveID)

	// Prove the fixture really is the prior schema: the pointer column does not
	// exist yet, so what follows can only be the new migration's doing.
	if _, err := s.db.Exec(`SELECT published_profile_id FROM editions`); err == nil {
		t.Fatal("the prior schema already had editions.published_profile_id — migrateTo applied too much")
	}

	applied, err := s.Migrate(ctx)
	if err != nil {
		t.Fatalf("Migrate on a prior database: %v", err)
	}
	if want := MigrationNames()[prior:]; strings.Join(applied, ",") != strings.Join(want, ",") {
		t.Fatalf("upgrade applied %v, want only the pending %v", applied, want)
	}

	var pointer sql.NullString
	if err := s.db.QueryRow(`SELECT published_profile_id FROM editions WHERE id = ?`, editionID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	if !pointer.Valid || pointer.String != liveID {
		t.Fatalf("backfilled pointer = %v, want %s: the live release must survive the upgrade", pointer, liveID)
	}
	if err := s.db.QueryRow(`SELECT published_profile_id FROM editions WHERE id = ?`, emptyID).Scan(&pointer); err != nil {
		t.Fatal(err)
	}
	if pointer.Valid {
		t.Errorf("an edition with no published revision got a backfilled pointer to %q", pointer.String)
	}

	// The repositories work against rows this build never wrote: the counter is
	// there (0004's DEFAULT), the pointer answers, and retiring the legacy
	// revision moves both representations and audits itself.
	got, version, err := s.PublishedProfile(ctx, editionID)
	if err != nil {
		t.Fatalf("PublishedProfile after the upgrade: %v", err)
	}
	if got.ID != liveID {
		t.Fatalf("served %s, want %s", got.ID, liveID)
	}
	if version != freshRowVersion {
		t.Errorf("legacy row version = %d, want %d", version, freshRowVersion)
	}
	if err := s.SetProfileStatus(ctx, liveID, games.StatusArchived, freshRowVersion); err != nil {
		t.Fatalf("retiring the legacy revision: %v", err)
	}
	if v := mustVersion(t, s, ctx, liveID); v != freshRowVersion+1 {
		t.Errorf("row version after one write on an upgraded row = %d, want %d", v, freshRowVersion+1)
	}
	if _, _, err := s.PublishedProfile(ctx, editionID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PublishedProfile after retiring the upgraded revision = %v, want ErrNotFound", err)
	}
	// The audit trail accepts records about rows that predate the table, which is
	// what makes an upgraded appliance's history continuous rather than starting
	// from the migration date.
	actions, err := s.PublishActions(ctx, editionID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Action != "archive" || actions[0].ProfileID != liveID {
		t.Errorf("audit trail after retiring an upgraded revision = %+v, want its archive record", actions)
	}
	if err := s.IntegrityCheck(ctx); err != nil {
		t.Errorf("IntegrityCheck after the upgrade: %v", err)
	}
}

// TestPruningTheEventLogChangesNothingAboutPublicationFacts is the reason
// ADR-0015 exists. The `events` log is bounded and pruned by age by design
// (docs/spec/02), so when an earlier build kept the counter, the pointer and the
// audit trail in it, emptying the log restarted a revision's counter at zero — and
// a months-old If-Match token matched again. That is a fail-open concurrency bug,
// the one direction docs/spec/12's "no published edit without valid revision
// token" must never allow. It is now unrepresentable, and this test is the proof.
func TestPruningTheEventLogChangesNothingAboutPublicationFacts(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(2)
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	first, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	if err != nil {
		t.Fatal(err)
	}

	// A second release, so the pointer has actually moved and the audit trail has
	// a publish, an archive and a pointer swap to lose.
	child, err := f.s.NewRevision(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetProfileStatus(ctx, child.ID, games.StatusReview, mustVersion(t, f.s, ctx, child.ID)); err != nil {
		t.Fatal(err)
	}
	second, err := f.s.Publish(ctx, child.ID, mustVersion(t, f.s, ctx, child.ID), "curator@local")
	if err != nil {
		t.Fatal(err)
	}

	// Operational history exists, and the UI feed depends on it.
	for i := 0; i < 5; i++ {
		mustExec(t, f.s.db, eventBaseline(fmt.Sprintf("ev-prune-%d", i)).
			with("name", "'speech.started'").insert())
	}
	if n := countRows(t, f.s.db, "events"); n != 5 {
		t.Fatalf("fixture events = %d, want 5", n)
	}

	beforeVersion := mustVersion(t, f.s, ctx, second.ID)
	beforeActions, err := f.s.PublishActions(ctx, f.edition.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(beforeActions) != 3 {
		t.Fatalf("audit trail = %+v, want publish, archive, publish", beforeActions)
	}

	// Retention runs: the whole log goes.
	mustExec(t, f.s.db, `DELETE FROM events`)
	if n := countRows(t, f.s.db, "events"); n != 0 {
		t.Fatalf("events after pruning = %d, want 0", n)
	}

	// Every publication fact still reads the same.
	live, version, err := f.s.PublishedProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("PublishedProfile after pruning the log: %v", err)
	}
	if live.ID != second.ID || version != beforeVersion {
		t.Fatalf("after pruning, the edition serves %s at version %d, want %s at %d", live.ID, version, second.ID, beforeVersion)
	}
	afterActions, err := f.s.PublishActions(ctx, f.edition.ID, 20)
	if err != nil {
		t.Fatalf("PublishActions after pruning the log: %v", err)
	}
	if fmt.Sprint(afterActions) != fmt.Sprint(beforeActions) {
		t.Errorf("audit trail changed when the log was pruned:\nbefore %+v\nafter  %+v", beforeActions, afterActions)
	}

	// The counter does not restart: the next write continues from where the
	// database says it was, and the token issued before pruning is still stale.
	if err := f.s.PutCharacters(ctx, second.ID, []games.Character{f.character("After pruning")}); !errors.Is(err, ErrImmutable) {
		t.Fatalf("PutCharacters on the live revision = %v, want ErrImmutable (it is published)", err)
	}
	draft, err := f.s.NewRevision(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v := mustVersion(t, f.s, ctx, draft.ID); v != freshRowVersion {
		t.Fatalf("a revision created after pruning starts at %d, want %d: creation is one write, not a restart from zero", v, freshRowVersion)
	}
	if err := f.s.SetProfileStatus(ctx, second.ID, games.StatusArchived, beforeVersion-1); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("the pre-pruning token, used one version low = %v, want ErrRevisionConflict", err)
	}
	if err := f.s.SetProfileStatus(ctx, second.ID, games.StatusArchived, beforeVersion); err != nil {
		t.Fatalf("retiring with the true post-pruning token: %v", err)
	}
	if v := mustVersion(t, f.s, ctx, second.ID); v != beforeVersion+1 {
		t.Errorf("row version after one post-pruning write = %d, want %d", v, beforeVersion+1)
	}
}
