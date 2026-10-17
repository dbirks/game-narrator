package profileio

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/store"
)

// Deps is what an import needs from the outside world: the catalogue, the blobs,
// and the adapters linked into this build. All three are required for importing
// a file; manifest import can run without the asset store, and reports every
// source as needing its bytes re-supplied in that case.
type Deps struct {
	Store    *store.Store
	Assets   assets.Store
	Adapters *adapter.Registry
}

func (d Deps) validate() error {
	if d.Store == nil {
		return fmt.Errorf("%w: Deps.Store is required", ErrInvalid)
	}
	return nil
}

// ImportRequest describes one uploaded script and where it goes.
type ImportRequest struct {
	// ProfileID must name a *draft* revision. Content never enters a published
	// or archived revision (docs/spec/02 "Transaction workflows"), and the
	// repository layer would refuse every write anyway — checking first is what
	// turns that into an onboarding message instead of a 409 from inside a
	// transaction.
	ProfileID string

	// FileName is the name the upload arrived under. It is used for detection,
	// for the provenance URI, and for nothing else: nothing is ever opened from
	// it, so a path traversal in an uploaded name cannot reach a filesystem
	// (docs/spec/14 "Filesystem/import").
	FileName string

	// Reader supplies the bytes. It is consumed once, streamed, and never
	// buffered whole.
	Reader io.Reader

	// Format forces a source format (a generic adapter's ParseOptions.Format).
	// Empty asks the registry to detect one from the name and the first bytes.
	Format string

	// Parse carries the adapter options for this file: column mapping, default
	// kind, language, size limits. ProfileID and SourceID are set by ImportFile
	// and any values here are ignored — they are the importer's to assign, since
	// reconciliation depends on them.
	Parse adapter.ParseOptions

	// Rights is the user's declaration about these bytes. Empty means
	// user_supplied, which is what docs/spec/01 invariant 6 says a handed-over
	// file is; 'unknown' is refused rather than stored, because the question
	// belongs to the onboarding flow (games.Source.Validate forbids it for a
	// file:// URI for the same reason).
	Rights games.RightsStatus

	// Trust is confidence in the source on [0,1]; empty means 1, which is what
	// bytes the user supplied directly deserve.
	Trust float64
}

// ImportReport is the outcome of one import, shaped for the onboarding screen
// docs/spec/03 describes ("the reviewer sees what was parsed").
type ImportReport struct {
	SourceID     string            `json:"source_id"`
	SHA256       string            `json:"sha256"`
	Size         int64             `json:"size"`
	MediaType    string            `json:"media_type"`
	AdapterID    string            `json:"adapter"`
	Format       string            `json:"format"`
	Stats        adapter.Stats     `json:"stats"`
	Warnings     []adapter.Warning `json:"warnings"`
	Truncated    bool              `json:"warnings_truncated"`
	Inserted     int               `json:"messages_inserted"`
	Updated      int               `json:"messages_updated"`
	Pages        int               `json:"pages_written"`
	Batches      int               `json:"write_batches"`
	ReusedSource bool              `json:"reused_source"`
	Characters   int               `json:"characters_created"`
	Speakers     int               `json:"messages_linked_to_speakers"`
	Duration     time.Duration     `json:"duration"`
}

// maxReportedWarnings bounds the report. A malformed file can produce one
// warning per row; docs/spec/12's "10k messages" profile would then drown the
// screen that is supposed to explain the import. The count in Stats is exact.
const maxReportedWarnings = 100

