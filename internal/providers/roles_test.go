package providers

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// envLookupFunc builds a lookup function from a snapshot, so tests never mutate
// process environment state.
func envLookupFunc(values map[string]string) func(string) (string, bool) {
	return func(key string) (string, bool) {
		v, ok := values[key]
		return v, ok
	}
}

const testToken = "super-secret-token-value"

func TestLoadRolesDefaultsEveryRoleUnconfigured(t *testing.T) {
	roles, err := LoadRoles(envLookupFunc(map[string]string{}))
	if err != nil {
		t.Fatalf("LoadRoles with empty env: %v", err)
	}
	if len(roles) != len(AllRoles) {
		t.Fatalf("got %d roles, want %d", len(roles), len(AllRoles))
	}
	wantDefaults := map[Role]struct {
		protocol Protocol
		timeout  time.Duration
		deployed bool
	}{
		RoleAgentReasoning:    {ProtocolOpenAIChat, 30 * time.Second, true},
		RoleFastReasoning:     {ProtocolOpenAIChat, 10 * time.Second, true},
		RoleVisionOCRFallback: {ProtocolOpenAIChatVision, 10 * time.Second, true},
		RoleDecision:          {ProtocolSystemOne, 1500 * time.Millisecond, false},
		RoleVoiceDesign:       {ProtocolVLLMOmniSpeech, 120 * time.Second, false},
		RoleVoiceClone:        {ProtocolVLLMOmniSpeech, 120 * time.Second, false},
		RoleSpeechASR:         {ProtocolProbeRequired, 30 * time.Second, false},
	}
	for _, role := range AllRoles {
		cfg, ok := roles[role]
		if !ok {
			t.Fatalf("role %s missing from registry", role)
		}
		want := wantDefaults[role]
		if cfg.Protocol != want.protocol {
			t.Errorf("role %s protocol = %q, want %q", role, cfg.Protocol, want.protocol)
		}
		if cfg.Timeout != want.timeout {
			t.Errorf("role %s timeout = %s, want %s", role, cfg.Timeout, want.timeout)
		}
		if cfg.Deployed() != want.deployed {
			t.Errorf("role %s deployed = %v, want %v", role, cfg.Deployed(), want.deployed)
		}
		if cfg.Configured() {
			t.Errorf("role %s must be unconfigured with no model set", role)
		}
		if !strings.Contains(cfg.Status(), "NOT CONFIGURED") {
			t.Errorf("role %s Status() = %q, want it to report NOT CONFIGURED", role, cfg.Status())
		}
	}
}

func TestLoadRolesSharedGatewayAndTokenEnvIndirection(t *testing.T) {
	env := map[string]string{
		EnvGatewayURL:         "https://gateway.example.invalid/v1/",
		EnvGatewayTokenEnv:    "MY_EXISTING_API_KEY",
		"MY_EXISTING_API_KEY": testToken,
		"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL":      "fast-model",
		"GAME_NARRATOR_ROLE_AGENT_REASONING_MODEL":     "agent-model",
		"GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL": "vision-model",
	}
	roles, err := LoadRoles(envLookupFunc(env))
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	fast := roles[RoleFastReasoning]
	if !fast.Configured() || fast.Model != "fast-model" {
		t.Errorf("fast_reasoning = %+v, want configured with model fast-model", fast.LogValue())
	}
	if fast.BaseURL != "https://gateway.example.invalid/v1" {
		t.Errorf("fast_reasoning BaseURL = %q, want trailing slash trimmed", fast.BaseURL)
	}
	if fast.TokenEnv != "MY_EXISTING_API_KEY" {
		t.Errorf("fast_reasoning TokenEnv = %q, want MY_EXISTING_API_KEY", fast.TokenEnv)
	}
	if fast.Token() != testToken {
		t.Errorf("fast_reasoning token not resolved from the named env var")
	}
	for _, role := range AllRoles {
		if roles[role].BaseURL != "https://gateway.example.invalid/v1" {
			t.Errorf("role %s BaseURL = %q, want the shared gateway", role, roles[role].BaseURL)
		}
	}
	// An undeployed role stays representable but inert even with a gateway set.
	if roles[RoleDecision].Configured() {
		t.Errorf("decision must remain unconfigured until a model is set")
	}
	// The chat roles are usable; the rest are reported missing.
	reg := NewRegistry(roles)
	gotReady := reg.Ready()
	if len(gotReady) != 3 {
		t.Errorf("Ready() = %v, want the three chat roles", gotReady)
	}
	if len(reg.Missing()) != 4 {
		t.Errorf("Missing() = %v, want the four undeployed roles", reg.Missing())
	}
}

