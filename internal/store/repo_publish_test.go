package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestConcurrentPublishLetsExactlyOneWin is the optimistic-concurrency contract
// docs/spec/08 names PROFILE_REVISION_CONFLICT for, run for real: two publishers
// read the same row version and both call Publish. One ships; the other is told
// its token is stale rather than being silently ignored or, worse, also shipping
// and leaving the edition with two published revisions.
func TestConcurrentPublishLetsExactlyOneWin(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	f.bundle(5).put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	token := f.version()

	type result struct {
		profile games.Profile
		err     error
	}
	start := make(chan struct{})
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // both goroutines race for the write lock from the same instant
			p, err := f.s.Publish(ctx, f.profile.ID, token, "curator"+strconv.Itoa(i)+"@local")
			results[i] = result{profile: p, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	var ok, conflicts, other int
	for _, r := range results {
		switch {
		case r.err == nil:
			ok++
		case errors.Is(r.err, ErrRevisionConflict):
			conflicts++
		default:
			other++
			t.Errorf("unexpected error from Publish: %v", r.err)
		}
	}
	if ok != 1 || conflicts != 1 {
		t.Fatalf("results = %+v, want exactly one success and one ErrRevisionConflict", results)
	}

	// The database agrees with what the callers were told, which is the part the
	// mock-level version of this test could not prove.
	p, rev, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != games.StatusPublished {
		t.Errorf("status = %q, want published", p.Status)
	}
	if rev != token+1 {
		t.Errorf("row version = %d, want %d: exactly one publication moved it", rev, token+1)
	}
	actions, err := f.s.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var published int
	for _, a := range actions {
		if a.Action == "publish" && a.Result == "succeeded" {
			published++
		}
	}
	if published != 1 {
		t.Errorf("audit trail holds %d successful publications, want 1", published)
	}
}

// TestPublishingAgainRetiresThePredecessor covers the swap itself: the edition
// serves the new revision, the old one is archived but stays addressable, and the
// lineage is recorded rather than implied by a timestamp.
func TestPublishingAgainRetiresThePredecessor(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	first := f.bundle(3)
	first.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	published1, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	if err != nil {
		t.Fatalf("first Publish: %v", err)
	}
	// The predecessor's version as it stands while it is the live revision; the
	// second publication must move it, because retiring it is a change an editor
	// holding its ETag has to be able to see.
	beforeSwap := mustVersion(t, f.s, ctx, published1.ID)

	// Derive, edit, review, publish: the ordinary second release.
	second, err := f.s.NewRevision(ctx, published1.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	edited, err := f.s.GetProfileContent(ctx, second.ID)
	if err != nil {
		t.Fatal(err)
	}
	changed := edited.Messages[0]
	changed.SpeechText = "Reworded for the second release."
	if err := f.s.PutMessages(ctx, second.ID, []games.Message{changed}, nil); err != nil {
		t.Fatalf("PutMessages on the snapshot: %v", err)
	}
	if err := f.s.SetProfileStatus(ctx, second.ID, games.StatusReview, mustVersion(t, f.s, ctx, second.ID)); err != nil {
		t.Fatalf("snapshot to review: %v", err)
	}
	published2, err := f.s.Publish(ctx, second.ID, mustVersion(t, f.s, ctx, second.ID), "curator@local")
	if err != nil {
		t.Fatalf("second Publish: %v", err)
	}

	// The pointer moved.
	current, _, err := f.s.PublishedProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("PublishedProfile: %v", err)
	}
	if current.ID != published2.ID {
		t.Errorf("edition serves %s, want the second revision %s", current.ID, published2.ID)
	}
	// The predecessor is archived, keeps its digest, and is still readable: audio
	// cached under it and the occurrences recorded against it have to survive
	// (docs/spec/02).
	old, oldVersion, err := f.s.GetProfile(ctx, published1.ID)
	if err != nil {
		t.Fatalf("the retired revision is unreadable: %v", err)
	}
	if old.Status != games.StatusArchived {
		t.Errorf("predecessor status = %q, want archived", old.Status)
	}
	if old.ManifestDigest != published1.ManifestDigest || old.PublishedAt == "" {
		t.Errorf("predecessor lost its publication facts: %+v", old)
	}
	if oldVersion != beforeSwap+1 {
		t.Errorf("predecessor row version = %d, want %d: an editor holding its token must find out it stopped being live",
			oldVersion, beforeSwap+1)
	}
	// Two published revisions for one edition is impossible at the schema level
	// too; the swap ordered the writes rather than relying on the index to tidy up.
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM profiles WHERE edition_id = ? AND status = 'published'`,
		f.edition.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("edition has %d published revisions, want 1", n)
	}
	// The audit trail shows both decisions and names what was superseded.
	actions, err := f.s.PublishActions(ctx, f.edition.ID, 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawPublish2, sawArchive1 bool
	for _, a := range actions {
		switch {
		case a.Action == "publish" && a.ProfileID == published2.ID:
			sawPublish2 = true
			if a.SupersededProfileID != published1.ID {
				t.Errorf("the second publication names %q as superseded, want %q", a.SupersededProfileID, published1.ID)
			}
			if a.ManifestDigest != published2.ManifestDigest {
				t.Errorf("audit digest %q disagrees with the published row %q", a.ManifestDigest, published2.ManifestDigest)
			}
		case a.Action == "archive" && a.ProfileID == published1.ID:
			sawArchive1 = true
		}
	}
	if !sawPublish2 || !sawArchive1 {
		t.Errorf("audit trail = %+v, want the second publication and the retirement of the first", actions)
	}
	// A different digest for different content: docs/spec/01 key invariant 4 makes
	// this value part of every audio cache key.
	if published1.ManifestDigest == published2.ManifestDigest {
		t.Error("two revisions with different speech text produced the same manifest digest")
	}
}

// TestPublishReportsEveryProblem is the gate's contract with the review screen
// (docs/spec/03): a curator gets the whole list at once, not one finding per
// retry.
func TestPublishReportsEveryProblem(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	b := f.bundle(4)
	b.put(f)
	// Problem 1: a source whose rights position is unresolved. docs/spec/14 forbids
	// shipping derived content from it, and docs/spec/08 has an error code for it.
	suspect := f.source("https://example.invalid/transcript", shaB, games.RightsUnknown)
	// Source.Validate refuses file:// with unknown rights, so an external page is
	// the honest way to reach this state; it needs the type set accordingly.
	suspect.SourceType = games.SourceWebPage
	// Problem 2 and 3: two lines whose speaker belongs to a *different* revision.
	// The FK only proves the character exists somewhere, so this is the state
	// SQLite cannot rule out and the gate must.
	abroad := f.character("Someone else's sailor")
	otherEdition := f.editionWithRegion("AU")
	otherDraft, err := f.s.CreateDraftProfile(ctx, otherEdition.ID)
	if err != nil {
		t.Fatal(err)
	}
	abroad.ProfileID = otherDraft.ID
	if err := f.s.PutCharacters(ctx, otherDraft.ID, []games.Character{abroad}); err != nil {
		t.Fatalf("put the foreign character: %v", err)
	}
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{suspect}); err != nil {
		t.Fatal(err)
	}
	messages := b.Messages
	for i := range messages {
		messages[i].SourceID = suspect.ID
		messages[i].SourceMessageID = b.Messages[i].SourceMessageID + "-x"
		messages[i].SpeakerID = abroad.ID
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, messages, nil); err != nil {
		t.Fatalf("putting the mis-attributed messages: %v", err)
	}
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	before := f.version()

	_, err = f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	if !errors.Is(err, ErrUnpublishable) {
		t.Fatalf("Publish = %v, want an ErrUnpublishable refusal", err)
	}
	var pe *PublishError
	if !errors.As(err, &pe) {
		t.Fatalf("error %v is not a *PublishError", err)
	}
	if len(pe.Problems) < 2 {
		t.Fatalf("gate reported %d problem(s) (%v), want every finding", len(pe.Problems), err)
	}
	joined := strings.Join(pe.Problems, "\n")
	for _, want := range []string{"rights_status", "speaker_id"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the findings omit %q:\n%s", want, joined)
		}
	}
	if !strings.Contains(err.Error(), pe.Problems[0]) || !strings.Contains(err.Error(), pe.Problems[1]) {
		t.Errorf("Error() does not list every finding: %v", err)
	}

	// A refusal changes nothing about the revision: it is still in review with the
	// same version, so the curator can fix the rows and retry with the token they
	// already hold.
	p, rev, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != games.StatusReview || rev != before {
		t.Errorf("after a refusal the revision is %+v version %d, want review version %d", p, rev, before)
	}

	// The refusal itself is durable evidence, with the findings attached.
	actions, err := f.s.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sawRejection bool
	for _, a := range actions {
		if a.Action == "rejected" && a.Result == "rejected" && len(a.Problems) == len(pe.Problems) {
			sawRejection = true
		}
	}
	if !sawRejection {
		t.Errorf("the gate's refusal was not audited with its findings: %+v", actions)
	}

	// Fix everything and the same token publishes: the refusal was advice, not a
	// lock.
	if err := f.s.PutMessages(ctx, f.profile.ID, restoreSpeakers(b, abroad.ID, messages), nil); err != nil {
		t.Fatalf("restoring the attributions: %v", err)
	}
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish after fixing the findings: %v", err)
	}
}

// restoreSpeakers points every message back at the revision's own character and
// clears the unlicensed source, which is the repair the findings described.
func restoreSpeakers(b *contentBundle, foreign string, messages []games.Message) []games.Message {
	out := append([]games.Message(nil), messages...)
	for i := range out {
		if out[i].SpeakerID == foreign {
			out[i].SpeakerID = b.Character.ID
		}
		out[i].SourceID = b.Source.ID
		out[i].SourceMessageID = b.Messages[i].SourceMessageID
	}
	return out
}

// TestPublishNeedsReviewAndAnAuthor covers the two things the audit trail cannot
// function without: a revision that went through review, and a name attached to
// the decision.
func TestPublishNeedsReviewAndAnAuthor(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	f.bundle(2).put(f)

	// Straight from draft: refused, and the finding is part of the list rather than
	// a separate kind of failure, so the review screen can show it beside the rest.
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); !errors.Is(err, ErrUnpublishable) {
		t.Fatalf("Publish from draft = %v, want a gate refusal", err)
	}
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	for _, blank := range []string{"", "   "} {
		if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), blank); err == nil || errors.Is(err, ErrUnpublishable) {
			t.Errorf("Publish with author %q = %v, want a plain author-required error", blank, err)
		}
	}
	// A revision with no content is not publishable either: "it narrates nothing"
	// is a finding, not a success.
	empty, err := f.s.CreateDraftProfile(ctx, f.editionWithRegion("JP").ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetProfileStatus(ctx, empty.ID, games.StatusReview, mustVersion(t, f.s, ctx, empty.ID)); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.Publish(ctx, empty.ID, mustVersion(t, f.s, ctx, empty.ID), "curator@local")
	if !errors.Is(err, ErrUnpublishable) || !strings.Contains(err.Error(), "no messages") {
		t.Errorf("Publish of an empty revision = %v, want the no-messages finding", err)
	}
	// The finding must name the ways out (ADR-0077, gap audit G4): a
	// research-only revision is not a dead end, and the message says how to
	// add dialogue — disc import, an approved web source, manual entry.
	for _, want := range []string{"import the disc", "propose_web_import", "manually"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the no-messages finding does not name %q: %s", want, err)
		}
	}
}

// TestPublishedProfileFollowsThePointerNotTheStatus pins what "the edition's live
// revision" means in this schema: the row `editions.published_profile_id` names
// (docs/spec/02 "Transaction workflows": "atomic publication pointer swap"; ADR-0015).
// The status column is a revision's own state, not a second source of truth about
// what is served, so this test asserts the three consequences of that choice —
// the ones the earlier status-derived lookup got wrong:
//
//   - a revision whose status says published, but that the edition does not name,
//     is NOT served: no hand-written row can enter service without the audited
//     act that moves the pointer;
//   - a raw status edit elsewhere in the database cannot change which revision is
//     served, and a raw status edit *of* the served revision is reported as the
//     contradiction it is rather than silently served or silently ignored;
//   - the version that comes back with the live revision is the column's value,
//     so a revision that exists in the file has a real token even if this build
//     never wrote it — 0004's DEFAULT 1, never a journal-derived 0 that a stale
//     If-Match could later match again.
func TestPublishedProfileFollowsThePointerNotTheStatus(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// Revision 2, because the fixture's own draft already holds revision 1 of this
	// edition and profiles_edition_revision_idx is UNIQUE per edition.
	legacy := games.NewID()
	mustExec(t, f.s.db,
		`INSERT INTO profiles (id, edition_id, revision_num, status, manifest_digest, published_at, publication_author)
		 VALUES (?, ?, 2, 'published', ?, ?, 'tester')`,
		legacy, f.edition.ID, strings.Repeat("a", games.MaxSHA256Hex), nowRFC3339())

	// 1. Published-in-name-only: no pointer names it, so the edition serves nothing.
	if _, _, err := f.s.PublishedProfile(ctx, f.edition.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("PublishedProfile with a NULL pointer = %v, want ErrNotFound: the pointer, not the status, decides what is served", err)
	}

	// 2. Name it and it is served, with the version the column holds.
	mustExec(t, f.s.db, `UPDATE editions SET published_profile_id = ? WHERE id = ?`, legacy, f.edition.ID)
	got, version, err := f.s.PublishedProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("PublishedProfile: %v", err)
	}
	if got.ID != legacy {
		t.Fatalf("served %s, want the revision the pointer names (%s)", got.ID, legacy)
	}
	if version != freshRowVersion {
		t.Errorf("row version of the live revision = %d, want %d from profiles.row_version", version, freshRowVersion)
	}

	// 3. A raw status edit of some *other* revision changes nothing about service.
	mustExec(t, f.s.db, `UPDATE profiles SET status = 'review' WHERE id = ?`, f.profile.ID)
	if again, _, err := f.s.PublishedProfile(ctx, f.edition.ID); err != nil || again.ID != legacy {
		t.Errorf("PublishedProfile after an unrelated status edit = %+v, %v; want %s", again, err, legacy)
	}

	// 4. A raw status edit of the live revision, without the pointer, is a
	// contradiction between the two representations and is refused: docs/spec/13
	// wants a session to refuse to start rather than narrate a revision the review
	// screen says is no longer published.
	mustExec(t, f.s.db, `UPDATE profiles SET status = 'archived' WHERE id = ?`, legacy)
	if _, _, err := f.s.PublishedProfile(ctx, f.edition.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("PublishedProfile with the pointer naming an archived revision = %v, want ErrConflict", err)
	}
	mustExec(t, f.s.db, `UPDATE profiles SET status = 'published' WHERE id = ?`, legacy)
	if again, _, err := f.s.PublishedProfile(ctx, f.edition.ID); err != nil || again.ID != legacy {
		t.Errorf("PublishedProfile after restoring the status = %+v, %v; want %s served again", again, err, legacy)
	}

	// 5. Retirement through the repositories moves both representations at once and
	// leaves the audit row that says so — the honest order the RESTRICT foreign key
	// in 0004 forces.
	if err := f.s.SetProfileStatus(ctx, legacy, games.StatusArchived, freshRowVersion); err != nil {
		t.Fatalf("SetProfileStatus(published to archived): %v", err)
	}
	if _, _, err := f.s.PublishedProfile(ctx, f.edition.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PublishedProfile after the live revision was retired = %v, want ErrNotFound", err)
	}
	var pointer sql.NullString
	if err := f.s.db.QueryRow(`SELECT published_profile_id FROM editions WHERE id = ?`, f.edition.ID).Scan(&pointer); err != nil {
		t.Fatalf("read the pointer: %v", err)
	}
	if pointer.Valid {
		t.Errorf("pointer still names %q after retirement, want NULL", pointer.String)
	}
	actions, err := f.s.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) != 1 || actions[0].Action != "archive" || actions[0].ProfileID != legacy {
		t.Errorf("audit trail = %+v, want one archive record for %s", actions, legacy)
	}
}

// TestPublishProblemsAreCappedAndCounted guards the bounded-response rule
// (AGENTS.md "bounded queues"): a revision with thousands of findings must not
// produce a thousands-long error, and must still say how many there were.
func TestPublishProblemsAreCappedAndCounted(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	otherEdition := f.editionWithRegion("NZ")
	other, err := f.s.CreateDraftProfile(ctx, otherEdition.ID)
	if err != nil {
		t.Fatal(err)
	}
	foreign := f.character("Foreign")
	foreign.ProfileID = other.ID
	if err := f.s.PutCharacters(ctx, other.ID, []games.Character{foreign}); err != nil {
		t.Fatal(err)
	}

	// More findings than maxListedProblems, so the gate has to bound the list it
	// returns while still stating the exact total.
	const wantTotal = maxListedProblems + 10
	b := f.bundle(wantTotal)
	b.put(f)
	messages := b.Messages
	for i := range messages {
		messages[i].SpeakerID = foreign.ID
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, messages, b.Pages); err != nil {
		t.Fatal(err)
	}
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}

	_, err = f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	var pe *PublishError
	if !errors.As(err, &pe) {
		t.Fatalf("Publish = %v, want a *PublishError", err)
	}
	joined := strings.Join(pe.Problems, "\n")
	wantTail := fmt.Sprintf("and %d more (%d total)", wantTotal-maxListedProblems, wantTotal)
	if !strings.Contains(joined, wantTail) {
		t.Errorf("findings do not state the exact remainder (%q):\n%s", wantTail, joined)
	}
	if len(pe.Problems) > 4 {
		t.Errorf("the gate returned %d findings, want a bounded few: %v", len(pe.Problems), pe.Problems)
	}
	// The audit row carries the same bounded list, not an unbounded one.
	actions, err := f.s.PublishActions(ctx, f.edition.ID, 5)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(actions[0].Problems)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 {
		t.Errorf("audited findings are %d bytes, want a bounded list", len(raw))
	}
}

// mustVersion reads a profile's row version, for tests that hold an id rather than
// a fixture.
func mustVersion(t *testing.T, s *Store, ctx context.Context, id string) int64 {
	t.Helper()
	_, rev, err := s.GetProfile(ctx, id)
	if err != nil {
		t.Fatalf("GetProfile(%s): %v", id, err)
	}
	return rev
}