// ImportFile streams a user-supplied script into the asset store and imports it
// into a draft revision, recording provenance as it goes.
//
// The order of operations is the point:
//
//  1. Stream the upload into the asset store while it is hashed (PutStaged, then
//     Commit). Nothing holds the file in memory, and the bytes are durable before
//     any parser sees them, so a crash between stages resumes by content hash
//     (docs/spec/02 "Import resume by content hash").
//  2. Resolve provenance: a source row with this exact content hash already on
//     the revision is reused rather than re-registered. Provenance is
//     write-once (docs/spec/01 invariant 6), so a replay of the same bytes is
//     the same source, whatever the clock says about when it was retrieved.
//  3. Parse from the committed blob, never from the upload, so the bytes the
//     messages came from are exactly the bytes the hash names — a truncated
//     stream cannot produce a profile that disagrees with its own provenance.
//  4. Write messages in batches of ImportBatchSize, reconciling by
//     (source, source_message_id) so a re-import updates rows instead of adding
//     copies of them.
//  5. Bind the source's own speaker labels to the roster: a label the file
//     named is source evidence, so it becomes a characters row (reused by the
//     parse's normalized key, or created) and the row's messages point at it
//     with a source-column attribution and a script_order speaker assertion.
//     Rows the file left unnamed keep unknown attribution: nothing is invented
//     (docs/spec/04 "Generic adapter minimum").
//
// Idempotence is a property of the whole path, not of one step: the same bytes
// imported twice into the same revision leave one source row, the same message
// ids, the same count, the same roster (a label already in the roster is reused,
// not re-created) and one script_order assertion per named message (derived ids
// plus the (message, character, method) triple make a re-import a no-op).
func ImportFile(ctx context.Context, deps Deps, req ImportRequest) (ImportReport, error) {
	start := time.Now()
	var report ImportReport
	if err := deps.validate(); err != nil {
		return report, err
	}
	if deps.Assets == nil {
		return report, fmt.Errorf("%w: ImportFile requires an asset store for the source bytes", ErrInvalid)
	}
	if deps.Adapters == nil || deps.Adapters.Len() == 0 {
		return report, fmt.Errorf("%w: ImportFile requires at least one registered adapter", ErrNoAdapter)
	}
	if req.Reader == nil {
		return report, fmt.Errorf("%w: ImportRequest.Reader is required", ErrInvalid)
	}
	if !games.ValidID(req.ProfileID) {
		return report, fmt.Errorf("%w: ImportRequest.ProfileID %q is not a ULID", ErrInvalid, req.ProfileID)
	}
	if err := requireDraft(ctx, deps.Store, req.ProfileID); err != nil {
		return report, err
	}
	rights := req.Rights
	if rights == "" {
		rights = games.RightsUserSupplied
	}
	if !rights.Valid() {
		return report, fmt.Errorf("%w: ImportRequest.Rights %q", ErrInvalid, string(rights))
	}
	if rights == games.RightsUnknown {
		return report, fmt.Errorf("%w: a file the user handed over has a known rights position (docs/spec/01 invariant 6): record user_supplied or licensed after asking, rather than storing the question", ErrInvalid)
	}
	trust := req.Trust
	if trust == 0 {
		trust = 1
	}
	if trust < 0 || trust > 1 {
		return report, fmt.Errorf("%w: ImportRequest.Trust %v is outside [0,1]", ErrInvalid, trust)
	}

	// ── 1. store the bytes ───────────────────────────────────────────────────
	stagingID, _, err := deps.Assets.PutStaged(ctx, req.Reader, "")
	if err != nil {
		return report, fmt.Errorf("profileio: stage source: %w", err)
	}
	committed, err := deps.Assets.Commit(ctx, stagingID)
	if err != nil {
		// The staged copy must not survive a failed commit: RecoverStaging would
		// collect it eventually, but an importer that retries in a loop would
		// accumulate a copy per attempt until then.
		_ = deps.Assets.Abort(context.WithoutCancel(ctx), stagingID)
		return report, fmt.Errorf("profileio: commit source: %w", err)
	}
	if err := deps.Assets.Verify(ctx, committed.SHA256); err != nil {
		return report, fmt.Errorf("profileio: verify source blob: %w", err)
	}
	report.SHA256 = committed.SHA256
	report.Size = committed.Size
	report.MediaType = committed.MediaType

	// ── 2. provenance ────────────────────────────────────────────────────────
	existing, err := sourceByHash(ctx, deps.Store, req.ProfileID, committed.SHA256)
	if err != nil {
		return report, err
	}

	// ── 3. parse from the committed blob ─────────────────────────────────────
	// The parse is told which source id to write on every row, so a row that
	// survives to the database carries its provenance with it: a message with no
	// source identity cannot be reconciled on the next import
	// (games.Message.Validate says so).
	blob, _, err := deps.Assets.Get(ctx, committed.SHA256)
	if err != nil {
		return report, fmt.Errorf("profileio: read back source: %w", err)
	}
	defer blob.Close()

	sourceID := existing.ID
	if sourceID == "" {
		sourceID = games.NewID()
	}
	parsed, chosen, err := parseBlob(ctx, deps.Adapters, blob, req, sourceID)
	if err != nil {
		return report, err
	}
	report.AdapterID = chosen.ID
	report.Format = chosen.format
	report.Stats = parsed.Stats
	report.Warnings = parsed.Warnings
	if len(parsed.Warnings) > maxReportedWarnings {
		report.Warnings = parsed.Warnings[:maxReportedWarnings]
		report.Truncated = true
	}

	// ── 4. register provenance, then write content ───────────────────────────
	if existing.ID == "" {
		src := games.Source{
			ID:            sourceID,
			ProfileID:     req.ProfileID,
			SourceType:    games.SourceUserFile,
			URI:           sourceURI(req.FileName),
			RetrievedAt:   nowStamp(),
			ContentSHA256: committed.SHA256,
			MimeType:      committed.MediaType,
			Rights:        rights,
			Trust:         trust,
			Parser:        parserID(chosen),
			CoverageNotes: coverageNotes(parsed.Stats, committed),
		}
		if err := src.Validate(); err != nil {
			return report, err
		}
		if err := deps.Store.PutSources(ctx, req.ProfileID, []games.Source{src}); err != nil {
			return report, fmt.Errorf("profileio: register source: %w", err)
		}
		report.ReusedSource = false
	} else {
		// The bytes were already registered. Reuse the row exactly — including
		// the rights the user gave when they first brought the file in, which is
		// what "provenance is immutable" costs and buys (docs/spec/01 invariant
		// 6). Silence here would let a caller believe a re-import had changed
		// the rights position of a source.
		if existing.Rights != rights || existing.Trust != trust || existing.SourceType != games.SourceUserFile {
			report.Warnings = append(report.Warnings, adapter.Warning{
				Line:   0,
				Column: "provenance",
				Msg: fmt.Sprintf("these bytes are already registered as source %s (type %q, rights %q, trust %v, from %q); a re-import cannot rewrite provenance, so that record stands (docs/spec/01 invariant 6) — correct it by registering a new source",
					existing.ID, existing.SourceType, existing.Rights, existing.Trust, existing.URI),
			})
		}
		sourceID = existing.ID
		report.ReusedSource = true
	}
	report.SourceID = sourceID

	// Messages of a reused source keep their database identity: the adapter
	// minted fresh ULIDs, and writing those would add a second copy of every line
	// under a second id (docs/spec/02: "reconcile, not duplicate").
	if report.ReusedSource {
		bySourceMessage, err := messageIDsBySource(ctx, deps.Store, req.ProfileID, sourceID)
		if err != nil {
			return report, err
		}
		for i := range parsed.Messages {
			m := &parsed.Messages[i]
			m.SourceID = sourceID
			if old, ok := bySourceMessage[m.SourceMessageID]; ok {
				m.ID = old
				report.Updated++
				continue
			}
			report.Inserted++
		}
	} else {
		report.Inserted = len(parsed.Messages)
	}

	// ── 5. bind the source's own speaker labels to the roster ────────────────
	// A label the user's file named is source evidence (docs/spec/04's
	// "never invent a speaker" covers unnamed rows only), so it becomes a
	// characters row — an existing one, matched by the parse's normalized key,
	// or a new one — and the row's messages point at it. The messages' ids must
	// already be final (the reconciliation above), because the assertions
	// derived from them are keyed on the stored message id.
	resolution, err := resolveSpeakers(ctx, deps.Store, req.ProfileID, req.Parse.TextNorm, parsed, sourceID)
	if err != nil {
		return report, err
	}
	report.Warnings = append(report.Warnings, resolution.warnings...)
	report.Characters = resolution.created
	linkSpeakers(parsed, resolution)
	report.Speakers = resolution.linked
	if len(resolution.characters) > 0 {
		// Characters precede messages and assertions: both foreign keys run
		// that way (0002_content.sql), so the order is load-bearing, not
		// ceremonial.
		if err := deps.Store.PutCharacters(ctx, req.ProfileID, resolution.characters); err != nil {
			return report, fmt.Errorf("profileio: write speaker characters: %w", err)
		}
	}

	if err := writeMessages(ctx, deps, req.ProfileID, parsed, &report); err != nil {
		return report, err
	}
	if err := writeSpeakerAssertions(ctx, deps, req.ProfileID, parsed, resolution, &report); err != nil {
		return report, err
	}
	report.Duration = time.Since(start)
	return report, nil
}

