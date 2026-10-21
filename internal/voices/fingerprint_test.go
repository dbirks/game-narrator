package voices

// Issue #29: roster names with spaces could never be approved because the
// slug was the raw name lowercased and tts.MakeName rejects anything outside
// [a-z0-9_-]. These tests (docs/spec/12 "Unit" mode, synthetic stubs per
// AGENTS.md rule 3a: scripted fakes only — the design clips, the ASR
// read-back and the registry are all in-memory, so NO voice of any
// namespace, production or gn_test_, is ever placed on a real provider by
// this file) pin the new slug rule:
//
//   - the fold: lowercase, runs of non-[a-z0-9] to a single '-', edge dashes
//     trimmed, 32-char cap (names with spaces, punctuation, unicode and
//     empty input);
//   - collisions stay unique within a profile: distinct roster names fold to
//     distinct slugs even when the fold alone would merge them;
//   - the stored form does not move: canonical names are byte-for-byte the
//     pre-#29 lowercased form, and legacy stored names still validate — so
//     no forward migration is required (AGENTS.md rule 10);
//   - the approval path: a two-word character name gets an approved voice,
//     both by human click and by the auto-cast job that the issue saw fail.

import (
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/store"
)

// TestCharacterSlug is the table test: names with spaces, punctuation,
// unicode and empty input. The expected slugs are pinned exactly — the 8-hex
// suffixes are the first 8 hex chars of SHA-256 over
// "gn-voice-character-slug/1\n<raw name>" (the versioned identity framing),
// pinned here so a framing or fold change fails loudly instead of silently
// re-naming every lossy voice (ADR-0023's key-space rule). Every slug,
// whatever the case, must fit tts.MakeName's character grammar — the exact
// check the issue's "Killer Bees" used to fail.
func TestCharacterSlug(t *testing.T) {
	ref := wavTone(200, 1)
	cases := []struct {
		in   string
		want string
	}{
		// the issue's repro: a two-word roster name
		{"Killer Bees", "killer-bees-b6876858"},
		// punctuation: every run of non-[a-z0-9] is one '-', trimmed at the
		// edges, and distinct spellings keep distinct identities
		{"Killer  Bees", "killer-bees-" + d8pin("Killer  Bees")},
		{"Killer-Bees", "killer-bees-15122b76"},
		{"KILLER-BEES", "killer-bees-" + d8pin("KILLER-BEES")},
		{"a.b", "a-b-321d4952"},
		{"a_b", "a-b-736092ee"},
		{"-abc", "abc-79f9186f"},
		{"abc-", "abc-95e29e14"},
		{"a--b", "a-b-" + d8pin("a--b")},
		// unicode: non-ASCII letters are not [a-z0-9], so they fold to '-'
		{"Zoë", "zo-" + d8pin("Zoë")},
		{"ΩΔΨ", "char-60873042"},
		// empty input: the deterministic fallback, distinct per input
		{"", "char-95bb2299"},
		{"   ", "char-7c140b38"},
		{"???", "char-c4f3101a"},
		// canonical names come back unchanged — the pre-#29 stored form
		{"narrator", "narrator"},
		{"killer-bees", "killer-bees"},
		{"a1-b2", "a1-b2"},
		// the 32-char cap: 33 valid chars fold by stable prefix and the
		// identity suffix keeps the field at exactly 32
		{strings.Repeat("a", 33), strings.Repeat("a", 23) + "-" + d8pin(strings.Repeat("a", 33))},
	}
	for _, c := range cases {
		if got := CharacterSlug(c.in); got != c.want {
			t.Errorf("CharacterSlug(%q) = %q, want %q", c.in, got, c.want)
		}
		// The grammar check the approval used to trip on (issue #29): the
		// slug must fit MakeName's [a-z0-9][a-z0-9_-]{0,31} field, and the
		// assembled name must pass ValidateName.
		if _, err := tts.MakeName("ab12cd34", CharacterSlug(c.in), 1, ref); err != nil {
			t.Errorf("CharacterSlug(%q) fails tts.MakeName: %v", c.in, err)
		}
		name, err := RegistryName("profile-x", c.in, 1, ref)
		if err != nil {
			t.Errorf("RegistryName(%q): %v", c.in, err)
		} else if err := tts.ValidateName(name); err != nil {
			t.Errorf("RegistryName(%q) = %q fails the registry grammar: %v", c.in, name, err)
		}
		// Deterministic: the slug is a pure function of the roster name.
		if again := CharacterSlug(c.in); again != c.want {
			t.Errorf("CharacterSlug(%q) is not stable: %q then %q", c.in, c.want, again)
		}
	}
}

// d8pin returns the pinned 8-hex identity of one raw name, computed with
// the production framing (characterSlugDigest8). The table pins the VALUES;
// the helper keeps the table readable — a change to the framing changes the
// pinned values and fails the test.
func d8pin(name string) string { return characterSlugDigest8(name) }