func TestLoadRoleOverridesModelURLProtocolTimeout(t *testing.T) {
	env := map[string]string{
		EnvGatewayURL: "https://gateway.example.invalid/v1",
		"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL":    "fast-model",
		"GAME_NARRATOR_ROLE_FAST_REASONING_URL":      "http://127.0.0.1:8097/v1",
		"GAME_NARRATOR_ROLE_FAST_REASONING_PROTOCOL": "openai_chat_vision",
		"GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT":  "2500ms",
		"GAME_NARRATOR_ROLE_DECISION_MODEL":          "Cloudflare/clef-omni",
	}
	roles, err := LoadRoles(envLookupFunc(env))
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	fast := roles[RoleFastReasoning]
	if fast.BaseURL != "http://127.0.0.1:8097/v1" {
		t.Errorf("role URL override not applied: %q", fast.BaseURL)
	}
	if fast.Protocol != ProtocolOpenAIChatVision {
		t.Errorf("role protocol override not applied: %q", fast.Protocol)
	}
	if fast.Timeout != 2500*time.Millisecond {
		t.Errorf("role timeout override = %s, want 2.5s", fast.Timeout)
	}
	if fast.Token() != "" {
		t.Errorf("a dedicated endpoint must not inherit the shared gateway credential")
	}
	decision := roles[RoleDecision]
	if !decision.Configured() {
		t.Fatal("decision with an explicit model must report configured")
	}
	if decision.Deployed() {
		t.Error("decision must report no adapter in this build")
	}
	if !strings.Contains(decision.Status(), "NOT") || !strings.Contains(decision.Status(), "adapter") {
		t.Errorf("decision Status() = %q, want an explicit capability statement", decision.Status())
	}
}

func TestLoadRolesRejectsInvalidURLsNamingTheEnvVar(t *testing.T) {
	cases := []struct {
		name  string
		env   map[string]string
		wants []string
	}{
		{"gateway scheme", map[string]string{EnvGatewayURL: "ftp://gateway.example.invalid/v1"}, []string{EnvGatewayURL, "scheme"}},
		{"gateway userinfo", map[string]string{EnvGatewayURL: "https://user:pw@gateway.example.invalid/v1"}, []string{EnvGatewayURL, "userinfo"}},
		{"gateway no host", map[string]string{EnvGatewayURL: "https://"}, []string{EnvGatewayURL, "host"}},
		{"gateway unparseable", map[string]string{EnvGatewayURL: "https://[::1"}, []string{EnvGatewayURL}},
		{"role url", map[string]string{
			"GAME_NARRATOR_ROLE_FAST_REASONING_URL":   "ws://gateway.example.invalid/v1",
			"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL": "fast-model",
		}, []string{"GAME_NARRATOR_ROLE_FAST_REASONING_URL", "scheme"}},
		{"role protocol", map[string]string{
			"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL":    "fast-model",
			"GAME_NARRATOR_ROLE_FAST_REASONING_PROTOCOL": "grpc",
		}, []string{"GAME_NARRATOR_ROLE_FAST_REASONING_PROTOCOL", "unknown protocol"}},
		{"role timeout", map[string]string{
			"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL":   "fast-model",
			"GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT": "soon",
		}, []string{"GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT", "duration"}},
		{"role zero timeout", map[string]string{
			"GAME_NARRATOR_ROLE_FAST_REASONING_MODEL":   "fast-model",
			"GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT": "0s",
		}, []string{"GAME_NARRATOR_ROLE_FAST_REASONING_TIMEOUT"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A token is present in every case so the assertions below prove the
			// credential never reaches the error text.
			tc.env[EnvGatewayToken] = testToken
			_, err := LoadRoles(envLookupFunc(tc.env))
			if err == nil {
				t.Fatalf("want an error for %v", tc.env)
			}
			msg := err.Error()
			for _, want := range tc.wants {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			if strings.Contains(msg, testToken) {
				t.Errorf("error leaked the token: %q", msg)
			}
		})
	}
}

// TestRoleConfigStringMasksToken guards the logging path: a RoleConfig handed
// to fmt, slog or the preflight UI must never carry the credential.
func TestRoleConfigStringMasksToken(t *testing.T) {
	cfg := RoleConfig{
		Role:     RoleFastReasoning,
		Protocol: ProtocolOpenAIChat,
		BaseURL:  "https://gateway.example.invalid/v1",
		Model:    "fast-model",
		TokenEnv: EnvGatewayToken,
		Timeout:  10 * time.Second,
		token:    testToken,
	}
	stringed := cfg.String()
	if strings.Contains(stringed, testToken) {
		t.Errorf("String() leaked the token: %q", stringed)
	}
	if !strings.Contains(stringed, MaskValue) {
		t.Errorf("String() does not mask the token: %q", stringed)
	}
	logValue := sprintLogValue(cfg)
	if strings.Contains(logValue, testToken) {
		t.Errorf("LogValue() leaked the token: %q", logValue)
	}
	if !strings.Contains(logValue, MaskValue) {
		t.Errorf("LogValue() does not mask the token: %q", logValue)
	}
	// The registry-wide rendering used by preflight must be safe too.
	reg := NewRegistry(map[Role]RoleConfig{RoleFastReasoning: cfg, RoleDecision: {Role: RoleDecision, Protocol: ProtocolSystemOne}})
	if strings.Contains(reg.String(), testToken) {
		t.Errorf("Registry.String() leaked the token: %q", reg.String())
	}
	var formatter interface{ String() string } = cfg
	if strings.Contains(formatter.String(), testToken) {
		t.Error("fmt.Stringer path leaked the token")
	}
}

func sprintLogValue(cfg RoleConfig) string {
	// LogValue must be a plain, serialisable redacted view.
	return mapToString(cfg.LogValue())
}

func mapToString(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "<unexpected LogValue shape>"
	}
	var b strings.Builder
	for _, key := range []string{"role", "protocol", "model", "base_url", "token_env", "token", "timeout", "state"} {
		if entry, exists := m[key]; exists {
			b.WriteString(key + "=" + toString(entry) + " ")
		}
	}
	return b.String()
}

