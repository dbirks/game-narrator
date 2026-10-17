package speech

import (
	"strings"
	"testing"
)

func testKeyInput() KeyInput {
	seed := 42
	return KeyInput{
		UtteranceText:            "Zelda is waiting at the shrine.",
		OriginalMessageID:        "msg-0042",
		SpeechRulesRevision:      7,
		Language:                 "en",
		VoiceRevisionFingerprint: "sha256:abcd",
		TTSModelID:               "qwen3-tts-base@2026.10",
		BackendConfigDigest:      "digest-1",
		StyleVariant:             "warm",
		Seed:                     &seed,
		Format:                   "wav/pcm16",
	}
}

// TestCacheKeyStable: same input → same key, always (durability contract).
func TestCacheKeyStable(t *testing.T) {
	in := testKeyInput()
	a, err := CacheKeyOf(in)
	if err != nil {
		t.Fatalf("CacheKeyOf: %v", err)
	}
	b, err := CacheKeyOf(in)
	if err != nil {
		t.Fatalf("CacheKeyOf: %v", err)
	}
	if a != b {
		t.Fatalf("key not stable: %s != %s", a, b)
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if len(a) != 64 {
		t.Fatalf("bad key shape: %q", a)
	}
}

// TestCacheKeyFieldSensitivity: changing ANY single field changes the key
// (docs/spec/07 "Cache" + docs/spec/01 invariant 4). This is the no-collision
// contract: a voice revision bump, a seed, a rules revision, or a format
// change must never hit the old entry.
func TestCacheKeyFieldSensitivity(t *testing.T) {
	base := testKeyInput()
	baseKey, err := CacheKeyOf(base)
	if err != nil {
		t.Fatalf("base key: %v", err)
	}

	s32 := 32
	cases := []struct {
		name string
		mut  func(*KeyInput)
	}{
		{"utterance text", func(k *KeyInput) { k.UtteranceText = "Link waits at the shrine." }},
		{"utterance text whitespace only", func(k *KeyInput) { k.UtteranceText = " Zelda is waiting at the shrine." }},
		{"original message id", func(k *KeyInput) { k.OriginalMessageID = "msg-0043" }},
		{"speech rules revision", func(k *KeyInput) { k.SpeechRulesRevision = 8 }},
		{"language", func(k *KeyInput) { k.Language = "ja" }},
		{"voice revision fingerprint", func(k *KeyInput) { k.VoiceRevisionFingerprint = "sha256:beef" }},
		{"tts model id+version", func(k *KeyInput) { k.TTSModelID = "qwen3-tts-base@2026.11" }},
		{"backend config digest", func(k *KeyInput) { k.BackendConfigDigest = "digest-2" }},
		{"style variant", func(k *KeyInput) { k.StyleVariant = "neutral" }},
		{"seed 42 → 32", func(k *KeyInput) { k.Seed = &s32 }},
		{"seed set → nil", func(k *KeyInput) { k.Seed = nil }},
		{"seed nil → 0", func(k *KeyInput) { k.Seed = intPtr(0) }},
		{"format", func(k *KeyInput) { k.Format = "wav/pcm32" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mut := base
			tc.mut(&mut)
			got, err := CacheKeyOf(mut)
			if err != nil {
				t.Fatalf("mutated key: %v", err)
			}
			if got == baseKey {
				t.Fatalf("changing %q did not change the key (%s): collision across cache identity fields", tc.name, baseKey)
			}
		})
	}
}

// TestCacheKeySeedNilDistinctFromZero: seed absence and seed 0 are different
// identities (docs/spec/07: seed is reproducibility metadata; nil = "not sent").
func TestCacheKeySeedNilDistinctFromZero(t *testing.T) {
	in := testKeyInput()
	in.Seed = nil
	nilKey, err := CacheKeyOf(in)
	if err != nil {
		t.Fatalf("nil seed key: %v", err)
	}
	in.Seed = intPtr(0)
	zeroKey, err := CacheKeyOf(in)
	if err != nil {
		t.Fatalf("zero seed key: %v", err)
	}
	if nilKey == zeroKey {
		t.Fatalf("seed=nil and seed=0 produced the same key %s", nilKey)
	}
}

// TestCacheKeyRequiresText: an empty utterance is a programming error, not a
// key space entry (a cache key for "speak nothing" would be a hazard).
func TestCacheKeyRequiresText(t *testing.T) {
	in := testKeyInput()
	in.UtteranceText = ""
	if _, err := CacheKeyOf(in); err == nil {
		t.Fatal("empty utterance_text must not produce a key")
	}
}

// TestCacheKeyValidate rejects malformed keys before they hit the store.
func TestCacheKeyValidate(t *testing.T) {
	for _, bad := range []string{"", "zz", strings.Repeat("g", 64), "ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF01234567"} {
		if err := CacheKey(bad).Validate(); err == nil {
			t.Errorf("Validate(%q) should fail", bad)
		}
	}
}

func intPtr(v int) *int { return &v }
