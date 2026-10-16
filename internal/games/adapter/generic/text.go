package generic

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

// readText reads a plain-text script.
//
// BlankLineSplitsPages changes what a blank line means, and the difference is the
// whole reason the option exists:
//
//   - off (the default): one message per non-empty line, and a blank line is a
//     paragraph break that carries no content. This is a list of lines.
//   - on: consecutive non-empty lines belong to *one* message, because a wrapped
//     textbox script breaks a sentence wherever the box ends a line. A single
//     blank line ends one page of that message and starts the next; a run of two
//     or more blank lines ends the message. This is docs/spec/02's rule that a
//     message may have several pages and a page is not an utterance.
//
// Text stays byte-faithful: only the line terminator is removed, so a line with
// trailing spaces stores them. Removing a trailing CR handles CRLF files, where
// the CR is part of the terminator and not content.
func (b *builder) readText(br *bufio.Reader) error {
	var (
		line   int
		block  []string // lines of the page being read
		pages  []string // finished pages of the message being read
		start  int      // line the message being read started on
		blanks int      // consecutive blank lines since the last content line
	)

	closeBlock := func() {
		if len(block) == 0 {
			return
		}
		pages = append(pages, strings.Join(block, "\n"))
		block = nil
	}
	flush := func() error {
		closeBlock()
		if len(pages) == 0 {
			return nil
		}
		owned := pages
		pages, start, blanks = nil, 0, 0
		return b.add(sourceRow{line: start, pages: owned})
	}

	for {
		if err := b.contextIfDone(); err != nil {
			return err
		}
		text, err := readCappedLine(br)
		if errors.Is(err, errLineTooLong{}) {
			// readCappedLine already drained the rest of the line, so the scan
			// continues at the next one: a single runaway line in a text file is
			// a skipped row, not a lost file.
			line++
			b.skipRow(line, "line", "%v; the rest of the line was skipped", err)
			continue
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		atEOF := errors.Is(err, io.EOF)
		if text == "" && atEOF {
			return flush()
		}
		line++

		if !b.opts.BlankLineSplitsPages {
			if isSpace(text) {
				continue
			}
			if err := b.add(sourceRow{line: line, pages: []string{text}}); err != nil {
				return err
			}
			if atEOF {
				return nil
			}
			continue
		}

		if isSpace(text) {
			if len(block) == 0 && len(pages) == 0 {
				continue // blank lines before a message starts
			}
			closeBlock()
			blanks++
			if blanks >= 2 {
				if err := flush(); err != nil {
					return err
				}
			}
			if atEOF {
				return nil
			}
			continue
		}

		blanks = 0
		if len(block) == 0 && len(pages) == 0 {
			start = line
		}
		block = append(block, text)
		if atEOF {
			return flush()
		}
	}
}

//
// ── capped line reading ─────────────────────────────────────────────────────
//

// errLineTooLong reports a physical line over maxLineBytes. It is a value type so
// errors.Is can match it without a pointer identity.
type errLineTooLong struct{}

func (errLineTooLong) Error() string {
	return "line is longer than the limit for one record"
}

// readCappedLine reads one physical line, refusing one longer than maxLineBytes.
//
// The cap is applied while accumulating rather than after, because the whole
// point is that an over-long line — a minified JSON document named .jsonl, a text
// file with its newlines stripped — must not be buffered first and then rejected.
// When the cap is hit the remainder of the line is drained and the error
// returned, so a caller that skips the row resumes at the next line.
//
// bufio.Reader.ReadSlice is used instead of ReadBytes for the same reason: it
// returns a slice of its own buffer and never copies the rejected bytes.
func readCappedLine(br *bufio.Reader) (string, error) {
	var out []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if int64(len(out))+int64(len(chunk)) > maxLineBytes {
			drainLine(br, err == bufio.ErrBufferFull)
			return "", errLineTooLong{}
		}
		out = append(out, chunk...)
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				s := trimTerminator(string(out))
				if s == "" {
					return "", io.EOF
				}
				return s, io.EOF
			}
			return "", err
		}
		return trimTerminator(string(out)), nil
	}
}

// drainLine consumes input through the end of the current line. more is set when
// the last read stopped at a buffer boundary rather than at a newline.
func drainLine(br *bufio.Reader, more bool) {
	for more {
		_, err := br.ReadSlice('\n')
		if err != bufio.ErrBufferFull {
			return
		}
	}
}

// trimTerminator removes one line terminator, and only one: trailing whitespace
// that is not a terminator is content the source meant to have.
func trimTerminator(s string) string {
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}
