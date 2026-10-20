package doctor

// ProbeRole is the API surface over the doctor's per-role provider check
// (ADR-0087): the diagnostics run endpoint (internal/api) answers "can this
// role reach its gateway, and how long did it take" by running exactly the
// `providers.<role>` check the CLI's report runs — the same role-config
// loading, the same probe budget (the decision role's shorter clamp, the
// vision role's longer one), the same probe seams and the same failure
// taxonomy. A second, API-owned probe client would be a second spelling of
// the content probe, and a doctor row and a page row that disagree are the
// drift docs/spec/13 writes about.

import (
	"context"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// roleProbeDeadline caps ProbeRole's own context when the caller's is
// unlimited (the diagnostics run executes out-of-band on context.Background).
// The per-probe budgets (roleProbeBudget, the decision and vision clamps) are
// far smaller; this is the outer wall a wedged probe must hit.
const roleProbeDeadline = 30 * time.Minute

// ProbeRole runs the role's doctor check and returns its Check. The error is
// non-nil only when the role name is not one of docs/spec/06's seven or the
// role configuration itself is invalid; a configured-but-broken role is a
// FAIL Check, not an error (the same distinction the report makes).
func ProbeRole(ctx context.Context, o Options, role providers.Role) (Check, error) {
	o = o.withDefaults()

	roles := o.Roles
	if roles == nil {
		loaded, err := providers.LoadRoles(o.LookupEnv)
		if err != nil {
			return Check{}, fmt.Errorf("model role configuration is invalid: %w", err)
		}
		roles = loaded
	}
	known := false
	for _, r := range providers.AllRoles {
		if r == role {
			known = true
			break
		}
	}
	if !known {
		return Check{}, fmt.Errorf("unknown model role %q", role)
	}
	cfg := roles[role]

	// The caller's context may be unlimited (the run executes out-of-band);
	// wall the whole probe so a probe that ignores every deadline cannot run
	// for the process's lifetime.
	if ctx.Done() == nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, roleProbeDeadline)
		defer cancel()
	}

	runner := newRunner(o)
	budget := roleProbeBudget(runner.opts.ModelTimeout, cfg)
	if role == providers.RoleDecision && decisionProbeTimeout > budget {
		budget = decisionProbeTimeout
	}
	if cfg.Protocol == providers.ProtocolOpenAIChatVision && visionProbeTimeout > budget {
		budget = visionProbeTimeout
	}
	// add runs the check under its own deadline and appends exactly one row.
	runner.add(ctx, "providers."+string(role), budget, func(ctx context.Context) Check {
		return runner.checkRole(ctx, role, cfg)
	})
	if len(runner.checks) != 1 {
		return Check{}, fmt.Errorf("the role probe recorded %d checks, want 1", len(runner.checks))
	}
	return runner.checks[0], nil
}
