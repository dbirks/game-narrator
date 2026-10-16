package games

import (
	"fmt"
	"slices"
	"strings"
)

// The enums below mirror the CHECK (col IN (…)) constraints in
// internal/store/migrations one for one, and the tests in enums_test.go assert
// the two vocabularies agree by reading the migration text. A closed vocabulary
// is only justified where the *code* branches on the value: a text kind selects
// OCR and speech-rendering defaults, an edge type changes how traversal treats a
// transition, a job state decides whether a worker may claim the row. Where the
// meaning belongs to a game adapter instead (characters.role, region
// ocr_settings, message flags, edge condition expressions) the type stays open
// data, because docs/spec/04's portability rule is violated the moment a
// second title needs a value this enum never imagined.

//
// ── Enum registry ───────────────────────────────────────────────────────────
//

// Enum is a named vocabulary: the column it constrains and the exact spellings
// it allows.
type Enum struct {
	Column string
	Values []string
}

// enumsByName indexes every vocabulary in this package by its Go type name.
// Tooling needs to enumerate them without importing 15 separate lists — the
// OpenAPI generator writes them into schema components, and
// TestEnumsMatchTheSchemaCHECKs in this package's tests compares them against
// the migrations' CHECK lists, which is what stops the two vocabularies drifting.
var enumsByName = map[string]Enum{}

// register adds a table to the registry. init order does not matter: each call
// names its own type.
func register[T ~string](typeName string, table enumTable[T]) {
	if _, dup := enumsByName[typeName]; dup {
		panic("games: enum registered twice: " + typeName)
	}
	values := make([]string, len(table.values))
	for i, v := range table.values {
		values[i] = string(v)
	}
	enumsByName[typeName] = Enum{Column: table.field, Values: values}
}

func init() {
	register("ProfileStatus", profileStatuses)
	register("SourceType", sourceTypes)
	register("RightsStatus", rightsStatuses)
	register("TextKind", textKinds)
	register("AttributionStatus", attributionStatuses)
	register("EdgeType", edgeTypes)
	register("CharacterStatus", characterStatuses)
	register("RegionType", regionTypes)
	register("DetectMethod", detectMethods)
	register("AssertionMethod", assertionMethods)
	register("CorrectionKind", correctionKinds)
	register("CorrectionTarget", correctionTargets)
	register("CorrectionStatus", correctionStatuses)
	register("JobState", jobStates)
	register("AttemptOutcome", attemptOutcomes)
}

