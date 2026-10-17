// Package tts is the client adapter for the vLLM-Omni Qwen3-TTS checkpoints
// (docs/spec/06 "Provider registry": voice_design = VoiceDesign checkpoint,
// voice_clone = Base checkpoint).
//
// It covers the TTSProvider contract from docs/spec/06: Design (VoiceDesign
// checkpoint), Generate (Base checkpoint), Register/List/Remove against the
// Base service's voice registry, and the negotiated inline-reference fallback
// (docs/spec/06 "Routing, reliability and replicas"). Every WAV the server
// returns is validated with the invariants from docs/spec/07 ("Failure
// behavior and test coverage": VoiceDesign returns malformed/noisy audio →
// quarantine, do not approve); every failure is classified as a
// *providers.Error from the docs/spec/06 failure taxonomy.
//
// Naming and consent rules: docs/spec/06 "never rely on friendly voice names"
// (names are namespaced and immutable), docs/spec/14 "Voice consent" (the
// consent metadata is passed truthfully and registration is refused without
// it), ADR-0005 (the app-owned asset store is the source of truth; the server
// registry holds copies, reconciled per instance).
package tts

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

// Hard limits for a single generated utterance (docs/spec/07: canonical speech
// is segmented at sentence boundaries, so anything outside this band is a
// server fault, not a line the runtime asked for).
const (
	MinSampleRate = 8000
	MaxSampleRate = 96000
	MaxDuration   = 120 * time.Second
	// minRMSPerFullScale is the RMS floor below which the audio is treated as
	// all-silent. ~-72 dBFS: real speech is orders of magnitude above it, pure
	// silence and most encoder artefacts are at or below it.
	minRMSPerFullScale = 0.00025
	// maxClippedFraction is the largest share of samples that may sit at ±full
	// scale before the audio is classified as clipped.
	maxClippedFraction = 0.01
)

// WAVInfo is the parsed, validated PCM stream of one RIFF/WAVE file.
type WAVInfo struct {
	SampleRate    int
	Channels      int
	BitsPerSample int
	DataBytes     int
	Duration      time.Duration
}

// ValidateAudio parses b as a RIFF/WAVE file and enforces the invariants a
// generated utterance must satisfy:
//
//   - RIFF "WAVE" container with a PCM (format 1) fmt chunk;
//   - fmt field consistency (byte rate and block align match channels, rate
//     and bit depth) and an integer number of sample frames in the data chunk;
//   - sample rate inside [8 kHz, 96 kHz];
//   - duration strictly inside (0, 120 s);
//   - not all-silent (RMS above minRMSPerFullScale of full scale);
//   - not clipped (fewer than 1 % of the samples at ±full scale).
//
// The error, when present, is always a *providers.Error of kind
// audio_invalid whose message names the failed invariant (docs/spec/06
// failure taxonomy). Callers quarantine such audio instead of approving it.
func ValidateAudio(b []byte) (WAVInfo, error) {
	info, reason, ok := parseAndCheck(b)
	if !ok {
		return WAVInfo{}, invalid(reason)
	}
	return info, nil
}

func invalid(reason string) *providers.Error {
	e := providers.NewError(providers.KindAudioInvalid, 0, "tts: "+reason)
	e.Retryable = false // a malformed artifact is not fixed by retrying the same request
	return e
}

