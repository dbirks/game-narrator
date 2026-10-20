package profileio

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/store"
)

// The voice-portability tests for issue #11: a profile export must carry the
// approved voices, the speaker links, and the narrator fallback so that a
// re-import into a fresh database still narrates. These drive the real
// export→import path against synthetic content; no real game text and no
// real voice samples appear (AGENTS.md rule 5).

// seedVoiceRef commits one blob under the given content address so an import
// that names it finds the bytes on the appliance. It returns the sha.
func seedVoiceRef(t *testing.T, e *env, payload string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(payload))
	ref := hex.EncodeToString(sum[:])
	id, _, err := e.assets.PutStaged(e.ctx, bytes.NewReader([]byte(payload)), ref)
	if err != nil {
		t.Fatalf("staging voice ref: %v", err)
	}
	if _, err := e.assets.Commit(e.ctx, id); err != nil {
		t.Fatalf("committing voice ref: %v", err)
	}
	return ref
}

// seedApprovedVoice gives one character (or the narrator constant) a single
// approved, referenced voice revision and returns its stable id and the
// reference blob's content address.
func seedApprovedVoice(t *testing.T, e *env, character, refPayload string) (voiceID, refSHA string) {
	t.Helper()
	refSHA = seedVoiceRef(t, e, refPayload)
	voiceID = games.NewID()
	now := time.Now().UTC()
	fingerprint := sha256.Sum256([]byte(refSHA + "|" + character + "|fake-base"))
	if err := e.store.CreateVoiceRevision(e.ctx, store.VoiceProfile{
		ID:                voiceID,
		Character:         character,
		Revision:          1,
		State:             store.VoiceStateApproved,
		DesignInstruction: "a calm synthetic voice",
		ReferenceAssetID:  refSHA,
		RefTranscript:     "Keep the log dry and the wick trimmed.",
		ModelID:           "fake-base",
		EmbeddingMode:     "registered",
		Language:          "en",
		ConsentProvenance: "original_synthetic_design (automated test)",
		ApprovedBy:        "test",
		Fingerprint:       hex.EncodeToString(fingerprint[:]),
		Gain:              1,
		CreatedAt:         now,
		UpdatedAt:         now,
	}); err != nil {
		t.Fatalf("creating approved voice for %s: %v", character, err)
	}
	return voiceID, refSHA
}

