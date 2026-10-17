package voices

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/store"
)

// Fingerprint is the content identity of one approved revision
// (docs/spec/07 "Voice lifecycle" step 5: the cache key carries the
// fingerprint of the approved, immutable revision — never a mutable voice
// name). It is the SHA-256 of a canonical framing of the three ingredients:
// the reference clip's own SHA-256 (its bytes are the identity, ADR-0005),
// the exact transcript, and the model that produced it. Length framing keeps
// the fields unambiguous; the version prefix makes a future formula change a
// disjoint key space instead of a collision (ADR-0023's key-space rule).
func Fingerprint(referenceSHA256, transcript, modelID string) string {
	var b strings.Builder
	b.WriteString("gn-voice-fingerprint/1\n")
	b.WriteString("reference=")
	b.WriteString(strings.ToLower(strings.TrimSpace(referenceSHA256)))
	b.WriteString("\n")
	b.WriteString("transcript=")
	b.WriteString(transcript)
	b.WriteString("\n")
	b.WriteString("model=")
	b.WriteString(modelID)
	b.WriteString("\n")
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// ProfileIdentity8 is the 8-hex identity a voice name embeds: the first 8
// hex chars of SHA-256(profile row id). ULIDs are Crockford base32, not hex,
// so the raw id cannot be sliced — a stable digest of it can (docs/spec/06:
// "never rely on friendly voice names"; namespaces on account/profile/
// revision digests).
func ProfileIdentity8(profileID string) string {
	sum := sha256.Sum256([]byte("gn-voice-profile/" + profileID))
	return hex.EncodeToString(sum[:])[:8]
}

// CharacterSlug renders a character id for a voice name: lowercase, so the
// ULID's base32 letters fit tts's name grammar ([a-z0-9_-]).
func CharacterSlug(characterID string) string { return strings.ToLower(characterID) }

// RegistryName derives the immutable namespaced voice name for an approved
// revision: gn_<profile8>_<character>_v<rev>_<digest8> (tts.MakeName). It is
// a pure function of the reference content: the digest8 suffix is the first 8
// hex chars of SHA-256 over the reference WAV bytes themselves, which is what
// tts.Registry.Ensure re-derives to accept a name (docs/spec/06: "never rely
// on friendly voice names"). Making it a function of the raw bytes — not of
// the stored SHA-256 — is what keeps Ensure's identity check honest, and it
// is what makes a re-registration after a provider restart land on the same
// name.
func RegistryName(profileID, character string, rev int, refAudio []byte) (string, error) {
	if len(refAudio) == 0 {
		return "", errors.New("voices: RegistryName requires the reference WAV bytes")
	}
	return tts.MakeName(ProfileIdentity8(profileID), CharacterSlug(character), rev, refAudio)
}

// previewCacheKey builds the speech cache key for one preview line of one
// revision (docs/spec/07 "Cache": the key is the fingerprint plus the text
// plus the backend identity). Every field comes from docs/spec/07 "Key
// invariants"; the preview's OriginalMessageID is the revision itself, so a
// preview never collides with a session's message-derived keys and two
// identical previews for one revision share one key — which is exactly the
// miss-then-hit behaviour docs/spec/07 step 7 requires.
func previewCacheKey(p store.VoiceProfile, text, backendDigest string) (speech.CacheKey, error) {
	key, err := speech.CacheKeyOf(speech.KeyInput{
		UtteranceText:            strings.TrimSpace(text),
		OriginalMessageID:        "voice-preview:" + p.ID,
		SpeechRulesRevision:      1,
		Language:                 p.Language,
		VoiceRevisionFingerprint: p.Fingerprint,
		TTSModelID:               p.ModelID,
		BackendConfigDigest:      backendDigest,
		Format:                   "wav/pcm16",
	})
	if err != nil {
		return "", fmt.Errorf("voices: preview cache key: %w", err)
	}
	return key, nil
}
