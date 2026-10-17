package profileio

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// TestDigestTamperRejected pins both halves of what manifest_digest is for.
//
// The first half is the obvious one: content that does not hash to the digest the
// document carries is refused, because docs/spec/02 makes verification at import
// the price of portability, and a manifest that was edited after export is not
// evidence of anything.
//
// The second half is the part that is easy to get wrong in the other direction.
// The lifecycle section is *not* in the digest, on purpose: a re-import is a draft
// by construction, so including status, revision number or an approval timestamp
// would make the digest of a published revision impossible to reproduce after the
// round trip docs/spec/16 demands. That is only safe because importing a manifest
// that claims to be published creates a draft anyway — so this test pins the
// inertness too, rather than leaving "not covered by the digest" to read as an
// accident.
func TestDigestTamperRejected(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-digest", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	m := profileContentFor(t, e, p.ID)
	good := mustManifestBytes(t, m)

	fresh := newIn(t)
	doc := decodeDocument(t, good)

	// 1. A word changed in one message's spoken rendering.
	edited := cloneDocument(doc)
	message := edited["messages"].([]any)[2].(map[string]any)
	message["speech_text"] = "Speak this line as though it were somebody else's."
	if _, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(encodeDocument(t, edited)), ImportManifestOptions{}); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("edited speech text: err = %v, want ErrDigestMismatch", err)
	}

	// 2. A rights state quietly upgraded — the tamper that actually matters,
	// because it is the one that would let withheld text ship.
	upgraded := cloneDocument(doc)
	upgraded["sources"].([]any)[0].(map[string]any)["rights"] = string(games.RightsLicensed)
	if _, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(encodeDocument(t, upgraded)), ImportManifestOptions{}); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("upgraded rights: err = %v, want ErrDigestMismatch", err)
	}

	// 3. A dropped message, and a renumbered page: content edits at any depth.
	dropped := cloneDocument(doc)
	msgs := dropped["messages"].([]any)
	dropped["messages"] = msgs[:len(msgs)-1]
	// The dropped message may carry speaker assertions: drop those too, so the
	// tampered document stays internally consistent and it is the digest — not
	// the reference check — that catches the edit.
	if row, ok := msgs[len(msgs)-1].(map[string]any); ok {
		if removedID, _ := row["id"].(string); removedID != "" {
			list := dropped["speaker_assertions"].([]any)
			kept := list[:0]
			for _, a := range list {
				if m, _ := a.(map[string]any); m["message"] != removedID {
					kept = append(kept, a)
				}
			}
			dropped["speaker_assertions"] = kept
		}
	}
	if _, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(encodeDocument(t, dropped)), ImportManifestOptions{}); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("dropped message: err = %v, want ErrDigestMismatch", err)
	}

	// 4. The digest is the document's own claim: a manifest that carries none is
	// not trusted by default.
	unsigned := m
	unsigned.Digest = ""
	unsignedBytes, err := Encode(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(unsigned); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("an unstamped manifest verified: %v", err)
	}
	// Import refuses it. Which refusal it is depends on which check runs first —
	// the schema forbids an empty digest, and the digest check forbids a missing
	// one — and either is the right answer.
	if _, err := DecodeManifestBytes(unsignedBytes); !errors.Is(err, ErrDigestMismatch) && !errors.Is(err, ErrSchemaValidation) {
		t.Errorf("ImportManifest accepted an unsigned document: %v", err)
	}

	// 5. The lifecycle section: not in the digest, and worth nothing on the way
	// in. A manifest edited to claim it was published by somebody famous still
	// becomes a draft, and the audit trail of the real publication stays in the
	// database it happened in (ADR-0015).
	lie := cloneDocument(doc)
	pub := lie["publication"].(map[string]any)
	pub["status"] = string(games.StatusPublished)
	pub["author"] = "publisher-in-a-hurry@example.invalid"
	at := "2026-10-16T12:30:10Z"
	pub["approved_at"] = &at
	liedBytes := encodeDocument(t, lie)
	if bytes.Equal(liedBytes, encodeDocument(t, doc)) {
		t.Fatal("the test did not actually change the lifecycle section")
	}
	imported, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(liedBytes), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("a manifest whose lifecycle section was forged must still import (it is ignored): %v", err)
	}
	rev, _, err := fresh.store.GetProfile(fresh.ctx, imported.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Status != games.StatusDraft {
		t.Errorf("importing a manifest claiming %q produced %q", games.StatusPublished, rev.Status)
	}
	if rev.PublicationAuthor != "" {
		t.Errorf("the forged author was recorded: %q", rev.PublicationAuthor)
	}
	if rev.PublishedAt != "" {
		t.Errorf("the forged approval timestamp was recorded: %q", rev.PublishedAt)
	}
	if digest := ContentDigest(m); digest != m.Digest {
		t.Error("the exported manifest does not carry its own digest")
	}
}

