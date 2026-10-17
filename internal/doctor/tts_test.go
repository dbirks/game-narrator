package doctor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// The TTS preflight tests (ADR-0022) run the real voice probes against
// recorded gateways over real HTTP, the same way the chat role tests do: the
// adapter, the deadline and the redaction are all covered by the same code
// path the CLI executes.

// recordedTTSGateway is the /audio/speech stub. It answers with a valid
// 24 kHz sine WAV and records the request bodies so the tests can assert the
// wire contract (task_type for Design, the chosen voice for Generate, the
// inline ref_audio for the reference probe).
type recordedTTSGateway struct {
	server *httptest.Server

	mu       sync.Mutex
	bodies   []string
	auths    []string
	failWith int // one-shot non-200 answer for /audio/speech
	failBody string
}

func newTTSGateway(t *testing.T) *recordedTTSGateway {
	t.Helper()
	g := &recordedTTSGateway{}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *recordedTTSGateway) url() string { return g.server.URL }

func (g *recordedTTSGateway) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	g.bodies = append(g.bodies, string(body))
	g.auths = append(g.auths, r.Header.Get("Authorization"))
	fail := g.failWith
	g.failWith = 0
	g.mu.Unlock()
	if fail != 0 {
		w.WriteHeader(fail)
		_, _ = w.Write([]byte(g.failBody))
		return
	}
	w.Header().Set("Content-Type", "audio/wav")
	_, _ = w.Write(probeWAV())
}

func (g *recordedTTSGateway) lastBody() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return ""
	}
	return g.bodies[len(g.bodies)-1]
}

// synthesisBody returns the body of the request that carried the synthesis
// probe text, as opposed to the reference-probe request.
func (g *recordedTTSGateway) synthesisBody() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	for i := len(g.bodies) - 1; i >= 0; i-- {
		if strings.Contains(g.bodies[i], "Probe the registered voice.") {
			return g.bodies[i]
		}
	}
	return ""
}

func (g *recordedTTSGateway) sawBearer(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.auths {
		if a == "Bearer "+token {
			return true
		}
	}
	return false
}

// recordedRegistry is the /v1/audio/voices stub for the direct Base service.
type recordedRegistry struct {
	server *httptest.Server
	voices []string
}

func newRegistry(t *testing.T, voices ...string) *recordedRegistry {
	t.Helper()
	rr := &recordedRegistry{voices: voices}
	rr.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/audio/voices" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"voices":          rr.voices,
			"uploaded_voices": []any{},
		})
	}))
	t.Cleanup(rr.server.Close)
	return rr
}

func (rr *recordedRegistry) url() string { return rr.server.URL }

// probeWAV renders a deterministic 0.5 s 24 kHz 16-bit mono sine — valid
// under the tts package's invariants (rate, duration, RMS, no clipping).
func probeWAV() []byte {
	const sr = 24000
	n := sr / 2
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		s := int16(12000 * math.Sin(2*math.Pi*440*float64(i)/float64(sr)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(s))
	}
	out := make([]byte, 0, 44+len(data))
	out = append(out, "RIFF"...)
	out = appendU32LE(out, 36+uint32(len(data)))
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = appendU32LE(out, 16)
	out = appendU16LE(out, 1)
	out = appendU16LE(out, 1)
	out = appendU32LE(out, uint32(sr))
	out = appendU32LE(out, uint32(sr)*2)
	out = appendU16LE(out, 2)
	out = appendU16LE(out, 16)
	out = append(out, "data"...)
	out = appendU32LE(out, uint32(len(data)))
	out = append(out, data...)
	return out
}

func appendU16LE(b []byte, v uint16) []byte {
	return append(b, byte(v), byte(v>>8))
}

func appendU32LE(b []byte, v uint32) []byte {
	return append(b, byte(v), byte(v>>8), byte(v>>16), byte(v>>24))
}

// --- voice_design ----------------------------------------------------------

