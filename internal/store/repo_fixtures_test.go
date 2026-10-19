package store

// Shared fixtures for the repository tests. Everything is synthetic text typed
// into the test — no game data, no copyrighted script lines (AGENTS.md rule 5) —
// and every identifier is a real ULID from games.NewID, because the domain
// types' Validate requires one and pagination cursors are built from ids.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// repoFixture is a game, one edition of it, and a fresh draft revision, with
// helpers to add well-formed content to that revision.
type repoFixture struct {
	t       *testing.T
	s       *Store
	ctx     context.Context
	game    games.Game
	edition games.Edition
	profile games.Profile
}

func newRepoFixture(t *testing.T) *repoFixture {
	t.Helper()
	return fixtureOn(t, migratedStore(t))
}

// fixtureOn builds the game/edition/profile trio on a caller-supplied store.
// newRepoFixture is the convenience wrapper that opens its own store; this
// one is for tests that must share a pool — for example the stale-FTS-view
// regression test, which needs the fixture and its poison-connections on the
// very same *sql.DB.
func fixtureOn(t *testing.T, s *Store) *repoFixture {
	t.Helper()
	f := &repoFixture{t: t, s: s, ctx: testCtx(t)}
	f.game = games.Game{
		ID:        games.NewID(),
		Slug:      "synthetic-tide",
		Title:     "Synthetic Tide",
		Platform:  "gamecube",
		UserTags:  []byte(`["synthetic"]`),
		CreatedAt: nowRFC3339(),
	}
	if err := s.CreateGame(f.ctx, f.game); err != nil {
		t.Fatalf("CreateGame: %v", err)
	}
	f.edition = games.Edition{
		ID:       games.NewID(),
		GameID:   f.game.ID,
		Region:   "US",
		Language: "en",
		Revision: "rev0",
		Locale:   "en_US",
	}
	if err := s.CreateEdition(f.ctx, f.edition); err != nil {
		t.Fatalf("CreateEdition: %v", err)
	}
	p, err := s.CreateDraftProfile(f.ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile: %v", err)
	}
	f.profile = p
	return f
}

// edition adds a second edition of the same game (region is part of the
// uniqueness key, so a new region is how two editions coexist).
func (f *repoFixture) editionWithRegion(region string) games.Edition {
	f.t.Helper()
	e := games.Edition{
		ID:       games.NewID(),
		GameID:   f.game.ID,
		Region:   region,
		Language: "en",
		Revision: "rev0",
	}
	if err := f.s.CreateEdition(f.ctx, e); err != nil {
		f.t.Fatalf("CreateEdition(%s): %v", region, err)
	}
	return e
}

func (f *repoFixture) version() int64 {
	f.t.Helper()
	_, rev, err := f.s.GetProfile(f.ctx, f.profile.ID)
	if err != nil {
		f.t.Fatalf("GetProfile: %v", err)
	}
	return rev
}

func (f *repoFixture) source(uri, sha string, rights games.RightsStatus) games.Source {
	f.t.Helper()
	return games.Source{
		ID:            games.NewID(),
		ProfileID:     f.profile.ID,
		SourceType:    games.SourceUserFile,
		URI:           uri,
		RetrievedAt:   nowRFC3339(),
		ContentSHA256: sha,
		MimeType:      "text/csv",
		Rights:        rights,
		Trust:         1,
		Parser:        "generic-csv",
	}
}

func (f *repoFixture) character(name string) games.Character {
	f.t.Helper()
	return games.Character{
		ID:        games.NewID(),
		ProfileID: f.profile.ID,
		Name:      name,
		Status:    games.CharacterConfirmed,
		CreatedAt: nowRFC3339(),
	}
}

func (f *repoFixture) scene(name string) games.Scene {
	f.t.Helper()
	return games.Scene{
		ID:        games.NewID(),
		ProfileID: f.profile.ID,
		Name:      name,
		CreatedAt: nowRFC3339(),
	}
}

func (f *repoFixture) region(sceneID string) games.Region {
	f.t.Helper()
	return games.Region{
		ID:           games.NewID(),
		ProfileID:    f.profile.ID,
		SceneID:      sceneID,
		Label:        "textbox",
		RegionType:   games.RegionTextbox,
		DetectMethod: games.DetectManual,
		RectX:        0.1,
		RectY:        0.8,
		RectW:        0.8,
		RectH:        0.15,
		Revision:     1,
		UpdatedAt:    nowRFC3339(),
	}
}

// message builds a well-formed dialogue line of the fixture's revision.
func (f *repoFixture) message(original string) games.Message {
	f.t.Helper()
	return games.Message{
		ID:                games.NewID(),
		ProfileID:         f.profile.ID,
		OriginalText:      original,
		NormalizedText:    strings.ToLower(strings.TrimRight(original, "!?.")),
		SpeechText:        original,
		Language:          "en",
		TextKind:          games.TextDialogue,
		AttributionStatus: games.AttributionUnknown,
		Revision:          1,
		CreatedAt:         nowRFC3339(),
		UpdatedAt:         nowRFC3339(),
	}
}

func (f *repoFixture) page(messageID, text string, ordinal int) games.MessagePage {
	f.t.Helper()
	return games.MessagePage{
		MessageID: messageID,
		Ordinal:   ordinal,
		PageText:  text,
	}
}