// TestDigestSurvivesRelabelling is the property the whole canonicalization
// exists for: rewrite every identifier in a manifest, including the ones inside
// JSON payloads, and the content fingerprint is unchanged.
func TestDigestSurvivesRelabelling(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-relabel", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	decorate(t, e, p.ID, mustContent(t, e, p.ID))
	m := profileContentFor(t, e, p.ID)
	if len(m.SpeakerAssertions) == 0 || len(m.MessageEdges) == 0 {
		t.Fatal("the fixture lost the graph the relabelling test is about")
	}

	remap := map[string]string{}
	mint := func(id string) string {
		if next, ok := remap[id]; ok {
			return next
		}
		next := games.NewID()
		remap[id] = next
		return next
	}
	// A deep copy: rel and m must not share a backing array, or "the ids really
	// did change" would be comparing one manifest with itself.
	rel := cloneManifest(t, m)
	for i := range rel.Sources {
		rel.Sources[i].SourceID = mint(rel.Sources[i].SourceID)
	}
	for i := range rel.Characters {
		rel.Characters[i].ID = mint(rel.Characters[i].ID)
		rel.Characters[i].VerifiedRefs = remapRefs(rel.Characters[i].VerifiedRefs, remap)
	}
	for i := range rel.Scenes {
		rel.Scenes[i].ID = mint(rel.Scenes[i].ID)
	}
	for i := range rel.Regions {
		rel.Regions[i].ID = mint(rel.Regions[i].ID)
		if sceneID := rel.Regions[i].SceneID; sceneID != nil {
			next := mint(*sceneID)
			rel.Regions[i].SceneID = &next
		}
	}
	for i := range rel.Messages {
		rel.Messages[i].ID = mint(rel.Messages[i].ID)
		for _, field := range []*string{rel.Messages[i].Source, rel.Messages[i].SceneID, rel.Messages[i].SpeakerID} {
			if field != nil {
				*field = mint(*field)
			}
		}
		// The attribution evidence names the source row by id; an id left
		// inside the payload would be a reference the relabelling forgot.
		rel.Messages[i].Evidence = remapRefs(rel.Messages[i].Evidence, remap)
	}
	for i := range rel.MessageEdges {
		rel.MessageEdges[i].From = mint(rel.MessageEdges[i].From)
		rel.MessageEdges[i].To = mint(rel.MessageEdges[i].To)
		if src := rel.MessageEdges[i].Source; src != nil {
			next := mint(*src)
			rel.MessageEdges[i].Source = &next
		}
	}
	for i := range rel.SpeakerAssertions {
		rel.SpeakerAssertions[i].ID = mint(rel.SpeakerAssertions[i].ID)
		rel.SpeakerAssertions[i].Message = mint(rel.SpeakerAssertions[i].Message)
		rel.SpeakerAssertions[i].Character = mint(rel.SpeakerAssertions[i].Character)
		// The assertion's evidence names the source row by id too.
		rel.SpeakerAssertions[i].Evidence = remapRefs(rel.SpeakerAssertions[i].Evidence, remap)
	}
	if len(remap) < 8 {
		t.Fatalf("only %d ids were relabelled; the fixture is not exercising references", len(remap))
	}

	// A relabelled manifest is still a well-formed one. Checked before the
	// digest, because "the digest changed" is a confusing way to report "you
	// left a reference pointing at a row you renamed".
	if err := rel.Validate(); err != nil {
		t.Fatalf("the relabelled manifest is not self-consistent: %v", err)
	}
	// And the check catches a relabelling that missed a reference: rename the
	// edges' source back to the old id and the dangling pointer is named.
	dangling := cloneManifest(t, rel)
	for i := range dangling.MessageEdges {
		if dangling.MessageEdges[i].Source != nil {
			old := m.MessageEdges[i].Source
			dangling.MessageEdges[i].Source = old
		}
	}
	if err := dangling.Validate(); !errors.Is(err, ErrDanglingRef) {
		t.Errorf("an edge naming a source the manifest no longer defines: err = %v, want ErrDanglingRef", err)
	}

	if ContentDigest(rel) != m.Digest {
		t.Errorf("digest changed under relabelling: %s vs %s\n%s", ContentDigest(rel), m.Digest, describeCanonicalDiff(t, m, rel))
	}
	// The ids really did change, or the comparison above proved nothing.
	same := 0
	for i := range rel.Messages {
		if rel.Messages[i].ID == m.Messages[i].ID {
			same++
		}
	}
	if same != 0 {
		t.Errorf("%d message ids survived the relabelling", same)
	}
}

