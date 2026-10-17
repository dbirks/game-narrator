package voices

// LIVE workflow test (docs/spec/16 M4: "real TTS tests when configured"):
// the full voice setup against the deployed checkpoints — three real
// VoiceDesign auditions, one approval with the real consent record,
// registration of a gn_test_ name on the direct Base registry, and the
// preview miss-then-hit contract through the speech cache. It SKIPs as
// "NOT RUN" unless the gateway and the direct registry URL are configured,
// so `go test ./...` stays green offline.
//
// Configuration (never printed):
//
//	GAME_NARRATOR_GATEWAY_URL              shared gateway (speech route)
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV        token holder (default GAME_NARRATOR_GATEWAY_TOKEN)
//	GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL  default qwen3-tts-voicedesign
//	GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL   default qwen3-tts-base
//	GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL
//	   or GAME_NARRATOR_TTS_BASE_DIRECT_URL  direct Base service (registry);
//	   the first set wins, the second is the test-harness spelling
//
// The registry cycle touches exactly one name of its own, gn_test_<random8>,
// and removes it via t.Cleanup. Log lines carry model ids, durations,
// latencies and the sanitised test name only — never a URL or a token
// (AGENTS.md rule 5).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/assets"
	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/speech"
)

const liveConsent = "original_synthetic_design (game-narrator automated test)"

// liveVoiceEnv reports the skip reason; ok is true only when the speech
// gateway and the direct registry URL are both configured.
func liveVoiceEnv(t *testing.T) (providers.RoleConfig, providers.RoleConfig, string, bool) {
	t.Helper()
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	designEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceDesign)
	cloneEnv, _, _, _ := providers.RoleEnvVars(providers.RoleVoiceClone)
	design, clone := roles[providers.RoleVoiceDesign], roles[providers.RoleVoiceClone]
	if m := strings.TrimSpace(os.Getenv(designEnv)); m != "" {
		design.Model = m
	} else {
		design.Model = "qwen3-tts-voicedesign"
	}
	if m := strings.TrimSpace(os.Getenv(cloneEnv)); m != "" {
		clone.Model = m
	} else {
		clone.Model = "qwen3-tts-base"
	}
	design.Timeout, clone.Timeout = 180*time.Second, 180*time.Second

	direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL"))
	if direct == "" {
		direct = strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	}
	if strings.TrimSpace(design.BaseURL) == "" || strings.TrimSpace(design.Token()) == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
			providers.EnvGatewayTokenEnv + " to run the live voice workflow test")
	}
	if direct == "" {
		t.Skip("NOT RUN: set GAME_NARRATOR_ROLE_VOICE_CLONE_REGISTRY_URL (or GAME_NARRATOR_TTS_BASE_DIRECT_URL) to the direct Base service (orchestrator port-forward) to run the live voice workflow test")
	}
	return design, clone, direct, true
}