// TestVoiceManifestRoundTrip is the issue #11 acceptance line: build a profile
// on the PC (a roster with a speaker link, an approved character voice, an
// approved narrator voice, and the narrator fallback on), export the manifest,
// and re-import it into a fresh database. The voices must land with their
// stable ids, the speaker link must survive, and the narrator fallback must
// be applied — so the fresh appliance can still narrate the committed line.
func TestVoiceManifestRoundTrip(t *testing.T) {
	t.Parallel()

	// ── PC: build the profile ───────────────────────────────────────────────
	pc := newEnv(t)
	g := pc.game("voice-roundtrip", "Voice Round Trip")
	edition := pc.edition(g, "US", "en")
	first := pc.draft(edition.ID)

	report, err := ImportFile(pc.ctx, pc.deps, csvRequest(t, pc, first.ID))
	if err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	if report.Stats.Imported == 0 {
		t.Fatalf("nothing imported")
	}
	content := mustContent(t, pc, first.ID)
	if len(content.Messages) == 0 {
		t.Fatalf("no messages to wire")
	}
	if len(content.Sources) == 0 {
		t.Fatalf("no sources")
	}
	now := nowStamp()

	// The target character carries the INF1 speaker key (ww:spk:1) folded into
	// its aliases — the curator's speaker link (ADR-0070): a line whose speaker
	// is the placeholder (whose name is that key) resolves to this character.
	target := games.Character{
		ID: games.NewID(), ProfileID: first.ID, Name: "Captain Vayne",
		Aliases: json.RawMessage(`["Vayne","ww:spk:1"]`),
		Role:    "captain", Status: games.CharacterConfirmed,
		VerifiedSourceRefs: json.RawMessage(`["` + content.Sources[0].ID + `"]`),
		CreatedAt:          now,
	}
	// The placeholder row the import created for the opaque speaker key.
	placeholder := games.Character{
		ID: games.NewID(), ProfileID: first.ID, Name: "ww:spk:1",
		Aliases: json.RawMessage(`[]`), Role: "crew",
		Status: games.CharacterProposed, CreatedAt: now,
	}
	for _, ch := range []games.Character{target, placeholder} {
		if err := ch.Validate(); err != nil {
			t.Fatalf("character fixture invalid: %v", err)
		}
	}
	if err := pc.store.PutCharacters(pc.ctx, first.ID, []games.Character{target, placeholder}); err != nil {
		t.Fatalf("PutCharacters: %v", err)
	}

	// A scene and a region so the profile is exportable and runnable.
	scene := games.Scene{ID: games.NewID(), ProfileID: first.ID, Name: "The Bridge", Aliases: json.RawMessage(`[]`), CreatedAt: now}
	if err := pc.store.PutScenes(pc.ctx, first.ID, []games.Scene{scene}); err != nil {
		t.Fatalf("PutScenes: %v", err)
	}
	region := games.Region{
		ID: games.NewID(), ProfileID: first.ID, SceneID: scene.ID, Label: "textbox",
		RegionType: games.RegionTextbox, DetectMethod: games.DetectManual,
		RectX: 0.07, RectY: 0.72, RectW: 0.86, RectH: 0.23,
		OCRSettings: json.RawMessage(`{"engine":"tesseract","psm":6,"language":"eng"}`),
		Revision:    1, UpdatedAt: now,
	}
	if err := region.Validate(); err != nil {
		t.Fatalf("region fixture invalid: %v", err)
	}
	if err := pc.store.PutRegions(pc.ctx, first.ID, []games.Region{region}); err != nil {
		t.Fatalf("PutRegions: %v", err)
	}

	// Wire the first message to the placeholder: the speaker-linked line.
	m := content.Messages[0]
	m.SpeakerID, m.SceneID = placeholder.ID, scene.ID
	m.AttributionStatus, m.AttributionConfidence = games.AttributionConfirmed, 0.9
	m.AttributionEvidence = json.RawMessage(`[{"method":"manual","note":"speaker link"}]`)
	if err := pc.store.PutMessages(pc.ctx, first.ID, []games.Message{m}, nil); err != nil {
		t.Fatalf("PutMessages: %v", err)
	}

	// The two approved voices: the target's voice and the narrator's.
	targetVoiceID, targetRef := seedApprovedVoice(t, pc, target.ID, "target reference wav")
	narratorVoiceID, narratorRef := seedApprovedVoice(t, pc, "narrator", "narrator reference wav")

	// The draft's narrator fallback is on by default (the column's DEFAULT 1).
	prof, _, err := pc.store.GetProfile(pc.ctx, first.ID)
	if err != nil {
		t.Fatalf("GetProfile: %v", err)
	}
	if !prof.NarratorFallback {
		t.Fatalf("draft narrator fallback = off, want the default on")
	}

	published := publish(t, pc, first.ID, "curator-a@local")

	// ── export ──────────────────────────────────────────────────────────────
	before := profileContentFor(t, pc, published.ID)
	file := mustManifestBytes(t, before)
	if err := ValidateDocument(file); err != nil {
		t.Fatalf("exported manifest does not satisfy the schema: %v", err)
	}
	if !bytes.Contains(file, []byte(targetVoiceID)) || !bytes.Contains(file, []byte(narratorVoiceID)) {
		t.Fatalf("the export does not carry the approved voice ids")
	}
	// The narrator fallback is on the wire.
	var wire struct {
		Publication struct {
			NarratorFallback *bool `json:"narrator_fallback"`
		} `json:"publication"`
	}
	if err := json.Unmarshal(file, &wire); err != nil {
		t.Fatalf("unmarshal export: %v", err)
	}
	if wire.Publication.NarratorFallback == nil || !*wire.Publication.NarratorFallback {
		t.Errorf("publication.narrator_fallback = %v, want true", wire.Publication.NarratorFallback)
	}

	// ── fresh DB: import with the voice blobs present ───────────────────────
	fresh := newIn(t)
	seedVoiceRef(t, fresh, "target reference wav")
	seedVoiceRef(t, fresh, "narrator reference wav")
	imported, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(file), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	if imported.VoicesWritten != 2 {
		t.Fatalf("voices written = %d, want 2 (target + narrator)", imported.VoicesWritten)
	}
	if len(imported.VoiceRefsMissing) != 0 {
		t.Errorf("voice_refs_missing = %v, want none (the blobs were seeded)", imported.VoiceRefsMissing)
	}

	// The voices land with their stable ids.
	tv, err := fresh.store.GetVoiceRevision(fresh.ctx, targetVoiceID)
	if err != nil {
		t.Fatalf("the target voice did not round-trip with its stable id: %v", err)
	}
	nv, err := fresh.store.GetVoiceRevision(fresh.ctx, narratorVoiceID)
	if err != nil {
		t.Fatalf("the narrator voice did not round-trip with its stable id: %v", err)
	}
	if nv.Character != "narrator" {
		t.Errorf("narrator voice character = %q, want the narrator constant", nv.Character)
	}
	// The target voice is remapped to the fresh target row, not the PC's id.
	freshTargetID := ""
	for _, ch := range mustContent(t, fresh, imported.ProfileID).Characters {
		if ch.Name == "Captain Vayne" {
			freshTargetID = ch.ID
		}
	}
	if tv.Character != freshTargetID {
		t.Errorf("target voice character = %s, want the fresh target row %s", tv.Character, freshTargetID)
	}

	// The speaker link survives: the fresh target carries the placeholder name
	// in its aliases, and the committed line still points at the (remapped)
	// placeholder.
	fcontent := mustContent(t, fresh, imported.ProfileID)
	targetHasAlias, placeholderExists := false, false
	var linkedLine string
	for _, ch := range fcontent.Characters {
		if ch.Name == "ww:spk:1" {
			placeholderExists = true
		}
		if ch.Name == "Captain Vayne" {
			var aliases []string
			_ = json.Unmarshal(ch.Aliases, &aliases)
			for _, a := range aliases {
				if a == "ww:spk:1" {
					targetHasAlias = true
				}
			}
		}
	}
	for _, msg := range fcontent.Messages {
		if msg.AttributionStatus == games.AttributionConfirmed && msg.SpeakerID != "" {
			linkedLine = msg.SpeakerID
		}
	}
	if !targetHasAlias {
		t.Errorf("the imported target lost the speaker-link alias ww:spk:1")
	}
	if !placeholderExists {
		t.Errorf("the imported profile is missing the ww:spk:1 placeholder row")
	}
	if linkedLine == "" {
		t.Errorf("the committed speaker-linked line lost its speaker on re-import")
	}

	// The narrator fallback is applied to the fresh draft.
	fprof, _, err := fresh.store.GetProfile(fresh.ctx, imported.ProfileID)
	if err != nil {
		t.Fatalf("fresh GetProfile: %v", err)
	}
	if !fprof.NarratorFallback {
		t.Errorf("the imported profile's narrator fallback = off, want on (the export carried it)")
	}

	// The digest round-trips: a re-export of what was imported fingerprints the
	// same content (the voices included).
	again := profileContentFor(t, fresh, imported.ProfileID)
	if again.Digest != before.Digest {
		t.Errorf("re-export digest = %s, want %s (the voices must round-trip as content)", again.Digest, before.Digest)
	}
	_ = targetRef
	_ = narratorRef
}

