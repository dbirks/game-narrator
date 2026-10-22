package runtime

// White-box tests for the narrator fallback and the speaker-link resolution
// (issue #11, ADR-0101): a line whose speaker has no approved voice is spoken
// by the narrator voice when one is configured and the profile's fallback is
// on; the skip reason names what kept the line silent (the setting, or the
// roster); and a line whose speaker is a placeholder the curator identified
// resolves to the character the placeholder was linked to.
//
// These drive the wiring's resolveVoice directly with a real store and a real
// asset store, so the voice rows and the reference blobs are the real thing —
// no mocked resolution.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/voices"
)

// seedFallbackVoice gives one character (or the narrator constant) a single
// approved, referenced voice revision and returns its id. No registration row
// is written: resolveVoice takes the inline-reference path, which is the one a
// freshly imported appliance takes before its Reconcile pass re-registers.
func seedFallbackVoice(t *testing.T, st *store.Store, a *assets.Local, character string) string {
	t.Helper()
	ctx := context.Background()
	ref := speech.MakeToneWAV(220, 1.0, 16000)
	sum := sha256.Sum256(ref)
	refSHA := hex.EncodeToString(sum[:])
	id, _, err := a.PutStaged(ctx, bytes.NewReader(ref), refSHA)
	if err != nil {
		t.Fatalf("staging reference for %s: %v", character, err)
	}
	if _, err := a.Commit(ctx, id); err != nil {
		t.Fatalf("committing reference for %s: %v", character, err)
	}
	profID := games.NewID()
	now := time.Now().UTC()
	if err := st.CreateVoiceRevision(ctx, store.VoiceProfile{
		ID:                profID,
		Character:         character,
		Revision:          1,
		State:             store.VoiceStateApproved,
		DesignInstruction: "a calm test voice",
		ReferenceAssetID:  refSHA,
		RefTranscript:     voices.SyntheticAuditionLines[0],
		ModelID:           "fake-base",
		Language:          "en",
		ConsentProvenance: "original_synthetic_design (automated test)",
		ApprovedBy:        "test",
		Fingerprint:       voices.Fingerprint(refSHA, voices.SyntheticAuditionLines[0], "fake-base"),
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("creating approved voice for %s: %v", character, err)
	}
	return profID
}

// newFallbackWiring builds a speechWiring that resolves voices against a real
// store and asset store: just enough of the struct for resolveVoice, nothing
// the scheduler or the fence needs.
func newFallbackWiring(t *testing.T, st *store.Store, a *assets.Local, speakerByMessage map[string]string, narratorFallback bool) *speechWiring {
	t.Helper()
	svc := &voices.Service{Store: st, Assets: a, InstanceID: "primary", BaseModelID: "fake-base", BackendDigest: "test-backend-v1"}
	return &speechWiring{
		id:               "fallback-test",
		log:              func(string, ...any) {},
		voices:           svc,
		assets:           a,
		speakerByMessage: speakerByMessage,
		narratorFallback: narratorFallback,
	}
}

// TestResolveVoiceNarratorFallback is the ADR-0101 matrix: what a line whose
// speaker has no approved voice resolves to, under each combination of the
// narrator's roster state and the profile's fallback setting.
func TestResolveVoiceNarratorFallback(t *testing.T) {
	const line = "line-1"

	newEnv := func(t *testing.T) (*store.Store, *assets.Local) {
		t.Helper()
		ctx := context.Background()
		st, err := store.Open(ctx, store.DBPath(t.TempDir()))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		if _, err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		a, err := assets.NewLocal(t.TempDir())
		if err != nil {
			t.Fatalf("assets: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st, a
	}

	for _, tc := range []struct {
		name           string
		speakerVoice   bool // the speaker character has an approved voice
		narratorVoice  bool // the narrator has an approved voice
		fallback       bool // the profile's narrator fallback
		wantReason     string
		wantNarratorID string // the voice id the line should resolve to, when any
	}{
		{"speaker has a voice", true, true, true, "", ""},
		{"speaker voiceless, narrator voice, fallback on", false, true, true, "", ""},
		{"speaker voiceless, narrator voice, fallback off", false, true, false, "narrator_fallback_disabled", ""},
		{"speaker voiceless, no narrator voice, fallback on", false, false, true, "no_approved_voice", ""},
		{"speaker voiceless, no narrator voice, fallback off", false, false, false, "no_approved_voice", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, a := newEnv(t)
			const speaker = "captain"
			if tc.speakerVoice {
				seedFallbackVoice(t, st, a, speaker)
			}
			var narratorID string
			if tc.narratorVoice {
				narratorID = seedFallbackVoice(t, st, a, narratorCharacter)
			}
			w := newFallbackWiring(t, st, a, map[string]string{line: speaker}, tc.fallback)
			rv, why := w.resolveVoice(context.Background(), line)
			// A resolved voice always names the speaker's own id; a skip
			// (empty id) carries the reason.
			if tc.speakerVoice {
				if rv.profile.Character != speaker {
					t.Fatalf("resolved voice character = %q, want the speaker %q", rv.profile.Character, speaker)
				}
				if why != "" {
					t.Fatalf("a resolvable line reported reason %q, want none", why)
				}
				return
			}
			if tc.narratorVoice && tc.fallback {
				// The narrator speaks the line.
				if rv.profile.ID != narratorID {
					t.Fatalf("resolved voice = %s (character %s), want the narrator %s", rv.profile.ID, rv.profile.Character, narratorID)
				}
				if why != "" {
					t.Fatalf("the narrator fallback reported reason %q, want none", why)
				}
				return
			}
			// A skip: no voice, and the reason names the cause.
			if rv.profile.ID != "" {
				t.Fatalf("a skipped line resolved to voice %s, want none", rv.profile.ID)
			}
			if why != tc.wantReason {
				t.Fatalf("skip reason = %q, want %q", why, tc.wantReason)
			}
		})
	}
}

// TestResolveVoiceNarratorOwnLine is the degenerate case: the narrator's own
// line, with the narrator voiceless. There is no fallback question to ask —
// the speaker and the fallback are the same voice — so the reason is the
// roster's, not the setting's.
func TestResolveVoiceNarratorOwnLine(t *testing.T) {
	const line = "line-1"
	newEnv := func(t *testing.T) (*store.Store, *assets.Local) {
		t.Helper()
		ctx := context.Background()
		st, err := store.Open(ctx, store.DBPath(t.TempDir()))
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		if _, err := st.Migrate(ctx); err != nil {
			t.Fatalf("migrate: %v", err)
		}
		a, err := assets.NewLocal(t.TempDir())
		if err != nil {
			t.Fatalf("assets: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st, a
	}

	// Voiceless narrator, unattributed line: no_approved_voice, fallback on or off.
	for _, fallback := range []bool{true, false} {
		st, a := newEnv(t)
		w := newFallbackWiring(t, st, a, map[string]string{}, fallback) // no speaker → narrator
		rv, why := w.resolveVoice(context.Background(), line)
		if rv.profile.ID != "" {
			t.Fatalf("a voiceless narrator resolved to %s, want no voice", rv.profile.ID)
		}
		if why != "no_approved_voice" {
			t.Fatalf("narrator's own voiceless line: reason = %q, want no_approved_voice", why)
		}
	}
}

// TestSpeakerLinkResolver is the runtime half of the speaker link (ADR-0070's
// link, ADR-0101): a line whose speaker character is identified by another
// live character — the placeholder's name, folded into the target's aliases —
// resolves to the target, so the line speaks with the target's voice.
func TestSpeakerLinkResolver(t *testing.T) {
	// A roster: the target carries the placeholder's name in its aliases; the
	// placeholder is retired (its identity is the target's); an unrelated
	// character is untouched.
	target := games.Character{ID: "target-id", Name: "Captain Vayne", Status: games.CharacterConfirmed, Aliases: json.RawMessage(`["Vayne","ww:spk:1"]`)}
	placeholder := games.Character{ID: "placeholder-id", Name: "ww:spk:1", Status: games.CharacterRetired}
	unrelated := games.Character{ID: "unrelated-id", Name: "Barnacle", Status: games.CharacterConfirmed, Aliases: json.RawMessage(`[]`)}
	named := games.Character{ID: "named-id", Name: "The Harbourmaster", Status: games.CharacterConfirmed}

	roster := []games.Character{target, placeholder, unrelated, named}
	resolve := speakerLinkResolver(roster)

	cases := map[string]string{
		// The placeholder's line resolves to the target.
		"placeholder-id": "target-id",
		// The target and the unrelated character resolve to themselves.
		"target-id":    "target-id",
		"unrelated-id": "unrelated-id",
		"named-id":     "named-id",
		// An unattributed line stays unattributed.
		"": "",
		// An unknown speaker id resolves to itself (nothing invented).
		"ghost-id": "ghost-id",
	}
	for speaker, want := range cases {
		if got := resolve(speaker); got != want {
			t.Errorf("resolve(%q) = %q, want %q", speaker, got, want)
		}
	}

	// A retired speaker whose name no live character carries stays where it is:
	// the link the curator never made is not invented by the resolver.
	orphan := games.Character{ID: "orphan-id", Name: "ww:spk:9", Status: games.CharacterRetired}
	if got := speakerLinkResolver([]games.Character{orphan})("orphan-id"); got != "orphan-id" {
		t.Errorf("a placeholder with no target resolves to %q, want itself (no invented link)", got)
	}
}

// TestResolveVoiceByCharacterNameAlias verifies that a voice profile stored
// under the character's display Name (as created by the Cast UI / auto-cast,
// ADR-0109/ADR-0135) is resolved when speakerByMessage maps the message to the
// character's row ID and speakerAliasByID maps the row ID to the display Name.
func TestResolveVoiceByCharacterNameAlias(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const charID = "01M5BHWSNQR26YAGNMVXG00001"
	const charName = "Aryll"
	const lineID = "line-aryll-1"

	// Auto-cast / Cast UI creates the voice keyed by character display Name.
	aryllVoiceID := seedFallbackVoice(t, st, a, charName)
	_ = seedFallbackVoice(t, st, a, narratorCharacter)

	w := newFallbackWiring(t, st, a, map[string]string{lineID: charID}, true)
	w.speakerAliasByID = map[string]string{charID: charName}

	rv, why := w.resolveVoice(ctx, lineID)
	if why != "" {
		t.Fatalf("resolveVoice reported skip reason %q, want none", why)
	}
	if rv.profile.ID != aryllVoiceID {
		t.Fatalf("resolved voice ID = %s (character %q), want Aryll's voice %s (not narrator fallback)",
			rv.profile.ID, rv.profile.Character, aryllVoiceID)
	}
}

// TestResolveVoiceKindCharacter is the ADR-0158 end-to-end half of the kind
// gate: a confident apparent-kind answer on a line whose speaker gate fell
// back voices through the roster's generic kind character — and that
// character's own approved voice is the line's voice, not the narrator's.
// The kind character without a usable voice keeps the narrator fallback.
func TestResolveVoiceKindCharacter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	const kindChar = "character-adult-male"
	kindVoiceID := seedFallbackVoice(t, st, a, kindChar)
	_ = seedFallbackVoice(t, st, a, narratorCharacter)

	w := newFallbackWiring(t, st, a, map[string]string{"line-1": "character-aryll"}, true)
	w.speakerSlotByMessage = map[string]bool{"line-1": true} // the shared-slot shape
	w.kindSpeakerByKey = map[string]string{"kind:adult_male": kindChar}

	// The speaker gate fell back (unconfident speaker on a shared slot) and
	// the kind was confident: the line voices the kind character, whose voice
	// is the kind character's own approved voice.
	d := commitPayload{
		MessageID:             "line-1",
		SpeakerID:             "character-aryll",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2, // the slot answer could not name the speaker
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.9,
	}
	character, route := w.commitVoice(d)
	if character != kindChar {
		t.Fatalf("commitVoice character = %q, want the kind character %q", character, kindChar)
	}
	if route != voiceRouteSpeakerKind {
		t.Fatalf("commitVoice route = %q, want %q", route, voiceRouteSpeakerKind)
	}
	rv, why := w.resolveVoiceForCharacter(ctx, character)
	if why != "" {
		t.Fatalf("the kind character's voice resolved with reason %q, want none", why)
	}
	if rv.profile.ID != kindVoiceID {
		t.Fatalf("resolved voice = %s (character %q), want the kind character's voice %s", rv.profile.ID, rv.profile.Character, kindVoiceID)
	}

	// An unconfident kind keeps the narrator fallback for the same line.
	w2 := newFallbackWiring(t, st, a, map[string]string{"line-1": "character-aryll"}, true)
	w2.speakerSlotByMessage = map[string]bool{"line-1": true}
	w2.kindSpeakerByKey = map[string]string{"kind:adult_male": kindChar}
	d2 := commitPayload{
		MessageID:             "line-1",
		SpeakerID:             "character-aryll",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2,
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.2,
	}
	character2, route2 := w2.commitVoice(d2)
	if character2 != narratorCharacter || route2 != voiceRouteNarrator {
		t.Fatalf("an unconfident kind resolved to %q/%q, want the narrator fallback", character2, route2)
	}
}

// TestKindVoiceKeyWithoutRosterCharacter is the ADR-0158 amendment: no roster
// character claims the kind, but a voice is approved under the opaque key
// itself ("kind:adult_male") — the line routes to that voice. A kind with no
// such voice keeps the narrator fallback.
func TestKindVoiceKeyWithoutRosterCharacter(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	kindVoiceID := seedFallbackVoice(t, st, a, "kind:adult_male")
	_ = seedFallbackVoice(t, st, a, narratorCharacter)

	w := newFallbackWiring(t, st, a, map[string]string{"line-1": "character-aryll"}, true)
	w.speakerSlotByMessage = map[string]bool{"line-1": true}
	w.kindSpeakerByKey = map[string]string{} // no roster kind character
	w.addKindVoiceKeys(ctx)

	d := commitPayload{
		MessageID:             "line-1",
		SpeakerID:             "character-aryll",
		SpeakerKnown:          true,
		SpeakerConfidence:     0.2,
		SpeakerKind:           systemone.SpeakerKindAdultMale,
		SpeakerKindConfidence: 0.9,
	}
	character, route := w.commitVoice(d)
	if character != "kind:adult_male" || route != voiceRouteSpeakerKind {
		t.Fatalf("commitVoice = %q/%q, want the kind voice key via %q", character, route, voiceRouteSpeakerKind)
	}
	rv, why := w.resolveVoiceForCharacter(ctx, character)
	if why != "" || rv.profile.ID != kindVoiceID {
		t.Fatalf("resolved voice = %s (reason %q), want the kind voice %s", rv.profile.ID, why, kindVoiceID)
	}

	// A kind nobody voiced stays on the narrator.
	d.SpeakerKind = systemone.SpeakerKindGirl
	if character, route := w.commitVoice(d); character != narratorCharacter || route != voiceRouteNarrator {
		t.Fatalf("an unvoiced kind resolved to %q/%q, want the narrator fallback", character, route)
	}
}

// TestReachableKeysCoverSharedSlotVoices (ADR-0159 with ADR-0156/0158): the
// prune's reachable set keeps every key the live path can produce for a
// shared-slot line — the narrator's, the linked character's and each approved
// kind voice's — and exactly one key for a single-speaker line.
func TestReachableKeysCoverSharedSlotVoices(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	a, err := assets.NewLocal(t.TempDir())
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	_ = seedFallbackVoice(t, st, a, narratorCharacter)
	_ = seedFallbackVoice(t, st, a, "character-sister")
	_ = seedFallbackVoice(t, st, a, "kind:boy")
	svc := &voices.Service{Store: st, Assets: a, InstanceID: "primary", BaseModelID: "fake-base", BackendDigest: "test-backend-v1"}
	speakers := map[string]string{"msg-shared": "character-sister", "msg-single": "character-sister"}
	slots := map[string]bool{"msg-shared": true}

	shared, err := reachableKey(ctx, st, a, svc, "msg-shared", "msg-shared", "A line on a shared slot.", speakers, slots, nil, true)
	if err != nil {
		t.Fatalf("reachableKey: %v", err)
	}
	if len(shared) != 3 {
		t.Fatalf("shared-slot keys = %d, want 3 (narrator, linked character, kind:boy)", len(shared))
	}
	single, err := reachableKey(ctx, st, a, svc, "msg-single", "msg-single", "A line with one speaker.", speakers, slots, nil, true)
	if err != nil {
		t.Fatalf("reachableKey: %v", err)
	}
	if len(single) != 1 {
		t.Fatalf("single-speaker keys = %d, want 1", len(single))
	}
}
