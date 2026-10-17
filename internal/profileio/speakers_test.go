package profileio

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/store"
)

// The speaker-binding tests drive the real import path (file bytes in, store
// rows out) against the synthetic fixtures: no real game text, no mocked parse.

// listAllMessages pages a profile's messages without a limit assumption.
func listAllMessages(t *testing.T, e *env, profileID string) []games.Message {
	t.Helper()
	var out []games.Message
	cursor := ""
	for {
		page, next, err := e.store.ListMessages(e.ctx, profileID, store.MessageFilter{}, cursor, store.MaxListLimit)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		out = append(out, page...)
		if next == "" {
			return out
		}
		cursor = next
	}
}

// TestImportFileCreatesCharactersForNamedSpeakers is the bug this work fixes:
// the CSV fixture names five people in its speaker column, and the import must
// land all five as roster rows with the named messages pointed at them.
func TestImportFileCreatesCharactersForNamedSpeakers(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-speakers", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	report, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}

	// The roster: exactly the five names the file used, confirmed (the source
	// row is the verified reference), each carrying the source id.
	chs, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListCharacters: %v", err)
	}
	if len(chs) != 5 {
		t.Fatalf("characters = %d, want the fixture's five named speakers: %+v", len(chs), chs)
	}
	byName := map[string]games.Character{}
	for _, c := range chs {
		byName[c.Name] = c
		if c.Status != games.CharacterConfirmed {
			t.Errorf("character %q status = %q, want confirmed (a verified source row names it)", c.Name, c.Status)
		}
		if c.ProfileID != p.ID {
			t.Errorf("character %q belongs to profile %s", c.Name, c.ProfileID)
		}
		var refs []string
		if err := json.Unmarshal(c.VerifiedSourceRefs, &refs); err != nil || len(refs) != 1 || refs[0] != report.SourceID {
			t.Errorf("character %q verified_source_refs = %s, want the source id %s", c.Name, c.VerifiedSourceRefs, report.SourceID)
		}
	}
	for _, want := range []string{"Marla", "Corvin", "Boy", "Têvance", "Chef Ondine"} {
		if _, ok := byName[want]; !ok {
			t.Errorf("roster is missing %q; names: %v", want, keys(byName))
		}
	}

	// The messages: 22 of the 40 rows are named, and every named one points at
	// its character with source-column evidence; the 18 unnamed ones stay
	// unknown, with nothing invented.
	msgs := listAllMessages(t, e, p.ID)
	if len(msgs) != 40 {
		t.Fatalf("messages = %d, want the fixture's 40", len(msgs))
	}
	var linked, unknown int
	for _, m := range msgs {
		if m.SpeakerID == "" {
			unknown++
			if m.AttributionStatus != games.AttributionUnknown {
				t.Errorf("unnamed message %s has attribution %q, want unknown", m.SourceMessageID, m.AttributionStatus)
			}
			continue
		}
		linked++
		if m.AttributionStatus != games.AttributionConfirmed || m.AttributionConfidence != 1.0 {
			t.Errorf("named message %s: %q/%v, want confirmed/1.0", m.SourceMessageID, m.AttributionStatus, m.AttributionConfidence)
		}
		var ch games.Character
		for _, c := range chs {
			if c.ID == m.SpeakerID {
				ch = c
			}
		}
		if ch.ID == "" {
			t.Errorf("message %s points at no roster row (%s)", m.SourceMessageID, m.SpeakerID)
		}
		if !strings.Contains(string(m.AttributionEvidence), "source_column") || !strings.Contains(string(m.AttributionEvidence), report.SourceID) {
			t.Errorf("message %s evidence %s must name the source column and the source", m.SourceMessageID, m.AttributionEvidence)
		}
	}
	if linked != 22 || unknown != 18 {
		t.Errorf("linked = %d, unknown = %d; the fixture names 22 of 40 rows", linked, unknown)
	}
	if report.Characters != 5 || report.Speakers != 22 {
		t.Errorf("report says characters=%d speakers=%d, want 5/22", report.Characters, report.Speakers)
	}

	// One script_order assertion per named message, at full posterior, each
	// naming a real message and a real character.
	asserts, err := e.store.ListSpeakerAssertions(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("ListSpeakerAssertions: %v", err)
	}
	if len(asserts) != 22 {
		t.Fatalf("assertions = %d, want one per named message (22)", len(asserts))
	}
	seen := map[string]bool{}
	for _, a := range asserts {
		if a.Method != games.AssertionScriptOrder || a.Posterior != 1.0 {
			t.Errorf("assertion %s = %q/%v, want script_order/1.0", a.ID, a.Method, a.Posterior)
		}
		if !seen[a.MessageID] {
			seen[a.MessageID] = true
		} else {
			t.Errorf("two assertions for message %s; a re-import must not pile up", a.MessageID)
		}
	}
}