// TestVoiceDesignRoleOKAgainstTheRecordedGateway proves the real probe path:
// the role's client sends the VoiceDesign task type and the check reports OK
// with measured duration and latency, without leaking the gateway URL.
func TestVoiceDesignRoleOKAgainstTheRecordedGateway(t *testing.T) {
	const token = "voice-secret-token"
	gw := newTTSGateway(t)
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL", "recorded-voicedesign")

	rep := h.run()
	c := h.check(rep, "providers.voice_design")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "recorded-voicedesign") {
		t.Fatalf("summary = %q, want the model id", c.Summary)
	}
	if !strings.Contains(c.Summary, "designed a ") {
		t.Fatalf("summary = %q, want the measured probe", c.Summary)
	}
	if !strings.Contains(c.Detail, "24000 Hz") {
		t.Errorf("detail = %q, want the sample rate evidence", c.Detail)
	}
	body := gw.lastBody()
	if !strings.Contains(body, `"task_type":"VoiceDesign"`) {
		t.Errorf("gateway did not see task_type VoiceDesign: %s", body)
	}
	if !gw.sawBearer(token) {
		t.Error("the gateway did not see the bearer token")
	}
	if strings.Contains(c.Summary+c.Detail, gw.url()) {
		t.Fatalf("check leaked the gateway URL: %+v", c)
	}
}

// TestVoiceDesignRoleFailsWithTheTaxonomyKind: a broken route is FAIL with
// its kind, and the remediation names the env var, never the token value.
func TestVoiceDesignRoleFailsWithTheTaxonomyKind(t *testing.T) {
	const token = "voice-secret-token"
	gw := newTTSGateway(t)
	gw.failWith = http.StatusUnauthorized
	gw.failBody = `{"error":{"message":"Invalid token"}}`
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL", "recorded-voicedesign")

	rep := h.run()
	c := h.check(rep, "providers.voice_design")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string("auth")) {
		t.Fatalf("summary = %q, want the taxonomy kind", c.Summary)
	}
	if strings.Contains(c.Summary+c.Detail+c.Remedy, token) {
		t.Fatalf("check leaked the token: %+v", c)
	}
	if !strings.Contains(c.Remedy, "GAME_NARRATOR_ROLE_VOICE_DESIGN_MODEL") {
		t.Errorf("remedy = %q, want the model env var", c.Remedy)
	}
}

// --- voice_clone ------------------------------------------------------------

// TestVoiceCloneRoleOKWithTheRegistry lists the recorded registry and
// synthesises with the registered voice; the inline-reference negotiation is
// reported in the detail line.
func TestVoiceCloneRoleOKWithTheRegistry(t *testing.T) {
	const token = "voice-secret-token"
	const registered = "gn_ab12cd34_narrator_v1_0123abcd"
	gw := newTTSGateway(t)
	reg := newRegistry(t, "default", registered)
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL", "recorded-base")
	h.withEnv(envVoiceRegistryURL, reg.url())

	rep := h.run()
	c := h.check(rep, "providers.voice_clone")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, registered) {
		t.Fatalf("summary = %q, want the chosen registered voice", c.Summary)
	}
	if !strings.Contains(c.Summary, "recorded-base") {
		t.Fatalf("summary = %q, want the model id", c.Summary)
	}
	// The synthesis request must have carried the registered voice.
	if !strings.Contains(gw.synthesisBody(), `"voice":"`+registered+`"`) {
		t.Errorf("gateway did not see the registered voice: %s", gw.synthesisBody())
	}
	if !strings.Contains(c.Detail, "2 voices listed") {
		t.Errorf("detail = %q, want the listing evidence", c.Detail)
	}
	if !strings.Contains(c.Detail, "inline ref_audio") {
		t.Errorf("detail = %q, want the negotiated reference state", c.Detail)
	}
}

// TestVoiceCloneRoleFallsBackToDefaultVoice: without a registry URL the probe
// speaks with the built-in "default", as the check contract requires.
func TestVoiceCloneRoleFallsBackToDefaultVoice(t *testing.T) {
	const token = "voice-secret-token"
	gw := newTTSGateway(t)
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL", "recorded-base")

	rep := h.run()
	c := h.check(rep, "providers.voice_clone")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, `"default"`) {
		t.Fatalf("summary = %q, want the default voice", c.Summary)
	}
	if !strings.Contains(gw.lastBody(), `"voice":"default"`) {
		t.Errorf("gateway did not see voice=default: %s", gw.lastBody())
	}
}

