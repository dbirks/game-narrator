package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestProfileLifecycleDraftReviewPublished walks the workflow docs/spec/02
// "Transaction workflows" prescribes — edit the draft, put it up for review,
// publish atomically — and asserts the storage facts each stage is supposed to
// leave behind.
func TestProfileLifecycleDraftReviewPublished(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	if f.profile.RevisionNum != 1 || f.profile.Status != games.StatusDraft {
		t.Fatalf("new draft = %+v, want revision 1 in status draft", f.profile)
	}
	if v := f.version(); v != freshRowVersion {
		t.Fatalf("initial row version = %d, want %d", v, freshRowVersion)
	}

	b := f.bundle(3)
	b.put(f)
	if v := f.version(); v != freshRowVersion+7 {
		t.Fatalf("row version after 7 content writes = %d, want %d (every write counts)", v, freshRowVersion+7)
	}

	// review, with the current token.
	rev := f.version()
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, rev); err != nil {
		t.Fatalf("draft to review: %v", err)
	}
	if v := f.version(); v != rev+1 {
		t.Fatalf("row version after status change = %d, want %d", v, rev+1)
	}

	published, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if published.Status != games.StatusPublished {
		t.Errorf("published status = %q", published.Status)
	}
	if published.PublishedAt == "" || published.PublicationAuthor != "curator@local" {
		t.Errorf("published row is missing its timestamp or author: %+v", published)
	}
	if len(published.ManifestDigest) != games.MaxSHA256Hex {
		t.Errorf("manifest digest = %q, want a SHA-256 hex digest", published.ManifestDigest)
	}
	if err := published.Validate(); err != nil {
		t.Errorf("the published row does not satisfy its own type: %v", err)
	}

	// The edition now points at it, which is the one value a session start reads
	// (docs/spec/01: only published revisions are used at runtime).
	current, _, err := f.s.PublishedProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("PublishedProfile: %v", err)
	}
	if current.ID != f.profile.ID || current.ManifestDigest != published.ManifestDigest {
		t.Errorf("edition pointer = %+v, want the revision just published", current)
	}

	// An edition with nothing published is not an error waiting to happen: it
	// reports a miss rather than serving an unreviewed revision.
	other := f.editionWithRegion("EU")
	if _, _, err := f.s.PublishedProfile(ctx, other.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("PublishedProfile of an unpublished edition = %v, want ErrNotFound", err)
	}

	// And the frozen row stays frozen: the point of publication.
	for name, err := range map[string]error{
		"PutMessages":   f.s.PutMessages(ctx, f.profile.ID, []games.Message{f.message("after publishing")}, nil),
		"PutCharacters": f.s.PutCharacters(ctx, f.profile.ID, []games.Character{f.character("Ghost")}),
		"PutScenes":     f.s.PutScenes(ctx, f.profile.ID, []games.Scene{f.scene("Frozen")}),
		"PutRegions":    f.s.PutRegions(ctx, f.profile.ID, []games.Region{f.region("")}),
		"PutSources":    f.s.PutSources(ctx, f.profile.ID, []games.Source{f.source("file:///x.csv", shaB, games.RightsUserSupplied)}),
		"PutEdges":      f.s.PutEdges(ctx, f.profile.ID, []games.MessageEdge{f.edge(b.Messages[0].ID, b.Messages[1].ID)}),
		"PutAssertions": f.s.PutSpeakerAssertions(ctx, f.profile.ID, []games.SpeakerAssertion{f.assertion(b.Messages[0].ID, b.Character.ID)}),
		"SpeechText": func() error {
			_, err := f.s.UpdateMessageSpeechText(ctx, f.profile.ID, b.Messages[0].ID, "nope", f.version())
			return err
		}(),
		"SetStatus": f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusDraft, f.version()),
	} {
		if !errors.Is(err, ErrImmutable) {
			t.Errorf("%s on a published revision = %v, want ErrImmutable", name, err)
		}
	}
}

