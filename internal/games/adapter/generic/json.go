package generic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// messagesKey is the only object key read in the object form, per docs/spec/04's
// generic minimum: an array of objects, or {"messages": [...]}.
const messagesKey = "messages"

// readJSON reads a JSON document: an array of objects, or an object whose
// "messages" key holds one.
//
// It streams with json.Decoder rather than unmarshalling the file, for the same
// reason as the delimited reader: ReadAll of a 500 MB script is not an import, it
// is an outage (docs/spec/14 "memory/time limits"). Elements are decoded as
// json.RawMessage, so a nesting bomb is measured by games.RequireJSON — the same
// depth rule the storage layer applies — *before* anything recurses into it.
func (b *builder) readJSON(br *bufio.Reader) error {
	dec := json.NewDecoder(br)
	// Unknown keys are not disallowed: a hand-rolled script file carries columns
	// nobody would put in a message table (voice file, screenshot name, reviewer
	// notes). They are reported once by reportUnknownKeys and otherwise ignored,
	// so an import keeps working when somebody adds a column.

	tok, err := dec.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("generic: reading JSON: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return fmt.Errorf("generic: expected a JSON array or an object with a %q array, found a single %s; a file of one object per line is jsonl, not json",
			messagesKey, jsonKind(tok))
	}

	switch delim {
	case '[':
		if err := b.jsonElements(dec, "[]"); err != nil {
			return err
		}
	case '{':
		if err := b.jsonObjectForm(dec); err != nil {
			return err
		}
	default:
		return fmt.Errorf("generic: unexpected %q at the top level of a JSON source", string(delim))
	}
	return nil
}

// jsonObjectForm walks the top-level object's keys, decoding only "messages" and
// skipping everything else. Keys that are skipped are reported once each: the
// import summary has to say what it ignored, or a file whose list is called
// "lines" reads back as a successful import of nothing.
func (b *builder) jsonObjectForm(dec *json.Decoder) error {
	seen := map[string]bool{}
	closed := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("generic: reading JSON object key: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("generic: JSON object key is not a string (%s)", jsonKind(tok))
		}
		if key != messagesKey {
			if !seen[key] {
				seen[key] = true
				b.warn(0, key, "ignoring top-level JSON key %q: this adapter reads only %q", key, messagesKey)
			}
			if err := skipValue(dec); err != nil {
				return fmt.Errorf("generic: skipping JSON key %q: %w", key, err)
			}
			continue
		}
		open, err := dec.Token()
		if err != nil {
			return fmt.Errorf("generic: reading %q: %w", messagesKey, err)
		}
		if d, ok := open.(json.Delim); !ok || d != '[' {
			return fmt.Errorf("generic: %q must be an array of message objects, found a %s", messagesKey, jsonKind(open))
		}
		if err := b.jsonElements(dec, messagesKey); err != nil {
			return err
		}
		closed = true
	}
	if !closed {
		b.warn(0, messagesKey, "the JSON object had no %q array, so nothing was imported", messagesKey)
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("generic: closing the JSON object: %w", err)
	}
	return nil
}

// jsonElements decodes array elements until the closing bracket, one at a time.
// caller names the container for error messages.
func (b *builder) jsonElements(dec *json.Decoder, caller string) error {
	for dec.More() {
		if err := b.contextIfDone(); err != nil {
			return err
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("generic: reading an element of the JSON %s: %w", caller, err)
		}
		if err := games.RequireJSON("element", raw); err != nil {
			// A too-deep element is refused rather than flattened: the alternative
			// is to keep the parts that happen to be strings, which is exactly the
			// "confidently wrong index" outcome the limits exist to prevent.
			return err
		}
		fields, notes, keys, err := jsonFields(raw)
		if err != nil {
			return err
		}
		if b.columns == nil {
			// The first element's keys are this source's column names, which is
			// what makes JSON mapping the same operation as CSV mapping.
			columns, unmatched := resolveColumns(keys, b.opts.ColumnMap)
			b.setColumns(columns, unmatched)
		}
		line := b.nextJSONLine(raw)
		for _, n := range notes {
			b.warn(line, n.column, "%s", n.msg)
		}
		if err := b.add(b.bindJSON(line, fields)); err != nil {
			return err
		}
		b.reportUnknownKeys(keys)
	}
	if _, err := dec.Token(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("generic: closing the JSON %s: %w", caller, err)
	}
	return nil
}

