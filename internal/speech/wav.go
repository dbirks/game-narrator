package speech

import (
	"encoding/binary"
	"fmt"
	"math"
)

// WAVPCM describes one decoded RIFF/WAVE PCM payload.
type WAVPCM struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	Data          []byte // raw interleaved PCM, no framing
}

// Frames returns the number of sample frames (per channel) in Data.
func (p WAVPCM) Frames() int {
	frameBytes := p.Channels * (p.BitsPerSample / 8)
	if frameBytes <= 0 {
		return 0
	}
	return len(p.Data) / frameBytes
}

// DecodeWAV parses a RIFF/WAVE file and extracts the PCM payload. It accepts
// the same invariants tts.ValidateAudio enforces for generated utterances
// (PCM format 1; 16- or 32-bit little-endian; 1-2 channels); it does NOT run
// the audio-quality checks (silence/clipping) — those are the QA loop's job.
// Sinks use this to pace samples and to write "delivered samples" evidence
// (docs/spec/11: filesink records what would have played).
func DecodeWAV(b []byte) (WAVPCM, error) {
	var p WAVPCM
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return p, fmt.Errorf("speech/wav: not a RIFF/WAVE file")
	}
	pos := 12
	for pos+8 <= len(b) {
		id := string(b[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(b[pos+4 : pos+8]))
		body := pos + 8
		if body+size > len(b) {
			return p, fmt.Errorf("speech/wav: chunk %s overruns file", id)
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return p, fmt.Errorf("speech/wav: fmt chunk too small (%d)", size)
			}
			bodyB := b[body : body+size]
			format := binary.LittleEndian.Uint16(bodyB[0:2])
			if format != 1 {
				return p, fmt.Errorf("speech/wav: format %d is not PCM (1)", format)
			}
			p.Channels = int(binary.LittleEndian.Uint16(bodyB[2:4]))
			p.SampleRate = int(binary.LittleEndian.Uint32(bodyB[4:8]))
			p.BitsPerSample = int(binary.LittleEndian.Uint16(bodyB[14:16]))
		case "data":
			p.Data = append([]byte(nil), b[body:body+size]...)
		}
		pos = body + size
		if pos&1 == 1 { // RIFF chunks are word-aligned
			pos++
		}
	}
	if p.SampleRate <= 0 || p.Channels < 1 || p.Channels > 2 {
		return p, fmt.Errorf("speech/wav: invalid sample rate %d / channels %d", p.SampleRate, p.Channels)
	}
	if p.BitsPerSample != 16 && p.BitsPerSample != 32 {
		return p, fmt.Errorf("speech/wav: unsupported bit depth %d", p.BitsPerSample)
	}
	if p.Data == nil {
		return p, fmt.Errorf("speech/wav: no data chunk")
	}
	return p, nil
}

// EncodeWAV wraps raw PCM in a minimal RIFF/WAVE header (PCM, little-endian,
// the given format). The result is exactly what a subprocess sink or a
// filesink evidence file expects on disk.
func EncodeWAV(data []byte, sampleRate, channels, bits int) []byte {
	if bits != 16 && bits != 32 {
		bits = 16
	}
	if channels < 1 {
		channels = 1
	}
	byteRate := sampleRate * channels * (bits / 8)
	blockAlign := channels * (bits / 8)
	out := make([]byte, 44+len(data))
	copy(out[0:4], "RIFF")
	binary.LittleEndian.PutUint32(out[4:8], uint32(36+len(data)))
	copy(out[8:12], "WAVE")
	copy(out[12:16], "fmt ")
	binary.LittleEndian.PutUint32(out[16:20], 16)
	binary.LittleEndian.PutUint16(out[20:22], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(out[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(out[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(out[34:36], uint16(bits))
	copy(out[36:40], "data")
	binary.LittleEndian.PutUint32(out[40:44], uint32(len(data)))
	copy(out[44:], data)
	return out
}

// MakeToneWAV renders freq Hz for dur as a sine tone, 16-bit LE at the given
// sample rate (default 44100, mono). Test-only helper: it is the reference
// signal every sink test asserts against, including the PipeWire virtual-sink
// recording (docs/spec/11: prove samples reached the sink, not just that a
// file exists).
func MakeToneWAV(freq float64, durS float64, sampleRate int) []byte {
	if sampleRate <= 0 {
		sampleRate = 44100
	}
	n := int(durS * float64(sampleRate))
	ampl := 0.25 // well inside ValidateAudio's non-silence/no-clip bounds
	data := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(ampl * 32767 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(data[i*2:], uint16(v))
	}
	return EncodeWAV(data, sampleRate, 1, 16)
}
