package assets

// Reference checking for safe eviction (ADR-0041 "Reference check before
// delete").
//
// A blob is content-addressed, which is what makes deduplication safe — and
// what makes deletion dangerous: the same SHA-256 can name a blob one caller
// just stopped indexing AND a blob another catalogue record still names (an
// approved voice reference, an audition clip, a character portrait, an
// imported source). Every caller that may Delete a blob it does not own must
// first ask a ReferenceChecker.

import "context"

// ReferenceChecker answers whether a committed blob's hash is still named by
// any record the checker knows about, outside the caller's own index.
//
// *store.Store implements it (AssetReferenced, a single UNION over every
// catalogue column that names a blob). The speech cache takes one as an
// OPTIONAL dependency: when a checker is configured, eviction removes the
// index entry but keeps the blob whenever the checker reports a reference;
// when none is configured, eviction must refuse to delete any blob at all
// (index-only) — safe by default, because deleting a blob the cache cannot
// prove unshared is how approved content gets lost.
//
// A checker error is NOT a "no reference": a caller that cannot prove the
// blob unshared must keep it and surface the error.
type ReferenceChecker interface {
	AssetReferenced(ctx context.Context, sha256 string) (bool, error)
}
