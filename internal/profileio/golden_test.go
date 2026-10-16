package profileio

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestHandWrittenManifestImports covers the reader against a document that this
// package did not write.
//
// Every other test in this file round-trips a manifest through Export, which
// proves the writer and the reader agree with each other — a self-consistency
// proof, and a weak one on its own, because both sides could be wrong in the same
// way. The fixture here was authored by hand instead: keys in a different order,
// optional provenance fields absent, ids out of order relative to their
// references, a page ordinal out of sequence, one message redacted with no
// reason field (which must be refused, not guessed at), and a text that arrives
// as an empty string rather than null, which means something different
// ("deliberately not spoken" rather than "withheld", docs/spec/02).
//
// Regenerate it with `GN_UPDATE_FIXTURES=1 go test -run TestHandWrittenManifestImports`, and
// expect to do that only when the wire shape itself changes — which is the point
// of keeping the file in the tree rather than building it in the test.
//
// The `manifest_digest` line is the documented algorithm's output for *this*
// document, not a value copied out of an export: ContentDigest over the manifest
// as DecodeManifest reads it (unmarshal, normalized(), row ids aliased by content,
// `profile_id` and `publication` dropped, pages ordered by ordinal, a source's
// `uri`/`retrieved_at` dropped — see canonical.go). It was produced by decoding the
// fixture and recomputing, the same arithmetic Verify performs, whose failure
// message prints the recomputed digest. When the canonicalization rules change, the
// fixture's digest is regenerated that way; Verify is never loosened to match a
// stale file. Note the two properties the regeneration path cannot reproduce —
// hand-trimmed optional fields and the document's own key order — so a regenerated
// file is content-equivalent (same digest) but not byte-identical to the one in the
// tree.
func TestHandWrittenManifestImports(t *testing.T) {
	path := filepath.Join("testdata", "handwritten.gnprofile.json")
	if os.Getenv("GN_UPDATE_FIXTURES") != "" {
		writeHandWrittenFixture(t, path)
		t.Skip("fixture rewritten; run the test again to check it")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// Schema first: a document may not be "close enough" to the shape.
	if err := ValidateDocument(raw); err != nil {
		t.Fatalf("the hand-written fixture does not satisfy the schema: %v", err)
	}
	m, err := DecodeManifest(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	if m.SchemaVersion != SchemaVersion {
		t.Errorf("schema_version = %q", m.SchemaVersion)
	}
	if len(m.Messages) != 4 || len(m.Characters) != 1 || len(m.Scenes) != 1 || len(m.MessageEdges) != 2 {
		t.Errorf("counts = %d messages, %d characters, %d scenes, %d edges", len(m.Messages), len(m.Characters), len(m.Scenes), len(m.MessageEdges))
	}

	e := newIn(t)
	report, err := ImportManifest(e.ctx, e.deps, bytes.NewReader(raw), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	if report.MessagesWritten != 4 || report.MessagesWithheld != 0 {
		t.Errorf("imported %d messages and withheld %d, want 4 and 0", report.MessagesWritten, report.MessagesWithheld)
	}
	// The fixture names no uri, mime type, trust or retrieval time: provenance
	// that arrives incomplete is filled with what can be *verified* (the content
	// address) rather than a plausible-looking address nobody gave it.
	sources, err := e.store.ListSources(e.ctx, report.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	src := sources[0]
	if src.URI != "asset://"+src.ContentSHA256 {
		t.Errorf("imported source uri = %q, want the content address the manifest did imply", src.URI)
	}
	if src.RetrievedAt == "" {
		t.Error("a source row without a retrieval timestamp is not allowed by the schema of the table")
	}
	if src.SourceType != games.SourceUserFile {
		t.Errorf("source_type = %q, want the default a bare source entry means", src.SourceType)
	}
	// The digest the document carried is what it re-imports as, so an outside
	// author's manifest can be checked against an outside record of it.
	if report.Digest != m.Digest {
		t.Errorf("re-import reported digest %s, fixture carries %s", report.Digest, m.Digest)
	}

	content := mustContent(t, e, report.ProfileID)
	if len(content.Pages) != 3 {
		t.Errorf("pages = %d, want 3 (the fixture carries three page rows out of order)", len(content.Pages))
	}
	for _, p := range content.Pages {
		if p.Ordinal < 1 {
			t.Errorf("page %s ordinal = %d", p.MessageID, p.Ordinal)
		}
	}
	if len(content.Edges) != 2 {
		t.Errorf("edges = %d, want 2", len(content.Edges))
	}
	byID := map[string]games.Message{}
	for _, msg := range content.Messages {
		byID[msg.ID] = msg
	}
	for _, msg := range content.Messages {
		switch msg.SourceMessageID {
		case "hand-silent":
			// "" is a rendering decision; null is a redaction. Importing must not
			// collapse the two.
			if msg.SpeechText != "" {
				t.Errorf("the deliberately-unspoken line came back as %q", msg.SpeechText)
			}
			if msg.OriginalText == "" {
				t.Error("an unspoken line still has on-screen text")
			}
		case "hand-speaker":
			if msg.SpeakerID == "" || msg.AttributionStatus != games.AttributionConfirmed {
				t.Errorf("the attributed line lost its speaker or its status: %q/%q", msg.SpeakerID, msg.AttributionStatus)
			}
		}
	}
	// And a re-export of what was imported carries the same content fingerprint,
	// which is the same claim the round-trip tests make, now about a document
	// this code did not write.
	again := profileContentFor(t, e, report.ProfileID)
	if again.Digest != m.Digest {
		t.Errorf("re-export of the hand-written manifest digests %s, want %s\n%s", again.Digest, m.Digest, describeCanonicalDiff(t, m, again))
	}

	// A redacted message with no reason is refused rather than imported as an
	// empty line: docs/spec/14's redaction has to be explainable.
	noReason := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&noReason); err != nil {
		t.Fatal(err)
	}
	messages := noReason["messages"].([]any)
	msg := messages[0].(map[string]any)
	msg["original_text"], msg["normalized_text"], msg["speech_text"] = nil, nil, nil
	msg["redacted_reason"] = nil
	for _, page := range msg["pages"].([]any) {
		page.(map[string]any)["text"] = nil
	}
	broken := encodeDocument(t, noReason)
	// The schema allows redacted_reason to be null; the *meaning* is checked in
	// Go, because "text absent without a stated rule" is a semantic question.
	if err := ValidateDocument(broken); err != nil {
		t.Fatalf("the mutation should stay schema-valid so the semantic check is what bites: %v", err)
	}
	if _, err := ImportManifest(e.ctx, e.deps, bytes.NewReader(broken), ImportManifestOptions{}); err == nil || !strings.Contains(err.Error(), "redacted_reason") {
		t.Errorf("text withheld with no stated reason: err = %v, want it named redacted_reason", err)
	}
}

// writeHandWrittenFixture builds the document described above. The digest is
// computed by the same Stamp the importer verifies against, which is acceptable
// here because the fixture's job is to be an *input*: what it pins is the shape,
// the key spelling and the defaults, not the arithmetic.
func writeHandWrittenFixture(t *testing.T, path string) {
	t.Helper()
	m := Manifest{
		SchemaVersion: SchemaVersion,
		ProfileID:     games.NewID(),
		Edition: EditionInfo{
			Slug: "handwritten-quay", Title: "Handwritten Quay", Platform: "gamecube-like",
			Region: "US", Language: "en", Revision: "demo",
		},
		Publication: PublicationInfo{Status: games.StatusDraft, Revision: 1},
		Sources: []SourceInfo{{
			SourceID: games.NewID(), Type: games.SourceUserFile,
			SHA256: sha256Bytes([]byte("handwritten fixture payload")),
			Rights: games.RightsUserSupplied, Parser: "generic@1.0.0",
		}},
		Characters: []CharacterInfo{{
			ID: games.NewID(), Name: "The Lighthouse Keeper", Status: games.CharacterConfirmed,
			Aliases: json.RawMessage(`["Keeper"]`), Role: "keeper",
		}},
		Scenes: []SceneInfo{{ID: games.NewID(), Name: "The Quay", Aliases: json.RawMessage(`[]`)}},
		Regions: []RegionInfo{{
			ID: games.NewID(), Label: "textbox", Kind: games.RegionTextbox, Detect: games.DetectManual,
			Rect: Rect{X: 0.06, Y: 0.7, W: 0.88, H: 0.25},
			OCR:  json.RawMessage(`{"engine":"tesseract","psm":6,"language":"eng"}`),
		}},
	}
	// Four messages, built out of order on purpose: the reader must not depend on
	// references already having been seen.
	silent := MessageInfo{
		ID: games.NewID(), Kind: games.TextSystem, Language: "en",
		SourceMessageID: strPtr("hand-silent"),
		OriginalText:    strPtr("The lamp turns; nobody rows tonight."),
		NormalizedText:  strPtr("the lamp turns; nobody rows tonight"),
		SpeechText:      strPtr(""),
		Attribution:     AttributionInfo{Status: games.AttributionUnknown},
		Pages: []PageInfo{
			{Index: 2, Text: strPtr("Nobody rows tonight.")},
			{Index: 1, Text: strPtr("The lamp turns.")},
		},
	}
	spoken := MessageInfo{
		ID: games.NewID(), Kind: games.TextDialogue, Language: "en",
		SourceMessageID: strPtr("hand-speaker"),
		OriginalText:    strPtr("Keep the log dry and the wick trimmed."),
		NormalizedText:  strPtr("keep the log dry and the wick trimmed"),
		SpeechText:      strPtr("Keep the log dry and the wick trimmed."),
		Attribution:     AttributionInfo{Status: games.AttributionConfirmed, Confidence: 1},
		Pages:           []PageInfo{{Index: 1, Text: strPtr("Keep the log dry and the wick trimmed.")}},
	}
	narration := MessageInfo{
		ID: games.NewID(), Kind: games.TextTutorial, Language: "en",
		SourceMessageID: strPtr("hand-narration"),
		OriginalText:    strPtr("Press [BTN_A] to leave the quay."),
		NormalizedText:  strPtr("press a to leave the quay"),
		SpeechText:      strPtr("Press A to leave the quay."),
		Attribution:     AttributionInfo{Status: games.AttributionUnknown},
	}
	sign := MessageInfo{
		ID: games.NewID(), Kind: games.TextSign, Language: "en",
		SourceMessageID: strPtr("hand-sign"),
		OriginalText:    strPtr("QUAY CLOSED AFTER THE THIRD BELL"),
		NormalizedText:  strPtr("quay closed after the third bell"),
		SpeechText:      strPtr("Quay closed after the third bell."),
		Attribution:     AttributionInfo{Status: games.AttributionUnknown},
	}
	// The edges and the speaker refer to rows by their ids, which were minted
	// above; the message list is ordered so a reference follows its target, but
	// the characters section precedes the messages that name them, so at least
	// one kind of reference is deliberately backwards.
	messages := []MessageInfo{narration, silent, spoken, sign}
	for i := range messages {
		messages[i].Source = strPtr(m.Sources[0].SourceID)
		messages[i].SceneID = strPtr(m.Scenes[0].ID)
	}
	messages[2].SpeakerID = strPtr(m.Characters[0].ID)
	m.Messages = messages
	m.MessageEdges = []EdgeInfo{
		{From: silent.ID, To: spoken.ID, Type: games.EdgeNext, Confidence: 1},
		{From: spoken.ID, To: narration.ID, Type: games.EdgeBranch, Condition: `flags.lamp_lit == true`, Confidence: 0.8},
	}
	// Stamp the normalized document. A hand-built manifest carries nil JSON
	// columns where an exported one carries `[]`, and a reader normalizes them on
	// the way in — stamping before normalization would sign bytes nobody reads.
	m = m.normalized()
	Stamp(&m)

	// Written with keys in the document's own order and no trailing whitespace,
	// which is what the exporter produces; the fixture's difference from an
	// export is its content and omissions, not its formatting.
	raw, err := Encode(m)
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, raw, "", "  "); err != nil {
		t.Fatalf("indent fixture: %v", err)
	}
	pretty.WriteByte('\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, pretty.Bytes(), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	_ = store.MaxListLimit
}