func TestLiveVoiceWorkflow(t *testing.T) {
	designCfg, cloneCfg, directURL, ok := liveVoiceEnv(t)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	t.Cleanup(cancel)

	designClient, err := tts.NewFromRole(designCfg)
	if err != nil {
		t.Fatalf("design client: %v", err)
	}
	cloneClient, err := tts.NewFromRole(cloneCfg)
	if err != nil {
		t.Fatalf("clone client: %v", err)
	}
	registry, err := tts.NewRegistry(directURL, cloneCfg.Token())
	if err != nil {
		t.Fatalf("registry: %v", err)
	}

	// 1. Design three real auditions from the fixed synthetic lines.
	const brief = "A calm narrator voice, warm and unhurried, for an original synthetic character."
	var clips []tts.Audio
	for i, line := range SyntheticAuditionLines {
		start := time.Now()
		audio, err := designClient.Design(ctx, tts.DesignRequest{
			Text: line, Instructions: brief, Language: "English",
		})
		if err != nil {
			t.Fatalf("audition %d: %v", i+1, err)
		}
		clips = append(clips, audio)
		t.Logf("audition %d: model=%s duration=%s latency=%s (wall %s)",
			i+1, audio.Model, audio.Duration, audio.Latency, time.Since(start).Round(time.Millisecond))
	}

	// 2. Approve the first: it becomes the canonical reference with the real
	// consent record (docs/spec/14), and the fingerprint is its identity.
	chosen := clips[0]
	sum := sha256.Sum256(chosen.WAV)
	refSHA := hex.EncodeToString(sum[:])
	fingerprint := Fingerprint(refSHA, SyntheticAuditionLines[0], chosen.Model)
	t.Logf("approved: sha256=%s fingerprint=%s", refSHA[:16]+"…", fingerprint[:16]+"…")

	// 3. Register exactly one gn_test_ name, and remove it on the way out.
	var rand8 [4]byte
	if _, err := rand.Read(rand8[:]); err != nil {
		t.Fatalf("random name: %v", err)
	}
	name := "gn_test_" + hex.EncodeToString(rand8[:])
	if err := tts.ValidateName(name); err != nil {
		t.Fatalf("test name fails the grammar: %v", err)
	}
	start := time.Now()
	uploaded, err := registry.Register(ctx, tts.Voice{
		Name: name, RefAudio: chosen.WAV,
		RefText: SyntheticAuditionLines[0], Consent: liveConsent,
	})
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	t.Cleanup(func() {
		if err := registry.Remove(context.Background(), name); err != nil {
			t.Logf("cleanup remove %s: %v", name, err)
		}
	})
	t.Logf("registered: name=%s remote_file_size=%d (wall %s)",
		name, uploaded.FileSize, time.Since(start).Round(time.Millisecond))

	// 4. Preview: miss, then hit — the hit must make no TTS request.
	assetRoot := t.TempDir()
	assetStore, err := assets.NewLocal(assetRoot)
	if err != nil {
		t.Fatalf("assets: %v", err)
	}
	cache := speech.New(assetStore, speech.NewMemIndex(), 1)
	key, err := speech.CacheKeyOf(speech.KeyInput{
		UtteranceText:            "Preview line: the road ahead was long.",
		OriginalMessageID:        "voice-preview:live-test",
		SpeechRulesRevision:      1,
		Language:                 "English",
		VoiceRevisionFingerprint: fingerprint,
		TTSModelID:               cloneCfg.Model,
		BackendConfigDigest:      "live-preview-v1",
		Format:                   "wav/pcm16",
	})
	if err != nil {
		t.Fatalf("cache key: %v", err)
	}
	line := "Preview line: the road ahead was long."
	gen := func(ctx context.Context) (tts.Audio, error) {
		return cloneClient.Generate(ctx, tts.GenerateRequest{Text: line, Voice: name, Language: "English"})
	}
	start = time.Now()
	miss, err := cache.GetOrGenerate(ctx, key, speech.PriorityCurrent, gen)
	if err != nil {
		t.Fatalf("preview miss: %v", err)
	}
	if miss.FromCache {
		t.Fatal("first preview was a cache hit; want a miss")
	}
	t.Logf("preview miss: sha256=%s duration=%s (wall %s)",
		miss.SHA256[:16]+"…", miss.Duration, time.Since(start).Round(time.Millisecond))

	hitsBefore := cache.Stats().Hits
	start = time.Now()
	hit, err := cache.GetOrGenerate(ctx, key, speech.PriorityCurrent, gen)
	if err != nil {
		t.Fatalf("preview hit: %v", err)
	}
	if !hit.FromCache {
		t.Fatal("second preview was a miss; want a hit (docs/spec/07 step 7)")
	}
	if hit.SHA256 != miss.SHA256 {
		t.Fatal("hit returned different bytes than the miss committed")
	}
	if got := cache.Stats().Hits; got != hitsBefore+1 {
		t.Fatalf("cache hits = %d, want %d (a hit is a cache decision, not a TTS call)", got, hitsBefore+1)
	}
	t.Logf("preview hit: sha256=%s (wall %s, no TTS request)",
		hit.SHA256[:16]+"…", time.Since(start).Round(time.Millisecond))

	// 5. The instance lists the test voice while it lives.
	listed, err := registry.List(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	found := false
	for _, v := range listed.Voices {
		if v == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("instance does not list %s after registering it", name)
	}
}