func (f *repoFixture) edge(from, to string) games.MessageEdge {
	f.t.Helper()
	return games.MessageEdge{
		FromID:     from,
		ToID:       to,
		EdgeType:   games.EdgeNext,
		Confidence: 1,
		CreatedAt:  nowRFC3339(),
	}
}

func (f *repoFixture) assertion(messageID, characterID string) games.SpeakerAssertion {
	f.t.Helper()
	return games.SpeakerAssertion{
		ID:          games.NewID(),
		ProfileID:   f.profile.ID,
		MessageID:   messageID,
		CharacterID: characterID,
		Method:      games.AssertionScriptOrder,
		AssertedAt:  nowRFC3339(),
		Posterior:   0.8,
	}
}

// content is the "a revision that can actually be published" bundle: one source
// with publishable rights, one character, one scene, one line with two pages and
// an assertion. Tests that only need a graph to copy or paginate extend it.
type contentBundle struct {
	Source     games.Source
	Character  games.Character
	Scene      games.Scene
	Region     games.Region
	Messages   []games.Message
	Pages      []games.MessagePage
	Edges      []games.MessageEdge
	Assertions []games.SpeakerAssertion
}

func (f *repoFixture) bundle(n int) *contentBundle {
	f.t.Helper()
	b := &contentBundle{
		Source:    f.source("file:///curated/synthetic.csv", shaA, games.RightsUserSupplied),
		Character: f.character("Sailor"),
		Scene:     f.scene("Open Water"),
	}
	b.Region = f.region(b.Scene.ID)
	for i := 0; i < n; i++ {
		m := f.message(fmt.Sprintf("Synthetic line number %d about the current", i))
		m.SourceID = b.Source.ID
		m.SourceMessageID = fmt.Sprintf("row-%06d", i)
		m.SceneID = b.Scene.ID
		m.SpeakerID = b.Character.ID
		m.AttributionStatus = games.AttributionConfirmed
		m.AttributionConfidence = 0.9
		b.Messages = append(b.Messages, m)
		b.Pages = append(b.Pages, f.page(m.ID, m.OriginalText, 1))
		b.Assertions = append(b.Assertions, f.assertion(m.ID, b.Character.ID))
	}
	for i := 1; i < len(b.Messages); i++ {
		b.Edges = append(b.Edges, f.edge(b.Messages[i-1].ID, b.Messages[i].ID))
	}
	return b
}

// put writes the bundle into the fixture's revision.
func (b *contentBundle) put(f *repoFixture) {
	f.t.Helper()
	ctx := f.ctx
	if err := f.s.PutSources(ctx, f.profile.ID, []games.Source{b.Source}); err != nil {
		f.t.Fatalf("PutSources: %v", err)
	}
	if err := f.s.PutCharacters(ctx, f.profile.ID, []games.Character{b.Character}); err != nil {
		f.t.Fatalf("PutCharacters: %v", err)
	}
	if err := f.s.PutScenes(ctx, f.profile.ID, []games.Scene{b.Scene}); err != nil {
		f.t.Fatalf("PutScenes: %v", err)
	}
	if err := f.s.PutRegions(ctx, f.profile.ID, []games.Region{b.Region}); err != nil {
		f.t.Fatalf("PutRegions: %v", err)
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, b.Messages, b.Pages); err != nil {
		f.t.Fatalf("PutMessages: %v", err)
	}
	if err := f.s.PutEdges(ctx, f.profile.ID, b.Edges); err != nil {
		f.t.Fatalf("PutEdges: %v", err)
	}
	if err := f.s.PutSpeakerAssertions(ctx, f.profile.ID, b.Assertions); err != nil {
		f.t.Fatalf("PutSpeakerAssertions: %v", err)
	}
}

// idsOf collects the ids of a bundle's rows, for the "no row of the new revision
// points at a row of the old one" assertions.
func (b *contentBundle) ids() []string {
	out := []string{b.Source.ID, b.Character.ID, b.Scene.ID, b.Region.ID}
	for _, m := range b.Messages {
		out = append(out, m.ID)
	}
	for _, a := range b.Assertions {
		out = append(out, a.ID)
	}
	return out
}

// allIDsOf collects every identifier and reference in a loaded revision, so a
// test can assert the *whole* snapshot against the parent's ids instead of the
// handful of columns a reviewer would remember to name.
func (c ProfileContent) allValues() []string {
	out := []string{c.Profile.ID, c.Profile.EditionID}
	for _, r := range c.Sources {
		out = append(out, r.ID, r.ProfileID)
	}
	for _, r := range c.Characters {
		out = append(out, r.ID, r.ProfileID, string(r.VerifiedSourceRefs), string(r.Aliases))
	}
	for _, r := range c.Scenes {
		out = append(out, r.ID, r.ProfileID, string(r.VisualAnchors))
	}
	for _, r := range c.Regions {
		out = append(out, r.ID, r.ProfileID, r.SceneID)
	}
	for _, r := range c.Messages {
		out = append(out, r.ID, r.ProfileID, r.SourceID, r.SceneID, r.SpeakerID, string(r.AttributionEvidence), string(r.Flags))
	}
	for _, r := range c.Pages {
		out = append(out, r.MessageID)
	}
	for _, r := range c.Edges {
		out = append(out, r.FromID, r.ToID, r.SourceID)
	}
	for _, r := range c.Assertions {
		out = append(out, r.ID, r.ProfileID, r.MessageID, r.CharacterID)
	}
	return out
}
