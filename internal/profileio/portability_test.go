package profileio

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestTwoGamePortability is docs/spec/16's Milestone 1 acceptance line — "create
// edit/revise/publish/export/reimport two synthetic game profiles" — run against
// two scripts whose column names, speaker conventions, page addressing and
// structure deliberately disagree (docs/spec/04's portability rule). The core
// path is the same code for both: only the ParseOptions differ, and if a future
// edit made this pass by special-casing one file, the diff would be visible in
// this test's failure text rather than hidden in a runtime condition.
func TestTwoGamePortability(t *testing.T) {
	t.Parallel()
	for _, spec := range []struct {
		name    string
		fixture string
		slug    string
		title   string
		region  string
		request func(t *testing.T, e *env, profileID string) ImportRequest
	}{
		{"csv", "synthetic-adventure.csv", "synthetic-adventure-portable", "Synthetic Adventure", "US", csvRequest},
		{"json", "synthetic-other-game.json", "black-water-portable", "Black Water", "EU", jsonRequest},
	} {
		t.Run(spec.name, func(t *testing.T) {
			runPortability(t, spec.name, spec.fixture, spec.slug, spec.title, spec.region, spec.request)
		})
	}
}

func runPortability(t *testing.T, label, fixture, slug, title, region string, request func(*testing.T, *env, string) ImportRequest) {
	t.Helper()
	e := newEnv(t)
	g := e.game(slug, title)
	edition := e.edition(g, region, "en")
	first := e.draft(edition.ID)

	// ── create: import the file through the adapter ──────────────────────────
	report, err := ImportFile(e.ctx, e.deps, request(t, e, first.ID))
	if err != nil {
		t.Fatalf("ImportFile(%s): %v", label, err)
	}
	if report.Stats.Imported == 0 {
		t.Fatalf("nothing imported from %s", fixture)
	}
	content := mustContent(t, e, first.ID)
	if len(content.Messages) != report.Stats.Imported {
		t.Fatalf("stored %d messages, report says %d", len(content.Messages), report.Stats.Imported)
	}

	// ── edit: curate the rows the parser could not have produced alone ───────
	// Characters, scenes, a region, edges and one speaker assertion are what
	// separates a parse from a profile, and every one of them carries a foreign
	// key that a re-import has to relabel.
	decorate(t, e, first.ID, content)

	// The one field a curator types by hand: the spoken rendering.
	const spokenEdit = "Speak the mooring-bell line slowly, with a breath after 'tide'."
	editedID := content.Messages[0].ID
	if _, err := e.store.UpdateMessageSpeechText(e.ctx, first.ID, editedID, spokenEdit, versionOf(t, e, first.ID)); err != nil {
		t.Fatalf("UpdateMessageSpeechText: %v", err)
	}

	// ── publish revision 1 ──────────────────────────────────────────────────
	published := publish(t, e, first.ID, "curator-a@local")

	// ── revise, edit again, publish: the first revision is archived ─────────
	second := mustNewRevision(t, e, published.ID)
	content2 := mustContent(t, e, second.ID)
	const spokenRevise = "Second revision wording: the bell rings twice before the ferry."
	if _, err := e.store.UpdateMessageSpeechText(e.ctx, second.ID, content2.Messages[1].ID, spokenRevise, versionOf(t, e, second.ID)); err != nil {
		t.Fatalf("UpdateMessageSpeechText(revision 2): %v", err)
	}
	published2 := publish(t, e, second.ID, "curator-b@local")

	if published2.RevisionNum != 2 {
		t.Errorf("second publication is revision %d, want 2", published2.RevisionNum)
	}
	if parent, _, err := e.store.GetProfile(e.ctx, published.ID); err != nil {
		t.Fatal(err)
	} else if parent.Status != games.StatusArchived {
		t.Errorf("the superseded revision is %q, want archived", parent.Status)
	}
	served, _, err := e.store.PublishedProfile(e.ctx, edition.ID)
	if err != nil {
		t.Fatal(err)
	}
	if served.ID != published2.ID {
		t.Errorf("the edition serves %s, want the newest publication %s", served.ID, published2.ID)
	}

	// ── export ──────────────────────────────────────────────────────────────
	before := profileContentFor(t, e, published2.ID)
	file := mustManifestBytes(t, before)
	if err := ValidateDocument(file); err != nil {
		t.Fatalf("exported manifest does not satisfy the schema: %v", err)
	}
	if !bytes.Contains(file, []byte(fixture)) {
		t.Errorf("export does not point at the source file name it came from")
	}

	// ── re-import into a fresh database ─────────────────────────────────────
	fresh := newIn(t)
	imported, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(file), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	if imported.MessagesWritten != len(before.Messages) {
		t.Errorf("re-import wrote %d messages, manifest holds %d", imported.MessagesWritten, len(before.Messages))
	}
	if len(imported.MissingBlobs) != 1 {
		t.Errorf("re-import reported %d missing blobs, want the one source file: %v", len(imported.MissingBlobs), imported.MissingBlobs)
	}
	if imported.MissingBlobs[0] != report.SHA256 {
		t.Errorf("missing blob %q is not the imported file's hash %q", imported.MissingBlobs[0], report.SHA256)
	}
	if imported.Digest != before.Digest {
		t.Errorf("re-import digests %s, manifest says %s", imported.Digest, before.Digest)
	}

	// The new draft is a draft, of a newly created game and edition, with new ids.
	rev, _, err := fresh.store.GetProfile(fresh.ctx, imported.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Status != games.StatusDraft {
		t.Errorf("re-imported revision is %q, want draft", rev.Status)
	}
	if rev.ID == published2.ID || rev.EditionID == edition.ID {
		t.Error("re-import reused the source database's identifiers")
	}

	// ── the same content, and the same digest after a second export ─────────
	after := profileContentFor(t, fresh, imported.ProfileID)
	if after.Digest != before.Digest {
		t.Errorf("digest after re-import is %s, exported %s\n%s", after.Digest, before.Digest, describeCanonicalDiff(t, before, after))
	}
	if !bytes.Equal(Canonical(before), Canonical(after)) {
		t.Errorf("canonical projections differ after a round trip:\n%s", describeCanonicalDiff(t, before, after))
	}
	compareByContent(t, before, after)

	// And a second export of the re-imported revision reproduces its own digest,
	// so the file is stable rather than drifting one step per hop.
	again := profileContentFor(t, fresh, imported.ProfileID)
	if again.Digest != after.Digest {
		t.Errorf("exporting twice gave %s then %s", after.Digest, again.Digest)
	}

	// Re-importing the *re-imported* manifest is allowed and yields the same
	// content again: two hops, no drift.
	third := newIn(t)
	imported2, err := ImportManifest(third.ctx, third.deps, bytes.NewReader(mustManifestBytes(t, after)), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("second-hop ImportManifest: %v", err)
	}
	if got := profileContentFor(t, third, imported2.ProfileID); got.Digest != before.Digest {
		t.Errorf("two hops changed the digest: %s then %s", before.Digest, got.Digest)
	}

	// Import onto the edition the previous re-import created: same region and
	// language, so no force is needed, and a second catalogue row must not be
	// minted for a title the database already names.
	target := mustGameEdition(t, fresh, imported.EditionID)
	fourth, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(file), ImportManifestOptions{TargetEditionID: target.ID})
	if err != nil {
		t.Fatalf("ImportManifest(into an existing edition): %v", err)
	}
	if fourth.EditionID != target.ID {
		t.Errorf("re-import onto a named edition landed on %s", fourth.EditionID)
	}
	if got := profileContentFor(t, fresh, fourth.ProfileID); got.Digest != before.Digest {
		t.Errorf("import onto an existing edition changed the digest: %s vs %s", before.Digest, got.Digest)
	}
	if editionID := mustGameEdition(t, fresh, fourth.EditionID).ID; editionID != target.ID {
		t.Errorf("re-import created a second edition (%s) for a title the database already had (%s)", editionID, target.ID)
	}
}

