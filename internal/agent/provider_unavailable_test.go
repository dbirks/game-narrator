package agent

import (
	"errors"
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/providers"
)

// TestIsProviderUnavailable pins the degraded-mode predicate (docs/spec/03,
// ADR-0028): the run is blocked + resumable exactly when the fault is the
// provider's, not the run's. The 404 case is the collective-gateway
// observation of 2026-10-18: the gateway answers 404 not_found_error with
// "model ... is not currently available" while the configured model's
// replica is absent; the same model id succeeds moments later, so the fault
// is transient and the run must not be failed terminally.
func TestIsProviderUnavailable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"unclassified error is permanent", errors.New("boom"), false},
		{"unavailable kind", providers.NewError(providers.KindUnavailable, 503, "endpoint unreachable"), true},
		{"timeout kind", providers.NewError(providers.KindTimeout, 0, "openai: request deadline exceeded"), true},
		{"unsupported capability with no status (local capability gap)",
			providers.NewError(providers.KindUnsupportedCapability, 0, "no chat role configured"), true},
		{"404 unknown model id stays a permanent capability gap",
			providers.NewError(providers.KindUnsupportedCapability, 404,
				`openai: Not Found: not_found_error: model "does-not-exist" does not exist: provider returned http 404 in 131ms (request_id=)`), false},
		{"404 not currently available is the transient serving condition",
			providers.NewError(providers.KindUnsupportedCapability, 404,
				`openai: Not Found: not_found_error: model "qwen3-8-27b" is not currently available: provider returned http 404 in 131ms (request_id=)`), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IsProviderUnavailable(c.err); got != c.want {
				t.Fatalf("IsProviderUnavailable(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestRunTransient404IsResumableBlocked proves the loop's behaviour end to
// end: a round-trip that 404s with the gateway's "not currently available"
// wording stops the attempt as blocked/provider_unavailable (resumable), not
// failed/model_error (terminal). This is the ADR-0028 degraded mode the
// onboarding e2e relies on when the collective gateway briefly drops the
// configured model.
func TestRunTransient404IsResumableBlocked(t *testing.T) {
	_, es := openAgentStore(t)
	reg := testRegistry(t, echoTool(t, newCountingEcho().handler))
	model := &scriptedModel{responses: []scriptedResponse{{
		err: providers.NewError(providers.KindUnsupportedCapability, 404,
			`openai: Not Found: not_found_error: model "qwen3-8-27b" is not currently available: provider returned http 404 in 131ms (request_id=)`),
	}}}
	hub := events.New()
	t.Cleanup(func() { _ = hub.Close() })
	out, err := Run(testCtx(), runOpts("run-transient-404", model, reg, es, hub))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out.State != StateBlocked || out.StopReason != StopReasonProviderUnavailable {
		t.Fatalf("outcome = %+v, want blocked/provider_unavailable", out)
	}
}
