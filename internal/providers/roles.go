// Package providers holds the model-role configuration and the failure
// taxonomy shared by every provider adapter (docs/spec/06 "Provider registry",
// "Failure taxonomy"). Configure roles, not scattered raw URLs: a single
// gateway may route several roles to different machines, and the runtime must
// always be able to answer "which role is served by which endpoint, with what
// timeout, and is it deployed at all?".
package providers

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Role is a logical model capability used by the runtime (docs/spec/06).
type Role string

// The seven roles declared in docs/spec/06 "Provider registry".
const (
	// RoleAgentReasoning drives the onboarding agent's tool-use loop.
	RoleAgentReasoning Role = "agent_reasoning"
	// RoleFastReasoning serves low-latency classification/ranking inside the
	// runtime loop.
	RoleFastReasoning Role = "fast_reasoning"
	// RoleVisionOCRFallback is the multimodal fallback when local OCR cannot
	// resolve a textbox.
	RoleVisionOCRFallback Role = "vision_ocr_fallback"
	// RoleDecision is System One / Clef-Omni. Not deployed in M0: it stays
	// representable in configuration and reports NOT CONFIGURED.
	RoleDecision Role = "decision"
	// RoleVoiceDesign is the vLLM-Omni VoiceDesign checkpoint. Not deployed
	// in M0.
	RoleVoiceDesign Role = "voice_design"
	// RoleVoiceClone is the vLLM-Omni Base checkpoint. Not deployed in M0.
	RoleVoiceClone Role = "voice_clone"
	// RoleSpeechASR is the ASR endpoint the QA loop reads played audio back
	// with (docs/spec/07 "ASR loop", FR-10) and that the voice acceptance
	// tests exercise. It speaks the OpenAI-compatible /audio/transcriptions
	// wire (verified against the deployed qwen3-asr endpoint, ADR 0061) and
	// reports NOT CONFIGURED when no model is set.
	RoleSpeechASR Role = "speech_asr"
)

// ReasoningEffort is the reasoning_effort level an OpenAI-compatible gateway
// honours on chat completions (docs/spec/06 "Provider registry", ADR 0009
// "Reasoning effort" amendment). The empty value means "omit the field": the
// model's own default applies, which on reasoning-capable deployments can
// burn the entire max_tokens budget on thinking (observed live on
// qwen3-8-27b: omitted effort → thousands of reasoning tokens, "none" → 0).
type ReasoningEffort string

// The four levels the gateway accepts. Values are sent to the gateway
// verbatim, so operators must not invent new ones (LoadRoles validates).
const (
	ReasoningEffortNone   ReasoningEffort = "none"
	ReasoningEffortLow    ReasoningEffort = "low"
	ReasoningEffortMedium ReasoningEffort = "medium"
	ReasoningEffortHigh   ReasoningEffort = "high"
)

// ValidReasoningEffort reports whether e is one of the accepted levels.
// The empty value is a legal configuration ("omit the field") but is not an
// accepted level: use it only as "no per-role default", never as something to
// send.
func ValidReasoningEffort(e ReasoningEffort) bool {
	switch e {
	case ReasoningEffortNone, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
		return true
	}
	return false
}

// Protocol names the wire contract an adapter implements.
type Protocol string

// Protocols from the docs/spec/06 role table. Adapters for the OpenAI
// protocols exist in internal/providers/openai; the others are recorded here
// so configuration, preflight and the failure taxonomy can speak about them
// before their clients are implemented.
const (
	ProtocolOpenAIChat       Protocol = "openai_chat"
	ProtocolOpenAIChatVision Protocol = "openai_chat_vision"
	// ProtocolOpenAITranscriptions is the OpenAI-compatible audio
	// transcription wire (POST /v1/audio/transcriptions, multipart) the
	// qwen3-asr checkpoint serves (ADR 0061: the route was probed against
	// the collective gateway 2026-10-18, so the speech_asr role names it
	// directly instead of probing).
	ProtocolOpenAITranscriptions Protocol = "openai_transcriptions"
	ProtocolSystemOne            Protocol = "systemone"
	ProtocolVLLMOmniSpeech       Protocol = "vllm_omni_speech"
	ProtocolProbeRequired        Protocol = "probe_required"
)

