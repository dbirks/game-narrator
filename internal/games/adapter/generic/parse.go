package generic

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// Parse reads a source into validated messages.
//
// The reader is wrapped in a byte budget before anything looks at it, the format
// is either trusted or sniffed from the first bytes, and the format-specific
// reader streams one record at a time into the builder. Nothing in this path
// holds more than one record plus the accumulated messages, which is what lets a
// 100 000-row script import in bounded memory (docs/spec/16 M1 "without memory
// blowup").
func (a Adapter) Parse(ctx context.Context, r io.Reader, opts adapter.ParseOptions) (*adapter.ParseResult, error) {
	opts, err := opts.Normalize()
	if err != nil {
		return nil, err
	}
	m := a.Manifest()
	if !m.SupportsFormat(opts.Format) {
		return nil, fmt.Errorf("%w: %q (this adapter reads %s)", adapter.ErrFormatNotSupported,
			opts.Format, strings.Join(m.SupportedFormats, ", "))
	}
	limiter := newSizeLimiter(r, opts.MaxBytes)
	br := bufio.NewReaderSize(limiter, 32<<10)

	format := opts.Format
	if format == adapter.FormatAuto {
		head, _ := br.Peek(peekBytes)
		format = sniffFormat(head)
		if format == "" {
			return nil, fmt.Errorf("%w: the source is empty", adapter.ErrNoMessages)
		}
	}

	b, err := newBuilder(ctx, opts, format)
	if err != nil {
		return nil, err
	}
	switch format {
	case FormatCSV:
		err = b.readDelimited(br, ',', format)
	case FormatTSV:
		err = b.readDelimited(br, '\t', format)
	case FormatJSON:
		err = b.readJSON(br)
	case FormatJSONL:
		err = b.readJSONLines(br)
	case FormatText:
		err = b.readText(br)
	default:
		return nil, fmt.Errorf("%w: %q", adapter.ErrFormatNotSupported, format)
	}
	if err != nil {
		return nil, err
	}
	if limiter.overflow {
		// A reader that stopped at the end of its buffer must not turn a size
		// breach into a successful partial import.
		return nil, fmt.Errorf("%w: the source is larger than %d bytes", ErrTooLarge, opts.MaxBytes)
	}
	res := b.finalize()
	if len(res.Messages) == 0 {
		return nil, fmt.Errorf("%w: %d rows were read and none produced a message (%d warnings)",
			adapter.ErrNoMessages, res.Stats.Rows, len(res.Warnings))
	}
	return res, nil
}

//
// ── builder ─────────────────────────────────────────────────────────────────
//

// builder turns format-specific records into messages. It exists so the readers
// share one set of decisions — what an unknown content class means, what a
// repeated message identity means, when a row is skipped — which is the only way
// those decisions can stay consistent between formats.
type builder struct {
	ctx    context.Context
	opts   adapter.ParseOptions
	format string

	// columns maps each role onto the source's own column name or key. Readers
	// translate it once into indices (delimited) or key lookups (JSON) rather than
	// per record.
	columns map[adapter.LogicalColumn]string

	folder  *textnorm.Folder
	kindDef games.TextKind

	// stamp is one instant for the whole parse. Sharing the string also keeps
	// 100k messages from allocating 200k timestamps: assigning from the same
	// string value copies the header, not the bytes.
	stamp string

	res *adapter.ParseResult

	items []*item
	// byID locates a repeated source identity so its row becomes a page instead
	// of a duplicate message. Values are indices into items, not pointers, so the
	// map holds no second copy of anything.
	byID map[string]int

	suppressed int

	// seenKeys and jsonLine serve the JSON readers: once-per-key warnings, and a
	// running estimate of which source line an element ended on.
	seenKeys map[string]bool
	jsonLine int

	// speakerIndex and speakerSpells build ParseResult.Speakers: a key onto its
	// Speakers entry, and the raw spellings seen for it, in first-occurrence
	// order. They are released in finalize with the rest of the parse state.
	speakerIndex  map[string]int
	speakerSpells map[string][]string

	// evidenceCache and flagsCache share the encoded provenance of rows whose
	// labels repeat, which is the normal case in a script (one hundred lines by
	// "Marla"). A ParseResult's JSON columns are read-only by convention; a caller
	// that intended to edit one would copy it first, as the store's own writers do.
	evidenceCache map[string]json.RawMessage
	flagsCache    map[string]json.RawMessage
}

