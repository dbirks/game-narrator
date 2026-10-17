package windwaker

// OriginalText escape format.
//
// A message's original_text must decode the BMG pool bytes **losslessly**
// (docs/spec/02 "store original_text byte-faithful") while remaining a Go
// string the UI can display and the curator can copy. The escape is a small,
// documented grammar over the token stream (bmg.Token is the lossless source
// of truth; the escape is its text projection):
//
//	text run bytes (the 1-byte CP1252-like pool encoding):
//	  0x00-0x1F          ->  \x00 .. \x1f      (two lowercase hex digits)
//	  0x20-0x7E          ->  the literal byte, except:
//	    '\' (0x5C)       ->  \\
//	    '{' (0x7B)       ->  \{
//	    '}' (0x7D)       ->  \}
//	  0x7F               ->  \x7f
//	  0x80-0x9F          ->  the CP1252 character where CP1252 defines one,
//	                          otherwise  \xNN  (the five CP1252 holes)
//	  0xA0-0xFF          ->  the ISO-8859-1 (Latin-1) character
//
//	well-formed control tag (bmg.TokTag):
//	  {tag g=<group> t=<type> a=<args>}
//	    group/type are the token's decimal values; args is the tag body as
//	    lowercase hex. The raw bytes are 0x1A, len = 2+len(args), args — so
//	    the placeholder reconstructs the exact tag bytes.
//
//	malformed control tag (bmg.TokInvalid):
//	  {tag bad r=<raw hex>}
//	    the full raw bytes as lowercase hex, because a malformed tag has no
//	    dependable group/type/args decomposition to encode instead.
//
// Encoding (escape -> bytes) is the exact inverse: every produced character
// maps back to exactly one byte, so decode(encode(raw)) == raw and
// encode(decode(s)) == s hold for any s this package produces. The braces
// are escaped in text runs (not the tag placeholders' own braces), which is
// what makes the grammar unambiguous without a context-sensitive parser.
//
// The grammar carries no game text: it is byte-level syntax. Real Wind Waker
// pool text turned out to be pure ASCII plus 0x0A (line break), but the full
// CP1252-like range is supported because the encoding byte is recorded
// verbatim by the bmg package and a different dump may use more of it.

import (
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/dbirks/game-narrator/internal/games/bmg"
)

// cp1252 maps the 0x80-0x9F range. The five holes (0x81, 0x8D, 0x8F, 0x90,
// 0x9D) are deliberately absent: CP1252 leaves them undefined, and encoding
// them to a guessed character would break the round-trip.
var cp1252 = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…',
	0x86: '†', 0x87: '‡', 0x88: 'ˆ', 0x89: '‰', 0x8A: 'Š',
	0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž',
	0x91: '‘', 0x92: '’', 0x93: '“', 0x94: '”', 0x95: '•',
	0x96: '–', 0x97: '—', 0x98: '˜', 0x99: '™', 0x9A: 'š',
	0x9B: '›', 0x9C: 'œ', 0x9E: 'ž', 0x9F: 'Ÿ',
}

// cp1252Inverse maps back from the characters the decoder can produce to
// their single byte. Built once so encodeOriginal never rebuilds it.
var cp1252Inverse = buildCP1252Inverse()

func buildCP1252Inverse() map[rune]byte {
	out := make(map[rune]byte, 0x100)
	// int, not byte: a byte counter wrapping past 0xFF would loop forever
	// under the b <= 0xFF condition.
	for b := 0; b < 0x80; b++ {
		out[rune(b)] = byte(b) // ASCII: identity (escaping handles the reserved ones)
	}
	for b := 0xA0; b <= 0xFF; b++ {
		out[rune(b)] = byte(b) // Latin-1 upper half: identity
	}
	for b, r := range cp1252 {
		out[r] = b
	}
	return out
}

