// Package bmg parses BMG (big-endian message group) files into a
// lossless token stream. No bytes are ever dropped: every parsed message
// reconstructs exactly to its raw pool bytes via Reconstruct.
//
// A BMG file holds a fixed 0x20-byte header, a set of opaque sections
// (INF1, DAT1, and optionally MID1 / FLW1 / FLI1), and a big-endian
// text pool. A message runs from its INF1 offset to the next NUL
// terminator that is not part of a well-formed 0x1A control tag (tag
// bodies may contain zero bytes, so the terminator scan is tag-aware;
// see ADR-0029). Control bytes are parsed into tagged tokens with
// source offsets; malformed tags are retained as Invalid tokens, and
// undocumented fields are kept as raw attribute bytes — never
// interpreted, dropped, or logged.
package bmg

import "errors"

// ErrNotABMG is returned when the file header magic is not "MESGbmg1".
var ErrNotABMG = errors.New("bmg: not a BMG file")

// Bounds enforced by Parse (spec: hostile input safety).
const (
	MaxSections  = 64
	MaxEntries   = 100000
	MinEntrySize = 4
	MaxEntrySize = 64
	MaxMsgLen    = 64 * 1024 // message text length cap (1-byte units)
)

// Section is one opaque file section: magic, position, and raw bytes.
// Raw includes the 8-byte section header.
type Section struct {
	Magic  string
	Offset int
	Size   int
	Raw    []byte
}

// Message is one parsed message: INF1 metadata plus the tokenized text.
// TextOffset points into the DAT1 pool (relative to pool start).
// Attributes are the raw INF1 entry bytes after the offset word.
// Raw is the exact pool slice the tokens reconstruct.
type Message struct {
	Index      int
	ID         uint32 // from MID1 if present, else Index
	TextOffset uint32
	Attributes []byte
	Raw        []byte
	Tokens     []Token
}

// TokenKind classifies a token in a message's lossless token stream.
type TokenKind int

const (
	// TokText is a plain text run (no control bytes).
	TokText TokenKind = iota
	// TokTag is a well-formed 0x1A control tag.
	TokTag
	// TokInvalid is a control tag that is malformed (bad length or
	// truncated); its raw bytes are retained, never dropped.
	TokInvalid
)

// Token is one element of a message's lossless token stream.
// Offset/Length are positions within the message Raw slice.
// Group/Type/Args are populated for TokTag; Args are the tag body
// bytes after the 0x1A+length header. Raw is the exact original bytes.
type Token struct {
	Kind   TokenKind
	Offset int
	Length int
	Group  int
	Type   int
	Args   []byte
	Raw    []byte
}

// File is a fully parsed BMG file.
type File struct {
	Encoding byte // raw file header encoding byte, recorded verbatim
	Sections []Section
	Messages []Message
}

// Parse parses a BMG file into a lossless token model.
// It returns ErrNotABMG on a bad magic and descriptive errors on
// structural violations; it never panics on any input.
// The header u32 at 0x08 is treated as an opaque reserved field (the
// real file does not store the true file size there); the section
// count is read from 0x0C and the encoding byte from 0x10.
func Parse(b []byte) (*File, error) {
	count, encoding, err := parseHeader(b)
	if err != nil {
		return nil, err
	}
	secs, _, err := scanSections(b, count)
	if err != nil {
		return nil, err
	}
	pool, err := poolBytes(secs)
	if err != nil {
		return nil, err
	}
	entries, _, err := parseINF1(secs)
	if err != nil {
		return nil, err
	}
	ids := parseMID1(secs, len(entries))
	msgs, err := buildMessages(encoding, pool, entries, ids)
	if err != nil {
		return nil, err
	}
	return &File{Encoding: encoding, Sections: secs, Messages: msgs}, nil
}

// Reconstruct concatenates the token raw bytes of m. For a parsed
// message it always equals m.Raw exactly (losslessness property).
func (m Message) Reconstruct() []byte {
	total := 0
	for _, t := range m.Tokens {
		total += t.Length
	}
	out := make([]byte, 0, total)
	for _, t := range m.Tokens {
		out = append(out, t.Raw...)
	}
	return out
}