// item is one message under construction: its text pages, in source order, plus
// the labels the source attached. Single-page messages — nearly all of them —
// keep no slice at all, which is the difference between 100k small structs and
// 100k small structs plus 100k one-element slices.
type item struct {
	line     int
	text     string
	extra    []string // pages 2..n, set only when a source identity repeated
	extraIDs []string
	pageID   string // the source's own identity for page 1
	speaker  string
	scene    string
	kind     games.TextKind
	sourceID string
}

// pages returns the item's page texts. The single-page case is answered without
// allocating a slice.
func (it *item) pages() []string {
	if len(it.extra) == 0 {
		return []string{it.text}
	}
	return append([]string{it.text}, it.extra...)
}

func (it *item) pageIDAt(i int) string {
	if i == 0 {
		return it.pageID
	}
	if i-1 < len(it.extraIDs) {
		return it.extraIDs[i-1]
	}
	return ""
}

func newBuilder(ctx context.Context, opts adapter.ParseOptions, format string) (*builder, error) {
	def := opts.DefaultKind
	if def == "" {
		// TextOther rather than TextDialogue: docs/spec/04's generic minimum asks
		// for a "conservative narrator/unknown speaker", and a line the source
		// never classified is not evidence that a character said it. TextOther is
		// still spoken under the default policy (see TextKind.DefaultSpeakable),
		// so honesty costs the player nothing.
		def = games.TextOther
	}
	return &builder{
		ctx:           ctx,
		opts:          opts,
		format:        format,
		folder:        textnorm.NewFolder(opts.TextNorm),
		kindDef:       def,
		stamp:         time.Now().UTC().Format(time.RFC3339),
		res:           &adapter.ParseResult{},
		byID:          make(map[string]int, 1024),
		evidenceCache: map[string]json.RawMessage{},
		flagsCache:    map[string]json.RawMessage{},
		speakerIndex:  map[string]int{},
		speakerSpells: map[string][]string{},
	}, nil
}

// setColumns records the resolved mapping and warns once about explicit names the
// source does not have, which is the most common way an import silently produces
// 200 messages with no speakers.
func (b *builder) setColumns(columns map[adapter.LogicalColumn]string, unmatched []string) {
	b.columns = columns
	for _, u := range unmatched {
		b.warn(0, "columns", "mapped column is not present in the source: %s; that field stays empty for every row", u)
	}
	if _, ok := columns[adapter.ColText]; !ok {
		b.warn(0, "columns", "no text column was found or mapped, so no row can be imported (seen: %s)", describeColumns(columns))
	}
}

func (b *builder) warn(line int, column, format string, args ...any) {
	if len(b.res.Warnings) >= maxWarnings {
		b.suppressed++
		return
	}
	b.res.Warnings = append(b.res.Warnings, adapter.Warning{
		Line:   line,
		Column: column,
		Msg:    fmt.Sprintf(format, args...),
	})
}

