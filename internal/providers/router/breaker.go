package router

import (
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Default breaker and retry parameters (docs/spec/06 "Routing, reliability
// and replicas": role-based route table and circuit breakers; bounded
// exponential backoff). They are deliberately conservative for the LAN
// gateway: the decision budget is 1500 ms, so a 30 s cooldown keeps a dead
// endpoint out of the way without a long blind period, and 3 consecutive
// transient failures is enough signal that the endpoint, not the call, is
// the problem.
const (
	DefaultBreakerThreshold = 3
	DefaultBreakerWindow    = 60 * time.Second
	DefaultBreakerCooldown  = 30 * time.Second
)

// breakerState is the internal two-state view. "open" covers both the
// cooldown (fail fast) and the half-open phase (one probe allowed once the
// cooldown has elapsed); the reported HealthState splits them.
type breakerState int

const (
	bcClosed breakerState = iota
	bcOpen
)

// gate is the outcome of gating a new call against the breaker.
type gate int

const (
	gateDeny  gate = iota // open and still cooling down (or a probe in flight)
	gateAllow             // closed: a normal call
	gateProbe             // half-open: this call is the single probe
)

// breaker is the per-role circuit breaker.
//
// Semantics (docs/spec/06, docs/spec/12 "Decision/provider": per-role
// circuit breaker recovery):
//
//   - closed: calls pass. Each transient failure (timeout, unavailable,
//     rate_limited) is timestamped; failures older than the window are
//     pruned. When the in-window consecutive count reaches the threshold the
//     breaker opens and refuses calls for the cooldown.
//   - a success clears the count and closes the breaker.
//   - permanent kinds (auth, bad_schema, wrong_checkpoint,
//     unsupported_capability, payload_too_large, content_refused, …) are
//     returned to the caller immediately and never trip the breaker; they
//     also do not clear a transient streak (a 503-then-401 endpoint is still
//     half dead).
//   - open: while now < openUntil every call is denied (the router fails
//     fast with a retryable unavailable error). Once the cooldown has
//     elapsed the breaker is half-open: exactly one call is admitted as a
//     probe; other calls are denied until the probe resolves. A probe
//     success closes the breaker; a probe transient failure reopens it for
//     another cooldown; a probe permanent failure leaves the half-open
//     phase so the next call probes again.
type breaker struct {
	threshold int
	window    time.Duration
	cooldown  time.Duration
	now       func() time.Time

	mu        sync.Mutex
	state     breakerState
	times     []time.Time // in-window transient failures, oldest first
	openUntil time.Time
	probing   bool
}

func newBreaker(threshold int, window, cooldown time.Duration, now func() time.Time) *breaker {
	if threshold <= 0 {
		threshold = DefaultBreakerThreshold
	}
	if window <= 0 {
		window = DefaultBreakerWindow
	}
	if cooldown <= 0 {
		cooldown = DefaultBreakerCooldown
	}
	return &breaker{threshold: threshold, window: window, cooldown: cooldown, now: now}
}

// isTransient reports whether the kind can be caused by the endpoint being
// overloaded, restarted or unreachable, which is the only class of failure
// a breaker may react to (docs/spec/06 "Failure taxonomy").
func isTransient(k providers.Kind) bool {
	switch k {
	case providers.KindTimeout, providers.KindUnavailable, providers.KindRateLimited:
		return true
	default:
		return false
	}
}

// check gates one new call. It is the only entry that mutates the probing
// flag for admission.
func (b *breaker) check() gate {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if b.state == bcClosed {
		return gateAllow
	}
	if now.Before(b.openUntil) {
		return gateDeny
	}
	// Cooldown elapsed: half-open.
	if b.probing {
		return gateDeny
	}
	b.probing = true
	return gateProbe
}

// recordSuccess closes the breaker and clears the transient streak.
func (b *breaker) recordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state = bcClosed
	b.times = nil
	b.probing = false
}

// recordFailure feeds the breaker with one failed attempt. wasProbe marks a
// half-open probe attempt. Permanent kinds are ignored entirely.
func (b *breaker) recordFailure(k providers.Kind, wasProbe bool) {
	if !isTransient(k) {
		// Permanent: never trips the breaker. If this was the probe, just
		// release the probe slot so the next call can probe again; the
		// streak (if any) is untouched.
		if wasProbe {
			b.mu.Lock()
			b.probing = false
			b.mu.Unlock()
		}
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	if wasProbe {
		// A failed probe reopens the breaker for a fresh cooldown.
		b.state = bcOpen
		b.openUntil = now.Add(b.cooldown)
		b.probing = false
		b.times = nil
		return
	}
	b.times = append(b.times, now)
	b.prune(now)
	if len(b.times) >= b.threshold {
		b.state = bcOpen
		b.openUntil = now.Add(b.cooldown)
	}
}

// prune drops failures outside the window so a slow trickle of old failures
// cannot open the breaker (docs/spec/06: N consecutive failures *within a
// window*).
func (b *breaker) prune(now time.Time) {
	cutoff := now.Add(-b.window)
	for len(b.times) > 0 && b.times[0].Before(cutoff) {
		b.times = b.times[1:]
	}
}

// stateAt maps the internal state to the reportable HealthState.
func (b *breaker) stateAt(now time.Time) HealthState {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == bcClosed {
		if len(b.times) > 0 {
			// Had transient trouble recently and no success since: degraded.
			return StateDegraded
		}
		return StateOK
	}
	if now.Before(b.openUntil) {
		return StateOpen
	}
	// Cooldown elapsed: half-open, recovering.
	return StateDegraded
}

// consecutive reports the current in-window transient streak.
func (b *breaker) consecutive() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.times)
}
