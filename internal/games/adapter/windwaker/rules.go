package windwaker

// SpeechRenderingRules: the per-game mapping from a BMG control tag to how it
// speaks (docs/spec/02 "Normalization and markup": "speech_text is an
// editable rendering that expands button icons/variable names using a mapping
// ... and honors pause, emphasis and nonspoken elements. A per-game
// SpeechRenderingRules revision must be included in audio cache keys.").
//
// The table is *data*, not code: a flat list of (group,type) entries that a
// future revision edits by changing a line, never by changing the renderer.
// It is versioned by RulesRevision, which the adapter stamps into the parse
// result (Warnings, column "rules") and the import command writes into
// sources.coverage_notes — that stamp is what audio cache keys derive from,
// so a table change is detectable as stale cache instead of a silent voice
// change.
//
// Unknown tags default to RuleRemove (a deliberate, documented default, not a
// mistake): a tag this revision has not classified must never invent speech.
// The real-file test reports coverage as "share of tag occurrences whose
// (group,type) has an explicit entry", so a growing unknown-tag ratio is a
// visible regression, not a silent one.
//
// Every entry below was classified from structural inspection of the user's
// own disc (counts and contexts only; no message text was recorded): the
// (group,type) pairs and their counts are the evidence, and each comment says
// what role the context showed. Pairs that exist only in a single occurrence
// are classified on that one example and flagged as low-confidence.

import "fmt"

// RulesRevision is the table's identity for cache keys. Bump it whenever the
// table changes; the adapter semver bumps with it, and old caches keyed on a
// lower revision must not be served from.
const RulesRevision = "v1"

// RuleAction is one table entry's verdict for a (group,type) pair.
type RuleAction int

const (
	// RuleRemove drops the tag: nonspoken UI markers, colours, emphasis
	// switches and icon glyphs whose spoken form is redundant
	// (docs/spec/02: "on-screen icon not uttered if redundant").
	RuleRemove RuleAction = iota
	// RuleSpace renders the tag as a single space.
	RuleSpace
	// RuleNewline renders the tag as a line break; import collapses it to a
	// space in both normalized and speech text because a display wrap is not a
	// spoken pause. The action is kept for a table revision that maps a real
	// newline tag.
	RuleNewline
	// RulePause renders a spoken pause of Rule.Ms milliseconds.
	RulePause
	// RulePlayerName renders the textnorm player placeholder
	// ("{player}"), which the TTS stage resolves to the profile's configured
	// player name (docs/spec/02 "explicit player-name placeholders").
	RulePlayerName
	// RuleButton renders the button icon as its spoken name, "<name> button"
	// (docs/spec/02's example: the A icon speaks as "A button").
	RuleButton
)

func (a RuleAction) String() string {
	switch a {
	case RuleRemove:
		return "remove"
	case RuleSpace:
		return "space"
	case RuleNewline:
		return "newline"
	case RulePause:
		return "pause"
	case RulePlayerName:
		return "player_name"
	case RuleButton:
		return "button"
	default:
		return fmt.Sprintf("action(%d)", int(a))
	}
}

// Rule is one table entry. Ms applies to RulePause; Button to RuleButton;
// Deliberate marks a remove that was a classified decision (versus the
// unknown-tag default), which the coverage report counts separately so an
// honest "we chose silence" is not mistaken for "we did not look".
type Rule struct {
	Action     RuleAction
	Ms         int
	Button     string
	Deliberate bool
}

// Rules is the resolved table plus its default for unlisted pairs.
type Rules struct {
	Revision string
	Default  Rule
	entries  map[[2]int]Rule
}

// NewRules resolves the default Wind Waker table. It exists so a test (or a
// future per-edition override) can start from the documented defaults and
// mutate entries — the table stays data.
func NewRules() *Rules {
	entries := make(map[[2]int]Rule, len(defaultEntries))
	for _, e := range defaultEntries {
		entries[[2]int{e.group, e.typ}] = e.rule
	}
	return &Rules{
		Revision: RulesRevision,
		Default:  Rule{Action: RuleRemove},
		entries:  entries,
	}
}

