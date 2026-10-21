// Package adapter defines the contract every game adapter implements and the
// registry that holds the ones linked into this build.
//
// docs/spec/04 sets the rule this package exists to enforce: "Never bake
// Zelda-specific assumptions into the core runtime, source format parser, actor
// resolver or React UI." Everything above this package — the store, the matching
// index, the policy engine, the UI — talks to adapters through these types, so a
// new game is a new package under adapter/ plus a profile row, never a condition
// in core code.
//
// Adapters are statically linked (docs/spec/04: "First-party adapters are
// statically linked in Go"). There is no plugin loading and no dynamic library
// path here on purpose: an executable parser plugin is a *future optional*
// capability that docs/spec/04 requires to run with limited privileges, and
// docs/spec/14 forbids automatic execution of sourced binaries. A Registry built
// by explicit Register calls in the linking package is the whole mechanism,
// which also means `game-narrator doctor` can list the adapters that actually
// exist in the running binary (AGENTS.md "no simulation theater").
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"sort"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/gcdisc"
)

// Errors every adapter may return from Parse. Callers switch on these with
// errors.Is, never on an error string, and each one maps to a distinct onboarding
// message in the UI rather than one generic "import failed".
var (
	// ErrTooLarge means the source exceeded ParseOptions.MaxBytes. It is a hard
	// failure, not a truncation: half a script indexed as a whole script would
	// make every later "not found" answer wrong (docs/spec/14 "max decompressed
	// size/ratio/file count, memory/time limits").
	ErrTooLarge = errors.New("adapter: source exceeds the size limit")

	// ErrTooManyRows means the source exceeded ParseOptions.MaxRows.
	ErrTooManyRows = errors.New("adapter: source exceeds the row limit")

	// ErrFormatNotSupported means ParseOptions.Format names a format this adapter
	// does not implement.
	ErrFormatNotSupported = errors.New("adapter: format not supported")

	// ErrInvalidOptions means ParseOptions is internally inconsistent, e.g. a
	// profile id that is not a ULID.
	ErrInvalidOptions = errors.New("adapter: invalid parse options")

	// ErrNotRegistered means a Registry.Register call collided with an existing
	// adapter id.
	ErrNotRegistered = errors.New("adapter: registration refused")

	// ErrNoMessages means a parse produced nothing at all. Adapters return it so
	// an empty upload is reported as "no messages recognised in this file" rather
	// than a successful import of zero rows, which the UI would then show as an
	// empty profile with no explanation.
	ErrNoMessages = errors.New("adapter: no messages recognised")
)

// Confidence is an adapter's self-reported certainty that it can parse a given
// source, in [0,1]. It is not a probability and is never combined arithmetically
// with one: it only orders candidate adapters for a human or a policy to choose
// from (docs/spec/04 "Adapter capability set").
type Confidence float64

// Confidence values adapters should use. Naming the bands keeps five separately
// written adapters from each inventing their own scale, which would make
// Registry.Detect ordering meaningless across them.
const (
	ConfidentExact   Confidence = 0.95 // the manifest's own marker/hash matched
	ConfidentFormat  Confidence = 0.90 // extension and content agree
	ConfidentSniffed Confidence = 0.70 // content matched, name said nothing
	ConfidentName    Confidence = 0.80 // name matched, content inconclusive
	ConfidentWeak    Confidence = 0.50 // one weak signal
	ConfidentText    Confidence = 0.30 // "it is at least text"
	ConfidentNone    Confidence = 0.00 // not this adapter
)

// Valid reports whether c is in the range Registry accepts.
func (c Confidence) Valid() bool { return c >= 0 && c <= 1 && c == c /* rejects NaN */ }

// String renders two decimal places, which is what the profile inspector shows.
func (c Confidence) String() string { return fmt.Sprintf("%.2f", float64(c)) }