// TestVoiceCloneRolePrefersTheRegistryEnvOverTheFallback: when both registry
// variables are set, the role-level one wins and the detail says so.
func TestVoiceCloneRolePrefersTheRegistryEnvOverTheFallback(t *testing.T) {
	const token = "voice-secret-token"
	gw := newTTSGateway(t)
	reg := newRegistry(t, "gn_ab12cd34_narrator_v1_0123abcd")
	other := newRegistry(t, "gn_ffffffff_ignored_v1_0123abcd")
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL", "recorded-base")
	h.withEnv(envVoiceRegistryURL, reg.url())
	h.withEnv(envTTSBaseDirectURL, other.url())

	rep := h.run()
	c := h.check(rep, "providers.voice_clone")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "gn_ab12cd34_narrator_v1_0123abcd") {
		t.Fatalf("summary = %q, want the voice from the preferred registry", c.Summary)
	}
	if strings.Contains(c.Detail, "from "+envTTSBaseDirectURL) {
		t.Errorf("detail claimed the fallback variable was used: %q", c.Detail)
	}
}

// TestVoiceCloneRoleFailsWithTheTaxonomyKind: an unreachable voice is FAIL
// with its kind and a remedy that names the registry env var.
func TestVoiceCloneRoleFailsWithTheTaxonomyKind(t *testing.T) {
	const token = "voice-secret-token"
	gw := newTTSGateway(t)
	gw.failWith = http.StatusUnauthorized
	gw.failBody = `{"error":{"message":"Invalid token"}}`
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL", "recorded-base")

	rep := h.run()
	c := h.check(rep, "providers.voice_clone")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, "auth") {
		t.Fatalf("summary = %q, want the taxonomy kind", c.Summary)
	}
	if !strings.Contains(c.Remedy, envVoiceRegistryURL) {
		t.Errorf("remedy = %q, want the registry env var", c.Remedy)
	}
}

// TestPickProbeVoicePinsTheVoiceOrder: registered namespace first, then any
// non-default name, then the built-in default.
func TestPickProbeVoicePinsTheVoiceOrder(t *testing.T) {
	cases := []struct {
		name   string
		voices []string
		want   string
	}{
		{"registered first", []string{"default", "gn_ab12cd34_narrator_v1_0123abcd", "ryan"}, "gn_ab12cd34_narrator_v1_0123abcd"},
		{"non-default second", []string{"default", "ryan", "aiden"}, "ryan"},
		{"default alone", []string{"default"}, "default"},
		{"default last", []string{"ryan", "default"}, "ryan"},
		{"empty", nil, "default"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pickProbeVoice(tc.voices); got != tc.want {
				t.Errorf("pickProbeVoice(%v) = %q, want %q", tc.voices, got, tc.want)
			}
		})
	}
}

// TestVoiceProbeReferenceSupportNotProbedIsReported: a reference probe that
// never completes must not fail the check and must be reported as unprobed.
func TestVoiceCloneReportsUnprobedReference(t *testing.T) {
	const token = "voice-secret-token"
	gw := newTTSGateway(t)
	h := newFakeHost(t)
	h.opts.VoiceProbe = &flakyReferenceProbe{base: voiceProbe{}, failReference: true}
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_VOICE_CLONE_MODEL", "recorded-base")

	rep := h.run()
	c := h.check(rep, "providers.voice_clone")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Detail, "inline ref_audio not probed") {
		t.Errorf("detail = %q, want the unprobed reference state", c.Detail)
	}
	_ = fmt.Sprint
}

// flakyReferenceProbe wraps the real probe but makes the reference
// negotiation fail, so the doctor's best-effort reporting is covered.
type flakyReferenceProbe struct {
	base          voiceProbe
	failReference bool
}

func (p *flakyReferenceProbe) Design(ctx context.Context, cfg providers.RoleConfig, req tts.DesignRequest) (tts.Audio, error) {
	return p.base.Design(ctx, cfg, req)
}
func (p *flakyReferenceProbe) Generate(ctx context.Context, cfg providers.RoleConfig, req tts.GenerateRequest) (tts.Audio, error) {
	return p.base.Generate(ctx, cfg, req)
}
func (p *flakyReferenceProbe) ListVoices(ctx context.Context, cfg providers.RoleConfig, registryURL string) ([]string, error) {
	return p.base.ListVoices(ctx, cfg, registryURL)
}
func (p *flakyReferenceProbe) ProbeReference(ctx context.Context, cfg providers.RoleConfig) (tts.ReferenceProbe, error) {
	if p.failReference {
		return tts.ReferenceProbe{}, fmt.Errorf("simulated probe failure")
	}
	return p.base.ProbeReference(ctx, cfg)
}

var _ = context.Background
var _ = io.Discard
