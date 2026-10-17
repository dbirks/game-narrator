// Package router is the single path every model caller in the app takes
// (docs/spec/06 "Routing, reliability and replicas" and "Interface
// requirements": ModelRouter.Resolve, bounded retry, per-role circuit
// breakers; docs/spec/01 "Parallelism and deadline contracts": per-call
// circuit breaker plus returned-generation check; "Runtime and setup
// isolation": live callers take priority over background work).
//
// It is protocol-agnostic: it wraps any call fn returning *providers.Error,
// so the openai, systemone and tts adapters (and ASR later) are routed
// identically. Per role it maintains:
//
//   - a circuit breaker that opens after N consecutive transient failures
//     (timeout, unavailable, rate_limited) and fails fast for a cooldown;
//     permanent kinds are returned immediately and never trip it;
//   - bounded retries, only for idempotent calls with retryable errors,
//     exponential backoff with jitter, honouring Retry-After;
//   - a returned-generation guard: before each retry and before returning a
//     success, CallOptions.Generation is consulted and a result that is no
//     longer current is dropped as stale_result;
//   - a per-role concurrency limit with Live-before-Background priority and
//     a global background pause for live-session isolation.
//
// The router never inspects or logs request content: health metrics are
// keyed by role only, never by prompt text or request id (docs/spec/01 OTel
// note).
package router

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Priority orders callers for a per-role concurrency slot.
type Priority int

// The two priorities (docs/spec/06 "background generation receives lower
// priority than runtime"). The zero value is Live: an unlabelled call is
// assumed to be on the interactive path.
const (
	// PriorityLive is the interactive runtime path (decisions, VLM fallback,
	// on-demand TTS). It acquires slots before queued Background callers.
	PriorityLive Priority = iota
	// PriorityBackground is research and bulk pre-generation; it is lower
	// priority and pausable (docs/spec/01 "Runtime and setup isolation").
	PriorityBackground
)

// Capability is a declared, probed model capability (docs/spec/06 "Model-
// serving version and health negotiation").
type Capability string

// The capability vocabulary from docs/spec/06.
const (
	CapabilityChat      Capability = "chat"
	CapabilityVision    Capability = "vision"
	CapabilityJSONMode  Capability = "json_mode"
	CapabilityTools     Capability = "tools"
	CapabilitySystemOne Capability = "systemone"
	CapabilityTTSDesign Capability = "tts_design"
	CapabilityTTSClone  Capability = "tts_clone"
	CapabilityASR       Capability = "asr"
)

// Affinity is a sticky-routing hint. It is reserved for sticky cloned-voice
// routing (docs/spec/06 "Routing, reliability and replicas"); today there is
// one target per role, so Resolve records it and LastAffinity reports it,
// but nothing switches on it yet.
type Affinity struct {
	// Key identifies the sticky session (for example a voice revision
	// digest).
	Key string
}

// Target is the resolved route for one call. BaseURL is a label for logs and
// the developer UI only; the router never re-dials or rewrites it.
type Target struct {
	Role    providers.Role
	RouteID string
	// BaseURL is the serving endpoint's log-safe label.
	BaseURL string
	// Model is the model id the target serves.
	Model string
	// Health is the role's health state at resolution time.
	Health HealthState
}

// HealthState is a role's routing health. The app publishes provider.degraded
// events from the OnStateChange callback (docs/spec/13 resilience matrix).
type HealthState string

// The four reportable states (docs/spec/06 "Model-serving version and health
// negotiation"). A half-open role — cooldown elapsed, one probe in flight —
// reports degraded, not open: it is recovering, not refusing calls.
const (
	StateOK            HealthState = "ok"
	StateDegraded      HealthState = "degraded"
	StateOpen          HealthState = "open"
	StateNotConfigured HealthState = "not_configured"
)

// RoleHealth is a snapshot of one role's router state. Every field is keyed
// by role only — never by prompt text, request id or any other free-form
// value (docs/spec/01 OTel note).
type RoleHealth struct {
	Role providers.Role
	// State is ok, degraded, open or not_configured.
	State HealthState
	// ConsecutiveFailures is the in-window transient-failure streak driving
	// the breaker. Permanent failures do not count.
	ConsecutiveFailures int
	// LastError is the taxonomy kind of the most recent failure ("" if none).
	LastError     providers.Kind
	LastErrorAt   time.Time
	LastSuccessAt time.Time
	// P50 and P95 are over the ring of the last LatencyRingSize attempt
	// latencies (successes and failures alike).
	P50 time.Duration
	P95 time.Duration
	// Calls counts attempts sent to the provider. Failures counts failed
	// attempts. Retries counts retries performed (attempts beyond the
	// first). StaleDropped counts results the generation guard dropped.
	Calls        int64
	Failures     int64
	Retries      int64
	StaleDropped int64
}