//
// ── fixtures and comparisons ────────────────────────────────────────────────
//

// decorate adds the curation layer: a roster, two scenes, a region, a small
// graph, one speaker assertion, and messages wired to the rows they now name.
//
// It is written against ids the *store* minted, and every one of them is a
// reference a re-import must relabel — which is the point. A manifest that
// round-trips text but loses the graph has not been made portable, only shorter.
func decorate(t *testing.T, e *env, profileID string, content store.ProfileContent) {
	t.Helper()
	now := nowStamp()

	narrator := games.Character{
		ID: games.NewID(), ProfileID: profileID, Name: "The Harbourmaster",
		Aliases:            json.RawMessage(`["Harbourmaster","Port Reckoner"]`),
		Role:               "official",
		PersonalitySummary: "Counts crates, mistrusts strangers, keeps the tide table.",
		Status:             games.CharacterProposed,
		CreatedAt:          now,
	}
	second := games.Character{
		ID: games.NewID(), ProfileID: profileID, Name: "Corvin",
		Aliases:            json.RawMessage(`[]`),
		VisualDescriptors:  json.RawMessage(`["rope-stained coat"]`),
		Role:               "crew",
		Status:             games.CharacterConfirmed,
		VerifiedSourceRefs: json.RawMessage(`["` + content.Sources[0].ID + `"]`),
		CreatedAt:          now,
	}
	for _, ch := range []games.Character{narrator, second} {
		if err := ch.Validate(); err != nil {
			t.Fatalf("character fixture invalid: %v", err)
		}
	}
	if err := e.store.PutCharacters(e.ctx, profileID, []games.Character{narrator, second}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	harbour := games.Scene{
		ID: games.NewID(), ProfileID: profileID, Name: "Harbour Quay",
		Aliases: json.RawMessage(`["quay","berth 4"]`), CreatedAt: now,
	}
	wreck := games.Scene{
		ID: games.NewID(), ProfileID: profileID, Name: "The Old Wreck",
		Aliases: json.RawMessage(`[]`), CreatedAt: now,
	}
	if err := e.store.PutScenes(e.ctx, profileID, []games.Scene{harbour, wreck}); err != nil {
		t.Fatalf("PutScenes: %v", err)
	}

	region := games.Region{
		ID: games.NewID(), ProfileID: profileID, SceneID: harbour.ID, Label: "lower textbox",
		RegionType: games.RegionTextbox, DetectMethod: games.DetectManual,
		RectX: 0.07, RectY: 0.72, RectW: 0.86, RectH: 0.23,
		OCRSettings: json.RawMessage(`{"engine":"tesseract","psm":6,"language":"eng"}`),
		Revision:    1, UpdatedAt: now,
	}
	if err := region.Validate(); err != nil {
		t.Fatalf("region fixture invalid: %v", err)
	}
	if err := e.store.PutRegions(e.ctx, profileID, []games.Region{region}); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}

	// Wire three messages to the new rows: one named speaker, one scene, one of
	// each. Attribution rules apply (docs/spec/02): a speaker claim carries its
	// status, and a rejected one cannot keep the speaker.
	msgs := content.Messages
	wired := make([]games.Message, 0, 3)
	for i := range msgs {
		m := msgs[i]
		switch i {
		case 0:
			m.SpeakerID, m.SceneID = narrator.ID, harbour.ID
			m.AttributionStatus, m.AttributionConfidence = games.AttributionConfirmed, 0.9
			m.AttributionEvidence = json.RawMessage(`[{"method":"script_order","note":"named in the source column"}]`)
		case 1:
			m.SceneID = harbour.ID
		case 2:
			m.SceneID, m.SpeakerID = wreck.ID, second.ID
			m.AttributionStatus = games.AttributionProposed
			m.AttributionConfidence = 0.4
		default:
			continue
		}
		wired = append(wired, m)
	}
	if err := e.store.PutMessages(e.ctx, profileID, wired, nil); err != nil {
		t.Fatalf("PutMessages(wiring): %v", err)
	}

	edges := []games.MessageEdge{
		{FromID: msgs[0].ID, ToID: msgs[1].ID, EdgeType: games.EdgeNext, Confidence: 1, SourceID: content.Sources[0].ID, CreatedAt: now},
		{FromID: msgs[1].ID, ToID: msgs[2].ID, EdgeType: games.EdgeBranch, ConditionExpr: `flags.has_rope == true`, Confidence: 0.9, CreatedAt: now},
		{FromID: msgs[2].ID, ToID: msgs[2].ID, EdgeType: games.EdgeRepeat, Confidence: 1, CreatedAt: now},
		{FromID: msgs[1].ID, ToID: msgs[0].ID, EdgeType: games.EdgeInferred, Confidence: 0.5, CreatedAt: now},
	}
	if err := e.store.PutEdges(e.ctx, profileID, edges); err != nil {
		t.Fatalf("PutEdges: %v", err)
	}

	assertion := games.SpeakerAssertion{
		ID: games.NewID(), ProfileID: profileID, MessageID: msgs[2].ID, CharacterID: second.ID,
		Method: games.AssertionScriptOrder, AssertedAt: now,
		Evidence: json.RawMessage(`[{"column":"speaker","value":"Corvin"}]`), Posterior: 0.4,
	}
	if err := assertion.Validate(); err != nil {
		t.Fatalf("assertion fixture invalid: %v", err)
	}
	if err := e.store.PutSpeakerAssertions(e.ctx, profileID, []games.SpeakerAssertion{assertion}); err != nil {
		t.Fatalf("PutSpeakerAssertions: %v", err)
	}
}

