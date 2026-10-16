package doctor

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Role checks are exercised two ways: against a recorded OpenAI-compatible
// gateway over real HTTP (the default probe, so the adapter, the deadline and
// the redaction are all covered) and against an in-process fake for the
// answer-shape matrix.

// --- fake probe ------------------------------------------------------------

type fakeProbe struct {
	models []string
	chat   func(prompt string, images [][]byte) (ChatResult, error)
	calls  []string
}

func (p *fakeProbe) ListModels(context.Context, providers.RoleConfig) ([]string, error) {
	return p.models, nil
}

func (p *fakeProbe) Chat(_ context.Context, _ providers.RoleConfig, prompt string, images [][]byte) (ChatResult, error) {
	p.calls = append(p.calls, prompt)
	return p.chat(prompt, images)
}

func configuredRoles(role providers.Role, model string) map[providers.Role]providers.RoleConfig {
	return map[providers.Role]providers.RoleConfig{
		role: {
			Role:     role,
			Protocol: providers.ProtocolOpenAIChat,
			BaseURL:  "http://gateway.invalid/v1",
			Model:    model,
			TokenEnv: providers.EnvGatewayToken,
			Timeout:  5 * time.Second,
		},
	}
}

func TestUnconfiguredRolesAreNotConfigured(t *testing.T) {
	h := newFakeHost(t)
	rep := h.run()
	for _, role := range providers.AllRoles {
		c := h.check(rep, "providers."+string(role))
		mustStatus(t, c, StatusNotConfigured)
		modelEnv, _, _, _ := providers.RoleEnvVars(role)
		if !strings.Contains(c.Remedy, modelEnv) {
			t.Errorf("%s: remedy = %q, want it to name %s", role, c.Remedy, modelEnv)
		}
	}
	if rep.HasFail() {
		t.Fatal("an unconfigured gateway must not fail the run — a first-boot host is expected to look like this")
	}
}

func TestUndeployedRolesAreDegradedEvenWhenConfigured(t *testing.T) {
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", "http://gateway.invalid/v1")
	for _, role := range []providers.Role{
		providers.RoleDecision, providers.RoleVoiceDesign, providers.RoleVoiceClone, providers.RoleSpeechASR,
	} {
		modelEnv, _, _, _ := providers.RoleEnvVars(role)
		h.withEnv(modelEnv, "some-checkpoint")
	}
	rep := h.run()
	for _, role := range []providers.Role{
		providers.RoleDecision, providers.RoleVoiceDesign, providers.RoleVoiceClone, providers.RoleSpeechASR,
	} {
		c := h.check(rep, "providers."+string(role))
		mustStatus(t, c, StatusDegraded)
		if c.Summary != "client not implemented yet (M3/M4)" {
			t.Fatalf("%s: summary = %q, want the documented wording", role, c.Summary)
		}
		if !strings.Contains(c.Detail, "some-checkpoint") {
			t.Errorf("%s: detail = %q, want the configured model recorded", role, c.Detail)
		}
	}
}

func TestChatRoleOKAgainstTheRecordedGateway(t *testing.T) {
	const token = "super-secret-token-value"
	gw := newGateway(t, gatewayStub{
		models: []string{"recorded-chat-model"},
		answer: func(body string) string { return "pong" },
	})
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", token)
	h.withEnv("GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "recorded-chat-model")

	rep := h.run()
	c := h.check(rep, "providers.fast_reasoning")
	mustStatus(t, c, StatusOK)
	if !strings.Contains(c.Summary, "recorded-chat-model") {
		t.Fatalf("summary = %q, want the model id", c.Summary)
	}
	if !strings.Contains(c.Summary, "answered the chat probe in ") {
		t.Fatalf("summary = %q, want the measured latency", c.Summary)
	}
	if !strings.Contains(c.Detail, "1 models offered") {
		t.Fatalf("detail = %q, want the listing evidence", c.Detail)
	}
	if !gw.sawChat() {
		t.Fatal("no chat request reached the gateway — an OK without a request is simulation theatre")
	}
	if !gw.sawBearer(token) {
		t.Fatalf("the gateway did not see the bearer token: %+v", gw.summary())
	}
	if strings.Contains(c.Summary+c.Detail, gw.url()) {
		t.Fatalf("check leaked the gateway URL: %+v", c)
	}
}

func TestVisionRoleMustReadTheControlledImage(t *testing.T) {
	cases := []struct {
		name   string
		vision string
		want   string
	}{
		{"model reads the image", controlledText, StatusOK},
		{"model hallucinates", "NARRATOR 4Z", StatusFail},
		{"model refuses", "I cannot see an image", StatusFail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newGateway(t, gatewayStub{
				models: []string{"recorded-vision-model"},
				answer: func(body string) string {
					if strings.Contains(body, "image_url") {
						return tc.vision
					}
					return "pong"
				},
			})
			h := newFakeHost(t)
			h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
			h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", "t")
			h.withEnv("GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL", "recorded-vision-model")

			c := h.check(h.run(), "providers.vision_ocr_fallback")
			mustStatus(t, c, tc.want)
			if tc.want == StatusOK {
				if !strings.Contains(c.Summary, controlledText) {
					t.Fatalf("summary = %q, want the recovered text", c.Summary)
				}
				if !gw.sawImage() {
					t.Fatal("the vision probe sent no image part")
				}
			}
		})
	}
}