// TestImportFileFoldsSpeakerVariantsToOneCharacter proves the matching rule on
// the import path: case- and spacing-variants of one name are one roster row,
// with the first spelling as the name and the rest as aliases.
func TestImportFileFoldsSpeakerVariantsToOneCharacter(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-variants", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	body := "line,speaker\nFirst,Marla\nSecond,marla\nThird, MARLA \nFourth,\n"
	req := ImportRequest{
		ProfileID: p.ID, FileName: "variants.csv", Reader: strings.NewReader(body),
		Rights: games.RightsUserSupplied,
		Parse:  adapter.ParseOptions{Format: generic.FormatCSV, Language: "en"},
	}
	if _, err := ImportFile(e.ctx, e.deps, req); err != nil {
		t.Fatalf("ImportFile: %v", err)
	}

	chs, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 1 {
		t.Fatalf("characters = %d, want one: the variants are one person: %+v", len(chs), chs)
	}
	c := chs[0]
	if c.Name != "Marla" {
		t.Errorf("name = %q, want the first spelling as written", c.Name)
	}
	var aliases []string
	if err := json.Unmarshal(c.Aliases, &aliases); err != nil {
		t.Fatalf("aliases %s: %v", c.Aliases, err)
	}
	if len(aliases) != 2 || aliases[0] != "marla" || aliases[1] != "MARLA" {
		t.Errorf("aliases = %v, want the other spellings in first-occurrence order", aliases)
	}

	msgs := listAllMessages(t, e, p.ID)
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4", len(msgs))
	}
	// ListMessages does not promise source order, so the rows are matched by
	// their text rather than by position.
	byText := map[string]games.Message{}
	for _, m := range msgs {
		byText[m.OriginalText] = m
	}
	for _, text := range []string{"First", "Second", "Third"} {
		m, ok := byText[text]
		if !ok {
			t.Fatalf("message %q is missing", text)
		}
		if m.SpeakerID != c.ID {
			t.Errorf("message %q points at %s, want the one folded character %s", text, m.SpeakerID, c.ID)
		}
	}
	fourth, ok := byText["Fourth"]
	if !ok {
		t.Fatalf("message Fourth is missing")
	}
	if fourth.SpeakerID != "" || fourth.AttributionStatus != games.AttributionUnknown {
		t.Errorf("the unnamed row kept a speaker: %q/%q", fourth.SpeakerID, fourth.AttributionStatus)
	}
}

// TestImportFileCreatesCharactersFromJSONWithExplicitColumn drives the second
// reader family (JSON) with an explicit speaker column mapping.
func TestImportFileCreatesCharactersFromJSONWithExplicitColumn(t *testing.T) {
	e := newEnv(t)
	g := e.game("black-water-speakers", "Black Water")
	p := e.draft(e.edition(g, "EU", "en").ID)

	report, err := ImportFile(e.ctx, e.deps, jsonRequest(t, e, p.ID))
	if err != nil {
		t.Fatalf("ImportFile(json): %v", err)
	}
	chs, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 {
		t.Fatalf("characters = %d, want the fixture's two speakers: %+v", len(chs), chs)
	}
	names := map[string]bool{}
	for _, c := range chs {
		names[c.Name] = true
	}
	if !names["The Ferryman"] || !names["Wren of the Reed Shore"] {
		t.Errorf("roster names = %v, want The Ferryman and Wren of the Reed Shore", keys(names))
	}

	// 4 of the 13 messages are The Ferryman's, 3 are Wren's, 6 unnamed.
	msgs := listAllMessages(t, e, p.ID)
	byChar := map[string]int{}
	var unknown int
	for _, m := range msgs {
		if m.SpeakerID == "" {
			unknown++
			continue
		}
		byChar[m.SpeakerID]++
	}
	if unknown != 6 || len(byChar) != 2 {
		t.Fatalf("byChar = %v, unknown = %d; the fixture names 7 of 13 messages under two speakers", byChar, unknown)
	}
	for _, c := range chs {
		want := 4
		if c.Name == "Wren of the Reed Shore" {
			want = 3
		}
		if byChar[c.ID] != want {
			t.Errorf("character %q has %d linked messages, want %d", c.Name, byChar[c.ID], want)
		}
	}
	if report.Characters != 2 || report.Speakers != 7 {
		t.Errorf("report says characters=%d speakers=%d, want 2/7", report.Characters, report.Speakers)
	}
}

