package windwaker

// Rendering from the lossless token stream to the three stored text columns
// (docs/spec/02 "Normalization and markup"):
//
//   - OriginalText: the byte-faithful escape (escape.go) — never re-derived
//     from the other columns;
//   - MarkupJSON:   the token AST with source offsets (the lossless source of
//     truth, verbatim from the bmg package);
//   - NormalizedText: the text runs with control tags removed and line breaks
//     (the 0x0A display wrap and any newline rule) folded to spaces, then
//     through textnorm.Normalize with the caller's options — the matching key;
//   - SpeechText:   the text runs with the SpeechRenderingRules applied — the
//     editable rendering the TTS stage consumes.
//
// Both derived texts walk the tokens left to right, so a tag renders exactly
// where it sat in the pool; nothing is reordered, guessed or dropped beyond
// the rule table's decisions.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/bmg"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// markupToken is one element of the markup_json array. Raw is the token's
// exact pool bytes as lowercase hex (with the token's Offset and Length, the
// AST reconstructs the message byte for byte without reading original_text).
type markupToken struct {
	Kind   string `json:"kind"`
	Offset int    `json:"offset"`
	Length int    `json:"length"`
	Group  *int   `json:"group,omitempty"`
	Type   *int   `json:"type,omitempty"`
	Args   string `json:"args,omitempty"`
	Raw    string `json:"raw"`
}

func kindName(k bmg.TokenKind) string {
	switch k {
	case bmg.TokText:
		return "text"
	case bmg.TokTag:
		return "tag"
	case bmg.TokInvalid:
		return "invalid"
	}
	return fmt.Sprintf("kind(%d)", int(k))
}

// markupJSON marshals the token AST. It is always a JSON array (an empty
// message would be "[]"), which is what games.Message.Validate requires.
func markupJSON(tokens []bmg.Token) (json.RawMessage, error) {
	out := make([]markupToken, 0, len(tokens))
	for _, t := range tokens {
		mt := markupToken{
			Kind:   kindName(t.Kind),
			Offset: t.Offset,
			Length: t.Length,
			Raw:    hexLower(t.Raw),
		}
		if t.Kind != bmg.TokText {
			g, ty := t.Group, t.Type
			mt.Group = &g
			mt.Type = &ty
			if t.Kind == bmg.TokTag {
				mt.Args = hexLower(t.Args)
			}
		}
		out = append(out, mt)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("windwaker: marshal markup: %w", err)
	}
	return raw, nil
}

// hexLower is lowercase hex without the allocation a per-call Sprintf would
// cost at 10 000 messages.
func hexLower(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[2*i] = digits[c>>4]
		out[2*i+1] = digits[c&0xF]
	}
	return string(out)
}

// textFor walks the tokens for one of the two derived columns.
//
//	lineBreaks are the 0x0A bytes that Wind Waker uses as display line wraps
//	inside text runs: a wrap is layout, not speech, so both columns fold it to
//	a single space.
//
//	ruleFor returns the rendering for a control tag: in the normalized column
//	only space/newline rules contribute anything (everything else is removed),
//	while in the speech column the full rule applies.
func textFor(tokens []bmg.Token, ruleFor func(b bmg.Token) string) string {
	var b strings.Builder
	for _, t := range tokens {
		if t.Kind == bmg.TokText {
			for _, c := range t.Raw {
				if c == 0x0A {
					b.WriteByte(' ')
				} else {
					b.WriteByte(c)
				}
			}
			continue
		}
		b.WriteString(ruleFor(t))
	}
	return collapseSpaces(b.String())
}

// collapseSpaces joins whitespace runs to single spaces and trims the ends.
// Double spaces would survive otherwise (a wrap plus a following space), and
// a matching key or a spoken line must not depend on display spacing.
func collapseSpaces(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == ' ' || c == '\t' {
			if !prevSpace && b.Len() > 0 {
				b.WriteByte(' ')
			}
			prevSpace = true
			continue
		}
		prevSpace = false
		b.WriteByte(c)
	}
	return b.String()
}

// renderNormalized builds the normalized-column pre-normalization text: tags
// removed except space/newline rules, which contribute a space.
func renderNormalized(tokens []bmg.Token, rules *Rules) string {
	return textFor(tokens, func(t bmg.Token) string {
		r := rules.Lookup(t.Group, t.Type)
		switch r.Action {
		case RuleSpace, RuleNewline:
			return " "
		default:
			return ""
		}
	})
}

// renderSpeech builds the speech column with the full rule table.
func renderSpeech(tokens []bmg.Token, rules *Rules) string {
	return textFor(tokens, func(t bmg.Token) string {
		return speechFor(rules.Lookup(t.Group, t.Type))
	})
}

// speechFor renders one rule to its speech-column text. The pause marker is a
// literal, machine-readable bracket ([pause:250ms]): speech_text is the
// editable rendering, and the TTS stage (ADR-0022/0023 territory) converts
// markers to silence. It is deliberately not an ellipsis, which the game also
// uses as a spoken "…" the text runs already contain.
func speechFor(r Rule) string {
	switch r.Action {
	case RuleRemove:
		return ""
	case RuleSpace, RuleNewline:
		return " "
	case RulePause:
		return fmt.Sprintf("[pause:%dms]", r.Ms)
	case RulePlayerName:
		return textnorm.Placeholder
	case RuleButton:
		return r.Button + " button"
	}
	return ""
}

// messageTexts derives the four stored columns of one message from its token
// stream. It returns an empty normalizedPre (the pre-textnorm string) when the
// message carries no text at all — the caller skips such messages as empty.
func messageTexts(m bmg.Message, rules *Rules, normOpts textnorm.Options) (original, normalized, speech string, markup json.RawMessage, err error) {
	original = decodeOriginal(m.Tokens)
	markup, err = markupJSON(m.Tokens)
	if err != nil {
		return "", "", "", nil, err
	}
	pre := renderNormalized(m.Tokens, rules)
	if pre == "" {
		// No text runs (or only tags that remove): the row would violate
		// games.Message.Validate's non-empty normalizedText, so the caller
		// counts it as skipped rather than importing an empty line.
		return original, "", "", markup, nil
	}
	normalized = textnorm.Normalize(pre, normOpts)
	if normalized == "" {
		// textnorm removed everything (e.g. the run was punctuation only):
		// same skip, reported by the caller with the same count.
		return original, "", "", markup, nil
	}
	speech = renderSpeech(m.Tokens, rules)
	return original, normalized, speech, markup, nil
}

// maxOriginalLen bounds original_text the way games.Message.Validate does
// (8000 characters); a message that escapes past it is skipped rather than
// failing the whole import mid-transaction.
const maxOriginalLen = 8000

// textKind decides games.TextKind. The BMG INF1 attribute bytes carry no
// reliable kind signal in this file (the first attribute words form a
// per-message counter, not a class), so the adapter does not invent one:
// every imported message is dialogue; the only rows that are not are the
// empty/system-looking entries, which are skipped with a count instead of
// being stored as "other" (a stored "other" with no text could not validate
// anyway).
func textKind() games.TextKind { return games.TextDialogue }
