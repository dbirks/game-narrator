package router

import (
	"context"
	"errors"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Helpers shared by the unit tests: a registry with the given roles
// configured, a fake clock, and a fake provider fn that returns scripted
// per-attempt errors.

func testRegistry(configured ...providers.Role) *providers.Registry {
	m := make(map[providers.Role]providers.RoleConfig, len(providers.AllRoles))
	for _, role := range providers.AllRoles {
		cfg := providers.RoleConfig{
			Role:     role,
			Protocol: providers.ProtocolOpenAIChat,
			Timeout:  5 * time.Second,
		}
		for _, c := range configured {
			if role == c {
				cfg.BaseURL = "http://example.invalid/v1"
				cfg.Model = "test-model"
			}
		}
		m[role] = cfg
	}
	return providers.NewRegistry(m)
}

type fakeClock struct {
	t time.Time
}

func (c *fakeClock) now() time.Time       { return c.t }
func (c *fakeClock) step(d time.Duration) { c.t = c.t.Add(d) }

// harness builds a Router on a fake clock and records backoff sleeps.
type harness struct {
	r     *Router
	clock *fakeClock

	mu     sync.Mutex
	sleeps []time.Duration
}

func newHarness(t *testing.T, opts Options, configured ...providers.Role) *harness {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	h := &harness{clock: clock}
	if opts.Now == nil {
		opts.Now = clock.now
	}
	if opts.Sleep == nil {
		opts.Sleep = func(_ context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			h.mu.Unlock()
			return nil
		}
	}
	if opts.Rand == nil {
		opts.Rand = rand.New(rand.NewSource(1))
	}
	h.r = New(testRegistry(configured...), opts)
	return h
}

// sleepsCopy returns the recorded backoff delays. Call it after the Do
// calls it guards have returned.
func (h *harness) sleepsCopy() []time.Duration {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]time.Duration, len(h.sleeps))
	copy(out, h.sleeps)
	return out
}

// fakeCalls scripts per-attempt errors; a nil slot succeeds. The mutex
// makes it safe for the concurrent-callers test.
type fakeCalls struct {
	mu     sync.Mutex
	script []error
	calls  int
}

func (f *fakeCalls) fn() func(context.Context, Target) (string, error) {
	return func(_ context.Context, _ Target) (string, error) {
		f.mu.Lock()
		n := f.calls
		f.calls++
		f.mu.Unlock()
		if n < len(f.script) && f.script[n] != nil {
			return "", f.script[n]
		}
		return "ok", nil
	}
}

func transientErr() *providers.Error {
	return providers.NewError(providers.KindUnavailable, 503, "endpoint down")
}
func rateLimitErr() *providers.Error {
	return providers.NewError(providers.KindRateLimited, 429, "slow down")
}
func authErr() *providers.Error   { return providers.NewError(providers.KindAuth, 401, "bad token") }
func schemaErr() *providers.Error { return providers.NewError(providers.KindBadSchema, 400, "schema") }

func isKind(t *testing.T, err error, kind providers.Kind) {
	t.Helper()
	var pe *providers.Error
	if !errors.As(err, &pe) {
		t.Fatalf("expected *providers.Error, got %v", err)
	}
	if pe.Kind != kind {
		t.Fatalf("expected kind %s, got %s (%v)", kind, pe.Kind, err)
	}
}

const fast = providers.RoleFastReasoning

func TestResolveReturnsConfiguredTarget(t *testing.T) {
	h := newHarness(t, Options{}, fast)
	target, err := h.r.Resolve(fast, Affinity{Key: "voice-42"}, nil)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if target.Role != fast || target.Model != "test-model" || target.RouteID != "fast_reasoning/test-model" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if target.BaseURL != "http://example.invalid/v1" {
		t.Fatalf("BaseURL label = %q", target.BaseURL)
	}
	if target.Health != StateOK {
		t.Fatalf("health = %s, want ok", target.Health)
	}
	aff, ok := h.r.LastAffinity(fast)
	if !ok || aff.Key != "voice-42" {
		t.Fatalf("affinity not recorded: %v (ok=%v)", aff, ok)
	}
}