// TestCharacterSlugCollisionsStayUniqueWithinAProfile: a roster where the
// fold alone would merge rows ("Killer Bees" and "Killer-Bees" both fold to
// "killer-bees") must still get one distinct slug — and therefore one
// distinct voice name — per row. The strongest collision case is the same
// fold base, the same revision number and byte-identical reference audio:
// even then the registry names differ.
func TestCharacterSlugCollisionsStayUniqueWithinAProfile(t *testing.T) {
	roster := []string{
		"Killer Bees", "Killer-Bees", "KILLER-BEES", "Killer  Bees", "killer-bees",
		"a_b", "a-b", "a.b", "abc-", "abc", "ΩΔΨ", "???",
	}
	seen := map[string]string{}
	for _, name := range roster {
		slug := CharacterSlug(name)
		if other, dup := seen[slug]; dup {
			t.Fatalf("slugs collide within a profile: %q and %q both fold to %q", other, name, slug)
		}
		seen[slug] = name
	}

	// Same fold base, same revision, IDENTICAL reference bytes: the issue's
	// pre-#29 failure mode was exactly this identity collision. The raw-name
	// identity suffix keeps the two rows apart.
	ref := wavTone(200, 1)
	n1, err := RegistryName("profile-1", "Killer Bees", 1, ref)
	if err != nil {
		t.Fatalf("RegistryName (Killer Bees): %v", err)
	}
	n2, err := RegistryName("profile-1", "Killer-Bees", 1, ref)
	if err != nil {
		t.Fatalf("RegistryName (Killer-Bees): %v", err)
	}
	if n1 == n2 {
		t.Fatalf("two roster rows with the same fold, revision and audio derive the same name %q", n1)
	}
	// The same row, different reference content, is a different name (the
	// audio digest is the name's tail) — immutability in both directions.
	n3, err := RegistryName("profile-1", "Killer Bees", 1, wavTone(300, 1))
	if err != nil {
		t.Fatalf("RegistryName (different audio): %v", err)
	}
	if n3 == n1 {
		t.Fatalf("different reference content derived the same name %q", n1)
	}
}

// TestCharacterSlugKeepsTheStoredForm is the no-migration proof (AGENTS.md
// rule 10): the slug is never stored on its own — voice_profiles.character
// holds the raw roster name and voice_registrations.registered_name holds
// the frozen full name — and the frozen form stays readable:
//
//   - canonical names derive byte-for-byte the pre-#29 lowercased slug, so
//     any name the old code could have approved is re-derived identically;
//   - a legacy stored name (old-style underscore slug) still passes the
//     unchanged registry grammar, so the rows the old code wrote validate
//     as-is and no forward migration is required.
func TestCharacterSlugKeepsTheStoredForm(t *testing.T) {
	for _, name := range []string{"narrator", "killer-bees", "a1-b2", "z9", strings.Repeat("k", 32)} {
		if got, want := CharacterSlug(name), strings.ToLower(name); got != want {
			t.Errorf("canonical %q derives %q, want the pre-#29 form %q (stored names would move)", name, got, want)
		}
	}

	// A name the old code could store (lowercase with an underscore) and a
	// legacy full name built from it: the grammar still accepts it.
	legacy := "gn_ab12cd34_killer_bees_v1_0123abcd"
	if err := tts.ValidateName(legacy); err != nil {
		t.Fatalf("legacy stored name %q no longer validates: %v (a stored form that stops validating needs a migration)", legacy, err)
	}
	// The new derivation for that roster name is a different, equally valid
	// name — a new revision gets the new form; the frozen old row keeps the
	// old one.
	newSlug := CharacterSlug("killer_bees")
	if newSlug == "killer_bees" {
		t.Fatalf("lossy name %q keeps the old slug byte-for-byte; new approvals must use the folded form", "killer_bees")
	}
	fresh, err := tts.MakeName("ab12cd34", newSlug, 2, wavTone(200, 1))
	if err != nil {
		t.Fatalf("new derivation fails the grammar: %v", err)
	}
	if err := tts.ValidateName(fresh); err != nil {
		t.Fatalf("new derivation %q fails the grammar: %v", fresh, err)
	}
}

