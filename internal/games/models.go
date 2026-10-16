package games

import (
	"encoding/json"
	"strings"
)

// The structs map the tables in internal/store/migrations one to one, column
// names rendered in CamelCase. Optional (nullable) columns are empty string /
// nil in Go and written as NULL by the store layer; a JSON column is never nil
// when valid, because the schema defaults it to [] or {} and an absent value
// would silently mean "no evidence" in one read and "not yet looked" in another.

// Game is a title family (games).
type Game struct {
	ID          string
	Slug        string
	Title       string
	Description string
	Platform    string
	UserTags    json.RawMessage
	CreatedAt   string
}

// Validate mirrors the games table's constraints.
func (g Game) Validate() error {
	return validation(
		requireID("Game.id", g.ID),
		validateSlug(g.Slug),
		requireNonEmpty("Game.title", g.Title, 200),
		requireMaxLength("Game.description", g.Description, 4000),
		requireMaxLength("Game.platform", g.Platform, 100),
		requireJSONArray("Game.userTags", orEmptyArray(g.UserTags)),
		requireTimestamp("Game.createdAt", g.CreatedAt),
	)
}

// Edition binds a game to region/language/revision/platform specifics
// (editions).
type Edition struct {
	ID                 string
	GameID             string
	Region             string
	Language           string
	Revision           string
	SourceIdentity     string
	DisplayAspect      *float64
	Locale             string
	CompatibilityNotes string
}

// Validate mirrors the editions table's constraints.
func (e Edition) Validate() error {
	var problems []error
	problems = append(problems,
		requireID("Edition.id", e.ID),
		requireID("Edition.gameID", e.GameID),
		requireMaxLength("Edition.region", e.Region, 32),
		requireMaxLength("Edition.language", e.Language, 32),
		requireMaxLength("Edition.revision", e.Revision, 64),
		requireMaxLength("Edition.locale", e.Locale, 32),
		requireMaxLength("Edition.compatibilityNotes", e.CompatibilityNotes, 4000),
	)
	if e.SourceIdentity != "" {
		// An optional hash of the exact image the edition was observed against;
		// anything else is a guess about provenance and cannot be checked later.
		problems = append(problems, requireHash("Edition.sourceIdentity", e.SourceIdentity, true))
	}
	if e.DisplayAspect != nil {
		if *e.DisplayAspect <= 0 {
			problems = append(problems, invalid("Edition.displayAspect", "%v must be positive", *e.DisplayAspect))
		}
	}
	return validation(problems...)
}

// Profile is one revision of one edition (profiles). Revision numbers are dense
// per edition and ParentRevision points at the revision this one was derived
// from, which is how a published revision keeps a lineage while staying
// immutable.
type Profile struct {
	ID                string
	EditionID         string
	RevisionNum       int
	ParentRevision    *int
	Status            ProfileStatus
	ManifestDigest    string
	PublishedAt       string
	PreferredVoiceID  string
	PublicationAuthor string
}

// Validate mirrors the profiles table's constraints, including the rule that
// only a published revision has a publication timestamp and digest.
func (p Profile) Validate() error {
	problems := []error{
		requireID("Profile.id", p.ID),
		requireID("Profile.editionID", p.EditionID),
		requireRevision("Profile.revisionNum", p.RevisionNum),
		profileStatuses.require("Profile.status", p.Status),
		requireMaxLength("Profile.publicationAuthor", p.PublicationAuthor, 200),
	}
	if p.ParentRevision != nil && *p.ParentRevision < 1 {
		problems = append(problems, invalid("Profile.parentRevision", "%d is below 1", *p.ParentRevision))
	}
	if p.ManifestDigest != "" {
		problems = append(problems, requireHash("Profile.manifestDigest", p.ManifestDigest, true))
	}
	problems = append(problems, requireOptionalTimestamp("Profile.publishedAt", p.PublishedAt))
	// The status/timestamp pair is the immutability gate in concrete form: a
	// revision that claims to be published without a timestamp and digest could
	// not be re-validated by an auditor, and one that is not published but has
	// them implies a pointer swap nobody recorded (docs/spec/02 "Transaction
	// workflows").
	switch p.Status {
	case StatusPublished, StatusArchived:
		if p.PublishedAt == "" {
			problems = append(problems, invalid("Profile.publishedAt", "is required when status is %q", p.Status))
		}
		if p.ManifestDigest == "" {
			problems = append(problems, invalid("Profile.manifestDigest", "is required when status is %q", p.Status))
		}
	default:
		if p.PublishedAt != "" {
			problems = append(problems, invalid("Profile.publishedAt", "must be absent when status is %q", p.Status))
		}
	}
	if p.PreferredVoiceID != "" {
		problems = append(problems, requireID("Profile.preferredVoiceID", p.PreferredVoiceID))
	}
	return validation(problems...)
}