// compareByContent checks the round trip field by field, keyed by the identities
// a manifest keeps stable (source message ids, names, labels). The digest
// comparison above says "the canonical bytes agree"; this says what those bytes
// mean, so a failure names the row rather than a byte offset.
func compareByContent(t *testing.T, before, after Manifest) {
	t.Helper()

	byKey := func(msgs []MessageInfo) map[string]MessageInfo {
		out := map[string]MessageInfo{}
		for _, m := range msgs {
			key := textOrEmpty(m.SourceMessageID)
			if key == "" {
				key = "kind=" + string(m.Kind) + " text=" + textOrEmpty(m.OriginalText)
			}
			out[key] = m
		}
		if len(out) != len(msgs) {
			t.Errorf("%d messages collapse to %d keys; the fixture has two rows with the same identity", len(msgs), len(out))
		}
		return out
	}
	a, b := byKey(before.Messages), byKey(after.Messages)
	for key, want := range a {
		got, ok := b[key]
		if !ok {
			t.Errorf("message %q did not survive the round trip", key)
			continue
		}
		if textOrEmpty(got.NormalizedText) != textOrEmpty(want.NormalizedText) {
			t.Errorf("message %q normalized text = %q, want %q", key, textOrEmpty(got.NormalizedText), textOrEmpty(want.NormalizedText))
		}
		if textOrEmpty(got.SpeechText) != textOrEmpty(want.SpeechText) {
			t.Errorf("message %q speech text = %q, want %q", key, textOrEmpty(got.SpeechText), textOrEmpty(want.SpeechText))
		}
		if textOrEmpty(got.OriginalText) != textOrEmpty(want.OriginalText) {
			t.Errorf("message %q original text = %q, want %q", key, textOrEmpty(want.OriginalText), textOrEmpty(want.OriginalText))
		}
		if got.Kind != want.Kind || got.Language != want.Language {
			t.Errorf("message %q came back as %q/%q, want %q/%q", key, got.Kind, got.Language, want.Kind, want.Language)
		}
		if len(got.Pages) != len(want.Pages) {
			t.Errorf("message %q has %d pages, want %d", key, len(got.Pages), len(want.Pages))
			continue
		}
		for i := range want.Pages {
			if textOrEmpty(got.Pages[i].Text) != textOrEmpty(want.Pages[i].Text) || got.Pages[i].Index != want.Pages[i].Index {
				t.Errorf("message %q page %d = %q@%d, want %q@%d", key, i,
					textOrEmpty(got.Pages[i].Text), got.Pages[i].Index, textOrEmpty(want.Pages[i].Text), want.Pages[i].Index)
			}
		}
	}
	for key := range b {
		if _, ok := a[key]; !ok {
			t.Errorf("message %q appeared out of nowhere", key)
		}
	}

	names := func(cs []CharacterInfo) []string {
		out := make([]string, 0, len(cs))
		for _, c := range cs {
			out = append(out, c.Name)
		}
		// A manifest's sections are canonically ordered by content, so roster
		// order is not a fact about the profile and is not compared.
		sort.Strings(out)
		return out
	}
	if strings.Join(names(before.Characters), ",") != strings.Join(names(after.Characters), ",") {
		t.Errorf("characters = %v, want %v", names(after.Characters), names(before.Characters))
	}
	for _, want := range before.Characters {
		var got *CharacterInfo
		for i := range after.Characters {
			if after.Characters[i].Name == want.Name {
				got = &after.Characters[i]
			}
		}
		if got == nil {
			continue
		}
		if string(got.Aliases) != string(want.Aliases) {
			t.Errorf("character %q aliases = %s, want %s", want.Name, got.Aliases, want.Aliases)
		}
		if got.Role != want.Role || got.Status != want.Status || got.Summary != want.Summary {
			t.Errorf("character %q came back as %q/%q/%q", want.Name, got.Role, got.Status, got.Summary)
		}
	}

	scenes := func(ss []SceneInfo) map[string]string {
		out := map[string]string{}
		for _, s := range ss {
			out[s.Name] = string(s.Aliases)
		}
		return out
	}
	if fmt.Sprint(scenes(before.Scenes)) != fmt.Sprint(scenes(after.Scenes)) {
		t.Errorf("scenes = %v, want %v", scenes(after.Scenes), scenes(before.Scenes))
	}

	// Edges are addressed by message ids, which do not survive: compare them as
	// (source message, source message, type) triples.
	edgeKey := func(m Manifest, e EdgeInfo) string {
		idToKey := map[string]string{}
		for _, msg := range m.Messages {
			idToKey[msg.ID] = textOrEmpty(msg.SourceMessageID)
		}
		sceneOf := func(id string) string {
			for _, msg := range m.Messages {
				if msg.ID == id {
					return textOrEmpty(msg.SourceMessageID)
				}
			}
			return id
		}
		return sceneOf(e.From) + " -> " + sceneOf(e.To) + " (" + string(e.Type) + ")"
	}
	beforeEdges, afterEdges := map[string]bool{}, map[string]bool{}
	for _, e := range before.MessageEdges {
		beforeEdges[edgeKey(before, e)] = true
	}
	for _, e := range after.MessageEdges {
		afterEdges[edgeKey(after, e)] = true
	}
	for key := range beforeEdges {
		if !afterEdges[key] {
			t.Errorf("edge %s did not survive the round trip", key)
		}
	}
	if len(beforeEdges) != len(afterEdges) {
		t.Errorf("edges: %d exported, %d re-imported", len(beforeEdges), len(afterEdges))
	}
	if len(before.SpeakerAssertions) != len(after.SpeakerAssertions) {
		t.Errorf("assertions: %d exported, %d re-imported", len(before.SpeakerAssertions), len(after.SpeakerAssertions))
	}
	if len(before.Regions) != len(after.Regions) {
		t.Errorf("regions: %d exported, %d re-imported", len(before.Regions), len(after.Regions))
	}
	if len(after.Regions) > 0 && after.Regions[0].SceneID == nil {
		t.Error("the region lost the scene it belongs to")
	}
}