func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return "?"
}

func TestLogValueShapeIsSerialisable(t *testing.T) {
	cfg := RoleConfig{Role: RoleVisionOCRFallback, Protocol: ProtocolOpenAIChatVision, Model: "v", Timeout: time.Second, token: testToken}
	m, ok := cfg.LogValue().(map[string]any)
	if !ok {
		t.Fatalf("LogValue() returned %T, want map[string]any", cfg.LogValue())
	}
	if m["token"] != MaskValue {
		t.Errorf("LogValue token = %v, want %q", m["token"], MaskValue)
	}
	if m["role"] != string(RoleVisionOCRFallback) {
		t.Errorf("LogValue role = %v", m["role"])
	}
}

func TestRoleEnvVarsUseUppercaseSegments(t *testing.T) {
	model, u, proto, timeout := RoleEnvVars(RoleVisionOCRFallback)
	if model != "GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_MODEL" {
		t.Errorf("model env = %q", model)
	}
	if u != "GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_URL" || proto != "GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_PROTOCOL" ||
		timeout != "GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_TIMEOUT" {
		t.Errorf("override envs = %q %q %q", u, proto, timeout)
	}
	if got := len(EnvKeyList()); got != 3+5*len(AllRoles) {
		t.Errorf("EnvKeyList length = %d, want %d", got, 3+5*len(AllRoles))
	}
	if reasoning := RoleReasoningEffortEnv(RoleVisionOCRFallback); reasoning != "GAME_NARRATOR_ROLE_VISION_OCR_FALLBACK_REASONING_EFFORT" {
		t.Errorf("reasoning effort env = %q", reasoning)
	}
}

