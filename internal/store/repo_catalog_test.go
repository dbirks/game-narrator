package store

// Catalog round-trip tests for games and editions (docs/spec/02 "Entity catalog").
// The point of these is fidelity: what the domain type carried in is what it reads
// back, including the columns a UI would otherwise silently lose — a null source
// identity, a null aspect ratio, an empty tag list.

import (
	"errors"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestCatalogGamesRoundTrip(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	got, err := f.s.GetGame(ctx, f.game.ID)
	if err != nil {
		t.Fatalf("GetGame: %v", err)
	}
	if got.ID != f.game.ID || got.Slug != f.game.Slug || got.Title != f.game.Title ||
		got.Description != f.game.Description || got.Platform != f.game.Platform ||
		got.CreatedAt != f.game.CreatedAt || string(got.UserTags) != `["synthetic"]` {
		t.Errorf("GetGame = %+v tags %s, want %+v", got, got.UserTags, f.game)
	}
	bySlug, err := f.s.GetGameBySlug(ctx, f.game.Slug)
	if err != nil || bySlug.ID != got.ID {
		t.Errorf("GetGameBySlug = %+v, %v; want the same row as GetGame", bySlug, err)
	}

	// Update replaces the metadata in place — games carry no revision token
	// (docs/spec/08 puts ETag/If-Match on profile revisions, not on library rows).
	edited := got
	edited.Title = "Synthetic Tide (Director's Cut)"
	edited.Description = "Rewritten after the region fix."
	edited.Platform = "dolphin"
	edited.UserTags = []byte(`["synthetic","region-pal"]`)
	if err := f.s.UpdateGame(ctx, edited); err != nil {
		t.Fatalf("UpdateGame: %v", err)
	}
	after, err := f.s.GetGame(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != edited.Title || after.Description != edited.Description ||
		after.Platform != edited.Platform || string(after.UserTags) != string(edited.UserTags) {
		t.Errorf("UpdateGame lost fields: %+v", after)
	}
	// The timestamp belongs to creation and an update does not mint a new one.
	if after.CreatedAt != got.CreatedAt {
		t.Errorf("created_at moved from %q to %q", got.CreatedAt, after.CreatedAt)
	}

	// The old slug is gone and the new one resolves: a slug is an identity, not a
	// label.
	if _, err := f.s.GetGameBySlug(ctx, "synthetic-tide"); err != nil {
		t.Errorf("GetGameBySlug after the rename: %v", err)
	}

	// A duplicate slug is a conflict naming the collision, not a silent overwrite.
	dupe := games.Game{ID: games.NewID(), Slug: edited.Slug, Title: "Impostor", CreatedAt: nowRFC3339()}
	if err := f.s.CreateGame(ctx, dupe); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate slug = %v, want ErrConflict", err)
	}
}

func TestCatalogEditionsRoundTrip(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// An edition with both optional fields set, and one with neither: the nullable
	// columns must read back as absent rather than as the zero values a UI would
	// render as "0.00 aspect" or an empty hash.
	au := games.Edition{
		ID:             games.NewID(),
		GameID:         f.game.ID,
		Region:         "AU",
		Language:       "en",
		Revision:       "rev1",
		SourceIdentity: strings.Repeat("c", games.MaxSHA256Hex),
		Locale:         "en_AU",
	}
	aspect := 1.7778
	au.DisplayAspect = &aspect
	if err := f.s.CreateEdition(ctx, au); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	jp := games.Edition{
		ID:       games.NewID(),
		GameID:   f.game.ID,
		Region:   "JP",
		Language: "ja",
		Revision: "rev0",
	}
	if err := f.s.CreateEdition(ctx, jp); err != nil {
		t.Fatalf("CreateEdition(JP): %v", err)
	}

	loaded, err := f.s.GetEdition(ctx, au.ID)
	if err != nil {
		t.Fatalf("GetEdition: %v", err)
	}
	if loaded.DisplayAspect == nil || *loaded.DisplayAspect != aspect {
		t.Errorf("display_aspect = %v, want %v", loaded.DisplayAspect, aspect)
	}
	if loaded.SourceIdentity != au.SourceIdentity {
		t.Errorf("source_identity = %q", loaded.SourceIdentity)
	}
	plain, err := f.s.GetEdition(ctx, jp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if plain.DisplayAspect != nil || plain.SourceIdentity != "" {
		t.Errorf("an edition with no capture facts reads back as %+v", plain)
	}

	editions, err := f.s.ListEditions(ctx, f.game.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(editions) != 3 { // the fixture's US edition plus the two above
		t.Fatalf("ListEditions = %+v, want three", editions)
	}
	seen := map[string]bool{}
	for _, e := range editions {
		seen[e.ID] = true
	}
	if !seen[au.ID] || !seen[jp.ID] || !seen[f.edition.ID] {
		t.Errorf("ListEditions missed an edition: %v", seen)
	}
	// Another game's editions stay out of the list.
	other := games.Game{ID: games.NewID(), Slug: "another-title", Title: "Another Title", CreatedAt: nowRFC3339()}
	if err := f.s.CreateGame(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := f.s.CreateEdition(ctx, games.Edition{ID: games.NewID(), GameID: other.ID, Region: "US"}); err != nil {
		t.Fatal(err)
	}
	if again, err := f.s.ListEditions(ctx, f.game.ID); err != nil || len(again) != 3 {
		t.Errorf("ListEditions after another game's edition = %d, %v; want 3", len(again), err)
	}

	// The same region/language/revision twice is the re-import case
	// editions_identity_idx exists for: a conflict to reconcile, not a duplicate.
	if err := f.s.CreateEdition(ctx, au); !errors.Is(err, ErrConflict) {
		t.Errorf("duplicate edition identity = %v, want ErrConflict", err)
	}
}
