package generic

import (
	"bytes"
	"context"
	"path"
	"strings"

	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// Adapter identity. ID is what lands in sources.parser, so it is frozen: a
// renamed adapter orphans every row that recorded it (docs/spec/02 "Source").
const (
	// ID is the adapter's registered name.
	ID = "generic"
	// Version is the adapter's semver. A parse that produces different messages
	// from the same file bumps this (docs/spec/04 "versioned adapter manifest").
	Version = "1.1.0"
	// SchemaVersion is the ParseResult shape this build emits. Version 2 added
	// the speaker sidecar (Speakers/SpeakerKeys/SpeakerColumn): the same
	// messages, plus the source's own labels resolved into matching keys. The
	// semver bumps because a re-import of an old file through the new build
	// produces a different result shape, which is the signal a cached revision
	// is stale (docs/spec/04 "Adapter evolution and repair").
	SchemaVersion = 2
)

// Formats this adapter reads. They are the ParseOptions.Format values.
const (
	FormatCSV   = "csv"
	FormatTSV   = "tsv"
	FormatJSON  = "json"
	FormatJSONL = "jsonl"
	FormatText  = "txt"
)

// Adapter parses user-supplied tabular and text scripts. It carries no state:
// one value serves every import, which is what makes it safe to hand out of a
// Registry to concurrent requests (docs/spec/08's server is concurrent by
// definition).
type Adapter struct{}

// Compile-time proof that the interface is implemented, so a signature change in
// the adapter package fails the build here rather than at an import site.
var _ adapter.Adapter = Adapter{}

// New returns the adapter. A function rather than a zero value because callers
// register it by interface and an explicit call reads better at wiring sites.
func New() adapter.Adapter { return Adapter{} }

// Register adds the generic adapter to r. Wiring lives in the linking package
// (docs/spec/04: first-party adapters are statically linked), which is also where
// a game-specific adapter is registered alongside it.
func Register(r *adapter.Registry) error { return r.Register(New()) }

// Manifest describes this adapter.
func (Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		ID:               ID,
		Version:          Version,
		SchemaVersion:    SchemaVersion,
		Description:      "Reads a user-supplied CSV, TSV, JSON, JSON Lines or plain-text dialogue script with column mapping. No game-specific knowledge.",
		SupportedFormats: []string{FormatCSV, FormatTSV, FormatJSON, FormatJSONL, FormatText},
		RequiredPermissions: []string{
			adapter.PermReadSource,
		},
		Locales: []string{"en", "ja"},
		Fixtures: []string{
			"testdata/synthetic-adventure.csv",
			"testdata/synthetic-jp.jsonl",
			"testdata/plain.txt",
			"testdata/synthetic-other-game.json",
		},
	}
}

// extensions maps a file name suffix to a format. The mapping is by *suffix*,
// not by MIME type, because the thing being identified is a file the user picked
// in a dialog, and the extension is the only signal the upload carries
// (docs/spec/03 onboarding hands over a path, not an HTTP part with a type).
var extensions = map[string]string{
	".csv":    FormatCSV,
	".tsv":    FormatTSV,
	".tab":    FormatTSV,
	".json":   FormatJSON,
	".jsonl":  FormatJSONL,
	".ndjson": FormatJSONL,
	".txt":    FormatText,
	".text":   FormatText,
	".md":     FormatText,
}

// Detect reports how confidently this adapter could parse name/head.
//
// Name and content can disagree — a .txt file that is really TSV exported by a
// spreadsheet is a weekly occurrence — so the two signals are combined rather
// than one preferred blindly: agreement is the strongest claim the generic
// adapter makes, a name with unreadable content is weaker, and a contradiction
// is weak but non-zero, because Parse will follow an explicit Format and the user
// may well know better than the file name.
func (a Adapter) Detect(ctx context.Context, name string, head []byte) adapter.Confidence {
	_ = ctx // detection reads nothing; the parameter is the interface's
	sniffed := sniffFormat(head)
	lower := strings.ToLower(name)
	ext := strings.ToLower(path.Ext(lower))
	fromName, nameKnown := extensions[ext]
	// A name with no recognizable suffix ("Dialogues Export", or a dump whose
	// real extension was stripped) contributes no signal, so it is scored on
	// content alone. "script.v1.csv" still resolves, because the suffix is what
	// is looked up, not the whole base name.
	if nameKnown && sniffed == fromName {
		return adapter.ConfidentFormat
	}
	if nameKnown && len(bytes.TrimSpace(head)) == 0 {
		return adapter.ConfidentName
	}
	if sniffed != "" && !nameKnown {
		return adapter.ConfidentSniffed
	}
	if nameKnown {
		// Name says one thing, content says another: still parseable, since
		// Parse trusts an explicit format and sniffs otherwise, but the caller
		// should show the disagreement instead of auto-picking silently.
		return adapter.ConfidentWeak
	}
	if sniffed == FormatText {
		return adapter.ConfidentText
	}
	return adapter.ConfidentNone
}

