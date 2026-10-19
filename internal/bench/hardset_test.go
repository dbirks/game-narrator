package bench

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/games/textnorm"
	"github.com/dbirks/game-narrator/internal/matcher"
)

// The test lines are invented (AGENTS.md rule 5: no game text in tracked
// files). They exercise every construction: two clauses, a digit, and a
// punctuation shape for each punct fallback.
var hardsetTestLines = []string{
	"The river remembers every name it has carried. Tell me yours.",
	"Seven lanterns burn on the causeway at the 3rd bell.",
	"Carry the letter home and seal the door behind you",
}

func TestHardsetDistractorsDeterministic(t *testing.T) {
	for _, line := range hardsetTestLines {
		a := hardsetDistractors(line)
		b := hardsetDistractors(line)
		if len(a) != len(b) {
			t.Fatalf("distractor count not deterministic for %q: %d vs %d", line, len(a), len(b))
		}
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("distractor %d not deterministic for %q: %+v vs %+v", i, line, a[i], b[i])
			}
		}
		// No variant equals the truth line.
		for _, d := range a {
			if d.text == line {
				t.Fatalf("distractor %+v equals the truth line", d)
			}
		}
	}
}

func TestHardsetDistractorKinds(t *testing.T) {
	byKind := func(kind string, ds []hardsetDistractor) *hardsetDistractor {
		for i := range ds {
			if ds[i].kind == kind {
				return &ds[i]
			}
		}
		return nil
	}
	// Line 1: two clauses, no digit.
	ds := hardsetDistractors(hardsetTestLines[0])
	words := strings.Fields(hardsetTestLines[0])
	if d := byKind("swap", ds); d == nil {
		t.Fatalf("no swap distractor")
	} else {
		got := strings.Fields(d.text)
		if len(got) != len(words) {
			t.Fatalf("swap changed the word count: %d -> %d", len(words), len(got))
		}
		if got[0] != words[len(words)-1] || got[len(words)-1] != words[0] {
			t.Fatalf("swap did not exchange the first and last word: %q", d.text)
		}
	}
	for _, kind := range []string{"clause-last", "clause-first"} {
		d := byKind(kind, ds)
		if d == nil {
			t.Fatalf("no %s distractor", kind)
		} else {
			// The dropped-clause variant reuses only the line's own words (a
			// strict subset — no invented vocabulary), and is shorter than the
			// line.
			orig := map[string]bool{}
			for _, w := range strings.Fields(hardsetTestLines[0]) {
				orig[w] = true
			}
			for _, w := range strings.Fields(d.text) {
				if !orig[w] {
					t.Fatalf("%s distractor carries a word the line does not have: %q", kind, d.text)
				}
			}
			if len(d.text) >= len(hardsetTestLines[0]) {
				t.Fatalf("%s distractor is not shorter than the line: %q", kind, d.text)
			}
		}
	}
	// clause-last keeps the opening clause (its first word is the line's);
	// clause-first drops it (its first word is the second clause's).
	if d := byKind("clause-last", ds); d != nil && !strings.HasPrefix(d.text, strings.Fields(hardsetTestLines[0])[0]) {
		t.Fatalf("clause-last does not keep the line's opening word: %q", d.text)
	}
	if d := byKind("clause-first", ds); d != nil && strings.HasPrefix(d.text, strings.Fields(hardsetTestLines[0])[0]+" ") {
		t.Fatalf("clause-first still starts with the line's first clause: %q", d.text)
	}
	if d := byKind("number", ds); d != nil {
		t.Fatalf("number distractor on a digit-free line: %q", d.text)
	}
	if d := byKind("punct", ds); d == nil {
		t.Fatalf("no punct distractor")
	} else if !strings.HasSuffix(d.text, "!") {
		t.Fatalf("trailing-period punct edit wrong: %q", d.text)
	}
	// The prefix variants: word-boundary prefixes of the line's own words, at
	// half and three quarters of the word count, strictly shorter than the
	// line and each a true prefix of it. Line 3 (10 words, no clause
	// delimiter) exercises both kinds without the clause constructions
	// coinciding with them; on line 1 the 8-word prefix coincides with the
	// first clause, so the dedupe keeps the earlier clause-last registration
	// (constructions are tried in hardsetKinds order).
	words10 := strings.Fields(hardsetTestLines[2])
	ds3 := hardsetDistractors(hardsetTestLines[2])
	for _, tc := range []struct {
		kind string
		n    int
	}{
		{"prefix-half", len(words10) / 2},
		{"prefix-34", 3 * len(words10) / 4},
	} {
		d := byKind(tc.kind, ds3)
		if d == nil {
			t.Fatalf("no %s distractor", tc.kind)
		}
		want := strings.Join(words10[:tc.n], " ")
		if d.text != want {
			t.Fatalf("%s distractor is not the first %d words: %q", tc.kind, tc.n, d.text)
		}
		if !strings.HasPrefix(hardsetTestLines[2], d.text) {
			t.Fatalf("%s distractor is not a prefix of the line", tc.kind)
		}
	}
	words11 := strings.Fields(hardsetTestLines[0])
	if d := byKind("prefix-half", ds); d != nil && d.text != strings.Join(words11[:len(words11)/2], " ") {
		t.Fatalf("prefix-half distractor is not the line's first half: %q", d.text)
	}
	if byKind("prefix-34", ds) != nil { // expected absent: deduped into clause-last
		t.Fatalf("prefix-34 should be deduped into clause-last on line 1 (same 8-word text)")
	}
	// A short line (fewer than 4 words) gets no prefix variants: a one-word
	// prefix cannot carry a match and would be pure pool noise.
	if ds := hardsetDistractors("Yes. I know."); byKind("prefix-half", ds) != nil || byKind("prefix-34", ds) != nil {
		t.Fatal("prefix distractor on a line with fewer than 4 words")
	}
	// The punct variant's folded key equals the truth line's folded key:
	// the punctuation-identical near-duplicate, by construction.
	ds = hardsetDistractors(hardsetTestLines[0])
	if d := byKind("punct", ds); d != nil {
		if textnorm.Normalize(d.text, matchOpts) != textnorm.Normalize(hardsetTestLines[0], matchOpts) {
			t.Fatalf("punct variant changed the folded key: %q", d.text)
		}
	}
	// Line 2: has a digit.
	ds = hardsetDistractors(hardsetTestLines[1])
	if d := byKind("number", ds); d == nil {
		t.Fatalf("no number distractor on a line with a digit")
	} else {
		// Differs from the truth by exactly one rune, at the first digit.
		if len(d.text) != len(hardsetTestLines[1]) {
			t.Fatalf("number distractor changed the length: %q", d.text)
		}
		diff := 0
		for i := 0; i < len(d.text); i++ {
			if d.text[i] != hardsetTestLines[1][i] {
				diff++
				if !strings.ContainsRune("0123456789", rune(hardsetTestLines[1][i])) {
					t.Fatalf("number edit at a non-digit rune %d: %q", i, d.text)
				}
			}
		}
		if diff != 1 {
			t.Fatalf("number distractor differs by %d runes, want 1: %q", diff, d.text)
		}
	}
	// Line 3: no trailing period, no period at all — the punct fallback
	// appends '!'.
	ds = hardsetDistractors(hardsetTestLines[2])
	if d := byKind("punct", ds); d == nil {
		t.Fatalf("no punct distractor")
	} else if d.text != hardsetTestLines[2]+"!" {
		t.Fatalf("punct fallback appended the wrong rune: %q", d.text)
	}
}

