package matcher

// The fragment scenarios, one test per board shape. The corpus is the invented
// ~200-message set (fragment_test.go); the queries are windows of it plus the
// small OCR damage the board showed (a trailing junk character, a leading "|").

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// fragAmbiguity counts how many corpus messages cover at least frac of the
// reading's interior trigrams — the test-side view of the matcher's own
// uniqueness question. It is computed from the corpus, not from the ranking, so
// the expectation is what the matcher's evidence says the answer must be.
//
// The matcher ranks the reading's raw and box-edge-trimmed forms (retrieve.go)
// and keeps the better top score, ties to the raw. The form that won is the
// evidence the commitment stands on, so the ambiguity is the winner's: a read
// that is unique raw but ambiguous after the trim eats a real character keeps
// its raw ranking and may commit, and a read the trim cleans up abstains if
// the cleaned-up read covers two lines. Mirroring the form selection here
// (rather than taking the max over both) is what keeps the expectation equal to
// the matcher's own evidence.
func fragAmbiguity(msgs []games.Message, idx *Index, reading string, frac float64) int {
	// run trims the reading's edges before ranking the raw form (retrieve.go
	// run), so the raw form's evidence is the trimmed text's, not the raw
	// bytes': a leading space is a box artifact, not a character of the line.
	rawKey := idx.Fold(strings.TrimSpace(reading))
	ambRaw := ambiguityOf(msgs, idx, rawKey, frac)
	trimmed := TrimEdgeNoise(reading)
	if trimmed == reading {
		return ambRaw
	}
	// rankText is the per-form ranking Rank returns the better of (retrieve.go
	// run: trimmed wins only on a strictly higher top raw score, ties keep the
	// raw). The oracle must ask for each form's own ranking, not Rank's already-
	// merged answer, or the comparison is circular.
	rawRank := formRanking(idx, Observation{Text: reading, OCRConf: 0.9})
	trimRank := formRanking(idx, Observation{Text: trimmed, OCRConf: 0.9})
	if rawTop(rawRank) < rawTop(trimRank) {
		return ambiguityOf(msgs, idx, idx.Fold(strings.TrimSpace(trimmed)), frac)
	}
	return ambRaw
}

// formRanking ranks one reading's form exactly the way run ranks it internally,
// without the raw-versus-trimmed merge (the test's oracle for that merge). It
// must populate the same per-query state run does — weights and visibility in
// particular — or every candidate's raw score is a constant offset from the
// real one and the form selection compares the wrong numbers.
func formRanking(idx *Index, obs Observation) Ranking {
	opts := DefaultRankOptions().sanitize()
	r := &ranker{idx: idx, opts: opts, weights: opts.Weights,
		counts: make([]int32, len(idx.ids))}
	r.obs = obs
	r.obs.Text = truncateBounds(strings.TrimSpace(obs.Text), maxTextRunes)
	r.visibility = visibilityOf(obs.OCRConf)
	return r.rankText(r.obs.Text)
}

func rawTop(r Ranking) float64 {
	if len(r.Candidates) == 0 {
		return 0
	}
	return r.Candidates[0].Score
}

// ambiguityOf counts how many corpus messages have a single window covering at
// least frac of the reading's interior trigrams — the test-side view of the
// matcher's own uniqueness question. It runs the matcher's own window metric
// (fragmentWindowCoverage) over the whole corpus rather than the retrieval
// pool: a corpus count of one bounds the pool count from above (the pool is a
// subset), so "at most one line explains the read" is the safe side, and a
// read several lines explain is ambiguous whether or not retrieval happened to
// surface them all. The metric is order-sensitive, so a line that merely
// contains the read's words in another order does not count.
func ambiguityOf(msgs []games.Message, idx *Index, qFold string, frac float64) int {
	qRunes := []rune(qFold)
	r := &ranker{idx: idx}
	r.obsRunes = qRunes
	r.obsIntKeys = interiorTrigramKeys(qRunes, nil)
	n := 0
	for _, m := range msgs {
		cRunes := []rune(idx.Fold(m.NormalizedText))
		if r.fragmentWindowCoverage(cRunes) >= frac {
			n++
		}
	}
	return n
}

func fragLabel(what, id, frag string) string {
	if len(frag) > 40 {
		frag = frag[:40] + "…"
	}
	return fmt.Sprintf("%s %s %q", what, id, frag)
}

// TestFragmentExactMiddleSubstring is case (a): an exact 25-40 character
// substring from the middle of a message must commit that message.
func TestFragmentExactMiddleSubstring(t *testing.T) {
	msgs, _ := fragCorpus(200)
	idx := mustBuild(t, msgs, nil)
	rng := rand.New(rand.NewSource(911))
	for i := 0; i < 60; i++ {
		m := msgs[rng.Intn(len(msgs))]
		key := []rune(idx.Fold(m.NormalizedText))
		length := 25 + rng.Intn(16) // 25-40, the board's read width
		start := rng.Intn(len(key) - length)
		frag := string(key[start : start+length])
		r := idx.Rank(Observation{Text: frag, OCRConf: 0.9}, RankOptions{})
		if fragAmbiguity(msgs, idx, frag, 0.9) > 1 {
			// The window is (nearly) contained in more than one line: the honest
			// answer is an abstain.
			if !r.Abstain {
				t.Fatalf("%s: ambiguous window committed %s", fragLabel("case a", m.ID, frag), topID(r))
			}
			continue
		}
		requireFragmentCommit(t, fragLabel("case a", m.ID, frag), r, m.ID)
	}
}

