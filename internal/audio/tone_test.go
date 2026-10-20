package audio

import (
	"math"
	"testing"

	"github.com/dbirks/game-narrator/internal/speech"
)

// The tone is the deterministic evidence of docs/spec/10: the endpoint, the
// live check and the hardware gate must all compute the same bytes. So the
// tests pin the bytes (stable across calls), the shape (rate, length, level)
// and the one frequency that names the evidence (zero crossings).

func TestToneWAVIsDeterministic(t *testing.T) {
	a, b := ToneWAV(), ToneWAV()
	if len(a) == 0 {
		t.Fatal("empty tone")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("byte %d differs between two renders: the tone must be a pure function of its constants", i)
		}
	}
}

func TestToneWAVShape(t *testing.T) {
	pcm, err := speech.DecodeWAV(ToneWAV())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if pcm.SampleRate != ToneSampleRate {
		t.Errorf("sample rate %d, want %d", pcm.SampleRate, ToneSampleRate)
	}
	if pcm.Channels != 1 || pcm.BitsPerSample != 16 {
		t.Errorf("format %dch/%dbit, want 1ch/16bit", pcm.Channels, pcm.BitsPerSample)
	}
	// One second of 48 kHz: the length is the duration the spec names.
	wantFrames := int(ToneSeconds) * ToneSampleRate
	if got := pcm.Frames(); got != wantFrames {
		t.Errorf("frames %d, want %d (1.0 s at 48 kHz)", got, wantFrames)
	}

	// Peak amplitude: the rendered level is ToneLevel of full scale.
	var peak int
	for i := 0; i+1 < len(pcm.Data); i += 2 {
		s := int16(uint16(pcm.Data[i]) | uint16(pcm.Data[i+1])<<8)
		if v := abs(int(s)); v > peak {
			peak = v
		}
	}
	wantPeak := int(math.Round(ToneLevel * 32767))
	if math.Abs(float64(peak)-float64(wantPeak)) > 1 {
		t.Errorf("peak %d, want %d (level %v)", peak, wantPeak, ToneLevel)
	}

	// Frequency: the UPWARD zero crossings of one second of a 440 Hz sine
	// are ~440 (one per cycle; counting both directions would be ~880).
	crossings := 0
	prev := int16(0)
	for i := 0; i+1 < len(pcm.Data); i += 2 {
		s := int16(uint16(pcm.Data[i]) | uint16(pcm.Data[i+1])<<8)
		if prev < 0 && s >= 0 {
			crossings++
		}
		prev = s
	}
	if crossings < 438 || crossings > 442 {
		t.Errorf("zero crossings %d, want ~440 (the tone must be 440 Hz)", crossings)
	}
}

func TestScaleWAVScalesTheSamples(t *testing.T) {
	orig, err := speech.DecodeWAV(ToneWAV())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	half, err := speech.DecodeWAV(ScaleWAV(ToneWAV(), 0.5))
	if err != nil {
		t.Fatalf("decode scaled: %v", err)
	}
	if half.SampleRate != orig.SampleRate || half.Channels != orig.Channels || half.BitsPerSample != orig.BitsPerSample {
		t.Errorf("scaling changed the format: %dch/%d/%d", half.Channels, half.SampleRate, half.BitsPerSample)
	}
	if half.Frames() != orig.Frames() {
		t.Errorf("scaling changed the length: %d vs %d frames", half.Frames(), orig.Frames())
	}
	var origPeak, halfPeak int
	for i := 0; i+1 < len(orig.Data); i += 2 {
		s := abs(int(int16(uint16(orig.Data[i]) | uint16(orig.Data[i+1])<<8)))
		if s > origPeak {
			origPeak = s
		}
	}
	for i := 0; i+1 < len(half.Data); i += 2 {
		s := abs(int(int16(uint16(half.Data[i]) | uint16(half.Data[i+1])<<8)))
		if s > halfPeak {
			halfPeak = s
		}
	}
	if want := origPeak / 2; math.Abs(float64(halfPeak)-float64(want)) > 1 {
		t.Errorf("half-scale peak %d, want ~%d", halfPeak, want)
	}
}

func TestScaleWAVZeroIsSilenceAndAboveUnityClamps(t *testing.T) {
	silence, err := speech.DecodeWAV(ScaleWAV(ToneWAV(), 0))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, b := range silence.Data {
		if b != 0 {
			t.Fatal("a zero-scale tone must be silence (every sample zero)")
		}
	}
	// Above-unity clamps to unity: a product endpoint must not push the
	// device into clipping.
	same := ScaleWAV(ToneWAV(), 2.0)
	orig := ToneWAV()
	if len(same) != len(orig) {
		t.Fatalf("length changed: %d vs %d", len(same), len(orig))
	}
	for i := range same {
		if same[i] != orig[i] {
			t.Fatal("a >1 scale must clamp to unity, not amplify")
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
