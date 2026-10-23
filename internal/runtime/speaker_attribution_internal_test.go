package runtime

// The ADR-0156 speaker gate: what a committed occurrence's voice is, given
// the adjudicator's per-occurrence speaker answer and whether the line's
// speaker slot is shared. Internal-package tests: the gate is unexported and
// is the whole decision the live play's mis-attribution came from.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/store"
)

// newSpeakerGateWiring builds a speechWiring with just enough of the speaker
// tables the gate reads — no store, no scheduler, no fence.
func newSpeakerGateWiring(speakerByMessage map[string]string, shared map[string]bool) *speechWiring {
	return &speechWiring{
		id:                   "speaker-gate-test",
		speakerByMessage:     speakerByMessage,
		speakerSlotByMessage: shared,
	}
}

// newKindGateWiring builds a speechWiring with just enough of the ADR-0158
// kind tables the gate reads — the shared-slot view, the kind-key view, no
// store, no scheduler, no fence.
func newKindGateWiring(speakerByMessage map[string]string, shared map[string]bool, kind map[string]string) *speechWiring {
	return &speechWiring{
		id:                   "speaker-kind-test",
		speakerByMessage:     speakerByMessage,
		speakerSlotByMessage: shared,
		kindSpeakerByKey:     kind,
	}
}

// TestCommitSpeakerConfidentAdjudicatorWins is the ADR-0156 happy path: a
// slot that is shared (the live Wind Waker shape) and an adjudicator that
// named the speaker confidently — the confident answer wins, and the line
// voices the speaker the frame named, not the slot's linked character.
func TestCommitSpeakerConfidentAdjudicatorWins(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{"msg-1": true})
	d := commitPayload{
		MessageID:         "msg-1",
		SpeakerID:         "character-villager",
		SpeakerKnown:      true,
		SpeakerConfidence: 0.9,
	}
	if got := w.commitSpeaker(d); got != "character-villager" {
		t.Fatalf("commitSpeaker = %q, want the confident adjudicator's speaker %q", got, "character-villager")
	}
}

// TestCommitSpeakerLowConfidenceKeepsAssigned: a shared slot whose
// adjudicator answer is below the confidence floor is not a name, so the
// line keeps the slot's assigned character (ADR-0156 amendment 3).
func TestCommitSpeakerLowConfidenceKeepsAssigned(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{"msg-1": true})
	d := commitPayload{
		MessageID:         "msg-1",
		SpeakerID:         "character-villager",
		SpeakerKnown:      true,
		SpeakerConfidence: 0.2,
	}
	if got := w.commitSpeaker(d); got != "character-aryll" {
		t.Fatalf("commitSpeaker = %q, want the assigned character when the shared slot's answer is unconfident", got)
	}
}

// TestCommitSpeakerUnknownKeepsAssigned: an answer that named no speaker at
// all keeps the assignment too.
func TestCommitSpeakerUnknownKeepsAssigned(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{"msg-1": true})
	d := commitPayload{MessageID: "msg-1", SpeakerID: "", SpeakerKnown: false}
	if got := w.commitSpeaker(d); got != "character-aryll" {
		t.Fatalf("commitSpeaker = %q, want the assigned character when no speaker was named", got)
	}
}

// TestCommitSpeakerSingleCharacterSlotKeepsItsVoice: a slot that maps to a
// single character keeps the curator's assignment (the pre-ADR-0156 shape),
// and a confident answer naming that same character leaves it alone.
func TestCommitSpeakerSingleCharacterSlotKeepsItsVoice(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{})
	if got := w.commitSpeaker(commitPayload{MessageID: "msg-1"}); got != "character-aryll" {
		t.Fatalf("commitSpeaker = %q, want the assigned character %q for a single-speaker slot", got, "character-aryll")
	}
	if got := w.commitSpeaker(commitPayload{MessageID: "msg-1", SpeakerID: "character-aryll", SpeakerKnown: true, SpeakerConfidence: 0.95}); got != "character-aryll" {
		t.Fatalf("commitSpeaker = %q, want the assigned character %q when the confident answer names it", got, "character-aryll")
	}
}

// TestCommitSpeakerUnattributedLineIsTheNarrator: a line with no assignment
// and no confident answer is the narrator's, shared slot or not.
func TestCommitSpeakerUnattributedLineIsTheNarrator(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{}, map[string]bool{"msg-unknown": true})
	if got := w.commitSpeaker(commitPayload{MessageID: "msg-unknown"}); got != narratorCharacter {
		t.Fatalf("commitSpeaker = %q, want the neutral voice %q", got, narratorCharacter)
	}
}

