package windwaker

// The adapter implementation: manifest, detection, and the two parse entry
// points (raw .bmg bytes, or a disc image whose BMG is extracted through
// gcdisc). The message mapping itself lives in parse.go; this file is the
// adapter.Adapter surface.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dbirks/game-narrator/internal/games/adapter"
	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// Adapter identity. ID is what lands in sources.parser; Version is what a
// re-import compares against to find a stale cached revision.
const (
	// ID is the registered name, frozen like any parser id (ADR-0017's rule).
	ID = "windwaker-bmg"

	// Version is the adapter semver. It bumps with RulesRevision changes and
	// any mapping change, so "which adapter produced these rows" and "is the
	// audio cache stale" are both answerable from the stored strings.
	Version = "1.0.0"

	// SchemaVersion is the ParseResult shape (1 = base shape; the rules
	// revision is carried in Warnings, column "rules").
	SchemaVersion = 1
)

// Source formats Parse accepts (Manifest.SupportedFormats values).
const (
	FormatGCM  = "gcm" // plain GameCube disc image
	FormatCISO = "ciso"
	FormatBMG  = "bmg" // raw extracted BMG
)

// Adapter parses Wind Waker BMG sources. It carries no state: one value
// serves every import, which is what makes it safe to hand out of a Registry
// to concurrent requests.
type Adapter struct {
	// Rules is the speech rendering table. Exposed (rather than hidden
	// behind NewRules) so the import command can stamp the revision into
	// sources.coverage_notes and tests can assert on the exact table.
	Rules *Rules
}

var _ adapter.Adapter = Adapter{}

// New returns the adapter with the default rules table.
func New() adapter.Adapter { return Adapter{Rules: NewRules()} }

// Register adds the adapter to r.
func Register(r *adapter.Registry) error { return r.Register(New()) }

// Manifest describes the adapter (docs/spec/04 "versioned adapter manifest").
func (a Adapter) Manifest() adapter.Manifest {
	return adapter.Manifest{
		ID:            ID,
		Version:       Version,
		SchemaVersion: SchemaVersion,
		Description:   "Wind Waker BMG reference adapter: disc (gcm/ciso) or raw .bmg to canonical messages with a lossless token AST and rule-driven speech rendering.",
		SupportedFormats: []string{
			FormatGCM, FormatCISO, FormatBMG,
		},
		RequiredPermissions: []string{adapter.PermReadSource},
		Locales:             []string{"en"},
	}
}

// DiscGameIDPrefix is the disc-id prefix that identifies the user's
// Wind Waker images (game id "GZL…", e.g. GZLE01 on the USA disc). It is the
// detection signal docs/spec/04 asks for; other prefixes are refused at
// Detect time and error out in ParseDisc with a clear message rather than
// importing the wrong game's text under this adapter's rules.
const DiscGameIDPrefix = "GZL"

// Detect reports how confidently this adapter could parse the source.
//
// Content beats name: a file whose first bytes are the BMG magic is a BMG
// whatever it is called, and a plain disc image whose game id starts with
// GZL is Wind Waker whatever its extension says. For CISO containers the
// GCM header sits at file offset 0x8000, beyond the 512-byte peek, so the
// claim is name-based only (ConfidentName): ParseDisc re-checks the game id
// after opening, which is where a mislabelled container is caught.
func (a Adapter) Detect(_ context.Context, name string, head []byte) adapter.Confidence {
	// Raw BMG: the magic plus the first section being INF1 is the expected
	// section set this file was validated against (ADR-0029).
	if len(head) >= 36 && bytes.Equal(head[0:8], []byte("MESGbmg1")) {
		if bytes.Equal(head[32:36], []byte("INF1")) {
			return adapter.ConfidentExact
		}
		return adapter.ConfidentFormat
	}
	// Plain (non-containerised) disc image: the GCM boot header is at
	// offset 0, so the game id at 0x20 is readable from the head.
	if len(head) >= 0x23 && bytes.Equal(head[0x20:0x23], []byte(DiscGameIDPrefix)) {
		return adapter.ConfidentExact
	}
	// Containers (CISO) and extension-only signals: name-based confidence.
	lower := strings.ToLower(name)
	if strings.HasSuffix(lower, ".gcm") || strings.HasSuffix(lower, ".ciso") || strings.HasSuffix(lower, ".iso") {
		return adapter.ConfidentName
	}
	return adapter.ConfidentNone
}

// Parse reads a raw .bmg byte stream (a disc image is not accepted here:
// ParseDisc is the disc entry point, and the two must not quietly trade
// formats — a disc image is megabytes and would fail the size bound only
// after being buffered).
func (a Adapter) Parse(ctx context.Context, r io.Reader, opts adapter.ParseOptions) (*adapter.ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := opts.MaxBytes + 1 // one extra byte proves the limit was exceeded
	b, err := io.ReadAll(io.LimitReader(r, limit))
	if err != nil {
		return nil, fmt.Errorf("windwaker: read source: %w", err)
	}
	if int64(len(b)) > opts.MaxBytes {
		return nil, fmt.Errorf("windwaker: %w: %d bytes exceed the %d-byte limit", adapter.ErrTooLarge, len(b), opts.MaxBytes)
	}
	return a.parseBMG(ctx, b, opts)
}

// ParseDisc extracts the BMG from a disc image and parses it. The BMG path
// (res/Msg/bmgres.arc -> first .bmg entry) is the one the user's disc was
// validated against (ADR-0029); a disc without it is an error, not a guess
// about a different file. The game id is re-checked here, not just at
// Detect time: the rules table is game-specific, so a mislabelled or
// different GZL-prefixed disc must fail with a clear message rather than be
// imported under this adapter's rules.
func (a Adapter) ParseDisc(ctx context.Context, d *gcdisc.Disc, opts adapter.ParseOptions) (*adapter.ParseResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id := d.Header().GameID
	if !strings.HasPrefix(id, DiscGameIDPrefix) {
		return nil, fmt.Errorf("windwaker: disc game id %q does not start with %q; refusing to parse it with the Wind Waker adapter (its rules table is game-specific)", id, DiscGameIDPrefix)
	}
	data, err := ExtractBMG(d)
	if err != nil {
		return nil, err
	}
	return a.parseBMG(ctx, data, opts)
}

// rulesOrDefault is the table Parse uses; a zero Adapter still behaves.
func (a Adapter) rulesOrDefault() *Rules {
	if a.Rules == nil {
		return NewRules()
	}
	return a.Rules
}

// rulesWarning is the deterministic first warning of every parse result: the
// speech rendering rules revision, in the column "rules". ParseResult carries
// no free-form metadata field, and the revision must be part of the parse
// result for audio cache keys (docs/spec/02), so it rides the warnings — a
// stable, greppable marker the import command also copies into
// sources.coverage_notes.
func (a Adapter) rulesWarning() adapter.Warning {
	return adapter.Warning{
		Line:   0,
		Column: "rules",
		Msg:    fmt.Sprintf("speech rendering rules revision %s (parser %s@%s)", a.rulesOrDefault().Revision, ID, Version),
	}
}