// TestSetProfileStatusHonoursTheRules covers docs/spec/08's PROFILE_REVISION_CONFLICT
// and the deliberately narrow transition graph in games.ProfileStatus.
func TestSetProfileStatusHonoursTheRules(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// A stale token loses, whatever the transition would otherwise have been.
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()+5); !errors.Is(err, ErrRevisionConflict) {
		t.Errorf("stale token = %v, want ErrRevisionConflict", err)
	}
	// Skipping review is not a thing: publication is its own operation.
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusPublished, f.version()); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("draft to published = %v, want ErrInvalidTransition", err)
	}
	// A draft cannot be archived directly either — it was never shipped.
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusArchived, f.version()); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("draft to archived = %v, want ErrInvalidTransition", err)
	}
	if f.version() != 1 {
		t.Fatalf("a rejected transition moved the row version to %d, want 1", f.version())
	}

	// review, then back to draft (a curator or the validator sent it away), then up
	// again: the only round trip the graph allows before publication.
	for _, step := range []games.ProfileStatus{games.StatusReview, games.StatusDraft, games.StatusReview} {
		if err := f.s.SetProfileStatus(ctx, f.profile.ID, step, f.version()); err != nil {
			t.Fatalf("transition to %s: %v", step, err)
		}
	}
	if p, _, err := f.s.GetProfile(ctx, f.profile.ID); err != nil || p.Status != games.StatusReview {
		t.Fatalf("profile = %+v, %v; want review", p, err)
	}

	// The gate needs something to narrate before this revision can ship (the empty
	// case is TestPublishNeedsReviewAndAnAuthor's); this test is about the state machine.
	f.bundle(1).put(f)

	// Publish, then archive, then nothing: archived is terminal, and re-selecting
	// an old revision is done by deriving from it, not resurrecting it.
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusDraft, f.version()); !errors.Is(err, ErrImmutable) {
		t.Errorf("published to draft = %v, want ErrImmutable", err)
	}
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusArchived, f.version()); err != nil {
		t.Fatalf("published to archived: %v", err)
	}
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusDraft, f.version()); !errors.Is(err, ErrImmutable) {
		t.Errorf("archived to draft = %v, want ErrImmutable", err)
	}

	// A revision that does not exist is a miss, not a transition failure.
	if err := f.s.SetProfileStatus(ctx, games.NewID(), games.StatusReview, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetProfileStatus of a missing profile = %v, want ErrNotFound", err)
	}
}

