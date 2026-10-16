package matcher

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestBuildRejectsBadInput(t *testing.T) {
	msgs, edges := miniScript()

	if _, err := Build(nil, edges, IndexOptions{}); err != ErrNoMessages {
		t.Errorf("empty message list: got %v, want ErrNoMessages", err)
	}

	dup := append([]games.Message{}, msgs...)
	dup = append(dup, msgs[3])
	if _, err := Build(dup, edges, IndexOptions{}); err == ErrDuplicateID {
		t.Error("duplicate ID must be reported with the offending ID, not bare")
	} else if err == nil || !strings.Contains(err.Error(), msgs[3].ID) {
		t.Errorf("duplicate ID: got %v, want an error naming %s", err, msgs[3].ID)
	}

	empty := append([]games.Message{}, msgs...)
	empty[7].ID = ""
	if _, err := Build(empty, edges, IndexOptions{}); err == nil ||
		!strings.Contains(err.Error(), "index 7") {
		t.Errorf("empty ID: got %v, want an error locating index 7", err)
	}
}

func TestBuildReportsSkipsWithoutFailing(t *testing.T) {
	msgs, edges := miniScript()
	// A message with no text at all cannot be retrieved by text; and an edge whose
	// endpoints are missing is a partial import, not a corrupt profile. Both are
	// counted rather than turned into a hard failure (docs/spec/04 "partial dump").
	msgs = append(msgs, games.Message{ID: "m-empty", ProfileID: "profile.synthetic"})
	edges = append(edges, edge("m02", "m-does-not-exist", games.EdgeNext, 1))

	idx, err := Build(msgs, edges, IndexOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	st := idx.Stats()
	if st.SkippedMessages != 1 {
		t.Errorf("SkippedMessages = %d, want 1", st.SkippedMessages)
	}
	if st.SkippedEdges != 1 {
		t.Errorf("SkippedEdges = %d, want 1", st.SkippedEdges)
	}
	if st.Messages != len(msgs)-1 {
		t.Errorf("Messages = %d, want %d", st.Messages, len(msgs)-1)
	}
	if st.Keys == 0 || st.Trigrams == 0 || st.Postings < st.Trigrams {
		t.Errorf("implausible stats: %+v", st)
	}
	// Four lines say "Yes." and they must share one key — the ambiguity the
	// abstain policy is built around depends on it.
	if got := len(idx.Lookup("Yes.")); got != 4 {
		t.Errorf(`Lookup("Yes.") = %d hits, want 4`, got)
	}
}

func TestStatsShapeAndDeterminism(t *testing.T) {
	msgs, edges := miniScript()
	a, err := Build(msgs, edges, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Build(msgs, edges, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Stats() != b.Stats() {
		t.Errorf("stats differ between two builds of identical input: %+v vs %+v", a.Stats(), b.Stats())
	}
	// Approximate, but the right order of magnitude: a 40-line script is not
	// holding megabytes, and a mistake like storing trigram strings per message
	// would show up here immediately.
	if s := a.Stats(); s.ApproxBytes < 1000 || s.ApproxBytes > 1<<20 {
		t.Errorf("ApproxBytes = %d, implausible for 40 messages", s.ApproxBytes)
	}
}

func TestFoldMatchesStoredKeys(t *testing.T) {
	msgs, edges := miniScript()
	idx := mustBuild(t, msgs, edges)
	for _, m := range msgs {
		if got := idx.Fold(m.NormalizedText); got == "" {
			t.Errorf("Fold(%q) = empty", m.ID)
		} else if hits := idx.Lookup(got); len(hits) == 0 {
			t.Errorf("message %s is not findable by its own folded text", m.ID)
		}
	}
}

func TestFoldOptionsAreHonoured(t *testing.T) {
	msgs, _ := miniScript()
	// KeepPunctuation must separate "Wait for me!" from "Wait for me." again —
	// the profile-level knob that decides whether punctuation carries evidence
	// (docs/spec/02 keeps both lines; whether they are distinguishable at runtime
	// is the profile's call, not the matcher's).
	idx, err := Build(msgs, nil, IndexOptions{KeepPunctuation: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(idx.Lookup("Wait for me!")); got != 1 {
		t.Errorf("with punctuation kept, Lookup(Wait for me!) = %d, want 1", got)
	}
	if got := len(idx.Lookup("wait for me")); got != 0 {
		t.Errorf("punctuation-stripped query must not hit a punctuation-kept key, got %d", got)
	}

	// NoOCRFold must make the rn/m confusion a *miss* rather than a hit, which is
	// the only way to demonstrate the fold is doing the work the tests credit it
	// with.
	folded, err := Build(msgs, nil, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(folded.Lookup("we sail for the gull-king's rock on the rnorning tide")); got != 1 {
		t.Errorf("OCR-folded lookup of an rn-misread line = %d hits, want 1", got)
	}
	plain, err := Build(msgs, nil, IndexOptions{NoOCRFold: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(plain.Lookup("we sail for the gull-king's rock on the rnorning tide")); got != 0 {
		t.Errorf("with OCR folding off, an rn-misread must not hit exactly, got %d", got)
	}
}

func TestPlayerNamePlaceholderRoundTrip(t *testing.T) {
	msgs := []games.Message{{
		ID: "p1", ProfileID: "pr", OriginalText: "Well met, {player}!",
		NormalizedText: "Well met, {player}!", TextKind: games.TextDialogue,
	}}
	idx, err := Build(msgs, nil, IndexOptions{PlayerNames: []string{"Annika"}})
	if err != nil {
		t.Fatal(err)
	}
	// The screen shows the resolved name, the script holds the placeholder; the
	// folder maps both onto the stored key (docs/spec/05 "substitute variable
	// tokens using observed player name/locale when possible").
	if got := idx.Lookup("Well met, Annika!"); len(got) != 1 || got[0] != "p1" {
		t.Errorf(`Lookup("Well met, Annika!") = %v, want [p1]`, got)
	}
}

func TestEditionComponentReadsFlags(t *testing.T) {
	msgs := []games.Message{
		{ID: "e1", ProfileID: "pr", OriginalText: "A line unique to the demo build.",
			NormalizedText: "A line unique to the demo build.", TextKind: games.TextDialogue,
			Flags: json.RawMessage(`{"edition":"demo"}`)},
		{ID: "e2", ProfileID: "pr", OriginalText: "A line unique to the retail build.",
			NormalizedText: "A line unique to the retail build.", TextKind: games.TextDialogue,
			Flags: json.RawMessage(`{"edition":"retail"}`)},
	}
	idx, err := Build(msgs, nil, IndexOptions{})
	if err != nil {
		t.Fatal(err)
	}
	r := idx.Rank(Observation{Text: "A line unique to the demo build", Edition: "demo"}, RankOptions{})
	if r.Abstain || r.Candidates[0].MessageID != "e1" {
		t.Fatalf("edition-tagged observation: %+v", r)
	}
	if got := r.Candidates[0].Components[ComponentEdition]; got != 1 {
		t.Errorf("matching edition = %v, want 1", got)
	}
	// The other candidate is still retrieved (its text is close), but its edition
	// conflicts and the component must say so rather than staying silent.
	var other *Candidate
	for i := range r.Candidates {
		if r.Candidates[i].MessageID == "e2" {
			other = &r.Candidates[i]
		}
	}
	if other == nil {
		t.Fatal("the other-edition line should still be retrieved as a candidate")
	}
	if got := other.Components[ComponentEdition]; got != 0 {
		t.Errorf("conflicting edition = %v, want 0", got)
	}
	// With no edition stated by the caller, the component is agreement for both
	// (docs/spec/05's component is 1 when "matching or unspecified").
	r = idx.Rank(Observation{Text: "A line unique to the demo build"}, RankOptions{})
	if !r.Abstain && r.Candidates[0].Components[ComponentEdition] != 1 {
		t.Errorf("unspecified edition = %v, want 1", r.Candidates[0].Components[ComponentEdition])
	}
}

func TestConcurrentRankIsSafe(t *testing.T) {
	msgs, edges := miniScript()
	idx := mustBuild(t, msgs, edges)
	var wg sync.WaitGroup
	obs := Observation{Text: textOf(msgs, "m02"), SceneID: "scene.harbour"}
	want := idx.Rank(obs, RankOptions{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got := idx.Rank(obs, RankOptions{})
			if rankingString(got) != rankingString(want) {
				t.Errorf("worker %d disagreed with the serial ranking:\n %s\n %s",
					i, rankingString(got), rankingString(want))
			}
		}(i)
	}
	wg.Wait()
}

// rankingString renders everything a caller could observe, so determinism is
// asserted on the whole result rather than on the winner's ID alone.
func rankingString(r Ranking) string {
	var b strings.Builder
	fmt.Fprintf(&b, "reason=%s abstain=%v margin=%.17f;", r.Reason, r.Abstain, r.Margin)
	for _, c := range r.Candidates {
		fmt.Fprintf(&b, "%s:%.17f/%.17f{", c.MessageID, c.Score, c.Probability)
		for _, name := range ComponentOrder {
			fmt.Fprintf(&b, "%s=%.17f,", name, c.Components[name])
		}
		b.WriteString("};")
	}
	return b.String()
}