func describeCanonicalDiff(t *testing.T, before, after Manifest) string {
	t.Helper()
	a, b := string(Canonical(before)), string(Canonical(after))
	if a == b {
		return "canonical bytes are equal (the digests disagreed with themselves?)"
	}
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	lo := i - 60
	if lo < 0 {
		lo = 0
	}
	hi := i + 240
	window := func(s string) string {
		if len(s) < hi {
			return s[lo:]
		}
		return s[lo:hi]
	}
	return fmt.Sprintf("first difference at byte %d of %d/%d:\n  exported: %s\n  reimported: %s", i, len(a), len(b), window(a), window(b))
}

func mustContent(t *testing.T, e *env, profileID string) store.ProfileContent {
	t.Helper()
	c, err := e.store.GetProfileContent(e.ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfileContent(%s): %v", profileID, err)
	}
	return c
}

func mustGameEdition(t *testing.T, e *env, id string) games.Edition {
	t.Helper()
	got, err := e.store.GetEdition(e.ctx, id)
	if err != nil {
		t.Fatalf("GetEdition(%s): %v", id, err)
	}
	return got
}

func mustGame(t *testing.T, e *env, id string) games.Game {
	t.Helper()
	g, err := e.store.GetGame(e.ctx, id)
	if err != nil {
		t.Fatalf("GetGame(%s): %v", id, err)
	}
	return g
}

