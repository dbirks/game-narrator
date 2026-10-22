package windwaker

// Display pages (ADR-0155): a Wind Waker script message often spans several
// on-screen boxes, and the narrator must speak only the box the player reached.
// docs/spec/02 keeps a message's pages separate from its utterance — a message
// can have several pages and one utterance can cross or split pages — and
// docs/spec/05 keeps a multi-page utterance from speaking past the page that is
// on screen. This file is where a BMG message becomes its pages: the pool's 0x0A
// display wrap (docs/adr/0031: the pool is pure ASCII plus 0x0A) and a newline
// control tag are the only two things that start a new display line, and a box
// holds at most MaxBoxLines of them.

import (
	"strings"

	"github.com/dbirks/game-narrator/internal/games/bmg"
)

// MaxBoxLines is the dialogue box's display line limit the paging derivation
// packs against: Wind Waker boxes hold at most 3-4 lines, so a box closes when
// the next display line would not fit. It is the observed box geometry the
// paging split is derived from, not a narrator policy — the policy layer that
// decides what may be spoken lives in docs/spec/05 / ADR-0155.
const MaxBoxLines = 4

// displayLines returns the message's display lines in source order: the text
// runs' bytes with the 0x0A display wrap as the line separator, and a newline
// control tag starting a new line. Each line is collapsed the way the speech
// column folds a display wrap (a wrap is layout, not speech, so both columns
// fold it to a single space), so a page's text is the text that box would speak.
func displayLines(m bmg.Message, rules *Rules) []string {
	var lines []string
	var line strings.Builder
	flush := func() {
		if t := collapseSpaces(line.String()); t != "" {
			lines = append(lines, t)
		}
		line.Reset()
	}
	for _, t := range m.Tokens {
		switch t.Kind {
		case bmg.TokText:
			for i := 0; i < len(t.Raw); i++ {
				if t.Raw[i] == 0x0A {
					flush()
					continue
				}
				line.WriteByte(t.Raw[i])
			}
		case bmg.TokTag:
			r := rules.Lookup(t.Group, t.Type)
			rendered := speechFor(r)
			if rendered == "" {
				continue // removed: emphasis, colour, unclassified markers speak nothing
			}
			if r.Action == RuleNewline {
				flush()
				continue
			}
			if line.Len() > 0 {
				line.WriteByte(' ')
			}
			line.WriteString(rendered)
		}
	}
	flush()
	return lines
}

// messagePages splits one BMG message into its display pages and returns them
// in ordinal order. The common case — a message that fits one box — yields a
// single page whose text is the message's own text; a longer one yields one page
// per box, the last of which may hold fewer lines than the limit.
func messagePages(m bmg.Message, rules *Rules) []string {
	lines := displayLines(m, rules)
	if len(lines) == 0 {
		return nil
	}
	var pages [][]string
	var box []string
	closeBox := func() {
		if len(box) == 0 {
			return
		}
		pages = append(pages, box)
		box = nil
	}
	for _, l := range lines {
		if len(box) >= MaxBoxLines {
			closeBox()
		}
		box = append(box, l)
	}
	closeBox()
	out := make([]string, 0, len(pages))
	for _, b := range pages {
		out = append(out, collapseSpaces(strings.Join(b, " ")))
	}
	return out
}
