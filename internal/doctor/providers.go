package doctor

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// The two prompts every configured chat role must answer. They are content
// probes, not warm-and-fuzzy health pings: a gateway that answers /models but
// cannot complete a turn is not usable (docs/spec/13 "no generic success on an
// unprobed service").
const (
	chatPrompt   = "Reply with exactly: pong"
	visionPrompt = "Read the text in this image. Reply with exactly the text you see and nothing else."
	pongToken    = "pong"
)

// checkRoles reports one row per role in docs/spec/06 declaration order, so a
// seven-row table always has seven rows and an operator can diff two reports.
func (r *runner) checkRoles(ctx context.Context) {
	roles := r.opts.Roles
	if roles == nil {
		loaded, err := providers.LoadRoles(r.opts.LookupEnv)
		if err != nil {
			// A malformed GAME_NARRATOR_* value is a configuration fault that
			// would otherwise hide all seven role rows behind silence.
			r.add(ctx, "providers.config", r.opts.Timeout, func(context.Context) Check {
				return Check{
					Status:  StatusFail,
					Summary: "model role configuration is invalid",
					Detail:  safeText(err.Error(), ""),
					Remedy:  "fix the GAME_NARRATOR_* variables (see .env.example); URLs and tokens are never echoed here",
				}
			})
			return
		}
		roles = loaded
	}

	for _, role := range providers.AllRoles {
		role, cfg := role, roles[role]
		budget := r.opts.ModelTimeout
		if role == providers.RoleDecision && decisionProbeTimeout < budget {
			// The System One probe gets its own (shorter) budget: a cold gateway
			// can take ~13 s (ADR-0021), but a wedged one must not hold the
			// report for the full model timeout.
			budget = decisionProbeTimeout
		}
		r.add(ctx, "providers."+string(role), budget, func(ctx context.Context) Check {
			return r.checkRole(ctx, role, cfg)
		})
	}
}