// TestNewRevisionIsAnIndependentSnapshot is docs/spec/02's "new revision snapshot"
// step: every content row copied with new identifiers, every internal reference
// rewritten, nothing left pointing at the revision it came from.
func TestNewRevisionIsAnIndependentSnapshot(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	b := f.bundle(12)
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatalf("draft to review: %v", err)
	}
	// A correction on the parent, which must NOT be copied: it is an overlay that
	// outlives rows (0002_content.sql), not part of a revision's content.
	mustExec(t, f.s.db,
		`INSERT INTO corrections (id, profile_id, kind, target_type, target_id, message_id, actor, before_value, after_value, reason, created_at)
		 VALUES (?, ?, 'speaker', 'message', ?, ?, 'tester', 'Sailor', 'Pilot', 'misread label', ?)`,
		games.NewID(), f.profile.ID, b.Messages[0].ID, b.Messages[0].ID, nowRFC3339())

	// Publish the parent first: deriving from a published revision is the ordinary
	// case, and it proves the copy reads an immutable source without touching it.
	if _, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local"); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	parentIDs := b.ids()
	parentVersion := f.version()

	child, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if child.ID == f.profile.ID {
		t.Fatal("the snapshot reused the parent's id")
	}
	if child.RevisionNum != 2 || child.ParentRevision == nil || *child.ParentRevision != 1 {
		t.Errorf("snapshot = %+v, want revision 2 recording parent revision 1", child)
	}
	if child.Status != games.StatusDraft {
		t.Errorf("snapshot status = %q, want draft", child.Status)
	}
	if child.PublishedAt != "" || child.ManifestDigest != "" || child.PublicationAuthor != "" {
		t.Errorf("a fresh snapshot must not inherit publication facts: %+v", child)
	}

	src, err := f.s.GetProfileContent(ctx, f.profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	dst, err := f.s.GetProfileContent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Every row copied, one for one.
	cases := []struct {
		name          string
		parent, child int
	}{
		{"sources", len(src.Sources), len(dst.Sources)},
		{"characters", len(src.Characters), len(dst.Characters)},
		{"scenes", len(src.Scenes), len(dst.Scenes)},
		{"regions", len(src.Regions), len(dst.Regions)},
		{"messages", len(src.Messages), len(dst.Messages)},
		{"pages", len(src.Pages), len(dst.Pages)},
		{"edges", len(src.Edges), len(dst.Edges)},
		{"assertions", len(src.Assertions), len(dst.Assertions)},
	}
	for _, c := range cases {
		if c.parent != c.child {
			t.Errorf("%s copied %d of %d rows", c.name, c.child, c.parent)
		}
	}

	// Not one identifier of the parent survives anywhere in the child — including
	// inside the JSON reference columns, where a column-by-column check would miss
	// it. The child's own profile id is checked too: it must be the child's.
	parentSet := make(map[string]bool, len(parentIDs)+1)
	for _, id := range parentIDs {
		parentSet[id] = true
	}
	parentSet[f.profile.ID] = true
	for _, v := range dst.allValues() {
		if parentSet[v] {
			t.Errorf("snapshot row field %q still names a row of the parent revision", v)
		}
		// A JSON column holding a list of ids would show the leak as a substring.
		for id := range parentSet {
			if strings.Contains(v, id) {
				t.Errorf("snapshot field %q references parent row %s", truncate(v), id)
			}
		}
	}

	// The content came across unchanged. Rows are matched by their source-side
	// identity rather than by position: both revisions are ordered by id, and the
	// snapshot's ids are new, so position means nothing across the two (which is
	// itself evidence the copy is independent).
	bySourceID := func(ms []games.Message) map[string]games.Message {
		out := make(map[string]games.Message, len(ms))
		for _, m := range ms {
			out[m.SourceMessageID] = m
		}
		return out
	}
	srcByID, dstByID := bySourceID(src.Messages), bySourceID(dst.Messages)
	if len(srcByID) != len(src.Messages) || len(dstByID) != len(dst.Messages) {
		t.Fatalf("source message ids are not unique in one revision (%d/%d, %d/%d)",
			len(srcByID), len(src.Messages), len(dstByID), len(dst.Messages))
	}
	for key, want := range srcByID {
		got, ok := dstByID[key]
		if !ok {
			t.Errorf("message with source id %q was not copied", key)
			continue
		}
		if got.OriginalText != want.OriginalText || got.SpeechText != want.SpeechText ||
			got.NormalizedText != want.NormalizedText || got.TextKind != want.TextKind ||
			got.Language != want.Language {
			t.Errorf("message %q changed on the way into the snapshot: %+v vs %+v", key, want, got)
		}
		if got.Revision != 1 {
			t.Errorf("snapshot message %q carries revision %d, want 1 (a new row has not been edited)", key, got.Revision)
		}
	}
	if len(src.Pages) != len(dst.Pages) || len(src.Edges) != len(dst.Edges) {
		t.Fatalf("pages/edges copied %d/%d, want %d/%d",
			len(dst.Pages), len(dst.Edges), len(src.Pages), len(src.Edges))
	}
	// Character and scene names are the human-visible part of a snapshot: the copy
	// must not silently rename anybody.
	for i := range src.Characters {
		if src.Characters[i].Name != dst.Characters[i].Name || src.Scenes[i].Name != dst.Scenes[i].Name {
			t.Errorf("snapshot renamed %q/%q to %q/%q", src.Characters[i].Name, src.Scenes[i].Name,
				dst.Characters[i].Name, dst.Scenes[i].Name)
		}
	}

	// The graph still resolves inside the snapshot: references are rewritten, not
	// dropped, so a message still names a scene and speaker that exist *in this
	// revision*. This is the property publish also gates on, asserted here so a
	// remap bug is caught where it is introduced.
	inChild := map[string]bool{}
	for _, m := range dst.Messages {
		if m.SceneID == "" || !contains(dst.Scenes, m.SceneID) {
			t.Errorf("snapshot message %s lost its scene: %q not in the snapshot's scenes", m.ID, m.SceneID)
		}
		if m.SpeakerID == "" || !containsChar(dst.Characters, m.SpeakerID) {
			t.Errorf("snapshot message %s lost its speaker: %q", m.ID, m.SpeakerID)
		}
		if m.SourceID == "" || !containsSrc(dst.Sources, m.SourceID) {
			t.Errorf("snapshot message %s lost its source: %q", m.ID, m.SourceID)
		}
		inChild[m.ID] = true
	}
	for _, e := range dst.Edges {
		if !inChild[e.FromID] || !inChild[e.ToID] {
			t.Errorf("snapshot edge %+v leaves the snapshot's message set", e)
		}
	}
	for _, p := range dst.Pages {
		if !inChild[p.MessageID] {
			t.Errorf("snapshot page for %s is not for a snapshot message", p.MessageID)
		}
	}
	for _, a := range dst.Assertions {
		if !inChild[a.MessageID] || !containsChar(dst.Characters, a.CharacterID) {
			t.Errorf("snapshot assertion %+v names rows outside the snapshot", a)
		}
	}
	for _, r := range dst.Regions {
		if !contains(dst.Scenes, r.SceneID) {
			t.Errorf("snapshot region %s lost its scene %q", r.ID, r.SceneID)
		}
	}

	// Corrections were not copied, and the parent's is still there.
	var n int
	if err := f.s.db.QueryRow(`SELECT count(*) FROM corrections WHERE profile_id = ?`, child.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("snapshot copied %d corrections, which are overlays rather than content", n)
	}
	if err := f.s.db.QueryRow(`SELECT count(*) FROM corrections WHERE profile_id = ?`, f.profile.ID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("parent corrections = %d, want the one written", n)
	}

	// The immutable parent is untouched by having been copied — its row version
	// did not move, and it is still the published revision of its edition.
	if p, v, err := f.s.GetProfile(ctx, f.profile.ID); err != nil {
		t.Fatal(err)
	} else {
		if v != parentVersion {
			t.Errorf("deriving from a published revision moved its row version to %d, want %d", v, parentVersion)
		}
		if p.Status != games.StatusPublished {
			t.Errorf("parent status after NewRevision = %q, want published", p.Status)
		}
	}
	// The snapshot is editable, which is the whole reason it exists. Note the
	// explicit ProfileID: a row that names the parent revision is a caller bug and
	// must be refused rather than quietly reparented.
	fresh := f.character("New in the draft")
	fresh.ProfileID = child.ID
	if err := f.s.PutCharacters(ctx, child.ID, []games.Character{fresh}); err != nil {
		t.Errorf("the snapshot is not editable: %v", err)
	}
	if _, err := f.s.GetProfileContent(ctx, f.profile.ID); err != nil {
		t.Fatal(err)
	}
}