// TestVoiceManifestPointerOnlyImport is the pack's complement: a manifest that
// names voice reference blobs the importing appliance does not hold. The import
// must report them in voice_refs_missing and write no voice row — it must not
// fail, because the profile content (the point of the import) still lands and
// the narrator fallback can still be served by whatever voice the appliance
// already has.
func TestVoiceManifestPointerOnlyImport(t *testing.T) {
	t.Parallel()
	pc := newEnv(t)
	g := pc.game("voice-pointer", "Voice Pointer")
	first := pc.draft(pc.edition(g, "US", "en").ID)
	if _, err := ImportFile(pc.ctx, pc.deps, csvRequest(t, pc, first.ID)); err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	content := mustContent(t, pc, first.ID)
	targetVoiceID, _ := seedApprovedVoice(t, pc, content.Characters[0].ID, "pc only reference wav")
	published := publish(t, pc, first.ID, "curator-a@local")
	file := mustManifestBytes(t, profileContentFor(t, pc, published.ID))

	// The fresh appliance does not seed the blob.
	fresh := newIn(t)
	imported, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(file), ImportManifestOptions{})
	if err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	if imported.VoicesWritten != 0 {
		t.Errorf("voices written = %d, want 0 (the reference blob is absent)", imported.VoicesWritten)
	}
	if len(imported.VoiceRefsMissing) == 0 {
		t.Fatalf("voice_refs_missing is empty, want the absent reference reported")
	}
	if _, err := fresh.store.GetVoiceRevision(fresh.ctx, targetVoiceID); err == nil {
		t.Errorf("a voice row was written although its reference blob is absent")
	}
}