// jitterSource makes the jitter draw goroutine-safe: math/rand's *Rand is
// not thread-safe, and Do is called concurrently for the same role (and
// across roles) by the runtime.
type jitterSource struct {
	mu  sync.Mutex
	rnd *rand.Rand
}

func (j *jitterSource) float64() float64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.rnd.Float64()
}

// Router routes calls for every role in a providers.Registry.
type Router struct {
	reg         *providers.Registry
	now         func() time.Time
	sleep       func(ctx context.Context, d time.Duration) error
	jitter      *jitterSource
	threshold   int
	window      time.Duration
	cooldown    time.Duration
	maxAttempts int
	retryBase   time.Duration
	retryCap    time.Duration
	onState     func(role providers.Role, from, to HealthState)

	roles map[providers.Role]*roleState
}

// roleState is the per-role mutable router state. The mutex guards the health
// counters, the origin label, the affinity record and the previous state used
// for transition detection; the breaker, slots and ring carry their own
// locks and are never locked while holding this one.
type roleState struct {
	cfg   providers.RoleConfig
	caps  []Capability // nil = permissive (nothing declared)
	brk   *breaker
	slots *slot
	ring  *latRing

	mu            sync.Mutex
	prev          HealthState
	lastError     providers.Kind
	lastErrorAt   time.Time
	lastSuccessAt time.Time
	calls         int64
	failures      int64
	retries       int64
	staleDropped  int64
	origin        string
	affinity      Affinity
}

// New builds a Router over reg. Every role present in the registry gets a
// breaker, a concurrency slot set and a latency ring; roles that are not
// configured report not_configured and are refused by Resolve and Do.
func New(reg *providers.Registry, opts Options) *Router {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = realSleep
	}
	randSrc := opts.Rand
	if randSrc == nil {
		randSrc = rand.New(rand.NewSource(now().UnixNano()))
	}
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	retryBase := opts.RetryBase
	if retryBase <= 0 {
		retryBase = DefaultRetryBase
	}
	retryCap := opts.RetryCap
	if retryCap <= 0 {
		retryCap = DefaultRetryCap
	}
	r := &Router{
		reg:         reg,
		now:         now,
		sleep:       sleep,
		jitter:      &jitterSource{rnd: randSrc},
		threshold:   opts.BreakerThreshold,
		window:      opts.BreakerWindow,
		cooldown:    opts.BreakerCooldown,
		maxAttempts: maxAttempts,
		retryBase:   retryBase,
		retryCap:    retryCap,
		onState:     opts.OnStateChange,
		roles:       make(map[providers.Role]*roleState),
	}
	for _, role := range providers.AllRoles {
		cfg, ok := reg.Config(role)
		if !ok {
			continue
		}
		var caps []Capability
		caps = opts.Capabilities[role]
		capacity := opts.Concurrency[role]
		if capacity <= 0 {
			capacity = DefaultConcurrency
		}
		rs := &roleState{
			cfg:   cfg,
			caps:  caps,
			brk:   newBreaker(r.threshold, r.window, r.cooldown, now),
			slots: newSlot(capacity),
			ring:  newLatRing(opts.LatencyRingSize),
		}
		rs.prev = rs.stateAt(now())
		r.roles[role] = rs
	}
	return r
}

// Resolve is the read-only half of routing (docs/spec/06:
// ModelRouter.Resolve(role, affinity, capabilities) -> target, route_id,
// health_state). It records the affinity for the role and returns the single
// configured target. It fails with unsupported_capability when the role is
// unknown or not configured, or when a requested capability is not declared
// for the role.
func (r *Router) Resolve(role providers.Role, aff Affinity, caps []Capability) (Target, error) {
	rs, ok := r.roles[role]
	if !ok || !rs.cfg.Configured() {
		e := providers.NewError(providers.KindUnsupportedCapability, 0,
			fmt.Sprintf("role %s is not configured: no route exists", role))
		e.Role = role
		return Target{}, e
	}
	if rs.caps != nil {
		for _, c := range caps {
			if !declaresCap(rs.caps, c) {
				e := providers.NewError(providers.KindUnsupportedCapability, 0,
					fmt.Sprintf("role %s does not declare capability %q", role, c))
				e.Role = role
				e.Model = rs.cfg.Model
				return Target{}, e
			}
		}
	}
	rs.setAffinity(aff)
	return Target{
		Role:    role,
		RouteID: routeID(rs.cfg),
		BaseURL: rs.cfg.BaseURL,
		Model:   rs.cfg.Model,
		Health:  rs.stateAt(r.now()),
	}, nil
}

// routeID is a stable, log-safe identifier for the (role, model) route so the
// developer UI can correlate calls without a fresh id per resolution.
func routeID(cfg providers.RoleConfig) string {
	if cfg.Model != "" {
		return string(cfg.Role) + "/" + cfg.Model
	}
	return string(cfg.Role)
}

func declaresCap(caps []Capability, c Capability) bool {
	for _, d := range caps {
		if d == c {
			return true
		}
	}
	return false
}
