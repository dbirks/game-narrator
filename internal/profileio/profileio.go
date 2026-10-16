// Package profileio makes a profile revision portable: it imports a
// user-supplied script through an adapter into a draft revision, exports a
// revision as a versioned `.gnprofile.json` manifest, and re-imports such a
// manifest into another database (docs/spec/02-data-and-storage.md "Profile
// portability", docs/spec/17-example-contracts.md "Profile manifest",
// docs/spec/16 Milestone 1 acceptance).
//
// Three rules shape everything here.
//
//  1. Copyrighted bytes never travel by default. The manifest carries source
//     *pointers* — type, SHA-256, rights state, parser, URI, importer notes
//     (docs/spec/14 "Source material / intellectual-property policy" asks for
//     exactly that list) — and text withheld from any source whose rights
//     position is not settled. Only an explicit, acknowledged bundle (`.gnpack`,
//     a tar of the manifest plus the blobs it names) ever carries the bytes
//     themselves, and it is never produced implicitly.
//  2. Nothing holds a whole import in memory. The upload is streamed into the
//     asset store while it is hashed, and the adapter parses from the committed
//     blob, so peak memory is a copy buffer plus the rows the adapter returns
//     (docs/spec/16: "search 10k synthetic messages without memory blowup").
//  3. A re-import is a new draft, never an overwrite. Ids are minted again and
//     every internal reference is remapped — the same contract Store.NewRevision
//     applies inside one database (ADR-0015) — and the manifest's own digest is
//     computed over a canonical projection that survives that relabeling. That
//     is what lets `verify → re-import → verify` prove the two revisions hold
//     the same content instead of merely the same number of rows.
//
// The package deliberately depends on nothing beyond internal/store,
// internal/assets and internal/games/adapter: it is where those three meet, and
// it owns no state of its own.
package profileio

import (
	"errors"

	"github.com/dbirks/game-narrator/internal/games"
)

// SchemaVersion is the manifest compatibility version this package writes
// (`.gnprofile.json` carries it as schema_version) and the only one it reads.
// A reader that meets a higher value refuses rather than ignoring fields it
// does not know (the rule adapter.Manifest documents for ParseResult).
const SchemaVersion = "1.0"

// MaxManifestBytes bounds a manifest document accepted by ImportManifest. It is
// a hard ceiling, not a guess at a real profile's size: a 10 000-message export
// is a few megabytes, and anything past 256 MiB is either a mistake or an
// attempt to exhaust the appliance's memory (docs/spec/14 "upload sizes").
const MaxManifestBytes = 256 << 20

// ImportBatchSize is how many messages one PutMessages call carries. Batches
// keep a long import's transactions short enough that a concurrent editor's
// If-Match token expires once per batch rather than once per row, and small
// enough that the statement's parameter count stays bounded (docs/spec/02
// "Transaction workflows", ADR-0015's one-bump-per-transaction rule).
const ImportBatchSize = 500

// Errors this package returns. Callers test them with errors.Is.
var (
	// ErrInvalid reports a malformed request or a manifest that cannot be
	// read at all (not JSON, unknown schema_version, dangling references).
	ErrInvalid = errors.New("profileio: invalid")

	// ErrNotDraft means ImportFile was pointed at a revision that is frozen.
	// Content only ever enters a draft (docs/spec/02 "Transaction workflows").
	ErrNotDraft = errors.New("profileio: target profile is not a draft")

	// ErrNoAdapter means the registry recognized nothing in the upload, or the
	// requested format is not implemented by any registered adapter.
	ErrNoAdapter = errors.New("profileio: no adapter can read this source")

	// ErrSchemaValidation means a manifest document does not satisfy the
	// embedded JSON Schema. The error lists the failing JSON pointers.
	ErrSchemaValidation = errors.New("profileio: manifest does not satisfy the schema")

	// ErrDigestMismatch means manifest_digest does not match a recomputation
	// over the document. Import refuses: docs/spec/02 requires hash
	// verification at import, and a manifest that does not hash to its own
	// digest has been edited or truncated.
	ErrDigestMismatch = errors.New("profileio: manifest digest mismatch")

	// ErrIncompatibleEdition means the manifest's edition does not match the
	// target edition's language or region. Force overrides it; the default is
	// to refuse, because narrating a JP script with a US region's regions and
	// voices is exactly the "source cross-edition mixup" docs/spec/12 lists.
	ErrIncompatibleEdition = errors.New("profileio: edition is incompatible with the target")

	// ErrCopyrightAcknowledgement means IncludeSourceBytes was asked for
	// without AcknowledgeCopyright. There is no default and no config that
	// substitutes for the acknowledgement (docs/spec/14).
	ErrCopyrightAcknowledgement = errors.New("profileio: exporting source bytes requires an explicit copyright acknowledgement")

	// ErrBlobMissing names a source whose bytes are not in the asset store, so
	// a bundle cannot be written or a hash cannot be re-verified.
	ErrBlobMissing = errors.New("profileio: source blob is missing from the asset store")

	// ErrDanglingRef reports a manifest-internal reference to a row the same
	// manifest does not define. Checked before any write, because SQLite would
	// only notice halfway through a transaction and report a constraint.
	ErrDanglingRef = errors.New("profileio: manifest references a row it does not contain")
)

// rightsWithheld is the value of a redacted message's redacted_reason. It is a
// code rather than prose so a UI can translate it and a test can assert it.
const rightsWithheld = "rights_unknown"

// textFieldsPublishable reports whether a source's rights position allows its
// text to leave the appliance in a manifest. games.RightsStatus.Publishable is
// the same predicate the publication gate uses, so "may I ship this" and "may I
// speak this" cannot disagree (docs/spec/14).
func textFieldsPublishable(r games.RightsStatus) bool { return r.Publishable() }
