package profileio

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestRightsRedactionInDefaultExport is docs/spec/14's "profile export honors
// content rights flags" and docs/spec/16's default-distribution rule, run on one
// revision that mixes a user-supplied script with material whose rights position
// was never resolved.
func TestRightsRedactionInDefaultExport(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-rights", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	// A: bytes the user handed over.
	csvReport, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatal(err)
	}

	// B: content whose provenance carries an unresolved rights question. It has
	// to be registered outside ImportFile on purpose — an *upload* cannot be
	// 'unknown' (docs/spec/01 invariant 6 refuses to store the question), while
	// scraped or inherited provenance can and must be redacted on the way out.
	unsettled := games.Source{
		ID: games.NewID(), ProfileID: p.ID, SourceType: games.SourceWebPage,
		URI: "https://example.invalid/synthetic-script", RetrievedAt: nowStamp(),
		ContentSHA256: sha256Bytes([]byte("synthetic scraped summary")),
		MimeType:      "text/plain", Rights: games.RightsUnknown, Trust: 0.3,
		Parser: "profileio.test@1.0.0", CoverageNotes: "invented for this test",
	}
	if err := unsettled.Validate(); err != nil {
		t.Fatalf("unsettled source fixture invalid: %v", err)
	}
	handWritten := games.Message{
		ID: games.NewID(), ProfileID: p.ID, SourceID: unsettled.ID,
		SourceMessageID: "web-1", OriginalText: "A line nobody has cleared for sharing.",
		NormalizedText: "a line nobody has cleared for sharing",
		SpeechText:     "A line nobody has cleared for sharing.", Language: "en",
		TextKind: games.TextDialogue, AttributionStatus: games.AttributionUnknown,
		Revision: 1, CreatedAt: nowStamp(), UpdatedAt: nowStamp(),
	}
	// C: a line typed by hand with no provenance at all. "Never asked" is as
	// unresolved as "asked and refused", so it is withheld too.
	orphan := games.Message{
		ID: games.NewID(), ProfileID: p.ID, SourceMessageID: "",
		OriginalText:   "Something the curator typed in without a source.",
		NormalizedText: "something the curator typed in without a source",
		SpeechText:     "Something the curator typed in without a source.", Language: "en",
		TextKind: games.TextOther, AttributionStatus: games.AttributionUnknown,
		Revision: 1, CreatedAt: nowStamp(), UpdatedAt: nowStamp(),
	}
	if err := e.store.PutSources(e.ctx, p.ID, []games.Source{unsettled}); err != nil {
		t.Fatal(err)
	}
	if err := e.store.PutMessages(e.ctx, p.ID, []games.Message{handWritten, orphan}, nil); err != nil {
		t.Fatal(err)
	}

	m := profileContentFor(t, e, p.ID)

	// The cleared source keeps its text.
	cleared, ok := messageByKey(m, csvReport.SHA256, t)
	if !ok {
		t.Fatal("the user-supplied messages disappeared from the export")
	}
	for _, msg := range cleared {
		if msg.OriginalText == nil || msg.RedactedReason != nil {
			t.Errorf("user_supplied message %s was redacted: %v", msg.ID, msg.RedactedReason)
		}
	}

	// The unsettled source and the unprovenanced line do not.
	var redacted int
	for _, msg := range m.Messages {
		if msg.RedactedReason == nil {
			continue
		}
		redacted++
		if msg.OriginalText != nil || msg.NormalizedText != nil || msg.SpeechText != nil {
			t.Errorf("message %s claims to be redacted but still carries text", msg.ID)
		}
		for _, page := range msg.Pages {
			if page.Text != nil {
				t.Errorf("message %s kept page text under a redaction", msg.ID)
			}
		}
		// Structure survives: the profile still says the line exists, of what
		// class it is and where it sits.
		if msg.Kind == "" {
			t.Errorf("redacted message %s lost its content class too", msg.ID)
		}
	}
	if redacted != 2 {
		t.Errorf("redacted %d messages, want the two whose rights are unresolved", redacted)
	}
	if got := messageByID(m, handWritten.ID).RedactedReason; got == nil || *got != rightsWithheld {
		t.Errorf("unknown-rights message reason = %v, want %q", got, rightsWithheld)
	}
	if got := messageByID(m, orphan.ID).RedactedReason; got == nil || *got != noSourceReason {
		t.Errorf("unprovenanced message reason = %v, want %q", got, noSourceReason)
	}

	// No copyrighted text anywhere in the default file.
	file := mustManifestBytes(t, m)
	// The two withheld lines, and nothing derived from them. The user-supplied
	// script's text stays in: its rights are settled, which is the distinction
	// docs/spec/14 draws, not "all game text is forbidden".
	for _, secret := range []string{handWritten.OriginalText, orphan.OriginalText} {
		if bytes.Contains(file, []byte(secret)) {
			t.Errorf("default export leaked %q", secret)
		}
	}
	// What it does carry is the pointer set docs/spec/14 asks for.
	if !bytes.Contains(file, []byte(unsettled.URI)) || !bytes.Contains(file, []byte(unsettled.ContentSHA256)) {
		t.Error("default export lost the provenance it is supposed to carry")
	}
	if err := ValidateDocument(file); err != nil {
		t.Errorf("a redacted manifest must still be a valid manifest: %v", err)
	}

	// Redaction is content, so it is a different digest: an export that withheld
	// text and still hashed like the full one would make two different documents
	// claim to be the same profile.
	full, err := Export(e.ctx, e.store, p.ID, ExportOptions{IncludeSourceBytes: true, AcknowledgeCopyright: true})
	if err != nil {
		t.Fatal(err)
	}
	if full.Digest == m.Digest {
		t.Error("the acknowledged bundle and the redacted export have the same digest; they are not the same document")
	}
	if messageByID(full, handWritten.ID).RedactedReason != nil {
		t.Error("the acknowledged bundle withheld text the user asked to carry")
	}

	// And the gate agrees with the exporter: a revision whose content rests on
	// unsettled rights cannot be published, and says why in the vocabulary
	// docs/spec/08 names for it.
	if err := e.store.SetProfileStatus(e.ctx, p.ID, games.StatusReview, versionOf(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	_, err = e.store.Publish(e.ctx, p.ID, versionOf(t, e, p.ID), "curator@local")
	var pubErr *store.PublishError
	if !errors.As(err, &pubErr) || len(pubErr.Problems) == 0 {
		t.Fatalf("publish of a revision with unknown-rights content: err = %v, want the rights gate to refuse", err)
	}
	if !strings.Contains(strings.Join(pubErr.Problems, "\n"), "unpublishable rights_status") {
		t.Errorf("publication refusal did not name rights: %v", pubErr.Problems)
	}
}

// TestPackRequiresAcknowledgement is the other half of the same sentence: bytes
// leave the appliance only when somebody said so.
func TestPackRequiresAcknowledgement(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-packack", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	report, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatal(err)
	}

	// The default export carries no bytes at all.
	var def bytes.Buffer
	if err := WriteManifest(&def, profileContentFor(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(def.Bytes(), []byte("blobs/")) {
		t.Error("a .gnprofile.json names blobs, which it cannot contain")
	}

	// Asking for bytes without acknowledging copyright fails, in both entry
	// points, and writes nothing.
	if _, err := Export(e.ctx, e.store, p.ID, ExportOptions{IncludeSourceBytes: true}); !errors.Is(err, ErrCopyrightAcknowledgement) {
		t.Errorf("Export without acknowledgement: err = %v", err)
	}
	var pack bytes.Buffer
	if _, err := ExportBundle(e.ctx, e.deps, p.ID, ExportOptions{IncludeSourceBytes: true}, &pack); !errors.Is(err, ErrCopyrightAcknowledgement) {
		t.Errorf("ExportBundle without acknowledgement: err = %v", err)
	}
	if pack.Len() != 0 {
		t.Errorf("a refused bundle still wrote %d bytes", pack.Len())
	}

	// With the acknowledgement: a tar holding the manifest and exactly the blobs
	// the manifest references.
	pack.Reset()
	manifest, err := ExportBundle(e.ctx, e.deps, p.ID, ExportOptions{IncludeSourceBytes: true, AcknowledgeCopyright: true}, &pack)
	if err != nil {
		t.Fatalf("ExportBundle: %v", err)
	}
	want := SourceBlobRefs(manifest)
	if len(want) != 1 || want[0] != report.SHA256 {
		t.Fatalf("manifest references blobs %v, want the imported file %s", want, report.SHA256)
	}
	got := tarEntries(t, pack.Bytes())
	if len(got.entries) != len(want)+1 {
		t.Errorf("pack holds %d entries: %v", len(got.entries), got.entries)
	}
	for _, ref := range want {
		if !contains(got.entries, blobPath(ref)) {
			t.Errorf("pack is missing %s", blobPath(ref))
		}
	}
	for _, name := range got.entries {
		if name != PackManifestName && !strings.HasPrefix(name, "blobs/") {
			t.Errorf("pack carries an unexpected entry %q", name)
		}
		if strings.HasPrefix(name, "blobs/") && !contains(want, strings.TrimPrefix(name, "blobs/")) {
			t.Errorf("pack carries %s, which no source of this profile references", name)
		}
	}
	// The blob bytes are the source bytes, addressed by their own hash.
	body, ok := got.bodies[blobPath(report.SHA256)]
	if !ok {
		t.Fatal("the referenced blob is not in the pack")
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != report.SHA256 {
		t.Error("the pack's blob does not hash to the address its name claims")
	}
	if !bytes.Contains(body, []byte("Saltmarsh")) {
		t.Error("the pack's blob is not the script that was imported")
	}
	// The manifest entry is the same document the plain export writes, so a
	// recipient can read either form with one parser.
	if !contains(got.entries, PackManifestName) {
		t.Fatal("the pack has no manifest entry")
	}
	packed, err := DecodeManifestBytes(got.bodies[PackManifestName])
	if err != nil {
		t.Fatalf("the pack's manifest is not readable: %v", err)
	}
	if packed.Digest != manifest.Digest {
		t.Errorf("pack manifest digest %s disagrees with the export's %s", packed.Digest, manifest.Digest)
	}
}

func TestWriteManifestIsCanonicalAndSelfVerifying(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-canonical", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	m := profileContentFor(t, e, p.ID)

	first := mustManifestBytes(t, m)
	second := mustManifestBytes(t, m)
	if !bytes.Equal(first, second) {
		t.Error("two writes of one manifest produced different bytes")
	}
	line := bytes.TrimSuffix(first, []byte("\n"))
	if bytes.Contains(line, []byte("\n")) {
		t.Error("the canonical form contains a newline: it must be one line")
	}
	if bytes.Contains(line, []byte("  ")) || bytes.Contains(line, []byte(`": `)) {
		t.Error("the canonical form carries insignificant whitespace")
	}
	// Sorted keys: edition comes before messages, which comes before sources.
	if !(bytes.Index(line, []byte(`"edition"`)) < bytes.Index(line, []byte(`"messages"`)) &&
		bytes.Index(line, []byte(`"messages"`)) < bytes.Index(line, []byte(`"sources"`))) {
		t.Error("keys are not in byte order")
	}
	// Re-reading what was written must reproduce the same document, ids included:
	// the file is a faithful record of one revision, not only of its content.
	again, err := DecodeManifest(bytes.NewReader(first))
	if err != nil {
		t.Fatalf("the exported file does not read back: %v", err)
	}
	if again.ProfileID != m.ProfileID || again.Digest != m.Digest || len(again.Messages) != len(m.Messages) {
		t.Error("reading the exported file lost something")
	}
	// Sections are emitted in canonical (content) order, not database order, so
	// the comparison is a set of ids plus a byte-identical re-encode.
	beforeIDs := map[string]bool{}
	for _, msg := range m.Messages {
		beforeIDs[msg.ID] = true
	}
	for _, msg := range again.Messages {
		if !beforeIDs[msg.ID] {
			t.Fatalf("re-decoded message %s was not in the export", msg.ID)
		}
		delete(beforeIDs, msg.ID)
	}
	if len(beforeIDs) != 0 {
		t.Fatalf("re-decoding lost %d messages", len(beforeIDs))
	}
	if !bytes.Equal(mustManifestBytes(t, again), first) {
		t.Error("re-encoding the decoded file did not reproduce its bytes: the canonical form is not canonical")
	}
	// An edited message does not verify. The lifecycle section is deliberately
	// *not* covered by the digest — see TestDigestTamperRejected, which pins both
	// halves of that choice.
	edited := m
	edited.Messages[0].SpeechText = strPtr(textOrEmpty(edited.Messages[0].SpeechText) + " (edited after export)")
	editedBytes, err := Encode(edited)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeManifestBytes(editedBytes); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("edited message: err = %v, want ErrDigestMismatch", err)
	}
	// A manifest handed to WriteManifest with a digest that is not its own is
	// refused rather than silently restamped.
	bad := m
	bad.Digest = strings.Repeat("0", 64)
	if err := WriteManifest(io.Discard, bad); !errors.Is(err, ErrDigestMismatch) {
		t.Errorf("WriteManifest of a mistyped digest: err = %v, want ErrDigestMismatch", err)
	}
	// And an unstamped one is stamped, not written unsigned.
	unsigned := m
	unsigned.Digest = ""
	out := mustManifestBytes(t, unsigned)
	if bytes.Contains(out, []byte(`"manifest_digest":""`)) {
		t.Error("an unstamped manifest was written out unsigned")
	}
}

//
// ── small test helpers ──────────────────────────────────────────────────────
//

type tarContents struct {
	entries []string
	bodies  map[string][]byte
}

func tarEntries(t *testing.T, blob []byte) tarContents {
	t.Helper()
	out := tarContents{bodies: map[string][]byte{}}
	tr := tar.NewReader(bytes.NewReader(blob))
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("read tar: %v", err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("read tar body %s: %v", hdr.Name, err)
		}
		out.entries = append(out.entries, hdr.Name)
		out.bodies[hdr.Name] = body
	}
}

func contains(list []string, want string) bool {
	for _, item := range list {
		if item == want {
			return true
		}
	}
	return false
}

// messageByKey collects the exported messages whose source is the one with this
// content hash, keyed for spot checks.
func messageByKey(m Manifest, sha string, t *testing.T) ([]MessageInfo, bool) {
	t.Helper()
	sources := map[string]bool{}
	for _, s := range m.Sources {
		if s.SHA256 == sha {
			sources[s.SourceID] = true
		}
	}
	var out []MessageInfo
	for _, msg := range m.Messages {
		if msg.Source != nil && sources[*msg.Source] {
			out = append(out, msg)
		}
	}
	return out, len(out) > 0
}

func messageByID(m Manifest, id string) MessageInfo {
	for _, msg := range m.Messages {
		if msg.ID == id {
			return msg
		}
	}
	return MessageInfo{}
}