// CanPublish reports whether a revision may be published from its current
// status. Publication goes through review, never straight from draft
// (docs/spec/03: the user reviews and publishes).
func (p Profile) CanPublish() bool { return p.Status.CanTransition(StatusPublished) }

// IsImmutable reports whether edits must be refused in favour of deriving a new
// revision.
func (p Profile) IsImmutable() bool { return p.Status == StatusPublished || p.Status == StatusArchived }

// Source records where content came from (sources). Provenance rows are
// immutable once registered (docs/spec/01 "Key invariants"); corrections are
// overlays rather than edits to them.
type Source struct {
	ID            string
	ProfileID     string
	SourceType    SourceType
	URI           string
	RetrievedAt   string
	ContentSHA256 string
	MimeType      string
	Rights        RightsStatus
	Trust         float64
	Parser        string
	CoverageNotes string
}

// Validate mirrors the sources table's constraints.
func (s Source) Validate() error {
	problems := []error{
		requireID("Source.id", s.ID),
		requireID("Source.profileID", s.ProfileID),
		sourceTypes.require("Source.sourceType", s.SourceType),
		requireNonEmpty("Source.uri", s.URI, 2000),
		requireTimestamp("Source.retrievedAt", s.RetrievedAt),
		rightsStatuses.require("Source.rights", s.Rights),
		requireFraction("Source.trust", s.Trust),
		requireMaxLength("Source.parser", s.Parser, 100),
		requireMaxLength("Source.coverageNotes", s.CoverageNotes, 4000),
	}
	// '' means "no addressable payload" (a manual note). Any other value has to
	// be a digest, or provenance cannot be re-verified.
	if s.ContentSHA256 != "" {
		problems = append(problems, requireHash("Source.contentSHA256", s.ContentSHA256, true))
	}
	if strings.HasPrefix(s.URI, "file://") && s.Rights == RightsUnknown {
		// A local file the user handed over is user-supplied by definition;
		// 'unknown' here would be an unresolved question about their own disc,
		// which the onboarding flow has to answer rather than store.
		problems = append(problems, invalid("Source.rights",
			"a user-supplied file cannot have rights %q; ask the user instead of guessing", s.Rights))
	}
	return validation(problems...)
}

// Message is canonical source text plus its renderings (messages). It is *not*
// an occurrence, a page, an utterance or an audio asset — see docs/spec/02
// "Core data distinctions".
type Message struct {
	ID                    string
	ProfileID             string
	SourceID              string // empty = no registered source row
	SourceMessageID       string // source-side identity, stable across re-imports
	OriginalText          string
	MarkupJSON            json.RawMessage
	NormalizedText        string
	SpeechText            string
	Language              string
	TextKind              TextKind
	SceneID               string
	SpeakerID             string
	AttributionStatus     AttributionStatus
	AttributionConfidence float64
	AttributionEvidence   json.RawMessage
	Flags                 json.RawMessage
	Revision              int
	CreatedAt             string
	UpdatedAt             string
}