func TestResolveUnconfiguredRoleIsUnsupported(t *testing.T) {
	h := newHarness(t, Options{})
	_, err := h.r.Resolve(providers.RoleDecision, Affinity{}, nil)
	if err == nil {
		t.Fatal("expected error for unconfigured role")
	}
	isKind(t, err, providers.KindUnsupportedCapability)
	var pe *providers.Error
	errors.As(err, &pe)
	if pe.Role != providers.RoleDecision {
		t.Fatalf("error Role = %q", pe.Role)
	}
}

func TestResolveCapabilityGate(t *testing.T) {
	h := newHarness(t, Options{
		// fast declares chat+json_mode; decision is unconfigured; vision has
		// no entry (permissive, nothing declared).
		Capabilities: map[providers.Role][]Capability{
			fast: {CapabilityChat, CapabilityJSONMode},
		},
	}, fast, providers.RoleVisionOCRFallback)

	if _, err := h.r.Resolve(fast, Affinity{}, []Capability{CapabilityChat, CapabilityJSONMode}); err != nil {
		t.Fatalf("declared capabilities rejected: %v", err)
	}
	if _, err := h.r.Resolve(fast, Affinity{}, []Capability{CapabilityTools}); err == nil {
		t.Fatal("undeclared capability accepted")
	} else {
		isKind(t, err, providers.KindUnsupportedCapability)
	}
	// No declaration for the role => permissive.
	if _, err := h.r.Resolve(providers.RoleVisionOCRFallback, Affinity{}, []Capability{CapabilityChat}); err != nil {
		t.Fatalf("permissive role rejected a capability: %v", err)
	}
}

func TestDoRefusesUnconfiguredRole(t *testing.T) {
	h := newHarness(t, Options{})
	calls := 0
	_, err := Do(context.Background(), h.r, providers.RoleSpeechASR, CallOptions{}, func(context.Context, Target) (string, error) {
		calls++
		return "", nil
	})
	isKind(t, err, providers.KindUnsupportedCapability)
	if calls != 0 {
		t.Fatalf("fn ran %d times for an unroutable role", calls)
	}
}

func TestHealthSnapshotCountersAndPercentiles(t *testing.T) {
	h := newHarness(t, Options{}, fast)
	// Drive: 1 failure, 1 retry success, 1 stale drop on success.
	gen := true
	var f *fakeCalls
	f = &fakeCalls{script: []error{transientErr(), nil}}
	_, err := Do(context.Background(), h.r, fast, CallOptions{Idempotent: true, Generation: func() bool { return gen }}, f.fn())
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	gen = false
	if _, err := Do(context.Background(), h.r, fast, CallOptions{Generation: func() bool { return gen }}, f.fn()); err == nil {
		t.Fatal("expected stale_result when the generation is no longer current")
	} else {
		isKind(t, err, providers.KindStaleResult)
	}

	rs := h.r.roles[fast]
	rs.ring.add(10 * time.Millisecond)
	rs.ring.add(20 * time.Millisecond)
	rs.ring.add(30 * time.Millisecond)
	rs.ring.add(40 * time.Millisecond)
	rs.ring.add(50 * time.Millisecond)
	// The two real Do attempts also recorded latencies; use the ring directly
	// for deterministic percentile input by draining and re-adding below is
	// not possible, so assert the percentiles against the known superset:
	// with n>=5 known samples the nearest-rank P95 must be >= 40ms and P50
	// must be >= 10ms (attempts' own latencies are >= 0 and small).
	ph := h.r.Health()[fast]
	if ph.State != StateOK {
		t.Fatalf("state = %s, want ok after recovery", ph.State)
	}
	if ph.Calls != 3 { // 1 fail + 1 success (1 call) + 1 stale-success call
		t.Fatalf("Calls = %d, want 3", ph.Calls)
	}
	if ph.Failures != 1 {
		t.Fatalf("Failures = %d, want 1", ph.Failures)
	}
	if ph.Retries != 1 {
		t.Fatalf("Retries = %d, want 1", ph.Retries)
	}
	if ph.StaleDropped != 1 {
		t.Fatalf("StaleDropped = %d, want 1", ph.StaleDropped)
	}
	if ph.LastError != providers.KindUnavailable {
		t.Fatalf("LastError = %s", ph.LastError)
	}
	if ph.P50 < time.Millisecond || ph.P95 < 40*time.Millisecond {
		t.Fatalf("P50=%s P95=%s: ring percentiles not sane", ph.P50, ph.P95)
	}
	if _, ok := h.r.Health()[providers.RoleDecision]; !ok {
		t.Fatal("Health must cover unconfigured roles too")
	}
	if h.r.Health()[providers.RoleDecision].State != StateNotConfigured {
		t.Fatal("unconfigured role must report not_configured")
	}
}