// reportUnknownKeys warns once per key the mapping did not claim. It is a
// serviceability detail with a sharp edge: without it, a renamed column reads as
// a successful import that lost every speaker.
func (b *builder) reportUnknownKeys(keys []string) {
	if b.seenKeys == nil {
		b.seenKeys = map[string]bool{}
	}
	claimed := make(map[string]bool, len(b.columns))
	for _, name := range b.columns {
		claimed[name] = true
	}
	for _, k := range keys {
		if claimed[k] || b.seenKeys[k] {
			continue
		}
		b.seenKeys[k] = true
		b.warn(0, k, "JSON key %q is not mapped to anything and was ignored; if it carries text, dialogue or a speaker, add it to the column mapping", k)
	}
}

type fieldNote struct {
	column string
	msg    string
}

// jsonFields converts one message object into the same keyed record a CSV row
// produces. Values that are not strings are handled by kind:
//
//   - a number or a boolean becomes its source text, because a message id stored
//     as 142 is the same identity as "142" and refusing it would be pedantry;
//   - null is absence;
//   - an object or an array is reported and dropped, because there is no place
//     for nested structure in a message row and inventing one is how markup gets
//     mangled (docs/spec/02 keeps that in an AST with offsets, which a generic
//     source does not have).
func jsonFields(raw json.RawMessage) (fields map[string]string, notes []fieldNote, keys []string, err error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, nil, nil, fmt.Errorf("generic: a JSON message must be an object: %w", err)
	}
	fields = make(map[string]string, len(obj))
	keys = make([]string, 0, len(obj))
	for key, val := range obj {
		keys = append(keys, key)
		trimmed := bytes.TrimSpace(val)
		switch {
		case len(trimmed) == 0:
			continue
		case trimmed[0] == '"':
			var s string
			if err := json.Unmarshal(trimmed, &s); err != nil {
				notes = append(notes, fieldNote{column: key, msg: fmt.Sprintf("string value could not be decoded: %v", err)})
				continue
			}
			fields[key] = s
		case trimmed[0] == '{' || trimmed[0] == '[':
			notes = append(notes, fieldNote{column: key, msg: fmt.Sprintf("value is %s, not text; nested JSON is not imported", jsonKindName(trimmed))})
		case bytes.Equal(trimmed, []byte("null")):
			// Absence, not the empty string: the difference decides whether a
			// speaker is unknown or unnamed.
		case trimmed[0] == 't' || trimmed[0] == 'f' || trimmed[0] == '-' || (trimmed[0] >= '0' && trimmed[0] <= '9'):
			notes = append(notes, fieldNote{column: key, msg: fmt.Sprintf("value %s is not a string; its source text was imported", string(trimmed))})
			fields[key] = string(trimmed)
		default:
			notes = append(notes, fieldNote{column: key, msg: fmt.Sprintf("value %s is not valid JSON text and was skipped", snippet(string(trimmed)))})
		}
	}
	return fields, notes, keys, nil
}