// Validate mirrors the messages table's constraints.
func (m Message) Validate() error {
	problems := []error{
		requireID("Message.id", m.ID),
		requireID("Message.profileID", m.ProfileID),
		requireNonEmpty("Message.originalText", m.OriginalText, 8000),
		requireJSONArray("Message.markupJSON", orEmptyArray(m.MarkupJSON)),
		requireNonEmpty("Message.normalizedText", m.NormalizedText, 8000),
		requireMaxLength("Message.speechText", m.SpeechText, 8000),
		textKinds.require("Message.textKind", m.TextKind),
		attributionStatuses.require("Message.attributionStatus", m.AttributionStatus),
		requireFraction("Message.attributionConfidence", m.AttributionConfidence),
		requireJSONArray("Message.attributionEvidence", orEmptyArray(m.AttributionEvidence)),
		requireJSONObject("Message.flags", orEmptyObject(m.Flags)),
		requireRevision("Message.revision", m.Revision),
		requireTimestamp("Message.createdAt", m.CreatedAt),
		requireTimestamp("Message.updatedAt", m.UpdatedAt),
	}
	// Both are optional individually, but an id is checked when present so a
	// typo cannot persist as a dangling reference error at read time.
	problems = append(problems,
		requireRef("Message.sourceID", m.SourceID),
		requireRef("Message.sceneID", m.SceneID),
		requireRef("Message.speakerID", m.SpeakerID),
		requireMaxLength("Message.language", m.Language, 32),
	)
	if m.SourceMessageID == "" && m.SourceID != "" {
		// A message that came from a source but carries no source-side identity
		// cannot be reconciled on re-import: the next parse would duplicate it.
		problems = append(problems, invalid("Message.sourceMessageID",
			"is required when Message.sourceID is set, so re-imports reconcile instead of duplicating"))
	}
	if m.SpeakerID != "" && m.AttributionStatus == AttributionRejected {
		problems = append(problems, invalid("Message.speakerID",
			"must be absent when attributionStatus is %q", AttributionRejected))
	}
	if !validSourceSideID(m.SourceMessageID) {
		// Source-side ids come from dumps and scripts: bounded, printable, no
		// whitespace, so a parser bug cannot smuggle markup or a newline into a
		// UNIQUE key where it would be invisible in the UI and fatal on re-import.
		problems = append(problems, invalid("Message.sourceMessageID",
			"%q must be 1-%d printable characters without spaces", m.SourceMessageID, maxSourceSideID))
	}
	return validation(problems...)
}

// MessagePage is one display page of a message (message_pages). docs/spec/02:
// a message can have several pages, and a page is not an utterance.
type MessagePage struct {
	MessageID       string
	Ordinal         int
	SourcePageID    string
	PageText        string
	VisiblePrefixes json.RawMessage
	DisplayControls json.RawMessage
	TimingJSON      json.RawMessage
}

// Validate mirrors the message_pages table's constraints.
func (p MessagePage) Validate() error {
	return validation(
		requireID("MessagePage.messageID", p.MessageID),
		requireRevision("MessagePage.ordinal", p.Ordinal),
		requireNonEmpty("MessagePage.pageText", p.PageText, 8000),
		requireJSONArray("MessagePage.visiblePrefixes", orEmptyArray(p.VisiblePrefixes)),
		requireJSONArray("MessagePage.displayControls", orEmptyArray(p.DisplayControls)),
		requireJSONObject("MessagePage.timingJSON", orEmptyObject(p.TimingJSON)),
		requireSourcePageID("MessagePage.sourcePageID", p.SourcePageID),
	)
}

// MessageEdge is a dialogue transition (message_edges). ConditionExpr is
// data-only: it is never evaluated by SQLite and never executed (docs/spec/02
// marks it "**data-only**"; the policy engine in docs/spec/05 interprets it).
type MessageEdge struct {
	FromID        string
	ToID          string
	EdgeType      EdgeType
	ConditionExpr string
	SourceID      string
	Evidence      json.RawMessage
	Confidence    float64
	CreatedAt     string
}

// MaxConditionExpr is the length cap on a serialized condition. It is a
// deliberate bound, not a guess at a grammar: an unbounded condition string is
// an unbounded amount of work for the evaluator (AGENTS.md "bounded queues").
const MaxConditionExpr = 2000

// Validate mirrors the message_edges table's constraints, plus the ones the
// schema cannot express: a self-edge and an inferred edge claiming certainty.
func (e MessageEdge) Validate() error {
	problems := []error{
		requireID("MessageEdge.fromID", e.FromID),
		requireID("MessageEdge.toID", e.ToID),
		edgeTypes.require("MessageEdge.edgeType", e.EdgeType),
		requireJSONArray("MessageEdge.evidence", orEmptyArray(e.Evidence)),
		requireFraction("MessageEdge.confidence", e.Confidence),
		requireMaxLength("MessageEdge.conditionExpr", e.ConditionExpr, MaxConditionExpr),
		requireTimestamp("MessageEdge.createdAt", e.CreatedAt),
		requireRef("MessageEdge.sourceID", e.SourceID),
	}
	if e.FromID == e.ToID && e.EdgeType != EdgeRepeat {
		// Only a repeat may point at itself; any other self-edge is the graph
		// builder looping and would make traversal non-terminating.
		problems = append(problems, invalid("MessageEdge.toID",
			"equals fromID, which only a %q edge may do", EdgeRepeat))
	}
	if max := e.EdgeType.MaxConfidence(); e.Confidence > max {
		problems = append(problems, invalid("MessageEdge.confidence",
			"%v exceeds what a %q edge may claim (%v)", e.Confidence, e.EdgeType, max))
	}
	if e.EdgeType == EdgeContextual && strings.TrimSpace(e.ConditionExpr) == "" {
		problems = append(problems, invalid("MessageEdge.conditionExpr",
			"is required for a %q edge, otherwise the branch is undecidable", EdgeContextual))
	}
	return validation(problems...)
}