func TestHardsetMessagesAugmentsOnlyTruthLines(t *testing.T) {
	msgs := []games.Message{
		{ID: "m1", OriginalText: hardsetTestLines[0], NormalizedText: textnorm.Normalize(hardsetTestLines[0], textnorm.Options{}), SceneID: "s", TextKind: games.TextDialogue},
		{ID: "m2", OriginalText: "An unrelated line the pool must not touch.", NormalizedText: "An unrelated line the pool must not touch.", SceneID: "s", TextKind: games.TextDialogue},
	}
	truth := map[string]string{"m1": hardsetTestLines[0]}
	aug := hardsetMessages(msgs, truth)

	// IDs: the two truths plus one distractor per kind present for m1 — and
	// nothing for m2 (a non-truth line stays out of the adversarial pool).
	want := map[string]bool{"m1": true, "m2": true}
	for _, d := range hardsetDistractors(hardsetTestLines[0]) {
		want["m1:hard-"+d.kind] = true
	}
	seen := map[string]bool{}
	for _, m := range aug {
		if !want[m.ID] {
			t.Fatalf("unexpected id %q (want exactly the truths plus m1's distractors; m2 must stay clean)", m.ID)
		}
		if seen[m.ID] {
			t.Fatalf("duplicate message id %q in the augmented script", m.ID)
		}
		seen[m.ID] = true
		if strings.HasPrefix(m.ID, "m1:hard-") && m.NormalizedText == "" {
			t.Fatalf("distractor %s has no normalized text", m.ID)
		}
	}
	if len(aug) != len(want) {
		t.Fatalf("augmented script has %d messages, want %d (the truths plus %d distractors)", len(aug), len(want), len(want)-2)
	}

	// Ordering: each distractor follows its truth message (the exact-key
	// tie-break relies on the truth's lower ordinal).
	posByID := map[string]int{}
	for i, m := range aug {
		posByID[m.ID] = i
	}
	for id := range want {
		if strings.HasPrefix(id, "m1:hard-") && posByID["m1"] >= posByID[id] {
			t.Fatalf("distractor %s does not follow its truth message (ordinal %d vs %d)", id, posByID[id], posByID["m1"])
		}
	}
}