func TestProviderErrorKindIsInTheSummary(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"unauthorised", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, string(providers.KindAuth)},
		{"rate limited", http.StatusTooManyRequests, `{"error":{"message":"slow down"}}`, string(providers.KindRateLimited)},
		{"unreachable", http.StatusServiceUnavailable, `unavailable`, string(providers.KindUnavailable)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newGateway(t, gatewayStub{models: []string{"m"}, failStatus: tc.status, failBody: tc.body})
			h := newFakeHost(t)
			h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
			h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", "secret-token-value")
			h.withEnv("GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "m")

			rep := h.run()
			c := h.check(rep, "providers.fast_reasoning")
			mustStatus(t, c, StatusFail)
			if !strings.Contains(c.Summary, tc.want) {
				t.Fatalf("summary = %q, want the taxonomy kind %q", c.Summary, tc.want)
			}
			// Neither the gateway address nor the credential may appear in any
			// rendered form of a failing report (AGENTS.md rule 5).
			var out strings.Builder
			if err := Format(&out, rep); err != nil {
				t.Fatal(err)
			}
			if err := FormatJSON(&out, rep); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), gw.host()) {
				t.Fatalf("report leaked the gateway host:\n%s", out.String())
			}
			if strings.Contains(out.String(), "secret-token-value") {
				t.Fatalf("report leaked the token:\n%s", out.String())
			}
		})
	}
}

func TestModelAbsentFromTheListingIsFail(t *testing.T) {
	gw := newGateway(t, gatewayStub{models: []string{"other-model"}})
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "wanted-model")

	c := h.check(h.run(), "providers.fast_reasoning")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindUnsupportedCapability)) ||
		!strings.Contains(c.Summary, "wanted-model") {
		t.Fatalf("summary = %q, want the kind and the model id", c.Summary)
	}
	if gw.sawChat() {
		t.Fatal("a chat request was sent for a model the gateway does not offer")
	}
}

func TestWrongAnswerIsFailNotDegraded(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Roles = configuredRoles(providers.RoleFastReasoning, "fake-model")
	h.opts.Probe = &fakeProbe{
		models: []string{"fake-model"},
		chat: func(string, [][]byte) (ChatResult, error) {
			return ChatResult{Text: "I would rather not say", Latency: 12 * time.Millisecond}, nil
		},
	}
	c := h.check(h.run(), "providers.fast_reasoning")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindBadSchema)) {
		t.Fatalf("summary = %q", c.Summary)
	}
}

func TestProbeErrorFromAFakeClientIsFail(t *testing.T) {
	h := newFakeHost(t)
	h.opts.Roles = configuredRoles(providers.RoleAgentReasoning, "fake-model")
	h.opts.Probe = &fakeProbe{
		models: []string{"fake-model"},
		chat: func(string, [][]byte) (ChatResult, error) {
			return ChatResult{}, providers.NewError(providers.KindTimeout, http.StatusRequestTimeout, "gateway too slow")
		},
	}
	c := h.check(h.run(), "providers.agent_reasoning")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindTimeout)) {
		t.Fatalf("summary = %q, want the timeout kind", c.Summary)
	}
}

func TestModelTimeoutBoundsASlowGateway(t *testing.T) {
	gw := newGateway(t, gatewayStub{
		models: []string{"slow-model"},
		delay:  400 * time.Millisecond,
		answer: func(string) string { return "pong" },
	})
	h := newFakeHost(t)
	h.opts.ModelTimeout = 80 * time.Millisecond
	h.withEnv("GAME_NARRATOR_GATEWAY_URL", gw.url())
	h.withEnv("GAME_NARRATOR_GATEWAY_TOKEN", "t")
	h.withEnv("GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL", "slow-model")

	c := h.check(h.run(), "providers.agent_reasoning")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindTimeout)) {
		t.Fatalf("summary = %q, want a deadline-driven timeout", c.Summary)
	}
	if c.Duration > 2*time.Second {
		t.Fatalf("duration = %s, the model deadline did not bound the probe", c.Duration)
	}
}

