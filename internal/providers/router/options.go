package router

import (
	"context"
	"math/rand"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Options configures a Router. Zero fields take the documented defaults.
type Options struct {
	// Capabilities declares which capabilities each role provides. A role
	// with no entry (or a nil slice) accepts any requested capability —
	// the declaration is unknown and preflight must probe it (docs/spec/06);
	// a role with a non-nil entry must list every requested capability or
	// Resolve fails with unsupported_capability.
	Capabilities map[providers.Role][]Capability
	// Concurrency is the per-role in-flight cap; zero uses
	// DefaultConcurrency.
	Concurrency map[providers.Role]int
	// BreakerThreshold is the number of consecutive transient failures that
	// open the breaker; zero uses DefaultBreakerThreshold.
	BreakerThreshold int
	// BreakerWindow bounds how old a counted failure may be; zero uses
	// DefaultBreakerWindow.
	BreakerWindow time.Duration
	// BreakerCooldown is how long an open breaker refuses calls; zero uses
	// DefaultBreakerCooldown.
	BreakerCooldown time.Duration
	// MaxAttempts is the total attempts per Do call for idempotent calls
	// with retryable errors; zero uses DefaultMaxAttempts.
	MaxAttempts int
	// RetryBase is the first backoff delay; zero uses DefaultRetryBase.
	RetryBase time.Duration
	// RetryCap caps a backoff delay; zero uses DefaultRetryCap.
	RetryCap time.Duration
	// LatencyRingSize is the P50/P95 ring length; zero uses
	// DefaultLatencyRingSize.
	LatencyRingSize int
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Sleep delays a retry and must honour ctx; nil uses a context-aware
	// real sleep.
	Sleep func(ctx context.Context, d time.Duration) error
	// Rand supplies the jitter draws; nil uses a private source.
	Rand *rand.Rand
	// OnStateChange observes role health-state transitions. It is called
	// from the Do path, once per transition, and may run concurrently for
	// different roles; keep it short and side-effect free (the app
	// publishes provider.degraded events from it).
	OnStateChange func(role providers.Role, from, to HealthState)
}