// Manifest is the versioned description docs/spec/04 requires: "Implement
// interfaces and a versioned adapter manifest (semver, required permissions,
// supported game/edition hashes, supported locales, test fixtures, schema
// compatibility)."
type Manifest struct {
	// ID is the adapter's stable name, stored in sources.parser and
	// profile revisions. Changing it is a breaking change: existing rows would
	// reference an adapter that no longer exists.
	ID string `json:"id"`

	// Version is the adapter's own semver, not the schema version. An adapter
	// that changes the messages it produces bumps this so a re-import can be
	// detected and a cached revision identified as stale.
	Version string `json:"version"`

	// SchemaVersion is the compatibility version of the ParseResult shape this
	// build emits (docs/spec/04 "schema compatibility"). A reader refuses a
	// result whose SchemaVersion exceeds what it understands rather than
	// silently ignoring a field — the same rule internal/providers/recorded
	// applies to cassettes. Version 2 added Speakers/SpeakerKeys/SpeakerColumn.
	SchemaVersion int `json:"schemaVersion"`

	Description string `json:"description"`

	// SupportedFormats lists the ParseOptions.Format values Parse accepts.
	SupportedFormats []string `json:"supportedFormats"`

	// RequiredPermissions is an allowlist, not free text: an adapter that asks
	// for something outside the list fails Manifest.Validate, so "execute arbitrary
	// binaries" cannot be smuggled into a manifest (docs/spec/14).
	RequiredPermissions []string `json:"requiredPermissions"`

	// SupportedGames lists edition identities (edition id or content SHA-256)
	// this adapter is known to parse. Empty means "any source the user points
	// at", which is the generic adapter's answer.
	SupportedGames []string `json:"supportedGames,omitempty"`

	// Locales lists BCP-47 language tags the adapter has fixtures for. An
	// adapter may parse text in a language it has no fixtures for; it may not
	// claim a locale it has not tested.
	Locales []string `json:"locales,omitempty"`

	// Fixtures names the synthetic test inputs this adapter ships, so
	// `make test-replay` can prove an adapter still reads what it claimed to
	// read without access to anyone's game files (AGENTS.md "No simulation
	// theater"). Real dumps are never listed here — docs/spec/14 keeps them out
	// of the tree.
	Fixtures []string `json:"fixtures,omitempty"`
}

// Permissions an adapter may ask for. Anything else is refused: the point of the
// list is that a manifest can be reviewed by a human in one screen.
const (
	PermReadSource  = "read_source"  // read the user-selected import file
	PermWriteCache  = "write_cache"  // write derived bytes into the asset cache
	PermNetwork     = "network"      // reach a configured endpoint
	PermReadScreens = "read_screens" // read captured frames for region detection
)

var allowedPermissions = map[string]bool{
	PermReadSource:  true,
	PermWriteCache:  true,
	PermNetwork:     true,
	PermReadScreens: true,
}