// TestDigestIgnoresExportOrder pins the other input that must not matter: the
// order rows came back out of the database. Two revisions holding the same
// content in a different insertion order are the same content.
func TestDigestIgnoresExportOrder(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-order", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	decorate(t, e, p.ID, mustContent(t, e, p.ID))
	m := profileContentFor(t, e, p.ID)

	shuffled := cloneManifest(t, m)
	reverse(&shuffled.Messages)
	reverse(&shuffled.Characters)
	reverse(&shuffled.Scenes)
	reverse(&shuffled.MessageEdges)
	if ContentDigest(shuffled) != m.Digest {
		t.Errorf("reversing the row order changed the digest: %s vs %s", ContentDigest(shuffled), m.Digest)
	}
	// And the emitted document, which is what a human diffs, is stable too.
	a, b := mustManifestBytes(t, m), mustManifestBytes(t, shuffled)
	if !bytes.Equal(a, b) {
		t.Error("the same content exported in a different row order produced a different file")
	}
}

// TestDigestIgnoresPageListingOrderAndLocalProvenance pins the two canonicalization
// rules the hand-written fixture in testdata/ forced into the open, so they are
// stated as rules rather than as a side effect of one file:
//
//   - a message's `pages` array is ordered by its own `index`. The ordinal *is*
//     the order (docs/spec/02 keeps pages as rows carrying one), the store returns
//     them sorted, so the sequence a document happens to list them in carries no
//     content — and a fingerprint that read it would disagree with itself after a
//     re-import of a document written by hand;
//   - a source row's `uri` and `retrieved_at` describe this store's copy of the
//     bytes, not the bytes: a re-import derives the address from the hash it
//     already carries and timestamps when it read them.
//
// Both are inertness checks with a teeth-check beside them: renumbering a page, or
// changing a content hash, must still move the digest.
func TestDigestIgnoresPageListingOrderAndLocalProvenance(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-provenance", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	decorate(t, e, p.ID, mustContent(t, e, p.ID))
	m := profileContentFor(t, e, p.ID)

	// A two-page message to reorder: the CSV fixture's lines are single-page, so
	// one is given a second page rather than the test quietly checking nothing.
	two := -1
	for i := range m.Messages {
		if len(m.Messages[i].Pages) >= 2 {
			two = i
			break
		}
		if m.Messages[i].SpeechText != nil {
			m.Messages[i].Pages = []PageInfo{
				{Index: 1, Text: strPtr("The tide turns at the third bell."), Prefixes: json.RawMessage(`[]`), Controls: json.RawMessage(`[]`), Timing: json.RawMessage(`{}`)},
				{Index: 2, Text: strPtr("Nobody rows against it twice."), Prefixes: json.RawMessage(`[]`), Controls: json.RawMessage(`[]`), Timing: json.RawMessage(`{}`)},
			}
			two = i
			break
		}
	}
	if two < 0 {
		t.Fatal("no message could carry two pages")
	}
	// The manifest changed after Export stamped it, so it is re-stamped rather
	// than compared against a fingerprint it no longer carries.
	m.Digest = ""
	want := Stamp(&m)

	listed := cloneManifest(t, m)
	reverse(&listed.Messages[two].Pages)
	if got := ContentDigest(listed); got != want {
		t.Errorf("listing a message's pages out of ordinal order changed the digest: %s vs %s", got, want)
	}

	relocated := cloneManifest(t, m)
	relocated.Sources[0].URI = "https://elsewhere.invalid/copy-of-the-file.csv"
	relocated.Sources[0].RetrievedAt = "2020-01-01T00:00:00Z"
	if got := ContentDigest(relocated); got != want {
		t.Errorf("where and when this store read the bytes changed the content digest: %s vs %s", got, want)
	}

	// The exported document still states them: they are out of the fingerprint, not
	// out of the record. (WriteManifest stamps the re-located copy, which is the
	// same bytes plus a fingerprint over content they did not change.)
	relocated.Digest = ""
	if raw := mustManifestBytes(t, relocated); !bytes.Contains(raw, []byte("elsewhere.invalid")) || !bytes.Contains(raw, []byte("2020-01-01T00:00:00Z")) {
		t.Error("dropping the volatile columns from the digest also dropped them from the file")
	}

	renumbered := cloneManifest(t, m)
	renumbered.Messages[two].Pages[1].Index = 7
	if ContentDigest(renumbered) == want {
		t.Error("renumbering a page did not change the digest — the ordinals are content, only their listing order is not")
	}
	moved := cloneManifest(t, m)
	moved.Sources[0].SHA256 = sha256Bytes([]byte("a different file entirely"))
	if ContentDigest(moved) == want {
		t.Error("changing a source's content hash did not change the digest")
	}
}