// Character is a person or thing that speaks (characters).
type Character struct {
	ID                 string
	ProfileID          string
	Name               string
	Aliases            json.RawMessage
	VisualDescriptors  json.RawMessage
	PersonalitySummary string
	Role               string
	VerifiedSourceRefs json.RawMessage
	PortraitAssetID    string
	Status             CharacterStatus
	CreatedAt          string
}

// Validate mirrors the characters table's constraints.
func (c Character) Validate() error {
	return validation(
		requireID("Character.id", c.ID),
		requireID("Character.profileID", c.ProfileID),
		requireNonEmpty("Character.name", c.Name, 200),
		requireJSONArray("Character.aliases", orEmptyArray(c.Aliases)),
		requireJSONArray("Character.visualDescriptors", orEmptyArray(c.VisualDescriptors)),
		requireMaxLength("Character.personalitySummary", c.PersonalitySummary, 4000),
		requireMaxLength("Character.role", c.Role, 100),
		requireJSONArray("Character.verifiedSourceRefs", orEmptyArray(c.VerifiedSourceRefs)),
		requireHash("Character.portraitAssetID", c.PortraitAssetID, false),
		characterStatuses.require("Character.status", c.Status),
		requireTimestamp("Character.createdAt", c.CreatedAt),
	)
}

// Scene is an editorial grouping with matching signals (scenes).
type Scene struct {
	ID                string
	ProfileID         string
	Name              string
	Aliases           json.RawMessage
	RegionFingerprint string
	VisualAnchors     json.RawMessage
	ProgressJSON      json.RawMessage
	CreatedAt         string
}

// Validate mirrors the scenes table's constraints.
func (s Scene) Validate() error {
	return validation(
		requireID("Scene.id", s.ID),
		requireID("Scene.profileID", s.ProfileID),
		requireNonEmpty("Scene.name", s.Name, 200),
		requireJSONArray("Scene.aliases", orEmptyArray(s.Aliases)),
		requireJSONArray("Scene.visualAnchors", orEmptyArray(s.VisualAnchors)),
		requireJSONObject("Scene.progressJSON", orEmptyObject(s.ProgressJSON)),
		requireMaxLength("Scene.regionFingerprint", s.RegionFingerprint, 200),
		requireTimestamp("Scene.createdAt", s.CreatedAt),
	)
}

// Region is one named rectangle of the screen where text is read (regions).
// Rect* are normalised [0,1] fractions of the display frame, so the aspect
// conversion to device pixels stays in the adapter (docs/spec/04).
type Region struct {
	ID           string
	ProfileID    string
	SceneID      string
	Label        string
	RegionType   RegionType
	DetectMethod DetectMethod
	RectX        float64
	RectY        float64
	RectW        float64
	RectH        float64
	MinAspect    *float64
	MaxAspect    *float64
	MinWidthPx   *int
	MinHeightPx  *int
	OCRSettings  json.RawMessage
	MaskAssetIDs json.RawMessage
	Reliability  *float64
	Revision     int
	UpdatedAt    string
}