func (r *runner) checkRole(ctx context.Context, role providers.Role, cfg providers.RoleConfig) Check {
	if !cfg.Configured() {
		modelEnv, _, _, _ := providers.RoleEnvVars(role)
		return Check{
			Status:  StatusNotConfigured,
			Summary: fmt.Sprintf("role %s has no model configured", role),
			Detail:  fmt.Sprintf("protocol %s, request budget %s", cfg.Protocol, cfg.Timeout),
			Remedy:  fmt.Sprintf("set %s to a deployed model id, then re-run doctor", modelEnv),
		}
	}
	if role == providers.RoleDecision {
		// The decision role speaks System One, not OpenAI chat: it gets the
		// real decision probe instead of the chat/vision probe path below.
		return r.checkDecisionRole(ctx, role, cfg)
	}
	if role == providers.RoleVoiceDesign || role == providers.RoleVoiceClone {
		// The voice roles speak vLLM-Omni speech, not OpenAI chat: they get the
		// real TTS probes (ADR-0022) instead of the chat/vision probe path below.
		return r.checkVoiceRole(ctx, role, cfg)
	}
	if !providers.ProtocolImplemented(cfg.Protocol) {
		// A model id alone is not proof of a working route: the ASR client lands
		// in M4, and claiming OK now would be the simulation theatre
		// docs/spec/12 forbids.
		return Check{
			Status:  StatusDegraded,
			Summary: "client not implemented yet (M4)",
			Detail: fmt.Sprintf("role %s is configured with model %s over protocol %q, which has no adapter in this build",
				role, cfg.Model, cfg.Protocol),
			Remedy: "the endpoint is not deployed in M0 (docs/spec/16); no request was sent, so nothing is proven",
		}
	}

	probe := r.opts.Probe
	if probe == nil {
		probe = openAIProbe{}
	}

	models, err := probe.ListModels(ctx, cfg)
	if err != nil {
		return roleFailure(role, cfg, "model listing", err)
	}
	if !containsString(models, cfg.Model) {
		return Check{
			Status: StatusFail,
			Summary: fmt.Sprintf("%s: model %s is not in the gateway model list (%d models offered)",
				providers.KindUnsupportedCapability, cfg.Model, len(models)),
			Detail: "the route answered, but not with this model; the model list itself is not reproduced",
			Remedy: fmt.Sprintf("correct %s or deploy the model", roleModelEnv(role)),
		}
	}

	res, err := probe.Chat(ctx, cfg, chatPrompt, nil)
	if err != nil {
		return roleFailure(role, cfg, "chat probe", err)
	}
	if !strings.Contains(strings.ToLower(normalizeText([]byte(res.Text))), pongToken) {
		return Check{
			Status:  StatusFail,
			Summary: fmt.Sprintf("%s: role %s did not answer the chat probe with pong", providers.KindBadSchema, role),
			Detail:  safeText("answered: "+res.Text, cfg.Token()),
			Remedy:  "the route completes but not as a chat model; check which checkpoint the alias points at",
		}
	}

	chatLatency := res.Latency.Round(time.Millisecond)
	summary := fmt.Sprintf("model %s answered the chat probe in %s", cfg.Model, chatLatency)
	detail := fmt.Sprintf("%d models offered by the route; pong after %s", len(models), chatLatency)

	if cfg.Protocol == providers.ProtocolOpenAIChatVision {
		vres, err := probe.Chat(ctx, cfg, visionPrompt, [][]byte{r.ocrImage()})
		if err != nil {
			return roleFailure(role, cfg, "vision probe", err)
		}
		got := normalizeText([]byte(vres.Text))
		if !strings.Contains(got, controlledText) {
			return Check{
				Status:  StatusFail,
				Summary: fmt.Sprintf("%s: role %s vision probe did not recover %q", providers.KindBadSchema, role, controlledText),
				Detail:  safeText("answered: "+res.Text+" / vision answered: "+vres.Text, cfg.Token()),
				Remedy:  "the route may not accept images; confirm the alias serves a multimodal checkpoint",
			}
		}
		visionLatency := vres.Latency.Round(time.Millisecond)
		summary = fmt.Sprintf("model %s answered the chat probe in %s and read %q in %s",
			cfg.Model, chatLatency, controlledText, visionLatency)
		detail += fmt.Sprintf("; vision request answered in %s", visionLatency)
	}

	// Optional capability findings (json_mode, tool_choice_required) from
	// docs/spec/06 "Model-serving version and health negotiation": a
	// diagnostic, not a hard requirement — the app degrades from a rejected
	// capability (JSON-mode fallback, tool_choice auto). A probe error (client
	// construction) simply omits the findings; it never fails the role.
	if caps, err := probe.ProbeCapabilities(ctx, cfg); err == nil {
		detail += fmt.Sprintf("; json_mode=%s tool_choice_required=%s", caps.JSONMode, caps.ToolChoiceRequired)
	}

	return Check{Status: StatusOK, Summary: summary, Detail: detail}
}

// roleFailure maps any probe error onto FAIL with its taxonomy kind in the
// summary, so an operator sees "auth" or "unavailable" rather than a stack of
// provider prose.
func roleFailure(role providers.Role, cfg providers.RoleConfig, stage string, err error) Check {
	kind := providers.KindUnavailable
	var pe *providers.Error
	if errors.As(err, &pe) && pe.Kind != "" {
		kind = pe.Kind
	}
	return Check{
		Status:  StatusFail,
		Summary: fmt.Sprintf("%s: %s failed for role %s", kind, stage, role),
		Detail:  safeText(err.Error(), cfg.Token()),
		Remedy: fmt.Sprintf("re-check %s and the token named by %s, then re-run doctor",
			roleModelEnv(role), tokenEnvName(cfg)),
	}
}

func roleModelEnv(role providers.Role) string {
	modelEnv, _, _, _ := providers.RoleEnvVars(role)
	return modelEnv
}

// tokenEnvName returns the *name* of the variable holding the credential, never
// its value.
func tokenEnvName(cfg providers.RoleConfig) string {
	if strings.TrimSpace(cfg.TokenEnv) == "" {
		return providers.EnvGatewayToken
	}
	return cfg.TokenEnv
}

// endpointPattern matches any absolute URL so a provider error that echoes its
// own endpoint (url.Error does) cannot leak a gateway address into a report.
var endpointPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^\s,)'"]+`)

// safeText redacts a credential, strips endpoints and caps the length. Every
// Detail line built from provider or tool output goes through it.
func safeText(s, secret string) string {
	s = providers.Redact(s, secret)
	s = endpointPattern.ReplaceAllString(s, "<endpoint>")
	return oneLine(s, 300)
}