// readJSONLines reads one JSON object per line. It is a separate reader rather
// than a flag on readJSON because the two have different failure modes: a broken
// line in JSONL is one bad record in an otherwise appendable file, while a broken
// element in a JSON array invalidates the document.
func (b *builder) readJSONLines(br *bufio.Reader) error {
	line := 0
	for {
		if err := b.contextIfDone(); err != nil {
			return err
		}
		text, err := readCappedLine(br)
		line++
		if errors.Is(err, errLineTooLong{}) {
			b.skipRow(line, "line", "%v; the rest of the line was skipped", err)
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		atEOF := errors.Is(err, io.EOF)
		if text == "" {
			// A blank line between records, or the end of input: readCappedLine
			// reports io.EOF only when it has nothing left to hand back.
			if atEOF {
				return nil
			}
			continue
		}

		var element json.RawMessage
		if err := json.Unmarshal([]byte(text), &element); err != nil {
			b.skipRow(line, "json", "line is not valid JSON: %v", err)
			if atEOF {
				return nil
			}
			continue
		}
		if err := games.RequireJSON("element", element); err != nil {
			return fmt.Errorf("generic: line %d: %w", line, err)
		}
		fields, notes, keys, err := jsonFields(element)
		if err != nil {
			return fmt.Errorf("generic: line %d: %w", line, err)
		}
		if b.columns == nil {
			columns, unmatched := resolveColumns(keys, b.opts.ColumnMap)
			b.setColumns(columns, unmatched)
		}
		for _, n := range notes {
			b.warn(line, n.column, "%s", n.msg)
		}
		if err := b.add(b.bindJSON(line, fields)); err != nil {
			return err
		}
		b.reportUnknownKeys(keys)
		if atEOF {
			return nil
		}
	}
}

// nextJSONLine estimates the source line a decoded element ends on.
//
// json.Decoder reports byte offsets, not lines. Counting the newlines inside the
// elements consumed so far is exact for a pretty-printed file (one element per
// line, which is how people write these by hand) and an underestimate for a
// minified one, where every element reports the same line. That is stated rather
// than papered over: a warning that points at *a* line in the file is worth
// having, and claiming a precision the decoder cannot give would be worse.
func (b *builder) nextJSONLine(raw json.RawMessage) int {
	b.jsonLine += countNewlines(raw)
	if b.jsonLine < 1 {
		b.jsonLine = 1
	}
	return b.jsonLine
}

func countNewlines(b []byte) int { return bytes.Count(b, []byte("\n")) }

// bindJSON turns one decoded object's values into a record, addressing them by
// the column names the mapping resolved.
//
// Unlike the delimited readers, this one builds a map per record (jsonFields
// cannot know an object's keys otherwise). That costs an allocation per row, and
// is why the bounded-memory test drives a CSV: a JSON script of 100 000 messages
// is possible but unusual, whereas a spreadsheet export of exactly that size is
// the ordinary case the onboarding path has to survive.
func (b *builder) bindJSON(line int, fields map[string]string) sourceRow {
	r := sourceRow{line: line}
	get := func(col adapter.LogicalColumn) (string, bool) {
		name, ok := b.columns[col]
		if !ok {
			return "", false
		}
		v, ok := fields[name]
		return v, ok
	}
	r.text, _ = get(adapter.ColText)
	r.id, r.idSet = get(adapter.ColID)
	r.speaker, _ = get(adapter.ColSpeaker)
	r.kind, _ = get(adapter.ColKind)
	r.scene, _ = get(adapter.ColScene)
	r.page, _ = get(adapter.ColPage)
	return r
}

// skipValue consumes one complete JSON value from a token stream.
//
// json.Decoder has no Skip, and the depth bookkeeping is written out rather than
// folded into the caller because the naive version — "read tokens until the
// nesting is back to zero" — consumes one token too many at the end and eats the
// next object key of the parent. This loop stops the moment the value's own
// closing delimiter is read.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, isDelim := tok.(json.Delim); !isDelim {
		return nil // a scalar: already consumed
	}
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}

func jsonKind(v any) string {
	switch v.(type) {
	case json.Delim:
		if v == json.Delim('{') {
			return "object"
		}
		return "array"
	case string:
		return "string"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case nil:
		return "null"
	}
	return strconv.Quote(fmt.Sprint(v))
}

// jsonKindName names a raw value for a warning without decoding it.
func jsonKindName(trimmed []byte) string {
	switch trimmed[0] {
	case '{':
		return "an object"
	case '[':
		return "an array"
	}
	return "a value"
}

// snippet shortens a raw value for a warning line.
func snippet(s string) string {
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:40]) + "…"
	}
	return s
}
