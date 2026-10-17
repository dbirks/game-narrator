package doctor

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
)

// decisionProbeTimeout is the budget for the System One probe. A warm call is
// ~0.5 s, but a cold gateway queue straight after a GPU reset has been
// observed at 13 s (ADR-0021), so the probe gets far more than the runtime's
// 1500 ms decision budget (docs/spec/06 "Timeout and cancellation defaults")
// while still bounding a wedged endpoint. The runtime loop keeps its 1500 ms
// deadline and simply skips Clef on timeout; the diagnostic never does.
const decisionProbeTimeout = 20 * time.Second

// DecisionProbe is the slice of the System One client the doctor needs.
// Injecting it keeps the decision check testable without a gateway, mirroring
// ProviderProbe for the chat roles.
type DecisionProbe interface {
	Decide(ctx context.Context, cfg providers.RoleConfig, req systemone.Request) (systemone.Decision, error)
}

// systemoneProbe is the real DecisionProbe: it builds the same adapter the
// runtime uses and sends a real request.
type systemoneProbe struct{}

// decisionProbeRequest is the probe: one tiny choice question. The content of
// the answer is irrelevant to the check — a valid, schema-conformant response
// proves the route serves System One at all; what it actually classifies is
// the runtime's calibration problem, not a preflight one.
var decisionProbeRequest = systemone.Request{
	State: map[string]any{"probe": "doctor"},
	Questions: []systemone.Question{{
		ID:           "probe",
		Type:         systemone.TypeChoice,
		Instructions: "Choose exactly one option to confirm this endpoint answers.",
		Criteria: []systemone.Option{
			{ID: "ok", Description: "the endpoint is usable"},
			{ID: "bad", Description: "the endpoint is not usable"},
		},
	}},
}

func (systemoneProbe) Decide(ctx context.Context, cfg providers.RoleConfig, req systemone.Request) (systemone.Decision, error) {
	client, err := systemone.NewFromRole(cfg)
	if err != nil {
		return systemone.Decision{}, err
	}
	// Remove the role's runtime latency budget as a transport timeout: 1500 ms
	// bounds an in-loop observation, not a diagnostic a user runs deliberately,
	// where a cold model load is normal. The check's own context deadline
	// (decisionProbeTimeout, clamped by doctor.Options.ModelTimeout) bounds the
	// probe instead.
	transport := http.Client{}
	if client.HTTP != nil {
		transport = *client.HTTP
	}
	transport.Timeout = 0
	client.HTTP = &transport
	return client.Decide(ctx, req)
}

// checkDecisionRole runs the real System One probe for the decision role:
// OK with the measured latency when the endpoint answers a valid decision,
// FAIL with the taxonomy kind otherwise, NOT CONFIGURED when the role has no
// model (handled by the generic branch in checkRole).
func (r *runner) checkDecisionRole(ctx context.Context, role providers.Role, cfg providers.RoleConfig) Check {
	probe := r.opts.DecisionProbe
	if probe == nil {
		probe = systemoneProbe{}
	}
	ctx, cancel := context.WithTimeout(ctx, decisionProbeTimeout)
	defer cancel()
	res, err := probe.Decide(ctx, cfg, decisionProbeRequest)
	if err != nil {
		return roleFailure(role, cfg, "decision probe", err)
	}
	choice := "?"
	for _, a := range res.Answers {
		if a.Type == systemone.TypeChoice && a.Choice != "" {
			choice = a.Choice
			break
		}
	}
	summary := fmt.Sprintf("model %s answered the decision probe in %s", res.Model, res.Latency.Round(time.Millisecond))
	detail := fmt.Sprintf("probe answer %q; %d questions answered, usage %d/%d; probabilities are raw signals, not reliability guarantees (docs/spec/06)",
		choice, len(res.Answers), res.Usage.Input, res.Usage.Output)
	return Check{Status: StatusOK, Summary: summary, Detail: detail}
}
