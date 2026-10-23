package windwaker

// Display-page derivation tests (ADR-0155): a Wind Waker script message that
// spans several on-screen boxes yields one page per box, and a message that
// fits one box yields a single page — the same text either way, because a page
// is a box's display lines with the wrap folded to the speech column's space.

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games/bmg"
)

// msgWithLines builds one BMG message whose text runs are the given lines,
// joined by the pool's display line break (0x0A) — the encoding docs/adr/0031
// measured as the pool's line structure.
func msgWithLines(t *testing.T, id uint32, lines ...string) bmg.Message {
	t.Helper()
	var raw []byte
	for _, l := range lines {
		raw = append(raw, []byte(l)...)
		raw = append(raw, 0x0A)
	}
	return bmg.Message{
		Index:      int(id),
		ID:         id,
		TextOffset: 0,
		Attributes: []byte{1},
		Raw:        raw,
		Tokens: []bmg.Token{{
			Kind:   bmg.TokText,
			Offset: 0,
			Length: len(raw),
			Raw:    raw,
		}},
	}
}

func TestMessagePagesDerivesBoxes(t *testing.T) {
	rules := NewRules()
	cases := []struct {
		name  string
		lines []string
		want  []string
	}{
		{"single line is one page", []string{"HELLO"}, []string{"HELLO"}},
		{"four lines fit one box", []string{"L1", "L2", "L3", "L4"}, []string{"L1 L2 L3 L4"}},
		{"five lines split into two boxes", []string{"L1", "L2", "L3", "L4", "L5"}, []string{"L1 L2 L3 L4", "L5"}},
		{"nine lines split into three boxes", []string{"L1", "L2", "L3", "L4", "L5", "L6", "L7", "L8", "L9"}, []string{"L1 L2 L3 L4", "L5 L6 L7 L8", "L9"}},
		// The script's own box break: a blank line (two or more wraps). The
		// board's three-box message had boxes of 2, 3 and 3 lines.
		{"blank lines are box breaks", []string{"L1", "L2", "", "", "L3", "L4", "L5", "", "L6", "L7", "L8"}, []string{"L1 L2", "L3 L4 L5", "L6 L7 L8"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pages := messagePages(msgWithLines(t, 1, tc.lines...), rules)
			if len(pages) != len(tc.want) {
				t.Fatalf("pages = %q, want %q", pages, tc.want)
			}
			for i := range pages {
				if pages[i] != tc.want[i] {
					t.Fatalf("page %d = %q, want %q", i+1, pages[i], tc.want[i])
				}
			}
		})
	}
}

func TestMessagePagesEmptyMessageHasNoPages(t *testing.T) {
	if pages := messagePages(msgWithLines(t, 7), NewRules()); len(pages) != 0 {
		t.Fatalf("pages = %q, want none for an empty message", pages)
	}
}
