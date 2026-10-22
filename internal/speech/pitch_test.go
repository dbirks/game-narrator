package speech

import (
	"encoding/binary"
	"math"
	"testing"
)

// TestMeasurePitchMedianYIN: a 440 Hz sine over a voiced span must measure a
// median F0 near 440 Hz (the YIN estimator over voiced frames), and a pure
// silence clip must measure no pitch (zero, unmeasured — never a fabricated
// F0).
func TestMeasurePitchMedianYIN(t *testing.T) {
	wav := MakeToneWAV(440, 1.0, 16000)
	p, err := MeasurePitch(wav)
	if err != nil {
		t.Fatalf("MeasurePitch: %v", err)
	}
	if p.VoicedFrames == 0 {
		t.Fatalf("MeasurePitch: no voiced frames measured (the tone should voice)")
	}
	if p.F0 < 300 || p.F0 > 580 {
		t.Fatalf("MeasurePitch: median F0 = %.1f Hz; want near 440 (the YIN estimate over voiced frames)", p.F0)
	}
	if got := VoiceClass(p.F0); got != "mid (adult female)" && got != "low (adult male)" && got != "high (child)" && got != "unmeasured" {
		t.Fatalf("VoiceClass: %q", got)
	}

	// Silence: no voiced frames → F0 0, never a fabricated tone.
	silent := EncodeWAV(make([]byte, 2*16000*2), 16000, 1, 16)
	p2, err := MeasurePitch(silent)
	if err != nil {
		t.Fatalf("MeasurePitch(silent): %v", err)
	}
	if p2.F0 != 0 || p2.VoicedFrames != 0 {
		t.Fatalf("MeasurePitch(silent) = %+v; want F0 0 / no voiced frames", p2)
	}
}

// TestVoiceClassBounds pins the ADR-0162 class edges: below 165 Hz the
// voice reads low, from 165 up to 255 mid, above 255 high.
func TestVoiceClassBounds(t *testing.T) {
	cases := []struct {
		f0   float64
		want string
	}{
		{0, "unmeasured"},
		{100, "low (adult male)"},
		{165, "mid (adult female)"},
		{210, "mid (adult female)"},
		{255, "mid (adult female)"},
		{300, "high (child)"},
		{500, "high (child)"},
	}
	for _, c := range cases {
		if got := VoiceClass(c.f0); got != c.want {
			t.Fatalf("VoiceClass(%v) = %q; want %q", c.f0, got, c.want)
		}
	}
}

// TestMeasurePitchNoOctaveError pins the estimate within 5% on harmonic-rich
// synthetic voices (fundamental plus decaying harmonics, the shape that pulls
// a naive estimator an octave off). The first implementation reported a
// child's 274 Hz voice as 533 Hz, which would class an adult woman as a
// child and a man as a woman.
func TestMeasurePitchNoOctaveError(t *testing.T) {
	for _, f0 := range []float64{110, 210, 300} {
		const sr = 16000
		n := sr // 1 s
		pcm := make([]byte, 2*n)
		for i := 0; i < n; i++ {
			ti := float64(i) / sr
			v := 0.0
			for h := 1; h <= 6; h++ {
				v += math.Sin(2*math.Pi*f0*float64(h)*ti) / float64(h)
			}
			binary.LittleEndian.PutUint16(pcm[2*i:], uint16(int16(v*0.25*32767)))
		}
		p, err := MeasurePitch(EncodeWAV(pcm, sr, 1, 16))
		if err != nil {
			t.Fatalf("MeasurePitch(%v Hz): %v", f0, err)
		}
		if math.Abs(p.F0-f0)/f0 > 0.05 {
			t.Errorf("MeasurePitch(%v Hz voice) = %.1f Hz, want within 5%%", f0, p.F0)
		}
	}
}
