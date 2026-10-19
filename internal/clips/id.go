// Package clips manages the appliance's stored replay clips (ADR-0073):
// an operator uploads a tar/zip of a replay layout (timestamps.json plus the
// frame files, the same shape internal/capture/replay reads), and the clip is
// installed under a managed directory inside the data directory that the
// session runtime always treats as a replay root.
//
// Clips are user content (AGENTS.md rule 5): the provenance of every installed
// clip is the content hash of the uploaded archive, recorded in the clip index
// and committed to the asset store — the extracted paths themselves never leave
// the managed directory, and nothing here logs an absolute clip path.
//
// A clip id is the only client-controlled path component on this surface, so
// it is one conservative slug (lowercase alphanumerics plus dot, dash and
// underscore) with no separators: an id that cannot contain a path separator
// cannot traverse, and the id check plus the allowlist check are the two
// independent layers that make that true (docs/spec/14 "Filesystem").
package clips

import (
	"errors"
	"regexp"
	"strings"
	"unicode"
)

// MaxIDLen bounds a clip id: long enough for a descriptive slug, short enough
// that an id never dominates a path or a log line.
const MaxIDLen = 64

// idPattern is the whole id contract: one lowercase slug. It is checked before
// any filesystem access, so a request cannot even name a directory outside the
// managed store.
var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// ValidID reports whether id is a usable clip id.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// ErrInvalidName is a caller-supplied name that yields no usable id.
var ErrInvalidName = errors.New("clips: the name does not yield a usable id")

// NormalizeName turns a caller-supplied display name into an id: lowercase,
// ASCII letters and digits only, runs of separators collapsed to one dash,
// every other character dropped. The result is a ValidID or ErrInvalidName —
// a name of only punctuation cannot name a clip, and the caller must say so
// rather than get a generated id it did not choose.
func NormalizeName(name string) (string, error) {
	var b strings.Builder
	prevSep := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevSep = false
		case r == '.' || r == '_' || r == '-' || unicode.IsSpace(r):
			if !prevSep && b.Len() > 0 {
				b.WriteByte('-')
				prevSep = true
			}
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		return "", ErrInvalidName
	}
	if len(out) > MaxIDLen {
		out = strings.TrimRight(out[:MaxIDLen], "-.")
		if out == "" {
			return "", ErrInvalidName
		}
	}
	if !ValidID(out) {
		return "", ErrInvalidName
	}
	return out, nil
}
