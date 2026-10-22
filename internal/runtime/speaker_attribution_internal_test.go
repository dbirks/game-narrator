package runtime

// The ADR-0156 speaker gate: what a committed occurrence's voice is, given
// the adjudicator's per-occurrence speaker answer and whether the line's
// speaker slot is shared. Internal-package tests: the gate is unexported and
// is the whole decision the live play's mis-attribution came from.

import (
	"encoding/json"
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
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

// TestCommitSpeakerLowConfidenceFallsBackToNeutral: a shared slot whose
// adjudicator answer is below the confidence floor is not a name — the line
// must fall back to the neutral voice, never to the slot's linked character.
func TestCommitSpeakerLowConfidenceFallsBackToNeutral(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{"msg-1": true})
	d := commitPayload{
		MessageID:         "msg-1",
		SpeakerID:         "character-villager",
		SpeakerKnown:      true,
		SpeakerConfidence: 0.2,
	}
	if got := w.commitSpeaker(d); got != narratorCharacter {
		t.Fatalf("commitSpeaker = %q, want the neutral voice %q when the shared slot's answer is unconfident", got, narratorCharacter)
	}
}

// TestCommitSpeakerUnknownFallsBackToNeutral: an answer that named no
// speaker at all lands the same fallback.
func TestCommitSpeakerUnknownFallsBackToNeutral(t *testing.T) {
	w := newSpeakerGateWiring(map[string]string{"msg-1": "character-aryll"}, map[string]bool{"msg-1": true})
	d := commitPayload{MessageID: "msg-1", SpeakerID: "", SpeakerKnown: false}
	if got := w.commitSpeaker(d); got != narratorCharacter {
		t.Fatalf("commitSpeaker = %q, want the neutral voice %q when no speaker was named", got, narratorCharacter)
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
}