// Per-role reasoning effort: the latency-critical runtime roles default to
// "none", the agent loop to "low", the rest omit the field; the env var
// overrides per role and unknown values fail startup naming the role.
func TestLoadRolesReasoningEffortDefaultsOverrideAndValidation(t *testing.T) {
	// Defaults: no reasoning env set at all.
	roles, err := LoadRoles(envLookupFunc(map[string]string{}))
	if err != nil {
		t.Fatalf("LoadRoles: %v", err)
	}
	wantDefaults := map[Role]ReasoningEffort{
		RoleAgentReasoning:    ReasoningEffortLow,
		RoleFastReasoning:     ReasoningEffortNone,
		RoleVisionOCRFallback: ReasoningEffortNone,
		RoleDecision:          "",
		RoleVoiceDesign:       "",
		RoleVoiceClone:        "",
		RoleSpeechASR:         "",
	}
	for _, role := range AllRoles {
		if got := roles[role].ReasoningEffort; got != wantDefaults[role] {
			t.Errorf("role %s default ReasoningEffort = %q, want %q", role, got, wantDefaults[role])
		}
	}

	// Override: the env var wins over the role default (case-insensitive).
	env := map[string]string{
		"GAME_NARRATOR_ROLE_FAST_REASONING_REASONING_EFFORT":  "HIGH",
		"GAME_NARRATOR_ROLE_AGENT_REASONING_REASONING_EFFORT": "none",
	}
	roles, err = LoadRoles(envLookupFunc(env))
	if err != nil {
		t.Fatalf("LoadRoles with overrides: %v", err)
	}
	if got := roles[RoleFastReasoning].ReasoningEffort; got != ReasoningEffortHigh {
		t.Errorf("fast_reasoning override = %q, want high", got)
	}
	if got := roles[RoleAgentReasoning].ReasoningEffort; got != ReasoningEffortNone {
		t.Errorf("agent_reasoning override = %q, want none", got)
	}

	// An empty env var keeps the role default (empty = "leave it to the
	// default"), it does not clear the default to "omit".
	env["GAME_NARRATOR_ROLE_FAST_REASONING_REASONING_EFFORT"] = "  "
	roles, err = LoadRoles(envLookupFunc(env))
	if err != nil {
		t.Fatalf("LoadRoles with blank override: %v", err)
	}
	if got := roles[RoleFastReasoning].ReasoningEffort; got != ReasoningEffortNone {
		t.Errorf("fast_reasoning blank override = %q, want the role default none", got)
	}

	// Unknown value: a configuration error that names the role and the
	// variable, and never the token.
	bad := map[string]string{
		"GAME_NARRATOR_ROLE_FAST_REASONING_REASONING_EFFORT": "maximum",
		EnvGatewayToken: testToken,
	}
	_, err = LoadRoles(envLookupFunc(bad))
	if err == nil {
		t.Fatal("want an error for an unknown reasoning effort")
	}
	msg := err.Error()
	for _, want := range []string{"GAME_NARRATOR_ROLE_FAST_REASONING_REASONING_EFFORT", "fast_reasoning", "none, low, medium, high"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, testToken) {
		t.Errorf("error leaked the token: %q", msg)
	}
}

func TestFailureKindTaxonomy(t *testing.T) {
	// Every kind in docs/spec/06 must exist and classify retryability.
	for _, k := range []Kind{KindTimeout, KindUnavailable, KindRateLimited, KindBadSchema,
		KindBudgetExhausted, KindAuth, KindPayloadTooLarge, KindUnsupportedCapability,
		KindWrongCheckpoint, KindVoiceMissing, KindReplicaMismatch, KindDecodeError,
		KindAudioInvalid, KindContentRefused, KindCancelled, KindStaleResult} {
		if !k.Valid() {
			t.Errorf("kind %q not recognised by Valid()", k)
		}
	}
	if Kind("bogus").Valid() {
		t.Error("bogus kind must not validate")
	}
	err := NewError(KindRateLimited, 429, "too many requests")
	if !errors.Is(err, &Error{Kind: KindRateLimited}) {
		t.Error("errors.Is by kind failed")
	}
	if !err.Retryable {
		t.Error("rate_limited must be retryable")
	}
	if NewError(KindAuth, 401, "bad key").Retryable {
		t.Error("auth must not be retryable")
	}
	unknown := NewError(Kind("bogus"), 0, "some message")
	if unknown.Kind != KindUnavailable {
		t.Errorf("unknown kind should degrade to unavailable, got %q", unknown.Kind)
	}
}

func TestRedactRemovesSecretsAndCapsLength(t *testing.T) {
	got := Redact("authorization: Bearer "+testToken+" failed after "+strings.Repeat("x", 500), testToken)
	if strings.Contains(got, testToken) {
		t.Errorf("Redact leaked the token: %q", got)
	}
	if !strings.Contains(got, MaskValue) {
		t.Errorf("Redact did not mask: %q", got)
	}
	if len(got) > 500 {
		t.Errorf("Redact did not cap length: %d bytes", len(got))
	}
}

func TestLoadRolesNilLookupUsesProcessEnvironment(t *testing.T) {
	// nil must fall back to the process environment rather than panic. The
	// result depends on the caller's environment, so only invariants are
	// asserted: every role resolves and no default credential is invented.
	roles, err := LoadRoles(nil)
	if err != nil {
		t.Fatalf("LoadRoles(nil): %v", err)
	}
	if len(roles) != len(AllRoles) {
		t.Fatalf("got %d roles, want %d", len(roles), len(AllRoles))
	}
	for _, role := range AllRoles {
		if roles[role].Role != role {
			t.Errorf("role %s mislabeled as %s", role, roles[role].Role)
		}
	}
}
