package generic

import (
	"bufio"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// readDelimited reads CSV or TSV.
//
// Three reader settings come straight from the safety list and are each a
// footgun the other way, so they are named rather than assumed:
//
//   - LazyQuotes = false. A stray double quote must stop the import rather than
//     be absorbed into a field, because absorbing it shifts every later column on
//     that row and produces confident wrong messages.
//   - FieldsPerRecord = -1. Ragged rows are the norm in hand-edited scripts: they
//     are reported as warnings, the fields that are present are still mapped, and
//     a missing trailing column does not lose the line.
//   - ReuseRecord = true. The record slice is copied into a sourceRow and never
//     retained, which is what keeps a 100 000-row file from allocating a slice per
//     row (docs/spec/16 M1 "without memory blowup").
func (b *builder) readDelimited(br *bufio.Reader, sep rune, format string) error {
	cr := csv.NewReader(br)
	cr.Comma = sep
	cr.LazyQuotes = false
	cr.FieldsPerRecord = -1
	cr.ReuseRecord = true

	first, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil // an empty file produces no messages; Parse reports that
	}
	if err != nil {
		return csvError(err)
	}
	// ReuseRecord hands out the same slice on every Read, so the header row is
	// copied here. Without the copy, the second Read overwrites the strings the
	// column mapping is keyed by, and every row after the first reports "no text
	// column" while reading its *own* values as header names — a failure that
	// looks like a broken file and is in fact a broken reader.
	headers := append([]string(nil), first...)

	lines := &csvLineCounter{}

	columns, unmatched := resolveColumns(headers, b.opts.ColumnMap)
	headerless := false
	if !looksLikeHeader(headers, columns) {
		headerless = true
		columns = positionalColumns(b.opts.ColumnMap, len(headers))
		unmatched = nil
		b.warn(1, "columns", "first row does not name its columns convincingly (it holds %s), so it was treated as data and the columns were mapped by position: %s. Map the columns explicitly if this file really has a header row.",
			previewFields(headers), describeColumns(columns))
	}
	b.setColumns(columns, unmatched)

	// Resolve each role to a column index once, here, rather than per record.
	var idx [numSlots]int
	for i := range idx {
		idx[i] = -1
	}
	for _, col := range logicalOrder {
		name, ok := columns[col]
		if !ok {
			continue
		}
		if at := columnIndex(headers, name); at >= 0 {
			idx[slotOf(col)] = at
		}
	}

	// keyed copies one reused record into the roles this adapter understands.
	keyed := func(line int, fields []string) sourceRow {
		r := sourceRow{line: line}
		pick := func(slot int) (string, bool) {
			at := idx[slot]
			if at < 0 || at >= len(fields) {
				return "", false
			}
			return fields[at], true
		}
		r.text, _ = pick(slotText)
		r.id, r.idSet = pick(slotID)
		r.speaker, _ = pick(slotSpeaker)
		r.kind, _ = pick(slotKind)
		r.scene, _ = pick(slotScene)
		r.page, _ = pick(slotPage)
		return r
	}

	// In header mode the record just read *is* the header and carries no message.
	// In headerless mode it is the first message. That is the only place the two
	// modes differ.
	if headerless {
		if err := b.add(keyed(lines.record(headers), headers)); err != nil {
			return err
		}
	} else {
		lines.record(headers)
	}

	for {
		if err := b.contextIfDone(); err != nil {
			return err
		}
		fields, err := cr.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return csvError(err)
		}
		line := lines.record(fields)
		if !headerless && len(fields) != len(headers) {
			b.warn(line, "row", "row has %d fields, the header row has %d; missing fields are treated as empty",
				len(fields), len(headers))
		}
		if err := b.add(keyed(line, fields)); err != nil {
			return err
		}
	}
}

// csvLines estimates source line numbers for records read from encoding/csv,
// which publishes none. One per record plus every newline inside a quoted field:
// exact for the records themselves. Lines that csv skips on its own (empty lines)
// are not counted, so a file that has them reports a slightly early line. Warnings
// locate a row approximately and say what they are doing rather than claiming an
// exactness this standard library reader cannot provide.
type csvLineCounter struct{ n int }

func (c *csvLineCounter) record(fields []string) int {
	start := c.n + 1
	c.n = start
	for _, f := range fields {
		c.n += strings.Count(f, "\n")
	}
	return start
}