// add folds one source record into the result. A row it cannot import is skipped
// with a warning: one bad line in a 5 000-line script must not lose the other
// 4 999, and must not vanish without a line number either.
func (b *builder) add(r sourceRow) error {
	b.res.Stats.Rows++
	if b.res.Stats.Rows > b.opts.MaxRows {
		return fmt.Errorf("%w: the source has more than %d rows", ErrTooManyRows, b.opts.MaxRows)
	}
	if err := b.ctx.Err(); err != nil {
		return err
	}

	text := r.text
	if len(r.pages) > 0 {
		// A record that arrived as pages carries no single text value; the
		// message's text is their join, and the split is preserved for the page
		// rows afterwards.
		text = strings.Join(r.pages, "\n\n")
	}
	if strings.TrimSpace(text) == "" {
		b.skipRow(r.line, "text", "row has no text in the mapped column %q", b.columns[adapter.ColText])
		return nil
	}
	if len(text) > maxFieldBytes {
		b.skipRow(r.line, "text", "text is %d bytes, above the %d byte limit for one field", len(text), maxFieldBytes)
		return nil
	}
	if !utf8.ValidString(text) {
		bad := invalidUTF8Count(text)
		b.warn(r.line, "text", "text contains %d invalid UTF-8 sequence(s); each was replaced with U+FFFD, so the stored text is not byte-identical to the source", bad)
		text = strings.ToValidUTF8(text, string(utf8.RuneError))
	}
	if n := utf8.RuneCountInString(text); n > maxTextRunes {
		b.skipRow(r.line, "text", "text is %d characters, above the %d character limit the message column holds", n, maxTextRunes)
		return nil
	}
	for i, page := range r.pages {
		if len(page) > maxFieldBytes {
			b.skipRow(r.line, "text", "page %d is %d bytes, above the %d byte limit for one field", i+1, len(page), maxFieldBytes)
			return nil
		}
	}

	sourceID, idWarning := b.sourceID(r)
	if idWarning != "" {
		b.warn(r.line, "id", "%s", idWarning)
	}

	if at, duplicate := b.byID[sourceID]; duplicate {
		// A repeated source identity is the shape of a multi-page message, which
		// docs/spec/02 insists is one message with several pages rather than
		// several messages. Anything else the repeat disagrees about is reported:
		// a script reusing one id for two speakers is a script bug, and only a
		// curator can fix it.
		it := b.items[at]
		if len(it.extra) == 0 {
			it.extra = []string{text}
			it.extraIDs = []string{cleanSourceID(r.page)}
		} else {
			it.extra = append(it.extra, text)
			it.extraIDs = append(it.extraIDs, cleanSourceID(r.page))
		}
		b.checkRepeatConsistent(it, r)
		return nil
	}

	it := &item{
		line:     r.line,
		text:     text,
		pageID:   cleanSourceID(r.page),
		sourceID: sourceID,
		kind:     b.kindDef,
	}
	if len(r.pages) > 1 {
		// The record arrived as several pages (a plain-text script with paging on),
		// so they stay split for the page rows. Each page is sanitized
		// individually: text above is the sanitized *join*, and storing the raw
		// page bytes beside it would put invalid UTF-8 into original_text and
		// page_text through the back door. For one page there is nothing to split,
		// and it.text already holds the sanitized value.
		clean := make([]string, len(r.pages))
		for i, page := range r.pages {
			clean[i] = strings.ToValidUTF8(page, string(utf8.RuneError))
		}
		it.text = clean[0]
		it.extra = clean[1:]
	}
	if r.kind != "" {
		kind, warned := b.parseKind(r.kind)
		it.kind = kind
		if warned != "" {
			b.warn(r.line, "kind", "%s", warned)
		}
	}
	it.speaker = strings.TrimSpace(r.speaker)
	it.scene = strings.TrimSpace(r.scene)
	b.items = append(b.items, it)
	b.byID[sourceID] = len(b.items) - 1
	return nil
}

// checkRepeatConsistent warns about the ways a repeated message identity can be
// wrong.
func (b *builder) checkRepeatConsistent(it *item, r sourceRow) {
	if sp := strings.TrimSpace(r.speaker); sp != "" && sp != it.speaker {
		b.warn(r.line, "speaker", "row repeats message %q with a different speaker (%q; the first page said %q). The first one is kept",
			it.sourceID, sp, it.speaker)
	}
	if r.kind != "" {
		if kind, warned := b.parseKind(r.kind); warned == "" && kind != it.kind {
			b.warn(r.line, "kind", "row repeats message %q with kind %q; the first page said %q. The first one is kept",
				it.sourceID, kind, it.kind)
		}
	}
	if len(it.extra) == 1 {
		return
	}
	// Two pages is expected. Past that, say so once per extra page: a script whose
	// "id" column is really a row counter collides on every empty value and
	// produces a 5 000-page message, and the warning is how anyone finds that out.
	b.warn(r.line, "id", "message %q now has %d pages; check that the id column is an identity and not a counter",
		it.sourceID, len(it.extra)+1)
}

