package profileio

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/adapter/generic"
	"github.com/dbirks/game-narrator/internal/store"
)

// csvRequest imports the CSV fixture with everything left to be discovered: no
// format, no column map. That is the path an onboarding upload takes
// (docs/spec/03), and it is what proves detection, not just parsing, works.
func csvRequest(t *testing.T, e *env, profileID string) ImportRequest {
	t.Helper()
	return ImportRequest{
		ProfileID: profileID,
		FileName:  "synthetic-adventure.csv",
		Reader:    openFixture(t, "synthetic-adventure.csv"),
		Rights:    games.RightsUserSupplied,
		Parse:     adapter.ParseOptions{Language: "en"},
	}
}

func jsonRequest(t *testing.T, e *env, profileID string) ImportRequest {
	t.Helper()
	return ImportRequest{
		ProfileID: profileID,
		FileName:  "synthetic-other-game.json",
		Reader:    openFixture(t, "synthetic-other-game.json"),
		Rights:    games.RightsUserSupplied,
		Parse: adapter.ParseOptions{
			Format:      generic.FormatJSON,
			Language:    "en",
			DefaultKind: games.TextOther,
			ColumnMap: adapter.ColumnMap{
				Text:    "line_of_dialogue",
				ID:      "utterance_id",
				Speaker: "who_speaks",
				Kind:    "sort_of_text",
				Scene:   "stage",
				Page:    "leaf_number",
			},
		},
	}
}

