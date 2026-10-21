package store

// The shared voiced-count definition (ADR 0134): the profile's "n of m
// voiced" pair that the Library read, the status bar's setup-run progress
// and the Cast page header all derive from. The fixture mirrors the board
// the definition was written for — named voiced characters, named
// needs-a-voice characters, unnamed placeholders with lines, retired rows,
// and the edge rows (a retired character WITH lines and WITH a voice, an
// active character with no lines, a character whose only voice revision is
// retired) — and pins each of the definition's clauses to a number.

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/games"
)

func TestProfileCharacterVoiceCountsSharedDefinition(t *testing.T) {
	s := migratedStore(t)
	f := fixtureOn(t, s)
	ctx := f.ctx

	// named & voiced: confirmed, lines, an approved take (the board's ten).
	aryll := f.character("Aryll")
	// named, needs a voice: confirmed, lines, no take yet.
	grandma := f.character("Grandma")
	// unnamed placeholder: proposed, lines (the board's ww:spk:N rows).
	sp1 := f.character("ww:spk:1")
	sp1.Status = games.CharacterProposed
	sp2 := f.character("ww:spk:2")
	sp2.Status = games.CharacterProposed
	// retired WITH lines and WITH an approved voice: the row the old count
	// would have dragged into both numbers.
	dead := f.character("Retired One")
	dead.Status = games.CharacterRetired
	// active but silent: confirmed, no lines at all — not a speaker.
	quiet := f.character("Quiet")

	chars := []games.Character{aryll, grandma, sp1, sp2, dead, quiet}
	if err := f.s.PutCharacters(ctx, f.profile.ID, chars); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	line := func(c games.Character, text string) games.Message {
		t.Helper()
		m := f.message(text)
		m.SpeakerID = c.ID
		m.AttributionStatus = games.AttributionConfirmed
		return m
	}
	lines := []games.Message{
		line(aryll, "Aryll line one."),
		line(aryll, "Aryll line two."),
		line(grandma, "Grandma line one."),
		line(sp1, "Placeholder one, line one."),
		line(sp1, "Placeholder one, line two."),
		line(sp2, "Placeholder two, line one."),
		line(dead, "The retired character's line."),
	}
	if err := f.s.PutMessages(ctx, f.profile.ID, lines, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	// The voices, keyed by NAME the way the voice surface does:
	//  - Aryll: an approved take → voiced.
	//  - Grandma: a draft take in flight → still counted (the line has a
	//    voice once it lands; ADR 0109's auto-cast default).
	//  - ww:spk:1: an auditioning take → counted.
	//  - Retired One: an approved take → NOT counted (the character is
	//    retired; the take is the row's, not the roster's).
	//  - Quiet: an approved take → NOT counted (no lines; not a speaker).
	//  - Nobody: an approved take for a name the roster does not have →
	//    inert (the join finds no row).
	//  - Grandma's retired v1: a one-step undo (ADR 0109) → NOT counted on
	//    its own; Grandma's draft v2 keeps her in the numerator.
	for _, rev := range []VoiceProfile{
		testVoiceRevision(t, "Aryll", 1, VoiceStateApproved),
		testVoiceRevision(t, "Grandma", 1, VoiceStateRetired),
		testVoiceRevision(t, "Grandma", 2, VoiceStateDraft),
		testVoiceRevision(t, "ww:spk:1", 1, VoiceStateAuditioning),
		testVoiceRevision(t, "Retired One", 1, VoiceStateApproved),
		testVoiceRevision(t, "Quiet", 1, VoiceStateApproved),
		testVoiceRevision(t, "Nobody", 1, VoiceStateApproved),
	} {
		if err := s.CreateVoiceRevision(ctx, rev); err != nil {
			t.Fatalf("CreateVoiceRevision(%s v%d): %v", rev.Character, rev.Revision, err)
		}
	}

	characters, voiced, err := s.ProfileCharacterVoiceCounts(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("ProfileCharacterVoiceCounts: %v", err)
	}
	// The denominator: Aryll, Grandma, ww:spk:1, ww:spk:2 — Retired One is
	// retired and Quiet has no lines.
	if characters != 4 {
		t.Errorf("characters = %d, want 4 (active speaking characters only)", characters)
	}
	// The numerator: Aryll (approved), Grandma (draft in flight), ww:spk:1
	// (auditioning) — Retired One's take and Quiet's take do not count.
	if voiced != 3 {
		t.Errorf("voiced = %d, want 3 (the name join, retired rows and 0-line rows out)", voiced)
	}
}

// A profile whose only voice row is retired reads "not voiced", and a take
// that was never retired (approved) still counts after its successor was
// retired by the one-step undo: the runtime speaks the highest approved, and
// the count must agree with what the box actually speaks.
func TestProfileCharacterVoiceCountsUndoLeavesTheCountHonest(t *testing.T) {
	s := migratedStore(t)
	f := fixtureOn(t, s)
	ctx := f.ctx

	boy := f.character("Boy")
	girl := f.character("Girl")
	if err := f.s.PutCharacters(ctx, f.profile.ID, []games.Character{boy, girl}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}
	for _, who := range []games.Character{boy, girl} {
		m := f.message("A line.")
		m.SpeakerID = who.ID
		m.AttributionStatus = games.AttributionConfirmed
		if err := f.s.PutMessages(ctx, f.profile.ID, []games.Message{m}, nil); err != nil {
			t.Fatalf("PutMessages: %v", err)
		}
	}

	// Boy: v1 approved, v2 retired — the one-step undo (ADR 0109). The lower
	// approved take still speaks, so Boy stays voiced.
	// Girl: her only take is retired (the regeneration that replaced it was
	// itself undone later — the row set says "no live take").
	for _, rev := range []VoiceProfile{
		testVoiceRevision(t, "Boy", 1, VoiceStateApproved),
		testVoiceRevision(t, "Boy", 2, VoiceStateRetired),
		testVoiceRevision(t, "Girl", 1, VoiceStateRetired),
	} {
		if err := s.CreateVoiceRevision(ctx, rev); err != nil {
			t.Fatalf("create %s v%d: %v", rev.Character, rev.Revision, err)
		}
	}

	characters, voiced, err := s.ProfileCharacterVoiceCounts(ctx, f.profile.ID)
	if err != nil {
		t.Fatalf("ProfileCharacterVoiceCounts: %v", err)
	}
	// Both characters are active speakers (two in the denominator); only Boy
	// holds a take that is not retired (one in the numerator). Girl's
	// retired-only take and her line count do not count her as voiced.
	if characters != 2 || voiced != 1 {
		t.Errorf("the pair is (%d, %d), want (2, 1): the undo keeps Boy voiced, Girl's retired-only take does not", characters, voiced)
	}
}