// decodeOriginal projects a message's token stream into the escaped string
// stored as games.Message.OriginalText. It never fails: every token kind is
// covered and no input byte is dropped.
func decodeOriginal(tokens []bmg.Token) string {
	var b strings.Builder
	for _, t := range tokens {
		switch t.Kind {
		case bmg.TokText:
			decodeTextRun(&b, t.Raw)
		case bmg.TokTag:
			fmt.Fprintf(&b, "{tag g=%d t=%d a=%s}", t.Group, t.Type, strings.ToLower(hex.EncodeToString(t.Args)))
		case bmg.TokInvalid:
			fmt.Fprintf(&b, "{tag bad r=%s}", strings.ToLower(hex.EncodeToString(t.Raw)))
		}
	}
	return b.String()
}

// decodeTextRun escapes one plain-text run.
func decodeTextRun(b *strings.Builder, raw []byte) {
	for _, c := range raw {
		switch {
		case c < 0x20:
			fmt.Fprintf(b, "\\x%02x", c)
		case c == 0x7F:
			b.WriteString("\\x7f")
		case c >= 0x20 && c <= 0x7E:
			switch c {
			case '\\':
				b.WriteString(`\\`)
			case '{':
				b.WriteString(`\{`)
			case '}':
				b.WriteString(`\}`)
			default:
				b.WriteByte(c)
			}
		case c >= 0x80 && c <= 0x9F:
			if r, ok := cp1252[c]; ok {
				b.WriteRune(r)
			} else {
				fmt.Fprintf(b, "\\x%02x", c)
			}
		default: // 0xA0-0xFF
			b.WriteRune(rune(c))
		}
	}
}

// encodeOriginal is the inverse of decodeOriginal: it turns an escaped
// OriginalText back into the exact BMG pool bytes. It is a total function on
// strings this package produced and a strict parser on any input: an unknown
// escape, a malformed placeholder or a character without a byte is an error,
// so a corrupted or hand-edited original_text fails loudly instead of
// silently re-encoding to different bytes.
func encodeOriginal(s string) ([]byte, error) {
	var out []byte
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '\\':
			if i+1 >= len(s) {
				return nil, fmt.Errorf("windwaker: original text ends with a bare backslash at offset %d", i)
			}
			switch s[i+1] {
			case 'x':
				if i+4 > len(s) {
					return nil, fmt.Errorf("windwaker: truncated \\x escape at offset %d", i)
				}
				n, err := parseHex2(s[i+2 : i+4])
				if err != nil {
					return nil, fmt.Errorf("windwaker: bad \\x escape at offset %d: %w", i, err)
				}
				out = append(out, byte(n))
				i += 4
			case '\\':
				out = append(out, '\\')
				i += 2
			case '{':
				out = append(out, '{')
				i += 2
			case '}':
				out = append(out, '}')
				i += 2
			default:
				return nil, fmt.Errorf("windwaker: unknown escape %q at offset %d", s[i:i+2], i)
			}
		case c == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("windwaker: unterminated tag placeholder at offset %d", i)
			}
			ph := s[i : i+end+1]
			rest, err := encodeTagPlaceholder(ph)
			if err != nil {
				return nil, fmt.Errorf("windwaker: %w (offset %d)", err, i)
			}
			out = append(out, rest...)
			i += end + 1
		default:
			// Rune, not byte: the decoder writes multi-byte UTF-8 for the
			// CP1252 upper range (0x80-0x9F), so the inverse must read one
			// rune, map it, and advance by the rune's size.
			r, size := utf8.DecodeRuneInString(s[i:])
			b, ok := cp1252Inverse[r]
			if !ok {
				return nil, fmt.Errorf("windwaker: character %q at offset %d has no CP1252-like byte; original text was not produced by this adapter", r, i)
			}
			out = append(out, b)
			i += size
		}
	}
	return out, nil
}

