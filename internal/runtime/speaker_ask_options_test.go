package runtime

import (
	"fmt"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers/systemone"
)

// TestSpeakerAskOptionsCapsTheRoster pins the board defect: an ~80-character
// roster sent whole made Clef answer bad_schema ("over the 16 option cap") on
// every speaker-at-commit ask. The options stay under the cap, and the line's
// assigned character comes first even when it sits late in the roster.
func TestSpeakerAskOptionsCapsTheRoster(t *testing.T) {
	var roster []systemone.Speaker
	for i := 0; i < 81; i++ {
		roster = append(roster, systemone.Speaker{ID: fmt.Sprintf("c%02d", i), Description: "someone"})
	}
	got := speakerAskOptions(roster, "c70", 0)
	if len(got) != systemone.MaxOptions-1 {
		t.Fatalf("len = %d, want %d (plus the unknown option = the 16 cap)", len(got), systemone.MaxOptions-1)
	}
	if got[0].ID != "c70" {
		t.Fatalf("first option = %s, want the assigned character c70", got[0].ID)
	}
	seen := map[string]bool{}
	for _, s := range got {
		if seen[s.ID] {
			t.Fatalf("duplicate option %s", s.ID)
		}
		seen[s.ID] = true
	}
	if q := systemone.BuildAdjudicationSpeakerOnly(systemone.Context{}, got); len(q.Questions[0].Criteria) > systemone.MaxOptions {
		t.Fatalf("speaker question has %d options, over the %d cap", len(q.Questions[0].Criteria), systemone.MaxOptions)
	}
	if small := speakerAskOptions(roster[:3], "", 0); len(small) != 3 {
		t.Fatalf("a small roster must pass whole, got %d", len(small))
	}
}