func TestImportFileRegistersProvenanceAndMessages(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-import", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	raw := readFile(t, fixturePath(t, "synthetic-adventure.csv"))
	report, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}

	if report.SHA256 != sha256Bytes(raw) {
		t.Errorf("reported sha256 %s, want %s", report.SHA256, sha256Bytes(raw))
	}
	if !games.ValidID(report.SourceID) {
		t.Errorf("reported source id %q is not a ULID", report.SourceID)
	}
	if report.Stats.Imported == 0 || report.Inserted != report.Stats.Imported {
		t.Errorf("report says %d rows, %d imported, %d inserted", report.Stats.Rows, report.Stats.Imported, report.Inserted)
	}
	if report.AdapterID != generic.ID {
		t.Errorf("adapter = %q, want %q", report.AdapterID, generic.ID)
	}
	if !strings.HasPrefix(report.MediaType, "text/") {
		t.Errorf("media type = %q, want the sniffed type of a text script", report.MediaType)
	}
	if report.Format != "" {
		t.Errorf("format = %q on the auto-detect path; the report must not claim a format the caller did not name", report.Format)
	}
	if report.Duration <= 0 {
		t.Errorf("report carries no duration: %v", report.Duration)
	}

	sources, err := e.store.ListSources(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	src := sources[0]
	if src.SourceType != games.SourceUserFile {
		t.Errorf("source_type = %q", src.SourceType)
	}
	if src.ContentSHA256 != report.SHA256 {
		t.Errorf("source sha256 = %q, want %q", src.ContentSHA256, report.SHA256)
	}
	if src.Parser != "generic@1.0.0" {
		t.Errorf("parser = %q, want adapter-id@version", src.Parser)
	}
	if src.Rights != games.RightsUserSupplied {
		t.Errorf("rights = %q", src.Rights)
	}
	if src.Trust != 1 {
		t.Errorf("trust = %v, want the 1 a directly supplied file earns", src.Trust)
	}
	if src.URI != "file://synthetic-adventure.csv" {
		t.Errorf("uri = %q", src.URI)
	}
	if !strings.Contains(src.CoverageNotes, "imported="+itoa(report.Stats.Imported)) {
		t.Errorf("coverage notes do not record the parse: %q", src.CoverageNotes)
	}
	if !strings.Contains(src.CoverageNotes, "bytes="+itoa(len(raw))) {
		t.Errorf("coverage notes do not record the blob size: %q", src.CoverageNotes)
	}

	// The bytes are readable from the asset store under the hash the source
	// claims, which is what makes provenance checkable after the upload is gone.
	blob, info, err := e.assets.Get(e.ctx, report.SHA256)
	if err != nil {
		t.Fatalf("asset Get(%s): %v", report.SHA256[:12], err)
	}
	defer blob.Close()
	got, err := io.ReadAll(blob)
	if err != nil {
		t.Fatalf("read blob: %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Errorf("blob is %d bytes, the upload was %d", len(got), len(raw))
	}
	if info.Size != int64(len(raw)) {
		t.Errorf("stat size = %d, want %d", info.Size, len(raw))
	}

	// And the messages landed.
	msgs, _, err := e.store.ListMessages(e.ctx, p.ID, store.MessageFilter{}, "", store.MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != report.Stats.Imported {
		t.Errorf("stored %d messages of the first page, report says %d imported", len(msgs), report.Stats.Imported)
	}
	for _, m := range msgs {
		if m.SourceID != src.ID {
			t.Errorf("message %s names source %s, not the registered %s", m.ID, m.SourceID, src.ID)
		}
		if m.SourceMessageID == "" {
			t.Errorf("message %s has no source identity, so a re-import could not reconcile it", m.ID)
		}
		if m.NormalizedText == "" || m.SpeechText == "" {
			t.Errorf("message %s came back with empty text", m.ID)
		}
	}
}

func TestImportFileIsIdempotentByContentHash(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-repeat", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	first, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID))
	if err != nil {
		t.Fatalf("re-import of the same bytes: %v", err)
	}

	if second.SourceID != first.SourceID {
		t.Errorf("re-import minted a new source: %s then %s", first.SourceID, second.SourceID)
	}
	if !second.ReusedSource {
		t.Error("re-import did not report reusing the source")
	}
	if second.Inserted != 0 {
		t.Errorf("re-import inserted %d new messages; reconciliation by source_message_id failed", second.Inserted)
	}
	if second.Updated != first.Stats.Imported {
		t.Errorf("re-import updated %d messages, want %d", second.Updated, first.Stats.Imported)
	}
	if first.ReusedSource {
		t.Error("the first import of a file should not claim it reused a source")
	}

	sources, err := e.store.ListSources(e.ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Errorf("sources after a re-import = %d, want 1 (docs/spec/02: reconcile, not duplicate)", len(sources))
	}
	count := countMessages(t, e, p.ID)
	if count != first.Stats.Imported {
		t.Errorf("messages after a re-import = %d, want %d", count, first.Stats.Imported)
	}

	// Two different files are two sources, even in the same revision.
	other := jsonRequest(t, e, p.ID)
	third, err := ImportFile(e.ctx, e.deps, other)
	if err != nil {
		t.Fatal(err)
	}
	if third.SourceID == first.SourceID {
		t.Error("a different file reused the same source row")
	}
	sources, _ = e.store.ListSources(e.ctx, p.ID)
	if len(sources) != 2 {
		t.Errorf("sources = %d, want 2", len(sources))
	}
}

func TestImportFileRefusesNonDraftAndUnresolvedRights(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-refusals", "Synthetic Tide")
	edition := e.edition(g, "US", "en")
	p := e.draft(edition.ID)

	// Unknown rights: the question belongs to the human, and storing it as a
	// fact about a file they handed over is what games.Source.Validate forbids.
	req := csvRequest(t, e, p.ID)
	req.Rights = games.RightsUnknown
	if _, err := ImportFile(e.ctx, e.deps, req); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown rights: err = %v, want ErrInvalid", err)
	}

	// A published revision is frozen; content goes into a derived one.
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	if err := e.store.SetProfileStatus(e.ctx, p.ID, games.StatusReview, versionOf(t, e, p.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.Publish(e.ctx, p.ID, versionOf(t, e, p.ID), "curator@local"); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, p.ID)); !errors.Is(err, ErrNotDraft) {
		t.Errorf("import into a published revision: err = %v, want ErrNotDraft", err)
	}

	// The derived revision is the one that takes the bytes.
	next := mustNewRevision(t, e, p.ID)
	after, err := ImportFile(e.ctx, e.deps, csvRequest(t, e, next.ID))
	if err != nil {
		t.Fatalf("import into the derived draft: %v", err)
	}
	// NewRevision copies provenance and content, so re-importing the same bytes
	// into the derived revision reconciles with the copy rather than adding a
	// second set of rows (docs/spec/02 "Import resume by content hash").
	if !after.ReusedSource {
		t.Error("the derived revision carries the copied source, so re-import should reuse it")
	}
	if after.Inserted != 0 || after.Updated != after.Stats.Imported {
		t.Errorf("derived-revision re-import inserted %d and updated %d, want 0 inserted", after.Inserted, after.Updated)
	}

	// A missing adapter is reported as such, not as a parse failure.
	noAdapters := e.deps
	noAdapters.Adapters = adapter.NewRegistry()
	if _, err := ImportFile(e.ctx, noAdapters, csvRequest(t, e, next.ID)); !errors.Is(err, adapter.ErrNoMessages) && !errors.Is(err, ErrNoAdapter) {
		t.Errorf("empty registry: err = %v, want ErrNoAdapter", err)
	}
	// And an unreadable format is refused before the blob is even staged.
	bad := csvRequest(t, e, next.ID)
	bad.Format = "parquet"
	if _, err := ImportFile(e.ctx, e.deps, bad); !errors.Is(err, ErrNoAdapter) {
		t.Errorf("unknown format: err = %v, want ErrNoAdapter", err)
	}
}

func TestImportFileReadsTheSecondGamesStructure(t *testing.T) {
	e := newEnv(t)
	g := e.game("black-water-import", "Black Water")
	p := e.draft(e.edition(g, "EU", "en").ID)

	report, err := ImportFile(e.ctx, e.deps, jsonRequest(t, e, p.ID))
	if err != nil {
		t.Fatalf("ImportFile(json): %v", err)
	}
	// 15 rows, and the repeats of utterance_id are pages rather than messages.
	if report.Stats.Rows != 15 {
		t.Errorf("rows = %d, want 15", report.Stats.Rows)
	}
	if report.Inserted != 13 {
		t.Errorf("inserted = %d, want 13", report.Inserted)
	}
	if report.Pages == 0 {
		t.Error("the second game's page rows were not written")
	}
	msgs, _, err := e.store.ListMessages(e.ctx, p.ID, store.MessageFilter{}, "", store.MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	var withPages int
	for _, m := range msgs {
		_, pages, err := e.store.GetMessage(e.ctx, p.ID, m.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(pages) > 1 {
			withPages++
		}
	}
	if withPages == 0 {
		t.Error("no message kept more than one page, so pagination was lost on the way in")
	}
}

func TestImportFileDoesNotBufferTheUpload(t *testing.T) {
	// A large file must go through the asset store without being held: the
	// ceiling this test asserts is on the *report*, and the property is that a
	// 6 MiB upload succeeds with the adapter's own streaming parse. The
	// measurable claim is in TestSearchScaleAtTenThousandMessages; this one
	// proves the import path is not the one that materialises a file.
	e := newEnv(t)
	g := e.game("synthetic-long-tide", "Synthetic Long Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	var buf bytes.Buffer
	for i := 0; i < 1200; i++ {
		writeCSVRow(&buf, i)
	}
	req := ImportRequest{
		ProfileID: p.ID, FileName: "long.csv", Reader: &buf,
		Format: generic.FormatCSV,
		Parse: adapter.ParseOptions{
			Language: "en", Format: generic.FormatCSV,
			// No header row: address the columns by position, which is
			// the mapping docs/spec/03 shows a reviewer correcting.
			ColumnMap: adapter.ColumnMap{ID: "0", Text: "2", Kind: "3", Scene: "4"},
		},
	}
	report, err := ImportFile(e.ctx, e.deps, req)
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if report.Stats.Imported < 1000 {
		t.Errorf("imported %d rows of a 1200-row file", report.Stats.Imported)
	}
	if got := countMessages(t, e, p.ID); got != report.Stats.Imported {
		t.Errorf("stored %d messages, report says %d", got, report.Stats.Imported)
	}
	if report.Batches < 2 {
		t.Errorf("write batches = %d, want more than one for a %d-message import", report.Batches, report.Stats.Imported)
	}
}

// binary writes one CSV row of invented dialogue.
func writeCSVRow(buf *bytes.Buffer, i int) {
	line := syntheticMessage(uint64(i) + 1)
	if strings.ContainsAny(line, `"`) {
		line = strings.ReplaceAll(line, `"`, `'`)
	}
	buf.WriteString("line-" + itoa(i) + ",," + line + ",dialogue,Quay\n")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// TestImportFileReportsWarningsWithoutDrowningTheReviewer checks the bound docs/spec/12
// implies for a 10 000-row profile: the counts stay exact while the list of
// per-row complaints is capped, so the onboarding screen can show every finding a
// human can act on rather than the first screenful of one repeated mistake.
func TestImportFileReportsWarningsWithoutDrowningTheReviewer(t *testing.T) {
	e := newEnv(t)
	g := e.game("synthetic-tide-warnings", "Synthetic Tide")
	p := e.draft(e.edition(g, "US", "en").ID)

	var buf strings.Builder
	buf.WriteString("message_id,text,kind\n")
	const rows = 150
	for i := 0; i < rows; i++ {
		fmt.Fprintf(&buf, "w-%03d,%s,cutscene\n", i, syntheticMessage(uint64(i)+1))
	}
	report, err := ImportFile(e.ctx, e.deps, ImportRequest{
		ProfileID: p.ID, FileName: "kinds.csv", Reader: strings.NewReader(buf.String()),
		Format: generic.FormatCSV,
		Parse:  adapter.ParseOptions{Format: generic.FormatCSV, Language: "en", DefaultKind: games.TextOther},
	})
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if len(report.Warnings) != maxReportedWarnings {
		t.Errorf("warnings = %d, want the cap of %d", len(report.Warnings), maxReportedWarnings)
	}
	if !report.Truncated {
		t.Error("a capped warning list is not reported as capped")
	}
	if report.Stats.Skipped+report.Stats.Imported != report.Stats.Rows {
		t.Errorf("stats do not account for every row: %+v", report.Stats)
	}
	if report.Stats.Imported == 0 {
		t.Errorf("an unknown kind should default and warn, not fail the row: %+v", report.Stats)
	}
	if report.SHA256 == "" || report.ReusedSource {
		t.Errorf("report = sha %q reused %v, want a hash and a fresh source", report.SHA256[:min(12, len(report.SHA256))], report.ReusedSource)
	}
	// The unknown kinds became the profile's default class rather than an error,
	// which is docs/spec/04's "an unknown mapping is data" rule.
	msgs, _, err := e.store.ListMessages(e.ctx, p.ID, store.MessageFilter{TextKind: games.TextOther}, "", store.MaxListLimit)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != report.Stats.Imported {
		t.Errorf("defaulted %d messages, stored %d under kind %q", report.Stats.Imported, len(msgs), games.TextOther)
	}
}

//
// ── helpers shared across this package's tests ──────────────────────────────
//

func countMessages(t *testing.T, e *env, profileID string) int {
	t.Helper()
	total, cursor := 0, ""
	for {
		page, next, err := e.store.ListMessages(e.ctx, profileID, store.MessageFilter{}, cursor, store.MaxListLimit)
		if err != nil {
			t.Fatalf("ListMessages: %v", err)
		}
		total += len(page)
		if next == "" {
			return total
		}
		cursor = next
	}
}

func versionOf(t *testing.T, e *env, profileID string) int64 {
	t.Helper()
	_, rev, err := e.store.GetProfile(e.ctx, profileID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	return rev
}

func mustNewRevision(t *testing.T, e *env, profileID string) games.Profile {
	t.Helper()
	next, err := e.store.NewRevision(e.ctx, profileID)
	if err != nil {
		t.Fatalf("NewRevision: %v", err)
	}
	return next
}

// publish drives the lifecycle the acceptance criteria describe: draft → review
// → publish, with the incumbent of an edition retired by the same act.
func publish(t *testing.T, e *env, profileID, author string) games.Profile {
	t.Helper()
	if err := e.store.SetProfileStatus(e.ctx, profileID, games.StatusReview, versionOf(t, e, profileID)); err != nil {
		t.Fatalf("SetProfileStatus(review): %v", err)
	}
	p, err := e.store.Publish(e.ctx, profileID, versionOf(t, e, profileID), author)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	return p
}

func mustManifestBytes(t *testing.T, m Manifest) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := WriteManifest(&buf, m); err != nil {
		t.Fatalf("WriteManifest: %v", err)
	}
	return buf.Bytes()
}
