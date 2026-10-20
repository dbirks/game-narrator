package runtime

import (
	"testing"
	"time"
)

// The frameVoiceSampler is the bar's latency half: per-line commit→voice
// samples over the last-N window, with the pending map the commit side
// books and the voice side resolves. These tests pin the window bound, the
// p50 math, the pending cap (a run that commits without ever speaking must
// not grow a map for its whole life), and the negative-clamp.

func TestFrameVoiceSamplerWindowBounded(t *testing.T) {
	s := newFrameVoiceSampler()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 40; i++ {
		s.noteCommit("occ-"+string(rune('a'+i%26))+string(rune('0'+i/26)), base.Add(time.Duration(i)*time.Millisecond))
		s.noteVoiceStart("occ-"+string(rune('a'+i%26))+string(rune('0'+i/26)), base.Add(time.Duration(i)*time.Millisecond+time.Duration(100+i)*time.Millisecond))
	}
	snap := s.snapshot()
	if snap.Count != frameVoiceN {
		t.Fatalf("count = %d, want the window bound %d", snap.Count, frameVoiceN)
	}
	// The window kept the last 32 samples: i=8..39 → 108..139 ms. The p50
	// is the upper-middle of the sorted window: vals[16] = 124.
	if snap.P50.Milliseconds() != 124 {
		t.Errorf("p50 = %v, want 124ms (the window must hold the LAST %d lines)", snap.P50, frameVoiceN)
	}
}

func TestFrameVoiceSamplerP50IsTheMedian(t *testing.T) {
	s := newFrameVoiceSampler()
	base := time.Now().UTC()
	values := []time.Duration{900, 100, 400, 200, 300} // sorted: 100 200 300 400 900 (ms)
	for i, v := range values {
		occ := "occ-" + string(rune('a'+i))
		s.noteCommit(occ, base)
		s.noteVoiceStart(occ, base.Add(v*time.Millisecond))
	}
	snap := s.snapshot()
	if snap.P50.Milliseconds() != 300 {
		t.Fatalf("p50 = %v, want 300ms (the median of %v)", snap.P50, values)
	}
}

func TestFrameVoiceSamplerPairsTheOccurrenceNotTheNeighbour(t *testing.T) {
	s := newFrameVoiceSampler()
	base := time.Now().UTC()
	s.noteCommit("a", base)
	s.noteCommit("b", base.Add(50*time.Millisecond))
	// b's voice starts first (the sink is fast for one line); a's is later.
	s.noteVoiceStart("b", base.Add(60*time.Millisecond))
	s.noteVoiceStart("a", base.Add(400*time.Millisecond))
	snap := s.snapshot()
	if snap.Count != 2 {
		t.Fatalf("count = %d, want 2 (both occurrences voiced)", snap.Count)
	}
	// Samples: b = 10ms, a = 400ms → sorted 10, 400 → the p50 is the
	// upper-middle: 400. (The point is the pairing: a paired with b's
	// commit would have read 10ms.)
	if snap.P50.Milliseconds() != 400 {
		t.Fatalf("p50 = %v, want 400ms (each sample must pair its own occurrence's commit)", snap.P50)
	}
	// Neither pending entry survives its voice.
	s.noteCommit("c", base)
	s.noteVoiceStart("c", base.Add(10*time.Millisecond))
	if got := s.snapshot(); got.Count != 3 {
		t.Fatalf("count = %d after three pairs; want 3", got.Count)
	}
}

func TestFrameVoiceSamplerPendingCapResets(t *testing.T) {
	s := newFrameVoiceSampler()
	base := time.Now().UTC()
	// A run that commits but never speaks: the pending table must not grow
	// past its cap, and hitting the cap resets it (the waits that survive a
	// reset are the ones re-anchored; the rest are shed, and a line shed
	// here measures nothing — the bar's number must not be poisoned by a
	// queue that never drains).
	for i := 0; i < commitVoicePendingCap*3; i++ {
		s.noteCommit("never-voiced-"+string(rune('a'+i%26)), base.Add(time.Duration(i)*time.Millisecond))
	}
	if got := s.snapshot(); got.Count != 0 || got.P50 != 0 {
		t.Fatalf("after the cap resets the sampler is %+v; want empty (no voice ever started)", got)
	}
	// The sampler still works after the reset.
	s.noteCommit("voiced", base)
	s.noteVoiceStart("voiced", base.Add(123*time.Millisecond))
	if got := s.snapshot(); got.Count != 1 || got.P50.Milliseconds() != 123 {
		t.Fatalf("after the reset the sampler is %+v; want the one fresh sample", got)
	}
}

func TestFrameVoiceSamplerClampsNegativeAndDropsUnknowns(t *testing.T) {
	s := newFrameVoiceSampler()
	base := time.Now().UTC()
	// The clock ran backwards between commit and voice: clamp to zero.
	s.noteCommit("backwards", base.Add(100*time.Millisecond))
	s.noteVoiceStart("backwards", base)
	// A voice with no booked commit (the session's first line committed
	// before the sampler existed, or an occurrence id the commit side
	// never saw): dropped, not a negative sample.
	s.noteVoiceStart("unknown", base.Add(time.Second))
	got := s.snapshot()
	if got.Count != 1 {
		t.Fatalf("count = %d, want 1 (the unknown voice is dropped)", got.Count)
	}
	if got.P50 != 0 {
		t.Fatalf("p50 = %v, want 0 (the backwards sample clamps)", got.P50)
	}
}

func TestFrameVoiceSamplerSnapshotIsAStableCopy(t *testing.T) {
	s := newFrameVoiceSampler()
	base := time.Now().UTC()
	s.noteCommit("one", base)
	s.noteVoiceStart("one", base.Add(50*time.Millisecond))
	first := s.snapshot()
	s.noteCommit("two", base)
	s.noteVoiceStart("two", base.Add(900*time.Millisecond))
	// The earlier snapshot must not move with the sampler.
	if first.Count != 1 || first.P50.Milliseconds() != 50 {
		t.Fatalf("earlier snapshot moved with the sampler: %+v", first)
	}
}