// sniffFormat guesses a format from the first bytes of a source, or "" when the
// sample is empty. It is used by Detect and by Parse when Format is FormatAuto,
// and it is the same function in both places on purpose: a file Detect claims to
// recognise must parse the way detection said it would, or the onboarding UI
// picks an adapter on one reading of the bytes and imports on another.
func sniffFormat(head []byte) string {
	trimmed := bytes.TrimLeft(head, "\ufeff \t\r\n")
	if len(trimmed) == 0 {
		return ""
	}
	switch trimmed[0] {
	case '{', '[':
		return sniffJSON(trimmed)
	}
	return sniffDelimitedOrText(head)
}

// sniffJSON separates a JSON document from JSON Lines by whether anything but
// whitespace follows the first complete value. A one-object-per-line file starts
// with '{' too, so a bracket check alone would call it json and then fail to
// decode the second line.
func sniffJSON(b []byte) string {
	depth := 0
	inString := false
	escaped := false
	for i := 0; i < len(b); i++ {
		c := b[i]
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
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				rest := bytes.TrimLeft(b[i+1:], "\ufeff \t\r\n")
				if len(rest) == 0 {
					return FormatJSON
				}
				// A comma means "more elements of the same document", which is
				// json; anything else following a complete top-level value is a
				// separate record, which is jsonl.
				if rest[0] == ',' {
					return FormatJSON
				}
				return FormatJSONL
			}
		}
	}
	// The sample ended mid-document: too big for the peek window, but it is one
	// JSON document and not lines of them.
	return FormatJSON
}

// sniffDelimitedOrText counts candidate field separators on the first lines. A
// real CSV/TSV repeats the same delimiter count on every row; a prose line with
// a comma in it does not, and calling prose TSV would split a novel at every tab.
func sniffDelimitedOrText(head []byte) string {
	sep, count := byte(0), 0
	for _, cand := range []byte{',', '\t', ';'} {
		c := countDelimiter(head, cand)
		if c > count {
			sep, count = cand, c
		}
	}
	if sep == 0 || !repeats(head, sep) {
		return FormatText
	}
	switch sep {
	case '\t':
		return FormatTSV
	case ',':
		return FormatCSV
	}
	// Semicolon-separated values are still comma-shaped to the reader, which is
	// configured by separator rather than by name. Calling it csv is honest about
	// the shape; Parse sets the separator from the sniffed byte, not the label.
	return FormatCSV
}

// countDelimiter counts c outside quoted fields, on up to four lines.
func countDelimiter(head []byte, c byte) int {
	n := 0
	inQuotes := false
	lines := 0
	for i := 0; i < len(head) && lines < 4; i++ {
		switch head[i] {
		case '"':
			inQuotes = !inQuotes
		case '\n':
			lines++
		case c:
			if !inQuotes {
				n++
			}
		}
	}
	return n
}

// repeats reports whether every line of the sample that has any content has at
// least one occurrence of c. Requiring "at least one per line" rather than "the
// same count per line" tolerates a ragged tail (which Parse warns about) without
// tolerating prose.
func repeats(head []byte, c byte) bool {
	seenLine := false
	for _, line := range bytes.Split(head, []byte("\n")) {
		line = bytes.TrimRight(line, "\r \t")
		if len(line) == 0 {
			continue
		}
		seenLine = true
		if !bytes.Contains(line, []byte{c}) {
			return false
		}
	}
	return seenLine
}

// InferFormat reports the format Parse would choose for these bytes. It is
// exported for the onboarding UI, which shows "detected: tsv" before the user
// commits to an import (docs/spec/03 "the reviewer sees what was parsed").
func InferFormat(head []byte) string { return sniffFormat(head) }

//
// ── column name auto-mapping ────────────────────────────────────────────────
//