// Provider protocols that have an adapter implemented in this build.
var implementedProtocols = map[Protocol]bool{
	ProtocolOpenAIChat:           true,
	ProtocolOpenAIChatVision:     true,
	ProtocolOpenAITranscriptions: true,
}

// Environment variable names. A shared gateway is configured once; individual
// roles override it only when they really need a dedicated endpoint.
// GAME_NARRATOR_GATEWAY_TOKEN_ENV names the variable that *holds* the token,
// so an operator can point the service at an existing secret variable instead
// of copying the secret into a second place.
const (
	EnvGatewayURL          = "GAME_NARRATOR_GATEWAY_URL"
	EnvGatewayTokenEnv     = "GAME_NARRATOR_GATEWAY_TOKEN_ENV"
	EnvGatewayToken        = "GAME_NARRATOR_GATEWAY_TOKEN" // default named by EnvGatewayTokenEnv
	envRolePrefix          = "GAME_NARRATOR_ROLE_"
	envRoleModelSuffix     = "_MODEL"
	envRoleURLSuffix       = "_URL"
	envRoleProtoSuffix     = "_PROTOCOL"
	envRoleTimeoutSufix    = "_TIMEOUT"
	envRoleReasoningSuffix = "_REASONING_EFFORT"
	// MaskValue replaces any credential in strings meant for logs, UI and
	// reports.
	MaskValue = "****"
)

// AllRoles lists every configurable role in declaration order.
var AllRoles = []Role{
	RoleAgentReasoning,
	RoleFastReasoning,
	RoleVisionOCRFallback,
	RoleDecision,
	RoleVoiceDesign,
	RoleVoiceClone,
	RoleSpeechASR,
}

// roleSpec is the static default for a role: protocol and the timeout floor
// from docs/spec/06 "Timeout and cancellation defaults".
type roleSpec struct {
	protocol Protocol
	timeout  time.Duration
	// deployed records whether an adapter exists in this build. Undeployed
	// roles can still be configured (and are reported by preflight), but no
	// client is constructed for them until their ADR lands.
	deployed bool
	// note explains a NOT CONFIGURED state to a human reading preflight.
	note string
	// reasoningEffort is the per-role default for the reasoning_effort request
	// field (empty = omit it, the model's default applies). The defaults are
	// latency-driven (docs/spec/05 timings, ADR 0009 "Reasoning effort"):
	// the runtime hot paths pin "none" so a reasoning model cannot spend the
	// whole output budget thinking inside the 10 s ceiling, and the agent
	// loop pins "low" so one tool call does not cost thousands of reasoning
	// tokens.
	reasoningEffort ReasoningEffort
}

var roleSpecs = map[Role]roleSpec{
	RoleAgentReasoning: {protocol: ProtocolOpenAIChat, timeout: 30 * time.Second, deployed: true,
		reasoningEffort: ReasoningEffortLow},
	RoleFastReasoning: {protocol: ProtocolOpenAIChat, timeout: 10 * time.Second, deployed: true,
		reasoningEffort: ReasoningEffortNone},
	// VLM fallback: "3-10 s" upstream; the 10 s ceiling is the default so a
	// slow gateway answer still lands before the observation is stale.
	RoleVisionOCRFallback: {protocol: ProtocolOpenAIChatVision, timeout: 10 * time.Second, deployed: true,
		reasoningEffort: ReasoningEffortNone},
	RoleDecision: {
		protocol: ProtocolSystemOne, timeout: 1500 * time.Millisecond,
		note: "System One / Clef-Omni endpoint is not deployed yet",
	},
	RoleVoiceDesign: {
		protocol: ProtocolVLLMOmniSpeech, timeout: 120 * time.Second,
		note: "vLLM-Omni VoiceDesign endpoint is not deployed yet",
	},
	RoleVoiceClone: {
		protocol: ProtocolVLLMOmniSpeech, timeout: 120 * time.Second,
		note: "vLLM-Omni Base endpoint is not deployed yet",
	},
	RoleSpeechASR: {
		protocol: ProtocolOpenAITranscriptions, timeout: 30 * time.Second, deployed: true,
		note: "OpenAI-compatible /audio/transcriptions endpoint for the ASR QA loop (ADR 0061)",
	},
}