// looksLikeHeader decides whether the first row of a delimited file names its
// columns or is itself the first record.
//
// The rule: the text role must be present, and either it is the first field or at
// least two roles matched. A row where only one later field happens to be a word
// from the candidate list ("First line,Sae,dialogue") is far more likely a record
// whose third value is the string "dialogue" than a header row; reading it as a
// header would drop a message and address every later row by the wrong name. Being
// wrong the other way costs one warning that names the setting to fix, which is why
// this is a heuristic at all and says so out loud.
func looksLikeHeader(headers []string, columns map[adapter.LogicalColumn]string) bool {
	textCol, ok := columns[adapter.ColText]
	if !ok {
		return false
	}
	if columnIndex(headers, textCol) == 0 {
		return true
	}
	return len(columns) >= 2
}

// columnIndex resolves a mapped column name to its position: an explicit decimal
// address ("2") always means the third column, and anything else is looked up by
// header text. The dual meaning is what lets one mapping structure serve a header
// row and a headerless file.
func columnIndex(headers []string, name string) int {
	if i, err := strconv.Atoi(strings.TrimSpace(name)); err == nil && i >= 0 {
		return i
	}
	at, ok := indexOf(headers, name)
	if !ok {
		return -1
	}
	return at
}

// positionalColumns maps roles onto column indices for a file with no header row,
// honouring an explicit mapping that names indices.
//
// The default order is the one a person writes a script in: the line, then who says
// it, then what kind of text it is. A different order would be one game's export
// format encoded in shared code, which docs/spec/04 forbids.
func positionalColumns(cm adapter.ColumnMap, n int) map[adapter.LogicalColumn]string {
	defaults := map[adapter.LogicalColumn]int{
		adapter.ColText:    0,
		adapter.ColSpeaker: 1,
		adapter.ColKind:    2,
		adapter.ColScene:   3,
		adapter.ColID:      4,
		adapter.ColPage:    5,
	}
	explicit := map[adapter.LogicalColumn]string{
		adapter.ColText:    cm.Text,
		adapter.ColID:      cm.ID,
		adapter.ColSpeaker: cm.Speaker,
		adapter.ColKind:    cm.Kind,
		adapter.ColScene:   cm.Scene,
		adapter.ColPage:    cm.Page,
	}
	out := make(map[adapter.LogicalColumn]string, len(defaults))
	for _, col := range logicalOrder {
		if name, ok := explicit[col]; ok && name != "" {
			if i, err := strconv.Atoi(strings.TrimSpace(name)); err == nil && i >= 0 && i < n {
				out[col] = strconv.Itoa(i)
				continue
			}
			// A name that is not an index cannot address anything in a headerless
			// file. Leave the role unmapped and let the report say so rather than
			// guessing which column the user meant.
			continue
		}
		if i, ok := defaults[col]; ok && i < n {
			out[col] = strconv.Itoa(i)
		}
	}
	return out
}

func describeColumns(columns map[adapter.LogicalColumn]string) string {
	var parts []string
	for _, col := range logicalOrder {
		if name, ok := columns[col]; ok {
			parts = append(parts, string(col)+"="+name)
		}
	}
	if len(parts) == 0 {
		return "nothing mapped"
	}
	return strings.Join(parts, ", ")
}

// previewFields renders the first fields of a row for a warning message.
func previewFields(fields []string) string {
	if len(fields) == 0 {
		return "nothing"
	}
	shown, suffix := fields, ""
	if len(shown) > 4 {
		shown, suffix = shown[:4], ", …"
	}
	parts := make([]string, 0, len(shown))
	for _, f := range shown {
		parts = append(parts, strconv.Quote(truncateForWarning(f)))
	}
	return strings.Join(parts, ", ") + suffix
}

// csvError names the row and the problem. encoding/csv's ParseError carries both
// already; it is rephrased because its own text reports where the reader noticed
// the trouble ("parse error on line 4, column 20") rather than the record that
// needs fixing, which reads as a lie to someone comparing it to a spreadsheet.
func csvError(err error) error {
	var pe *csv.ParseError
	if errors.As(err, &pe) {
		return fmt.Errorf("generic: %s in the record starting on line %d (noticed at line %d, column %d): %w",
			csvErrorKind(pe.Err), pe.StartLine, pe.Line, pe.Column, err)
	}
	return fmt.Errorf("generic: %w", err)
}

func csvErrorKind(err error) string {
	switch {
	case errors.Is(err, csv.ErrFieldCount):
		return "wrong number of fields"
	case errors.Is(err, csv.ErrBareQuote), errors.Is(err, csv.ErrQuote):
		return "malformed quoting"
	case errors.Is(err, csv.ErrTrailingComma):
		return "trailing comma"
	}
	return "parse error"
}