// Lookup returns the rule for a (group,type) pair, or the default (remove)
// when the pair has no entry.
func (r *Rules) Lookup(group, typ int) Rule {
	if e, ok := r.entries[[2]int{group, typ}]; ok {
		return e
	}
	return r.Default
}

// Entries reports the explicit table size, for manifests and diagnostics.
func (r *Rules) Entries() int { return len(r.entries) }

// entry is one line of the data table.
type entry struct {
	group, typ int
	rule       Rule
}

// rm/... are shorthand constructors for table lines.
func rm(group, typ int) entry {
	return entry{group, typ, Rule{Action: RuleRemove, Deliberate: true}}
}

var defaultEntries = []entry{
	// ── group 0: text-level controls ────────────────────────────────────────
	// (0,0): the player's own name — letters addressed to the player ("Dear …"),
	// second-person address across the script. 357 occurrences.
	entry{0, 0, Rule{Action: RulePlayerName}},
	// (0,1)/(0,2): message-level open/close markers that bracket system
	// notifications; nothing is said for them.
	rm(0, 1), rm(0, 2),
	// (0,3)-(0,6): unclassified single-digit markers seen only in a handful of
	// messages; low confidence, nonspoken.
	rm(0, 3), rm(0, 4), rm(0, 5), rm(0, 6),
	// (0,7): the typewriter pause. Occurrences follow sentences and mid-word
	// breaks ("Th-…The milk…"); the args byte varies (0x00-0x3C) as a duration
	// hint. Fixed 250 ms here: a middle of the observed range in frame units,
	// and the args value is preserved in MarkupJSON for a revision that wants
	// to scale by it.
	entry{0, 7, Rule{Action: RulePause, Ms: 250, Deliberate: true}},
	// (0,8)/(0,9): choice-list markers preceding menu option lines; the
	// options themselves are plain text runs.
	rm(0, 8), rm(0, 9),
	// Button icons, group 0, type 0x0A-0x1F: the GameCube control set.
	// (0,10) and (0,11) are confirmed by their menu contexts (decide /
	// cancel); (0,14) by its "hold to defend" context. The remaining icons
	// are nonspoken: their letters are not confirmed by this evidence and an
	// icon glyph is exactly the case docs/spec/02 says to skip.
	entry{0, 0x0A, Rule{Action: RuleButton, Button: "A", Deliberate: true}},
	entry{0, 0x0B, Rule{Action: RuleButton, Button: "B", Deliberate: true}},
	rm(0, 0x0C), rm(0, 0x0D),
	entry{0, 0x0E, Rule{Action: RuleButton, Button: "Z", Deliberate: true}},
	// (0,0x0F)-(0,0x17): remaining control icons (L/R/stick/d-pad ranges).
	rm(0, 0x0F), rm(0, 0x10), rm(0, 0x11), rm(0, 0x12), rm(0, 0x13),
	rm(0, 0x14), rm(0, 0x15), rm(0, 0x16),
	// (0,0x18)-(0,0x22): directional/stick variants seen in "swing with …"
	// style instructions and map markers.
	rm(0, 0x17), rm(0, 0x18), rm(0, 0x19), rm(0, 0x1A), rm(0, 0x1B),
	rm(0, 0x1C), rm(0, 0x1D), rm(0, 0x1E), rm(0, 0x1F), rm(0, 0x20),
	rm(0, 0x21), rm(0, 0x22),
	// (0,0x23)-(0,0x31): further control/position icons.
	rm(0, 0x23), rm(0, 0x24), rm(0, 0x25), rm(0, 0x26), rm(0, 0x27),
	rm(0, 0x28), rm(0, 0x29), rm(0, 0x2A), rm(0, 0x2B), rm(0, 0x2C),
	rm(0, 0x2D), rm(0, 0x2E), rm(0, 0x2F), rm(0, 0x30), rm(0, 0x31),
	// (0,0x32)-(0,0x38): low-frequency icons/markers.
	rm(0, 0x32), rm(0, 0x33), rm(0, 0x34), rm(0, 0x35), rm(0, 0x36),
	rm(0, 0x37), rm(0, 0x38),
	// (0,0x39)-(0,0x4A): low-frequency markers and variable slots.
	rm(0, 0x39), rm(0, 0x3A), rm(0, 0x3B), rm(0, 0x3C), rm(0, 0x3D),
	rm(0, 0x3E), rm(0, 0x3F), rm(0, 0x40), rm(0, 0x41), rm(0, 0x42),
	rm(0, 0x43), rm(0, 0x44), rm(0, 0x45), rm(0, 0x46), rm(0, 0x47),
	rm(0, 0x48), rm(0, 0x49), rm(0, 0x4A),
	// (0,0x4B)-(0,0x56): low-frequency markers.
	rm(0, 0x4B), rm(0, 0x4C), rm(0, 0x4D), rm(0, 0x4E), rm(0, 0x4F),
	rm(0, 0x50), rm(0, 0x51), rm(0, 0x52), rm(0, 0x53), rm(0, 0x54),
	rm(0, 0x55), rm(0, 0x56),
	// (0,0x57): tripled emphasis glyph at message ends; nonspoken.
	rm(0, 0x57),
	// (0,0x58)-(0,0x6A): low-frequency markers and dynamic value slots
	// (prices, quantities); a value the game computed is not speech this
	// revision can reconstruct.
	rm(0, 0x58), rm(0, 0x59), rm(0, 0x5A), rm(0, 0x5B), rm(0, 0x5C),
	rm(0, 0x5D), rm(0, 0x5E), rm(0, 0x5F), rm(0, 0x60), rm(0, 0x61),
	rm(0, 0x62), rm(0, 0x63), rm(0, 0x64), rm(0, 0x65), rm(0, 0x66),
	rm(0, 0x67), rm(0, 0x68), rm(0, 0x69), rm(0, 0x6A),
	// (0,0x6B)-(0,0x74): low-frequency markers.
	rm(0, 0x6B), rm(0, 0x6C), rm(0, 0x6D), rm(0, 0x6E), rm(0, 0x6F),
	rm(0, 0x70), rm(0, 0x71), rm(0, 0x72), rm(0, 0x73), rm(0, 0x74),
	// (0,0x75)-(0,0x78): tail of the observed range.
	rm(0, 0x75), rm(0, 0x76), rm(0, 0x77), rm(0, 0x78),

	// ── group 1: per-character voice/emotion codes ─────────────────────────
	// Group 1 tags sit at the head of NPC dialogue lines with a per-character
	// parameter; they select a voice timbre or delivery, not text. Only the
	// pairs observed on the user's disc are listed; anything else falls to
	// the unknown-tag default (remove) and shows up in the coverage report.
	rm(1, 1), rm(1, 2), rm(1, 3), rm(1, 4), rm(1, 5), rm(1, 6), rm(1, 7), rm(1, 8),
	rm(1, 10), rm(1, 30), rm(1, 36), rm(1, 42), rm(1, 44), rm(1, 52), rm(1, 59), rm(1, 65),
	rm(1, 69), rm(1, 70), rm(1, 84), rm(1, 87), rm(1, 91), rm(1, 125), rm(1, 126), rm(1, 127),
	rm(1, 128), rm(1, 134), rm(1, 135), rm(1, 140), rm(1, 141), rm(1, 142), rm(1, 159), rm(1, 160),
	rm(1, 163), rm(1, 167), rm(1, 228), rm(1, 240), rm(1, 256), rm(1, 257), rm(1, 258), rm(1, 259),
	rm(1, 260), rm(1, 261), rm(1, 262), rm(1, 263), rm(1, 264), rm(1, 265), rm(1, 266), rm(1, 267),
	rm(1, 268), rm(1, 269), rm(1, 270), rm(1, 271), rm(1, 272), rm(1, 273), rm(1, 274), rm(1, 275),
	rm(1, 276), rm(1, 277),

	// ── group 2: speaker/emotion codes ──────────────────────────────────────
	rm(2, 12), rm(2, 21), rm(2, 23), rm(2, 29), rm(2, 31),

	// ── group 3 ─────────────────────────────────────────────────────────────
	rm(3, 10),

	// ── group 0xFF: display effects ─────────────────────────────────────────
	// (0xFF,0): emphasis start/end/alternate markers that wrap spans; the
	// wrapped text runs are spoken, the switches are not. 5232 occurrences.
	rm(0xFF, 0),
	// (0xFF,1): a colour specification (parameter in args); purely visual.
	rm(0xFF, 1),
}