func TestBreakerWindowPrunesOldFailures(t *testing.T) {
	h := newHarness(t, Options{BreakerThreshold: 3, BreakerWindow: 10 * time.Second, BreakerCooldown: 30 * time.Second}, fast)
	f := &fakeCalls{script: []error{transientErr(), transientErr(), transientErr(), transientErr()}}
	// Three failures 12s apart with a 10s window: at the third failure only
	// two are still in-window, so the breaker stays closed.
	for i := 0; i < 3; i++ {
		if i > 0 {
			h.clock.step(6 * time.Second)
		}
		if _, err := Do(context.Background(), h.r, fast, CallOptions{}, f.fn()); err == nil {
			t.Fatalf("attempt %d: want failure", i)
		}
	}
	if h.r.Health()[fast].State != StateDegraded {
		t.Fatalf("state = %s, want degraded (streak kept, breaker closed)", h.r.Health()[fast].State)
	}
	// A fourth immediate failure is the third in-window one: breaker opens.
	if _, err := Do(context.Background(), h.r, fast, CallOptions{}, f.fn()); err == nil {
		t.Fatal("want fourth failure")
	}
	if st := h.r.Health()[fast].State; st != StateOpen {
		t.Fatalf("state = %s, want open", st)
	}
	// The next call fails fast without reaching the provider.
	before := f.calls
	_, err := Do(context.Background(), h.r, fast, CallOptions{}, f.fn())
	if err == nil {
		t.Fatal("want fail-fast error")
	}
	isKind(t, err, providers.KindUnavailable)
	var pe *providers.Error
	errors.As(err, &pe)
	if !pe.Retryable {
		t.Fatal("fail-fast error must be retryable")
	}
	if f.calls != before {
		t.Fatal("fail-fast must not call the provider")
	}
}

func TestStateChangeCallback(t *testing.T) {
	type change struct{ from, to HealthState }
	var changes []change
	h := newHarness(t, Options{
		BreakerThreshold: 2,
		BreakerCooldown:  30 * time.Second,
		OnStateChange: func(_ providers.Role, from, to HealthState) {
			changes = append(changes, change{from, to})
		},
	}, fast)
	f := &fakeCalls{script: []error{transientErr(), transientErr(), nil}}
	// Failure 1: ok -> degraded. Failure 2: degraded -> open.
	Do(context.Background(), h.r, fast, CallOptions{}, f.fn())
	Do(context.Background(), h.r, fast, CallOptions{}, f.fn())
	h.clock.step(31 * time.Second)
	Do(context.Background(), h.r, fast, CallOptions{}, f.fn()) // probe succeeds -> ok
	if len(changes) != 3 {
		t.Fatalf("changes = %+v, want 3 transitions", changes)
	}
	want := []change{{StateOK, StateDegraded}, {StateDegraded, StateOpen}, {StateOpen, StateOK}}
	for i, w := range want {
		if changes[i] != w {
			t.Fatalf("change %d = %+v, want %+v", i, changes[i], w)
		}
	}
}
