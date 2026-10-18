// passstate.go is the sidecar's per-pass bookkeeping with explicit bounds:
// the leak behind the 1-hour soak's RSS growth (run 20261017-235844-replay,
// ~0.5 KiB per committed line, heap-profiled: the per-pass event slices the
// harness kept for every pass of the run — ADR-0045 amendment). A pass slice
// is *settled* once every utterance that started in it has a terminal event
// (speech.ended, failed ends included, or speech.cancelled); a settled slice
// is graded immediately and dropped, so the retained state is bounded by the
// unsettled horizon (maxPendingPasses) instead of the run's pass count.

package soak

import (
	"math"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// maxPendingPasses bounds how many pass slices the harness keeps unsettled.
// A pass's last utterance can outlive the pass by at most one decision
// round-trip (providerTimeout), one TTS round-trip (providerTimeout) and the
// tone's playback (toneEndDelay) before its terminal event is filed — so the
// unsettled horizon is 2*providerTimeout + toneEndDelay. A pass is paced at
// its clip's own span (minClipSpanS, measured from the clips at build time),
// so the horizon spans at most ceil(horizon/span) passes; the +1 is the pass
// still being fed, and the +1 slack covers clock jitter. With the 10 s
// provider budget and the 2.3 s synthetic clip that is ~9.
const (
	settleHorizon    = 2*providerTimeout + toneEndDelay
	maxPendingPasses = 9
)

// minClipSpanS reports the shortest clip's capture span in seconds: the
// sidecar paces one pass at that span, so it is the smallest wall time a pass
// can take and the lower bound the pending-pass calculation divides by.
func minClipSpanS(clips []clip) float64 {
	if len(clips) == 0 {
		return 1 // no clip: the harness cannot run; keep the division honest
	}
	min := math.MaxFloat64
	for _, c := range clips {
		if c.stride <= 0 {
			continue
		}
		if float64(c.stride)/1e9 < min {
			min = float64(c.stride) / 1e9
		}
	}
	if min == math.MaxFloat64 || min <= 0 {
		return 1
	}
	return min
}

// pendingPassBound re-derives the pending-pass bound from the measured clip
// spans (documented here rather than as a bare constant so a future longer
// provider budget or shorter clip changes the bound in one place). It is
// max(maxPendingPasses, derived) so the derived value can only make the
// harness more conservative, never less.
func pendingPassBound(spanS float64) int {
	if spanS <= 0 {
		return maxPendingPasses
	}
	n := int(math.Ceil(settleHorizon.Seconds()/spanS)) + 2
	if n < maxPendingPasses {
		return maxPendingPasses
	}
	return n
}

// passState is one pass's slice and the utterance-lifecycle knowledge that
// decides when the slice is settled.
type passState struct {
	stamp  passStamp
	events []events.Envelope
	// closed is set when the sidecar finishes feeding the pass: until then
	// commit events are still on the way, so "no utterance started yet" is
	// not settled — the pass is mid-feed.
	closed bool
	// lastCommit is the wall time of the latest commit event filed into the
	// slice: a commit's utterance can still start up to settleHorizon after
	// it (one decision round-trip, one TTS round-trip, the tone's playback),
	// so the slice is not settled while that window is open even if every
	// utterance that started has a terminal.
	lastCommit time.Time
	// started names the utterances that began in this pass (occurrence ->
	// generation); terminal names the occurrences that reached a terminal
	// event. Settled: closed, the commit window is closed, and every started
	// occurrence has a terminal.
	started  map[string]int64
	terminal map[string]bool
}

// settled reports whether the slice is complete at now: the pass stopped
// being fed (no commit can still arrive), no commit's utterance can still
// start (the settle window closed), and no utterance that started can still
// emit an event (a terminal already closed every one of them).
func (ps *passState) settled(now time.Time) bool {
	if !ps.closed {
		return false
	}
	if !ps.lastCommit.IsZero() && now.Sub(ps.lastCommit) < settleHorizon {
		return false
	}
	for occ := range ps.started {
		if !ps.terminal[occ] {
			return false
		}
	}
	return true
}