// sourceID returns the source-side identity for a row, generating one from the
// row's position when the source has none.
//
// An identity is always present: with ParseOptions.SourceID set,
// games.Message.Validate requires a source_message_id, because a row that cannot
// be recognised on the next import would be inserted twice (docs/spec/02 "Import
// resume by content hash and job_id").
//
// Generated ids count rows rather than lines: a minified JSON document puts every
// record on line 1, and a line-derived identity would collide, merging the whole
// file into one enormous message.
func (b *builder) sourceID(r sourceRow) (string, string) {
	raw := strings.TrimSpace(r.id)
	if raw == "" {
		if r.idSet {
			return fmt.Sprintf("%s-%d", b.format, b.res.Stats.Rows),
				"the id column is present but empty on this row; its position was used instead"
		}
		return fmt.Sprintf("%s-%d", b.format, b.res.Stats.Rows), ""
	}
	if cleaned := cleanSourceID(raw); cleaned != raw {
		// The suffix keeps the replacement unique, and the base is shortened to
		// leave room for it: maxSourceSideID is the storage limit, and an id that
		// overflowed it would be rejected at insert time with no explanation.
		suffix := "#" + strconv.Itoa(b.res.Stats.Rows)
		base := cleaned
		if len(base)+len(suffix) > maxSourceSideID {
			base = base[:maxSourceSideID-len(suffix)]
		}
		return base + suffix, fmt.Sprintf("id %q cannot be an identity (it must be 1-%d printable characters with no spaces); used %q",
			truncateForWarning(raw), maxSourceSideID, base+suffix)
	}
	return raw, ""
}

// maxSourceSideID repeats the bound games.Message.Validate enforces on
// source_message_id. It is restated here rather than imported because the games
// package keeps it private, and because discovering it at insert time would mean
// a half-written import.
const maxSourceSideID = 120

// cleanSourceID strips what a source-side identity may not contain — whitespace,
// control bytes, non-ASCII (games.validSourceSideID) — and cuts the result to the
// storage limit. A BMG dump whose id reads "bmg 142" becomes "bmg142" with a
// warning, rather than being dropped for a space.
func cleanSourceID(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			continue
		}
		if b.Len() >= maxSourceSideID {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// parseKind maps a source's class label onto TextKind, defaulting rather than
// failing when the label is unknown: an unknown mapping is data for the profile,
// not an import error (docs/spec/04 "Adapter evolution and repair"). The returned
// TextKind is one of the package constants, not a substring of the source, so a
// profile whose rows are all "dialogue" shares one 8-byte string 100 000 times
// instead of allocating a copy per row.
func (b *builder) parseKind(raw string) (games.TextKind, string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return b.kindDef, ""
	}
	if k, err := games.ParseTextKind(strings.ToLower(raw)); err == nil {
		return canonicalKind(k), ""
	}
	return b.kindDef, fmt.Sprintf("kind %q is not a known content class; used %q. Map the value in the profile or fix the script",
		raw, b.kindDef)
}

// kindConstants indexes the vocabulary by its own spelling so the canonical
// value can be returned without allocating.
var kindConstants = func() map[games.TextKind]games.TextKind {
	out := map[games.TextKind]games.TextKind{}
	for _, k := range []games.TextKind{
		games.TextDialogue, games.TextSign, games.TextItem, games.TextMenu,
		games.TextTutorial, games.TextSystem, games.TextOther,
	} {
		out[k] = k
	}
	return out
}()

func canonicalKind(k games.TextKind) games.TextKind {
	if c, ok := kindConstants[k]; ok {
		return c
	}
	return k
}

func (b *builder) skipRow(line int, column, format string, args ...any) {
	b.res.Stats.Skipped++
	b.warn(line, column, format, args...)
}