// TestRestartKeepsImportedProfile is docs/spec/12's restart dimension applied to
// this path: close both stores, reopen the same two files, and require that the
// provenance, the messages and the digest are what was written and not a
// reconstruction from memory.
func TestRestartKeepsImportedProfile(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-restart", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	report, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	before := profileContentFor(t, e, p.ID)
	dbPath, assetRoot := e.store.Path(), e.assets.Root()

	e.closeAll()
	reopened := &env{t: t, ctx: testCtx(t), dir: filepath.Join(dbPath, "..")}
	reopened.open()
	if reopened.store.Path() != dbPath {
		t.Fatalf("reopened a different database: %s vs %s", reopened.store.Path(), dbPath)
	}
	if reopened.assets.Root() != assetRoot {
		t.Fatalf("reopened a different asset root: %s vs %s", reopened.assets.Root(), assetRoot)
	}

	after := profileContentFor(t, reopened, p.ID)
	if after.Digest != before.Digest {
		t.Errorf("digest changed across a restart: %s then %s\n%s", before.Digest, after.Digest, describeCanonicalDiff(t, before, after))
	}
	if err := reopened.assets.Verify(reopened.ctx, report.SHA256); err != nil {
		t.Errorf("the source blob did not survive the restart: %v", err)
	}
	sources, err := reopened.store.ListSources(reopened.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].ContentSHA256 != report.SHA256 {
		t.Errorf("provenance did not survive the restart: %+v", sources)
	}

	// Writing continues where the previous process left off.
	again, err := ImportFile(reopened.ctx, reopened.deps, csvRequest(t, reopened, p.ID))
	if err != nil {
		t.Fatalf("import after restart: %v", err)
	}
	if !again.ReusedSource {
		t.Error("after a restart the same bytes must resolve to the same source row")
	}
	if got := countMessages(t, reopened, p.ID); got != len(before.Messages) {
		t.Errorf("messages after a restart and a re-import = %d, want the same %d", got, len(before.Messages))
	}
}

