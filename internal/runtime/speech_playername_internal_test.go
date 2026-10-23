package runtime

// The player-name placeholder (ADR-0165, #48): the pure resolution of the
// "{player}" slot onto the name the narrator speaks, and the per-session
// learning that earns one. Internal-package tests: the resolution and the
// vote are unexported, and the learned name is per-session state the wiring
// holds. Invented names and lines only.

import "testing"

type playerResolveCase struct {
	text string
	name string
	want string
}

// TestResolvePlayerPlaceholder: with a name the slot is substituted; without
// one it is dropped and the punctuation the drop orphans is tidied — the
// ADR's three shapes, plus the no-placeholder passthrough.
func TestResolvePlayerPlaceholder(t *testing.T) {
	for _, tc := range []playerResolveCase{
		// With a name: substitute the slot, keeping the sentence's shape.
		{text: "Hurry, {player}!", name: "Aryll", want: "Hurry, Aryll!"},
		{text: "It's you, {player}.", name: "Aryll", want: "It's you, Aryll."},
		{text: "{player}, wait.", name: "Aryll", want: "Aryll, wait."},
		// Without a name: drop the slot and tidy.
		{text: "Hurry, {player}!", name: "", want: "Hurry!"},
		{text: "{player}, wait.", name: "", want: "Wait."},
		{text: "It's you, {player}.", name: "", want: "It's you."},
		{text: "The hero {player} saves the day!", name: "", want: "The hero saves the day!"},
		// No slot: the text passes through untouched, whatever the name.
		{text: "No placeholder here", name: "Aryll", want: "No placeholder here"},
	} {
		if got := resolvePlayerPlaceholder(tc.text, tc.name); got != tc.want {
			t.Errorf("resolvePlayerPlaceholder(%q, %q) = %q, want %q", tc.text, tc.name, got, tc.want)
		}
	}
}

// TestPlayerNameCandidate: the slot's position in the script text counts the
// words before it, so the read's word at that index is the candidate — with
// the sentence-final punctuation the slot carried kept off the token.
func TestPlayerNameCandidate(t *testing.T) {
	for _, tc := range []struct {
		text string
		read string
		want string
	}{
		{text: "Hurry, {player}!", read: "Hurry, Link!", want: "Link"},
		{text: "It's you, {player}.", read: "It's you, Link.", want: "Link"},
		{text: "{player}, wait.", read: "Link, wait.", want: "Link"},
		{text: "Hurry, {player}!", read: "Hurry, Zzz!", want: "Zzz"},
	} {
		if got := playerNameCandidate(tc.text, tc.read); got != tc.want {
			t.Errorf("playerNameCandidate(%q, %q) = %q, want %q", tc.text, tc.read, got, tc.want)
		}
	}
}

// TestValidPlayerName: a plausible spoken name is two to twelve capitalised
// letters; the OCR noise fragments are not.
func TestValidPlayerName(t *testing.T) {
	for s, want := range map[string]bool{
		"Link":            true,
		"Aryll":           true,
		"Jo":              true,
		"WonderfulPerson": false, // too long
		"Z":               false, // too short
		"link":            false, // not capitalised in the read
		"4x":              false, // not a word
	} {
		if got := validPlayerName(s); got != want {
			t.Errorf("validPlayerName(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestLearnPlayerName: two agreeing reads of one slot trust the name for the
// session; a noisy or implausible read does not.
func TestLearnPlayerName(t *testing.T) {
	w := &speechWiring{playerVotes: map[string]int{}}

	// One agreeing read: not yet trusted, the dropped form still speaks.
	if got := w.learnPlayerName("Hurry, {player}!", "Hurry, Link!"); got != "" {
		t.Fatalf("after one agreeing read the session learned %q, want %q", got, "")
	}
	// The second agreeing read trusts the name.
	if got := w.learnPlayerName("Hurry, {player}!", "Hurry, Link!"); got != "Link" {
		t.Fatalf("after two agreeing reads the session learned %q, want %q", got, "Link")
	}
	// Once trusted, the session speaks the name from here on.
	if got := w.learnPlayerName("Hurry, {player}!", "Hurry, Link!"); got != "Link" {
		t.Fatalf("a trusted session keeps speaking %q, got %q", "Link", got)
	}

	// A different, still-valid candidate resets the vote: no trust yet.
	w2 := &speechWiring{playerVotes: map[string]int{}}
	if got := w2.learnPlayerName("Hurry, {player}!", "Hurry, Link!"); got != "" {
		t.Fatalf("w2 learned %q after one read, want %q", got, "")
	}
	if got := w2.learnPlayerName("Hurry, {player}!", "Hurry, Bair! "); got != "" {
		t.Fatalf("w2 learned %q after two reads of different names, want %q", got, "")
	}

	// A noisy or implausible read does not vote — and never trusts junk.
	w3 := &speechWiring{playerVotes: map[string]int{}}
	if got := w3.learnPlayerName("Hurry, {player}!", "Hurry, 4x!"); got != "" {
		t.Fatalf("a read with no word at the slot learned %q, want %q", got, "")
	}
	if got := w3.learnPlayerName("Hurry, {player}!", "Hurry, link!"); got != "" {
		t.Fatalf("a read whose candidate is not capitalised learned %q, want %q", got, "")
	}
	if got := w3.learnPlayerName("Hurry, {player}!", "Hurry, Z!"); got != "" {
		t.Fatalf("a read whose candidate is too short learned %q, want %q", got, "")
	}
	if got := w3.learnPlayerName("Hurry, {player}!", "Hurry, WonderfulPerson"); got != "" {
		t.Fatalf("a read whose candidate is too long learned %q, want %q", got, "")
	}
}

// TestLearnPlayerNameRejectsScriptWords: a partial read that shifts the
// slot onto a script word never votes that word as the name.
func TestLearnPlayerNameRejectsScriptWords(t *testing.T) {
	w := &speechWiring{playerVotes: map[string]int{}}
	for i := 0; i < 3; i++ {
		if got := w.learnPlayerName("{player}, wait for the tide.", "Wait for the tide."); got != "" {
			t.Fatalf("learned %q from a read without the name, want none", got)
		}
	}
}