// TestHardsetPoolMechanism is the class-(a) unit assertion of the error
// mechanism the hard set depends on (ADR-0058 amendment 4): over the
// adversarial pool, (1) a verbatim observation still ranks the truth first —
// the punct near-duplicate ties it on the folded key but the ordinal
// tie-break keeps the truth on top — and (2) a partial observation (the
// degraded read that captures only the line's first clause) ranks the
// dropped-clause distractor first: a genuine incorrect top-1 the matcher's
// own scoring produced.
func TestHardsetPoolMechanism(t *testing.T) {
	line := hardsetTestLines[0]
	msgs := []games.Message{
		{ID: "truth", OriginalText: line, NormalizedText: textnorm.Normalize(line, textnorm.Options{}), SceneID: "s", TextKind: games.TextDialogue},
		{ID: "filler-a", OriginalText: "Unrelated filler line one for the pool.", NormalizedText: "Unrelated filler line one for the pool.", SceneID: "s", TextKind: games.TextDialogue},
		{ID: "filler-b", OriginalText: "Unrelated filler line two for the pool.", NormalizedText: "Unrelated filler line two for the pool.", SceneID: "s", TextKind: games.TextDialogue},
	}
	aug := hardsetMessages(msgs, map[string]string{"truth": line})
	idx, err := matcher.Build(aug, nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	// (1) Verbatim read: the truth is top-1 even though its
	// punctuation-identical near-duplicate is in the pool.
	rank := idx.Rank(matcher.Observation{Text: line, OCRConf: 0, TextKindHint: "dialogue"}, matcher.RankOptions{})
	if rank.Candidates[0].MessageID != "truth" {
		t.Fatalf("verbatim read: top-1 = %q, want the truth (the punct near-duplicate must tie, not beat, it)", rank.Candidates[0].MessageID)
	}
	// The punct distractor must be present in the returned set (it competes).
	foundPunct := false
	for _, c := range rank.Candidates {
		if c.MessageID == "truth:hard-punct" {
			foundPunct = true
		}
	}
	if !foundPunct {
		t.Fatalf("punct near-duplicate not in the candidate set: %+v", rank.Candidates)
	}
	// (2) Partial read: only the first clause survived the degradation.
	firstClause := ""
	for _, c := range splitClauses(line) {
		firstClause = c
		break
	}
	partial := strings.TrimSuffix(firstClause, ".")
	rank = idx.Rank(matcher.Observation{Text: partial, OCRConf: 0, TextKindHint: "dialogue"}, matcher.RankOptions{})
	if rank.Candidates[0].MessageID != "truth:hard-clause-last" {
		t.Fatalf("partial read: top-1 = %q (probs %v), want the dropped-last-clause distractor — the incorrect top-1 the fit calibrates on",
			rank.Candidates[0].MessageID, probsOf(rank))
	}
	// The truth is still in the set (not a retrieval miss): the fit can use
	// the sample.
	truthPos := -1
	for i, c := range rank.Candidates {
		if c.MessageID == "truth" {
			truthPos = i
			break
		}
	}
	if truthPos < 0 {
		t.Fatalf("partial read: the truth candidate is a retrieval miss; the fit would skip it")
	}
}

// TestHardsetPoolPrefixMechanism is the class-(a) unit assertion of the
// error-producing construction (ADR-0058 amendment 5): a clean truncated
// read of the line (the typewriter reveal, or the line's tail lost to a
// partial-visibility frame) ranks a word-boundary prefix near-duplicate
// first — a genuine incorrect top-1 — while the truth stays in the returned
// set (not a retrieval miss), so the sample feeds the fit. On the same pool
// a verbatim read still ranks the truth first (the prefixes only compete on
// trigrams and lose to the exact key).
func TestHardsetPoolPrefixMechanism(t *testing.T) {
	line := hardsetTestLines[2] // 10 words, no clause delimiter: the prefix
	// family is the only near-duplicate that can sit at the read's
	// truncation depth below (the clause constructions do not exist here).
	msgs := []games.Message{
		{ID: "truth", OriginalText: line, NormalizedText: textnorm.Normalize(line, textnorm.Options{}), SceneID: "s", TextKind: games.TextDialogue},
		{ID: "filler-a", OriginalText: "Unrelated filler line one for the pool.", NormalizedText: "Unrelated filler line one for the pool.", SceneID: "s", TextKind: games.TextDialogue},
		{ID: "filler-b", OriginalText: "Unrelated filler line two for the pool.", NormalizedText: "Unrelated filler line two for the pool.", SceneID: "s", TextKind: games.TextDialogue},
	}
	aug := hardsetMessages(msgs, map[string]string{"truth": line})
	idx, err := matcher.Build(aug, nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("matcher.Build: %v", err)
	}
	words := strings.Fields(line)
	// Verbatim read: the truth is top-1 (the prefixes must not beat it here —
	// on clean frames the hard pool changes nothing).
	rank := idx.Rank(matcher.Observation{Text: line, OCRConf: 0, TextKindHint: "dialogue"}, matcher.RankOptions{})
	if rank.Candidates[0].MessageID != "truth" {
		t.Fatalf("verbatim read: top-1 = %q, want the truth", rank.Candidates[0].MessageID)
	}
	// Truncated read: the OCR captured the line's head and lost its tail (the
	// clip variant's reading shape), at a depth past prefix-34's 7 words for
	// this 10-word line: the read keeps 6.
	partial := strings.Join(words[:6], " ")
	rank = idx.Rank(matcher.Observation{Text: partial, OCRConf: 0, TextKindHint: "dialogue"}, matcher.RankOptions{})
	if rank.Candidates[0].MessageID != "truth:hard-prefix-34" {
		t.Fatalf("truncated read: top-1 = %q (probs %v), want the prefix-34 distractor — the incorrect top-1 the fit calibrates on",
			rank.Candidates[0].MessageID, probsOf(rank))
	}
	// The truth is still in the returned set: the fit can use the sample.
	truthPos := -1
	for i, c := range rank.Candidates {
		if c.MessageID == "truth" {
			truthPos = i
			break
		}
	}
	if truthPos < 1 {
		t.Fatalf("truncated read: the truth candidate is at %d (retrieval miss or top-1); the fit needs it in the set behind the distractor", truthPos)
	}
	// A deeper truncation, closer to the shorter prefix's length, lands the
	// shorter prefix on top instead (the nearest prefix wins, as the
	// component math of the Dice denominators says).
	deep := strings.Join(words[:4], " ")
	rank = idx.Rank(matcher.Observation{Text: deep, OCRConf: 0, TextKindHint: "dialogue"}, matcher.RankOptions{})
	top := rank.Candidates[0].MessageID
	if top != "truth:hard-prefix-half" && top != "truth:hard-prefix-34" {
		t.Fatalf("deep truncated read: top-1 = %q, want a prefix distractor (probs %v)", top, probsOf(rank))
	}
	for i, c := range rank.Candidates {
		if c.MessageID == "truth" {
			if i == 0 {
				t.Fatal("deep truncated read: the truth ranked first; the prefixes did not do their job")
			}
			return
		}
	}
	t.Fatal("deep truncated read: the truth candidate is a retrieval miss")
}

func probsOf(r matcher.Ranking) []float64 {
	out := make([]float64, 0, len(r.Candidates))
	for _, c := range r.Candidates {
		out = append(out, c.Probability)
	}
	return out
}

// TestHardsetVariantsDeterministic applies every hard variant to a
// deterministic in-memory frame and asserts re-application agrees (the
// bench-time reproducibility contract) and the output differs from the
// input (the variant is not a no-op on this content).
func TestHardsetVariantsDeterministic(t *testing.T) {
	img := deterministicTestImage()
	for _, v := range HardsetVariants() {
		a := v.fn(img)
		b := v.fn(img)
		if !imagesEqual(a, b) {
			t.Fatalf("variant %s is not deterministic", v.Name)
		}
		if imagesEqual(a, img) {
			t.Fatalf("variant %s is a no-op on the test image", v.Name)
		}
		// The variant's output must survive the container round-trip the bench
		// writer uses (JPEG, quality 95) without error.
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, a, &jpeg.Options{Quality: 95}); err != nil {
			t.Fatalf("%s: jpeg container encode of the transformed frame: %v", v.Name, err)
		}
		if _, err := jpeg.Decode(&buf); err != nil {
			t.Fatalf("%s: jpeg decode of the round-trip: %v", v.Name, err)
		}
	}
}

// deterministicTestImage is a small fixed-pattern frame (no game pixels).
func deterministicTestImage() image.Image {
	img := image.NewNRGBA(image.Rect(0, 0, 64, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x), G: uint8(y), B: 200, A: 255})
		}
	}
	return img
}