// TestUnpackMakesBlobsPresent covers the other half of the pack contract: a
// bundle that arrives with its bytes leaves the recipient with nothing to
// re-supply.
func TestUnpackMakesBlobsPresent(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-pack", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	report, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := Export(e.ctx, e.store, p.ID, ExportOptions{IncludeSourceBytes: true, AcknowledgeCopyright: true})
	if err != nil {
		t.Fatal(err)
	}

	var pack bytes.Buffer
	if _, err := ExportBundle(e.ctx, e.deps, p.ID, ExportOptions{IncludeSourceBytes: true, AcknowledgeCopyright: true}, &pack); err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}

	fresh := newIn(t)
	unpacked, err := Unpack(fresh.ctx, fresh.assets, bytes.NewReader(pack.Bytes()))
	if err != nil {
		t.Fatalf("Unpack: %v", err)
	}
	if len(unpacked.Blobs) != 1 || unpacked.Blobs[0] != report.SHA256 {
		t.Errorf("unpacked blobs = %v, want the one source", unpacked.Blobs)
	}
	if err := fresh.assets.Verify(fresh.ctx, report.SHA256); err != nil {
		t.Errorf("unpacked blob does not verify: %v", err)
	}
	imported, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(mustManifestBytes(t, unpacked.Manifest)), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest(after Unpack): %v", err)
	}
	if len(imported.MissingBlobs) != 0 {
		t.Errorf("after unpacking the pack the recipient still reports %v missing", imported.MissingBlobs)
	}
	if len(imported.PresentBlobs) != 1 {
		t.Errorf("present blobs = %v, want the unpacked source", imported.PresentBlobs)
	}
	if imported.Digest != manifest.Digest {
		t.Errorf("bundle digest drifted: %s vs %s", manifest.Digest, imported.Digest)
	}
}

// TestImportManifestChecksEditionCompatibility is docs/spec/12's "source
// cross-edition mixup" case: a JP script may not be silently imported onto a US
// edition, and the override has to be asked for.
func TestImportManifestChecksEditionCompatibility(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-compat", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	file := mustManifestBytes(t, profileContentFor(t, e, p.ID))

	other := newIn(t)
	mismatchedGame := other.game("other-tide", "Other Tide")
	target := other.edition(mismatchedGame, "JP", "ja")

	if _, err := ImportManifest(other.ctx, other.deps, bytes.NewReader(file), ImportManifestOptions{TargetEditionID: target.ID}); !errors.Is(err, ErrIncompatibleEdition) {
		t.Errorf("err = %v, want ErrIncompatibleEdition", err)
	}
	// With Force the import happens and says what it accepted.
	imported, err := ImportManifest(other.ctx, other.deps, bytes.NewReader(file), ImportManifestOptions{TargetEditionID: target.ID, Force: true})
	if err != nil {
		t.Fatalf("forced import: %v", err)
	}
	if imported.EditionID != target.ID {
		t.Errorf("forced import landed on edition %s", imported.EditionID)
	}
	if _, _, err := other.store.GetProfile(other.ctx, imported.ProfileID); err != nil {
		t.Errorf("the forced draft does not exist: %v", err)
	}
	// An unknown target is a lookup failure, not a compatibility one.
	if _, err := ImportManifest(other.ctx, other.deps, bytes.NewReader(file), ImportManifestOptions{TargetEditionID: games.NewID()}); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown target: err = %v, want store.ErrNotFound", err)
	}
	// A matching edition needs no force.
	compatible := other.edition(mustGame(t, other, mismatchedGame.ID), "US", "en")
	if _, err := ImportManifest(other.ctx, other.deps, bytes.NewReader(file), ImportManifestOptions{TargetEditionID: compatible.ID}); err != nil {
		t.Errorf("compatible target: %v", err)
	}
}
