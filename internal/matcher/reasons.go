package matcher

// Reason codes explain a ranking in one token, so an event trace, the developer
// UI and a later calibration dataset all agree on what happened
// (docs/spec/17 "Confidence combination strategy": "Persist reason codes").
// Only the codes this package can actually justify are defined here;
// CLEF_DISAMBIGUATED, UNKNOWN_SPEAKER and NO_NARRATABLE_TEXT belong to the
// decision layer that runs after this one and are deliberately absent, so a
// caller cannot mistake a local guess for a model-adjudicated result
// (AGENTS.md "no simulation theater").
const (
	// ReasonExact means the observation folded to exactly one stored message's
	// key — the strongest evidence this package can produce on text alone.
	ReasonExact = "MATCH_EXACT"

	// ReasonGraphNeighbour means the winner was picked, or disambiguated, by a
	// graph edge out of a recently confirmed message. It is reported when text
	// alone left the field open — several messages with the same wording, or two
	// near-identical candidates — and the transition evidence chose between them
	// (docs/spec/05 "Scene belief graph").
	ReasonGraphNeighbour = "GRAPH_NEIGHBOR"

	// ReasonPredictivePrefix means the observation was still being revealed,
	// matched one complete canonical message as a prefix, and nothing else.
	// docs/spec/05 "Predictive playback policy" allows commitment on this only
	// with an explicit risk policy; the code exists so the policy can require it.
	ReasonPredictivePrefix = "PREDICTIVE_PREFIX"

	// ReasonFuzzy means the winner came from character/token similarity rather
	// than an exact key or the graph. docs/spec/17 does not list it; it is added
	// here because without it MATCH_EXACT and GRAPH_NEIGHBOR would be flanked by
	// ABSTAINED, and a reviewer could not tell "the OCR misread a letter" from
	// "we refused to answer". Documented as a local extension.
	ReasonFuzzy = "MATCH_FUZZY"

	// ReasonAbstained means no answer was good enough. Abstain is a normal,
	// expected output — the honest result for a three-character fragment, for
	// "Yes." spoken by four NPCs, and for a screen that is not dialogue at all.
	ReasonAbstained = "ABSTAINED"
)

// Component score keys, exactly as docs/spec/05 "Local candidate retrieval"
// names them. They are exported as constants rather than typed loosely because
// the dev UI, the event trace and the calibration importer all read them by
// name, and a typo in a string that silently means "no evidence" is the kind of
// bug a golden fixture will not catch.
const (
	ComponentText              = "text"
	ComponentPrefixCoverage    = "prefix_coverage"
	ComponentScene             = "scene"
	ComponentEdgePrior         = "edge_prior"
	ComponentVisibility        = "visibility"
	ComponentTextKind          = "text_kind"
	ComponentSpeakerContext    = "speaker_context"
	ComponentEdition           = "edition"
	ComponentSourceReliability = "source_reliability"
)

// Page evidence keys (ADR-0184): diagnostic additions to Candidate.Components,
// not weighted components. ComponentPage is the 1-based ordinal of the display
// page that won the text component for a multi-box message (0 means the whole
// text won, or the message carries no pages); ComponentPageTextScore is that
// page's text score. They are reported outside ComponentOrder because they
// carry no weight in the log-linear sum — the page score IS the text
// component's value when it wins — so componentCount and the weights are
// untouched.
const (
	ComponentPage          = "page"
	ComponentPageTextScore = "page_text_score"
)

// componentCount is the fixed size of a candidate's component array; the array
// is a fixed-size Go array rather than a map so scoring allocates nothing per
// candidate. It must equal len(ComponentOrder); TestComponentCountMatchesOrder
// asserts that, because a constant and a slice literal drift apart silently.
const componentCount = 9

// ComponentOrder is the display order of the decomposition, so a trace and the
// developer UI list components the same way round (docs/spec/05 "Expose top N
// and constituent scores in dev mode").
var ComponentOrder = []string{
	ComponentText,
	ComponentPrefixCoverage,
	ComponentScene,
	ComponentEdgePrior,
	ComponentVisibility,
	ComponentTextKind,
	ComponentSpeakerContext,
	ComponentEdition,
	ComponentSourceReliability,
}