// autoMapCandidates are the source column names accepted for each role when the
// caller did not say which is which. The lists are deliberately short and
// boring: they name the words a person writes in a spreadsheet header, not a
// game's own vocabulary, which is the point of the portability rule.
//
// Matching is case- and whitespace-insensitive (a header of " Speaker Name " or
// "SPEAKER" is the same header), which is done with textnorm-free folding here
// because a column name is not game text.
var autoMapCandidates = map[adapter.LogicalColumn][]string{
	adapter.ColText:    {"text", "line", "dialogue", "message", "dialog", "dialoguetext", "speech"},
	adapter.ColID:      {"id", "message_id", "messageid", "mid", "key", "index"},
	adapter.ColSpeaker: {"speaker", "character", "name", "who", "speakername", "charactername"},
	adapter.ColKind:    {"kind", "type", "class", "category", "textkind", "messagetype"},
	adapter.ColScene:   {"scene", "area", "room", "location", "chapter", "stage"},
	adapter.ColPage:    {"page", "pages", "pageid", "pagenum", "page_number", "part"},
}

// Slots index the per-record role array. A fixed array instead of a map is the
// difference between an import that allocates one small map per row and one that
// allocates none: at 100 000 rows, the maps are tens of megabytes of garbage whose
// collection timing, not the result size, would decide peak memory.
const (
	slotText = iota
	slotID
	slotSpeaker
	slotKind
	slotScene
	slotPage
	numSlots
)

func slotOf(col adapter.LogicalColumn) int {
	switch col {
	case adapter.ColText:
		return slotText
	case adapter.ColID:
		return slotID
	case adapter.ColSpeaker:
		return slotSpeaker
	case adapter.ColKind:
		return slotKind
	case adapter.ColScene:
		return slotScene
	case adapter.ColPage:
		return slotPage
	}
	return -1
}

// foldHeader normalizes a column name for comparison: lowercase, no spaces,
// underscores or hyphens, so "Speaker Name", "speaker_name" and "SPEAKERNAME"
// all address the same column.
func foldHeader(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch r {
		case ' ', '_', '-', '.', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// resolveColumns maps every logical role onto a concrete column name present in
// keys. Explicit names win; then the auto-map candidates; then nothing. A role
// with no column is simply absent from the result, which the builder reads as
// "this source does not carry that information" rather than as an error.
func resolveColumns(keys []string, cm adapter.ColumnMap) (map[adapter.LogicalColumn]string, []string) {
	byFold := make(map[string]string, len(keys))
	for _, k := range keys {
		byFold[foldHeader(k)] = k
	}
	explicit := map[adapter.LogicalColumn]string{
		adapter.ColText:    cm.Text,
		adapter.ColID:      cm.ID,
		adapter.ColSpeaker: cm.Speaker,
		adapter.ColKind:    cm.Kind,
		adapter.ColScene:   cm.Scene,
		adapter.ColPage:    cm.Page,
	}
	out := make(map[adapter.LogicalColumn]string, len(explicit))
	var unmatched []string
	for _, col := range logicalOrder {
		name, ok := explicit[col]
		if !ok {
			continue
		}
		if name == "" {
			continue
		}
		// An explicit name addresses a column exactly first, then loosely: a
		// caller who copied the header out of the file should always hit, and a
		// caller who retyped it with different spacing should too.
		if _, has := indexOf(keys, name); !has {
			folded, has := byFold[foldHeader(name)]
			if !has {
				unmatched = append(unmatched, string(col)+"="+name)
				continue
			}
			name = folded
		}
		out[col] = name
	}
	for _, col := range logicalOrder {
		if _, done := out[col]; done {
			continue
		}
		for _, cand := range autoMapCandidates[col] {
			if k, ok := byFold[cand]; ok {
				// A column claimed by an earlier role cannot be claimed again:
				// "name" is a speaker in most scripts and a message id in a few,
				// and a double binding would silently duplicate one into the
				// other. First role wins in logicalOrder, which puts text and id
				// ahead of speaker.
				if used(out, k) {
					continue
				}
				out[col] = k
				break
			}
		}
	}
	return out, unmatched
}

// logicalOrder fixes the precedence used by auto-mapping, so a column named
// "id" is never taken by a later role and the resolution is reproducible.
var logicalOrder = []adapter.LogicalColumn{
	adapter.ColText, adapter.ColID, adapter.ColKind, adapter.ColPage, adapter.ColSpeaker, adapter.ColScene,
}

func used(resolved map[adapter.LogicalColumn]string, key string) bool {
	for _, k := range resolved {
		if k == key {
			return true
		}
	}
	return false
}

func indexOf(keys []string, want string) (int, bool) {
	for i, k := range keys {
		if k == want {
			return i, true
		}
	}
	return -1, false
}
