package router

import (
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Health snapshots every role's router state. It is a read: it never fires
// OnStateChange and never mutates breaker state.
func (r *Router) Health() map[providers.Role]RoleHealth {
	now := r.now()
	out := make(map[providers.Role]RoleHealth, len(r.roles))
	for role, rs := range r.roles {
		p50, p95 := rs.ring.percentiles()
		rs.mu.Lock()
		out[role] = RoleHealth{
			Role:                role,
			State:               rs.stateAt(now),
			ConsecutiveFailures: rs.brk.consecutive(),
			LastError:           rs.lastError,
			LastErrorAt:         rs.lastErrorAt,
			LastSuccessAt:       rs.lastSuccessAt,
			P50:                 p50,
			P95:                 p95,
			Calls:               rs.calls,
			Failures:            rs.failures,
			Retries:             rs.retries,
			StaleDropped:        rs.staleDropped,
		}
		rs.mu.Unlock()
	}
	return out
}

// SetBackgroundPaused pauses (or resumes) all Background callers process-
// wide while Live callers proceed (docs/spec/01 "Runtime and setup
// isolation": background work is rate-limited or paused while a live session
// needs resources). Paused callers block; they are not failed.
func (r *Router) SetBackgroundPaused(paused bool) {
	for _, rs := range r.roles {
		rs.slots.setPaused(paused)
	}
}

// ObserveOrigin records the provenance label of a completed provider call so
// a report can state which stage was live and which was recorded
// (ADR-0013: reports explicitly record stage origins). Adapters pass
// recorded.Origin(resp) after a response; the label is per role.
func (r *Router) ObserveOrigin(role providers.Role, origin string) {
	rs, ok := r.roles[role]
	if !ok {
		return
	}
	rs.setOrigin(origin)
}

// Origins returns a copy of the last recorded origin label per role ("",
// "live" or "recorded" once observed).
func (r *Router) Origins() map[providers.Role]string {
	out := make(map[providers.Role]string, len(r.roles))
	for role, rs := range r.roles {
		rs.mu.Lock()
		out[role] = rs.origin
		rs.mu.Unlock()
	}
	return out
}

// LastAffinity returns the most recently recorded affinity for a role, so a
// later sticky-TTS router can pick up where the single-target routing left
// off (docs/spec/06 "Routing, reliability and replicas").
func (r *Router) LastAffinity(role providers.Role) (Affinity, bool) {
	rs, ok := r.roles[role]
	if !ok {
		return Affinity{}, false
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.affinity, true
}

// noteStateChange fires OnStateChange when the role's health state moved. It
// is the single event point: the Do path calls it after every attempt
// outcome and fail-fast.
func (r *Router) noteStateChange(role providers.Role) {
	rs := r.roles[role]
	if rs == nil {
		return
	}
	cur := rs.stateAt(r.now())
	rs.mu.Lock()
	prev := rs.prev
	changed := prev != cur
	if changed {
		rs.prev = cur
	}
	rs.mu.Unlock()
	if !changed || r.onState == nil {
		return
	}
	r.onState(role, prev, cur)
}

// Note the per-role recorders below take the role mutex themselves; the
// breaker, slots and ring are separate locks.

// noteFailure records one failed attempt's classification.
func (rs *roleState) noteFailure(kind providers.Kind, at time.Time) {
	rs.mu.Lock()
	rs.failures++
	rs.lastError = kind
	rs.lastErrorAt = at
	rs.mu.Unlock()
}

// noteCall counts one attempt sent to the provider (success or failure).
func (rs *roleState) noteCall() {
	rs.mu.Lock()
	rs.calls++
	rs.mu.Unlock()
}

func (rs *roleState) noteRetry() {
	rs.mu.Lock()
	rs.retries++
	rs.mu.Unlock()
}

func (rs *roleState) noteStale() {
	rs.mu.Lock()
	rs.staleDropped++
	rs.mu.Unlock()
}

func (rs *roleState) noteSuccess(at time.Time) {
	rs.mu.Lock()
	rs.lastSuccessAt = at
	rs.mu.Unlock()
}

func (rs *roleState) setOrigin(o string) {
	rs.mu.Lock()
	rs.origin = o
	rs.mu.Unlock()
}

func (rs *roleState) setAffinity(a Affinity) {
	rs.mu.Lock()
	rs.affinity = a
	rs.mu.Unlock()
}

func (rs *roleState) stateAt(now time.Time) HealthState {
	if !rs.cfg.Configured() {
		return StateNotConfigured
	}
	return rs.brk.stateAt(now)
}
