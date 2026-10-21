package voices

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
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

// CharacterSlug renders a character's roster name (the voice's character
// column, verbatim from the roster — natural names like "Killer Bees", not
// ULIDs) as the character field of a voice name (tts.MakeName's grammar:
// [a-z0-9][a-z0-9_-]{0,31}).
//
// The slug is slugified (issue #29): lowercase, every run of characters
// outside [a-z0-9] folded to a single '-', leading/trailing '-' trimmed, and
// capped at 32 chars by stable prefix. Before this rule the slug was the raw
// name lowercased, so any name with a space (or any character outside
// [a-z0-9_-]) failed the registry grammar at approval and the revision was
// left unapprovable — "Killer Bees" is a valid roster row and must yield a
// valid slug.
//
// Uniqueness within a profile: two DIFFERENT roster names must never fold to
// the same slug, or two characters could name the same voice at the same
// revision and reference content ("Killer Bees" vs "Killer-Bees" would both
// fold to "killer-bees"). A name that is already its own slug (canonical:
// lowercase, [a-z0-9-] only, no edge dashes, at most 32 chars) is returned
// unchanged — that is the byte-for-byte form every pre-existing stored name
// was built from — while a name that needed folding gets the 8-hex identity
// of its OWN raw string appended ("Killer Bees" -> "killer-bees-<d8>").
// Distinct raw strings therefore fold to distinct slugs (SHA-256 aside), and
// the name-to-slug mapping is injective within a profile's roster. A name
// that carries no ASCII alphanumeric at all ("…", "???") has no base to
// fold, so it gets the deterministic fallback "char-" + the same identity:
// stable per name, distinct per name, and valid under the grammar.
//
// The identity framing is versioned the way Fingerprint's is (ADR-0023's
// key-space rule): a future change to the fold rule is a disjoint slug space
// instead of a collision.
func CharacterSlug(characterID string) string {
	base := foldCharacterSlug(characterID)
	if base == "" {
		return characterSlugFallback + "-" + characterSlugDigest8(characterID)
	}
	if base == characterID {
		return base // canonical: the stored form, byte-for-byte
	}
	base = strings.TrimRight(base[:min(len(base), characterSlugCapBase)], "-")
	return base + "-" + characterSlugDigest8(characterID)
}

// foldCharacterSlug is the pure fold: lowercase, runs of non-[a-z0-9] to a
// single '-', edge dashes trimmed, capped at the grammar's 32 chars by stable
// prefix. The output is [a-z0-9-]* (ASCII only: the fold has already mapped
// every non-ASCII character away), so byte length is the rune length.
func foldCharacterSlug(name string) string {
	s := slugNonAlnumRe.ReplaceAllString(strings.ToLower(name), "-")
	s = strings.Trim(s, "-")
	if len(s) > characterSlugCap {
		s = strings.Trim(s[:characterSlugCap], "-")
	}
	return s
}

// characterSlugDigest8 is the 8-hex identity appended for a name that needed
// folding (or carried by the fallback): the first 8 hex chars of SHA-256
// over the versioned framing of the RAW roster name, so two spellings of the
// same folded base ("Killer Bees", "Killer-Bees", "KILLER-BEES") keep
// distinct identities exactly as the roster keeps them (roster names are
// case-sensitive; CastProfile.HasCharacter matches verbatim).
func characterSlugDigest8(characterID string) string {
	sum := sha256.Sum256([]byte(characterSlugFraming + characterID))
	return hex.EncodeToString(sum[:])[:8]
}

var slugNonAlnumRe = regexp.MustCompile(`[^a-z0-9]+`)

const (
	characterSlugCap      = 32 // tts grammar: [a-z0-9][a-z0-9_-]{0,31}
	characterSlugCapBase  = 23 // 32 - len("-") - 8: room for the digest suffix
	characterSlugFallback = "char"
	characterSlugFraming  = "gn-voice-character-slug/1\n"
)

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
