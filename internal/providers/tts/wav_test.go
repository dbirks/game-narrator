package tts

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// TestValidateAcceptsTheCommittedCassette: the committed fake cassette must
// round-trip through the validator with the parameters it was built with.
func TestValidateAcceptsTheCommittedCassette(t *testing.T) {
	info, err := ValidateAudio(fixtureWAV(t))
	if err != nil {
		t.Fatalf("fixture rejected: %v", err)
	}
	if info.SampleRate != 24000 || info.Channels != 1 || info.BitsPerSample != 16 {
		t.Errorf("format = %+v, want 24000 Hz mono 16-bit", info)
	}
	if info.Duration < 990*time.Millisecond || info.Duration > 1010*time.Millisecond {
		t.Errorf("duration = %s, want ~1 s", info.Duration)
	}
	if info.DataBytes != 48000 {
		t.Errorf("data bytes = %d, want 48000", info.DataBytes)
	}
}

// TestValidateTable pins every malformed shape the failure taxonomy treats
// as audio_invalid: truncated header, inconsistent chunk lengths, wrong
// format, absurd sample rate, overlong duration, silence, clipping.
func TestValidateTable(t *testing.T) {
	good := sineWAV(24000, 1, 220, 0.4)
	cases := []struct {
		name       string
		mutate     func() []byte
		wantSubstr string
	}{
		{"empty", func() []byte { return nil }, "truncated RIFF header"},
		{"truncated header", func() []byte { b := append([]byte(nil), good...); return b[:10] }, "truncated RIFF header"},
		{"wrong magic", func() []byte {
			b := append([]byte(nil), good...)
			copy(b[:4], "XXXX")
			return b
		}, "not a RIFF file"},
		{"wrong form type", func() []byte {
			b := append([]byte(nil), good...)
			copy(b[8:12], "AVI ")
			return b
		}, "is not WAVE"},
		{"data chunk overruns", func() []byte {
			b := setChunkSize(good, len(good)+1000)
			// also fix the RIFF size so the walk reaches the corrupt chunk
			return b
		}, "overruns"},
		{"not whole frames", func() []byte {
			b := append([]byte(nil), good...)
			b = b[:len(b)-1] // truncate one byte of PCM; RIFF size now smaller
			// the data chunk still declares its old length → overruns the file
			return b
		}, "overruns"},
		{"non-PCM fmt", func() []byte {
			data := wavDataOnly(good)
			return wavFormat(3, 1, 24000, 16, data) // format 3 = IEEE float
		}, "not PCM"},
		{"inconsistent byte rate", func() []byte {
			data := wavDataOnly(good)
			b := wavFormat(1, 1, 24000, 16, data)
			// corrupt the byte-rate field (file offset 28..32)
			b[28] = 9
			return b
		}, "byte rate"},
		{"absurd sample rate low", func() []byte {
			return sineWAV(3000, 1, 220, 0.4)
		}, "sample rate 3000 Hz outside"},
		{"absurd sample rate high", func() []byte {
			return sineWAV(192000, 1, 220, 0.4)
		}, "sample rate 192000 Hz outside"},
		{"overlong duration", func() []byte {
			// 121 s of sine at 24 kHz mono 16-bit: ~5.8 MB, allocable in tests.
			return sineWAV(24000, 121, 220, 0.4)
		}, "exceeds the 2m0s ceiling"},
		{"all silent 16-bit", func() []byte {
			return silenceWAV(24000, 1, 16)
		}, "all-silent"},
		{"all silent 8-bit", func() []byte {
			return silenceWAV(24000, 1, 8)
		}, "all-silent"},
		{"clipped", func() []byte {
			return clippedWAV(24000, 1)
		}, "clipped"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ValidateAudio(tc.mutate())
			var pe *providers.Error
			if !errors.As(err, &pe) {
				t.Fatalf("error = %T %v, want *providers.Error", err, err)
			}
			if pe.Kind != providers.KindAudioInvalid {
				t.Errorf("kind = %s, want %s", pe.Kind, providers.KindAudioInvalid)
			}
			if !strings.Contains(pe.Msg, tc.wantSubstr) {
				t.Errorf("reason %q does not explain the failure %q", tc.wantSubstr, pe.Msg)
			}
		})
	}
}