// RoleConfig is the resolved configuration for one model role.
type RoleConfig struct {
	Role     Role
	Protocol Protocol
	// BaseURL is the gateway (or dedicated endpoint) serving this role. It is
	// derived from the role override env var or the shared gateway.
	BaseURL string
	// Model is the deployed model id. An empty Model means the role is not
	// enabled: Configured() reports false and preflight shows NOT CONFIGURED.
	Model string
	// TokenEnv names the environment variable the bearer token is read from.
	TokenEnv string
	// Timeout bounds a single provider request. The router may shorten it for
	// a specific observation, never extend it silently.
	Timeout time.Duration
	// ReasoningEffort is the per-role default for the OpenAI-compatible
	// adapter's reasoning_effort request field. It is one of the accepted
	// levels for the roles that have a default (fast_reasoning and
	// vision_ocr_fallback: none; agent_reasoning: low) and empty for the
	// others, where empty means "omit the field, the model's default applies".
	ReasoningEffort ReasoningEffort

	// token is the resolved credential. It is kept so adapters can use it
	// without re-reading the environment, and it must never reach a String(),
	// LogValue(), error message, log line or report (AGENTS.md rule 5).
	token string
}

// RoleEnvVars returns the environment variable names that affect a role.
func RoleEnvVars(role Role) (model, urlOverride, protocol, timeout string) {
	p := envRolePrefix + string(roleEnvSegment(role))
	return p + envRoleModelSuffix, p + envRoleURLSuffix, p + envRoleProtoSuffix, p + envRoleTimeoutSufix
}

// RoleReasoningEffortEnv returns the environment variable name that overrides
// a role's reasoning effort (GAME_NARRATOR_ROLE_<ROLE>_REASONING_EFFORT).
func RoleReasoningEffortEnv(role Role) string {
	return envRolePrefix + string(roleEnvSegment(role)) + envRoleReasoningSuffix
}

func roleEnvSegment(role Role) string {
	return strings.ToUpper(strings.ReplaceAll(string(role), "-", "_"))
}

// Configured reports whether the role has a model and can therefore be used.
func (c RoleConfig) Configured() bool { return strings.TrimSpace(c.Model) != "" }

// Deployed reports whether this build contains an adapter for the role's
// protocol.
func (c RoleConfig) Deployed() bool {
	spec, ok := roleSpecs[c.Role]
	return ok && spec.deployed
}

// Token returns the resolved credential. Callers must treat it as a secret:
// pass it to an adapter, never print it.
func (c RoleConfig) Token() string { return c.token }

// Status renders the preflight state of the role, never the credential.
func (c RoleConfig) Status() string {
	if !c.Configured() {
		modelEnv, _, _, _ := RoleEnvVars(c.Role)
		note := roleSpecs[c.Role].note
		if note == "" {
			note = "no model configured"
		}
		return fmt.Sprintf("NOT CONFIGURED: %s (set %s)", note, modelEnv)
	}
	if !c.Deployed() {
		return fmt.Sprintf("NOT AVAILABLE: a model is configured, but protocol %q has no adapter implemented in this build", c.Protocol)
	}
	return "CONFIGURED"
}

// String renders a log-safe description. The credential is always masked.
func (c RoleConfig) String() string {
	model := c.Model
	if model == "" {
		model = `""`
	}
	token := `""`
	if c.token != "" {
		token = MaskValue
	}
	base := c.BaseURL
	if base == "" {
		base = `""`
	}
	effort := string(c.ReasoningEffort)
	if effort == "" {
		effort = "(omit)"
	}
	return fmt.Sprintf("%s protocol=%s model=%s base_url=%s token_env=%s token=%s timeout=%s reasoning_effort=%s state=%s",
		c.Role, c.Protocol, model, base, c.TokenEnv, token, c.Timeout, effort, c.Status())
}