// parseAndCheck returns the parsed info and, when validation fails, the human
// reason. It is split from ValidateAudio so tests can assert reasons verbatim.
func parseAndCheck(b []byte) (WAVInfo, string, bool) {
	var info WAVInfo

	if len(b) < 12+16+4 {
		return info, fmt.Sprintf("truncated RIFF header (%d bytes)", len(b)), false
	}
	if string(b[0:4]) != "RIFF" {
		return info, fmt.Sprintf("not a RIFF file (magic %q)", b[0:4]), false
	}
	if string(b[8:12]) != "WAVE" {
		return info, fmt.Sprintf("RIFF form type %q is not WAVE", b[8:12]), false
	}
	riffSize := binary.LittleEndian.Uint32(b[4:8])
	// The container is the declared RIFF size, capped at the actual file
	// length: trailing bytes beyond it are junk to skip, a file longer than
	// the declaration is not valid. A 0xFFFFFFFF declaration (stream marker)
	// caps at the file length.
	walkEnd := len(b)
	if declared := int(riffSize) + 8; declared > 0 && declared < walkEnd {
		walkEnd = declared
	}
	if walkEnd < 12+16+4 {
		return info, fmt.Sprintf("RIFF size field %d too small for the %d-byte payload", riffSize, len(b)-8), false
	}

	var haveFmt, haveData bool
	offset := 12
	for offset+8 <= walkEnd {
		chunkID := b[offset : offset+4]
		size := int(binary.LittleEndian.Uint32(b[offset+4 : offset+8]))
		payload := offset + 8
		if payload+size > len(b) {
			return info, fmt.Sprintf("chunk %q length %d overruns the file", chunkID, size), false
		}
		if payload+size > walkEnd {
			return info, fmt.Sprintf("chunk %q length %d overruns the RIFF container", chunkID, size), false
		}
		body := b[payload : payload+size]
		switch string(chunkID) {
		case "fmt ":
			if !haveFmt {
				reason := checkFmt(&info, body)
				if reason != "" {
					return info, reason, false
				}
				haveFmt = true
			}
		case "data":
			if !haveData {
				info.DataBytes = len(body)
				haveData = true
			}
		}
		// Chunks are word-aligned: odd-sized payloads are padded with one byte.
		offset = payload + size
		if size%2 == 1 {
			offset++
		}
	}
	if !haveFmt {
		return info, "no fmt chunk", false
	}
	if !haveData {
		return info, "no data chunk", false
	}

	if info.SampleRate < MinSampleRate || info.SampleRate > MaxSampleRate {
		return info, fmt.Sprintf("sample rate %d Hz outside [%d, %d]", info.SampleRate, MinSampleRate, MaxSampleRate), false
	}
	byteRate := info.SampleRate * info.Channels * info.BitsPerSample / 8
	if byteRate <= 0 {
		return info, "zero byte rate in fmt chunk", false
	}
	if info.DataBytes == 0 {
		return info, "empty data chunk", false
	}
	blockAlign := info.Channels * info.BitsPerSample / 8
	if info.DataBytes%blockAlign != 0 {
		return info, fmt.Sprintf("data chunk %d bytes is not a whole number of sample frames (block align %d)", info.DataBytes, blockAlign), false
	}
	info.Duration = time.Duration(float64(info.DataBytes) / float64(byteRate) * float64(time.Second))
	if info.Duration <= 0 {
		return info, "duration is zero", false
	}
	if info.Duration >= MaxDuration {
		return info, fmt.Sprintf("duration %s exceeds the %s ceiling", info.Duration, MaxDuration), false
	}

	// The content checks need the raw samples, so look the data payload up
	// again (it is the last meaningful chunk in every file the checkpoints
	// emit, but the walk is the only form-agnostic way to find it).
	data, ok := findChunk(b, "data")
	if !ok {
		return info, "data chunk vanished during walk", false
	}
	if reason := checkContent(data, info.BitsPerSample); reason != "" {
		return info, reason, false
	}
	return info, "", true
}

func findChunk(b []byte, id string) ([]byte, bool) {
	offset := 12
	for offset+8 <= len(b) {
		if string(b[offset:offset+4]) == id {
			size := int(binary.LittleEndian.Uint32(b[offset+4 : offset+8]))
			if offset+8+size > len(b) {
				return nil, false
			}
			return b[offset+8 : offset+8+size], true
		}
		sizeP := offset + 8
		if sizeP > len(b) {
			break
		}
		size := int(binary.LittleEndian.Uint32(b[offset+4 : offset+8]))
		offset = sizeP + size
		if size%2 == 1 {
			offset++
		}
	}
	return nil, false
}

