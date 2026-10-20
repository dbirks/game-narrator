package matcher

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
)

// buildFloorIndex builds a six-message index shaped like the second bench set's
// script (ADR-0079): five unrelated lines plus the one the garbage reading
// shares a " to" trigram with.
func buildFloorIndex(t *testing.T) *Index {
	t.Helper()
	lines := map[string]string{
		"m-01": "No coin, no crossing. That is the rule and the whole of it.",
		"m-02": "You paid. I did not say the coin was yours.",
		"m-03": "There is a second ferry. There is always a second ferry.",
		"m-04": "So the story goes. Stories are cheaper than coins.",
		"m-05": "Water keeps what people hide downriver.",
		"m-06": "This is not a toll. This is a habit.",
	}
	ids := []string{"m-01", "m-02", "m-03", "m-04", "m-05", "m-06"}
	msgs := make([]games.Message, 0, len(ids))
	for _, id := range ids {
		lt := lines[id]
		msgs = append(msgs, games.Message{
			ID: id, ProfileID: "floor", OriginalText: lt,
			NormalizedText: textnorm.Normalize(lt, textnorm.Options{StripPunctuation: true, OCRFold: true}),
			SpeechText:     lt, SceneID: "s", SpeakerID: "n", TextKind: games.TextDialogue,
		})
	}
	idx, err := Build(msgs, nil, IndexOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return idx
}

// TestFuzzyTextFloorOnRetrievedGarbage is the failing-first proof for the core
// assumption ADR-0079 broke: an out-of-script reading (the second game's
// Cyrillic page, read by the eng model as "BOA3 TO, MPRYYT.") enters the
// candidate pool through one shared trigram with "m-06" (the " to" in "not a
// TOLL") rather than through the global fallback. The pool is then a single
// candidate, whose softmax probability is 1.0 by definition, so the
// probability and margin gates pass — and before the fix the reading committed
// as MATCH_FUZZY with 0.02 of text evidence. The text-evidence floor must
// cover the retrieved path, not only the fallback path: "we had nothing" must
// not look like "we are sure" regardless of the route the candidate took in.
func TestFuzzyTextFloorOnRetrievedGarbage(t *testing.T) {
	idx := buildFloorIndex(t)
	// The real readings the eng model produced on the second set's Cyrillic
	// frames (recorded from the bench harness, 2026-10-19): the same shape of
	// garbage at every resolution.
	for _, obs := range []string{
		"BOA3 TO, MPRYYT.",
		"boa3 to, mpryyt.",
		"300a NOMHUT TO, YTO NPAYYT.",
		"BOA3 NOMHWUT TO, 4TO AW MPRYYT.",
	} {
		rk := idx.Rank(Observation{Text: obs}, RankOptions{})
		if !rk.Abstain {
			t.Fatalf("obs %q: matched %s (p=%.3f, text=%.4f) — a garbage reading must abstain, not commit as fuzzy",
				obs, rk.Candidates[0].MessageID, rk.Candidates[0].Probability, rk.Candidates[0].Components[ComponentText])
		}
		if rk.Reason != ReasonAbstained {
			t.Fatalf("obs %q: reason = %q, want abstained", obs, rk.Reason)
		}
	}
}

// TestFuzzyTextFloorKeepsRealFuzzyMatches: the floor must not push legitimate
// noisy reads off the fuzzy path. A one-glyph OCR slip on a long line keeps a
// high text score and must still commit the right message with a fuzzy (not
// exact) reason.
func TestFuzzyTextFloorKeepsRealFuzzyMatches(t *testing.T) {
	idx := buildFloorIndex(t)
	// "hahit" for "habit": a plausible OCR slip the fold does not cover.
	rk := idx.Rank(Observation{Text: "This is not a toll. This is a hahit."}, RankOptions{})
	if rk.Abstain {
		t.Fatalf("legitimate noisy read abstained: %s (text=%.4f)", rk.Reason, rk.Candidates[0].Components[ComponentText])
	}
	if rk.Candidates[0].MessageID != "m-06" {
		t.Fatalf("top-1 = %s, want m-06", rk.Candidates[0].MessageID)
	}
	// And a clean line still takes the exact path.
	rk = idx.Rank(Observation{Text: "This is not a toll. This is a habit."}, RankOptions{})
	if rk.Reason != ReasonExact {
		t.Fatalf("clean read: reason = %q, want exact", rk.Reason)
	}
}
