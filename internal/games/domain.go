// Package games holds the domain types behind the profile content tables
// (docs/spec/02-data-and-storage.md "Entity catalog"): games, editions, profile
// revisions, sources, the dialogue graph, and the labels curators put on it.
//
// Two rules govern this package.
//
// 1. Every type mirrors a table, and Validate mirrors that table's CHECK and
// NOT NULL constraints, so the service rejects bad input with a field name and
// a reason instead of returning an opaque "CHECK constraint failed" from inside
// a transaction. The schema keeps its constraints as the backstop: agents,
// later migrations and hand-edited imports write rows that never came through
// here (AGENTS.md "Keep runtime predictable").
//
// 2. Nothing in this package knows about Wind Waker. docs/spec/04's portability
// rule forbids one game's vocabulary leaking into core types, which is why
// roles, flags and condition expressions stay open data while content kinds and
// lifetimes are enums.
package games

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrValidation groups everything Validate returns, so a caller can tell "the
// request was malformed" (an 18-year-old 400) apart from a storage or provider
// failure (docs/spec/08's RFC 9457 problem objects).
var ErrValidation = errors.New("games: invalid")

// TimestampFormat is the RFC 3339 form the schema stores timestamps in. Every
// timestamp column carries a strftime CHECK; Go validates the same shape and is
// stricter, because SQLite also accepts a bare date as a convenience for
// hand-written fixtures, and "0001-01-01" would then read back as a real instant.
const TimestampFormat = time.RFC3339

// MaxJSONDepth bounds payload nesting. These columns are fed by model output
// and scraped pages, and a deep document is a cheap way to grow the stack
// (docs/spec/14 "Model output is data"). The limit is generous for real markup
// ASTs and unreachable by anything a human would author.
const MaxJSONDepth = 32

// MaxSHA256Hex is the length of a lowercase hex SHA-256 digest, the asset and
// content hash both source_blobs and the content tables store.
const MaxSHA256Hex = 64

// validation combines per-field complaints into one error. Every problem is
// reported rather than only the first, and errors.Is(result, ErrValidation)
// still holds because each part wraps it.
func validation(problems ...error) error {
	var kept []error
	for _, err := range problems {
		if err != nil {
			kept = append(kept, err)
		}
	}
	if len(kept) == 0 {
		return nil
	}
	return errors.Join(kept...)
}

// invalid reports one field problem as an ErrValidation-wrapping error.
func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrValidation, field, fmt.Sprintf(format, args...))
}

// requireID checks a mandatory public identifier.
func requireID(field, id string) error {
	if id == "" {
		return invalid(field, "is required")
	}
	if !ValidID(id) {
		return invalid(field, "%q is not a 26-character ULID", id)
	}
	return nil
}

// requireRef checks a foreign-key column: either absent or a well-formed id.
func requireRef(field, id string) error {
	if id == "" {
		return nil
	}
	if !ValidID(id) {
		return invalid(field, "%q is not a 26-character ULID", id)
	}
	return nil
}

// requireTimestamp checks a mandatory timestamp.
func requireTimestamp(field, value string) error {
	if value == "" {
		return invalid(field, "is required")
	}
	if _, err := time.Parse(TimestampFormat, value); err != nil {
		return invalid(field, "%q is not an RFC 3339 timestamp with an offset", value)
	}
	return nil
}

// requireOptionalTimestamp allows absence in a nullable column.
func requireOptionalTimestamp(field, value string) error {
	if value == "" {
		return nil
	}
	return requireTimestamp(field, value)
}

// requireFraction checks the [0,1] interval the schema's confidence CHECKs use.
func requireFraction(field string, v float64) error {
	if v < 0 || v > 1 {
		return invalid(field, "%v is outside [0,1]", v)
	}
	return nil
}

// requireRevision checks a counter that starts at 1 (docs/spec/08 optimistic
// locking: ETag/If-Match values are these numbers).
func requireRevision(field string, v int) error {
	if v < 1 {
		return invalid(field, "%d is below 1", v)
	}
	return nil
}

// requireJSONArray checks a JSON array column: required, valid, an array.
func requireJSONArray(field string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return invalid(field, "is required (use [] rather than null or nothing)")
	}
	if !json.Valid(raw) {
		return invalid(field, "is not valid JSON")
	}
	if err := requireDepth(field, raw); err != nil {
		return err
	}
	if trimmed := strings.TrimSpace(string(raw)); !strings.HasPrefix(trimmed, "[") {
		return invalid(field, "must be a JSON array, got %s", jsonKindName(trimmed))
	}
	return nil
}

// requireJSONObject checks a JSON object column: required, valid, an object.
func requireJSONObject(field string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return invalid(field, "is required (use {} rather than null or nothing)")
	}
	if !json.Valid(raw) {
		return invalid(field, "is not valid JSON")
	}
	if err := requireDepth(field, raw); err != nil {
		return err
	}
	if trimmed := strings.TrimSpace(string(raw)); !strings.HasPrefix(trimmed, "{") {
		return invalid(field, "must be a JSON object, got %s", jsonKindName(trimmed))
	}
	return nil
}

// RequireJSON is the exported form of the shape check, for callers that validate
// a payload (an import file, an agent tool argument) before building a row.
func RequireJSON(field string, raw json.RawMessage) error {
	if len(raw) == 0 {
		return invalid(field, "is required")
	}
	if !json.Valid(raw) {
		return invalid(field, "is not valid JSON")
	}
	return requireDepth(field, raw)
}

func requireDepth(field string, raw []byte) error {
	if d := jsonDepth(raw); d > MaxJSONDepth {
		return invalid(field, "nests %d levels deep, the limit is %d", d, MaxJSONDepth)
	}
	return nil
}

// jsonKindName names a JSON value for an error message.
func jsonKindName(s string) string {
	switch {
	case s == "":
		return "nothing"
	case strings.HasPrefix(s, `"`):
		return "a string"
	case s == "null":
		return "null"
	case s == "true" || s == "false":
		return "a boolean"
	case strings.HasPrefix(s, "["):
		return "an array"
	case strings.HasPrefix(s, "{"):
		return "an object"
	}
	return "a number"
}

// jsonDepth counts bracket nesting outside strings. It is deliberately crude:
// json.Valid does the real parse, this only rejects a document whose nesting
// would recurse further than the allowlist of shapes can justify.
func jsonDepth(raw []byte) int {
	var (
		depth, max int
		inString   bool
		escaped    bool
	)
	for _, c := range raw {
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
			if depth > max {
				max = depth
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return max
}

// requireHash checks the "64 lowercase hex characters or empty" rule the schema
// applies to content and asset references.
func requireHash(field, hash string, required bool) error {
	if hash == "" {
		if required {
			return invalid(field, "is required")
		}
		return nil
	}
	if len(hash) != MaxSHA256Hex {
		return invalid(field, "%q is %d characters, a SHA-256 digest is %d", hash, len(hash), MaxSHA256Hex)
	}
	for i := 0; i < len(hash); i++ {
		c := hash[i]
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') {
			continue
		}
		return invalid(field, "%q is not lowercase hex", hash)
	}
	return nil
}