// TestImportFileSpeakerReimportIsIdempotent is the reconcile-not-duplicate rule
// applied to the new rows: the same bytes twice leave one roster row per person
// and one assertion per named message — in the original revision and in a
// derived one, where NewRevision has copied the rows under fresh ids.
func TestImportFileSpeakerReimportIsIdempotent(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-speaker-repeat", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	second, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatalf("re-import: %v", err)
	}
	if second.Characters != 0 || !second.ReusedSource {
		t.Errorf("re-import created %d characters (reused %v); the roster must reconcile, not duplicate", second.Characters, second.ReusedSource)
	}
	chs, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 5 {
		t.Errorf("characters after re-import = %d, want 5", len(chs))
	}
	asserts, err := e.store.ListSpeakerAssertions(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(asserts) != 22 {
		t.Errorf("assertions after re-import = %d, want 22 (one per named message)", len(asserts))
	}
	if n := countMessages(t, e, p.ID); n != 40 {
		t.Errorf("messages after re-import = %d, want 40", n)
	}

	// A derived revision carries the rows under new ids; re-importing the same
	// bytes there must not trip the (message, character, method) unique index
	// and must not add roster rows.
	next := mustNewRevision(t, e, p.ID)
	third, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, next.ID))
	if err != nil {
		t.Fatalf("re-import into the derived revision: %v", err)
	}
	if !third.ReusedSource || third.Characters != 0 {
		t.Errorf("derived revision: reused=%v created=%d, want the copied roster reconciled in place", third.ReusedSource, third.Characters)
	}
	nextCHs, err := e.store.ListCharacters(e.ctx, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nextCHs) != 5 {
		t.Errorf("derived revision characters = %d, want 5", len(nextCHs))
	}
	nextAsserts, err := e.store.ListSpeakerAssertions(e.ctx, next.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(nextAsserts) != 22 {
		t.Errorf("derived revision assertions = %d, want 22 (the copied set, not a second set)", len(nextAsserts))
	}
}