// writeMessages writes a parse's messages and pages in bounded batches.
//
// Pages are addressed by the id the adapter gave its message, which is not
// necessarily the id being written (a reconciled import keeps the stored id), so
// the pairing is rebuilt per message here rather than trusted from the parse.
func writeMessages(ctx context.Context, deps Deps, profileID string, parsed *adapter.ParseResult, report *ImportReport) error {
	pagesByAdapterID := map[string][]games.MessagePage{}
	for _, p := range parsed.Pages {
		pagesByAdapterID[p.MessageID] = append(pagesByAdapterID[p.MessageID], p)
	}
	for start := 0; start < len(parsed.Messages); start += ImportBatchSize {
		end := min(start+ImportBatchSize, len(parsed.Messages))
		batch := parsed.Messages[start:end]
		batchPages := make([]games.MessagePage, 0, len(batch))
		for _, m := range batch {
			for _, p := range pagesByAdapterID[m.ID] {
				out := p
				out.MessageID = m.ID
				batchPages = append(batchPages, out)
				report.Pages++
			}
		}
		if err := deps.Store.PutMessages(ctx, profileID, batch, batchPages); err != nil {
			return fmt.Errorf("profileio: write messages batch %d-%d: %w", start, end-1, err)
		}
		report.Batches++
	}
	return nil
}