// TestSharedSpeakerSlots is the ADR-0156 roster half of the gate: which of a
// published profile's lines resolve through a speaker slot that is shared —
// a character the script reaches through more than one distinct speaker key
// (the curator's merge of slots the live play showed) — so the speech side
// knows when the slot→character link must not speak for the line.
func TestSharedSpeakerSlots(t *testing.T) {
	// The curator's links: two slot keys onto one character, one slot key
	// onto another (which also carries a display alias).
	links := map[string]string{"ww:spk:11": "char-sister", "ww:spk:3": "char-sister", "ww:spk:7": "char-baker"}
	resolve := func(id string) string { return links[id] }
	content := store.ProfileContent{
		Characters: []games.Character{
			{ID: "char-sister", Name: "Sister", Aliases: json.RawMessage(`["ww:spk:11","ww:spk:3"]`)},
			{ID: "char-baker", Name: "Baker", Aliases: json.RawMessage(`["ww:spk:7","the baker"]`)},
		},
		Messages: []games.Message{
			{ID: "msg-1", SpeakerID: "ww:spk:11"},
			{ID: "msg-2", SpeakerID: "ww:spk:3"},
			{ID: "msg-3", SpeakerID: "ww:spk:7"},
			{ID: "msg-4", SpeakerID: "ww:spk:7"},
		},
	}
	shared := sharedSpeakerSlots(content, resolve)
	if !shared["msg-1"] || !shared["msg-2"] {
		t.Fatalf("shared = %v, want msg-1 and msg-2 (two slots merged onto one character) marked shared", shared)
	}
	// A name plus aliases is not sharing: one slot key reaches the baker.
	if shared["msg-3"] || shared["msg-4"] {
		t.Fatalf("shared = %v, want the single-slot character's lines not marked shared", shared)
	}

	// After speaker-link the lines carry the target's own id and the
	// placeholders are retired, their names folded into the target's
	// aliases: two folded placeholders is the merge, one is not.
	linked := store.ProfileContent{
		Characters: []games.Character{
			{ID: "char-sister", Name: "Sister", Status: games.CharacterConfirmed, Aliases: json.RawMessage(`["ww:spk:11","ww:spk:3","little sis"]`)},
			{ID: "char-baker", Name: "Baker", Status: games.CharacterConfirmed, Aliases: json.RawMessage(`["ww:spk:7","the baker"]`)},
			{ID: "ph-11", Name: "ww:spk:11", Status: games.CharacterRetired},
			{ID: "ph-3", Name: "ww:spk:3", Status: games.CharacterRetired},
			{ID: "ph-7", Name: "ww:spk:7", Status: games.CharacterRetired},
		},
		Messages: []games.Message{
			{ID: "msg-a", SpeakerID: "char-sister"},
			{ID: "msg-b", SpeakerID: "char-baker"},
		},
	}
	shared = sharedSpeakerSlots(linked, speakerLinkResolver(linked.Characters))
	if !shared["msg-a"] {
		t.Fatalf("shared = %v, want msg-a (two placeholders folded into one character) marked shared", shared)
	}
	if shared["msg-b"] {
		t.Fatalf("shared = %v, want msg-b (one placeholder folded) not marked shared", shared)
	}
}

// ── ADR-0158: the apparent-kind fallback (the generic voice) -----------------

// TestCommitVoiceKindConfidentRouted is the ADR-0158 happy path: a shared BMG
// slot the adjudicator could not name, answered with a confident apparent
// kind, whose roster carries a generic kind character — the line's voice is
// the kind character, not the narrator.
func TestCommitVoiceKindConfidentRouted(t *testing.T) {
	w := newKindGateWiring(map[string]string{}, map[string]bool{"msg-1": true},
		map[string]string{"kind:adult_male": "character-adult-male"})
	d := commitPayload{
		MessageID:             "msg-1",
		SpeakerID:             "character-villager",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2, // the slot answer could not name the speaker
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.9,
	}
	character, route := w.commitVoice(d)
	if character != "character-adult-male" {
		t.Fatalf("commitVoice character = %q, want the kind character %q", character, "character-adult-male")
	}
	if route != voiceRouteSpeakerKind {
		t.Fatalf("commitVoice route = %q, want %q (the kind-routed line)", route, voiceRouteSpeakerKind)
	}
}