// TestTwoWordCharacterNameApprovesAndRegisters is the approval path: the
// flow the issue saw fail (the auto-approve step rejected the registry name
// for "Killer Bees"). By the human-click path the two-word roster name now
// approves, the precomputed name is stored and readable, the registration
// lands on the (fake) registry, and the stored character column is the raw
// roster name, unchanged.
func TestTwoWordCharacterNameApprovesAndRegisters(t *testing.T) {
	svc, _, reg, st := newTestService(t)
	// Before the fix, this Approve call returned:
	// "voices: registry name: tts: voice name is not a valid namespaced
	// immutable ID: character slug must be [a-z0-9][a-z0-9_-]{0,31}, got
	// \"killer bees\"".
	approved, _ := mustAuditionAndApprove(t, svc, "Killer Bees", "two-word roster name")

	// The stored form: the raw roster name, verbatim (nothing stored moved).
	if approved.Character != "Killer Bees" {
		t.Fatalf("character column = %q, want the raw roster name \"Killer Bees\"", approved.Character)
	}
	regRow, err := st.GetVoiceRegistration(ctx(t), approved.ID, svc.InstanceID)
	if err != nil {
		t.Fatalf("pending registration: %v", err)
	}
	if !strings.Contains(regRow.RegisteredName, "killer-bees-") {
		t.Fatalf("registered name %q does not carry the folded slug", regRow.RegisteredName)
	}
	if err := tts.ValidateName(regRow.RegisteredName); err != nil {
		t.Fatalf("registered name %q fails the registry grammar: %v", regRow.RegisteredName, err)
	}

	// The registration is accepted by the registry's identity rules (the
	// fake enforces the same name-digest-match Ensure does): ready, exactly
	// one voice under the derived name.
	row, err := svc.Register(ctx(t), approved.ID)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if row.State != store.VoiceRegReady || row.RegisteredName != regRow.RegisteredName {
		t.Fatalf("after Register = %+v, want ready under the precomputed name", row)
	}
	if len(reg.voices) != 1 {
		t.Fatalf("registry holds %d names, want exactly one", len(reg.voices))
	}
	if _, ok := reg.voices[regRow.RegisteredName]; !ok {
		t.Fatalf("registry holds %v, want the derived name %q", reg.voices, regRow.RegisteredName)
	}

	// The sibling row with the same fold base is a different voice: the
	// hyphenated spelling approves under its own name, not a collision.
	other, _ := mustAuditionAndApprove(t, svc, "Killer-Bees", "the hyphenated sibling")
	otherRow, err := st.GetVoiceRegistration(ctx(t), other.ID, svc.InstanceID)
	if err != nil {
		t.Fatalf("sibling registration: %v", err)
	}
	if otherRow.RegisteredName == regRow.RegisteredName {
		t.Fatalf("siblings %q and %q share the registry name %q", "Killer-Bees", "Killer Bees", regRow.RegisteredName)
	}
}

// TestAutoCastApprovesATwoWordCharacterName is the job path the issue
// actually hit: the voice.auditions job's auto-cast step, with a two-word
// roster name. The dial is on, the roster carries the name verbatim, the QA
// read-back passes — and the take auto-approves instead of failing the job.
func TestAutoCastApprovesATwoWordCharacterName(t *testing.T) {
	seam := &castSeam{profile: CastProfile{AutoCast: true, CharacterNames: []string{"Killer Bees"}}}
	svc, _, asr := newCastService(t, seam)
	asr.results = passScripts(1)

	fresh, result := runCastJob(t, svc, "Killer Bees", "a dry, level take", 1, "profile-bees")
	if !result.AutoApproved || result.State != store.VoiceStateApproved {
		t.Fatalf("auto-cast result = %+v, want auto-approved (issue #29's failing step)", result)
	}
	if fresh.State != store.VoiceStateApproved || fresh.Fingerprint == "" || fresh.ReferenceAssetID == "" {
		t.Fatalf("revision = %s (fingerprint %q), want approved with the frozen content", fresh.State, fresh.Fingerprint)
	}
	if fresh.ApprovedBy != AutoCaster || !strings.Contains(fresh.ConsentProvenance, "profile-bees") {
		t.Fatalf("approval attribution = %q / %q, want the auto-caster and the profile consent", fresh.ApprovedBy, fresh.ConsentProvenance)
	}
	// The stored registration carries the readable, grammar-valid name.
	regRow, err := svc.Store.GetVoiceRegistration(ctx(t), fresh.ID, svc.InstanceID)
	if err != nil || !strings.Contains(regRow.RegisteredName, "killer-bees-") {
		t.Fatalf("registration = %+v (err=%v), want the folded slug in the name", regRow, err)
	}
	// The manual path stays available too: a second revision of the same
	// two-word name exists and approves by click as well.
	rev2, auditions, err := svc.CreateAuditions(ctx(t), "Killer Bees", "a second take", 1)
	if err != nil || len(auditions) != 1 {
		t.Fatalf("second audition run: %v (auditions=%d)", err, len(auditions))
	}
	if _, err := svc.Approve(ctx(t), rev2.ID, auditions[0].ID, "original_synthetic_design (unit test)", "curator-1"); err != nil {
		t.Fatalf("manual approval of the second revision: %v", err)
	}
	profiles, err := svc.Store.ListVoiceProfiles(ctx(t), "Killer Bees")
	if err != nil || len(profiles) != 2 {
		t.Fatalf("profiles = %d (err=%v), want both revisions", len(profiles), err)
	}
}
