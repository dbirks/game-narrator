package bmg

import "fmt"

const tagStart = 0x1A

// maxTagLen bounds the 0x1A tag length accepted by the tag-aware
// message-end scan. Real tags in the Wind Waker pool use L in {5,6,7};
// longer declared lengths are treated as ordinary text so a hostile
// length byte cannot swallow the NUL terminator of a truncated tag.
// The tokenizer still inspects every 0x1A and keeps malformed tags as
// Invalid tokens.
const maxTagLen = 8

// unitSize returns the text unit width in bytes for the recorded
// encoding byte. 1-byte encodings (0/1, 3, 4) use 1; UTF-16BE (2)
// uses 2. Unknown encodings are treated as 1-byte to remain total.
func unitSize(encoding byte) int {
	if encoding == 2 {
		return 2
	}
	return 1
}

// messageEnd returns the end offset of the message starting at start
// within pool: the next NUL terminator that is not part of a
// well-formed 0x1A tag. Tag bodies may contain zero bytes (verified
// structurally against the real Wind Waker pool, ADR-0029), so the
// scan skips over well-formed tags. Malformed tags are not skipped;
// a following NUL still ends the message. If no such NUL exists the
// message runs to len(pool). For the 2-byte (UTF-16BE) fallback
// encoding, tag awareness is not applied.
func messageEnd(pool []byte, start, unit int) int {
	if unit == 2 {
		for i := start; i+2 <= len(pool); i += 2 {
			// big-endian 0x0000 terminator
			if pool[i] == 0 && pool[i+1] == 0 {
				return i
			}
		}
		return len(pool)
	}
	n := len(pool)
	for i := start; i < n; i++ {
		b := pool[i]
		if b == 0 {
			return i
		}
		if b == tagStart && i+1 < n {
			l := int(pool[i+1])
			if l >= 2 && l <= maxTagLen && i+l <= n {
				i += l - 1 // skip the tag body (loop adds one)
			}
		}
	}
	return n
}

// tokenize decomposes a message's raw pool slice into a lossless token
// stream. Every input byte appears in exactly one token, so
// Reconstruct always equals the raw slice. Malformed control tags are
// retained as TokInvalid tokens, never dropped.
func tokenize(encoding byte, raw []byte) []Token {
	unit := unitSize(encoding)
	var toks []Token
	run := 0 // start of current text run within raw
	n := len(raw)
	flush := func(end int) {
		if end > run {
			toks = append(toks, Token{
				Kind:   TokText,
				Offset: run,
				Length: end - run,
				Raw:    raw[run:end],
			})
		}
		run = end
	}
	for i := 0; i < n; {
		var b byte
		if unit == 2 {
			if i+2 > n {
				break // stray trailing unit; kept as text by the final flush
			}
			b = raw[i+1] // low byte of a big-endian unit
		} else {
			b = raw[i]
		}
		if b != tagStart {
			i += unit
			continue
		}
		flush(i) // pending text [run, i); afterwards run == i
		// Tag (or malformed tag) starts at i. Its end is computed
		// below; every branch retains all bytes in a token.
		var tag Token
		if i+unit >= n {
			// 0x1A with no length unit: invalid, keep the rest raw.
			tag = invalidToken(i, n, raw)
		} else {
			// In 1-byte encodings the length byte is raw[i+1]; in the
			// UTF-16BE fallback the low byte of the next unit.
			var length int
			if unit == 2 {
				length = int(raw[i+3])
			} else {
				length = int(raw[i+1])
			}
			switch {
			case length < 2:
				// L < 2: keep the 0x1A + length unit(s) as invalid.
				end := i + 2*unit
				if end > n {
					end = n
				}
				tag = invalidToken(i, end, raw)
			case i+length*unit > n:
				// Tag runs past the message end: keep everything from i
				// to the end as one invalid token.
				tag = invalidToken(i, n, raw)
			default:
				tag = parseTag(i, i+length*unit, unit, raw)
			}
		}
		toks = append(toks, tag)
		i = tag.Offset + tag.Length
		run = i
	}
	flush(n)
	return toks
}

// invalidToken builds a TokInvalid token covering raw[off:end].
func invalidToken(off, end int, raw []byte) Token {
	t := Token{Kind: TokInvalid, Offset: off, Length: end - off, Raw: raw[off:end]}
	if end-off >= 2 {
		t.Group = int(raw[off]) // 0x1A
		t.Type = int(raw[off+1])
	}
	return t
}

// parseTag decodes a well-formed tag (tagStart..tagEnd, unit-aligned)
// into its group, type, args and raw bytes.
//
// Tag layout in 1-byte encodings: 0x1A | L | body[L-2]. The body is
// typically a group byte then a u16/u8 type then arguments. The full
// body is retained as Args; Group is the first body byte; Type is the
// big-endian u16 of body[1:3] when present, else body[1] as u8.
func parseTag(off, end, unit int, raw []byte) Token {
	body := raw[off+2*unit : end]
	t := Token{Kind: TokTag, Offset: off, Length: end - off, Raw: raw[off:end]}
	t.Args = append([]byte(nil), body...)
	if len(body) >= 1 {
		t.Group = int(body[0])
	}
	switch {
	case len(body) >= 3:
		t.Type = int(beU16(body[1:3]))
	case len(body) >= 2:
		t.Type = int(body[1])
	}
	return t
}

// msgErr is the error type for per-message structural violations.
type msgErr struct {
	index int
	err   error
}

func (e *msgErr) Error() string {
	return fmt.Sprintf("bmg: message %d: %v", e.index, e.err)
}

// buildMessages assembles the File.Messages from INF1 entries over the
// DAT1 pool, validating bounds and enforcing the per-message length cap.
func buildMessages(encoding byte, pool []byte, entries []infEntry, ids []uint32) ([]Message, error) {
	unit := unitSize(encoding)
	msgs := make([]Message, len(entries))
	for i, e := range entries {
		if int(e.TextOffset) >= len(pool) {
			return nil, &msgErr{i, fmt.Errorf("text offset %d is past the DAT1 pool (%d bytes)", e.TextOffset, len(pool))}
		}
		start := int(e.TextOffset)
		end := messageEnd(pool, start, unit)
		if end-start > MaxMsgLen {
			return nil, &msgErr{i, fmt.Errorf("message length %d exceeds %d bytes", end-start, MaxMsgLen)}
		}
		raw := pool[start:end]
		id := uint32(i)
		if i < len(ids) {
			id = ids[i]
		}
		msgs[i] = Message{
			Index:      i,
			ID:         id,
			TextOffset: e.TextOffset,
			Attributes: e.Attributes,
			Raw:        raw,
			Tokens:     tokenize(encoding, raw),
		}
	}
	return msgs, nil
}