// TestCommitVoiceKindUnconfidentFallsBackToNarrator: a kind below the
// confidence floor is not a kind — the narrator fallback stands (ADR-0158).
func TestCommitVoiceKindUnconfidentFallsBackToNarrator(t *testing.T) {
	w := newKindGateWiring(map[string]string{}, map[string]bool{"msg-1": true},
		map[string]string{"kind:adult_male": "character-adult-male"})
	d := commitPayload{
		MessageID:             "msg-1",
		SpeakerID:             "character-villager",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2,
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.2,
	}
	character, route := w.commitVoice(d)
	if character != narratorCharacter {
		t.Fatalf("commitVoice character = %q, want the neutral voice %q when the kind is unconfident", character, narratorCharacter)
	}
	if route != voiceRouteNarrator {
		t.Fatalf("commitVoice route = %q, want %q", route, voiceRouteNarrator)
	}
}

// TestCommitVoiceKindUnknownFallsBackToNarrator: the unknown kind option is
// the answer the kind question's fallback carries — no kind is routed.
func TestCommitVoiceKindUnknownFallsBackToNarrator(t *testing.T) {
	w := newKindGateWiring(map[string]string{}, map[string]bool{"msg-1": true},
		map[string]string{"kind:adult_male": "character-adult-male"})
	d := commitPayload{
		MessageID:             "msg-1",
		SpeakerID:             "character-villager",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2,
		SpeakerKind:           systemone.SpeakerUnknown,
		SpeakerKindConfidence: 0.9,
	}
	character, _ := w.commitVoice(d)
	if character != narratorCharacter {
		t.Fatalf("commitVoice character = %q, want the neutral voice %q when the kind is unknown", character, narratorCharacter)
	}
}

// TestCommitVoiceKindWithoutRosterCharacterFallsBackToNarrator: no roster
// character carries the kind key — the profile has no generic kind voice for
// that kind, and the narrator fallback stands.
func TestCommitVoiceKindWithoutRosterCharacterFallsBackToNarrator(t *testing.T) {
	w := newKindGateWiring(map[string]string{}, map[string]bool{"msg-1": true},
		map[string]string{"kind:adult_female": "character-adult-female"})
	d := commitPayload{
		MessageID:             "msg-1",
		SpeakerID:             "character-villager",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2,
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.9,
	}
	character, _ := w.commitVoice(d)
	if character != narratorCharacter {
		t.Fatalf("commitVoice character = %q, want the neutral voice %q when the roster has no kind character", character, narratorCharacter)
	}
}

// TestCommitVoiceKindUnattributedLineRouted: an unattributed line (no speaker
// answer at all) with a confident kind and a roster kind character speaks the
// kind character — the kind is the only identity the frame established.
func TestCommitVoiceKindUnattributedLineRouted(t *testing.T) {
	w := newKindGateWiring(map[string]string{}, map[string]bool{}, map[string]string{"kind:adult_male": "character-adult-male"})
	d := commitPayload{
		MessageID:             "msg-1",
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.9,
	}
	character, route := w.commitVoice(d)
	if character != "character-adult-male" {
		t.Fatalf("commitVoice character = %q, want the kind character %q", character, "character-adult-male")
	}
	if route != voiceRouteSpeakerKind {
		t.Fatalf("commitVoice route = %q, want %q", route, voiceRouteSpeakerKind)
	}
}

// TestCommitVoiceKindUnattributedWithoutKindFallsBackToNarrator: an
// unattributed line with no confident kind answers the neutral voice.
func TestCommitVoiceKindUnattributedWithoutKindFallsBackToNarrator(t *testing.T) {
	w := newKindGateWiring(map[string]string{}, map[string]bool{}, map[string]string{})
	d := commitPayload{MessageID: "msg-1", SpeakerKind: systemone.SpeakerKindAdultMale, SpeakerKindConfidence: 0.2}
	character, _ := w.commitVoice(d)
	if character != narratorCharacter {
		t.Fatalf("commitVoice character = %q, want the neutral voice %q when the kind is unconfident", character, narratorCharacter)
	}
}