// finalize builds the messages and pages, validating each one. An item that will
// not validate is skipped with a warning rather than returned: docs/spec/04
// requires every message the generic adapter produces to be storable, and one
// that games.Message.Validate rejects would be lost later with less to say about
// itself.
func (b *builder) finalize() *adapter.ParseResult {
	res := b.res
	res.SpeakerColumn = b.columns[adapter.ColSpeaker]
	for _, it := range b.items {
		original := it.text
		pages := it.pages()
		if len(pages) > 1 {
			original = strings.Join(pages, "\n\n")
		}
		if n := utf8.RuneCountInString(original); n > maxTextRunes {
			b.skipRow(it.line, "text", "message %q spans %d characters across %d pages, above the %d character limit",
				it.sourceID, n, len(pages), maxTextRunes)
			continue
		}
		normalized := b.folder.Normalize(original)
		if normalized == "" {
			// Nothing left to match on. Keeping the row would put an unmatchable
			// message in the index, which is worse than skipping it and saying why:
			// recognition could never return it, and its presence would make the
			// profile look covered when it is not.
			b.skipRow(it.line, "normalizedText", "message %q normalizes to nothing (punctuation and whitespace only), so it cannot be matched and was not imported",
				it.sourceID)
			continue
		}

		msg := games.Message{
			ID:                games.NewID(),
			ProfileID:         b.opts.ProfileID,
			SourceID:          b.opts.SourceID,
			SourceMessageID:   it.sourceID,
			OriginalText:      original,
			NormalizedText:    normalized,
			SpeechText:        original,
			Language:          b.opts.Language,
			TextKind:          it.kind,
			AttributionStatus: games.AttributionUnknown,
			Revision:          1,
			CreatedAt:         b.stamp,
			UpdatedAt:         b.stamp,
		}
		if it.speaker != "" {
			// The label is kept; the character id is not invented. speaker_id is a
			// foreign key to characters and no such row exists yet — resolving
			// labels into characters is onboarding's job and a curator's decision
			// (docs/spec/04: "use scene signals, neighbouring message chain,
			// screenshot evidence and operator corrections"). What the parse does
			// add is the matching key: the label folded the way message text is,
			// so the importer can bind case- and spacing-variants to one
			// character instead of two.
			msg.AttributionStatus = games.AttributionProposed
			msg.AttributionConfidence = proposedSpeakerConfidence
			msg.AttributionEvidence = b.evidenceFor(it.speaker)
			msg.Flags = b.flagsFor(it)
		} else if it.scene != "" {
			msg.Flags = b.flagsFor(it)
		}
		if err := msg.Validate(); err != nil {
			b.skipRow(it.line, "message", "message %q failed validation and was not imported: %v", it.sourceID, err)
			continue
		}
		res.Messages = append(res.Messages, msg)
		if key := b.speakerKeyOf(it); key != "" {
			// Registered only for messages that actually land: a speaker whose
			// sole row was skipped must not become an unresolvable roster entry,
			// and Speakers/SpeakerKeys must stay exactly in lockstep.
			res.SpeakerKeys = append(res.SpeakerKeys, key)
			b.registerSpeaker(key, it.speaker)
		} else {
			res.SpeakerKeys = append(res.SpeakerKeys, "")
		}
		res.Stats.Imported++

		if len(pages) == 1 && it.pageID == "" {
			// One page with no source page identity needs no page row: the message
			// *is* the page, and materializing one per message would double the
			// index for every script in existence that has no paging.
			continue
		}
		for i, pageText := range pages {
			if n := utf8.RuneCountInString(pageText); n > maxTextRunes {
				b.warn(it.line, "page", "page %d of message %q is %d characters, above the limit; the page row was dropped and the message kept",
					i+1, it.sourceID, n)
				continue
			}
			p := games.MessagePage{
				MessageID:    msg.ID,
				Ordinal:      i + 1,
				SourcePageID: it.pageIDAt(i),
				PageText:     pageText,
			}
			if err := p.Validate(); err != nil {
				b.warn(it.line, "page", "page %d of message %q failed validation and was dropped: %v", i+1, it.sourceID, err)
				continue
			}
			res.Pages = append(res.Pages, p)
		}
	}
	if b.suppressed > 0 {
		res.Warnings = append(res.Warnings, adapter.Warning{
			Line: 0,
			Msg:  fmt.Sprintf("%d further warnings suppressed (the list is capped at %d)", b.suppressed, maxWarnings),
		})
	}
	// Release the parse-time state before the result is handed out: the items and
	// their index are what the accumulator needed, not what the caller reads, and
	// holding them alive until the builder itself is collected is a second copy of
	// the file's text in the heap for no reason.
	b.items = nil
	b.byID = nil
	b.evidenceCache = nil
	b.flagsCache = nil
	b.speakerIndex = nil
	b.speakerSpells = nil
	return res
}

// speakerKeyOf returns the sidecar key for a message the caller is about to
// append: the folded speaker label when one was kept, "" otherwise. It is
// computed from the item rather than the registered Speakers entry because a
// label that folds to nothing (punctuation only) is kept as evidence but has no
// key to reconcile against.
func (b *builder) speakerKeyOf(it *item) string {
	if it.speaker == "" {
		return ""
	}
	return b.folder.Normalize(it.speaker)
}