// encodeTagPlaceholder decodes one "{tag …}" placeholder to its raw bytes.
func encodeTagPlaceholder(ph string) ([]byte, error) {
	inner := strings.TrimPrefix(ph, "{")
	if !strings.HasPrefix(inner, "tag ") || !strings.HasSuffix(inner, "}") {
		return nil, fmt.Errorf("malformed tag placeholder %q", ph)
	}
	inner = inner[len("tag ") : len(inner)-1]
	if strings.HasPrefix(inner, "bad r=") {
		// {tag bad r=<raw hex>}: the malformed-tag form carries its exact bytes.
		hexStr := inner[len("bad r="):]
		if hexStr == "" {
			return nil, fmt.Errorf("tag placeholder carries no raw bytes: %q", ph)
		}
		n, err := hex.DecodeString(hexStr)
		if err != nil {
			return nil, fmt.Errorf("tag placeholder raw %q is not hex: %w", hexStr, err)
		}
		if len(n) == 0 {
			return nil, fmt.Errorf("tag placeholder raw bytes are empty: %q", ph)
		}
		return n, nil
	}
	if strings.HasPrefix(inner, "bad ") {
		return nil, fmt.Errorf("malformed tag placeholder missing r= bytes: %q", ph)
	}
	// {tag g=<group> t=<type> a=<args>}: the well-formed form reconstructs the
	// tag as 0x1A, len = 2+len(args), args — the tokenizer's own layout.
	_, _, aHex, err := parseTagPlaceholder(inner) // group/type are range-checked there
	if err != nil {
		return nil, err
	}
	args, err := hex.DecodeString(aHex)
	if err != nil {
		return nil, fmt.Errorf("tag placeholder args %q are not hex: %w", aHex, err)
	}
	if len(args) > 0xFE {
		return nil, fmt.Errorf("tag placeholder args exceed the 1-byte length field (%d bytes)", len(args))
	}
	out := []byte{0x1A, byte(2 + len(args))}
	return append(out, args...), nil
}

// parseTagPlaceholder splits the "g=.. t=.. a=.." part, requiring exactly the
// documented fields in order. Group and type must fit the token's own byte
// widths (u8 group, u16 type).
func parseTagPlaceholder(s string) (g, t, a string, err error) {
	fields := strings.Fields(s)
	if len(fields) != 3 {
		return "", "", "", fmt.Errorf("tag placeholder needs exactly g=, t= and a= fields, got %q", s)
	}
	for i, want := range []string{"g=", "t=", "a="} {
		if !strings.HasPrefix(fields[i], want) {
			return "", "", "", fmt.Errorf("tag placeholder field %d is not %q*: %q", i, want, fields[i])
		}
	}
	g, t, a = fields[0][2:], fields[1][2:], fields[2][2:]
	gVal, ok := parseTagValue(g)
	if !ok {
		return "", "", "", fmt.Errorf("tag placeholder g= value is not a non-negative integer: %q", g)
	}
	tVal, ok := parseTagValue(t)
	if !ok {
		return "", "", "", fmt.Errorf("tag placeholder t= value is not a non-negative integer: %q", t)
	}
	if gVal > 255 || tVal > 65535 {
		return "", "", "", fmt.Errorf("tag placeholder group %s or type %s out of byte range (u8/u16)", g, t)
	}
	return g, t, a, nil
}

// parseTagValue parses a non-negative decimal value, rejecting anything
// absurd (the field widths are checked by the caller).
func parseTagValue(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n := 0
	for i := 0; i < len(v); i++ {
		if v[i] < '0' || v[i] > '9' {
			return 0, false
		}
		n = n*10 + int(v[i]-'0')
		if n > 1<<20 { // far past any field width: reject early
			return 0, false
		}
	}
	return n, true
}

// parseHex2 parses exactly two lowercase hex digits.
func parseHex2(s string) (int, error) {
	n := 0
	for i := 0; i < 2; i++ {
		c := s[i]
		var d int
		switch {
		case c >= '0' && c <= '9':
			d = int(c - '0')
		case c >= 'a' && c <= 'f':
			d = int(c-'a') + 10
		default:
			return 0, fmt.Errorf("%q is not lowercase hex", s)
		}
		n = n*16 + d
	}
	return n, nil
}
