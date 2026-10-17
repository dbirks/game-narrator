package tts

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// LIVE contract tests (docs/spec/12 "Contract" row): the only proof that the
// recorded fixtures still match the deployed checkpoints. They run only when
// the environment names real endpoints and SKIP as "NOT RUN" otherwise, so
// `go test ./...` stays green offline.
//
// Configuration (never printed, never logged):
//
//	GAME_NARRATOR_GATEWAY_URL              shared gateway for the speech route
//	GAME_NARRATOR_GATEWAY_TOKEN_ENV        names the variable holding the token
//	GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL  default qwen3-tts-voicedesign
//	GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL   default qwen3-tts-base
//	GAME_NARRATOR_TTS_BASE_DIRECT_URL      direct Base service for the registry
//	                                       (orchestrator port-forward); the
//	                                       registry live tests are NOT RUN
//	                                       without it.
//
// Generated audio is written to t.TempDir() only. The registry cycle touches
// exclusively names of its own — gn_test_<random8> — and always removes it
// via t.Cleanup. Nothing in this file may print a URL or a token: log lines
// carry model ids, durations, latencies and (sanitised) voice names only.

const (
	liveDesignModel = "qwen3-tts-voicedesign"
	liveCloneModel  = "qwen3-tts-base"

	liveDesignLine    = "The map glows faintly where the river bends."
	liveDesignInstr   = "A calm narrator voice with a steady, unhurried cadence."
	liveGenDefault    = "A second line for the same speaker."
	liveGenRegistered = "The fog lifts just enough to see the beacon."
)

// liveGatewayConfig resolves the speech route through the normal role
// configuration path, so the live tests exercise the same resolution code
// the service uses. ok is false when the gateway or token is absent.
func liveGatewayConfig(t *testing.T, role providers.Role, modelDefault string) (cfg providers.RoleConfig, ok bool) {
	t.Helper()
	roles, err := providers.LoadRoles(os.LookupEnv)
	if err != nil {
		// A malformed GAME_NARRATOR_* value is a configuration fault; the
		// tests must fail loudly rather than skip.
		t.Fatalf("LoadRoles: %v", err)
	}
	c := roles[role]
	modelEnv, _, _, _ := providers.RoleEnvVars(role)
	if model := strings.TrimSpace(os.Getenv(modelEnv)); model != "" {
		c.Model = model
	} else {
		c.Model = modelDefault
	}
	c.Timeout = 120 * time.Second
	if strings.TrimSpace(c.BaseURL) == "" || strings.TrimSpace(c.Token()) == "" {
		return c, false
	}
	return c, true
}

// liveSkipGateway is the documented skip reason.
const liveSkipGateway = "NOT RUN: set GAME_NARRATOR_GATEWAY_URL and the token env named by " +
	providers.EnvGatewayTokenEnv + " (default " + providers.EnvGatewayToken + ") to run the live TTS tests"

func newLiveClient(t *testing.T, role providers.Role, modelDefault string) *Client {
	t.Helper()
	cfg, ok := liveGatewayConfig(t, role, modelDefault)
	if !ok {
		t.Skip(liveSkipGateway)
	}
	c, err := NewFromRole(cfg)
	if err != nil {
		t.Fatalf("NewFromRole(%s): %v", cfg.Model, err)
	}
	return c
}

func liveCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func saveWAV(t *testing.T, name string, a Audio) {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, a.WAV, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	t.Logf("saved %s (%d bytes)", name, len(a.WAV))
}

// TestLiveDesignViaGateway proves the VoiceDesign checkpoint through the
// shared gateway: one invented line, one voice description, a valid WAV back.
func TestLiveDesignViaGateway(t *testing.T) {
	c := newLiveClient(t, providers.RoleVoiceDesign, liveDesignModel)
	ctx := liveCtx(t)
	a, err := c.Design(ctx, DesignRequest{Text: liveDesignLine, Instructions: liveDesignInstr, Language: "English"})
	if err != nil {
		t.Fatalf("live Design: %v", err)
	}
	saveWAV(t, "live-design.wav", a)
	t.Logf("live design: model=%s duration=%s latency=%s sample_rate=%d origin=%s",
		a.Model, a.Duration.Round(time.Millisecond), a.Latency.Round(time.Millisecond), a.SampleRate, a.Origin)
}

// TestLiveGenerateViaGateway proves Base synthesis through the gateway. The
// voice is tried in the order the deployment actually resolves: the built-in
// "default" first, then the first listed voice. (A Base checkpoint may not
// resolve a built-in name to an embedding; a listed voice always does.)
func TestLiveGenerateViaGateway(t *testing.T) {
	c := newLiveClient(t, providers.RoleVoiceClone, liveCloneModel)
	ctx := liveCtx(t)
	candidates := []string{"default"}
	if names, ok := liveListedVoices(t, ctx); ok {
		candidates = append(candidates, names...)
	}
	var lastErr error
	for _, voice := range candidates {
		a, err := c.Generate(ctx, GenerateRequest{Text: liveGenDefault, Voice: voice})
		if err != nil {
			lastErr = err
			t.Logf("live generate with voice=%s failed: %v", voice, classifyForLog(err))
			continue
		}
		saveWAV(t, "live-generate.wav", a)
		t.Logf("live generate: model=%s voice=%s duration=%s latency=%s sample_rate=%d origin=%s",
			a.Model, voice, a.Duration.Round(time.Millisecond), a.Latency.Round(time.Millisecond), a.SampleRate, a.Origin)
		return
	}
	t.Fatalf("no listed voice could synthesise through the gateway; last error: %v", classifyForLog(lastErr))
}