// LogValue implements fmt.Stringer-compatible redaction and the slog
// LogValuer contract, so a RoleConfig can be handed to a logger directly
// without leaking the credential.
func (c RoleConfig) LogValue() any {
	token := ""
	if c.token != "" {
		token = MaskValue
	}
	return map[string]any{
		"role":             string(c.Role),
		"protocol":         string(c.Protocol),
		"model":            c.Model,
		"base_url":         c.BaseURL,
		"token_env":        c.TokenEnv,
		"token":            token,
		"timeout":          c.Timeout.String(),
		"reasoning_effort": string(c.ReasoningEffort),
		"state":            c.Status(),
	}
}

// LoadRoles resolves every role from the environment. lookup is injected so
// tests never have to mutate process state; nil means os.LookupEnv.
//
// Precedence per role: role-specific override env var > shared gateway env var
// > documented default. A role is only enabled when its model is set, which
// keeps undeployed roles (decision, TTS) representable but inert.
func LoadRoles(lookup func(string) (string, bool)) (map[Role]RoleConfig, error) {
	if lookup == nil {
		lookup = envLookup
	}
	gatewayURL, err := loadURL(lookup, EnvGatewayURL, false)
	if err != nil {
		return nil, err
	}
	tokenEnv := EnvGatewayToken
	if v, ok := lookup(EnvGatewayTokenEnv); ok {
		if v = strings.TrimSpace(v); v != "" {
			tokenEnv = v
		}
	}
	token, _ := lookup(tokenEnv)
	token = strings.TrimSpace(token)

	roles := make(map[Role]RoleConfig, len(AllRoles))
	for _, role := range AllRoles {
		spec := roleSpecs[role]
		modelEnv, urlEnv, protoEnv, timeoutEnv := RoleEnvVars(role)
		cfg := RoleConfig{
			Role:            role,
			Protocol:        spec.protocol,
			BaseURL:         gatewayURL,
			TokenEnv:        tokenEnv,
			Timeout:         spec.timeout,
			ReasoningEffort: spec.reasoningEffort,
			token:           token,
		}
		if model, ok := lookup(modelEnv); ok {
			cfg.Model = strings.TrimSpace(model)
		}
		if raw, ok := lookup(urlEnv); ok && strings.TrimSpace(raw) != "" {
			v, err := loadURL(lookup, urlEnv, true)
			if err != nil {
				return nil, err
			}
			cfg.BaseURL = v
			cfg.token = "" // a dedicated endpoint needs its own credential path
		}
		if raw, ok := lookup(protoEnv); ok {
			if p := Protocol(strings.TrimSpace(raw)); p != "" {
				if !validProtocol(p) {
					return nil, fmt.Errorf("invalid %s=%q: unknown protocol for role %s (expected one of %s)",
						protoEnv, strings.TrimSpace(raw), role, strings.Join(knownProtocols(), ", "))
				}
				cfg.Protocol = p
			}
		}
		if raw, ok := lookup(timeoutEnv); ok {
			if strings.TrimSpace(raw) != "" {
				d, err := time.ParseDuration(strings.TrimSpace(raw))
				if err != nil {
					return nil, fmt.Errorf("invalid %s=%q: expected a Go duration such as 1500ms or 30s", timeoutEnv, strings.TrimSpace(raw))
				}
				if d <= 0 {
					return nil, fmt.Errorf("invalid %s=%q: timeout must be greater than zero", timeoutEnv, raw)
				}
				cfg.Timeout = d
			}
		}
		if raw, ok := lookup(RoleReasoningEffortEnv(role)); ok {
			if raw = strings.TrimSpace(raw); raw != "" {
				e := ReasoningEffort(strings.ToLower(raw))
				if !ValidReasoningEffort(e) {
					return nil, fmt.Errorf("invalid %s=%q for role %s: reasoning effort must be one of none, low, medium, high",
						RoleReasoningEffortEnv(role), raw, role)
				}
				cfg.ReasoningEffort = e
			}
		}
		roles[role] = cfg
	}
	return roles, nil
}