// TestVoiceManifestFingerprintConflict is the tamper guard: a manifest whose
// voice id the store already holds under a different fingerprint is refused —
// one id, two contents — and the whole import fails, because a manifest that
// lies about one row forfeits the trust every other row rode on.
func TestVoiceManifestFingerprintConflict(t *testing.T) {
	t.Parallel()
	pc := newEnv(t)
	g := pc.game("voice-conflict", "Voice Conflict")
	first := pc.draft(pc.edition(g, "US", "en").ID)
	if _, err := ImportFile(pc.ctx, pc.deps, csvRequest(t, pc, first.ID)); err != nil {
		t.Fatalf("ImportFile: %v", err)
	}
	content := mustContent(t, pc, first.ID)
	voiceID, _ := seedApprovedVoice(t, pc, content.Characters[0].ID, "original reference wav")
	published := publish(t, pc, first.ID, "curator-a@local")
	file := mustManifestBytes(t, profileContentFor(t, pc, published.ID))

	// Tamper the wire document the honest way: decode it, claim a different
	// fingerprint for the held id, and re-stamp so the digest stays valid —
	// the document is internally consistent, which is what makes the conflict
	// (not a digest or schema refusal) the finding that bites.
	m, err := DecodeManifest(bytes.NewReader(file))
	if err != nil {
		t.Fatalf("DecodeManifest: %v", err)
	}
	mutated := false
	for i := range m.Voices {
		if m.Voices[i].ID == voiceID {
			m.Voices[i].Fingerprint = strings.Repeat("f", 64)
			mutated = true
		}
	}
	if !mutated {
		t.Fatalf("could not find the voice %s in the exported manifest", voiceID)
	}
	Stamp(&m)
	mut := mustManifestBytes(t, m)

	// Import into a store that already holds the id (re-import the uncorrupted
	// manifest first, then the tampered one into the same appliance).
	fresh := newIn(t)
	seedVoiceRef(t, fresh, "original reference wav")
	if _, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(file), ImportManifestOptions{}); err != nil {
		t.Fatalf("first ImportManifest: %v", err)
	}
	if _, err := ImportManifest(fresh.ctx, fresh.deps, bytes.NewReader(mut), ImportManifestOptions{Force: true}); err == nil ||
		!strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("a manifest claiming a different fingerprint for a held id: err = %v, want the fingerprint conflict named", err)
	}
}