// TestValidateAcceptsOtherBitDepths: 8/16/24/32-bit PCM are all legal
// checkpoint output formats; the validator must accept each with a
// distinguishable duration.
func TestValidateAcceptsOtherBitDepths(t *testing.T) {
	mono16 := wavDataOnly(sineWAV(24000, 1, 220, 0.4))
	cases := []struct {
		bits int
		data []byte
	}{
		{8, to8Bit(mono16)},
		{16, mono16},
		{24, to24Bit(mono16)},
		{32, to32Bit(mono16)},
	}
	for _, tc := range cases {
		wav := wavFormat(1, 1, 24000, tc.bits, tc.data)
		info, err := ValidateAudio(wav)
		if err != nil {
			t.Errorf("%d-bit: rejected: %v", tc.bits, err)
			continue
		}
		if info.BitsPerSample != tc.bits {
			t.Errorf("%d-bit: parsed bits = %d", tc.bits, info.BitsPerSample)
		}
		if info.Duration < 990*time.Millisecond || info.Duration > 1010*time.Millisecond {
			t.Errorf("%d-bit: duration = %s", tc.bits, info.Duration)
		}
	}
}

// TestValidateExtraChunksIgnored: a zero-size LIST chunk between fmt and
// data must not confuse the walker.
func TestValidateExtraChunksIgnored(t *testing.T) {
	good := sineWAV(24000, 1, 220, 0.4)
	data := good[44:]
	b := buildWithExtraChunk(data)
	info, err := ValidateAudio(b)
	if err != nil {
		t.Fatalf("extra LIST chunk broke validation: %v", err)
	}
	if info.SampleRate != 24000 {
		t.Errorf("rate = %d", info.SampleRate)
	}
}

// TestValidateRejectsTrailingJunkBeyondRIFFSize: bytes beyond the declared
// RIFF container are not part of the file.
func TestValidateRejectsTrailingJunkBeyondRIFFSize(t *testing.T) {
	good := sineWAV(24000, 1, 220, 0.4)
	// Shrink the RIFF size field so it no longer covers the full file.
	b := append([]byte(nil), good...)
	putU32(b, 4, 36+uint32(len(good)-44-8)) // covers only up to mid-data
	_, err := ValidateAudio(b)
	if err == nil {
		t.Fatal("a file longer than its RIFF size must be rejected")
	}
}

// --- bit-depth converters for the table above ------------------------------

func to8Bit(mono16 []byte) []byte {
	out := make([]byte, len(mono16)/2)
	for i := range out {
		s := int16(int(mono16[2*i]) | int(mono16[2*i+1])<<8)
		out[i] = byte(int(s/256) + 128)
	}
	return out
}

func to24Bit(mono16 []byte) []byte {
	n := len(mono16) / 2
	out := make([]byte, n*3)
	for i := 0; i < n; i++ {
		s := int16(int(mono16[2*i]) | int(mono16[2*i+1])<<8)
		v := int32(s) << 8
		out[3*i] = byte(v)
		out[3*i+1] = byte(v >> 8)
		out[3*i+2] = byte(v >> 16)
	}
	return out
}

func to32Bit(mono16 []byte) []byte {
	out := make([]byte, len(mono16)*2)
	for i := 0; i < len(mono16)/2; i++ {
		s := int32(int16(int(mono16[2*i])|int(mono16[2*i+1])<<8)) << 16
		out[4*i] = byte(s)
		out[4*i+1] = byte(s >> 8)
		out[4*i+2] = byte(s >> 16)
		out[4*i+3] = byte(s >> 24)
	}
	return out
}

// buildWithExtraChunk assembles RIFF/WAVE with fmt, a zero-size LIST chunk,
// and data.
func buildWithExtraChunk(data []byte) []byte {
	out := make([]byte, 0, 8+24+8+len(data))
	out = append(out, "RIFF"...)
	total := uint32(4 + 24 + 8 + 8 + len(data)) // WAVE + fmt + LIST + data header + payload
	out = appendU32(out, total)
	out = append(out, "WAVE"...)
	out = append(out, "fmt "...)
	out = appendU32(out, 16)
	out = appendU16(out, 1)
	out = appendU16(out, 1)
	out = appendU32(out, 24000)
	out = appendU32(out, 48000)
	out = appendU16(out, 2)
	out = appendU16(out, 16)
	out = append(out, "LIST"...)
	out = appendU32(out, 0)
	out = append(out, "data"...)
	out = appendU32(out, uint32(len(data)))
	out = append(out, data...)
	return out
}

func putU32(b []byte, off int, v uint32) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
}