// Validate mirrors the regions table's constraints, including the geometry that
// only makes sense across columns: the box must sit inside the frame.
func (r Region) Validate() error {
	problems := []error{
		requireID("Region.id", r.ID),
		requireID("Region.profileID", r.ProfileID),
		requireRef("Region.sceneID", r.SceneID),
		regionTypes.require("Region.regionType", r.RegionType),
		detectMethods.require("Region.detectMethod", r.DetectMethod),
		requireJSONObject("Region.ocrSettings", orEmptyObject(r.OCRSettings)),
		requireJSONArray("Region.maskAssetIDs", orEmptyArray(r.MaskAssetIDs)),
		requireRevision("Region.revision", r.Revision),
		requireTimestamp("Region.updatedAt", r.UpdatedAt),
		requireMaxLength("Region.label", r.Label, 200),
	}
	for _, side := range []struct {
		field string
		value float64
	}{{"Region.rectX", r.RectX}, {"Region.rectY", r.RectY}, {"Region.rectW", r.RectW}, {"Region.rectH", r.RectH}} {
		problems = append(problems, requireFraction(side.field, side.value))
	}
	// The sum guards, not the individual bounds alone: a 0.6-wide box at x=0.5
	// is four legal fractions describing half a textbox. The comparison is
	// written exactly as the SQL CHECK writes it, with no tolerance, because
	// SQLite's REAL is the same IEEE double Go uses here: a slack of its own
	// would let the service accept a box the database then rejects, which is
	// the failure this Validate exists to prevent.
	if r.RectX+r.RectW > 1 {
		problems = append(problems, invalid("Region.rectW",
			"%v at x=%v runs past the right edge of the frame", r.RectW, r.RectX))
	}
	if r.RectY+r.RectH > 1 {
		problems = append(problems, invalid("Region.rectH",
			"%v at y=%v runs past the bottom edge of the frame", r.RectH, r.RectY))
	}
	if r.MinAspect != nil && *r.MinAspect <= 0 {
		problems = append(problems, invalid("Region.minAspect", "%v must be positive", *r.MinAspect))
	}
	if r.MaxAspect != nil && *r.MaxAspect <= 0 {
		problems = append(problems, invalid("Region.maxAspect", "%v must be positive", *r.MaxAspect))
	}
	if r.MinAspect != nil && r.MaxAspect != nil && *r.MinAspect > *r.MaxAspect {
		problems = append(problems, invalid("Region.maxAspect",
			"%v is below minAspect %v, so no capture mode can match", *r.MaxAspect, *r.MinAspect))
	}
	if r.MinWidthPx != nil && *r.MinWidthPx < 1 {
		problems = append(problems, invalid("Region.minWidthPx", "%d must be at least 1", *r.MinWidthPx))
	}
	if r.MinHeightPx != nil && *r.MinHeightPx < 1 {
		problems = append(problems, invalid("Region.minHeightPx", "%d must be at least 1", *r.MinHeightPx))
	}
	if r.Reliability != nil {
		problems = append(problems, requireFraction("Region.reliability", *r.Reliability))
	}
	return validation(problems...)
}

// SpeakerAssertion is a candidate attribution that is *not* the resolved
// speaker (speaker_assertions). docs/spec/02 keeps these separate precisely
// because a name on screen is not proof.
type SpeakerAssertion struct {
	ID          string
	ProfileID   string
	MessageID   string
	CharacterID string
	Method      AssertionMethod
	AssertedAt  string
	Evidence    json.RawMessage
	Posterior   float64
	Reviewer    string
}

// Validate mirrors the speaker_assertions table's constraints, plus the rule
// that a manual claim has to name the person who made it.
func (a SpeakerAssertion) Validate() error {
	problems := []error{
		requireID("SpeakerAssertion.id", a.ID),
		requireID("SpeakerAssertion.profileID", a.ProfileID),
		requireID("SpeakerAssertion.messageID", a.MessageID),
		requireID("SpeakerAssertion.characterID", a.CharacterID),
		assertionMethods.require("SpeakerAssertion.method", a.Method),
		requireTimestamp("SpeakerAssertion.assertedAt", a.AssertedAt),
		requireJSONArray("SpeakerAssertion.evidence", orEmptyArray(a.Evidence)),
		requireFraction("SpeakerAssertion.posterior", a.Posterior),
		requireMaxLength("SpeakerAssertion.reviewer", a.Reviewer, 200),
	}
	if a.Method.FromReviewer() && a.Reviewer == "" {
		problems = append(problems, invalid("SpeakerAssertion.reviewer",
			"is required for a %q assertion, so the audit trail names who decided", a.Method))
	}
	if !a.Method.FromReviewer() && a.Reviewer != "" {
		problems = append(problems, invalid("SpeakerAssertion.reviewer",
			"must be absent for an automated %q assertion", a.Method))
	}
	return validation(problems...)
}

// Correction is a user fix recorded as a label (corrections). TargetID is a
// loose reference by design: the label has to outlive the row it was about, so
// a re-import that removes a message does not erase the evidence that the
// parser got it wrong (docs/spec/04 "Adapter evolution and repair").
type Correction struct {
	ID          string
	ProfileID   string
	Kind        CorrectionKind
	TargetType  CorrectionTarget
	TargetID    string
	MessageID   string
	BeforeValue string
	AfterValue  string
	Reason      string
	Evidence    json.RawMessage
	Actor       string
	Status      CorrectionStatus
	Reviewer    string
	CreatedAt   string
	ReviewedAt  string
}

