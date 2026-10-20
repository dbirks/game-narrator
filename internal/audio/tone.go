package audio

import (
	"github.com/dbirks/game-narrator/internal/speech"
)

// ScaleWAV multiplies the tone's samples by g (clamped to the format's range)
// and re-encodes. The endpoint applies the effective master volume to the
// tone this way — the same decode → scale → re-encode the session's sink
// wrappers use, so a tone the page plays is shaped exactly like the speech
// the device will hear. g is clamped to [0, 1): a product endpoint that can
// push the device into clipping is the defect the calibration exists to
// avoid — and g = 0 is the master volume's silence, which must be silence,
// so it is applied (every sample zero), never skipped.
func ScaleWAV(wav []byte, g float64) []byte {
	if g < 0 {
		g = 0
	}
	if g > 1 {
		g = 1
	}
	if g == 1 {
		return wav
	}
	pcm, err := speech.DecodeWAV(wav)
	if err != nil {
		return wav // a tone that cannot decode is the encoder's defect; the caller validates
	}
	return speech.EncodeWAV(scalePCM(pcm.Data, g, pcm.BitsPerSample), pcm.SampleRate, pcm.Channels, pcm.BitsPerSample)
}

// scalePCM multiplies interleaved little-endian PCM by g, clamping to the
// format's range (the runtime's same rule; a product tone and the session
// must not disagree about what 0.5 dB of headroom is).
func scalePCM(data []byte, g float64, bits int) []byte {
	out := make([]byte, len(data))
	switch bits {
	case 16:
		for i := 0; i+1 < len(data); i += 2 {
			s := int16(uint16(data[i]) | uint16(data[i+1])<<8) // little-endian
			v := int(float64(s) * g)
			if v > 32767 {
				v = 32767
			}
			if v < -32768 {
				v = -32768
			}
			out[i], out[i+1] = byte(uint16(v)&0xff), byte(uint16(v)>>8)
		}
	default:
		// The tone is 16-bit mono by construction (MakeToneWAV); an unknown
		// format passes through unscaled rather than being mis-scaled.
		return data
	}
	return out
}

// The deterministic test tone of docs/spec/10 (ADR-0074): the same 440 Hz /
// 1 s / 48 kHz tone the hwsmoke gate and the pipewire sink's own round-trip
// test use — the product surface and the hardware gate must not drift apart.
// MakeToneWAV renders it at 0.25 amplitude (about -12 dB, well inside the
// non-silence/no-clip bounds), which is the "short and quiet" the product
// click needs: answerable as "something came out of the speaker" without
// blasting the room.
const (
	ToneFreqHz     = 440.0
	ToneSeconds    = 1.0
	ToneSampleRate = 48000
	// ToneLevel is the tone's linear amplitude, the constant MakeToneWAV
	// itself renders at (documented here so the endpoint's answer and the
	// spec's "deterministic test tone" name the same number).
	ToneLevel = 0.25
)

// ToneWAV returns the deterministic test tone as a validated RIFF/WAVE file:
// a 1 s 440 Hz sine at the PipeWire default 48 kHz, 16-bit mono. The bytes
// are a pure function of the constants above (MakeToneWAV is a pure encoder),
// so the endpoint and the test that asserts the endpoint compute the same
// tone from the same source (docs/spec/10 "play deterministic test tone").
func ToneWAV() []byte {
	return speech.MakeToneWAV(ToneFreqHz, ToneSeconds, ToneSampleRate)
}
