package matcher

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// Page-aware scoring (ADR-0184). The board evidence (2026-10-24, UNO Q board,
// real GameCube, build 3f91310): an NPC's message displays in two boxes
// (pages) and the player was on page 2. OCR read page 2 with one misread
// word, but the matcher compared the single box on screen against the whole
// two-page message text, scored the text component at 0.44 and abstained
// (COMMITTED_ABSTAINED). The decision model then answered "none of the
// candidates" because the candidate text did not equal the box on screen,
// and the line was read through with the OCR misread instead of the
// script's page text in the character's voice.
//
// The fix: an index may carry each message's display pages (the same
// derivation speech uses, precomputed at build time), and the scorer scores
// the observation against each page as well as the whole text, keeping the
// best. A page read of a two-box message then scores like a one-box message.
//
// All strings below are invented; no game text is used.

// page2ReadMsgs is an invented mini corpus: m01 is a two-page message whose
// page 2 invites the player up the hill, plus three unrelated lines. (In a
// real profile the page texts do not exist as separate messages — only the
// message itself does — which is what the test must model.)
func page2ReadMsgs() []games.Message {
	return []games.Message{
		{ID: "m01", NormalizedText: "Would you care to join me for dinner tonight? Later, come up the hill and play with me on the long bridge."},
		{ID: "d01", NormalizedText: "The bridge creaks under the weight of the old cart."},
		{ID: "d02", NormalizedText: "You found a rupee coin among the dried leaves."},
		{ID: "d03", NormalizedText: "A storm gathers low over the far horizon tonight."},
	}
}

const (
	page1Text = "Would you care to join me for dinner tonight?"
	page2Text = "Later, come up the hill and play with me on the long bridge."
)

// page2Read is the on-screen box: page 2 with two misread characters ("plai"
// for "play", "brrdge" for "bridge") — enough noise that the fragment
// containment floor does not rescue the whole-message comparison (the
// board's shape), while a page-scoped comparison is a near-exact read of one
// box.
const page2Read = "Later, come up the hill and plai with me on the long brrdge."

func pageOpts() IndexOptions {
	return IndexOptions{Pages: map[string][]string{
		"m01": {page1Text, page2Text},
	}}
}

func TestPageTwoReadScoresPageAndCommits(t *testing.T) {
	idx, err := Build(page2ReadMsgs(), nil, pageOpts())
	if err != nil {
		t.Fatal(err)
	}

	r := idx.Rank(Observation{Text: page2Read, OCRConf: 0.94}, RankOptions{})

	if topID(r) != "m01" {
		t.Fatalf("top = %s, want m01 (its page 2 IS the read): %+v", topID(r), r.Candidates)
	}
	if got := r.Candidates[0].Components[ComponentPage]; got != 2 {
		t.Errorf("components[\"%s\"] = %v, want 2 (the page on screen)", ComponentPage, got)
	}
	if got := r.Candidates[0].Components[ComponentPageTextScore]; got < 0.8 {
		t.Errorf("components[\"%s\"] = %v, want the page score ≈1 (a page read of a two-box message scores like a one-box read)", ComponentPageTextScore, got)
	}
	if got := r.Candidates[0].Components[ComponentText]; got < 0.8 {
		t.Errorf("text component = %v, want ≥ 0.8: the winning page score must replace the under-scored whole-text comparison", got)
	}
	if r.Reason == ReasonAbstained {
		t.Errorf("reason = %s: the page-2 read of the two-page message must commit, not abstain", r.Reason)
	}
}

// TestPageTwoReadWithoutPagesStaysLow documents the pre-fix behaviour: without
// page rows the same read is compared against the whole message and scores
// low on text — the shape that abstained on the board (0.44 with the real
// profile's longer text). The test asserts the low score rather than the
// exact value, because it depends on the distractor set.
func TestPageTwoReadWithoutPagesStaysLow(t *testing.T) {
	idx, err := Build(page2ReadMsgs(), nil, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := idx.Rank(Observation{Text: page2Read, OCRConf: 0.94}, RankOptions{})
	for _, c := range r.Candidates {
		if c.MessageID == "m01" && c.Components[ComponentText] >= 0.8 {
			t.Fatalf("without page rows m01's text component = %v: a whole-message comparison of a single-box read must stay low", c.Components[ComponentText])
		}
		if c.MessageID == "m01" {
			if got := c.Components[ComponentPage]; got != 0 {
				t.Errorf("without page rows components[\"page\"] = %v, want 0 (no page evidence)", got)
			}
		}
	}
}

// TestPageOneReadIsUnchanged: page 1 of a two-page message read with its own
// words still resolves to the message, and the page-1 page score is recorded.
func TestPageOneReadIsUnchanged(t *testing.T) {
	idx, err := Build(page2ReadMsgs(), nil, pageOpts())
	if err != nil {
		t.Fatal(err)
	}
	r := idx.Rank(Observation{Text: page1Text, OCRConf: 0.94}, RankOptions{})
	if topID(r) != "m01" {
		t.Fatalf("top = %s, want m01 (page 1 is the read)", topID(r))
	}
	if got := r.Candidates[0].Components[ComponentPage]; got != 1 {
		t.Errorf("components[\"%s\"] = %v, want 1", ComponentPage, got)
	}
}