// TestNewRevisionCarriesJSONReferences covers the remap that a column-by-column
// review misses: a scene's visual anchors name regions, and a character names the
// sources that establish it.
func TestNewRevisionCarriesJSONReferences(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	b := f.bundle(1)
	b.put(f)
	charWithSource := b.Character
	charWithSource.VerifiedSourceRefs = jsonRaw(`["` + b.Source.ID + `"]`)
	anchoredScene := b.Scene
	anchoredScene.VisualAnchors = jsonRaw(`[{"region_id":"` + b.Region.ID + `","expected_text":"DECK"}]`)
	if err := f.s.PutCharacters(ctx, f.profile.ID, []games.Character{charWithSource}); err != nil {
		t.Fatal(err)
	}
	if err := f.s.PutScenes(ctx, f.profile.ID, []games.Scene{anchoredScene}); err != nil {
		t.Fatal(err)
	}

	child, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	dst, err := f.s.GetProfileContent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}

	newRegionID := dst.Regions[0].ID
	if got := string(dst.Characters[0].VerifiedSourceRefs); !strings.Contains(got, dst.Sources[0].ID) || strings.Contains(got, b.Source.ID) {
		t.Errorf("verified_source_refs after the copy = %s, want the snapshot's source id", got)
	}
	if got := string(dst.Scenes[0].VisualAnchors); !strings.Contains(got, newRegionID) || strings.Contains(got, b.Region.ID) {
		t.Errorf("visual_anchors after the copy = %s, want the snapshot's region id", got)
	}
	// The anchor's data member survived the rewrite untouched — the remap rewrites
	// identifiers, it does not re-render a document.
	if got := string(dst.Scenes[0].VisualAnchors); !strings.Contains(got, `"expected_text":"DECK"`) {
		t.Errorf("visual_anchors lost its payload: %s", got)
	}
}