// TestCommitVoiceConfidentNamedSpeakerWinsOverKind: a confident named speaker
// answer still wins over the kind (ADR-0158): the kind is the fallback, and a
// name the adjudicator was sure of is the name.
func TestCommitVoiceConfidentNamedSpeakerWinsOverKind(t *testing.T) {
	w := newKindGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{"msg-1": true},
		map[string]string{"kind:adult_male": "character-adult-male"})
	d := commitPayload{
		MessageID:             "msg-1",
		SpeakerID:             "character-villager",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.95,
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.9,
	}
	character, route := w.commitVoice(d)
	if character != "character-villager" {
		t.Fatalf("commitVoice character = %q, want the confident named speaker %q", character, "character-villager")
	}
	if route != voiceRouteSpeaker {
		t.Fatalf("commitVoice route = %q, want %q", route, voiceRouteSpeaker)
	}
}

// TestCommitVoiceSingleCharacterSlotUnchanged: a slot that maps to a single
// character keeps its assignment (the pre-ADR-0156 behaviour) — the kind does
// not reach it, and the route is the assignment's own.
func TestCommitVoiceSingleCharacterSlotUnchanged(t *testing.T) {
	w := newKindGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{},
		map[string]string{"kind:adult_male": "character-adult-male"})
	character, route := w.commitVoice(commitPayload{MessageID: "msg-1"})
	if character != "character-aryll" {
		t.Fatalf("commitVoice character = %q, want the assigned character %q", character, "character-aryll")
	}
	if route != voiceRouteAssigned {
		t.Fatalf("commitVoice route = %q, want %q", route, voiceRouteAssigned)
	}
}

// TestSplitCanonicalPagesEllipsis: an ellipsis is one sentence end, so no
// page of the runtime's fallback split is punctuation only (the board's
// presynth rendered such pages as all-silent audio). Invented text.
func TestSplitCanonicalPagesEllipsis(t *testing.T) {
	for _, canon := range []string{
		"Well, umm... Umm...",
		"Oh! Oh, thank... Thank the tides! You made it! When the gull dropped you here, I thought for sure you'd...",
		"Wait?! Really...? Yes!",
	} {
		pages := splitCanonicalPages(canon)
		if len(pages) == 0 {
			t.Fatalf("%q: no pages", canon)
		}
		for i, p := range pages {
			letters := false
			for _, r := range p {
				if unicode.IsLetter(r) {
					letters = true
					break
				}
			}
			if !letters {
				t.Errorf("%q: page %d = %q has no words", canon, i+1, p)
			}
		}
		if got := strings.Join(pages, " "); strings.Join(strings.Fields(got), " ") != strings.Join(strings.Fields(canon), " ") {
			t.Errorf("%q: pages rejoin to %q, want the canonical text", canon, got)
		}
	}
	if got := splitSentences("Well, umm... Umm..."); len(got) != 2 {
		t.Errorf("splitSentences = %q, want 2 sentences", got)
	}
}

// TestSpeakable: a punctuation-only box has nothing to say.
func TestSpeakable(t *testing.T) {
	for text, want := range map[string]bool{"......": false, "! ? !": false, ": !": false, "": false, "Hm.": true, "2!": true} {
		if got := speakable(text); got != want {
			t.Errorf("speakable(%q) = %v, want %v", text, got, want)
		}
	}
}

// TestReadThroughEcho: a garbled re-read of a line just spoken is an echo
// (the A/V harness phantom), an unrelated read is not, and an old line no
// longer counts. Invented text.
func TestReadThroughEcho(t *testing.T) {
	w := &speechWiring{}
	w.noteSpoken("You should probably head back and see what the baker wants.")
	if !w.isRecentEcho("You probzbly back and what") {
		t.Error("a garbled re-read of the spoken line was not an echo")
	}
	if w.isRecentEcho("The lighthouse keeper waves from the pier") {
		t.Error("an unrelated read counted as an echo")
	}
	w.mu.Lock()
	w.recentSpoken[0].at = time.Now().Add(-2 * echoWindow)
	w.mu.Unlock()
	if w.isRecentEcho("You probzbly back and what") {
		t.Error("a line spoken long ago still counted as the echo reference")
	}
	for a, b := range map[string]string{"probzbly": "probably", "and": "and", "see": "sea"} {
		if !withinOneEdit(a, b) {
			t.Errorf("withinOneEdit(%q, %q) = false", a, b)
		}
	}
	if withinOneEdit("home", "hollow") {
		t.Error("withinOneEdit(home, hollow) = true")
	}
}