// EnumNames lists the registered vocabularies, sorted.
func EnumNames() []string {
	out := make([]string, 0, len(enumsByName))
	for name := range enumsByName {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// LookupEnum returns the vocabulary registered under a Go type name.
func LookupEnum(typeName string) (Enum, bool) {
	e, ok := enumsByName[typeName]
	return e, ok
}

// WalkEnum reports the values of one vocabulary, in declaration order, and an
// error when the name is not a registered enum. The callback runs once per
// value; the order is the order the service lists them in errors.
func WalkEnum(typeName string, fn func(value string)) error {
	e, ok := enumsByName[typeName]
	if !ok {
		return fmt.Errorf("games: no enum registered as %q (known: %s)", typeName, strings.Join(EnumNames(), ", "))
	}
	for _, v := range e.Values {
		fn(v)
	}
	return nil
}

// enumTable is a validated vocabulary for one column.
type enumTable[T ~string] struct {
	field  string // column name, for error messages
	values []T
}

// Valid reports whether v is one of the allowed values.
func (e enumTable[T]) Valid(v T) bool {
	for _, allowed := range e.values {
		if allowed == v {
			return true
		}
	}
	return false
}

// String renders a value. An out-of-vocabulary value comes back unchanged so a
// log line shows what was actually stored rather than a rewrite of it — Validate
// is what refuses it.
func (e enumTable[T]) String(v T) string { return string(v) }

// Parse converts wire text to the typed value, listing the accepted spellings
// on failure so an API client learns the vocabulary from the error.
func (e enumTable[T]) Parse(s string) (T, error) {
	v := T(s)
	if !e.Valid(v) {
		var zero T
		return zero, invalid(e.field, "%q is not a valid %s (want one of %s)",
			s, e.field, strings.Join(quoteAll(e.values), ", "))
	}
	return v, nil
}

// require checks the value as stored in a domain struct.
func (e enumTable[T]) require(field string, v T) error {
	if !e.Valid(v) {
		return invalid(field, "%q is not a valid %s (want one of %s)",
			string(v), e.field, strings.Join(quoteAll(e.values), ", "))
	}
	return nil
}

func quoteAll[T ~string](vals []T) []string {
	out := make([]string, len(vals))
	for i, v := range vals {
		out[i] = fmt.Sprintf("%q", string(v))
	}
	return out
}

//
// ProfileStatus — profiles.status
//

// ProfileStatus is the publication lifecycle of one profile revision
// (docs/spec/02 "Transaction workflows": draft edit → validate → review →
// atomic publication; docs/spec/04 "Adapter evolution and repair").
type ProfileStatus string

const (
	// StatusDraft is editable working state.
	StatusDraft ProfileStatus = "draft"
	// StatusReview is frozen for approval; validation findings are recorded
	// against it and a curator decides (docs/spec/03 "review/publish").
	StatusReview ProfileStatus = "review"
	// StatusPublished is immutable and is what live sessions pin.
	StatusPublished ProfileStatus = "published"
	// StatusArchived keeps a published revision addressable (its audio cache
	// namespace and its occurrences must survive) while removing it from the
	// selectable set.
	StatusArchived ProfileStatus = "archived"
)

var profileStatuses = enumTable[ProfileStatus]{field: "status", values: []ProfileStatus{
	StatusDraft, StatusReview, StatusPublished, StatusArchived,
}}

// String implements fmt.Stringer.
func (s ProfileStatus) String() string { return profileStatuses.String(s) }

// Valid reports whether the status is one this build knows.
func (s ProfileStatus) Valid() bool { return profileStatuses.Valid(s) }

// ParseProfileStatus reads the wire spelling of a status.
func ParseProfileStatus(s string) (ProfileStatus, error) { return profileStatuses.Parse(s) }

// CanTransition reports whether a revision may move from s to to.
//
// The graph is deliberately narrow:
//
//	draft   → review            the curator asks for publication
//	review  → draft             validation or a reviewer sent it back
//	review  → published         the approval gate, and the only way in
//	published → archived        retirement; still addressable, no longer selectable
//
// Published is immutable: there is no path back to draft or review, because
// active sessions pin a published revision and audio cache keys derive from its
// manifest digest (docs/spec/02 "Transaction workflows"). Changing a published
// profile means deriving a new revision_num (0001's parent_revision chain) and
// publishing that, so a listener can never hear different words for the same
// revision. Archived is terminal: re-selecting an old revision is done by
// deriving from it, not by resurrecting it, which keeps "this session ran
// revision 3" a permanent statement. Self-transitions report false — a no-op
// state change is a bug in a caller, not a success to hide.
func (s ProfileStatus) CanTransition(to ProfileStatus) bool {
	if !s.Valid() || !to.Valid() {
		return false
	}
	switch s {
	case StatusDraft:
		return to == StatusReview
	case StatusReview:
		return to == StatusDraft || to == StatusPublished
	case StatusPublished:
		return to == StatusArchived
	case StatusArchived:
		return false
	}
	return false
}

// NextStates lists the statuses reachable from s, for the API and the UI so
// neither re-implements the transition table.
func (s ProfileStatus) NextStates() []ProfileStatus {
	var out []ProfileStatus
	for _, candidate := range profileStatuses.values {
		if s.CanTransition(candidate) {
			out = append(out, candidate)
		}
	}
	return out
}

//
// SourceType and RightsStatus — sources
//

// SourceType is sources.source_type: where profile content came from.
type SourceType string

const (
	SourceUserFile    SourceType = "user_file"
	SourceWebPage     SourceType = "web_page"
	SourceRepository  SourceType = "repository"
	SourceManual      SourceType = "manual"
	SourceGameExtract SourceType = "game_extract"
)

var sourceTypes = enumTable[SourceType]{field: "source_type", values: []SourceType{
	SourceUserFile, SourceWebPage, SourceRepository, SourceManual, SourceGameExtract,
}}

// String implements fmt.Stringer.
func (t SourceType) String() string { return sourceTypes.String(t) }

// Valid reports whether the value is known.
func (t SourceType) Valid() bool { return sourceTypes.Valid(t) }

// ParseSourceType reads a source type from wire text.
func ParseSourceType(s string) (SourceType, error) { return sourceTypes.Parse(s) }

// IsExternal reports whether content arrived from outside the user's machine.
// External material is untrusted input: docs/spec/14 requires it to be treated
// as data and forbids letting it steer tools.
func (t SourceType) IsExternal() bool {
	return t == SourceWebPage || t == SourceRepository
}

// RightsStatus is sources.rights_status — what the user may do with the bytes.
type RightsStatus string

const (
	RightsUserSupplied RightsStatus = "user_supplied"
	RightsLicensed     RightsStatus = "licensed"
	RightsUnknown      RightsStatus = "unknown"
)

var rightsStatuses = enumTable[RightsStatus]{field: "rights_status", values: []RightsStatus{
	RightsUserSupplied, RightsLicensed, RightsUnknown,
}}

// String implements fmt.Stringer.
func (r RightsStatus) String() string { return rightsStatuses.String(r) }

// Valid reports whether the value is known.
func (r RightsStatus) Valid() bool { return rightsStatuses.Valid(r) }

// ParseRightsStatus reads a rights status from wire text.
func ParseRightsStatus(s string) (RightsStatus, error) { return rightsStatuses.Parse(s) }

// Publishable reports whether the rights record allows shipping derived content
// (text or speech) in an export. Unknown is refused: docs/spec/02 requires the
// default export to exclude source blobs, and docs/spec/14 requires a named
// rights position before publication rather than an optimistic guess.
func (r RightsStatus) Publishable() bool { return r == RightsUserSupplied || r == RightsLicensed }

//
// TextKind — messages.text_kind
//

// TextKind classifies a message so the policy engine can decide whether to
// speak it and which rendering rules apply (docs/spec/05 "Message class and
// speech rules").
type TextKind string

const (
	// TextDialogue is a character's spoken line.
	TextDialogue TextKind = "dialogue"
	// TextSign is environmental text read off a board or plaque.
	TextSign TextKind = "sign"
	// TextItem is an item or description popup.
	TextItem TextKind = "item"
	// TextMenu is a UI/menu string; usually not narrated.
	TextMenu TextKind = "menu"
	// TextTutorial is a contextual teaching prompt.
	TextTutorial TextKind = "tutorial"
	// TextSystem is a system or notification message.
	TextSystem TextKind = "system"
	// TextOther is anything no other class fits, kept rather than guessed into
	// one: an unknown class is still speakable under the conservative policy.
	TextOther TextKind = "other"
)

var textKinds = enumTable[TextKind]{field: "text_kind", values: []TextKind{
	TextDialogue, TextSign, TextItem, TextMenu, TextTutorial, TextSystem, TextOther,
}}

// String implements fmt.Stringer.
func (k TextKind) String() string { return textKinds.String(k) }

// Valid reports whether the value is known.
func (k TextKind) Valid() bool { return textKinds.Valid(k) }

// ParseTextKind reads a text kind from wire text.
func ParseTextKind(s string) (TextKind, error) { return textKinds.Parse(s) }

// DefaultSpeakable is the docs/spec/05 starting policy per class: menus are the
// player's own interface, and everything else is content the user asked to hear.
// It is a default, always overridable by the profile's rules and the session's
// speech modes.
func (k TextKind) DefaultSpeakable() bool { return k != TextMenu }

//
// AttributionStatus — messages.attribution_status
//

// AttributionStatus says how much to trust messages.SpeakerID. The distinction
// matters because docs/spec/02 refuses to treat an on-screen name as proof.
type AttributionStatus string

const (
	// AttributionUnknown: nothing established the speaker either way.
	AttributionUnknown AttributionStatus = "unknown"
	// AttributionProposed: a candidate from a parser, a model or a visual
	// signal, not yet accepted.
	AttributionProposed AttributionStatus = "proposed"
	// AttributionConfirmed: evidence or a reviewer settled it.
	AttributionConfirmed AttributionStatus = "confirmed"
	// AttributionRejected: the current speaker_id was examined and refused, so
	// the narrator must not attribute the line.
	AttributionRejected AttributionStatus = "rejected"
)

var attributionStatuses = enumTable[AttributionStatus]{field: "attribution_status", values: []AttributionStatus{
	AttributionUnknown, AttributionProposed, AttributionConfirmed, AttributionRejected,
}}

// String implements fmt.Stringer.
func (s AttributionStatus) String() string { return attributionStatuses.String(s) }

// Valid reports whether the value is known.
func (s AttributionStatus) Valid() bool { return attributionStatuses.Valid(s) }

// ParseAttributionStatus reads an attribution status from wire text.
func ParseAttributionStatus(s string) (AttributionStatus, error) { return attributionStatuses.Parse(s) }

//
// EdgeType — message_edges.edge_type
//

// EdgeType is the provenance and behaviour of one dialogue transition
// (docs/spec/04 "Dialogue graph").
type EdgeType string

const (
	// EdgeNext is the ordinary continuation, from explicit source metadata
	// where the format has any.
	EdgeNext EdgeType = "next"
	// EdgeBranch is a player-selectable alternative successor.
	EdgeBranch EdgeType = "branch"
	// EdgeRepeat is the same message becoming current again (an NPC saying it
	// twice), which the runtime must not speak twice by accident.
	EdgeRepeat EdgeType = "repeat"
	// EdgeContextual depends on a condition the game evaluates.
	EdgeContextual EdgeType = "contextual"
	// EdgeInferred is a transition nobody observed: the graph builder guessed it
	// from source order or scene sequence, and it must never outrank contrary
	// OCR text.
	EdgeInferred EdgeType = "inferred"
)

var edgeTypes = enumTable[EdgeType]{field: "edge_type", values: []EdgeType{
	EdgeNext, EdgeBranch, EdgeRepeat, EdgeContextual, EdgeInferred,
}}

// String implements fmt.Stringer.
func (t EdgeType) String() string { return edgeTypes.String(t) }

// Valid reports whether the value is known.
func (t EdgeType) Valid() bool { return edgeTypes.Valid(t) }

// ParseEdgeType reads an edge type from wire text.
func ParseEdgeType(s string) (EdgeType, error) { return edgeTypes.Parse(s) }

// MaxConfidence is the highest trust an edge of this type may carry. An
// inferred edge that claims 1.0 contradicts its own name (docs/spec/04: "mark
// lower confidence"), so the ceiling is part of the type, not a convention.
func (t EdgeType) MaxConfidence() float64 {
	switch t {
	case EdgeNext, EdgeBranch, EdgeRepeat:
		return 1
	case EdgeContextual:
		return 1
	case EdgeInferred:
		return 0.6
	}
	return 1
}

//
// CharacterStatus — characters.status
//

// CharacterStatus is the editorial lifecycle of a character entry.
type CharacterStatus string

const (
	// CharacterProposed: emitted by a parser or an agent, unaudited.
	CharacterProposed CharacterStatus = "proposed"
	// CharacterConfirmed: a curator or verified sources established it.
	CharacterConfirmed CharacterStatus = "confirmed"
	// CharacterRetired: kept for history (its audio and corrections refer to
	// it) but out of the active roster.
	CharacterRetired CharacterStatus = "retired"
)

var characterStatuses = enumTable[CharacterStatus]{field: "status", values: []CharacterStatus{
	CharacterProposed, CharacterConfirmed, CharacterRetired,
}}

// String implements fmt.Stringer.
func (s CharacterStatus) String() string { return characterStatuses.String(s) }

// Valid reports whether the value is known.
func (s CharacterStatus) Valid() bool { return characterStatuses.Valid(s) }

// ParseCharacterStatus reads a character status from wire text.
func ParseCharacterStatus(s string) (CharacterStatus, error) { return characterStatuses.Parse(s) }

//
// RegionType and DetectMethod — regions
//

// RegionType is the on-screen role of a crop, which selects OCR and
// speech-rendering defaults (docs/spec/04 "Visual profiles").
type RegionType string

const (
	RegionTextbox      RegionType = "textbox"
	RegionSpeakerLabel RegionType = "speaker_label"
	RegionSubtitle     RegionType = "subtitle"
	RegionSign         RegionType = "sign"
	RegionItem         RegionType = "item"
	RegionMenu         RegionType = "menu"
	RegionTutorial     RegionType = "tutorial"
	RegionHUD          RegionType = "hud"
	RegionOther        RegionType = "other"
)

var regionTypes = enumTable[RegionType]{field: "region_type", values: []RegionType{
	RegionTextbox, RegionSpeakerLabel, RegionSubtitle, RegionSign, RegionItem,
	RegionMenu, RegionTutorial, RegionHUD, RegionOther,
}}

// String implements fmt.Stringer.
func (t RegionType) String() string { return regionTypes.String(t) }

// Valid reports whether the value is known.
func (t RegionType) Valid() bool { return regionTypes.Valid(t) }

// ParseRegionType reads a region type from wire text.
func ParseRegionType(s string) (RegionType, error) { return regionTypes.Parse(s) }

// DetectMethod is how a region's rectangle was obtained, which feeds the
// reliability weighting in docs/spec/04.
type DetectMethod string

const (
	DetectManual         DetectMethod = "manual"
	DetectAutoDiscovered DetectMethod = "auto_discovered"
	DetectImported       DetectMethod = "imported"
	DetectInferred       DetectMethod = "inferred"
)

var detectMethods = enumTable[DetectMethod]{field: "detect_method", values: []DetectMethod{
	DetectManual, DetectAutoDiscovered, DetectImported, DetectInferred,
}}

// String implements fmt.Stringer.
func (m DetectMethod) String() string { return detectMethods.String(m) }

// Valid reports whether the value is known.
func (m DetectMethod) Valid() bool { return detectMethods.Valid(m) }

// ParseDetectMethod reads a detection method from wire text.
func ParseDetectMethod(s string) (DetectMethod, error) { return detectMethods.Parse(s) }

//
// AssertionMethod — speaker_assertions.method
//

// AssertionMethod is how a candidate speaker was arrived at.
type AssertionMethod string

const (
	// AssertionOnScreenLabel: a name printed beside the box. Strong, but still
	// only an assertion (docs/spec/02 refuses to treat it as identity).
	AssertionOnScreenLabel AssertionMethod = "on_screen_label"
	// AssertionScriptOrder comes from a supplied script's speaker column.
	AssertionScriptOrder AssertionMethod = "script_order"
	// AssertionDumpMetadata comes from a data dump's own linkage.
	AssertionDumpMetadata AssertionMethod = "dump_metadata"
	// AssertionVisualAnchor is a sprite/portrait/clothing match.
	AssertionVisualAnchor AssertionMethod = "visual_anchor"
	// AssertionModel is an inference call's answer, with its request id in the
	// evidence column.
	AssertionModel AssertionMethod = "model"
	// AssertionManual is a curator's choice, not a correction of anything.
	AssertionManual AssertionMethod = "manual"
	// AssertionManualCorrection fixes a wrong speaker and is also a training
	// label (docs/spec/04: unknown mappings become first-class samples).
	AssertionManualCorrection AssertionMethod = "manual_correction"
)

var assertionMethods = enumTable[AssertionMethod]{field: "method", values: []AssertionMethod{
	AssertionOnScreenLabel, AssertionScriptOrder, AssertionDumpMetadata, AssertionVisualAnchor,
	AssertionModel, AssertionManual, AssertionManualCorrection,
}}

// String implements fmt.Stringer.
func (m AssertionMethod) String() string { return assertionMethods.String(m) }

// Valid reports whether the value is known.
func (m AssertionMethod) Valid() bool { return assertionMethods.Valid(m) }

// ParseAssertionMethod reads an assertion method from wire text.
func ParseAssertionMethod(s string) (AssertionMethod, error) { return assertionMethods.Parse(s) }

// FromReviewer reports whether only a curator (not an automated path) could
// have produced this assertion, which docs/spec/08's ACLs check before allowing
// a write.
func (m AssertionMethod) FromReviewer() bool {
	return m == AssertionManual || m == AssertionManualCorrection
}

//
// CorrectionKind / CorrectionTarget / CorrectionStatus — corrections
//

// CorrectionKind is what kind of mistake a correction fixes.
type CorrectionKind string

const (
	CorrectionSpeaker        CorrectionKind = "speaker"
	CorrectionText           CorrectionKind = "text"
	CorrectionSpeechText     CorrectionKind = "speech_text"
	CorrectionScene          CorrectionKind = "scene"
	CorrectionRegion         CorrectionKind = "region"
	CorrectionClassification CorrectionKind = "classification"
	CorrectionEdge           CorrectionKind = "edge"
	CorrectionOther          CorrectionKind = "other"
)

var correctionKinds = enumTable[CorrectionKind]{field: "kind", values: []CorrectionKind{
	CorrectionSpeaker, CorrectionText, CorrectionSpeechText, CorrectionScene,
	CorrectionRegion, CorrectionClassification, CorrectionEdge, CorrectionOther,
}}

// String implements fmt.Stringer.
func (k CorrectionKind) String() string { return correctionKinds.String(k) }

// Valid reports whether the value is known.
func (k CorrectionKind) Valid() bool { return correctionKinds.Valid(k) }

// ParseCorrectionKind reads a correction kind from wire text.
func ParseCorrectionKind(s string) (CorrectionKind, error) { return correctionKinds.Parse(s) }

// CorrectionTarget is the table a correction names.
type CorrectionTarget string

const (
	TargetMessage     CorrectionTarget = "message"
	TargetMessagePage CorrectionTarget = "message_page"
	TargetMessageEdge CorrectionTarget = "message_edge"
	TargetCharacter   CorrectionTarget = "character"
	TargetScene       CorrectionTarget = "scene"
	TargetRegion      CorrectionTarget = "region"
	TargetOther       CorrectionTarget = "other"
)

var correctionTargets = enumTable[CorrectionTarget]{field: "target_type", values: []CorrectionTarget{
	TargetMessage, TargetMessagePage, TargetMessageEdge, TargetCharacter,
	TargetScene, TargetRegion, TargetOther,
}}

// String implements fmt.Stringer.
func (t CorrectionTarget) String() string { return correctionTargets.String(t) }

// Valid reports whether the value is known.
func (t CorrectionTarget) Valid() bool { return correctionTargets.Valid(t) }

// ParseCorrectionTarget reads a correction target type from wire text.
func ParseCorrectionTarget(s string) (CorrectionTarget, error) { return correctionTargets.Parse(s) }

// CorrectionStatus is the review state of a correction.
type CorrectionStatus string

const (
	CorrectionProposed   CorrectionStatus = "proposed"
	CorrectionAccepted   CorrectionStatus = "accepted"
	CorrectionRejected   CorrectionStatus = "rejected"
	CorrectionSuperseded CorrectionStatus = "superseded"
)

var correctionStatuses = enumTable[CorrectionStatus]{field: "status", values: []CorrectionStatus{
	CorrectionProposed, CorrectionAccepted, CorrectionRejected, CorrectionSuperseded,
}}

// String implements fmt.Stringer.
func (s CorrectionStatus) String() string { return correctionStatuses.String(s) }

// Valid reports whether the value is known.
func (s CorrectionStatus) Valid() bool { return correctionStatuses.Valid(s) }

// ParseCorrectionStatus reads a correction status from wire text.
func ParseCorrectionStatus(s string) (CorrectionStatus, error) { return correctionStatuses.Parse(s) }

//
// JobState and AttemptOutcome — jobs, job_attempts
//

// JobState is the durable queue state (docs/spec/02 "Transaction workflows",
// docs/spec/08 /jobs).
type JobState string

const (
	JobQueued        JobState = "queued"
	JobRunning       JobState = "running"
	JobSucceeded     JobState = "succeeded"
	JobFailed        JobState = "failed"
	JobCancelled     JobState = "cancelled"
	JobRecoverable   JobState = "recoverable"
	JobRequiresInput JobState = "requires_input"
)

var jobStates = enumTable[JobState]{field: "state", values: []JobState{
	JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled, JobRecoverable, JobRequiresInput,
}}

// String implements fmt.Stringer.
func (s JobState) String() string { return jobStates.String(s) }

// Valid reports whether the value is known.
func (s JobState) Valid() bool { return jobStates.Valid(s) }

// ParseJobState reads a job state from wire text.
func ParseJobState(s string) (JobState, error) { return jobStates.Parse(s) }

// Claimable reports whether a worker may start this job. requires_input is
// parked on a human answer and must never be picked up; recoverable needs the
// reconciler's decision (resume from resume_token or fail), not a fresh run.
func (s JobState) Claimable() bool { return s == JobQueued }

// Terminal reports whether the job will not run again on its own.
func (s JobState) Terminal() bool {
	return s == JobSucceeded || s == JobFailed || s == JobCancelled
}

// CanTransition reports whether the job may move from s to to.
func (s JobState) CanTransition(to JobState) bool { return JobStateCanTransition(s, to) }

// JobStateCanTransition reports the queue's state machine: a crash leaves
// running → recoverable, a retry moves a failed/cancelled job back to queued,
// and a worker parks a job on requires_input (docs/spec/08's JOB_REQUIRES_INPUT).
func JobStateCanTransition(from, to JobState) bool {
	if !from.Valid() || !to.Valid() {
		return false
	}
	switch from {
	case JobQueued:
		return to == JobRunning || to == JobCancelled || to == JobRequiresInput
	case JobRunning:
		return to == JobSucceeded || to == JobFailed || to == JobCancelled ||
			to == JobRequiresInput || to == JobRecoverable
	case JobRecoverable:
		return to == JobRunning || to == JobFailed || to == JobCancelled
	case JobRequiresInput:
		return to == JobQueued || to == JobCancelled || to == JobFailed
	case JobFailed, JobCancelled:
		// Only an explicit retry, which resets the attempt counter's budget
		// rather than exceeding it (see the attempts <= max_attempts CHECK).
		return to == JobQueued
	case JobSucceeded:
		return false
	}
	return false
}

// AttemptOutcome is how one attempt ended (job_attempts.outcome).
type AttemptOutcome string

const (
	AttemptRunning       AttemptOutcome = "running"
	AttemptSucceeded     AttemptOutcome = "succeeded"
	AttemptFailed        AttemptOutcome = "failed"
	AttemptCancelled     AttemptOutcome = "cancelled"
	AttemptTimedOut      AttemptOutcome = "timed_out"
	AttemptCrashed       AttemptOutcome = "crashed"
	AttemptRequiresInput AttemptOutcome = "requires_input"
)

var attemptOutcomes = enumTable[AttemptOutcome]{field: "outcome", values: []AttemptOutcome{
	AttemptRunning, AttemptSucceeded, AttemptFailed, AttemptCancelled,
	AttemptTimedOut, AttemptCrashed, AttemptRequiresInput,
}}

// String implements fmt.Stringer.
func (o AttemptOutcome) String() string { return attemptOutcomes.String(o) }

// Valid reports whether the value is known.
func (o AttemptOutcome) Valid() bool { return attemptOutcomes.Valid(o) }

// ParseAttemptOutcome reads an attempt outcome from wire text.
func ParseAttemptOutcome(s string) (AttemptOutcome, error) { return attemptOutcomes.Parse(s) }

// Retryable reports whether another attempt at the same input is worth making.
// A deadline or a crash may be transient; a deterministic validation failure is
// not, and retrying it only burns the provider budget that docs/spec/06 bounds.
func (o AttemptOutcome) Retryable() bool {
	return o == AttemptTimedOut || o == AttemptCrashed
}