// TestFragmentMidWordCutAtEnd is case (b): the OCR read stops inside the last
// word of the box — the read is a genuine substring of the line, shorter than
// it, with no trailing context at all.
func TestFragmentMidWordCutAtEnd(t *testing.T) {
	msgs, _ := fragCorpus(200)
	idx := mustBuild(t, msgs, nil)
	rng := rand.New(rand.NewSource(913))
	for i := 0; i < 40; i++ {
		m := msgs[rng.Intn(len(msgs))]
		key := []rune(idx.Fold(m.NormalizedText))
		length := 20 + rng.Intn(20)
		// A window that ends inside the line (the read was cut mid-word).
		back := 1 + rng.Intn(maxInt((len(key)-length)/2, 1))
		start := len(key) - length - back
		frag := string(key[start : start+length])
		r := idx.Rank(Observation{Text: frag, OCRConf: 0.9}, RankOptions{})
		if fragAmbiguity(msgs, idx, frag, 0.9) > 1 {
			if !r.Abstain {
				t.Fatalf("%s: ambiguous cut committed %s", fragLabel("case b", m.ID, frag), topID(r))
			}
			continue
		}
		requireFragmentCommit(t, fragLabel("case b", m.ID, frag), r, m.ID)
	}
}

// TestFragmentWithSmallDamage is case (c): the board's damage profile on top of
// a real window — a leading "| " from the box border and one junk trailing
// character ("woe" read as "woet"). The fold strips the pipe; the junk char
// costs one trigram but must not push the fragment under the floor.
// (Stacking more damage on top — an interior misread as well as the junk tail —
// is the TestFragmentNeverCommitsAnotherLine contract: commit the right line or
// abstain, never another line.)
func TestFragmentWithSmallDamage(t *testing.T) {
	msgs, _ := fragCorpus(200)
	idx := mustBuild(t, msgs, nil)
	rng := rand.New(rand.NewSource(917))
	for i := 0; i < 40; i++ {
		m := msgs[rng.Intn(len(msgs))]
		key := []rune(idx.Fold(m.NormalizedText))
		length := 26 + rng.Intn(14)
		start := rng.Intn(len(key) - length)
		frag := string(key[start : start+length])
		damaged := "| " + frag + "t"
		r := idx.Rank(Observation{Text: damaged, OCRConf: 0.85}, RankOptions{})
		if fragAmbiguity(msgs, idx, damaged, 0.9) > 1 {
			if !r.Abstain {
				t.Fatalf("%s: ambiguous damaged read committed %s", fragLabel("case c", m.ID, damaged), topID(r))
			}
			continue
		}
		requireFragmentCommit(t, fragLabel("case c", m.ID, damaged), r, m.ID)
	}
}

// TestFragmentJunkStringAbstains is case (d): a 3-4 character junk string with
// no script support must still abstain. The fragment path must not be a way to
// commit short noise.
func TestFragmentJunkStringAbstains(t *testing.T) {
	msgs, _ := fragCorpus(200)
	idx := mustBuild(t, msgs, nil)
	for _, junk := range []string{"\u2122 a a", "| a a", "o o o", "x"} {
		r := idx.Rank(Observation{Text: junk, OCRConf: 0.9}, RankOptions{})
		if !r.Abstain {
			t.Fatalf("junk %q committed %s — short noise must abstain", junk, topID(r))
		}
	}
}

// TestFragmentGenericPhraseAbstains is case (e): a generic short phrase that
// occurs in many messages ("very well", here the invented "verel quon dax")
// must abstain — every message covering it is an equally good hypothesis.
func TestFragmentGenericPhraseAbstains(t *testing.T) {
	msgs, _ := fragCorpus(200)
	idx := mustBuild(t, msgs, nil)
	// Premise: the phrase is in every message, so it covers nobody uniquely.
	for _, m := range msgs[:5] {
		if !strings.Contains(m.NormalizedText, sharedFragPhrase) {
			t.Fatalf("corpus premise broken: %s lacks %q", m.ID, sharedFragPhrase)
		}
	}
	r := idx.Rank(Observation{Text: sharedFragPhrase, OCRConf: 0.9}, RankOptions{})
	if !r.Abstain {
		t.Fatalf("generic phrase %q committed %s (margin %.3f): it occurs in every message and must abstain",
			sharedFragPhrase, topID(r), r.Margin)
	}
	if r.Reason != ReasonAbstained {
		t.Fatalf("reason = %s, want %s", r.Reason, ReasonAbstained)
	}
	// And it must still show the rivals to a decision model: the shortlist is
	// non-empty and carries candidates a Clef call could narrow.
	if len(r.Candidates) == 0 {
		t.Fatal("abstain on the generic phrase returned no shortlist for adjudication")
	}
}

// TestFragmentNeverCommitsAnotherLine is the fragment-side of the corpus
// property TestPropertyUniqueLineAlwaysRecoverable: a corrupted fragment of a
// line never commits a *different* line with confidence.
func TestFragmentNeverCommitsAnotherLine(t *testing.T) {
	msgs, _ := fragCorpus(200)
	idx := mustBuild(t, msgs, nil)
	rng := rand.New(rand.NewSource(919))
	for i := 0; i < 40; i++ {
		m := msgs[rng.Intn(len(msgs))]
		key := []rune(idx.Fold(m.NormalizedText))
		length := 25 + rng.Intn(20)
		start := rng.Intn(len(key) - length)
		frag := corrupt(rng, string(key[start:start+length]))
		r := idx.Rank(Observation{Text: frag, OCRConf: 0.8}, RankOptions{})
		if r.Abstain {
			continue // honest
		}
		if topID(r) != m.ID {
			t.Fatalf("%s: committed %s\n%s", fragLabel("corrupt", m.ID, frag), topID(r), rankingString(r))
		}
	}
}