// parseBlob picks an adapter for the committed bytes and runs it over them.
func parseBlob(ctx context.Context, reg *adapter.Registry, blob io.Reader, req ImportRequest, sourceID string) (*adapter.ParseResult, chosenAdapter, error) {
	buffered := bufio.NewReaderSize(blob, headSize)
	head, err := buffered.Peek(headSize)
	if err != nil && err != io.EOF {
		return nil, chosenAdapter{}, fmt.Errorf("profileio: sniff source: %w", err)
	}

	var (
		pick  adapter.Adapter
		match adapter.Manifest
	)
	if req.Format != "" && req.Format != adapter.FormatAuto {
		for _, m := range reg.Adapters() {
			if m.SupportsFormat(req.Format) {
				pick, match = mustGet(reg, m.ID)
				break
			}
		}
		if pick == nil {
			return nil, chosenAdapter{}, fmt.Errorf("%w: no registered adapter reads format %q", ErrNoAdapter, req.Format)
		}
	} else {
		found, ok := reg.DetectOne(ctx, path.Base(req.FileName), head)
		if !ok {
			return nil, chosenAdapter{}, fmt.Errorf("%w: nothing registered recognised %q (%s detected)", ErrNoAdapter, req.FileName, describe(head))
		}
		pick, match = found.Adapter, found.Manifest
	}

	opts := req.Parse
	opts.ProfileID = req.ProfileID
	opts.SourceID = sourceID
	normalized, err := opts.Normalize()
	if err != nil {
		return nil, chosenAdapter{}, err
	}
	result, err := pick.Parse(ctx, buffered, normalized)
	if err != nil {
		return nil, chosenAdapter{}, fmt.Errorf("profileio: parse %s with %s: %w", req.FileName, match.ID, err)
	}
	if result == nil || len(result.Messages) == 0 {
		return nil, chosenAdapter{}, fmt.Errorf("%w: %s found no messages in %s", adapter.ErrNoMessages, match.ID, req.FileName)
	}
	return result, chosenAdapter{ID: match.ID, version: match.Version, format: normalized.Format}, nil
}

// chosenAdapter names the parser for sources.parser: id@semver, which is what
// makes "which build of this importer produced these rows" answerable after the
// adapter itself has moved on (docs/spec/04 "versioned adapter manifest").
type chosenAdapter struct {
	ID      string
	version string
	format  string
}

func (c chosenAdapter) String() string { return c.ID + "@" + c.version }

func parserID(c chosenAdapter) string { return c.String() }

