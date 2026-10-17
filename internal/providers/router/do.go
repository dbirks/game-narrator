package router

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Default retry parameters (docs/spec/06 "Routing, reliability and
// replicas": retry idempotent inference calls with bounded exponential
// backoff only if the event is still current).
const (
	DefaultMaxAttempts = 3
	DefaultRetryBase   = 200 * time.Millisecond
	DefaultRetryCap    = 5 * time.Second
)

// CallOptions configures one Do call.
type CallOptions struct {
	// Idempotent marks the call safe to repeat. Only idempotent calls with
	// retryable errors are retried (docs/spec/06).
	Idempotent bool
	// Priority selects the slot class; the zero value (Live) is deliberate:
	// an unlabelled call is assumed to be on the interactive path.
	Priority Priority
	// AffinityKey is the sticky-routing hint recorded for the role (reserved
	// for cloned-voice routing; see Affinity).
	AffinityKey string
	// Capabilities are capability-checked against the role's declaration
	// (same rules as Resolve). Usually empty.
	Capabilities []Capability
	// Generation reports whether the originating observation is still
	// current (docs/spec/01 "returned-generation check"). It is consulted
	// before each retry and before returning a success; when it returns
	// false the router returns stale_result and drops the value. Nil means
	// "always current".
	Generation func() bool
}

// RetryAfterError wraps a *providers.Error with the Retry-After hint
// carried by the provider response. providers.Error has no field for it and
// the taxonomy package is frozen this milestone, so the hint travels as a
// wrapper the router finds via errors.As: an idempotent retry waits the
// longer of the computed backoff and the hinted delay.
type RetryAfterError struct {
	Err        *providers.Error
	RetryAfter time.Duration
}

// Error renders the wrapped error plus the hint.
func (e *RetryAfterError) Error() string {
	return fmt.Sprintf("%v (retry-after %s)", e.Err, e.RetryAfter)
}

// Unwrap exposes the wrapped taxonomy error for errors.As/Is.
func (e *RetryAfterError) Unwrap() error { return e.Err }

// Do runs fn against the role's target through the router: breaker gate,
// priority slot, per-attempt timeout, bounded retries with the generation
// guard (docs/spec/06 "Retry idempotent inference calls … only if event
// still current"; docs/spec/01 "returned-generation check").
//
// Errors follow the providers taxonomy: a breaker fail-fast and a cancelled
// wait/backoff are classified unavailable/cancelled with Role and Model set
// for correlation; the raw fn error is always preserved in Err or returned
// as-is when it is already a *providers.Error.
func Do[T any](ctx context.Context, r *Router, role providers.Role, opts CallOptions, fn func(context.Context, Target) (T, error)) (T, error) {
	var zero T
	target, err := r.Resolve(role, Affinity{Key: opts.AffinityKey}, opts.Capabilities)
	if err != nil {
		return zero, err
	}
	rs := r.roles[role]

	maxAttempts := r.maxAttempts
	if !opts.Idempotent {
		maxAttempts = 1
	}

	for attempt := 1; ; attempt++ {
		// Breaker gate before occupying a slot: an open breaker must not
		// burn concurrency it is protecting.
		probing := false
		switch rs.brk.check() {
		case gateDeny:
			r.noteStateChange(role)
			return zero, failFastError(role, target.Model, r.now())
		case gateProbe:
			probing = true
		}

		if err := rs.slots.acquire(ctx, opts.Priority); err != nil {
			e := providers.NewError(providers.KindCancelled, 0,
				fmt.Sprintf("role %s: cancelled while waiting for a concurrency slot", role))
			e.Role = role
			e.Model = target.Model
			e.Err = err
			return zero, e
		}

		// Per-attempt timeout from the role config (docs/spec/06 "Timeout
		// and cancellation defaults"): every attempt gets a fresh budget
		// that never extends the role timeout, and the caller's ctx still
		// bounds the whole call. A zero role timeout means the caller's ctx
		// is the only deadline (matching the adapters' own convention).
		var cancel context.CancelFunc = func() {}
		attCtx := ctx
		if rs.cfg.Timeout > 0 {
			attCtx, cancel = context.WithTimeout(ctx, rs.cfg.Timeout)
		}
		attemptStart := r.now()
		res, fnErr := fn(attCtx, target)
		cancel()
		// Latency is measured with the router clock: identical to wall time
		// in production, deterministic under the tests' fake clock.
		latency := r.now().Sub(attemptStart)
		rs.slots.release()

		rs.ring.add(latency)
		rs.noteCall()

		if fnErr == nil {
			rs.brk.recordSuccess()
			rs.noteSuccess(r.now())
			if staleGuard(opts.Generation) {
				rs.noteStale()
				r.noteStateChange(role)
				return zero, staleResultError(role, target.Model)
			}
			r.noteStateChange(role)
			return res, nil
		}

		pe := classify(fnErr)
		pe.Role = role
		pe.Model = target.Model
		rs.brk.recordFailure(pe.Kind, probing)
		rs.noteFailure(pe.Kind, r.now())
		r.noteStateChange(role)

		if !opts.Idempotent || !pe.Retryable || attempt >= maxAttempts {
			return zero, pe
		}
		// Generation guard before retrying (docs/spec/06: retry "only if
		// event still current"): a superseded observation must not be
		// re-sent to the model.
		if staleGuard(opts.Generation) {
			rs.noteStale()
			r.noteStateChange(role)
			return zero, staleResultError(role, target.Model)
		}
		delay := backoff(r.retryBase, r.retryCap, r.jitter, attempt)
		if ra := retryAfter(fnErr); ra > delay {
			delay = ra
		}
		rs.noteRetry()
		if err := r.sleep(ctx, delay); err != nil {
			e := providers.NewError(providers.KindCancelled, 0,
				fmt.Sprintf("role %s: cancelled during retry backoff", role))
			e.Role = role
			e.Model = target.Model
			e.Err = err
			return zero, e
		}
	}
}

// staleGuard reports whether the generation check says "no longer current".
func staleGuard(g func() bool) bool { return g != nil && !g() }

// staleResultError is the error returned when a (successful or retryable)
// result is dropped because the originating observation is no longer
// current (docs/spec/01 "returned-generation check"). The late value is
// discarded by the caller; the error is permanent — retrying a stale
// observation cannot help.
func staleResultError(role providers.Role, model string) *providers.Error {
	e := providers.NewError(providers.KindStaleResult, 0,
		"router: result dropped because the originating observation is no longer current")
	e.Role = role
	e.Model = model
	return e
}

// failFastError is the breaker open error: unavailable and retryable, so a
// polite caller may re-ask later, but no provider request is made.
func failFastError(role providers.Role, model string, now time.Time) *providers.Error {
	e := providers.NewError(providers.KindUnavailable, 0,
		fmt.Sprintf("role %s: circuit breaker open, failing fast", role))
	e.Role = role
	e.Model = model
	e.Retryable = true
	_ = now
	return e
}

// classify turns an arbitrary fn error into a taxonomy error. Errors that
// are already classified are returned unchanged; anything else is classified
// defensively (caller cancellation is permanent, a deadline is a retryable
// timeout, and an unknown error is treated as a transient unavailability —
// the adapters are expected to classify properly, this is the safety net).
func classify(err error) *providers.Error {
	var pe *providers.Error
	if errors.As(err, &pe) {
		if pe.Kind == "" {
			pe.Kind = providers.KindUnavailable
		}
		return pe
	}
	var kind providers.Kind
	var retryable bool
	switch {
	case errors.Is(err, context.Canceled):
		kind, retryable = providers.KindCancelled, false
	case errors.Is(err, context.DeadlineExceeded):
		kind, retryable = providers.KindTimeout, true
	default:
		kind, retryable = providers.KindUnavailable, true
	}
	e := providers.NewError(kind, 0, "router: unclassified provider failure")
	e.Retryable = retryable
	e.Err = err
	return e
}

// retryAfter extracts the Retry-After hint when the error carries one.
func retryAfter(err error) time.Duration {
	var rae *RetryAfterError
	if errors.As(err, &rae) {
		return rae.RetryAfter
	}
	return 0
}

// backoff is the exponential delay after the given failed attempt
// (1-based): base, base*2, base*4, … capped at cap, then "equal jitter" in
// [d/2, d) so retries of independent callers do not thundering-herd the
// gateway (docs/spec/06: bounded exponential backoff).
func backoff(base, cap time.Duration, jitter *jitterSource, failedAttempt int) time.Duration {
	if base <= 0 {
		base = DefaultRetryBase
	}
	if cap <= 0 {
		cap = DefaultRetryCap
	}
	d := base
	for i := 1; i < failedAttempt && d < cap; i++ {
		d *= 2
		if d > cap {
			d = cap
		}
	}
	// Equal jitter: [d/2, d).
	j := 0.5 + 0.5*jitter.float64()
	return time.Duration(float64(d) * j)
}

// realSleep is the default Sleep: it honours ctx so a cancelled live
// session is never held in a backoff.
func realSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
