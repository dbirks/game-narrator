package matcher

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// Every string below is invented for this file: the vocabulary is generated
// nonsense, the "shared" phrase is three generated words, and the fragment
// queries are slices of this file's own corpus. No line comes from a real
// script (docs/spec/04 two-game portability; AGENTS.md rule 5).
//
// The cases replay the board evidence from 2026-10-21 (a real profile's ~4300
// messages, OCR reading only a fragment of a dialogue box):
//
//   - a ~30-character read that is an exact substring starting deep inside a
//     105-character message,
//   - a substring at character 93 of a 208-character message (a long message
//     spanning several boxes),
//   - the first 16 characters of an 84-character message,
//
// every one of which ABSTAINED (top scores -9 to -10, margins 0.11-0.20). The
// cause measured on this corpus: a contained fragment scores on the
// similarity metrics only — trigram Dice 2|S|/(|Q|+|C|) caps near 0.4 for a
// 30-character query against a 100-character line, and the banded
// Levenshtein is exactly 0 because the length gap exceeds the band — so the
// text component lands at 0.2-0.35 and the margin gate abstains. Fragment-aware
// scoring (fragment.go) must make a unique fragment commit with a passing
// margin while an ambiguous fragment still abstains.

// fragCommon plays the role real scripts' high-frequency words play: its
// trigrams are corpus-popular (the retrieval popularity cut-off skips them),
// which is what makes the fragment's content trigrams carry the retrieval
// evidence.
var fragCommon = []string{
	"verel", "quon", "dax", "mirel", "toss", "fen", "ulor", "bek",
}

// sharedFragPhrase is the invented generic phrase (case e): it occurs in every
// corpus message, so a reading of it alone must be ambiguous.
const sharedFragPhrase = "verel quon dax"

// fragCorpus invents n messages of ~60-220 characters: a head of generated
// vocabulary, the shared middle phrase, and a longer tail. Every message is
// long enough that a 25-40 character middle window is strictly shorter than
// the whole line — the fragment regime.
func fragCorpus(n int) ([]games.Message, []games.MessageEdge) {
	rng := rand.New(rand.NewSource(77431))
	vocab := make([]string, 0, 160)
	cons, vows := "bcdfghjklmnpqrstvwxyz", "aeiou"
	for i := 0; i < 160; i++ {
		wordLen := 4 + rng.Intn(5)
		var b strings.Builder
		for i := 0; i < wordLen; i++ {
			if i%2 == 1 {
				b.WriteByte(vows[rng.Intn(len(vows))])
			} else {
				b.WriteByte(cons[rng.Intn(len(cons))])
			}
		}
		vocab = append(vocab, b.String())
	}
	pick := func(count int) string {
		words := make([]string, 0, count)
		for w := 0; w < count; w++ {
			words = append(words, vocab[rng.Intn(len(vocab))])
		}
		return strings.Join(words, " ")
	}
	msgs := make([]games.Message, 0, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		b.WriteString(pick(4))
		b.WriteString(" ")
		b.WriteString(sharedFragPhrase)
		b.WriteString(" ")
		b.WriteString(pick(6 + rng.Intn(14)))
		text := b.String()
		msgs = append(msgs, games.Message{
			ID: fmt.Sprintf("fg-%03d", i), ProfileID: "profile.fragment",
			OriginalText: text, NormalizedText: text, SpeechText: text,
			Language: "en", TextKind: games.TextDialogue,
			SceneID: fmt.Sprintf("fs-%02d", i/50), SpeakerID: "fsp",
		})
	}
	return msgs, nil
}

func fragTextOf(msgs []games.Message, id string) string {
	for _, m := range msgs {
		if m.ID == id {
			return m.NormalizedText
		}
	}
	return ""
}

// fragWindow returns an exact [start, start+length) rune window of message id's
// folded key: the board's "a read that is an exact substring of the line".
func fragWindow(t *testing.T, idx *Index, msgs []games.Message, id string, start, length int) string {
	t.Helper()
	key := idx.Fold(fragTextOf(msgs, id))
	r := []rune(key)
	if start < 0 || start+length > len(r) {
		t.Fatalf("window %d:%d out of range for %s (key len %d)", start, length, id, len(r))
	}
	return string(r[start : start+length])
}

// requireFragmentCommit asserts the commit rule a unique fragment must pass:
// no abstention, the fragment's own message on top, a margin at or above the
// configured floor, and text evidence that says "contained", not "similar".
func requireFragmentCommit(t *testing.T, label string, r Ranking, want string) {
	t.Helper()
	if r.Abstain {
		t.Fatalf("%s: unique fragment abstained (reason %s, top %s, text %.3f, margin %.3f) — the line was read but the matcher refused to name it",
			label, r.Reason, topID(r), fragTopText(r), r.Margin)
	}
	if topID(r) != want {
		t.Fatalf("%s: top = %s, want %s\n%s", label, topID(r), want, rankingString(r))
	}
	if r.Margin < DefaultRankOptions().MinMargin {
		t.Fatalf("%s: margin %.3f below the commit floor %.3f", label, r.Margin, DefaultRankOptions().MinMargin)
	}
	// 0.85: the length-aware floor (score.go) puts a contained fragment at
	// coverage·(0.9 + 0.1·len(read)/len(line)) — far above the 0.2–0.35 its
	// similarity metrics give it, below a candidate whose text IS the read.
	if got := r.Candidates[0].Components[ComponentText]; got < 0.85 {
		t.Fatalf("%s: text component %.3f, want >= 0.85: a contained fragment must score as contained evidence, not as a half-similarity", label, got)
	}
}

func fragTopText(r Ranking) float64 {
	if len(r.Candidates) == 0 {
		return 0
	}
	return r.Candidates[0].Components[ComponentText]
}
