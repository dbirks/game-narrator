package matcher

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

// Everything below is invented for this test file. No line here comes from a real
// script, and none may be replaced with one: docs/spec/04 requires the core to
// pass on a synthetic profile, and AGENTS.md forbids committing game script dumps.
// The vocabulary is deliberately pirate-adjacent nonsense so that a reader can see
// at a glance that it is not a transcription.

type lineSpec struct {
	text    string
	scene   string
	speaker string
	kind    games.TextKind
}

// miniScript is a 40-message hand-written script with the structures docs/spec/12
// "Dialogue and graph" names: a linear chain, a branch, a loop back, an NPC repeat,
// the same line spoken by two characters, short phrases that occur many times, and
// two lines that differ only in punctuation.
func miniScript() ([]games.Message, []games.MessageEdge) {
	lines := []lineSpec{
		{`The harbour bell rings twice before dawn.`, `scene.harbour`, `speaker.narrator`, games.TextDialogue},                                               // 00
		{`Merra ties the rope and says nothing.`, `scene.harbour`, `speaker.narrator`, games.TextDialogue},                                                   // 01
		{`We sail for the Gull-King's rock on the morning tide.`, `scene.harbour`, `speaker.merra`, games.TextDialogue},                                      // 02 near-miss target
		{`Torvik counts the crates again.`, `scene.harbour`, `speaker.torvik`, games.TextDialogue},                                                           // 03 self-repeat
		{`Nobody asks how many crates there are.`, `scene.harbour`, `speaker.narrator`, games.TextDialogue},                                                  // 04
		{`Which way does the wind carry us?`, `scene.openwater`, `speaker.oddis`, games.TextDialogue},                                                        // 05 branch point
		{`North, to the salt flats where the glass birds nest.`, `scene.openwater`, `speaker.merra`, games.TextDialogue},                                     // 06
		{`South, to the lantern caves under the standing wave.`, `scene.openwater`, `speaker.oddis`, games.TextDialogue},                                     // 07
		{`The crew argues until the cook reads the sail numbers aloud.`, `scene.openwater`, `speaker.narrator`, games.TextDialogue},                          // 08
		{`Oddis spits over the side and picks south.`, `scene.openwater`, `speaker.narrator`, games.TextDialogue},                                            // 09
		{`A gull lands on the mast and refuses to leave.`, `scene.openwater`, `speaker.narrator`, games.TextDialogue},                                        // 10 loop head
		{`Torvik offers it a crate and it stays.`, `scene.openwater`, `speaker.torvik`, games.TextDialogue},                                                  // 11 loop tail -> 10
		{`The water turns the colour of old copper.`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                                                  // 12
		{`Aye, the wind turns.`, `scene.isle`, `speaker.merra`, games.TextDialogue},                                                                          // 13 same-text pair, graph-supported
		{`Something is following the boat at a polite distance.`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                                      // 14
		{`Merra swears she can hear it counting.`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                                                     // 15
		{`Yes.`, `scene.isle`, `speaker.merra`, games.TextDialogue},                                                                                          // 16 short phrase (short-phrase test target)
		{`No.`, `scene.isle`, `speaker.oddis`, games.TextDialogue},                                                                                           // 17
		{`Thank you.`, `scene.isle`, `speaker.torvik`, games.TextDialogue},                                                                                   // 18
		{`The isle smells of wet rope and citrus.`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                                                    // 19
		{`Yes.`, `scene.isle`, `speaker.oddis`, games.TextDialogue},                                                                                          // 20
		{`A door in the cliff face has no handle.`, `scene.cave`, `speaker.narrator`, games.TextDialogue},                                                    // 21
		{`Aye, the wind turns.`, `scene.cave`, `speaker.oddis`, games.TextDialogue},                                                                          // 22 same text, different speaker
		{`Knock three times and wait for the tide to answer.`, `scene.cave`, `speaker.merra`, games.TextDialogue},                                            // 23
		{`Hold the line!`, `scene.cave`, `speaker.torvik`, games.TextDialogue},                                                                               // 24 punctuation-insensitive target
		{`The lanterns inside are lit by someone who is not here.`, `scene.cave`, `speaker.narrator`, games.TextDialogue},                                    // 25
		{`Bones, but politely arranged bones.`, `scene.cave`, `speaker.oddis`, games.TextDialogue},                                                           // 26
		{`Yes.`, `scene.cave`, `speaker.torvik`, games.TextDialogue},                                                                                         // 27
		{`Oddis pockets two of the lanterns and says nothing about it.`, `scene.cave`, `speaker.narrator`, games.TextDialogue},                               // 28
		{`The map on the wall is wrong in a way that matters.`, `scene.cave`, `speaker.merra`, games.TextItem},                                               // 29
		{`Beyond the reef the water turns the colour of old copper and every bird stops singing`, `scene.openwater`, `speaker.narrator`, games.TextDialogue}, // 30 unique long prefix
		{`The tidewatcher counts the boats until the count stops mattering tonight`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                   // 31 ambiguous prefix pair
		{`The tidewatcher counts the boats until the count stops mattering tomorrow`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                  // 32
		{`Thank you.`, `scene.cave`, `speaker.merra`, games.TextDialogue},                                                                                    // 33
		{`Yes.`, `scene.cave`, `speaker.merra`, games.TextDialogue},                                                                                          // 34
		{`No.`, `scene.cave`, `speaker.oddis`, games.TextDialogue},                                                                                           // 35
		{`Wait for me!`, `scene.cave`, `speaker.torvik`, games.TextDialogue},                                                                                 // 36 punctuation-only pair
		{`Wait for me.`, `scene.cave`, `speaker.oddis`, games.TextDialogue},                                                                                  // 37
		{`The Gull-King's rock is a rock and nothing more disappointing.`, `scene.isle`, `speaker.oddis`, games.TextDialogue},                                // 38 far node (graph-jump target)
		{`Everybody agrees to never speak of this island again.`, `scene.isle`, `speaker.narrator`, games.TextDialogue},                                      // 39
	}
	if len(lines) != 40 {
		// The test bodies reference lines by ordinal; a silent edit that shifts them
		// would make every scenario assert the wrong thing.
		panic(fmt.Sprintf("miniScript: expected 40 lines, got %d", len(lines)))
	}

	msgs := make([]games.Message, 0, len(lines))
	for i, l := range lines {
		msgs = append(msgs, games.Message{
			ID:             fmt.Sprintf("m%02d", i),
			ProfileID:      "profile.synthetic",
			OriginalText:   l.text,
			NormalizedText: l.text,
			SpeechText:     l.text,
			Language:       "en",
			TextKind:       l.kind,
			SceneID:        l.scene,
			SpeakerID:      l.speaker,
			Revision:       1,
		})
	}

	next := func(from, to int) games.MessageEdge {
		return edge(msgs[from].ID, msgs[to].ID, games.EdgeNext, 0.95)
	}
	edges := []games.MessageEdge{}
	for i := 0; i < 5; i++ {
		edges = append(edges, next(i, i+1))
	}
	edges = append(edges,
		// The branch at 05.
		edge(msgs[5].ID, msgs[6].ID, games.EdgeBranch, 0.9),
		edge(msgs[5].ID, msgs[7].ID, games.EdgeBranch, 0.9),
		next(7, 8), next(8, 9), next(9, 10), next(10, 11),
		// A loop back: the gull scene restarts.
		next(11, 10),
		// An NPC who says the same line twice (docs/spec/04's repeat edge).
		edge(msgs[3].ID, msgs[3].ID, games.EdgeRepeat, 0.8),
		next(12, 13), next(13, 14), next(14, 15), next(15, 16), next(16, 17),
		next(17, 18), next(18, 19), next(19, 20),
		// A scene change with only an inferred transition — provenance the
		// matcher must not let override contrary text (docs/spec/04).
		edge(msgs[20].ID, msgs[21].ID, games.EdgeInferred, 0.4),
		next(21, 22), next(22, 23), next(23, 24), next(24, 25), next(25, 26),
		next(26, 27), next(27, 28), next(28, 29), next(29, 30), next(30, 31),
		next(31, 32), next(32, 33), next(33, 34), next(34, 35), next(35, 36),
		next(36, 37), next(37, 38), next(38, 39),
		// The duplicate line in the cave has its own predecessor, so the graph
		// really does contain two ways to reach the same words.
		edge(msgs[28].ID, msgs[22].ID, games.EdgeContextual, 0.6),
	)
	return msgs, edges
}

func edge(from, to string, kind games.EdgeType, conf float64) games.MessageEdge {
	return games.MessageEdge{
		FromID:     from,
		ToID:       to,
		EdgeType:   kind,
		Confidence: conf,
		CreatedAt:  "2026-01-01T00:00:00Z",
	}
}

// mustBuild fails the test rather than returning an error, for the many cases
// where the interesting assertion is about ranking, not about building.
func mustBuild(t *testing.T, msgs []games.Message, edges []games.MessageEdge) *Index {
	t.Helper()
	idx, err := Build(msgs, edges, IndexOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx.MessageCount() != len(msgs) {
		t.Fatalf("indexed %d of %d messages", idx.MessageCount(), len(msgs))
	}
	return idx
}

func textOf(msgs []games.Message, id string) string {
	for _, m := range msgs {
		if m.ID == id {
			return m.NormalizedText
		}
	}
	return ""
}

// Synthetic corpus at scale. The vocabulary is generated from a fixed seed, so the
// 10k corpus is byte-identical on every machine and every run — a scale gate that
// depends on the host's clock or on map order would not be a gate.
func bigCorpus(n int) ([]games.Message, []games.MessageEdge) {
	rng := rand.New(rand.NewSource(20260607))
	vocab := make([]string, 0, 240)
	for i := 0; i < 240; i++ {
		vocab = append(vocab, inventedWord(rng, i))
	}
	msgs := make([]games.Message, 0, n)
	edges := make([]games.MessageEdge, 0, n)
	for i := 0; i < n; i++ {
		words := 8 + rng.Intn(24)
		parts := make([]string, words)
		for w := range parts {
			parts[w] = vocab[rng.Intn(len(vocab))]
		}
		text := strings.Join(parts, " ")
		id := fmt.Sprintf("big-%06d", i)
		msgs = append(msgs, games.Message{
			ID: id, ProfileID: "profile.scale",
			OriginalText: text, NormalizedText: text, SpeechText: text,
			Language: "en", TextKind: games.TextDialogue,
			SceneID:   fmt.Sprintf("scene.%03d", i/250),
			SpeakerID: fmt.Sprintf("speaker.%02d", i%17),
			Revision:  1,
		})
		if i+1 < n {
			// Mostly linear, with a few branches and inferred edges so graph
			// expansion is exercised at scale rather than only in the mini script.
			kind := games.EdgeNext
			conf := 0.95
			switch {
			case i%40 == 7:
				kind, conf = games.EdgeBranch, 0.7
			case i%211 == 3:
				kind, conf = games.EdgeInferred, 0.3
			}
			edges = append(edges, edge(id, fmt.Sprintf("big-%06d", i+1), kind, conf))
		}
	}
	return msgs, edges
}

// inventedWord builds a pronounceable nonsense word. It is generated rather than
// listed so the corpus has the trigram statistics of English-ish text (which is
// what makes the trigram index's work bounds meaningful) without borrowing anyone's
// vocabulary.
func inventedWord(rng *rand.Rand, seed int) string {
	const cons = "bcdfghjklmnpqrstvwxyz"
	const vows = "aeiou"
	n := 3 + (seed+rng.Intn(3))%9
	var b strings.Builder
	for i := 0; i < n; i++ {
		src := cons
		if i%2 == 1 {
			src = vows
		}
		b.WriteByte(src[rng.Intn(len(src))])
	}
	return b.String()
}