// Validate checks the manifest the way games.Validate checks a row: fail with a
// field name and a reason, before an adapter is ever consulted.
func (m Manifest) Validate() error {
	var problems []string
	if !validAdapterID(m.ID) {
		problems = append(problems, fmt.Sprintf("Manifest.id %q must be lowercase letters, digits, '-', '_' or '.'", m.ID))
	}
	if _, err := ParseSemver(m.Version); err != nil {
		problems = append(problems, "Manifest.version: "+err.Error())
	}
	if m.SchemaVersion < 1 {
		problems = append(problems, fmt.Sprintf("Manifest.schemaVersion is %d, it must be at least 1", m.SchemaVersion))
	}
	if len(m.SupportedFormats) == 0 {
		problems = append(problems, "Manifest.supportedFormats is empty; an adapter that parses nothing must not be registered")
	}
	seen := map[string]bool{}
	for _, f := range m.SupportedFormats {
		if f == "" || strings.TrimSpace(f) != f {
			problems = append(problems, fmt.Sprintf("Manifest.supportedFormats contains %q", f))
		}
		if seen[f] {
			problems = append(problems, fmt.Sprintf("Manifest.supportedFormats lists %q twice", f))
		}
		seen[f] = true
	}
	for _, p := range m.RequiredPermissions {
		if !allowedPermissions[p] {
			problems = append(problems, fmt.Sprintf("Manifest.requiredPermissions contains %q, which is not a known permission", p))
		}
	}
	for _, l := range m.Locales {
		if !validLocaleTag(l) {
			problems = append(problems, fmt.Sprintf("Manifest.locales contains %q, which is not a BCP-47 language tag", l))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s: %s", ErrInvalidOptions, "manifest", strings.Join(problems, "; "))
}

// SupportsFormat reports whether this manifest advertises a format. Format
// "auto" and "" are always supported: they ask the adapter to sniff.
func (m Manifest) SupportsFormat(format string) bool {
	if format == "" || format == FormatAuto {
		return true
	}
	for _, f := range m.SupportedFormats {
		if strings.EqualFold(f, format) {
			return true
		}
	}
	return false
}

func validAdapterID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	// A leading '.' would turn an adapter id into a hidden path segment wherever
	// it is used as a filename (fixtures, cached renders).
	return s[0] != '.' && s[len(s)-1] != '.'
}

func validLocaleTag(s string) bool {
	// Structural BCP-47 only: subtags of 1-8 alphanumerics joined by '-', with a
	// primary subtag of 2-8 letters. Full registration lookup would need a
	// subtag registry this project does not carry (ADR-0008).
	if s == "" || len(s) > 35 {
		return false
	}
	primary, _, _ := strings.Cut(s, "-")
	if len(primary) < 2 || len(primary) > 8 {
		return false
	}
	for i := 0; i < len(primary); i++ {
		if c := primary[i]; !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	for _, sub := range strings.Split(s, "-") {
		if sub == "" || len(sub) > 8 {
			return false
		}
		for i := 0; i < len(sub); i++ {
			c := sub[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
				return false
			}
		}
	}
	return true
}

// FormatAuto asks the adapter to sniff the content instead of being told the
// format. It is the empty string, so a zero ParseOptions means "work it out".
const FormatAuto = ""

// ParseOptions configures one Parse call. It is a value, not a builder, so a
// caller can log it (minus secrets, which it has none of) alongside the import
// it describes.
type ParseOptions struct {
	// Format is one of the adapter's Manifest.SupportedFormats, or FormatAuto.
	Format string

	// ColumnMap names the source's columns. Empty fields ask the adapter to
	// auto-map common names; see generic's mapColumns for the candidate lists.
	ColumnMap ColumnMap

	// DefaultKind is assigned to a row whose kind column holds an unknown value
	// (docs/spec/04: an unknown mapping is data, not an error). Empty asks the
	// adapter for its own conservative default.
	DefaultKind games.TextKind

	// ProfileID is required: a message with no profile is unreachable by every
	// later query, and games.Message.Validate says so.
	ProfileID string

	// SourceID is the registered sources row this content came from. When set,
	// every message gets a SourceMessageID so a re-import reconciles instead of
	// duplicating (docs/spec/02 "Import resume by content hash and job_id").
	SourceID string

	// Language is the BCP-47 tag to record on every message. Empty means
	// "unknown"; adapters do not guess a language from the script, because
	// guessing wrong routes OCR and TTS to the wrong engine.
	Language string

	// MaxBytes bounds the input. Zero means DefaultMaxBytes; a caller may lower
	// it, never raise it past HardMaxBytes.
	MaxBytes int64

	// MaxRows bounds the number of logical rows. Zero means DefaultMaxRows.
	MaxRows int

	// BlankLineSplitsPages tells the plain-text reader that a blank line ends
	// one page and continues the *same* message, instead of starting a new one.
	BlankLineSplitsPages bool

	// TextNorm selects how normalized_text is produced. OCRFold belongs here
	// only for callers that deliberately want a lossy key; the recognition stage
	// applies it at match time instead.
	TextNorm textnorm.Options
}

// Limits for one import. The defaults are generous enough for a full game script
// (Wind Waker's is a few tens of thousands of messages) and small enough that a
// mistake — pointing the importer at a whole disk image — fails in a second
// rather than filling the disk (docs/spec/14 "memory/time limits").
const (
	DefaultMaxBytes int64 = 32 << 20
	HardMaxBytes    int64 = 512 << 20
	DefaultMaxRows        = 200_000
	HardMaxRows           = 2_000_000
)

// Normalize fills in defaults and clamps to the hard limits, so an adapter can
// trust its bounds without re-checking. It returns ErrInvalidOptions for the
// cases no default can fix.
func (o ParseOptions) Normalize() (ParseOptions, error) {
	if o.ProfileID == "" {
		return o, fmt.Errorf("%w: ParseOptions.ProfileID is required, a message with no profile is unreachable", ErrInvalidOptions)
	}
	if !games.ValidID(o.ProfileID) {
		return o, fmt.Errorf("%w: ParseOptions.ProfileID %q is not a 26-character ULID", ErrInvalidOptions, o.ProfileID)
	}
	if o.SourceID != "" && !games.ValidID(o.SourceID) {
		return o, fmt.Errorf("%w: ParseOptions.SourceID %q is not a 26-character ULID", ErrInvalidOptions, o.SourceID)
	}
	if o.DefaultKind != "" && !o.DefaultKind.Valid() {
		return o, fmt.Errorf("%w: ParseOptions.DefaultKind %q is not a TextKind", ErrInvalidOptions, string(o.DefaultKind))
	}
	if len(o.Language) > 32 {
		return o, fmt.Errorf("%w: ParseOptions.Language %q is longer than 32 characters", ErrInvalidOptions, o.Language)
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxBytes < 0 || o.MaxBytes > HardMaxBytes {
		o.MaxBytes = HardMaxBytes
	}
	if o.MaxRows == 0 {
		o.MaxRows = DefaultMaxRows
	}
	if o.MaxRows < 0 || o.MaxRows > HardMaxRows {
		o.MaxRows = HardMaxRows
	}
	return o, nil
}

// ColumnMap names source columns by their role. Every field is a column *name*
// (a CSV header, a JSON object key) or, for delimited files without a header
// row, a zero-based column index as text.
type ColumnMap struct {
	Text    string // the line to speak; required
	ID      string // source-side message identity, stable across re-imports
	Speaker string // speaker label; a label, never a character id
	Kind    string // content class: dialogue, sign, item, menu...
	Scene   string // scene or area label
	Page    string // page ordinal or id within a message
}

// Empty reports whether no explicit mapping was given at all.
func (c ColumnMap) Empty() bool {
	return c == ColumnMap{}
}

// LogicalColumn names one ColumnMap field, for error messages and for the
// onboarding UI's mapping table.
type LogicalColumn string

// The logical columns a source can supply.
const (
	ColText    LogicalColumn = "text"
	ColID      LogicalColumn = "id"
	ColSpeaker LogicalColumn = "speaker"
	ColKind    LogicalColumn = "kind"
	ColScene   LogicalColumn = "scene"
	ColPage    LogicalColumn = "page"
)

// SpeakerRef is one distinct speaker a parse reported. For a source that
// names its speakers (a script's speaker column) the label is source evidence,
// not an adapter's guess: docs/spec/04's "never invent a speaker" rule covers
// rows without a named speaker, while a speaker the user's own file names is a
// fact the importer may turn into a characters row. For a source that carries
// only an opaque per-line speaker id (the Wind Waker BMG's per-message
// attribute byte, ADR-0070), the ref reports the id as an identifier the
// importer may turn into a placeholder characters row a curator then links:
// SourceNamed is false and the identity stays unaudited until a reviewer settles
// it.
type SpeakerRef struct {
	// Key is the matching key: the name normalised the same way message text is
	// normalised (textnorm, case-folded, whitespace-collapsed). Two labels that
	// differ only in case, spacing or the compatibility folds map to one key, so
	// the importer can bind them to one character instead of two.
	Key string

	// DisplayName is the label exactly as written in the source on its first
	// occurrence. It becomes the characters.name of a character created for it,
	// which is why it is not normalised: the roster shows what the script said.
	// For a synthetic label (SourceNamed false) it is the opaque id itself.
	DisplayName string

	// Aliases are the other spellings the source used that normalise to Key, in
	// first-occurrence order, DisplayName excluded. They become characters.aliases
	// of a created character and are merged into a reused one.
	Aliases []string

	// SourceNamed reports whether the source itself supplied a name for this
	// speaker (true: a script column's label; false: an opaque id the adapter
	// minted from the source's own per-line linkage, e.g. the Wind Waker BMG's
	// speaker attribute byte, ADR-0070). The importer keeps a source-named
	// speaker confirmed at full confidence and creates a confirmed character for
	// it; a synthetic speaker gets a proposed placeholder character and a
	// proposed attribution, because the source vouches for the line-to-slot
	// linkage, not for the slot's identity.
	SourceNamed bool
}

// ParseResult is what an adapter produces. It is a value, not a live cursor: the
// caller validates each Message, decides on duplicates against the database, and
// writes in one transaction (docs/spec/02 "Transaction workflows").
type ParseResult struct {
	Messages []games.Message
	Pages    []games.MessagePage
	Warnings []Warning
	Stats    Stats

	// Speakers lists the distinct speakers the parse reported, in the order their
	// first-occurring label appears: either labels the source named itself
	// (SourceNamed true) or opaque ids the adapter derived from the source's own
	// per-line linkage (SourceNamed false, ADR-0070). Adapters that find no
	// speakers leave it empty (a nil slice, which the importer reads as "none").
	Speakers []SpeakerRef

	// SpeakerKeys is a per-message sidecar parallel to Messages: SpeakerKeys[i]
	// is the Speakers key of the speaker named on Messages[i], or "" when the row
	// carries no label. The sidecar exists instead of a field on games.Message
	// because the parser must not invent a characters row to point at —
	// messages.speaker_id is a foreign key the importer resolves (and may
	// reconcile against an existing roster) — and because keeping
	// games.Message unchanged keeps the stored row's contract identical for
	// adapters that do not report speakers at all. An adapter that fills
	// Speakers must fill this in parallel for every message it appends.
	SpeakerKeys []string

	// SpeakerColumn names the source column (header or key) the labels came from,
	// so the importer can record the provenance of an attribution the source
	// file itself supplied. "" when the source carried no speaker column.
	SpeakerColumn string
}

// Warning is a non-fatal defect, located for the importer report. Column is a
// human-readable locator ("line 12, field 3"), not a spreadsheet reference the
// importer would have to reverse-engineer.
type Warning struct {
	Line   int    `json:"line"`
	Column string `json:"column,omitempty"`
	Msg    string `json:"msg"`
}

// Stats counts rows for the import summary. Imported+Skipped accounts for every
// row the adapter looked at, so a curator can see that a 5,000-line file
// produced 4,100 messages and 900 skips rather than guessing.
type Stats struct {
	Rows     int `json:"rows"`
	Imported int `json:"imported"`
	Skipped  int `json:"skipped"`
}

// Adapter is the import-side subset of docs/spec/04's capability set:
// identifyEdition, parseArchive, extractMessages, decodeMarkup and
// renderSpeechText for a source format. The visual capabilities
// (proposeRegions, detectTextBox, detectPageCompletion, hintScene,
// proposeSpeakerVisuals) are added by a later milestone as separate interfaces,
// so an adapter that only imports text does not have to stub five methods — the
// interface segregation is the point, and a capability assertion
// (`v, ok := a.(adapter.RegionProposer)`) is how the runtime finds what exists.
type Adapter interface {
	// Manifest describes the adapter, its version and its permissions.
	Manifest() Manifest

	// Detect reports how confidently this adapter could parse a source with the
	// given file name and first bytes. head may be empty; an adapter that needs
	// content returns ConfidentNone rather than guessing from the name alone
	// when the name is meaningless.
	Detect(ctx context.Context, name string, head []byte) Confidence

	// Parse reads a source. r is read at most once and never beyond
	// ParseOptions.MaxBytes+1; implementations must stream rather than buffer the
	// whole file (docs/spec/16 M1 "without memory blowup"). Parse returns an
	// error, never a silently truncated result.
	Parse(ctx context.Context, r io.Reader, opts ParseOptions) (*ParseResult, error)
}

// EditionIdentifier is an optional capability: an adapter that can tell two
// editions of the same game apart (docs/spec/04 "identifyEdition() optional").
type EditionIdentifier interface {
	IdentifyEdition(ctx context.Context, head []byte) (edition string, confidence Confidence, err error)
}

// MarkupDecoder is an optional capability: an adapter whose sources carry
// control codes that must survive into a lossless AST rather than being regex'd
// away (docs/spec/02 "parse control bytes and symbolic tags into AST ...
// instead of regex removal").
type MarkupDecoder interface {
	// DecodeMarkup returns the AST and the plain text it renders to.
	DecodeMarkup(ctx context.Context, raw string) (markup json.RawMessage, plain string, err error)
}

// DiscExtractor is an optional capability: an adapter that reads a game's
// message script straight out of a user-supplied disc image (docs/spec/04
// "parseArchive"; ADR-0066). The runtime finds it by capability assertion,
// the way it finds the other optional capabilities, and a build without one
// reports that nothing can read a disc rather than trying the byte parser on
// an image it was not told about.
type DiscExtractor interface {
	// ExtractDiscSource verifies the image is this adapter's game (a wrong
	// game id is a provenance lie the user could not audit later, so the
	// refusal lives here, with the adapter's own rules) and returns the
	// message script's bytes: the content the importer hashes and stores,
	// the name to record on the source row, and the disc's game id for
	// provenance. The image is read in place and never copied.
	ExtractDiscSource(ctx context.Context, d *gcdisc.Disc) (script []byte, name, gameID string, err error)
}

// RevealCompleteDetector is an optional capability: an adapter whose game
// draws a visible marker once the typewriter has finished a line (docs/spec/04
// "detectPageCompletion"; ADR-0152). The recognition stage is game-free, so
// the marker's pixels stay in the adapter that knows them; the pipeline runs
// the detector where the region's crop is taken — the only stage that holds
// the frame image and the region rect together — and hands the verdict to the
// dialogue state machine as a single boolean on the observation (ADR-0152).
type RevealCompleteDetector interface {
	// RevealComplete reports whether img (the full frame) shows the marker
	// for the region whose frame-pixel rect is region. region is the
	// calibrated rect before the OCR inset: the marker sits relative to the
	// edge the profile calibrated, not the edge the crop cut. The call is
	// pure over the pixels and must allocate nothing per frame.
	RevealComplete(img image.Image, region image.Rectangle) bool
}

// Match is one Detect answer in Registry ordering.
type Match struct {
	Adapter    Adapter
	Manifest   Manifest
	Confidence Confidence
}

// Registry holds the adapters linked into this build. It is safe for concurrent
// use after construction: registration happens at init, detection happens per
// import.
type Registry struct {
	byID  map[string]Adapter
	order []string
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{byID: map[string]Adapter{}, order: nil}
}

// Register adds an adapter. A collision is refused rather than allowed to
// replace: two adapters claiming one ID would make sources.parser ambiguous, and
// the row that recorded the first would silently be parsed by the second.
func (r *Registry) Register(a Adapter) error {
	if a == nil {
		return fmt.Errorf("%w: Register called with a nil adapter", ErrNotRegistered)
	}
	m := a.Manifest()
	if err := m.Validate(); err != nil {
		return fmt.Errorf("%w: adapter %q: %w", ErrNotRegistered, m.ID, err)
	}
	if _, dup := r.byID[m.ID]; dup {
		return fmt.Errorf("%w: adapter id %q is already registered", ErrNotRegistered, m.ID)
	}
	r.byID[m.ID] = a
	r.order = append(r.order, m.ID)
	return nil
}

// MustRegister is Register for package-level wiring, where a failure is a build
// mistake rather than a runtime condition.
func (r *Registry) MustRegister(a Adapter) {
	if err := r.Register(a); err != nil {
		panic(err.Error())
	}
}

// Get returns an adapter by id.
func (r *Registry) Get(id string) (Adapter, bool) {
	a, ok := r.byID[id]
	return a, ok
}

// Adapters lists the registered adapters in registration order, which is the
// order doctor prints and the order tests assert on.
func (r *Registry) Adapters() []Manifest {
	out := make([]Manifest, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.byID[id].Manifest())
	}
	return out
}

// Len reports how many adapters are linked in.
func (r *Registry) Len() int { return len(r.order) }

// Detect returns every adapter that claims the source, best first. Ties break on
// adapter id so the ordering is stable across runs and process restarts — an
// unstable order would make an auto-picked adapter depend on map iteration
// (AGENTS.md "Keep runtime predictable").
func (r *Registry) Detect(ctx context.Context, name string, head []byte) []Match {
	var out []Match
	for _, id := range r.order {
		a := r.byID[id]
		c := a.Detect(ctx, name, head)
		if !c.Valid() || c <= ConfidentNone {
			continue
		}
		out = append(out, Match{Adapter: a, Manifest: a.Manifest(), Confidence: c})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Manifest.ID < out[j].Manifest.ID
	})
	return out
}

// DetectOne returns the best claimant, or nil when no adapter recognises the
// source. The caller decides whether "no adapter" is an error or an invitation to
// pick one by hand — the onboarding UI lists them all either way.
func (r *Registry) DetectOne(ctx context.Context, name string, head []byte) (Match, bool) {
	m := r.Detect(ctx, name, head)
	if len(m) == 0 {
		return Match{}, false
	}
	return m[0], true
}

//
// ── semver ──────────────────────────────────────────────────────────────────
//

// Semver is a parsed semantic version. Only the parts this project needs are
// kept: an adapter's version is compared for cache keys and reported in the
// manifest, never resolved like a package manager's.
type Semver struct {
	Major, Minor, Patch int
	Pre                 string
}

// ParseSemver reads "MAJOR.MINOR.PATCH[-pre]". Build metadata (+...) is
// accepted and discarded; anything else is refused, because "1.0" would otherwise
// let an adapter quietly change its idea of what a release is.
func ParseSemver(s string) (Semver, error) {
	var v Semver
	if s == "" {
		return v, errors.New("Manifest.version is empty; an adapter must state a semver")
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	core := s
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.Pre = s[i+1:]
		core = s[:i]
		if v.Pre == "" {
			return v, fmt.Errorf("Manifest.version %q has an empty prerelease part", s)
		}
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return v, fmt.Errorf("Manifest.version %q is not MAJOR.MINOR.PATCH", s)
	}
	out := []*int{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		if p == "" || (len(p) > 1 && p[0] == '0') {
			return v, fmt.Errorf("Manifest.version %q has a malformed or zero-padded component", s)
		}
		n := 0
		for j := 0; j < len(p); j++ {
			if p[j] < '0' || p[j] > '9' {
				return v, fmt.Errorf("Manifest.version %q is not numeric at %q", s, p)
			}
			n = n*10 + int(p[j]-'0')
		}
		*out[i] = n
	}
	for _, r := range v.Pre {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r == '.' || r == '-' || r == '+') {
			return v, fmt.Errorf("Manifest.version prerelease %q allows only alphanumerics, '-' and '+'", s)
		}
	}
	return v, nil
}

// String renders the version back.
func (v Semver) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