// TestCanonicalProjectionContainsNoRowIDs is the mechanism behind
// TestDigestSurvivesRelabelling, checked directly: if any raw id reached the
// digest input, the projection would change under relabelling and the round-trip
// acceptance criterion would be a coincidence.
func TestCanonicalProjectionContainsNoRowIDs(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-projection", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	decorate(t, e, p.ID, mustContent(t, e, p.ID))
	m := profileContentFor(t, e, p.ID)
	proj := string(Canonical(m))

	for _, id := range append([]string{m.ProfileID}, allRowIDs(m)...) {
		if strings.Contains(proj, id) {
			t.Errorf("the digest projection still contains the row id %s", id)
		}
	}
	for _, want := range []string{"src#", "chr#", "scn#", "reg#", "msg#", "asr#"} {
		if !strings.Contains(proj, want) && sectionHasRows(m, want) {
			t.Errorf("the projection has no %s aliases", want)
		}
	}
	// The lifecycle fields are out, the content is in.
	for _, absent := range []string{"publication", "approved_at", "store_digest", "manifest_digest"} {
		if strings.Contains(proj, `"`+absent+`"`) {
			t.Errorf("the projection carries %q, which a re-import cannot reproduce", absent)
		}
	}
	for _, present := range []string{"normalized_text", "speech_text", "message_edges", "rect", "rights"} {
		if !strings.Contains(proj, present) {
			t.Errorf("the projection dropped %q, which is content", present)
		}
	}
}

