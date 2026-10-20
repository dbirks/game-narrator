package runtime

// The session's frame→voice latency sampler (docs/design/plum-peach "Global
// chrome", the status bar's "412ms frame→voice" number).
//
// The line's total frame→voice interval has two halves measured by two owners:
//
//	frame → commit  the pipeline's own per-line histogram
//	(pipeline.Stats.Latency.FrameToCommitted), anchored on the committing
//	frame's capture timestamp — the pipeline is the only layer that sees
//	both ends of that interval.
//	commit → voice  this sampler: from the wall clock of the
//	message.committed event to the wall clock of the line's speech.started
//	(the scheduler accepting the playback on the sink), per line, kept for
//	the LAST N lines only.
//
// The sampler exists because the pipeline's half is a whole-run histogram:
// after a slow cold start its p50 is the average of the first minute, not of
// the last minute, which is the wrong number for a live status line. A
// bounded last-N window is the "current feeling" the bar wants, and the
// total the status string renders is the sum of the two halves' p50s —
// each measured per line, each named for what it is. The sum is an estimate
// of the total's p50, not the p50 of the total (the medians of two halves
// are not the median of the join); the ADR records the choice and the
// error is bounded by the variance of the two halves, which a status line
// is not the place to chase.

import (
	"sync"
	"time"
)

// frameVoiceN bounds the retained commit→voice samples: 32 lines is the last
// few minutes of a narrated textbox's dwell at its 2–5 Hz commit cadence.
const frameVoiceN = 32

// commitVoicePendingCap bounds the pending anchors: a generation the fence
// settles never gets a speech.started, so a table that kept every commit of
// a session would grow with the session. 64 in-flight lines is far past any
// real speech queue (the scheduler's slots are a single digit), so the
// bound is a leak tripwire, not a shed decision — a commit dropped here
// simply measures nothing.
const commitVoicePendingCap = 64

// frameVoiceSummary is the sampler's API view.
type frameVoiceSummary struct {
	// Count is the samples that went into P50 (0 = no measured samples yet).
	Count int `json:"count"`
	// P50 is the median of the retained samples (0 when Count is 0).
	P50 time.Duration `json:"p50"`
}

// CommitToVoiceSummary is the API view of the session's commit→voice
// window (Info.CommitToVoice): milliseconds, because the status bar renders
// it, and a status number in nanoseconds is a number nobody reads.
type CommitToVoiceSummary struct {
	// Count is the lines the window holds (0 = no line completed commit→
	// voice yet).
	Count int `json:"count"`
	// P50MS is the median of the window in whole milliseconds (0 when
	// Count is 0).
	P50MS int64 `json:"p50_ms"`
}

// frameVoiceSampler keeps the last N commit→voice samples of one session.
// Writers are two goroutines — the pipeline's writer (message.committed,
// via the commit watcher) and the speech scheduler's publisher
// (speech.started) — so the mutex is the contract, and the commit path
// must stay allocation-light: onCommit runs on the hot commit chain.
type frameVoiceSampler struct {
	mu      sync.Mutex
	samples []time.Duration      // oldest first, bounded to frameVoiceN
	pending map[string]time.Time // occurrence → commit wall clock
}

func newFrameVoiceSampler() *frameVoiceSampler {
	return &frameVoiceSampler{pending: make(map[string]time.Time, 8)}
}

// noteCommit books one committed line's wall clock, keyed by the line's
// dialogue occurrence id — the id speech.started carries back. A commit
// that never gets audio (fenced, skipped, no voice) is simply never
// completed: the pending entry falls out with the next bound trip.
func (s *frameVoiceSampler) noteCommit(occ string, at time.Time) {
	if occ == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) >= commitVoicePendingCap {
		// The table is at its bound and a commit is arriving: it is not
		// keeping up (speech is not consuming the commits), so it resets
		// rather than grows — the wall clocks alone cannot order a map for
		// an eviction, and a reset is a far better lie than an unbounded
		// table. The lines shed here measure nothing; the completed lines
		// that survive keep the window honest.
		s.pending = make(map[string]time.Time, 8)
	}
	s.pending[occ] = at
}

// noteVoiceStart completes the line whose speech just started and returns
// the current window under the same lock, so a reader sees one consistent
// pair.
func (s *frameVoiceSampler) noteVoiceStart(occ string, at time.Time) frameVoiceSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	anchor, ok := s.pending[occ]
	if ok {
		delete(s.pending, occ)
		d := at.Sub(anchor)
		if d < 0 {
			d = 0 // a clock step backwards books zero, like the pipeline's hist
		}
		s.samples = append(s.samples, d)
		if len(s.samples) > frameVoiceN {
			s.samples = s.samples[len(s.samples)-frameVoiceN:]
		}
	}
	return s.snapshotLocked()
}

// snapshot reports the current retained window (0/0 when no line has
// completed commit→voice in it).
func (s *frameVoiceSampler) snapshot() frameVoiceSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *frameVoiceSampler) snapshotLocked() frameVoiceSummary {
	if len(s.samples) == 0 {
		return frameVoiceSummary{}
	}
	// A copy: the summary must not hand out the live slice.
	vals := make([]time.Duration, len(s.samples))
	copy(vals, s.samples)
	sortDurations(vals)
	return frameVoiceSummary{Count: len(vals), P50: vals[len(vals)/2]}
}

func sortDurations(v []time.Duration) {
	for i := 1; i < len(v); i++ {
		for j := i; j > 0 && v[j] < v[j-1]; j-- {
			v[j], v[j-1] = v[j-1], v[j]
		}
	}
}
