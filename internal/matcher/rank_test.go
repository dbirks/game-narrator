package matcher

import (
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func miniIndex(t *testing.T) ([]games.Message, *Index) {
	t.Helper()
	msgs, edges := miniScript()
	return msgs, mustBuild(t, msgs, edges)
}

func topID(r Ranking) string {
	if len(r.Candidates) == 0 {
		return ""
	}
	return r.Candidates[0].MessageID
}

func TestExactHit(t *testing.T) {
	msgs, idx := miniIndex(t)
	r := idx.Rank(Observation{Text: textOf(msgs, "m01"), OCRConf: 0.94,
		SceneID: "scene.harbour"}, RankOptions{})
	if r.Abstain {
		t.Fatalf("an exact hit on a unique line abstained: %+v", r)
	}
	if topID(r) != "m01" {
		t.Fatalf("top = %s, want m01", topID(r))
	}
	if r.Reason != ReasonExact {
		t.Errorf("reason = %s, want %s", r.Reason, ReasonExact)
	}
	if r.Margin < 0.5 {
		t.Errorf("margin = %.3f, want a clear winner over the field", r.Margin)
	}
	if got := r.Candidates[0].Components[ComponentScene]; got != 1 {
		t.Errorf("scene component = %v, want 1 for a matching scene", got)
	}
	if got := r.Candidates[0].Components[ComponentText]; got < 0.99 {
		t.Errorf("text component = %v, want ≈1 for an exact key match", got)
	}
}

// TestOCRNearMiss is the case docs/spec/12 "Dialogue and graph" calls "Levenshtein
// near miss" plus the confusions textnorm's OCR fold exists for
// (docs/spec/12 "candidate ranking").
func TestOCRNearMiss(t *testing.T) {
	msgs, idx := miniIndex(t)
	const target = "m02"
	want := textOf(msgs, target)
	variants := map[string]string{
		"rn-for-m":            "We sail for the Gull-King's rock on the rnorning tide.",
		"0-for-o":             "We sail for the Gull-King's r0ck on the morning tide.",
		"1-for-i":             "We sa1l for the Gull-King's rock on the morning tide.",
		"dropped punctuation": "We sail for the Gull Kings rock on the morning tide",
		"one wrong char":      "We sail for the Gull-King's rock on the morning tied.",
		"wrong char and case": "we Sail For The Gull-King's rock on the rnorning tide!",
	}
	for name, obs := range variants {
		r := idx.Rank(Observation{Text: obs, OCRConf: 0.72, SceneID: "scene.harbour"}, RankOptions{})
		if r.Abstain {
			t.Errorf("%s: abstained (margin %.3f) on %q", name, r.Margin, obs)
			continue
		}
		if topID(r) != target {
			t.Errorf("%s: top = %s, want %s\n observation: %q\n stored:    %q\n ranking: %s",
				name, topID(r), target, obs, want, rankingString(r))
			continue
		}
		if r.Reason != ReasonExact && r.Reason != ReasonFuzzy {
			t.Errorf("%s: reason = %s, want MATCH_EXACT or MATCH_FUZZY", name, r.Reason)
		}
	}
}

// TestPrefixUniqueWhileRevealing is docs/spec/12's "predictive prefix unique".
func TestPrefixUniqueWhileRevealing(t *testing.T) {
	msgs, idx := miniIndex(t)
	full := textOf(msgs, "m30")
	prefix := full[:31] // "Beyond the reef the water turns"
	r := idx.Rank(Observation{Text: prefix, IsPrefix: true, OCRConf: 0.9,
		SceneID: "scene.openwater"}, RankOptions{})
	if r.Abstain {
		t.Fatalf("unique revealed prefix abstained: %s", rankingString(r))
	}
	if topID(r) != "m30" {
		t.Fatalf("top = %s, want m30", topID(r))
	}
	if r.Reason != ReasonPredictivePrefix {
		t.Errorf("reason = %s, want %s", r.Reason, ReasonPredictivePrefix)
	}
	if got := r.Candidates[0].Components[ComponentPrefixCoverage]; got <= 0 || got >= 1 {
		t.Errorf("prefix_coverage = %v, want a strict fraction of the revealed line", got)
	}
}

// TestPrefixSharedWhileRevealing is the other half of the same line in docs/spec/12:
// "predictive prefix unique vs two likely continuations". A prefix that two lines
// share must never be spoken.
func TestPrefixSharedWhileRevealing(t *testing.T) {
	msgs, idx := miniIndex(t)
	shared := textOf(msgs, "m31")[:48] // both tidewatcher lines agree up to here
	r := idx.Rank(Observation{Text: shared, IsPrefix: true, OCRConf: 0.9,
		SceneID: "scene.isle"}, RankOptions{})
	if !r.Abstain {
		t.Fatalf("a prefix shared by two lines was accepted: %s", rankingString(r))
	}
	if r.Reason != ReasonAbstained {
		t.Errorf("reason = %s, want %s", r.Reason, ReasonAbstained)
	}
	if r.Reason == ReasonPredictivePrefix {
		t.Error("a shared prefix must never be reported as a prediction")
	}
	if len(r.Candidates) < 2 {
		t.Fatalf("the shortlist should still show both continuations, got %d", len(r.Candidates))
	}
	if r.Margin > 0.12 {
		t.Errorf("margin = %.3f between two equally supported completions, want the tie to be visible", r.Margin)
	}
}

func TestPrefixNotReportedWhenTooShortOrNotMarked(t *testing.T) {
	msgs, idx := miniIndex(t)
	full := textOf(msgs, "m30")
	// A three-character typewriter prefix is the first frame of an animation, not
	// evidence (docs/spec/05 "Use holdoff for snippets like 'Yes.' or 'No.'").
	r := idx.Rank(Observation{Text: full[:6], IsPrefix: true}, RankOptions{})
	if r.Reason == ReasonPredictivePrefix || !r.Abstain {
		t.Errorf("short revealed prefix: reason=%s abstain=%v, want an abstain", r.Reason, r.Abstain)
	}
	// A complete observation that happens to be a prefix of a longer line is not a
	// prediction; the caller has to say the text is still revealing.
	r = idx.Rank(Observation{Text: full[:31], SceneID: "scene.openwater"}, RankOptions{})
	if r.Reason == ReasonPredictivePrefix {
		t.Error("IsPrefix=false must not produce PREDICTIVE_PREFIX")
	}
}

// TestGraphSuccessorBreaksATie is "same text spoken by two actors" plus
// "two adjacent graph candidates" in docs/spec/12: the words are identical, so
// only the transition evidence can choose, and it must.
func TestGraphSuccessorBreaksATie(t *testing.T) {
	msgs, idx := miniIndex(t)
	obs := Observation{Text: textOf(msgs, "m13"), OCRConf: 0.9}
	cases := []struct {
		recent []string
		want   string
		scene  string
	}{
		{recent: []string{"m12"}, want: "m13", scene: "scene.isle"}, // the harbour chain reaches Merra's line
		{recent: []string{"m21"}, want: "m22", scene: "scene.cave"}, // the cave chain reaches Oddis's
	}
	for _, tc := range cases {
		o := obs
		o.RecentMessageIDs = tc.recent
		o.SceneID = tc.scene
		r := idx.Rank(o, RankOptions{})
		if r.Abstain {
			t.Errorf("recent %v: graph-supported duplicate abstained: %s", tc.recent, rankingString(r))
			continue
		}
		if topID(r) != tc.want {
			t.Errorf("recent %v: top = %s, want %s: %s", tc.recent, topID(r), tc.want, rankingString(r))
		}
		if r.Reason != ReasonGraphNeighbour {
			t.Errorf("recent %v: reason = %s, want %s — the graph is what chose here",
				tc.recent, r.Reason, ReasonGraphNeighbour)
		}
		if got := r.Candidates[0].Components[ComponentEdgePrior]; got <= 0 {
			t.Errorf("recent %v: edge_prior = %v, want the successor to carry it", tc.recent, got)
		}
	}
}

// TestStrongTextBeatsEdgePrior is docs/spec/04's "Never force the most likely graph
// edge over strong contrary OCR text", and docs/spec/12's "graph jump/revisit".
func TestStrongTextBeatsEdgePrior(t *testing.T) {
	msgs, idx := miniIndex(t)
	// The last confirmed line is m00, whose only successor is m01. The screen
	// clearly shows m38, four scenes away.
	r := idx.Rank(Observation{Text: textOf(msgs, "m38"), OCRConf: 0.96,
		SceneID: "scene.isle", RecentMessageIDs: []string{"m00"}}, RankOptions{})
	if r.Abstain {
		t.Fatalf("a jump to a remote node abstained: %s", rankingString(r))
	}
	if topID(r) != "m38" {
		t.Fatalf("top = %s, want m38 — the graph predicted m01 and must not have won", topID(r))
	}
	for _, c := range r.Candidates {
		if c.MessageID == "m01" && c.Probability >= r.Candidates[0].Probability {
			t.Errorf("the predicted successor outranked the text on screen")
		}
	}
	if r.Reason == ReasonGraphNeighbour {
		t.Errorf("reason = %s: a full-text match is not a graph decision", r.Reason)
	}
}

// TestRepeatEdgeAndLoop: docs/spec/12 "repeat NPC after menu" and "loop".
func TestLoopAndRepeatEdges(t *testing.T) {
	msgs, idx := miniIndex(t)
	// The loop: after m11, the gull line m10 is again the likely next text.
	r := idx.Rank(Observation{Text: textOf(msgs, "m10"), SceneID: "scene.openwater",
		RecentMessageIDs: []string{"m11", "m10"}}, RankOptions{})
	if r.Abstain || topID(r) != "m10" {
		t.Errorf("loop revisit: %s", rankingString(r))
	}
	// A repeat edge points at the message itself; Torvik saying it again is the
	// same node, not a different one.
	r = idx.Rank(Observation{Text: textOf(msgs, "m03"), SceneID: "scene.harbour",
		RecentMessageIDs: []string{"m03"}}, RankOptions{})
	if r.Abstain || topID(r) != "m03" {
		t.Errorf("self-repeat: %s", rankingString(r))
	}
	if got := r.Candidates[0].Components[ComponentEdgePrior]; got <= 0 {
		t.Errorf("self-repeat edge_prior = %v, want the repeat edge to count", got)
	}
}

// TestBranchSelection: at a branch, both alternatives are graph-supported; with the
// text unreadable the matcher may not pick one (docs/spec/05 "Never speak ...
// scripted branches from guesswork where player choice determines line").
func TestBranchSelectionNotGuessed(t *testing.T) {
	_, idx := miniIndex(t)
	r := idx.Rank(Observation{Text: "Which way", RecentMessageIDs: []string{"m05"},
		SceneID: "scene.openwater"}, RankOptions{})
	if r.Reason == ReasonGraphNeighbour && topID(r) != "m05" {
		t.Errorf("a branch was resolved to one alternative on weak text: %s", rankingString(r))
	}
}

// TestShortPhraseHoldoff is docs/spec/12's "short common phrase" and the
// docs/spec/05 holdoff: "Yes." appears four times in this script.
func TestShortPhraseHoldoff(t *testing.T) {
	msgs, idx := miniIndex(t)
	plain := idx.Rank(Observation{Text: textOf(msgs, "m16"), OCRConf: 0.95}, RankOptions{})
	if !plain.Abstain || plain.Reason != ReasonAbstained {
		t.Errorf(`"Yes." with no context: reason=%s abstain=%v, want an abstain`, plain.Reason, plain.Abstain)
	}
	if got := countIDs(plain, "m16", "m20", "m27", "m34"); got != 4 {
		t.Errorf(`"Yes." shortlist holds %d of the four occurrences, want all four visible to a decision model`,
			got)
	}
	// Scene evidence alone is not enough either, because two of the four are in the
	// cave — the point of the holdoff is that more than one evidence axis is needed.
	if r := idx.Rank(Observation{Text: "Yes.", SceneID: "scene.isle"}, RankOptions{}); !r.Abstain {
		t.Errorf(`"Yes." with one scene of two candidates: %s`, rankingString(r))
	}
	// With the graph telling us which "Yes." this is, it resolves.
	r := idx.Rank(Observation{Text: "Yes.", OCRConf: 0.95, SceneID: "scene.isle",
		RecentMessageIDs: []string{"m15"}}, RankOptions{})
	if r.Abstain {
		t.Errorf(`"Yes." with graph evidence abstained: %s`, rankingString(r))
	}
	if topID(r) != "m16" {
		t.Errorf(`"Yes." after m15: top = %s, want m16`, topID(r))
	}
	if r.Reason != ReasonGraphNeighbour {
		t.Errorf("reason = %s, want %s", r.Reason, ReasonGraphNeighbour)
	}
}

// TestPunctuationOnlyDifference covers both readings of the case in docs/spec/12:
// an OCR read that dropped punctuation still finds the line, and two lines that
// differ *only* by punctuation are genuinely indistinguishable on text alone.
func TestPunctuationOnlyDifference(t *testing.T) {
	_, idx := miniIndex(t)
	r := idx.Rank(Observation{Text: "Hold the line", SceneID: "scene.cave"}, RankOptions{})
	if r.Abstain || topID(r) != "m24" {
		t.Fatalf(`dropped "!": %s`, rankingString(r))
	}
	if r.Reason != ReasonExact {
		t.Errorf("reason = %s, want MATCH_EXACT: folding makes these the same key", r.Reason)
	}
	// "Wait for me!" and "Wait for me." are both in the script. Text alone cannot
	// tell them apart; the graph can, and without it the answer is no answer.
	amb := idx.Rank(Observation{Text: "Wait for me", SceneID: "scene.cave"}, RankOptions{})
	if !amb.Abstain {
		t.Errorf("punctuation-only pair resolved on text alone: %s", rankingString(amb))
	}
	resolved := idx.Rank(Observation{Text: "Wait for me", SceneID: "scene.cave",
		RecentMessageIDs: []string{"m35"}}, RankOptions{}) // m35 -> m36 ("Wait for me!")
	if resolved.Abstain || topID(resolved) != "m36" {
		t.Errorf("punctuation-only pair with graph evidence: %s", rankingString(resolved))
	}
}

func TestEmptyAndHostileObservationsAreSafe(t *testing.T) {
	_, idx := miniIndex(t)
	cases := map[string]Observation{
		"empty":            {},
		"spaces":           {Text: "     \t\n "},
		"punctuation only": {Text: "!!!...???"},
		"control chars":    {Text: "\x00\x01\x02\x1f\x7f"},
		"invalid utf8":     {Text: "\xff\xfe bad bytes \x80"},
		"nan confidence":   {Text: "The harbour bell rings twice before dawn.", OCRConf: math.NaN()},
		"inf confidence":   {Text: "The harbour bell rings twice before dawn.", OCRConf: math.Inf(1)},
		"neg confidence":   {Text: "The harbour bell rings twice before dawn.", OCRConf: -3},
		"unknown ids":      {Text: "The harbour bell rings twice before dawn.", RecentMessageIDs: []string{"nope", ""}},
		"nil recent":       {Text: "The harbour bell rings twice before dawn.", RecentMessageIDs: nil},
		"huge":             {Text: strings.Repeat("the harbour bell rings twice ", 4000)}, // 136 KB
		"huge unicode":     {Text: strings.Repeat("é\U0001F600\u0000 ", 20000)},
	}
	for name, obs := range cases {
		r := idx.Rank(obs, RankOptions{})
		assertFiniteRanking(t, name, r)
		if name == "empty" || name == "spaces" || name == "punctuation only" || name == "control chars" {
			if !r.Abstain {
				t.Errorf("%s: abstain = false, want true: %s", name, rankingString(r))
			}
		}
		if len(r.Candidates) > 10 {
			t.Errorf("%s: %d candidates returned, over the default TopK", name, len(r.Candidates))
		}
	}
}

func TestRankOptionsBounds(t *testing.T) {
	msgs, idx := miniIndex(t)
	obs := Observation{Text: textOf(msgs, "m02"), SceneID: "scene.harbour"}
	if r := idx.Rank(obs, RankOptions{TopK: 5}); len(r.Candidates) > 5 {
		t.Errorf("TopK=5 returned %d", len(r.Candidates))
	}
	if r := idx.Rank(obs, RankOptions{TopK: 1000}); len(r.Candidates) > 50 {
		t.Errorf("TopK=1000 returned %d, want clamped to 50", len(r.Candidates))
	}
	// Negative and NaN options are repaired, not obeyed.
	for _, o := range []RankOptions{{TopK: -7, MinMargin: -1, MinProbability: -2},
		{TopK: 3, MinMargin: math.NaN(), MinProbability: math.Inf(-1)}} {
		r := idx.Rank(obs, o)
		if len(r.Candidates) > 50 || len(r.Candidates) == 0 {
			t.Errorf("options %+v returned %d candidates", o, len(r.Candidates))
		}
		assertFiniteRanking(t, fmt.Sprintf("%+v", o), r)
	}
	// A wholly-zero weight set means "unset" and must fall back to defaults rather
	// than ranking every candidate at log(epsilon).
	def := idx.Rank(obs, RankOptions{})
	zero := idx.Rank(obs, RankOptions{Weights: Weights{}})
	if rankingString(def) != rankingString(zero) {
		t.Errorf("zero weights did not fall back to defaults:\n %s\n %s", rankingString(zero), rankingString(def))
	}
}

func TestWeightsAreConfigurable(t *testing.T) {
	// The duplicate line again: with the scene coefficient raised, scene evidence
	// alone picks the cave copy. This is the knob docs/spec/05 means by "tunable".
	msgs, idx := miniIndex(t)
	obs := Observation{Text: textOf(msgs, "m13"), SceneID: "scene.cave"}
	w := DefaultWeights()
	w.Scene = 4
	r := idx.Rank(obs, RankOptions{Weights: w})
	if r.Abstain || topID(r) != "m22" {
		t.Errorf("scene-weighted ranking: %s", rankingString(r))
	}
	// A non-finite or absurd weight is clamped: the ranking stays finite and
	// ordered, which is the property docs/spec/12 actually fuzzes for.
	w.Text = math.Inf(1)
	w.EdgePrior = math.NaN()
	r = idx.Rank(obs, RankOptions{Weights: w})
	assertFiniteRanking(t, "hostile weights", r)
}

func TestNeutralPlaceholderComponents(t *testing.T) {
	msgs, idx := miniIndex(t)
	for _, id := range []string{"m02", "m16", "m29"} {
		r := idx.Rank(Observation{Text: textOf(msgs, id)}, RankOptions{})
		for _, c := range r.Candidates {
			if got := c.Components[ComponentSpeakerContext]; got != neutralScore {
				t.Errorf("speaker_context = %v, want the documented 0.5 placeholder", got)
			}
			if got := c.Components[ComponentSourceReliability]; got != neutralScore {
				t.Errorf("source_reliability = %v, want the documented 0.5 placeholder", got)
			}
		}
	}
}

func TestVisibilityComponentTracksOCREvidence(t *testing.T) {
	msgs, idx := miniIndex(t)
	obs := textOf(msgs, "m12")
	for _, conf := range []float64{0.05, 0.5, 1} {
		r := idx.Rank(Observation{Text: obs, OCRConf: conf}, RankOptions{})
		if len(r.Candidates) == 0 {
			t.Fatalf("no candidates at confidence %v", conf)
		}
		if got := r.Candidates[0].Components[ComponentVisibility]; math.Abs(got-conf) > 1e-9 {
			t.Errorf("visibility = %v, want the reported OCR confidence %v", got, conf)
		}
	}
	// Unreported confidence is unknown, not zero: a caller with no figure must not
	// be scored as though the OCR was certain the text was garbage.
	r := idx.Rank(Observation{Text: obs}, RankOptions{})
	if got := r.Candidates[0].Components[ComponentVisibility]; got != neutralScore {
		t.Errorf("unreported OCR confidence: visibility = %v, want %v", got, neutralScore)
	}
}

func TestTextKindHintComponent(t *testing.T) {
	msgs, idx := miniIndex(t)
	r := idx.Rank(Observation{Text: textOf(msgs, "m29"), TextKindHint: "item"}, RankOptions{})
	if r.Abstain || topID(r) != "m29" {
		t.Fatalf("item hint: %s", rankingString(r))
	}
	if got := r.Candidates[0].Components[ComponentTextKind]; got != 1 {
		t.Errorf("matching hint: text_kind = %v, want 1", got)
	}
	// A hint that contradicts the stored class lowers that component without
	// deciding the line: the hint is a classifier's guess, not ground truth.
	r = idx.Rank(Observation{Text: textOf(msgs, "m29"), TextKindHint: "sign"}, RankOptions{})
	if got := r.Candidates[0].Components[ComponentTextKind]; got != 0 {
		t.Errorf("contradicting hint: text_kind = %v, want 0", got)
	}
}

func TestTextKeyIsTruncatedNotUnbounded(t *testing.T) {
	// Both sides of the comparison are bounded by the same maxTextRunes, so a
	// complete observation of a very long line still matches the line it came from
	// exactly, and a 100 KB observation costs no more per candidate than a 100
	// character one.
	long := strings.Repeat("long invented utterance about nothing in particular ", 40)
	msgs := []games.Message{
		{ID: "big", ProfileID: "pr", OriginalText: long, NormalizedText: long,
			TextKind: games.TextDialogue},
		{ID: "small", ProfileID: "pr", OriginalText: "Short line.",
			NormalizedText: "Short line.", TextKind: games.TextDialogue},
	}
	idx, err := Build(msgs, nil, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A long line's text component must reach the same ceiling a short one's does;
	// a bound that disagreed between indexing and scoring would show up here as a
	// long line that can never score above ~0.6 on its own exact text.
	exact := idx.Rank(Observation{Text: long}, RankOptions{})
	if exact.Abstain || topID(exact) != "big" {
		t.Fatalf("exact hit on a very long line: %s", rankingString(exact))
	}
	if got := exact.Candidates[0].Components[ComponentText]; got < 0.99 {
		t.Errorf("text component for an exact long-line hit = %v, want ≈1", got)
	}
	if got := idx.Stats().Messages; got != 2 {
		t.Fatalf("messages = %d", got)
	}
	head := long[:maxTextRunes-40]
	r := idx.Rank(Observation{Text: head}, RankOptions{})
	if r.Abstain || topID(r) != "big" {
		t.Errorf("prefix of a very long line: %s", rankingString(r))
	}
}

// TestAbstainOnProbabilityThreshold pins the threshold behaviour itself: a
// near-tie is refused when the floor is raised, and a clear winner survives it.
func TestAbstainOnProbabilityThreshold(t *testing.T) {
	msgs, idx := miniIndex(t)
	strong := Observation{Text: textOf(msgs, "m02"), SceneID: "scene.harbour"}
	// Comfortably inside the floor: accepted.
	if r := idx.Rank(strong, RankOptions{MinProbability: 0.5, MinMargin: 0.5}); r.Abstain {
		t.Errorf("a decisive exact match was refused: %s", rankingString(r))
	}
	// Above it, with the margin threshold left at the default: this isolates the
	// probability floor from the runner-up comparison, because the winner here has a
	// margin over 0.8 and still must be refused when its probability is below the
	// stated floor. The number is 0.95 deliberately: the softmax runs over a
	// ten-candidate list, so even a correct exact hit leaves mass on the near-misses,
	// which is precisely why docs/spec/17 insists the constants come from labeled
	// holdout data rather than from this file.
	r := idx.Rank(strong, RankOptions{MinProbability: 0.95})
	if !r.Abstain || r.Reason != ReasonAbstained {
		t.Errorf("MinProbability=0.95 on a p=0.89 winner: reason=%s abstain=%v, want an abstain",
			r.Reason, r.Abstain)
	}
	// And the near-tie case is refused on margin alone.
	near := idx.Rank(Observation{Text: textOf(msgs, "m31")[:48], IsPrefix: true,
		SceneID: "scene.isle"}, RankOptions{})
	if !near.Abstain || near.Margin >= 0.12 {
		t.Errorf("shared-prefix margin = %.3f abstain=%v, want a refusal below the default margin",
			near.Margin, near.Abstain)
	}
}

// TestGlobalFallbackOnlyWhenNothingElse asserts docs/spec/05's last retrieval stage:
// it exists so a caller always has a shortlist to hand a human or Clef, and it must
// not fire when any earlier stage found evidence.
func TestGlobalFallbackOnlyWhenNothingElse(t *testing.T) {
	msgs, idx := miniIndex(t)
	// No shared trigram with anything in the corpus, in a script the index has never
	// seen: retrieval can only fall back.
	r := idx.Rank(Observation{Text: "𝔘𝔫𝔯𝔢𝔞𝔡𝔞𝔟𝔩𝔢 𝔊𝔩𝔶𝔭𝔥𝔰"}, RankOptions{})
	if !r.Abstain {
		t.Errorf("a fallback shortlist must never be accepted as an answer: %s", rankingString(r))
	}
	if len(r.Candidates) == 0 || len(r.Candidates) > 10 {
		t.Errorf("fallback returned %d candidates, want a bounded shortlist", len(r.Candidates))
	}
	for _, c := range r.Candidates {
		if c.Probability >= 0.5 {
			t.Errorf("fallback candidate %s carries probability %.3f; the fallback must not invent confidence",
				c.MessageID, c.Probability)
		}
	}
	// And when the trigram stage does find something, the fallback must not dilute
	// the shortlist with length-matched noise.
	withEvidence := idx.Rank(Observation{Text: textOf(msgs, "m25")}, RankOptions{})
	if withEvidence.Abstain || topID(withEvidence) != "m25" {
		t.Errorf("ordinary lookup: %s", rankingString(withEvidence))
	}
}

// TestFallbackCannotConfidenceItself: the degenerate one-message profile is where a
// single-candidate softmax would silently report certainty about an unreadable
// screen.
func TestFallbackCannotConfidenceItself(t *testing.T) {
	msgs := []games.Message{{
		ID: "only", ProfileID: "pr", OriginalText: "The harbour bell rings twice before dawn.",
		NormalizedText: "The harbour bell rings twice before dawn.", TextKind: games.TextDialogue,
	}}
	idx, err := Build(msgs, nil, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"𝔊𝔩𝔶𝔭𝔥 𝔫𝔬𝔦𝔰𝔢", "qqqq zzzz", strings.Repeat("\x00", 40)} {
		r := idx.Rank(Observation{Text: text}, RankOptions{})
		assertFiniteRanking(t, "single-message profile", r)
		if !r.Abstain {
			t.Errorf("fallback accepted %q as %s: %s", truncate(text, 20), r.Reason, rankingString(r))
		}
		if len(r.Candidates) == 0 {
			t.Errorf("the shortlist was empty for %q, so a human reviewer would see nothing", truncate(text, 20))
		}
	}
	// The same profile does answer when the text genuinely matches: the guard is
	// about the retrieval route, not about refusing to ever commit on a small profile.
	if r := idx.Rank(Observation{Text: "The harbour bell rings twice before dawn"}, RankOptions{}); r.Abstain || topID(r) != "only" {
		t.Errorf("exact hit on a one-message profile: %s", rankingString(r))
	}
}

func assertFiniteRanking(t *testing.T, label string, r Ranking) {
	t.Helper()
	if !validReason(r.Reason) {
		t.Errorf("%s: reason %q is not one of the defined codes", label, r.Reason)
	}
	sum := 0.0
	for _, c := range r.Candidates {
		if math.IsNaN(c.Score) || math.IsInf(c.Score, 0) {
			t.Errorf("%s: candidate %s score %v", label, c.MessageID, c.Score)
		}
		if math.IsNaN(c.Probability) || math.IsInf(c.Probability, 0) ||
			c.Probability < 0 || c.Probability > 1 {
			t.Errorf("%s: candidate %s probability %v", label, c.MessageID, c.Probability)
		}
		sum += c.Probability
		for _, name := range ComponentOrder {
			v, ok := c.Components[name]
			if !ok {
				t.Errorf("%s: candidate %s missing component %s", label, c.MessageID, name)
				continue
			}
			if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || v > 1 {
				t.Errorf("%s: candidate %s component %s = %v, want [0,1]", label, c.MessageID, name, v)
			}
		}
		if extra := len(c.Components) - len(ComponentOrder); extra != 0 {
			t.Errorf("%s: candidate %s has %d unexpected components", label, c.MessageID, extra)
		}
	}
	if len(r.Candidates) > 0 && math.Abs(sum-1) > 1e-9 {
		t.Errorf("%s: probabilities sum to %v, want 1", label, sum)
	}
	if math.IsNaN(r.Margin) || math.IsInf(r.Margin, 0) || r.Margin < 0 || r.Margin > 1 {
		t.Errorf("%s: margin = %v", label, r.Margin)
	}
}

func validReason(reason string) bool {
	switch reason {
	case ReasonExact, ReasonGraphNeighbour, ReasonPredictivePrefix, ReasonFuzzy, ReasonAbstained:
		return true
	}
	return false
}

func countIDs(r Ranking, ids ...string) int {
	n := 0
	for _, want := range ids {
		for _, c := range r.Candidates {
			if c.MessageID == want {
				n++
				break
			}
		}
	}
	return n
}

// TestBoxEdgeTrimRecoversExactHit is the ADR-0115 §2 live case: the captured
// feed OCR'd the textbox border into the reading as leading glyphs ("4 $o? "),
// which knocked the raw observation off the exact-match path. The trimmed
// reading restores the exact key, so the trim's ranking must win and the top
// candidate is the line itself, matched exactly, not fuzzily.
func TestBoxEdgeTrimRecoversExactHit(t *testing.T) {
	msgs, idx := miniIndex(t)
	want := textOf(msgs, "m02")
	noisy := "4 $o? " + want

	// The raw reading alone cannot be an exact hit: the border glyphs are not in
	// any stored key.
	if got := TrimEdgeNoise(noisy); got == noisy {
		t.Fatalf("premise: the noise was not trimmed (%q)", noisy)
	}
	r := idx.Rank(Observation{Text: noisy, OCRConf: 0.9, SceneID: "scene.harbour"}, RankOptions{})
	if r.Abstain {
		t.Fatalf("noisy reading abstained: %+v", r)
	}
	if topID(r) != "m02" {
		t.Fatalf("top = %s, want m02 (ranking: %s)", topID(r), rankingString(r))
	}
	// The trim must be what recovered the exact path: the winning ranking is an
	// exact match with a near-certain calibrated probability, not a fuzzy one.
	if r.Reason != ReasonExact {
		t.Errorf("reason = %s, want %s (the trimmed reading is an exact key)", r.Reason, ReasonExact)
	}
	if r.Candidates[0].Probability < 0.9 {
		t.Errorf("top probability = %.3f, want a confident exact hit", r.Candidates[0].Probability)
	}
}

// TestBoxEdgeTrimDoesNotHurtShortWordLines guards the other half of the rule: a
// line that legitimately begins with a short word must not be mangled by the
// trim. The raw reading is an exact match; the trimmed reading (with the
// genuine first word dropped) can only be a near-miss, so the raw ranking wins
// and the top stays the exact line.
func TestBoxEdgeTrimDoesNotHurtShortWordLines(t *testing.T) {
	const line = "I saw the boat come out of the fog."
	msgs := []games.Message{{
		ID: "s00", ProfileID: "profile.synthetic", OriginalText: line, NormalizedText: line,
		SpeechText: line, Language: "en", TextKind: games.TextDialogue,
		SceneID: "scene.fog", SpeakerID: "speaker.narrator", Revision: 1,
	}}
	idx := mustBuild(t, msgs, nil)

	r := idx.Rank(Observation{Text: line, OCRConf: 0.95, SceneID: "scene.fog"}, RankOptions{})
	if r.Abstain {
		t.Fatalf("an exact short-word line abstained: %+v", r)
	}
	if topID(r) != "s00" {
		t.Fatalf("top = %s, want s00 (ranking: %s)", topID(r), rankingString(r))
	}
	if r.Reason != ReasonExact {
		t.Fatalf("reason = %s, want %s: the raw reading must keep its exact match", r.Reason, ReasonExact)
	}
}