func TestCanonicalJSONIsSortedAndWhitespaceFree(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-cjson", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	m := profileContentFor(t, e, p.ID)
	raw := Canonical(m)
	if bytes.ContainsAny(raw, "\n\t") {
		t.Error("canonical bytes contain a newline or tab")
	}
	// Structural whitespace only: game text legitimately contains commas and
	// spaces, and a check that looked at every byte would be a check on the
	// fixture rather than on the encoder.
	if countStructuralWhitespace(raw) != 0 {
		t.Error("canonical bytes separate values with whitespace")
	}
	// A round trip through encoding/json must be byte-identical: the canonical
	// form is a fixed point, not merely a compact one.
	var via map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&via); err != nil {
		t.Fatalf("the projection is not parseable JSON: %v", err)
	}
	again, err := json.Marshal(via)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, raw) {
		t.Error("encoding/json's own key sorting disagrees with the canonical emitter")
	}
	// Text that encoding/json would HTML-escape stays as written, so the same
	// text hashes identically whichever encoder produced the file.
	withAngles := cloneManifest(t, m)
	withAngles.Messages[0].SpeechText = strPtr("a <bell> & a &lt;whistle&gt;")
	stamped := Stamp(&withAngles)
	if stamped == "" {
		t.Fatal("Stamp returned an empty digest")
	}
	body := string(Canonical(withAngles))
	if strings.Contains(body, `\u003c`) {
		t.Error("canonical bytes HTML-escape angle brackets, making the digest encoder-dependent")
	}
}

//
// ── helpers ─────────────────────────────────────────────────────────────────
//

// cloneManifest deep-copies a manifest through its own JSON form, which is the
// same bytes a round trip would carry.
func cloneManifest(t *testing.T, m Manifest) Manifest {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("clone manifest: %v", err)
	}
	var out Manifest
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("clone manifest: %v", err)
	}
	return out
}

// countStructuralWhitespace counts spaces outside JSON strings, which is the
// only whitespace a canonical document may not contain.
func countStructuralWhitespace(raw []byte) int {
	var (
		n        int
		inString bool
		escaped  bool
	)
	for _, c := range raw {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case ' ', '\t', '\n', '\r':
			n++
		}
	}
	return n
}

func decodeDocument(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	return out
}

func cloneDocument(doc map[string]any) map[string]any {
	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	var out map[string]any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		panic(err)
	}
	return out
}

func encodeDocument(t *testing.T, doc map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encode document: %v", err)
	}
	return append(raw, '\n')
}

func reverse[T any](s *[]T) {
	for i, j := 0, len(*s)-1; i < j; i, j = i+1, j-1 {
		(*s)[i], (*s)[j] = (*s)[j], (*s)[i]
	}
}

func allRowIDs(m Manifest) []string {
	var out []string
	for _, s := range m.Sources {
		out = append(out, s.SourceID)
	}
	for _, c := range m.Characters {
		out = append(out, c.ID)
	}
	for _, s := range m.Scenes {
		out = append(out, s.ID)
	}
	for _, r := range m.Regions {
		out = append(out, r.ID)
	}
	for _, msg := range m.Messages {
		out = append(out, msg.ID)
	}
	for _, a := range m.SpeakerAssertions {
		out = append(out, a.ID)
	}
	return out
}

func sectionHasRows(m Manifest, aliasPrefix string) bool {
	switch aliasPrefix {
	case "src#":
		return len(m.Sources) > 0
	case "chr#":
		return len(m.Characters) > 0
	case "scn#":
		return len(m.Scenes) > 0
	case "reg#":
		return len(m.Regions) > 0
	case "msg#":
		return len(m.Messages) > 0
	case "asr#":
		return len(m.SpeakerAssertions) > 0
	}
	return false
}
