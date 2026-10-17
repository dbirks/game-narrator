package router

import (
	"context"
	"errors"
	"math/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// TestRetryPolicyMatrix covers "retries only for idempotent + retryable"
// (docs/spec/06) and the bounded attempt count.
func TestRetryPolicyMatrix(t *testing.T) {
	ctx := context.Background()

	t.Run("non-idempotent-fails-fast", func(t *testing.T) {
		h := newHarness(t, Options{}, fast)
		f := &fakeCalls{script: []error{transientErr()}} // always fails
		_, err := Do(ctx, h.r, fast, CallOptions{Idempotent: false}, f.fn())
		isKind(t, err, providers.KindUnavailable)
		if f.calls != 1 {
			t.Fatalf("attempts = %d, want 1", f.calls)
		}
		if len(h.sleepsCopy()) != 0 {
			t.Fatalf("backoffs = %d, want 0", len(h.sleepsCopy()))
		}
	})

	t.Run("idempotent-non-retryable-fails-fast", func(t *testing.T) {
		h := newHarness(t, Options{}, fast)
		f := &fakeCalls{script: []error{schemaErr()}}
		_, err := Do(ctx, h.r, fast, CallOptions{Idempotent: true}, f.fn())
		isKind(t, err, providers.KindBadSchema)
		if f.calls != 1 || len(h.sleepsCopy()) != 0 {
			t.Fatalf("attempts = %d, backoffs = %d, want 1/0", f.calls, len(h.sleepsCopy()))
		}
	})

	t.Run("idempotent-retryable-retries-then-succeeds", func(t *testing.T) {
		h := newHarness(t, Options{}, fast)
		f := &fakeCalls{script: []error{rateLimitErr(), transientErr(), nil}}
		res, err := Do(ctx, h.r, fast, CallOptions{Idempotent: true}, f.fn())
		if err != nil || res != "ok" {
			t.Fatalf("Do = %q, %v; want ok", res, err)
		}
		if f.calls != 3 {
			t.Fatalf("attempts = %d, want 3", f.calls)
		}
		sleeps := h.sleepsCopy()
		if len(sleeps) != 2 {
			t.Fatalf("backoffs = %d, want 2", len(sleeps))
		}
		// Equal-jitter schedule: first backoff in [100ms,200ms), second in
		// [200ms,400ms), and the base doubles between them (no cap reached).
		if sleeps[0] < 100*time.Millisecond || sleeps[0] >= 200*time.Millisecond {
			t.Fatalf("first backoff = %s, want in [100ms,200ms)", sleeps[0])
		}
		if sleeps[1] < 200*time.Millisecond || sleeps[1] >= 400*time.Millisecond {
			t.Fatalf("second backoff = %s, want in [200ms,400ms)", sleeps[1])
		}
	})

	t.Run("retryable-failures-exhaust-attempts", func(t *testing.T) {
		h := newHarness(t, Options{MaxAttempts: 3}, fast)
		calls := 0
		_, err := Do(ctx, h.r, fast, CallOptions{Idempotent: true}, func(context.Context, Target) (string, error) {
			calls++
			return "", rateLimitErr()
		})
		isKind(t, err, providers.KindRateLimited)
		if calls != 3 {
			t.Fatalf("attempts = %d, want 3", calls)
		}
		if len(h.sleepsCopy()) != 2 {
			t.Fatalf("backoffs = %d, want 2", len(h.sleepsCopy()))
		}
		if h.r.Health()[fast].Retries != 2 {
			t.Fatalf("Retries = %d, want 2", h.r.Health()[fast].Retries)
		}
	})

	t.Run("backoff-honours-cap", func(t *testing.T) {
		// BreakerThreshold well above the attempt count keeps the breaker
		// out of the picture: this test is about the backoff schedule.
		h := newHarness(t, Options{RetryBase: 100 * time.Millisecond, RetryCap: 150 * time.Millisecond, MaxAttempts: 5,
			BreakerThreshold: 10, Rand: rand.New(rand.NewSource(7))}, fast)
		calls := 0
		Do(ctx, h.r, fast, CallOptions{Idempotent: true}, func(context.Context, Target) (string, error) {
			calls++
			return "", transientErr()
		})
		if calls != 5 {
			t.Fatalf("attempts = %d, want 5", calls)
		}
		for i, s := range h.sleepsCopy() {
			if s < 75*time.Millisecond || s > 150*time.Millisecond {
				t.Fatalf("backoff %d = %s, want in [75ms,150ms] (base 100ms, cap 150ms)", i, s)
			}
		}
	})
}

// TestRetryAfterHonoured: an idempotent retry waits the longer of the
// computed backoff and the Retry-After hint (docs/spec/06: retry only if the
// event is still current, and politely with respect to rate limits).
func TestRetryAfterHonoured(t *testing.T) {
	h := newHarness(t, Options{}, fast)
	e := rateLimitErr()
	ra := &RetryAfterError{Err: e, RetryAfter: 900 * time.Millisecond}
	f := &fakeCalls{script: []error{ra, nil}}
	res, err := Do(context.Background(), h.r, fast, CallOptions{Idempotent: true}, f.fn())
	if err != nil || res != "ok" {
		t.Fatalf("Do = %q, %v", res, err)
	}
	if s := h.sleepsCopy(); len(s) != 1 || s[0] < 900*time.Millisecond {
		t.Fatalf("backoffs = %v, want a single sleep >= 900ms", s)
	}
	// The wrapper still classifies as the inner error.
	var pe *providers.Error
	errors.As(ra, &pe)
	if pe.Kind != providers.KindRateLimited {
		t.Fatalf("wrapped kind = %s", pe.Kind)
	}
}

// TestStaleGenerationDropsLateValues: the returned-generation guard
// (docs/spec/01) fires before each retry and before returning a success; the
// late value is dropped in both cases.
func TestStaleGenerationDropsLateValues(t *testing.T) {
	ctx := context.Background()

	t.Run("stale-before-retry-drops-and-stops", func(t *testing.T) {
		h := newHarness(t, Options{}, fast)
		current := true
		f := &fakeCalls{script: []error{transientErr(), nil}}
		_, err := Do(ctx, h.r, fast, CallOptions{Idempotent: true, Generation: func() bool {
			current = false // the observation was superseded mid-flight
			return current
		}}, f.fn())
		isKind(t, err, providers.KindStaleResult)
		if f.calls != 1 {
			t.Fatalf("attempts = %d, want 1 (no retry of a superseded observation)", f.calls)
		}
		if len(h.sleepsCopy()) != 0 {
			t.Fatalf("backoffs = %d, want 0", len(h.sleepsCopy()))
		}
		if h.r.Health()[fast].StaleDropped != 1 {
			t.Fatalf("StaleDropped = %d, want 1", h.r.Health()[fast].StaleDropped)
		}
	})

	t.Run("stale-success-drops-the-value", func(t *testing.T) {
		h := newHarness(t, Options{}, fast)
		f := &fakeCalls{} // always succeeds
		res, err := Do(ctx, h.r, fast, CallOptions{Generation: func() bool { return false }}, f.fn())
		if res != "" {
			t.Fatalf("late value %q was not dropped", res)
		}
		isKind(t, err, providers.KindStaleResult)
		if h.r.Health()[fast].StaleDropped != 1 {
			t.Fatalf("StaleDropped = %d, want 1", h.r.Health()[fast].StaleDropped)
		}
		// A stale drop is not a failure: the attempt succeeded.
		if h := h.r.Health()[fast]; h.Failures != 0 || h.State != StateOK {
			t.Fatalf("health = %+v, want ok with no failures", h)
		}
	})
}

// TestBreakerHalfOpenSingleProbe: once the cooldown has elapsed exactly one
// call is admitted as a probe; concurrent callers fail fast. A probe failure
// reopens the breaker; a probe success closes it (docs/spec/12 "Decision/
// provider": per-role circuit breaker recovery).
func TestBreakerHalfOpenSingleProbe(t *testing.T) {
	h := newHarness(t, Options{BreakerThreshold: 1, BreakerCooldown: 30 * time.Second}, fast)
	f := &fakeCalls{script: []error{transientErr(), nil}}
	Do(context.Background(), h.r, fast, CallOptions{}, f.fn()) // trips the breaker
	if h.r.Health()[fast].State != StateOpen {
		t.Fatalf("state = %s, want open", h.r.Health()[fast].State)
	}
	h.clock.step(31 * time.Second) // cooldown elapsed: half-open

	// While the probe is in flight, a second caller must fail fast: the
	// half-open breaker admits exactly one probe at a time.
	var probes int32
	probeReleased := make(chan struct{})
	doProbe := func() (string, error) {
		return Do(context.Background(), h.r, fast, CallOptions{}, func(_ context.Context, _ Target) (string, error) {
			atomic.AddInt32(&probes, 1)
			<-probeReleased
			return "probe", nil
		})
	}
	type probeResult struct {
		res string
		err error
	}
	probeResultCh := make(chan probeResult, 1)
	go func() {
		res, err := doProbe()
		probeResultCh <- probeResult{res, err}
	}()
	// Wait for the probe to actually be in flight, then a second caller
	// must fail fast: the half-open breaker admits one probe at a time.
	waitFor(t, "probe in flight", func() bool {
		return atomic.LoadInt32(&probes) == 1
	})
	res2, err2 := doProbe() // must fail fast while the probe is in flight
	if res2 != "" {
		t.Fatalf("fail-fast caller returned value %q", res2)
	}
	isKind(t, err2, providers.KindUnavailable) // the concurrent caller failed fast
	close(probeReleased)
	res1 := <-probeResultCh
	if res1.res != "probe" || res1.err != nil {
		t.Fatalf("probe call = %q, %v; want success", res1.res, res1.err)
	}
	if atomic.LoadInt32(&probes) != 1 {
		t.Fatalf("probes = %d, want exactly 1", probes)
	}
	if st := h.r.Health()[fast].State; st != StateOK {
		t.Fatalf("state after probe success = %s, want ok", st)
	}

	// Reopen: trip again, then fail the probe.
	h2 := newHarness(t, Options{BreakerThreshold: 1, BreakerCooldown: 30 * time.Second}, fast)
	calls2 := 0
	alwaysFail := func(context.Context, Target) (string, error) {
		calls2++
		return "", transientErr()
	}
	Do(context.Background(), h2.r, fast, CallOptions{}, alwaysFail)
	h2.clock.step(31 * time.Second)
	_, err := Do(context.Background(), h2.r, fast, CallOptions{}, alwaysFail) // the probe fails
	isKind(t, err, providers.KindUnavailable)
	if st := h2.r.Health()[fast].State; st != StateOpen {
		t.Fatalf("state after probe failure = %s, want open", st)
	}
	before := calls2
	if _, err := Do(context.Background(), h2.r, fast, CallOptions{}, alwaysFail); err == nil {
		t.Fatal("want fail-fast while open")
	}
	if calls2 != before {
		t.Fatal("fail-fast must not reach the provider")
	}
}

// TestPermanentErrorsDoNotTripBreaker: auth/schema/policy failures are
// returned immediately and never open the breaker (docs/spec/06 "Failure
// taxonomy": retries must distinguish transient from permanent failure).
func TestPermanentErrorsDoNotTripBreaker(t *testing.T) {
	h := newHarness(t, Options{BreakerThreshold: 1}, fast)
	calls := 0
	for i := 0; i < 5; i++ {
		_, err := Do(context.Background(), h.r, fast, CallOptions{Idempotent: true}, func(context.Context, Target) (string, error) {
			calls++
			return "", authErr()
		})
		isKind(t, err, providers.KindAuth)
		if h.r.Health()[fast].State != StateOK {
			t.Fatalf("after %d permanent failures state = %s, want ok", i+1, h.r.Health()[fast].State)
		}
	}
	if calls != 5 {
		t.Fatalf("attempts = %d, want 5 (never failed fast)", calls)
	}
	if h := h.r.Health()[fast]; h.ConsecutiveFailures != 0 || h.LastError != providers.KindAuth {
		t.Fatalf("health = %+v: permanent failures must not build a transient streak", h)
	}
}

// TestUnclassifiedErrorsAreDefensivelyClassified: a fn error that is not a
// *providers.Error is mapped to the taxonomy (deadline -> retryable timeout,
// other -> retryable unavailable) instead of leaking through.
func TestUnclassifiedErrorsAreDefensivelyClassified(t *testing.T) {
	h := newHarness(t, Options{}, fast)
	_, err := Do(context.Background(), h.r, fast, CallOptions{}, func(context.Context, Target) (string, error) {
		return "", context.DeadlineExceeded
	})
	isKind(t, err, providers.KindTimeout)
	var pe *providers.Error
	if !errors.As(err, &pe) || !pe.Retryable {
		t.Fatalf("deadline error must classify as retryable timeout, got %v", err)
	}

	h2 := newHarness(t, Options{}, fast)
	_, err = Do(context.Background(), h2.r, fast, CallOptions{}, func(context.Context, Target) (string, error) {
		return "", errors.New("mystery")
	})
	isKind(t, err, providers.KindUnavailable)
}