// TestImportFileSpeakerReusesExistingRoster is the other half of reuse: a
// roster that already holds a person (with extra aliases a curator added) is
// not re-created, and the file's spellings merge into it without clobbering
// the curator's work.
func TestImportFileSpeakerReusesExistingRoster(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-roster", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	// A curator row that already names Marla, with a hand-added alias.
	curator := games.Character{
		ID: games.NewID(), ProfileID: p.ID, Name: "Marla",
		Aliases: json.RawMessage(`["Marlow"]`),
		Status:  games.CharacterConfirmed, CreatedAt: nowStamp(),
	}
	if err := curator.Validate(); err != nil {
		t.Fatalf("curator character invalid: %v", err)
	}
	if err := e.store.PutCharacters(e.ctx, p.ID, []games.Character{curator}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	body := "line,speaker\nFirst,Marla\nSecond,marla\nThird,Captain Salt\n"
	report, err := ImportFile(e.ctx, e.deps, ImportRequest{
		ProfileID: p.ID, FileName: "roster.csv", Reader: strings.NewReader(body),
		Rights: games.RightsUserSupplied,
		Parse:  adapter.ParseOptions{Format: generic.FormatCSV, Language: "en"},
	})
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if report.Characters != 1 {
		t.Errorf("created = %d, want only the one name the roster lacks (Captain Salt)", report.Characters)
	}
	chs, err := e.store.ListCharacters(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 {
		t.Fatalf("characters = %d, want the curator row plus one new: %+v", len(chs), chs)
	}
	var marla games.Character
	for _, c := range chs {
		if c.Name == "Marla" {
			marla = c
		}
	}
	if marla.ID != curator.ID {
		t.Fatalf("the roster row was replaced (was %s, now %s); reuse means keep the id", curator.ID, marla.ID)
	}
	var aliases []string
	if err := json.Unmarshal(marla.Aliases, &aliases); err != nil {
		t.Fatal(err)
	}
	// The curator's alias survives; the file's lowercase spelling is added.
	if !containsString(aliases, "Marlow") || !containsString(aliases, "marla") {
		t.Errorf("aliases = %v, want the curator's alias kept and the file's spelling added", aliases)
	}
	msgs := listAllMessages(t, e, p.ID)
	for _, m := range msgs {
		if m.SourceMessageID == "" {
			continue
		}
		switch m.OriginalText {
		case "First", "Second":
			if m.SpeakerID != curator.ID {
				t.Errorf("message %q points at %s, want the curator's row", m.OriginalText, m.SpeakerID)
			}
		case "Third":
			if m.SpeakerID == "" || m.SpeakerID == curator.ID {
				t.Errorf("message %q must point at the new character, not the roster's Marla", m.OriginalText)
			}
		}
	}
}

// TestExportImportRoundTripPreservesSpeakers is the portability leg for the new
// rows: characters and message→character links survive export → import into a
// fresh database, and the digest of a re-export is stable.
func TestExportImportRoundTripPreservesSpeakers(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-portability", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}

	before := profileContentFor(t, e, p.ID)
	if len(before.Characters) != 5 {
		t.Fatalf("exported characters = %d, want 5", len(before.Characters))
	}
	// Name the original profile's mapping at content level — normalized text
	// and speaker name — because that is the fact the round trip must preserve,
	// and ids are remapped on the way across machines.
	var linkedBefore int
	for _, m := range before.Messages {
		if m.SpeakerID == nil {
			continue
		}
		linkedBefore++
	}
	if linkedBefore != 22 {
		t.Fatalf("exported linked messages = %d, want 22", linkedBefore)
	}

	// Export a second time: the digest must not move, or a re-export would
	// imply the profile changed.
	again := profileContentFor(t, e, p.ID)
	if again.Digest != before.Digest {
		t.Errorf("re-export digest = %s, want the first export's %s", again.Digest, before.Digest)
	}

	charIDToName := map[string]string{}
	for _, c := range before.Characters {
		charIDToName[c.ID] = c.Name
	}

	fresh := newIn(t)
	// Unpack is not involved: the manifest is the portable unit, and the
	// missing source blobs are reported, not fatal.
	var buf bytes.Buffer
	if err := WriteManifest(&buf, before); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	rep, err := ImportManifest(e.ctx, fresh.deps, &buf, ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}

	after := profileContentFor(t, fresh, rep.ProfileID)
	if len(after.Characters) != len(before.Characters) {
		t.Fatalf("re-imported characters = %d, want %d", len(after.Characters), len(before.Characters))
	}
	afterNameOf := map[string]string{}
	for _, c := range after.Characters {
		afterNameOf[c.ID] = c.Name
	}
	gotNameOfBeforeID := map[string]string{}
	for _, m := range before.Messages {
		if m.SpeakerID == nil {
			continue
		}
		// Find the same message in the re-import by its source-side identity.
		for _, am := range after.Messages {
			if am.SourceMessageID == nil || m.SourceMessageID == nil || *am.SourceMessageID != *m.SourceMessageID {
				continue
			}
			if am.SpeakerID == nil {
				t.Errorf("message %s lost its speaker on the round trip", *m.SourceMessageID)
				continue
			}
			gotNameOfBeforeID[*m.SourceMessageID] = afterNameOf[*am.SpeakerID]
			if gotNameOfBeforeID[*m.SourceMessageID] != charIDToName[*m.SpeakerID] {
				t.Errorf("message %s: speaker = %q, want %q", *m.SourceMessageID,
					gotNameOfBeforeID[*m.SourceMessageID], charIDToName[*m.SpeakerID])
			}
		}
	}
	if len(gotNameOfBeforeID) != 22 {
		t.Errorf("round trip preserved %d speaker links, want 22", len(gotNameOfBeforeID))
	}

	// The assertions cross the boundary too, in the same count.
	if len(after.SpeakerAssertions) != len(before.SpeakerAssertions) {
		t.Errorf("assertions: %d exported, %d re-imported", len(before.SpeakerAssertions), len(after.SpeakerAssertions))
	}

	// And the two machines agree on the content fingerprint: ids are
	// canonicalized away, so a profile moved to a fresh database digests the
	// same as the one it left.
	if after.Digest != before.Digest {
		t.Errorf("fresh-DB export digest = %s, want the original %s (ids must not leak into the fingerprint)", after.Digest, before.Digest)
	}
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