// liveListedVoices lists the voices the direct Base service offers, when the
// direct URL is configured. It is best-effort: a listing failure just means
// the fallback chain is shorter.
func liveListedVoices(t *testing.T, ctx context.Context) ([]string, bool) {
	t.Helper()
	reg, ok := liveRegistry(t)
	if !ok {
		return nil, false
	}
	listed, err := reg.List(ctx)
	if err != nil {
		t.Logf("live listing failed, continuing without it: %v", classifyForLog(err))
		return nil, false
	}
	// Uploaded voices first: they are guaranteed Base voices on this
	// instance. Built-ins follow.
	var out []string
	seen := map[string]bool{}
	for _, u := range listed.Uploaded {
		if !seen[u.Name] {
			out = append(out, u.Name)
			seen[u.Name] = true
		}
	}
	for _, n := range listed.Voices {
		if !seen[n] {
			out = append(out, n)
			seen[n] = true
		}
	}
	return out, true
}

// liveRegistry builds the registry client from the direct Base service URL.
// ok is false when the variable is absent: the registry route is not exposed
// through the shared gateway, so without the direct URL these tests are NOT
// RUN, exactly as the task contract requires.
func liveRegistry(t *testing.T) (*Registry, bool) {
	t.Helper()
	direct := strings.TrimSpace(os.Getenv("GAME_NARRATOR_TTS_BASE_DIRECT_URL"))
	if direct == "" {
		return nil, false
	}
	token := ""
	if roles, err := providers.LoadRoles(os.LookupEnv); err == nil {
		token = roles[providers.RoleVoiceClone].Token()
	}
	r, err := NewRegistry(direct, token)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return r, true
}

// TestLiveRegistryCycleDirect is the full reconcile contract against the
// real Base service: register the just-designed clip under a gn_test_ name
// with truthful synthetic consent, list it, synthesise a different sentence
// with it, then remove it. The removal runs in t.Cleanup so a failed
// assertion never leaves a test voice behind.
func TestLiveRegistryCycleDirect(t *testing.T) {
	reg, ok := liveRegistry(t)
	if !ok {
		t.Skip("NOT RUN: set GAME_NARRATOR_TTS_BASE_DIRECT_URL to the direct Base service (orchestrator port-forward) to run the live voice registry tests")
	}
	design := newLiveClient(t, providers.RoleVoiceDesign, liveDesignModel)
	ctx := liveCtx(t)

	a, err := design.Design(ctx, DesignRequest{Text: liveDesignLine, Instructions: liveDesignInstr, Language: "English"})
	if err != nil {
		t.Fatalf("live Design for the registry clip: %v", err)
	}
	saveWAV(t, "live-registry-design.wav", a)
	t.Logf("registry clip designed: duration=%s latency=%s", a.Duration.Round(time.Millisecond), a.Latency.Round(time.Millisecond))

	name, err := testName()
	if err != nil {
		t.Fatalf("test name: %v", err)
	}
	removed := false
	t.Cleanup(func() {
		if removed {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := reg.Remove(cleanupCtx, name); err != nil && !errors.Is(err, ErrInvalidName) {
			t.Errorf("cleanup Remove(%s): %v", name, classifyForLog(err))
		}
	})

	voice := Voice{
		Name:     name,
		RefAudio: a.WAV,
		RefText:  liveDesignLine,
		Consent:  "original_synthetic_design (game-narrator automated test)",
	}
	if _, err := reg.Register(ctx, voice); err != nil {
		t.Fatalf("live Register(%s): %v", name, classifyForLog(err))
	}

	// List must show it.
	listed, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("live List: %v", err)
	}
	found := false
	for _, n := range listed.Voices {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("List does not show %s (voices: %v)", name, sanitizeNames(listed.Voices))
	}

	// Generate a different sentence with the registered voice.
	c := newLiveClient(t, providers.RoleVoiceClone, liveCloneModel)
	ga, err := c.Generate(ctx, GenerateRequest{Text: liveGenRegistered, Voice: name})
	if err != nil {
		t.Fatalf("live Generate(voice=%s): %v", name, classifyForLog(err))
	}
	saveWAV(t, "live-registry-generate.wav", ga)
	t.Logf("registered-voice generate: voice=%s duration=%s latency=%s origin=%s",
		name, ga.Duration.Round(time.Millisecond), ga.Latency.Round(time.Millisecond), ga.Origin)

	// Remove, then list must not show it.
	if err := reg.Remove(ctx, name); err != nil {
		t.Fatalf("live Remove(%s): %v", name, classifyForLog(err))
	}
	removed = true
	listed2, err := reg.List(ctx)
	if err != nil {
		t.Fatalf("live List after remove: %v", err)
	}
	for _, n := range listed2.Voices {
		if n == name {
			t.Fatalf("List still shows %s after removal", name)
		}
	}
	t.Logf("registry cycle complete: %s registered, spoken, removed", name)
}

// testName builds the only name shape the live tests may register or delete:
// gn_test_<random8>.
func testName() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "gn_test_" + hex.EncodeToString(b[:]), nil
}

// classifyForLog renders an error as "kind: short message" for log lines.
// It never includes a URL or token: provider errors are already redacted and
// transport errors are reduced to their classification.
func classifyForLog(err error) string {
	if err == nil {
		return ""
	}
	if pe := asProvidersError(err); pe != nil {
		return string(pe.Kind) + ": " + oneLine(pe.Msg, 160)
	}
	return "unclassified: " + oneLine(err.Error(), 160)
}

// sanitizeNames keeps the (non-sensitive) voice names for a log line while
// guaranteeing no URL-shaped value slips in.
func sanitizeNames(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if strings.Contains(n, "://") || strings.ContainsRune(n, ' ') {
			continue
		}
		out = append(out, n)
	}
	return out
}