func checkFmt(info *WAVInfo, body []byte) string {
	if len(body) < 16 {
		return fmt.Sprintf("fmt chunk %d bytes, want at least 16", len(body))
	}
	format := binary.LittleEndian.Uint16(body[0:2])
	if format != 1 {
		return fmt.Sprintf("fmt audio format %d is not PCM (1)", format)
	}
	info.Channels = int(binary.LittleEndian.Uint16(body[2:4]))
	info.SampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
	byteRate := binary.LittleEndian.Uint32(body[8:12])
	info.BitsPerSample = int(binary.LittleEndian.Uint16(body[14:16]))
	if info.Channels < 1 || info.Channels > 8 {
		return fmt.Sprintf("channel count %d outside [1, 8]", info.Channels)
	}
	if info.BitsPerSample != 8 && info.BitsPerSample != 16 && info.BitsPerSample != 24 && info.BitsPerSample != 32 {
		return fmt.Sprintf("bit depth %d not supported (8/16/24/32 PCM)", info.BitsPerSample)
	}
	if info.SampleRate <= 0 {
		return "fmt sample rate is zero"
	}
	expectedByteRate := uint32(info.SampleRate * info.Channels * info.BitsPerSample / 8)
	if byteRate != expectedByteRate {
		return fmt.Sprintf("fmt byte rate %d does not match rate/channels/bits (want %d)", byteRate, expectedByteRate)
	}
	return ""
}

// checkContent enforces the silence and clipping invariants over the raw PCM
// payload. Samples are normalised to [-1, 1) before measuring.
func checkContent(data []byte, bits int) string {
	if len(data) == 0 {
		return "empty data chunk"
	}
	var sumSq float64
	var clipped, total int64
	for i := 0; i < len(data); {
		v, n, atScale, ok := sampleAt(data, i, bits)
		if !ok {
			return fmt.Sprintf("trailing %d bytes are not whole samples of %d-bit PCM", len(data)-i, bits)
		}
		i += n
		total++
		sumSq += v * v
		if atScale {
			clipped++
		}
	}
	if sumSq/float64(total) < minRMSPerFullScale*minRMSPerFullScale {
		return fmt.Sprintf("all-silent: RMS %.2g is below the %.2g full-scale floor",
			rms(sumSq, total), minRMSPerFullScale)
	}
	if float64(clipped)/float64(total) >= maxClippedFraction {
		return fmt.Sprintf("clipped: %d of %d samples (%.2f %% at ±full scale, ceiling %.2f %%)",
			clipped, total, 100*float64(clipped)/float64(total), 100*maxClippedFraction)
	}
	return ""
}

// sampleAt decodes one sample frame (all channels) at byte offset i. It
// returns the per-channel normalised values collapsed into one RMS-relevant
// magnitude: the square root of the mean of the channel squares, the number
// of bytes consumed, and whether any channel sits at full scale.
func sampleAt(data []byte, i, bits int) (v float64, n int, atScale bool, ok bool) {
	switch bits {
	case 8:
		if i+1 > len(data) {
			return 0, 0, false, false
		}
		v = float64(int(data[i])-128) / 128.0
		atScale = data[i] == 0 || data[i] == 255
		return v, 1, atScale, true
	case 16:
		if i+2 > len(data) {
			return 0, 0, false, false
		}
		s := int16(binary.LittleEndian.Uint16(data[i:]))
		v = float64(s) / 32768.0
		atScale = s == -32768 || s == 32767
		return v, 2, atScale, true
	case 24:
		if i+3 > len(data) {
			return 0, 0, false, false
		}
		s := int32(data[i]) | int32(data[i+1])<<8 | int32(data[i+2])<<16
		if s&0x800000 != 0 {
			s |= ^int32(0) << 24
		}
		v = float64(s) / 8388608.0
		atScale = s == -8388608 || s == 8388607
		return v, 3, atScale, true
	case 32:
		if i+4 > len(data) {
			return 0, 0, false, false
		}
		s := int32(binary.LittleEndian.Uint32(data[i:]))
		v = float64(s) / 2147483648.0
		atScale = s == -2147483648 || s == 2147483647
		return v, 4, atScale, true
	default:
		return 0, 0, false, false
	}
}

func rms(sumSq float64, n int64) float64 {
	if n == 0 {
		return 0
	}
	return sqrtFloat(sumSq / float64(n))
}

// sqrtFloat is math.Sqrt kept out of the import list so the file's dependency
// surface stays obvious; the input is always non-negative by construction.
func sqrtFloat(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for {
		next := 0.5 * (z + x/z)
		if next == z {
			return next
		}
		z = next
	}
}