// TestRevisionNumbersAreDensePerEdition covers the UNIQUE index that makes
// "revision 3 of edition X" a nameable thing (docs/spec/01: a session pins a
// revision).
func TestRevisionNumbersAreDensePerEdition(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	eu := f.editionWithRegion("EU")
	euDraft, err := f.s.CreateDraftProfile(ctx, eu.ID)
	if err != nil {
		t.Fatalf("CreateDraftProfile(other edition): %v", err)
	}
	if euDraft.RevisionNum != 1 {
		t.Errorf("first revision of another edition = %d, want 1 (numbers are per edition)", euDraft.RevisionNum)
	}

	// A second independent draft of the same edition takes the next number rather
	// than colliding with profiles_edition_revision_idx.
	second, err := f.s.CreateDraftProfile(ctx, f.edition.ID)
	if err != nil {
		t.Fatalf("second draft of the same edition: %v", err)
	}
	if second.RevisionNum != 2 {
		t.Errorf("second draft revision = %d, want 2", second.RevisionNum)
	}

	all, err := f.s.ListProfiles(ctx, f.edition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("ListProfiles = %+v, want the two revisions", all)
	}

	// Deriving from a draft also takes the next free number.
	third, err := f.s.NewRevision(ctx, second.ID)
	if err != nil {
		t.Fatalf("NewRevision from a draft: %v", err)
	}
	if third.RevisionNum != 3 {
		t.Errorf("derived revision = %d, want 3", third.RevisionNum)
	}
	if third.ParentRevision == nil || *third.ParentRevision != 2 {
		t.Errorf("derived revision's parent = %v, want 2", third.ParentRevision)
	}

	// Deriving from a revision that does not exist is a miss.
	if _, err := f.s.NewRevision(ctx, games.NewID()); !errors.Is(err, ErrNotFound) {
		t.Errorf("NewRevision of a missing profile = %v, want ErrNotFound", err)
	}
}

// TestProfileStateSurvivesRestart is the docs/spec/16 M1 gate item "verify
// migration/restart": closing the database and opening the same file must not
// change any answer the repositories give.
func TestProfileStateSurvivesRestart(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	b := f.bundle(4)
	b.put(f)
	if err := f.s.SetProfileStatus(ctx, f.profile.ID, games.StatusReview, f.version()); err != nil {
		t.Fatal(err)
	}
	published, err := f.s.Publish(ctx, f.profile.ID, f.version(), "curator@local")
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	child, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	// The snapshot's messages have new ids, so the line to edit is found by index
	// in the snapshot rather than by the parent's identifier.
	dstContent, err := f.s.GetProfileContent(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetProfileContent(snapshot): %v", err)
	}
	if len(dstContent.Messages) != len(b.Messages) {
		t.Fatalf("snapshot has %d messages, want %d", len(dstContent.Messages), len(b.Messages))
	}
	childMessage := dstContent.Messages[0].ID
	childVersion := func() int64 {
		_, v, err := f.s.GetProfile(ctx, child.ID)
		if err != nil {
			t.Fatalf("GetProfile(snapshot): %v", err)
		}
		return v
	}()
	if _, err := f.s.UpdateMessageSpeechText(ctx, child.ID, childMessage, "Revised wording", childVersion); err != nil {
		t.Fatalf("UpdateMessageSpeechText: %v", err)
	}
	// Read the expected state after the edit, so the comparison across the restart
	// includes the bumped row version and the new speech text rather than the
	// values from before it.
	before, err := f.s.GetProfileContent(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Messages[0].SpeechText != "Revised wording" {
		t.Fatalf("the edit did not land before the restart: %+v", before.Messages[0])
	}
	beforeActions, err := f.s.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}

	// Close and reopen the same file, the way a service restart does.
	path := f.s.Path()
	if err := f.s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	reopened := openPath(t, path)

	after, err := reopened.GetProfileContent(ctx, child.ID)
	if err != nil {
		t.Fatalf("GetProfileContent after restart: %v", err)
	}
	if after.Profile.Status != before.Profile.Status || after.RowVersion != before.RowVersion {
		t.Errorf("profile across restart = %+v version %d, want %+v version %d",
			after.Profile, after.RowVersion, before.Profile, before.RowVersion)
	}
	if len(after.Messages) != len(before.Messages) || after.Messages[0].SpeechText != "Revised wording" {
		t.Errorf("content across restart = %d messages, first speech %q", len(after.Messages), after.Messages[0].SpeechText)
	}
	for i := range before.Messages {
		if before.Messages[i].ID != after.Messages[i].ID {
			t.Errorf("message %d has a different id after restart", i)
		}
	}
	// The publication pointer, the digest and the audit trail are all durable too:
	// a restart must not silently un-publish a revision or lose who shipped it.
	current, _, err := reopened.PublishedProfile(ctx, f.edition.ID)
	if err != nil || current.ID != published.ID || current.ManifestDigest != published.ManifestDigest {
		t.Errorf("published pointer after restart = %+v, %v", current, err)
	}
	afterActions, err := reopened.PublishActions(ctx, f.edition.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(afterActions) != len(beforeActions) || len(afterActions) == 0 {
		t.Errorf("audit rows after restart = %+v, want the %d written before", afterActions, len(beforeActions))
	}
	if err := reopened.IntegrityCheck(ctx); err != nil {
		t.Errorf("IntegrityCheck after restart: %v", err)
	}
	// The store is still writable after a restart, not just readable.
	if err := reopened.SetProfileStatus(ctx, child.ID, games.StatusReview, after.RowVersion); err != nil {
		t.Errorf("SetProfileStatus after restart: %v", err)
	}
	// And the repository works at whatever schema version this build ships, which
	// is the only version a test may assume.
	if v, err := reopened.SchemaVersion(ctx); err != nil || v != currentSchemaVersion(t) {
		t.Fatalf("SchemaVersion = %d, %v; want %d", v, err, currentSchemaVersion(t))
	}
}

func currentSchemaVersion(t *testing.T) int {
	t.Helper()
	migs, err := Migrations()
	if err != nil {
		t.Fatalf("Migrations: %v", err)
	}
	return len(migs)
}

func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }

func contains(scenes []games.Scene, id string) bool {
	for _, s := range scenes {
		if s.ID == id {
			return true
		}
	}
	return false
}

func containsChar(chars []games.Character, id string) bool {
	for _, c := range chars {
		if c.ID == id {
			return true
		}
	}
	return false
}

func containsSrc(sources []games.Source, id string) bool {
	for _, s := range sources {
		if s.ID == id {
			return true
		}
	}
	return false
}

func truncate(s string) string {
	if len(s) > 60 {
		return s[:60] + "…"
	}
	return s
}

// TestSetProfileNarratorFallback is the store half of the per-profile narrator
// fallback (ADR-0101, issue #11): the dial defaults on (the runtime's
// historical behaviour, migration 0015's DEFAULT 1), a write flips and
// persists it without touching the row version (an editorial setting, not
// content), and a revision snapshot inherits the setting it is cut from.
func TestSetProfileNarratorFallback(t *testing.T) {
	f := newRepoFixture(t)
	ctx := f.ctx

	// The default reads on.
	p, v, err := f.s.GetProfile(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !p.NarratorFallback {
		t.Fatalf("a fresh draft's narrator fallback = off, want the migration default on")
	}

	// Off, then back on: the flag follows the write.
	for _, want := range []bool{false, true} {
		if err := f.s.SetProfileNarratorFallback(ctx, f.profile.ID, want); err != nil {
			t.Fatalf("SetProfileNarratorFallback(%v): %v", want, err)
		}
		p, v2, err := f.s.GetProfile(ctx, f.profile.ID)
		if err != nil {
			t.Fatalf("GetProfile after set: %v", err)
		}
		if p.NarratorFallback != want {
			t.Fatalf("narrator fallback = %v after set(%v)", p.NarratorFallback, want)
		}
		if v2 != v {
			t.Fatalf("row version moved %d -> %d: the dial is an editorial setting, not content", v, v2)
		}
	}

	// A missing profile is ErrNotFound, the way the reads answer.
	if err := f.s.SetProfileNarratorFallback(ctx, games.NewID(), true); !errors.Is(err, ErrNotFound) {
		t.Fatalf("set on a missing profile = %v, want ErrNotFound", err)
	}

	// The snapshot carries the setting it was cut from: a curator who opted
	// this profile out of the narrator fallback does not silently get the
	// fallback back on the next revision.
	if err := f.s.SetProfileNarratorFallback(ctx, f.profile.ID, false); err != nil {
		t.Fatalf("set off before snapshot: %v", err)
	}
	child, err := f.s.NewRevision(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	if child.NarratorFallback {
		t.Fatalf("the snapshot's narrator fallback = on, want the parent's off inherited")
	}
}