func mustGet(reg *adapter.Registry, id string) (adapter.Adapter, adapter.Manifest) {
	a, ok := reg.Get(id)
	if !ok {
		return nil, adapter.Manifest{}
	}
	return a, a.Manifest()
}

// headSize is what ImportFile sniffs. It is the same sample http.DetectContentType
// and the generic adapter's sniffFormat work from, so detection and parsing
// cannot disagree about what the file is.
const headSize = 512

// describe names a sniffed prefix for an error message without dumping the
// user's file into a log line (docs/spec/14: treat imported content as hostile
// strings).
func describe(head []byte) string {
	s := string(head)
	if len(s) > 24 {
		s = s[:24]
	}
	return fmt.Sprintf("%d bytes starting %q", len(head), strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s))
}

// sourceURI renders an uploaded file's name as the provenance address. It is a
// label for a human and an audit trail, not something to open: the bytes live in
// the asset store under their hash, which is the only thing anyone reads.
func sourceURI(name string) string {
	base := path.Base(strings.TrimSpace(name))
	if base == "" || base == "." || base == "/" {
		base = "upload"
	}
	var b strings.Builder
	b.WriteString("file://")
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == '.', r == '~':
			b.WriteRune(r)
		default:
			// Percent-encode rather than strip: the name is provenance, and
			// silently rewriting it would make two different uploads look like
			// the same one.
			for _, by := range []byte(string(r)) {
				fmt.Fprintf(&b, "%%%02X", by)
			}
		}
	}
	out := b.String()
	if len(out) > 2000 {
		out = out[:2000]
	}
	return out
}

// coverageNotes is the importer's own account of what it saw, in the column
// docs/spec/02 names for exactly that (sources.coverage_notes).
func coverageNotes(stats adapter.Stats, info assets.Info) string {
	return fmt.Sprintf("adapter rows=%d imported=%d skipped=%d; blob bytes=%d media_type=%s",
		stats.Rows, stats.Imported, stats.Skipped, info.Size, info.MediaType)
}

// requireDraft refuses an import into anything that is not a draft, before a
// single byte is written. The repository layer enforces this too; checking first
// is what keeps a 200 MB upload from being stored and hashed only for the write
// to fail on the first message.
func requireDraft(ctx context.Context, s *store.Store, profileID string) error {
	profile, _, err := s.GetProfile(ctx, profileID)
	if err != nil {
		return fmt.Errorf("profileio: %w", err)
	}
	if profile.IsImmutable() {
		return fmt.Errorf("%w: profile %s is %s; derive a revision with NewRevision and import into that (docs/spec/02 \"Transaction workflows\")",
			ErrNotDraft, profileID, profile.Status)
	}
	return nil
}

// sourceByHash returns the provenance row of this revision that already holds
// these exact bytes, or a zero Source when there is none.
func sourceByHash(ctx context.Context, s *store.Store, profileID, sha string) (games.Source, error) {
	sources, err := s.ListSources(ctx, profileID)
	if err != nil {
		return games.Source{}, fmt.Errorf("profileio: list sources: %w", err)
	}
	for _, src := range sources {
		if src.ContentSHA256 == sha {
			return src, nil
		}
	}
	return games.Source{}, nil
}

// messageIDsBySource indexes a revision's existing messages of one source by
// their source-side identity, which is the key the unique index
// messages_source_identity_idx is built on — the reconciliation happens on the
// same columns the database will enforce it on (docs/spec/02 "SQLite schema
// principles": "stable source message IDs alongside generated canonical IDs").
//
// It pages through ListMessages rather than GetProfileContent so the working set
// is one page of rows plus one map of ids, not every message's text.
func messageIDsBySource(ctx context.Context, s *store.Store, profileID, sourceID string) (map[string]string, error) {
	out := map[string]string{}
	cursor := ""
	for {
		page, next, err := s.ListMessages(ctx, profileID, store.MessageFilter{}, cursor, store.MaxListLimit)
		if err != nil {
			return nil, fmt.Errorf("profileio: list messages for reconciliation: %w", err)
		}
		for _, m := range page {
			if m.SourceID == sourceID && m.SourceMessageID != "" {
				out[m.SourceMessageID] = m.ID
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
}

// nowStamp is the RFC 3339 form the schema's timestamp CHECKs accept.
func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }
