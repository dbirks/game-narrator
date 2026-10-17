// Package assets implements the Game Narrator content-addressed asset store
// (docs/spec/02-data-and-storage.md "Asset store contract"): large images, WAVs
// and imported source files live as SHA-256 named blobs on disk and are never
// embedded in SQLite (docs/spec/02 "SQLite schema principles"), which stores
// only the hash, size and media type.
//
// The local backend is written against two requirements at once: durability and
// containment. Durability — staged writes stream to a temporary file with
// fsync, Commit renames into place, and leftovers from a crash are discarded by
// RecoverStaging (docs/spec/02 "Transaction workflows"). Containment — the
// importer path is untrusted input (docs/spec/14 "Filesystem/import"), so every
// hash and staging ID is validated before the filesystem is touched, traversal
// outside the root is impossible by construction, symlinks are refused at every
// path component, and file modes stay private (directories 0700, files 0600).
//
// An S3-compatible backend can be added behind the same Store interface later
// without changing callers (docs/spec/02 "Asset store contract").
package assets

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// HashHexLen is the length of a lowercase hexadecimal SHA-256 digest.
const HashHexLen = 64

// StagingID identifies a staged (not yet committed) write returned by
// PutStaged. It is opaque: callers pass it back to Commit or Abort.
type StagingID string

// Info describes one blob. MediaType comes from content sniffing
// (http.DetectContentType), never from a name or a caller-supplied header.
type Info struct {
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}

// Store is the asset store contract from docs/spec/02. A blob is content
// addressed: the same bytes always produce the same Info and occupy one entry,
// which is what makes import resume and audio-cache deduplication safe.
type Store interface {
	// PutStaged streams content into staging while hashing it. Nothing becomes
	// visible to readers until Commit. If expectedSHA256 is non-empty and the
	// content hashes differently, ErrHashMismatch is returned and no bytes are
	// kept. If a size limit is configured and exceeded, ErrTooLarge is
	// returned and the staged bytes are discarded.
	PutStaged(ctx context.Context, r io.Reader, expectedSHA256 string) (StagingID, Info, error)

	// Commit makes a staged entry visible under its content hash. Committing a
	// hash that is already present is idempotent: the staging copy is discarded
	// and the existing Info is returned.
	Commit(ctx context.Context, id StagingID) (Info, error)

	// Abort discards a staged entry. Aborting an already-discarded entry is
	// not an error.
	Abort(ctx context.Context, id StagingID) error

	// Get opens a committed blob for reading. ErrNotFound if absent.
	Get(ctx context.Context, hash string) (io.ReadCloser, Info, error)

	// Stat returns a blob's metadata without reading its body.
	Stat(ctx context.Context, hash string) (Info, error)

	// Verify re-hashes a committed blob and returns ErrCorrupt if the bytes no
	// longer match the address ("verify on read").
	Verify(ctx context.Context, hash string) error

	// Export writes a tar stream containing each requested blob as
	// blobs/<hash> plus a manifest.json. Every reference is resolved before any
	// byte is written, so a missing ref leaves the writer untouched.
	Export(ctx context.Context, refs []string, w io.Writer) error

	// DeleteUnreachable garbage-collects blobs the reachability predicate
	// rejects (docs/spec/02: "reference counts are advisory, GC via
	// reachability"). With dryRun it only reports what it would delete.
	DeleteUnreachable(ctx context.Context, reachable func(hash string) bool, dryRun bool) ([]string, error)

	// Delete removes one committed blob and nothing else. It is the primitive
	// for targeted reclamation (for example evicting a transient audio-cache
	// entry to make room on a full disk, docs/spec/13 "Disk full"). The store
	// does not know the catalogue, so reachability is the caller's contract —
	// exactly as with DeleteUnreachable's predicate: never call it on a hash a
	// voice reference or imported source still names. A missing hash is
	// ErrNotFound, not an error to hide.
	Delete(ctx context.Context, hash string) error

	// RecoverStaging discards staging leftovers from interrupted writes and
	// reports how many files it removed.
	RecoverStaging(ctx context.Context) (discarded int, err error)
}

// Sentinel errors. Callers switch on these with errors.Is; each is wrapped in
// context (path, hash, expected/actual) so a log line explains itself.
var (
	// ErrNotFound reports no blob (or staging entry) at the address.
	ErrNotFound = errors.New("assets: not found")
	// ErrInvalidHash reports a hash argument that is not exactly 64 lowercase
	// hex characters. It is returned before any filesystem access, because a
	// hash doubles as a path component.
	ErrInvalidHash = errors.New("assets: invalid SHA-256 hash")
	// ErrHashMismatch reports that content did not hash to the expected value;
	// nothing was kept.
	ErrHashMismatch = errors.New("assets: content hash mismatch")
	// ErrTooLarge reports content exceeding the configured maximum size; the
	// staged write was aborted.
	ErrTooLarge = errors.New("assets: content exceeds size limit")
	// ErrCorrupt reports a committed blob whose bytes no longer hash to its
	// address, or staging content that changed between PutStaged and Commit.
	ErrCorrupt = errors.New("assets: blob does not match its address")
	// ErrUnsafePath reports a symlink, a non-regular file, or a path outside
	// the root. Nothing is read or written through such a path
	// (docs/spec/14 "Filesystem/import").
	ErrUnsafePath = errors.New("assets: refusing unsafe path")
	// ErrInvalidStagingID reports a staging ID that could not have come from
	// PutStaged.
	ErrInvalidStagingID = errors.New("assets: invalid staging ID")
)

// ValidHash reports whether s is exactly 64 lowercase hex characters. Exported
// so callers can validate database rows before handing hashes to a Store.
func ValidHash(s string) bool {
	return len(s) == HashHexLen && isLowerHex(s)
}

// isLowerHex reports whether s is non-empty and consists only of [0-9a-f].
func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return s != ""
}

// newStagingID mints an unpredictable staging identifier. Randomness (not a
// counter or the content hash) keeps concurrent importers from colliding and
// stops a staged blob's address from being guessable before Commit.
func newStagingID() (StagingID, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("assets: generate staging ID: %w", err)
	}
	return StagingID(hex.EncodeToString(raw[:])), nil
}

// normalizeHash validates a caller-supplied hash. It deliberately does not
// uppercase, trim, or otherwise repair the input: a hash is an address, and
// silently normalizing one would let two spellings name the same file while
// the catalogue disagrees with the filesystem.
func normalizeHash(hash string) (string, error) {
	if !ValidHash(hash) {
		return "", fmt.Errorf("%w: %q (want %d lowercase hex characters)", ErrInvalidHash, redactHash(hash), HashHexLen)
	}
	return hash, nil
}

// redactHash keeps an untrusted hash argument from smuggling control
// characters or long payloads into an error message (which ends up in logs).
func redactHash(s string) string {
	if len(s) > 80 {
		return fmt.Sprintf("%q...", s[:80])
	}
	return strconv.Quote(s)
}

// stagingIDHexLen is the hex length of the random staging identifier.
const stagingIDHexLen = 2 * 16

// validateStagingID restricts staging IDs to the exact shape PutStaged issues.
// IDs become path components, so anything else — "../", absolute paths, empty
// names — is rejected outright.
func validateStagingID(id StagingID) error {
	s := string(id)
	if len(s) != stagingIDHexLen || !isLowerHex(s) {
		return fmt.Errorf("%w: %q (want %d lowercase hex characters)", ErrInvalidStagingID, redactHash(s), stagingIDHexLen)
	}
	return nil
}