// registerSpeaker records one occurrence of a speaker label under its folded
// key. The first spelling seen becomes the DisplayName, later ones are aliases
// when they differ from it. A label that differs only in case or spacing folds
// to the same key as its first occurrence and so becomes an alias of one
// character, not a second (docs/spec/04: the source's own labels are source
// evidence, and a roster must not list one person twice for one typo).
func (b *builder) registerSpeaker(key, spelling string) {
	if idx, ok := b.speakerIndex[key]; ok {
		seen := b.speakerSpells[key]
		for _, s := range seen {
			if s == spelling {
				return
			}
		}
		if spelling != b.res.Speakers[idx].DisplayName {
			b.res.Speakers[idx].Aliases = append(b.res.Speakers[idx].Aliases, spelling)
		}
		b.speakerSpells[key] = append(seen, spelling)
		return
	}
	b.speakerIndex[key] = len(b.res.Speakers)
	// SourceNamed true: the label is the source's own text from the mapped
	// column, so the importer may confirm the speaker at the source column's
	// strength (a synthetic id, ADR-0070, leaves this false).
	b.res.Speakers = append(b.res.Speakers, adapter.SpeakerRef{Key: key, DisplayName: spelling, SourceNamed: true})
	b.speakerSpells[key] = []string{spelling}
}

// proposedSpeakerConfidence is what a source's own label is worth. Not 1.0: a
// script column naming a speaker is strong evidence, but docs/spec/02 refuses to
// treat an in-file or on-screen name as proof, and 1.0 would claim a label and a
// line can never be misaligned.
const proposedSpeakerConfidence = 0.9

// evidenceFor returns the attribution provenance for a speaker label, shared
// between every row with the same label.
func (b *builder) evidenceFor(label string) json.RawMessage {
	if raw, ok := b.evidenceCache[label]; ok {
		return raw
	}
	raw, err := json.Marshal([]map[string]any{{
		"method":    "source_label",
		"at":        b.stamp,
		"evidence":  label,
		"posterior": proposedSpeakerConfidence,
	}})
	if err != nil {
		// json.Marshal of a map of strings and a float cannot fail. If it somehow
		// did, dropping the provenance and keeping the message is the right trade:
		// the text is the product, the evidence is the label's label.
		return nil
	}
	b.evidenceCache[label] = raw
	return raw
}

// flagsFor keeps the source's own speaker and scene labels as data. They are not
// foreign keys and must not be thrown away: the character and scene rows do not
// exist yet, and onboarding matches these labels against them (docs/spec/04
// "Generic adapter minimum": "allow annotating at runtime").
func (b *builder) flagsFor(it *item) json.RawMessage {
	key := it.speaker + "\x00" + it.scene
	if raw, ok := b.flagsCache[key]; ok {
		return raw
	}
	labels := map[string]string{}
	if it.speaker != "" {
		labels["source_speaker"] = it.speaker
	}
	if it.scene != "" {
		labels["source_scene"] = it.scene
	}
	if len(labels) == 0 {
		return nil
	}
	raw, err := json.Marshal(labels)
	if err != nil {
		return nil
	}
	b.flagsCache[key] = raw
	return raw
}

//
// ── shared record type ──────────────────────────────────────────────────────
//

// sourceRow is one logical record with its roles already resolved.
//
// It is a struct of named fields rather than a map of column names because a
// 100 000-row import would otherwise allocate and discard 100 000 maps, and the
// garbage is what turns a streaming reader into a memory problem: the peak is
// bounded by how fast the collector runs rather than by the result's size. Each
// reader resolves its column mapping once, up front, and fills this struct in
// place afterwards.
type sourceRow struct {
	line int

	text string
	// pages, when set, replaces text: the record arrived as the pages of one
	// message. The plain-text reader uses it, because a wrapped textbox script
	// carries a message's pages inline with no column to name them and no id to
	// group them by.
	pages []string

	id      string
	idSet   bool // the source has an id column, even if this row's value is empty
	speaker string
	kind    string
	scene   string
	page    string
}

//
// ── helpers ─────────────────────────────────────────────────────────────────
//

// invalidUTF8Count counts maximal invalid byte sequences, for the warning text.
func invalidUTF8Count(s string) int {
	n := 0
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError && size <= 1 {
			n++
			s = s[1:]
			continue
		}
		s = s[size:]
	}
	return n
}

// truncateForWarning shortens a value for a warning line, on rune boundaries so
// the message cannot end in half a character.
func truncateForWarning(s string) string {
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}
