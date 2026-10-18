// Package speech implements the audio side of the runtime (docs/spec/07
// "Voice lifecycle, TTS caching, ASR quality and audio output"): an immutable
// cache key plus a singleflight, priority-bounded TTS cache in front of the
// asset store, an AudioSink abstraction (PipeWire subprocess, WAV file, dummy),
// and a generation-fenced playback scheduler that only ever plays the
// currently committed generation (docs/spec/01 "Parallelism and deadline
// contracts": only currently committed generation can play; the audio sink
// must never block the session event loop).
package speech

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// KeyInput is one cache identity. Every field comes from docs/spec/07 "Cache"
// and docs/spec/01 "Key invariants" 4 (cache identity). The identity includes
// the normalized TTS text, the message it came from, the rules and voice
// revisions that shaped it, the exact engine and backend settings, the style
// variant, the seed (where supported), and the output format.
//
// Sampling parameters (temperature, top-p, ...) are intentionally NOT a
// separate field: they are backend synthesis settings and therefore covered
// by BackendConfigDigest (ADR-0023). A backend that changes sampling without
// changing the digest is a bug in the digest owner, not a key hole.
type KeyInput struct {
	// UtteranceText is the canonical, TTS-normalized speech text (non-speech
	// tokens removed, button icons mapped, substitutions applied).
	UtteranceText string
	// OriginalMessageID identifies the source dialogue message (stable across
	// repeated OCR observations of the same occurrence).
	OriginalMessageID string
	// SpeechRulesRevision is the revision of the speech rendering rules
	// (segmentation, icon mapping, locale handling) used to derive
	// UtteranceText from the message.
	SpeechRulesRevision int64
	// Language is the spoken language code (e.g. "en").
	Language string
	// VoiceRevisionFingerprint identifies the approved, immutable
	// VoiceProfileRevision (fingerprint of the reference + identity), never a
	// mutable "voice name" (docs/spec/07 "Voice lifecycle" step 5).
	VoiceRevisionFingerprint string
	// TTSModelID is "model+version" exactly as the inference gateway routes on
	// it (e.g. "qwen3-tts-base@2026.10").
	TTSModelID string
	// BackendConfigDigest is a stable digest of the synthesis backend
	// configuration: endpoint policy, sampling parameters, voice-registry
	// reconciliation mode, output settings. Changing any of those must change
	// this digest (ADR-0023).
	BackendConfigDigest string
	// StyleVariant is the optional pre-approved delivery style
	// (docs/spec/07 "Identity vs emotion"); empty means neutral delivery.
	StyleVariant string
	// Seed is the stochastic generation seed when the backend honors one.
	// Nil means "no seed sent" and is distinct from seed 0 (docs/spec/07:
	// seed does not define a voice, but it is part of reproducibility).
	Seed *int
	// Format is the output format the cache entry is stored in (e.g. "wav/pcm16").
	Format string
}

// CacheKey is the 64-lowercase-hex SHA-256 of the canonical JSON encoding of
// in. Canonical means: fixed key set, all fields present (zero values
// included, Seed nil distinct from 0), map-key ordering (encoding/json sorts
// map keys), no whitespace. The same KeyInput therefore always yields the
// same key on every host and Go version, which is what makes the cache
// durable across restarts and shareable in manifests (docs/spec/07 "Export/
// import voice cache manifests").
type CacheKey string

// canonicalKeyFields is the exact key set. The order is irrelevant (sorted at
// marshal time); the SET is part of the key contract — a new field is a new
// key space and must be introduced with a new key version, not silently
// appended (ADR-0023, consequences).
var canonicalKeyFields = []string{
	"backend_config_digest",
	"format",
	"language",
	"original_message_id",
	"seed",
	"speech_rules_revision",
	"style_variant",
	"tts_model_id",
	"utterance_text",
	"voice_revision_fingerprint",
}

// keyVersion guards the whole key space. It is a field in the canonical JSON
// so that adding or removing a field in the future produces a disjoint key
// space instead of colliding with an old key by chance.
const keyVersion = 1

// KeyVersion reports the version field of the canonical key JSON. It guards
// the key space: a new field set is a new key version, never a silent append
// (ADR-0023). The store-backed index persists it (migration 0011's
// key_version column) so a future key-space change can partition the rows
// without re-deriving the version from the hash.
func KeyVersion() int { return keyVersion }

// keyJSON is the exact shape hashed. Every field of KeyInput appears exactly
// once; nothing else appears.
type keyJSON struct {
	BackendConfigDigest      string `json:"backend_config_digest"`
	Format                   string `json:"format"`
	Language                 string `json:"language"`
	OriginalMessageID        string `json:"original_message_id"`
	Seed                     *int   `json:"seed"`
	SpeechRulesRevision      int64  `json:"speech_rules_revision"`
	StyleVariant             string `json:"style_variant"`
	TTSModelID               string `json:"tts_model_id"`
	UtteranceText            string `json:"utterance_text"`
	VoiceRevisionFingerprint string `json:"voice_revision_fingerprint"`
	Version                  int    `json:"version"`
}

// canonicalJSON marshals in to the canonical byte form: fixed field set,
// fixed (sorted) key order, no insignificant variation.
func canonicalJSON(in KeyInput) ([]byte, error) {
	b, err := json.Marshal(keyJSON{
		BackendConfigDigest:      in.BackendConfigDigest,
		Format:                   in.Format,
		Language:                 in.Language,
		OriginalMessageID:        in.OriginalMessageID,
		Seed:                     in.Seed,
		SpeechRulesRevision:      in.SpeechRulesRevision,
		StyleVariant:             in.StyleVariant,
		TTSModelID:               in.TTSModelID,
		UtteranceText:            in.UtteranceText,
		VoiceRevisionFingerprint: in.VoiceRevisionFingerprint,
		Version:                  keyVersion,
	})
	if err != nil { // json.Marshal of this struct cannot fail; keep the guard.
		return nil, fmt.Errorf("speech: marshal canonical key: %w", err)
	}
	return b, nil
}

// CacheKeyOf computes the cache key for in.
func CacheKeyOf(in KeyInput) (CacheKey, error) {
	if len(in.UtteranceText) == 0 {
		return "", fmt.Errorf("speech: cache key requires non-empty utterance_text")
	}
	b, err := canonicalJSON(in)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return CacheKey(hex.EncodeToString(sum[:])), nil
}

// Validate reports whether k is a well-formed cache key (64 lowercase hex
// chars). Callers use it before persisting a key into SQLite or a manifest,
// so a bad key never becomes a path or a query parameter.
func (k CacheKey) Validate() error {
	if len(k) != 64 {
		return fmt.Errorf("speech: cache key length %d, want %d", len(k), 64)
	}
	_, err := hex.DecodeString(string(k))
	if err != nil {
		return fmt.Errorf("speech: cache key is not lowercase hex: %w", err)
	}
	return nil
}
