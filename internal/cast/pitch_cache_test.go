package cast

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/speech"
)

// TestReferencePitchIsMeasuredOnce: the audit measures a reference asset's
// pitch once per SHA and serves repeats from the cache (the Cast page's audit
// took 15 s per view on the UNO Q when every view re-decoded and re-tracked
// every reference).
func TestReferencePitchIsMeasuredOnce(t *testing.T) {
	f, _, ctx := newFixEnv(t, &toneDesigner{freqs: []float64{220}})
	if _, err := f.FixCharacter(ctx, "pn_test_profile", "Cache Knight", ClassFemale, 110, true); err != nil {
		t.Fatalf("seed an approved voice: %v", err)
	}
	v, err := f.Aud.AssignedVoiceOf(ctx, "Cache Knight")
	if err != nil || v.ReferenceSHA == "" {
		t.Fatalf("assigned voice = %+v, %v", v, err)
	}
	calls := 0
	orig := measurePitch
	measurePitch = func(wav []byte) (speech.Pitch, error) { calls++; return orig(wav) }
	t.Cleanup(func() { measurePitch = orig })
	pitchCache.Lock()
	delete(pitchCache.m, v.ReferenceSHA)
	pitchCache.Unlock()

	p1, err := f.Aud.measureReference(ctx, v.ReferenceSHA)
	if err != nil {
		t.Fatalf("first measure: %v", err)
	}
	p2, err := f.Aud.measureReference(ctx, v.ReferenceSHA)
	if err != nil {
		t.Fatalf("second measure: %v", err)
	}
	if calls != 1 {
		t.Fatalf("pitch measured %d times for one SHA, want 1", calls)
	}
	if p1.F0 != p2.F0 || p1.F0 < 200 || p1.F0 > 240 {
		t.Fatalf("cached pitch = %.1f / %.1f Hz, want the same ~220 Hz", p1.F0, p2.F0)
	}
}
