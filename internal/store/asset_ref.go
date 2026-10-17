package store

// Blob reference checking for eviction safety (ADR-0041 "Reference check
// before delete").
//
// Assets are content-addressed and shared: the same SHA-256 may name a blob
// the speech cache generated AND a blob some catalogue row still names (an
// approved voice reference, an audition clip, a character portrait, an
// imported source). An eviction path may only delete a blob it just stopped
// indexing if no such row exists; this file is the catalogue-side half of that
// proof (the cache side is speech.Cache's reference checker).

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/dbirks/game-narrator/internal/assets"
)

// *Store is the production ReferenceChecker the speech cache takes as an
// eviction-safety dependency (ADR-0041): the interface and the implementation
// live in different packages, so pin the satisfaction here, at compile time.
var _ assets.ReferenceChecker = (*Store)(nil)

// assetReferenceQuery is one UNION over every table column that names a
// committed blob in the content-addressed asset store OUTSIDE the speech cache
// index. All five branches are point lookups on indexed columns:
// source_blobs.sha256 is the primary key and sources.content_sha256 carries
// sources_content_sha256_idx (both from migration 0001); the other three
// indexes come from migration 0010. The check is therefore five probes, not
// five scans, on a path the disk-full recovery runs under time pressure.
//
// Columns that mention hashes only inside JSON (characters.visual_descriptors,
// scenes.visual_anchors, regions.mask_asset_ids, messages.attribution_evidence)
// are evidence pointers — "refer by hash, verify on read" (migration 0002) —
// not ownership references, and are deliberately not covered.
const assetReferenceQuery = `
	SELECT 1 FROM source_blobs    WHERE sha256 = ?
	UNION
	SELECT 1 FROM sources         WHERE content_sha256 = ?
	UNION
	SELECT 1 FROM characters      WHERE portrait_asset_id = ?
	UNION
	SELECT 1 FROM voice_profiles  WHERE canonical_reference_asset_id = ?
	UNION
	SELECT 1 FROM voice_auditions WHERE asset_sha256 = ?
	LIMIT 1`

// AssetReferenced reports whether any catalogue row other than the speech cache
// index names the committed blob under the given hash. It is the reachability
// check an eviction path must run before assets.Store.Delete: a true answer
// means the bytes are shared with the catalogue (voice reference, audition,
// portrait, source) and must be kept even though the cache's own entry is
// dropped.
//
// An empty or malformed hash is a caller bug, not a "no references": answering
// false there would let the caller delete a blob it cannot even address, so
// the method errors instead of guessing. The store package owns this decision
// rather than the assets package because the answer is a catalogue fact.
func (s *Store) AssetReferenced(ctx context.Context, sha256 string) (bool, error) {
	const op = "asset referenced"
	if !assets.ValidHash(sha256) {
		return false, fmt.Errorf("store: %s: %s is not a 64-character lowercase hex SHA-256",
			op, redactedHash(sha256))
	}
	var ref int
	err := s.inRead(ctx, func(ctx context.Context, q queryable) error {
		row := q.queryRow(ctx, op, assetReferenceQuery, sha256, sha256, sha256, sha256, sha256)
		return row.Scan(&ref)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // no row in any referencing table names this hash
	}
	if err != nil {
		return false, mapReadError(op, err)
	}
	return true, nil
}

// redactedHash keeps an untrusted hash argument from smuggling a long payload
// into an error message (which ends up in logs): keep a bounded prefix.
func redactedHash(s string) string {
	if len(s) <= 16 {
		return fmt.Sprintf("%q", s)
	}
	return fmt.Sprintf("%q…", s[:16])
}