// Validate mirrors the corrections table's constraints.
func (c Correction) Validate() error {
	problems := []error{
		requireID("Correction.id", c.ID),
		requireID("Correction.profileID", c.ProfileID),
		correctionKinds.require("Correction.kind", c.Kind),
		correctionTargets.require("Correction.targetType", c.TargetType),
		requireNonEmpty("Correction.targetID", c.TargetID, 120),
		requireJSONArray("Correction.evidence", orEmptyArray(c.Evidence)),
		requireNonEmpty("Correction.actor", c.Actor, 200),
		correctionStatuses.require("Correction.status", c.Status),
		requireTimestamp("Correction.createdAt", c.CreatedAt),
		requireOptionalTimestamp("Correction.reviewedAt", c.ReviewedAt),
		requireRef("Correction.messageID", c.MessageID),
		requireMaxLength("Correction.reason", c.Reason, 2000),
	}
	switch c.Status {
	case CorrectionAccepted, CorrectionRejected, CorrectionSuperseded:
		if c.Reviewer == "" {
			problems = append(problems, invalid("Correction.reviewer",
				"is required once a correction is %q", c.Status))
		}
		if c.ReviewedAt == "" {
			problems = append(problems, invalid("Correction.reviewedAt",
				"is required once a correction is %q", c.Status))
		}
	default:
		if c.Reviewer != "" || c.ReviewedAt != "" {
			problems = append(problems, invalid("Correction.status",
				"must be accepted or rejected before a reviewer or review time is recorded"))
		}
	}
	if c.Status == CorrectionAccepted && c.AfterValue == "" {
		problems = append(problems, invalid("Correction.afterValue",
			"is required for an accepted correction: there is nothing to apply"))
	}
	if c.Kind == CorrectionRegion && c.TargetType != TargetRegion {
		problems = append(problems, invalid("Correction.targetType",
			"%q does not name a region, which a %q correction changes", c.TargetType, c.Kind))
	}
	if c.TargetType == TargetMessage && c.MessageID != "" && c.MessageID != c.TargetID {
		problems = append(problems, invalid("Correction.messageID",
			"%q disagrees with targetID %q", c.MessageID, c.TargetID))
	}
	return validation(problems...)
}

// maxSourceSideID bounds a source-side identity column (messages.source_message_id,
// message_pages.source_page_id): long enough for any dump's addressing scheme,
// short enough to stay indexable and visible.
const maxSourceSideID = 120

// validSourceSideID reports whether s is a usable source-side identity: empty
// (no source identity at all) or a bounded printable run with no whitespace.
func validSourceSideID(s string) bool {
	if s == "" || len(s) > maxSourceSideID {
		return s == ""
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

//
// ── helpers ─────────────────────────────────────────────────────────────────
//

func requireNonEmpty(field, value string, max int) error {
	if strings.TrimSpace(value) == "" {
		return invalid(field, "is required")
	}
	return requireMaxLength(field, value, max)
}

func requireMaxLength(field, value string, max int) error {
	// Length in runes: these columns hold game text with multibyte characters,
	// and a byte limit would make the allowed content depend on the encoding.
	if n := len([]rune(value)); n > max {
		return invalid(field, "is %d characters, the limit is %d", n, max)
	}
	return nil
}

// requireSourcePageID checks an optional source-side page identity.
func requireSourcePageID(field, s string) error {
	if !validSourceSideID(s) {
		return invalid(field, "%q must be at most %d printable characters without spaces", s, maxSourceSideID)
	}
	return nil
}

// validateSlug enforces the URL-safe identity used in /api/v1/games/:id paths.
func validateSlug(slug string) error {
	if slug == "" {
		return invalid("Game.slug", "is required")
	}
	if len(slug) > 80 {
		return invalid("Game.slug", "is %d characters, the limit is 80", len(slug))
	}
	if strings.ToLower(slug) != slug {
		return invalid("Game.slug", "%q must be lowercase", slug)
	}
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		ok := (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_'
		if !ok {
			return invalid("Game.slug", "%q allows only lowercase letters, digits, '-' and '_'", slug)
		}
	}
	// A slug that starts or ends with a separator reads as a broken path and
	// makes a trailing-slash redirect loop possible in the API surface.
	if strings.HasPrefix(slug, "-") || strings.HasSuffix(slug, "-") {
		return invalid("Game.slug", "%q must not begin or end with '-'", slug)
	}
	return nil
}

// orEmptyArray treats a nil JSON column as the schema's own default so that a
// caller who never touched the field is not reported as having an invalid one.
func orEmptyArray(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("[]")
	}
	return raw
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}
