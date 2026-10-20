package store

// ADR-0077: the rights assertion. SetSourceRights is the one operation that
// may change rights_status on a registered source row — the user's assertion
// that lands on the row the content rests on. Everything else on the row
// (provenance: URI, content hash, retrieval time) is untouched.

import (
	"errors"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestSetSourceRightsRecordsTheAssertion(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	src := games.Source{
		ID:            games.NewID(),
		ProfileID:     f.profile.ID,
		SourceType:    games.SourceWebPage,
		URI:           "https://example/dump.txt",
		RetrievedAt:   nowRFC3339(),
		ContentSHA256: "aa" + strings.Repeat("00", 31),
		Rights:        games.RightsUnknown,
		Trust:         0.4,
	}
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{src}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}

	// The assertion lands: rights become the user's position, the note
	// records who and when, and the provenance fields are untouched.
	const note = "rights asserted by curator@local at 2026-10-19T00:00:00Z via the approved web import (ADR-0077)"
	if err := f.s.SetSourceRights(ctx, f.profile.ID, src.ID, games.RightsUserSupplied, note); err != nil {
		t.Fatalf("SetSourceRights: %v", err)
	}
	sources, err := f.s.ListSources(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	got := sources[0]
	if got.Rights != games.RightsUserSupplied {
		t.Fatalf("rights = %s, want user_supplied", got.Rights)
	}
	if !strings.Contains(got.CoverageNotes, note) {
		t.Fatalf("coverage notes %q do not carry the assertion note", got.CoverageNotes)
	}
	// Provenance is the invariant the operation must not touch.
	if got.URI != src.URI || got.ContentSHA256 != src.ContentSHA256 || got.RetrievedAt != src.RetrievedAt || got.Trust != 0.4 {
		t.Fatalf("provenance changed: %+v", got)
	}

	// A second assertion appends, not overwrites (two human acts, two notes).
	if err := f.s.SetSourceRights(ctx, f.profile.ID, src.ID, games.RightsLicensed, "licensed per the site's terms"); err != nil {
		t.Fatalf("second SetSourceRights: %v", err)
	}
	sources, err = f.s.ListSources(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	if sources[0].Rights != games.RightsLicensed {
		t.Fatalf("rights = %s, want licensed after the second assertion", sources[0].Rights)
	}
	if strings.Count(sources[0].CoverageNotes, "via the approved web import") != 1 || !strings.Contains(sources[0].CoverageNotes, "licensed per the site's terms") {
		t.Fatalf("coverage notes %q do not carry both assertions", sources[0].CoverageNotes)
	}
}

func TestSetSourceRightsRefusals(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	src := f.source("https://example/dump.txt", "aa"+strings.Repeat("00", 31), games.RightsUnknown)
	src.SourceType = games.SourceWebPage
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{src}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}

	// `unknown` is the question, not an assertion a user can hold.
	if err := f.s.SetSourceRights(ctx, f.profile.ID, src.ID, games.RightsUnknown, "n/a"); err == nil {
		t.Fatal("unknown was accepted as a user assertion")
	}
	// A source that is not on this revision: not found.
	if err := f.s.SetSourceRights(ctx, f.profile.ID, games.NewID(), games.RightsUserSupplied, "n/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown source = %v, want ErrNotFound", err)
	}
	// A note above the bound is refused.
	if err := f.s.SetSourceRights(ctx, f.profile.ID, src.ID, games.RightsUserSupplied, strings.Repeat("x", 1025)); err == nil {
		t.Fatal("an over-long note was accepted")
	}
}

// The assertion is a content write: it needs a mutable (draft/review)
// revision, bumps the row version, and a published revision refuses it.
func TestSetSourceRightsIsADraftOnlyContentWrite(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx
	src := f.source("https://example/dump.txt", "aa"+strings.Repeat("00", 31), games.RightsUnknown)
	src.SourceType = games.SourceWebPage
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{src}); err != nil {
		t.Fatalf("PutSources: %v", err)
	}
	before := f.version()
	if err := f.s.SetSourceRights(ctx, f.profile.ID, src.ID, games.RightsUserSupplied, "assertion"); err != nil {
		t.Fatalf("SetSourceRights: %v", err)
	}
	if f.version() != before+1 {
		t.Fatalf("row version = %d, want %d (the assertion is a content write)", f.version(), before+1)
	}

	// Publish the revision, then the immutable target refuses the write.
	f.bundle(2).put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := f.s.SetSourceRights(ctx, f.profile.ID, src.ID, games.RightsLicensed, "too late"); !errors.Is(err, ErrImmutable) {
		t.Fatalf("published revision accepted the write: %v", err)
	}
	sources, err := f.s.ListSources(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	for _, s := range sources {
		if s.ID == src.ID && s.Rights != games.RightsUserSupplied {
			t.Fatalf("the published row's rights changed to %s", s.Rights)
		}
	}
}
