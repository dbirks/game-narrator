// Package generic is the fallback adapter docs/spec/04 "Generic adapter minimum"
// asks for: it reads a user's CSV, TSV, JSON, JSON Lines or plain-text script,
// maps their column names onto the domain's roles, and produces validated
// games.Message values.
//
// It contains no game's vocabulary. Nothing here may branch on a title, an
// engine, or a language: docs/spec/04's portability rule says a second game is
// supported by writing an adapter or a profile, not by adding a condition to
// shared code, and the two-game test in portability_test.go is what enforces
// that in practice. Where a source's own structure is ambiguous, this package
// records a warning and keeps the data rather than guessing: an unknown speaker
// stays unknown, an unknown content class becomes TextOther, a wrong column name
// becomes a warning that names the columns it did see.
//
// Every limit listed in adapter.ParseOptions is enforced by refusing the import
// outright rather than by truncating it (docs/spec/14 "Filesystem/import":
// "max decompressed size/ratio/file count, memory/time limits"). Half a script
// indexed as a whole script is worse than no script: every later "not found"
// answer would be confidently wrong.
package generic

import (
	"io"
	"unicode"

	"github.com/dbirks/game-narrator/internal/games/adapter"
)

// Errors this package returns from Parse. They alias the shared ones so a caller
// can switch on adapter.ErrTooLarge without importing this package's names, and so
// errors.Is works from either side.
var (
	ErrTooLarge    = adapter.ErrTooLarge
	ErrTooManyRows = adapter.ErrTooManyRows
)

// Hard caps on one record. maxFieldBytes is what a single source field may hold
// before the row is skipped: a textbox line is a few hundred bytes, so anything
// at 64 KiB is a parsing mistake (an unquoted block of the next 200 lines) and
// continuing would grow the index with junk. maxTextRunes mirrors the 8000
// character limit games.Message.Validate enforces on original_text — it is the
// storage layer's number, not this package's, and the comment at
// internal/games/models.go is the authority.
const (
	maxFieldBytes = 64 << 10
	maxTextRunes  = 8000
	// maxLineBytes bounds one physical line for the line-oriented readers (txt,
	// jsonl). It is maxFieldBytes plus room for JSON punctuation around it.
	maxLineBytes = 96 << 10
	// maxWarnings bounds the returned warning list, not the number of defects: a
	// file that is wrong in 200,000 identical ways gets 500 examples and a
	// "further warnings suppressed" line, so the result stays small enough to
	// hand to a browser (docs/spec/14 "memory/time limits").
	maxWarnings = 500
	// peekBytes is how much of the head of a file Detect and format inference may
	// look at. A header row and the first few records are enough to tell csv from
	// json from txt; anything more is guessing at the wrong question.
	peekBytes = 4 << 10
)

// sizeLimiter bounds how much of the source any reader may consume.
//
// It is deliberately not io.LimitReader: that silently ends the stream at the
// cap, which is precisely the truncated import this package must never produce.
// Once the budget is spent, Read returns ErrTooLarge and every reader above it
// unwinds with it.
type sizeLimiter struct {
	r        io.Reader
	left     int64 // bytes still permitted, including one probe byte
	overflow bool
}

// newSizeLimiter allows maxBytes and one byte more, so that a source of exactly
// maxBytes succeeds and maxBytes+1 does not.
func newSizeLimiter(r io.Reader, maxBytes int64) *sizeLimiter {
	return &sizeLimiter{r: r, left: maxBytes + 1}
}

func (l *sizeLimiter) Read(p []byte) (int, error) {
	if l.overflow || l.left <= 0 {
		l.overflow = true
		return 0, ErrTooLarge
	}
	if len(p) == 0 {
		return 0, nil
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	if l.left <= 0 && n > 0 {
		// The budget is gone. If the source really did end here the caller's
		// next read gets EOF from underneath; if it did not, this read delivered
		// maxBytes+1 bytes and the import is refused. The distinction is made by
		// the caller seeing io.EOF versus this error, never by guessing.
		l.overflow = true
		return n, nil
	}
	return n, err
}

// contextIfDone is called between records by every reader. An import is a
// request, and a request whose client went away must stop reading (docs/spec/05
// "Deadline/cancellation propagation"). Checking per record rather than per file
// is what makes the difference observable for a 200 000-row upload.
func (b *builder) contextIfDone() error { return b.ctx.Err() }

// isSpace reports whether every rune of s is whitespace, without allocating the
// trim that strings.TrimSpace would produce.
func isSpace(s string) bool {
	for _, r := range s {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}
