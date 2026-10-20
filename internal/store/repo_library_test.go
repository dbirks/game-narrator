package store

// ADR-0107 (gap G14) store coverage: the disable switch, the soft delete with
// its retention window, the admin purge (with the cascade and the speech-cache
// provenance), and the clone with its two-direction provenance. Every fixture
// is synthetic text typed into the test (AGENTS.md rule 5).

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestSetGameDisabledTogglesTheGameRow(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	if err := f.s.SetGameDisabled(ctx, f.game.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	g, err := f.s.GetGame(ctx, f.game.ID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if !g.Disabled {
		t.Errorf("game disabled = false after SetGameDisabled(true), want true")
	}
	// The write touched the game row and nothing else: the catalogue still
	// lists the game and reads the revision (a disabled game stays editable
	// and visible in the library — only Play refuses it, ADR-0107).
	if _, _, err := f.s.ListGames(ctx, "", 10); err != nil {
		t.Errorf("ListGames after disable: %v", err)
	}
	if _, _, err := f.s.GetProfile(ctx, f.profile.ID); err != nil {
		t.Errorf("GetProfile after disable: %v (profiles stay editable)", err)
	}

	if err := f.s.SetGameDisabled(ctx, f.game.ID, false); err != nil {
		t.Fatalf("enable: %v", err)
	}
	g, err = f.s.GetGame(ctx, f.game.ID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if g.Disabled {
		t.Errorf("game disabled = true after SetGameDisabled(false), want false")
	}

	if err := f.s.SetGameDisabled(ctx, games.NewID(), true); !errors.Is(err, ErrNotFound) {
		t.Errorf("disable of a missing game = %v, want ErrNotFound", err)
	}
}

func TestSoftDeleteProfileMarksAndHides(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(2)
	b.put(f)
	beforeVersion := f.version()

	marked, err := f.s.SoftDeleteProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("SoftDeleteProfile: %v", err)
	}
	if marked.DeletedAt == "" || marked.PurgeAfter == "" {
		t.Fatalf("marked profile carries no mark: %+v", marked)
	}
	deletedAt, err := time.Parse(time.RFC3339, marked.DeletedAt)
	if err != nil {
		t.Fatalf("deleted_at %q: %v", marked.DeletedAt, err)
	}
	purgeAfter, err := time.Parse(time.RFC3339, marked.PurgeAfter)
	if err != nil {
		t.Fatalf("purge_after %q: %v", marked.PurgeAfter, err)
	}
	if got := purgeAfter.Sub(deletedAt); got != profileDeletionRetention {
		t.Errorf("retention window = %s, want %s (deleted_at + 72h)", got, profileDeletionRetention)
	}
	if _, got, err := f.s.GetProfile(ctx, f.profile.ID); err != nil || got != beforeVersion+1 {
		t.Errorf("row version after the mark = %d, %v; want %d (the compare-and-swap bumps it)", got, err, beforeVersion+1)
	}

	// Hidden from the library list — the surface a user acts on.
	list, err := f.s.ListProfiles(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	for _, p := range list {
		if p.ID == f.profile.ID {
			t.Errorf("deleted profile %s is still listed: %+v", f.profile.ID, p)
		}
	}
	// The store's own read still answers (the API 404s, the mark stays
	// addressable by purge and by support) and the content is intact.
	p, _, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil || p.DeletedAt == "" {
		t.Fatalf("GetProfile after soft delete = %+v, %v; want the marked row", p, err)
	}
	content, err := f.s.GetProfileContent(ctx, f.profile.ID)
	if err != nil || len(content.Messages) != len(b.Messages) {
		t.Errorf("content after soft delete = %d messages, %v; want %d (the content stays on disk until the purge)",
			len(content.Messages), err, len(b.Messages))
	}

	// The same mark, read back through the list-free path: a second delete
	// names the existing mark instead of writing a second one.
	if _, err := f.s.SoftDeleteProfile(ctx, f.profile.ID); !errors.Is(err, ErrProfileAlreadyDeleted) {
		t.Errorf("second soft delete = %v, want ErrProfileAlreadyDeleted", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, games.NewID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("soft delete of a missing profile = %v, want ErrNotFound", err)
	}
}

// TestSoftDeleteRefusesShippedAndParentedRevisions is 0004's rule enforced as
// a sentinel: a revision with publication history or derived revisions cannot
// be deleted at all (the RESTRICT foreign keys are the schema's statement of
// the same rule; the guard names it before the write would meet it).
func TestSoftDeleteRefusesShippedAndParentedRevisions(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)

	// A shipped revision: the publication gate writes the history row.
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, f.profile.ID); !errors.Is(err, ErrProfileHasPublicationHistory) {
		t.Errorf("soft delete of a shipped revision = %v, want ErrProfileHasPublicationHistory", err)
	}

	// An unshipped revision with a successor: the parent cannot outlive its
	// child chain (the 0001 parent_revision RESTRICT, named). A separate
	// fixture: a published parent would name its publication history first,
	// which is the other guard's case above.
	f2 := newRepoFixture(t)
	b2 := f2.bundle(1)
	b2.put(f2)
	child, err := f2.s.NewRevision(ctx, f2.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if _, err := f2.s.SoftDeleteProfile(ctx, f2.profile.ID); !errors.Is(err, ErrProfileHasDerivedRevisions) {
		t.Errorf("soft delete of a parented revision = %v, want ErrProfileHasDerivedRevisions", err)
	}
	// The child itself is the deletable leaf.
	if _, err := f2.s.SoftDeleteProfile(ctx, child.ID); err != nil {
		t.Errorf("soft delete of the leaf revision = %v, want success (the guard names the parent, not the leaf)", err)
	}
}

// TestSoftDeleteBackstopForThePublishedPointer covers the defensive half of
// the ADR-0107 rule: a published profile can never reach SoftDeleteProfile
// (the history guard refuses it), so the edition pointer's deleted_at filter
// is a backstop for a hand-written row — and the session-start path must not
// serve a hidden revision even then.
func TestSoftDeleteBackstopForThePublishedPointer(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	published, _, err := f.s.PublishedProfile(ctx, f.edition.ID)
	if err != nil || published.ID != f.profile.ID {
		t.Fatalf("PublishedProfile = %+v, %v; want the published revision", published, err)
	}
	// Hand-write the mark (the only way a published row can carry one): the
	// pointer must stop serving it rather than start a session on it.
	mustExec(t, f.s.db,
		"UPDATE profiles SET deleted_at = ?, purge_after = ? WHERE id = ?",
		nowRFC3339(), nowRFC3339(), f.profile.ID)
	if _, _, err := f.s.PublishedProfile(ctx, f.edition.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PublishedProfile of a marked revision = %v, want ErrNotFound (a hidden revision is not a session target)", err)
	}
}

// TestPurgeProfileRetentionWindowAndCascade is the admin purge end to end:
// the retention refusal, the removal after the window, the content and FTS
// cascade, and the speech-cache rows addressed through 0017's
// original_message_id.
func TestPurgeProfileRetentionWindowAndCascade(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(2)
	b.put(f)
	// The blob the source row names (the asset store's bookkeeping row): the
	// purge reports the hash for the API's reference check but must not delete
	// what it cannot see.
	insertBlob(t, f.s.db, shaA, 2048)

	// Two cache entries keyed to the profile's messages and one voice-preview
	// entry (original_message_id NULL: the preview's identity is
	// "voice-preview:<id>", never a message id, so no purge may touch it).
	const (
		key1 = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		key2 = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
		key3 = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	)
	past := nowRFC3339()
	for _, r := range []struct {
		key, sha, messageID string
	}{
		{key1, shaC, b.Messages[0].ID},
		{key2, shaD, b.Messages[1].ID},
		{key3, shaE, ""},
	} {
		mustExec(t, f.s.db,
			`INSERT INTO speech_cache (key, sha256, size_bytes, duration_ns, sample_rate, qa_state, key_version, created_at, accessed_at, original_message_id)
			 VALUES (?, ?, 4096, 100000000, 24000, 1, 1, ?, ?, ?)`,
			r.key, r.sha, past, past, nullStrOrNil(r.messageID))
	}

	if _, err := f.s.SoftDeleteProfile(ctx, f.profile.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}

	// Inside the window the purge refuses, and the refusal names the window.
	if _, err := f.s.PurgeProfile(ctx, f.profile.ID); !errors.Is(err, ErrRetentionNotElapsed) {
		t.Fatalf("purge inside the retention window = %v, want ErrRetentionNotElapsed", err)
	}
	// Nothing changed: the mark and the content are exactly as left.
	if _, _, err := f.s.GetProfile(ctx, f.profile.ID); err != nil {
		t.Errorf("profile vanished before its retention window: %v", err)
	}

	// Backdate the mark the way a week of real time would: the store reads
	// purge_after, it does not own the clock.
	mustExec(t, f.s.db,
		"UPDATE profiles SET deleted_at = ?, purge_after = ? WHERE id = ?",
		time.Now().UTC().Add(-73*time.Hour).Format(time.RFC3339),
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339),
		f.profile.ID)

	report, err := f.s.PurgeProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("PurgeProfile after the window: %v", err)
	}
	if report.CacheEntries != 2 {
		t.Errorf("purge removed %d cache entries, want 2 (the profile's two messages; the preview entry has no message id)", report.CacheEntries)
	}
	if len(report.CacheBlobs) != 2 || report.CacheBlobs[0] != shaC || report.CacheBlobs[1] != shaD {
		t.Errorf("purge report cache blobs = %v, want [%s %s]", report.CacheBlobs, shaC, shaD)
	}
	foundSourceBlob := false
	for _, h := range report.ReferencedBlobs {
		if h == shaA {
			foundSourceBlob = true
		}
	}
	if !foundSourceBlob {
		t.Errorf("purge report referenced blobs = %v, want the source blob %s in it (the API's reference-before-delete check needs it)", report.ReferencedBlobs, shaA)
	}

	// The row and its content are gone; the edition stands (0001's RESTRICT
	// keeps a title from being dropped out from under its published work).
	if _, _, err := f.s.GetProfile(ctx, f.profile.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("purged profile still answers GetProfile: %v", err)
	}
	if n := countRows(t, f.s.db, "messages"); n != 0 {
		t.Errorf("messages = %d after the purge, want 0 (the content cascades)", n)
	}
	if n := countRows(t, f.s.db, "messages_fts"); n != 0 {
		t.Errorf("messages_fts = %d after the purge, want 0 (the sync triggers follow the cascade)", n)
	}
	if n := countRows(t, f.s.db, "editions"); n != 1 {
		t.Errorf("editions = %d after the purge, want 1 (the edition is not the profile)", n)
	}
	if n := countRows(t, f.s.db, "games"); n != 1 {
		t.Errorf("games = %d after the purge, want 1", n)
	}
	// The speech cache kept the preview entry and lost the profile's rows.
	if n := countRows(t, f.s.db, "speech_cache"); n != 1 {
		t.Errorf("speech_cache = %d rows after the purge, want 1 (the preview entry with no message id)", n)
	}
	if _, err := f.s.SpeechCacheRowCountByHash(ctx, shaC); err != nil {
		t.Errorf("SpeechCacheRowCountByHash(removed blob): %v", err)
	}
	// The blob bytes themselves are the asset store's (the API layer's
	// reference-before-delete decision); the store's index rows named them in
	// the report instead of deleting what it cannot see.
	if n := countRows(t, f.s.db, "source_blobs"); n != 1 {
		t.Errorf("source_blobs = %d after the purge, want 1 (the store does not delete blob bytes)", n)
	}
	if err := f.s.IntegrityCheck(ctx); err != nil {
		t.Errorf("IntegrityCheck after the purge: %v", err)
	}
}

// TestPurgeProfileRefusals names the two refusals a purge can give: a row that
// was never marked (purging is the second half of a two-step act) and — after
// the window — a row that gained publication history or a successor inside
// the retention window itself.
func TestPurgeProfileRefusals(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	if _, err := f.s.PurgeProfile(ctx, f.profile.ID); !errors.Is(err, ErrProfileNotDeleted) {
		t.Errorf("purge of an unmarked profile = %v, want ErrProfileNotDeleted", err)
	}
	if _, err := f.s.PurgeProfile(ctx, games.NewID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("purge of a missing profile = %v, want ErrNotFound", err)
	}

	// Publication history that appears while the mark waits for its window.
	b := f.bundle(1)
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("review: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, f.profile.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	// Re-open the history the soft delete just refused: a hand-written row
	// models the only way a publish_actions row can exist here.
	mustExec(t, f.s.db,
		`INSERT INTO publish_actions (id, profile_id, edition_id, action, author, row_version, detail_json, occurred_at)
		 VALUES (?, ?, ?, 'published', 'curator@local', 2, '{}', ?)`,
		games.NewID(), f.profile.ID, f.edition.ID, nowRFC3339())
	mustExec(t, f.s.db, "UPDATE profiles SET purge_after = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), f.profile.ID)
	if _, err := f.s.PurgeProfile(ctx, f.profile.ID); !errors.Is(err, ErrProfileHasPublicationHistory) {
		t.Errorf("purge after a history row appeared in the window = %v, want ErrProfileHasPublicationHistory", err)
	}
}

// TestLifecycleStateSurvivesRestart is the AGENTS.md restart guarantee for
// G14: a soft-deleted profile stays deleted (the mark and the hiding), a
// purged profile stays gone, and a disabled game stays disabled, across a
// close-and-reopen of the same file.
func TestLifecycleStateSurvivesRestart(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// One draft to mark and one game to disable on the same store.
	if err := f.s.SetGameDisabled(ctx, f.game.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, f.profile.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	marked, _, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}

	// A second draft on a second edition, deleted and purged (backdated)
	// before the restart, so the restart also proves a purge stays a purge.
	ed2 := f.editionWithRegion("EU")
	p2, err := f.s.CreateDraftProfile(ctx, ed2.ID)
	if err != nil {
		t.Fatalf("second draft: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, p2.ID); err != nil {
		t.Fatalf("soft delete second: %v", err)
	}
	mustExec(t, f.s.db, "UPDATE profiles SET purge_after = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), p2.ID)
	if _, err := f.s.PurgeProfile(ctx, p2.ID); err != nil {
		t.Fatalf("purge second: %v", err)
	}

	path := f.s.Path()
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openPath(t, path)

	// The mark survived: still hidden from the list, still addressable with
	// its original timestamps (a restart neither un-deletes nor re-dates).
	after, _, err := reopened.GetProfile(ctx, f.profile.ID)
	if err != nil || after.DeletedAt != marked.DeletedAt || after.PurgeAfter != marked.PurgeAfter {
		t.Errorf("marked profile across restart = %+v, %v; want the same mark %+v", after, err, marked)
	}
	list, err := reopened.ListProfiles(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("ListProfiles: %v", err)
	}
	for _, p := range list {
		if p.ID == f.profile.ID {
			t.Errorf("deleted profile is listed again after the restart: %+v", p)
		}
	}
	// The purge survived: the purged row is gone, its edition still stands.
	if _, _, err := reopened.GetProfile(ctx, p2.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("purged profile answers after the restart: %v", err)
	}
	if n := countRows(t, reopened.db, "profiles"); n != 1 {
		t.Errorf("profiles = %d after the restart, want 1 (the marked one; the purged one is gone)", n)
	}
	if g, err := reopened.GetGame(ctx, f.game.ID); err != nil || !g.Disabled {
		t.Errorf("game across restart = %+v, %v; want it still disabled", g, err)
	}
	if err := reopened.IntegrityCheck(ctx); err != nil {
		t.Errorf("IntegrityCheck after the restart: %v", err)
	}
}

func TestCloneProfileCopiesContentAndRecordsProvenance(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(3)
	b.put(f)
	sourceBefore, err := f.s.GetProfileContent(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("source content: %v", err)
	}
	sourceIDs := sourceBefore.allValues()

	res, err := f.s.CloneProfile(ctx, f.profile.ID, "Cloned Tide", "cloned-tide", "curator@local")
	if err != nil {
		t.Fatalf("CloneProfile: %v", err)
	}
	if res.Game.Slug != "cloned-tide" || res.Game.ID == f.game.ID {
		t.Fatalf("clone game = %+v, want a new game with the caller's slug", res.Game)
	}
	if res.Edition.Region != f.edition.Region || res.Edition.Language != f.edition.Language ||
		res.Edition.Revision != f.edition.Revision || res.Edition.GameID != res.Game.ID {
		t.Errorf("clone edition = %+v, want the source edition's binding under the new game", res.Edition)
	}
	if res.Profile.RevisionNum != 1 || res.Profile.Status != games.StatusDraft ||
		res.Profile.ParentRevision != nil || res.Profile.EditionID != res.Edition.ID {
		t.Errorf("clone profile = %+v, want a fresh draft revision 1 with no parent", res.Profile)
	}
	if res.Profile.DeletedAt != "" {
		t.Errorf("clone profile carries a delete mark: %+v", res.Profile)
	}
	// The provenance link ties the two revisions and both games together,
	// attributed to the caller.
	if res.Link.SourceProfileID != f.profile.ID || res.Link.TargetProfileID != res.Profile.ID ||
		res.Link.SourceGameID != f.game.ID || res.Link.TargetGameID != res.Game.ID ||
		res.Link.CreatedBy != "curator@local" {
		t.Errorf("clone link = %+v, want the full provenance between source and clone", res.Link)
	}

	// The snapshot is an independent set of rows: every id of the clone
	// differs from every id of the source, with the content facts intact.
	cloneContent, err := f.s.GetProfileContent(ctx, res.Profile.ID)
	if err != nil {
		t.Fatalf("clone content: %v", err)
	}
	if len(cloneContent.Messages) != len(b.Messages) {
		t.Fatalf("clone has %d messages, want %d", len(cloneContent.Messages), len(b.Messages))
	}
	cloneIDs := cloneContent.allValues()
	for _, id := range cloneIDs {
		if !looksLikeID(id) {
			continue // allValues also collects raw columns (JSON defaults, empty
			// strings): the identifier rule applies to identifiers only
		}
		for _, src := range sourceIDs {
			if id == src {
				t.Fatalf("clone and source share identifier %s: the snapshot is a copy, not a view", id)
			}
		}
	}
	if cloneContent.Messages[0].SpeechText != b.Messages[0].SpeechText {
		t.Errorf("clone message 0 speech = %q, want %q (the content, not just the shape)",
			cloneContent.Messages[0].SpeechText, b.Messages[0].SpeechText)
	}

	// Provenance is readable in both directions (the audit query ADR-0107
	// promises: "where did this game come from" and "what was cloned out of
	// this revision").
	byGame, err := f.s.CloneLinksForGame(ctx, res.Game.ID)
	if err != nil || len(byGame) != 1 || byGame[0].ID != res.Link.ID {
		t.Errorf("CloneLinksForGame(clone) = %v, %v; want the clone's link", byGame, err)
	}
	byProfile, err := f.s.CloneLinksForProfile(ctx, f.profile.ID)
	if err != nil || len(byProfile) != 1 || byProfile[0].TargetProfileID != res.Profile.ID {
		t.Errorf("CloneLinksForProfile(source) = %v, %v; want the outgoing clone", byProfile, err)
	}
	// And the source is untouched: the clone is a copy, the origin keeps its
	// revision number, status and row version.
	after, _, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("source after clone: %v", err)
	}
	if after.DeletedAt != "" || after.Status != sourceBefore.Profile.Status {
		t.Errorf("source profile after the clone = %+v, want it untouched", after)
	}

	// A second clone of the same spelling gets the suffixed slug, never a
	// collision and never a truncation.
	res2, err := f.s.CloneProfile(ctx, f.profile.ID, "", "cloned-tide", "curator@local")
	if err != nil {
		t.Fatalf("second clone: %v", err)
	}
	if res2.Game.Slug != "cloned-tide-2" {
		t.Errorf("second clone slug = %q, want \"cloned-tide-2\" (the collision suffix)", res2.Game.Slug)
	}
	// An empty slug derives <source-slug>-copy.
	res3, err := f.s.CloneProfile(ctx, f.profile.ID, "", "", "curator@local")
	if err != nil {
		t.Fatalf("third clone: %v", err)
	}
	if want := f.game.Slug + "-copy"; res3.Game.Slug != want {
		t.Errorf("derived slug = %q, want %q", res3.Game.Slug, want)
	}
}

// TestCloneProfileOfDeletedSourceIsRefused is the ADR-0107 rule "a hidden
// revision is not a clone source": the refusal is a 404-class sentinel and
// the transaction leaves no orphan game behind.
func TestCloneProfileOfDeletedSourceIsRefused(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)
	if _, err := f.s.SoftDeleteProfile(ctx, f.profile.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	gamesBefore := countRows(t, f.s.db, "games")
	if _, err := f.s.CloneProfile(ctx, f.profile.ID, "", "", "curator@local"); !errors.Is(err, ErrNotFound) {
		t.Errorf("clone of a deleted source = %v, want ErrNotFound", err)
	}
	if n := countRows(t, f.s.db, "games"); n != gamesBefore {
		t.Errorf("games = %d after the refused clone, want %d (the transaction left no half-clone behind)", n, gamesBefore)
	}
}

// TestBackupCarriesLibraryLifecycleState is the AGENTS.md rule 10 round trip
// for G14's schema additions: the backup (SQLite backup API) opens on its own,
// is at this build's schema version, and carries every lifecycle fact — the
// disable flag, the soft-delete mark, the purged revision's absence, the
// clone's provenance row, and the speech-cache provenance column — while the
// live store keeps serving.
func TestBackupCarriesLibraryLifecycleState(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)

	// The full G14 state on the live store: one disabled game, one marked
	// draft, one clone with its provenance, one purged draft, one
	// original_message_id-tagged cache row and one untagged preview row.
	other := f.editionWithRegion("EU")
	markedDraft, err := f.s.CreateDraftProfile(ctx, other.ID)
	if err != nil {
		t.Fatalf("marked draft: %v", err)
	}
	clone, err := f.s.CloneProfile(ctx, f.profile.ID, "Cloned Tide", "cloned-tide", "curator@local")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if err := f.s.SetGameDisabled(ctx, f.game.ID, true); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, markedDraft.ID); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	purgedDraft, err := f.s.CreateDraftProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("purged draft: %v", err)
	}
	if _, err := f.s.SoftDeleteProfile(ctx, purgedDraft.ID); err != nil {
		t.Fatalf("soft delete purged: %v", err)
	}
	mustExec(t, f.s.db, "UPDATE profiles SET purge_after = ? WHERE id = ?",
		time.Now().UTC().Add(-time.Hour).Format(time.RFC3339), purgedDraft.ID)
	if _, err := f.s.PurgeProfile(ctx, purgedDraft.ID); err != nil {
		t.Fatalf("purge: %v", err)
	}
	const (
		provenanceKey = "1111111111111111111111111111111111111111111111111111111111111111"
		previewKey    = "2222222222222222222222222222222222222222222222222222222222222222"
	)
	past := nowRFC3339()
	mustExec(t, f.s.db,
		`INSERT INTO speech_cache (key, sha256, size_bytes, duration_ns, sample_rate, qa_state, key_version, created_at, accessed_at, original_message_id)
		 VALUES (?, ?, 4096, 100000000, 24000, 1, 1, ?, ?, ?)`,
		provenanceKey, shaB, past, past, b.Messages[0].ID)
	mustExec(t, f.s.db,
		`INSERT INTO speech_cache (key, sha256, size_bytes, duration_ns, sample_rate, qa_state, key_version, created_at, accessed_at, original_message_id)
		 VALUES (?, ?, 4096, 100000000, 24000, 0, 1, ?, ?, NULL)`,
		previewKey, shaE, past, past)

	dest := t.TempDir() + "/snapshot.sqlite"
	if err := f.s.Backup(ctx, dest); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	// The live store is still usable after the snapshot.
	if _, err := f.s.GetGame(ctx, f.game.ID); err != nil {
		t.Fatalf("live store after the backup: %v", err)
	}

	backup := openPath(t, dest)
	if err := backup.IntegrityCheck(ctx); err != nil {
		t.Fatalf("IntegrityCheck on the backup: %v", err)
	}
	if v, err := backup.SchemaVersion(ctx); err != nil || v != LatestSchemaVersion() {
		t.Fatalf("backup SchemaVersion = %d, %v; want %d", v, err, LatestSchemaVersion())
	}
	// The disable flag crossed the backup.
	g, err := backup.GetGame(ctx, f.game.ID)
	if err != nil || !g.Disabled {
		t.Errorf("backup game = %+v, %v; want it disabled", g, err)
	}
	// The mark crossed the backup, with its timestamps intact.
	m, _, err := backup.GetProfile(ctx, markedDraft.ID)
	if err != nil || m.DeletedAt == "" || m.PurgeAfter == "" {
		t.Errorf("backup marked draft = %+v, %v; want the soft-delete mark", m, err)
	}
	// The purged revision is absent in the backup too (a restore must not
	// resurrect what the purge removed).
	if _, _, err := backup.GetProfile(ctx, purgedDraft.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("backup serves the purged profile: %v", err)
	}
	// The clone's provenance row crossed the backup, in both directions.
	links, err := backup.CloneLinksForGame(ctx, clone.Game.ID)
	if err != nil || len(links) != 1 || links[0].SourceProfileID != f.profile.ID || links[0].TargetProfileID != clone.Profile.ID {
		t.Errorf("backup clone links = %v, %v; want the clone's provenance", links, err)
	}
	// The speech-cache provenance column crossed the backup: the tagged row
	// still names its message, the preview row still names nothing.
	var tagged, untagged int
	row := backup.db.QueryRow(
		"SELECT count(*) FROM speech_cache WHERE key = ? AND original_message_id = ?",
		provenanceKey, b.Messages[0].ID)
	if err := row.Scan(&tagged); err != nil {
		t.Fatalf("backup provenance row: %v", err)
	}
	if tagged != 1 {
		t.Errorf("backup provenance row = %d, want 1 (original_message_id survived the backup)", tagged)
	}
	row = backup.db.QueryRow(
		"SELECT count(*) FROM speech_cache WHERE key = ? AND original_message_id IS NULL",
		previewKey)
	if err := row.Scan(&untagged); err != nil {
		t.Fatalf("backup preview row: %v", err)
	}
	if untagged != 1 {
		t.Errorf("backup preview row = %d, want 1 (a NULL stays a NULL across the backup)", untagged)
	}
	// And the backup is usable, not just inspectable: a lifecycle write on
	// the restored copy works (re-enable the game there).
	if err := backup.SetGameDisabled(ctx, f.game.ID, false); err != nil {
		t.Errorf("SetGameDisabled on the restored copy: %v", err)
	}
	if g, err := backup.GetGame(ctx, f.game.ID); err != nil || g.Disabled {
		t.Errorf("restored copy after re-enable = %+v, %v; want it enabled", g, err)
	}
}

// TestDuplicateIsNewRevisionUnderTheLibraryName is the ADR-0107 note that
// duplicate needs no store of its own: it is NewRevision, and the store
// proves the facts a duplicate depends on (a fresh draft revision with the
// parent link and the copied content).
func TestDuplicateIsNewRevisionUnderTheLibraryName(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(2)
	b.put(f)

	dup, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision (the duplicate's store): %v", err)
	}
	if dup.EditionID != f.profile.EditionID {
		t.Fatalf("duplicate edition = %s, want the same edition %s (a duplicate stays in its game)", dup.EditionID, f.profile.EditionID)
	}
	if dup.RevisionNum != 2 || dup.Status != games.StatusDraft {
		t.Errorf("duplicate = revision %d %s, want revision 2 draft", dup.RevisionNum, dup.Status)
	}
	if dup.ParentRevision == nil || *dup.ParentRevision != 1 {
		t.Errorf("duplicate parent_revision = %v, want 1 (the revision it duplicated)", dup.ParentRevision)
	}
	content, err := f.s.GetProfileContent(ctx, dup.ID)
	if err != nil {
		t.Fatalf("duplicate content: %v", err)
	}
	if len(content.Messages) != len(b.Messages) {
		t.Errorf("duplicate has %d messages, want %d (the content copy the duplicate is promised)", len(content.Messages), len(b.Messages))
	}
}

// TestDeleteSpeechCacheRowsForMessages is the standalone half of the purge's
// cache addressing: rows addressed by message id, the report naming the
// committed blobs, and the no-op on an empty id set.
func TestDeleteSpeechCacheRowsForMessages(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	b := f.bundle(1)
	b.put(f)
	past := nowRFC3339()
	mustExec(t, f.s.db,
		`INSERT INTO speech_cache (key, sha256, size_bytes, duration_ns, sample_rate, qa_state, key_version, created_at, accessed_at, original_message_id)
		 VALUES (?, ?, 4096, 100000000, 24000, 1, 1, ?, ?, ?)`,
		strings.Repeat("a", 64), shaC, past, past, b.Messages[0].ID)

	n, blobs, err := f.s.DeleteSpeechCacheRowsForMessages(ctx, []string{b.Messages[0].ID})
	if err != nil {
		t.Fatalf("DeleteSpeechCacheRowsForMessages: %v", err)
	}
	if n != 1 || len(blobs) != 1 || blobs[0] != shaC {
		t.Errorf("deletion report = (%d, %v), want (1, [%s])", n, blobs, shaC)
	}
	if n := countRows(t, f.s.db, "speech_cache"); n != 0 {
		t.Errorf("speech_cache = %d rows after the delete, want 0", n)
	}
	// An empty id set addresses nothing and reports nothing.
	if n, blobs, err := f.s.DeleteSpeechCacheRowsForMessages(ctx, nil); err != nil || n != 0 || len(blobs) != 0 {
		t.Errorf("empty deletion = (%d, %v, %v), want a no-op", n, blobs, err)
	}
}

var (
	shaC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	shaD = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
	shaE = "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
)

// crockford is the ULID alphabet: digits and A-Z minus the four Crockford
// exclusions (I, L, O, U), all upper case.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// looksLikeID recognizes the ULID games.NewID issues (26 Crockford-base32
// characters): the copy test compares identifiers against identifiers, not
// against the raw columns allValues carries along with them.
func looksLikeID(v string) bool {
	if len(v) != 26 {
		return false
	}
	for i := 0; i < len(v); i++ {
		if strings.IndexByte(crockford, v[i]) < 0 {
			return false
		}
	}
	return true
}

// nullStrOrNil renders the NULL for the nullable provenance column.
func nullStrOrNil(v string) any {
	if v == "" {
		return nil
	}
	return v
}
