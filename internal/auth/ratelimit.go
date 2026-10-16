package auth

import (
	"math"
	"sync"
	"time"
)

// Rate limits applied to the two endpoints that can be used to guess a
// password (docs/spec/08 "Rate-limit agent web search, file import, inference
// calls and preview streaming"; docs/spec/14 "Abuse and resource controls" and
// "Authorization: … rate limits").
//
// 10 attempts per 15 minutes, expressed as a token bucket rather than a fixed
// window: a fixed window lets an attacker spend 10 guesses at 14:59 and 10 more
// at 15:01, and a bucket that starts full at 10 with a refill of 10/15min gives
// the documented allowance with no such cliff. The bucket refills continuously,
// so a legitimate user who mistypes twice is not locked out of a reboot cycle.
const (
	// LoginBucketCapacity is the burst allowance per key.
	LoginBucketCapacity = 10
	// LoginWindow is the interval over which that allowance refills.
	LoginWindow = 15 * time.Minute
	// maxLimiterKeys bounds memory. This is a single-appliance service, so the
	// population of keys is "LAN hosts × usernames", but a hostile host can
	// mint distinct keys by varying its source address, and an unbounded map is
	// a memory-exhaustion DoS on a 4 GB target (docs/spec/14 "Bound tool budget
	// … upload sizes … quota"). When the map is full, expired buckets are
	// reclaimed; if none are reclaimable the *new* key is refused — an evicted
	// attacker is rate-limited, not un-rate-limited.
	maxLimiterKeys = 4096
)

// Limiter is a token bucket per key, in memory.
//
// In memory is the right place: the state is *abuse telemetry*, it is worthless
// across a restart (an attacker who can force restarts is not the threat model,
// and a restart is not a way to reset somebody else's lockout either because
// the failed attempts are audited in SQLite regardless), and putting it in
// SQLite would add a write to every rejected login — turning the DoS endpoint
// into the busiest writer on the appliance. ADR-0016 notes the tradeoff.
type Limiter struct {
	mu       sync.Mutex
	capacity float64
	refill   float64 // tokens per second
	now      func() time.Time
	buckets  map[string]*bucket
}

type bucket struct {
	tokens  float64
	updated time.Time
}

// NewLoginLimiter returns the default login/setup limiter.
func NewLoginLimiter() *Limiter { return NewLimiter(LoginBucketCapacity, LoginWindow, time.Now) }

// NewLimiter builds a bucket with the given capacity that refills fully over
// window. now is injectable so tests do not sleep.
func NewLimiter(capacity int, window time.Duration, now func() time.Time) *Limiter {
	if capacity <= 0 {
		capacity = 1
	}
	if window <= 0 {
		window = time.Second
	}
	if now == nil {
		now = time.Now
	}
	return &Limiter{
		capacity: float64(capacity),
		refill:   float64(capacity) / window.Seconds(),
		now:      now,
		buckets:  make(map[string]*bucket),
	}
}

// Allow consumes one token from key, reporting whether the request may proceed.
// When it refuses, retryAfter is how long until a token is available (never
// zero — the caller puts it in a Retry-After header, and "0" would tell a client
// to hammer immediately).
func (l *Limiter) Allow(key string) (allowed bool, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now().UTC()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxLimiterKeys {
			if !l.reclaimLocked(now) {
				// No room and nothing expired: refuse. Fail closed — the
				// alternative is to stop rate-limiting the new key, which is the
				// exact moment a flood starts.
				return false, time.Minute
			}
		}
		b = &bucket{tokens: l.capacity, updated: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.updated); elapsed > 0 {
		b.tokens = math.Min(l.capacity, b.tokens+l.refill*elapsed.Seconds())
		b.updated = now
	}
	if b.tokens < 1 {
		return false, time.Duration(math.Ceil((1 - b.tokens) / l.refill * float64(time.Second)))
	}
	b.tokens--
	return true, 0
}

// reclaimLocked drops buckets that are already full again (i.e. idle long
// enough that they hold no information worth keeping). Returns whether anything
// was freed.
func (l *Limiter) reclaimLocked(now time.Time) bool {
	freed := false
	for k, b := range l.buckets {
		if b.tokens >= l.capacity {
			delete(l.buckets, k)
			freed = true
		}
	}
	return freed
}

// Reset forgets one key. It is called after a successful login so a user who
// has proven knowledge of the password is not throttled for their earlier
// typos, and it is *not* called on logout or on any unauthenticated path.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// Keys reports the tracked key count (for diagnostics; the key strings
// themselves are usernames and addresses and stay out of any payload).
func (l *Limiter) Keys() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