func TestInvalidRoleConfigurationIsReportedOnceAndLoudly(t *testing.T) {
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_ROLE_FAST_REASONING_PROTOCOL", "not-a-protocol")
	rep := h.run()

	c := h.check(rep, "providers.config")
	mustStatus(t, c, StatusFail)
	if !rep.HasFail() {
		t.Fatal("invalid role configuration must exit non-zero")
	}
	// The seven role rows are replaced by the one configuration failure, not
	// silently reported as NOT CONFIGURED.
	for _, role := range providers.AllRoles {
		for _, got := range rep.Checks {
			if got.ID == "providers."+string(role) {
				t.Fatalf("%s was reported although the configuration is invalid: %+v", role, got)
			}
		}
	}
	if strings.Contains(c.Summary+c.Detail, "http://") || strings.Contains(c.Summary+c.Detail, "https://") {
		t.Fatalf("config failure leaked a URL: %+v", c)
	}
}

// TestRoleProbeWithoutGatewayURLIsFail proves a half-configured role is not
// presented as working: the model is set, the endpoint is not.
func TestRoleProbeWithoutGatewayURLIsFail(t *testing.T) {
	h := newFakeHost(t)
	h.withEnv("GAME_NARRATOR_ROLE_FAST_REASONING_MODEL", "orphan-model")
	c := h.check(h.run(), "providers.fast_reasoning")
	mustStatus(t, c, StatusFail)
	if !strings.Contains(c.Summary, string(providers.KindUnsupportedCapability)) {
		t.Fatalf("summary = %q", c.Summary)
	}
}

// --- recorded gateway ------------------------------------------------------

type gatewayStub struct {
	models     []string
	answer     func(body string) string
	failStatus int
	failBody   string
	delay      time.Duration
}

type recordedGateway struct {
	server *httptest.Server
	stub   gatewayStub

	mu       sync.Mutex
	chatSeen int
	image    bool
	bodies   []string
	auths    []string
}

func newGateway(t *testing.T, stub gatewayStub) *recordedGateway {
	t.Helper()
	g := &recordedGateway{stub: stub}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *recordedGateway) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	auth := r.Header.Get("Authorization")
	g.auths = append(g.auths, auth)
	g.mu.Unlock()

	switch r.URL.Path {
	case "/models":
		if g.stub.failStatus != 0 {
			http.Error(w, g.stub.failBody, g.stub.failStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data":   mapModels(g.stub.models),
		})
	case "/chat/completions":
		body, _ := io.ReadAll(r.Body)
		if g.stub.delay > 0 {
			select {
			case <-time.After(g.stub.delay):
			case <-r.Context().Done():
				return
			}
		}
		g.mu.Lock()
		g.chatSeen++
		text := strings.Join([]string{"image_url"}, " ")
		if strings.Contains(string(body), text) {
			g.image = true
		}
		g.bodies = append(g.bodies, string(body))
		g.mu.Unlock()

		if g.stub.failStatus != 0 {
			http.Error(w, g.stub.failBody, g.stub.failStatus)
			return
		}
		reply := "pong"
		if g.stub.answer != nil {
			reply = g.stub.answer(string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-recorded",
			"object":  "chat.completion",
			"created": 1,
			"model":   "recorded",
			"choices": []any{map[string]any{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": reply},
			}},
			"usage": map[string]any{"prompt_tokens": 5, "completion_tokens": 2},
		})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

func mapModels(models []string) []map[string]string {
	out := make([]map[string]string, 0, len(models))
	for _, m := range models {
		out = append(out, map[string]string{"id": m, "object": "model"})
	}
	return out
}

func (g *recordedGateway) url() string  { return g.server.URL }
func (g *recordedGateway) host() string { return strings.TrimPrefix(g.server.URL, "http://") }

func (g *recordedGateway) sawChat() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.chatSeen > 0
}

func (g *recordedGateway) sawImage() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.image
}

func (g *recordedGateway) sawBearer(token string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range g.auths {
		if a == "Bearer "+token {
			return true
		}
	}
	return false
}

func (g *recordedGateway) summary() map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return map[string]any{"chat": g.chatSeen, "image": g.image, "auths": len(g.auths)}
}