func loadURL(lookup func(string) (string, bool), env string, requiredHost bool) (string, error) {
	raw, ok := lookup(env)
	if !ok {
		return "", nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if !requiredHost {
			return "", nil
		}
		return "", fmt.Errorf("invalid %s: value is empty", env)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid %s: %q is not a valid URL", env, raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid %s: scheme %q is not supported, use http or https", env, u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid %s: %q has no host", env, raw)
	}
	if u.User != nil {
		// Credentials in a URL end up in access logs and proxy logs; force the
		// dedicated token env var instead.
		return "", fmt.Errorf("invalid %s: userinfo in the URL is not allowed, use %s for credentials", env, EnvGatewayTokenEnv)
	}
	return strings.TrimRight(raw, "/"), nil
}

// ProtocolImplemented reports whether an adapter exists for a protocol.
func ProtocolImplemented(p Protocol) bool { return implementedProtocols[p] }

func validProtocol(p Protocol) bool {
	for _, known := range knownProtocols() {
		if string(p) == known {
			return true
		}
	}
	return false
}

func knownProtocols() []string {
	out := make([]string, 0, len(roleSpecs))
	seen := map[Protocol]bool{}
	for _, spec := range roleSpecs {
		if !seen[spec.protocol] {
			seen[spec.protocol] = true
			out = append(out, string(spec.protocol))
		}
	}
	for _, p := range []Protocol{ProtocolOpenAIChat, ProtocolOpenAIChatVision, ProtocolSystemOne, ProtocolVLLMOmniSpeech, ProtocolProbeRequired, ProtocolOpenAITranscriptions} {
		if !seen[p] {
			out = append(out, string(p))
		}
	}
	sort.Strings(out)
	return out
}

// Registry is the resolved role table. The runtime and preflight read it
// instead of re-deriving env vars, so every component agrees on one answer.
type Registry struct {
	roles map[Role]RoleConfig
}

// NewRegistry wraps a LoadRoles result.
func NewRegistry(roles map[Role]RoleConfig) *Registry {
	return &Registry{roles: roles}
}

// Config returns the configuration for a role; ok is false for an unknown
// role name.
func (r *Registry) Config(role Role) (RoleConfig, bool) {
	cfg, ok := r.roles[role]
	return cfg, ok
}

// MustConfig returns an unconfigured placeholder rather than failing when a
// role name is unknown, which keeps preflight reporting total.
func (r *Registry) MustConfig(role Role) RoleConfig {
	if cfg, ok := r.Config(role); ok {
		return cfg
	}
	return RoleConfig{Role: role, Protocol: roleSpecs[role].protocol, Timeout: roleSpecs[role].timeout}
}

// Ready lists configured roles that have an adapter in this build, sorted by
// role name.
func (r *Registry) Ready() []Role {
	var out []Role
	for _, cfg := range r.roles {
		if cfg.Configured() && cfg.Deployed() {
			out = append(out, cfg.Role)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Missing lists roles that are not usable, configured or not, sorted by name.
// Preflight shows these with their Status() text; a service with an empty
// Ready() list can still run in cached-only mode (docs/spec/06).
func (r *Registry) Missing() []Role {
	var out []Role
	for _, cfg := range r.roles {
		if !cfg.Configured() || !cfg.Deployed() {
			out = append(out, cfg.Role)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// String renders one line per role in declaration order.
func (r *Registry) String() string {
	lines := make([]string, 0, len(r.roles))
	for _, role := range AllRoles {
		if cfg, ok := r.Config(role); ok {
			lines = append(lines, cfg.String())
		}
	}
	return strings.Join(lines, "\n")
}

// EnvKeyList returns the role env var names for documentation and diagnostics.
func EnvKeyList() []string {
	out := []string{EnvGatewayURL, EnvGatewayTokenEnv, EnvGatewayToken}
	for _, role := range AllRoles {
		model, u, proto, timeout := RoleEnvVars(role)
		out = append(out, model, u, proto, timeout, RoleReasoningEffortEnv(role))
	}
	return out
}
