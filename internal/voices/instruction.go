package voices

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DesignInstructionCap is the default ceiling (in runes, not bytes) for the
// provider-facing VoiceDesign instruction. The live gateway rejects
// instructions longer than 500 characters (observed 400: "Instructions too
// long (max 500 characters)"), and a ~900-char research-grounded brief is
// the normal shape (ADR-0046). The full brief stays on the revision row —
// the UI edits it and the audition rows record it; only the payload sent to
// the design checkpoint is derived and capped (ADR-0050).
const DesignInstructionCap = 500

// briefFieldOrder is the priority order of the structured voice_brief fields
// (docs/spec/03: vocal age impression, timbre, cadence, emotional baseline,
// language and pronunciation notes). The voice-design fields (timbre, age,
// pace, emotion, accent) are listed before the prose notes, so when the cap
// is reached the derivation drops prose before it cuts a design field.
var briefFieldOrder = []struct {
	key   string
	label string
}{
	{"timbre", "timbre"},
	{"vocal_age_impression", "age"},
	{"cadence", "pace"},
	{"emotional_baseline", "emotion"},
	{"language_notes", "accent"},
	{"pronunciation_notes", "pronunciation"},
}

// DesignInstruction derives the provider-facing VoiceDesign instruction from
// a stored voice brief, deterministically, within maxRunes characters
// (maxRunes <= 0 uses DesignInstructionCap). A structured brief (the JSON
// object analyze_character produces, optionally wrapped in a "voice_brief"
// key) is re-serialised with the voice-design fields first and the prose
// notes last; segments that do not fit the cap are dropped from the lowest
// priority up, so the cap drops prose before it cuts a design field. A
// brief that is not a usable JSON object is treated as prose and cut at a
// word boundary. The result never cuts mid-word: the last space at or
// before the cap wins, and trailing punctuation is trimmed. The only hard
// cut is when a single word exceeds the cap — an empty instruction would
// be a worse failure than a truncated one.
func DesignInstruction(brief string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = DesignInstructionCap
	}
	brief = strings.TrimSpace(brief)
	if brief == "" {
		return ""
	}
	segments, structured := briefSegments(brief)
	if !structured {
		return fitRunes(brief, maxRunes)
	}
	out := ""
	for i, seg := range segments {
		cand := seg
		if out != "" {
			cand = out + "; " + seg
		}
		if utf8.RuneCountInString(cand) > maxRunes {
			if i == 0 {
				// Even the top-priority field alone exceeds the cap:
				// cut it at a word boundary instead of sending an
				// instruction shaped by nothing.
				out = fitRunes(seg, maxRunes)
			}
			break
		}
		out = cand
	}
	return out
}

// briefSegments reports whether the brief is the structured voice_brief JSON
// object (usable string fields only) and returns its labelled segments in
// priority order: the known brief fields first, then any remaining string
// fields sorted by key, so an edited or wrapped brief stays deterministic
// despite Go's random map iteration. An unparseable brief, a brief with no
// usable string field, or a brief whose only values are non-strings all
// make it prose.
func briefSegments(brief string) ([]string, bool) {
	var obj map[string]any
	if err := json.Unmarshal([]byte(brief), &obj); err != nil {
		return nil, false
	}
	if inner, ok := obj["voice_brief"].(map[string]any); ok {
		obj = inner
	}
	labels := make(map[string]string, len(briefFieldOrder))
	for _, f := range briefFieldOrder {
		labels[f.key] = f.label
	}
	order := make([]string, 0, len(obj))
	for _, f := range briefFieldOrder {
		if _, ok := obj[f.key]; ok {
			order = append(order, f.key)
		}
	}
	var extra []string
	for k := range obj {
		if _, known := labels[k]; !known && k != "voice_brief" {
			extra = append(extra, k)
		}
	}
	sort.Strings(extra)
	order = append(order, extra...)

	segments := make([]string, 0, len(order))
	for _, k := range order {
		v, ok := obj[k].(string)
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		label := labels[k]
		if label == "" {
			label = k // edited or unknown brief field: the key is its own label
		}
		segments = append(segments, label+": "+strings.TrimSpace(v))
	}
	if len(segments) == 0 {
		return nil, false
	}
	return segments, true
}

// fitRunes cuts s to at most maxRunes runes at a word boundary: the last
// space at or before the cap, then trailing punctuation and whitespace.
// When the first word itself is longer than the cap it hard-cuts at the cap
// (a rune boundary — never mid-rune, even for multi-byte text) because a
// voice with zero instruction is worse than a truncated one.
func fitRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	cut := maxRunes
	for cut > 0 && !unicode.IsSpace(r[cut-1]) {
		cut--
	}
	if cut == 0 {
		return strings.TrimRight(string(r[:maxRunes]), " \t.;,")
	}
	out := strings.TrimRight(string(r[:cut]), " \t.;,")
	if out == "" {
		return strings.TrimRight(string(r[:maxRunes]), " \t.;,")
	}
	return out
}
