package alsahw

import (
	"fmt"
	"math"

	"github.com/dbirks/game-narrator/internal/speech"
)

// ConvertToHW converts a validated RIFF/WAVE to the sink's negotiated
// format: S16_LE interleaved at Params.Rate / Params.Channels. The TTS
// source is 24 kHz mono (16- or 32-bit); the conversion is in-process so
// the device stream stays exactly the proven hw_params — no plugin, no
// subprocess, no kernel-side plug.
//
// Resampling is linear interpolation in the time domain. For the product
// case (24 kHz source, 48 kHz target, a pure 2x upsampling) the TTS band
// (content ≤ 12 kHz by the source's own Nyquist) is reproduced at full
// amplitude and its imaging products land at ≥ 36 kHz, inaudible at the
// receiver. It is deliberately not a windowed-sinc bank: the narration
// product has no audible reason to pay for one, and the hwsmoke FFT
// assertion on the deterministic tone bounds any future drift.
func ConvertToHW(wav []byte, p Params) ([]byte, error) {
	pcm, err := speech.DecodeWAV(wav)
	if err != nil {
		return nil, fmt.Errorf("alsahw: %w", err)
	}
	return convertPCM(pcm, p)
}

// convertPCM converts raw interleaved PCM (16- or 32-bit, 1-2 channels,
// any source rate) to S16_LE interleaved at the target rate/channels.
func convertPCM(src speech.WAVPCM, p Params) ([]byte, error) {
	if p.Format != "S16_LE" {
		return nil, fmt.Errorf("alsahw: conversion target must be S16_LE, got %q", p.Format)
	}
	if src.Channels < 1 || src.Channels > 2 {
		return nil, fmt.Errorf("alsahw: unsupported source channels %d", src.Channels)
	}
	srcSamples := decodeChannels(src)
	if len(srcSamples) == 0 {
		return nil, fmt.Errorf("alsahw: no samples to convert")
	}
	// Per-channel resample (linear) to the target rate.
	outCh := make([][]float64, p.Channels)
	for ch := 0; ch < p.Channels; ch++ {
		srcCh := pickSourceChannel(srcSamples, ch, src.Channels)
		outCh[ch] = resampleLinear(srcCh, src.SampleRate, p.Rate)
	}
	// Interleave and encode to S16_LE.
	frames := len(outCh[0])
	for ch := 1; ch < p.Channels; ch++ {
		if len(outCh[ch]) != frames {
			// A shared length is the resampler's contract; a mismatch is a
			// converter defect, not a caller condition.
			return nil, fmt.Errorf("alsahw: resampler length mismatch (%d vs %d)", len(outCh[ch]), frames)
		}
	}
	out := make([]byte, frames*p.Channels*2)
	for f := 0; f < frames; f++ {
		for ch := 0; ch < p.Channels; ch++ {
			// The planes are full-scale [-1, 1]; scale to the S16 range.
			v := outCh[ch][f] * 32767.0
			if v > 32767 {
				v = 32767
			}
			if v < -32768 {
				v = -32768
			}
			s := int16(v)
			o := (f*p.Channels + ch) * 2
			out[o] = byte(uint16(s) & 0xff)
			out[o+1] = byte(uint16(s) >> 8)
		}
	}
	return out, nil
}

// decodeChannels turns interleaved 16/32-bit PCM into per-channel float64
// sample planes in full-scale [-1, 1].
func decodeChannels(src speech.WAVPCM) [][]float64 {
	planes := make([][]float64, src.Channels)
	for ch := range planes {
		planes[ch] = make([]float64, 0, src.Frames())
	}
	switch src.BitsPerSample {
	case 16:
		for i := 0; i+1 < len(src.Data); i += 2 {
			s := int16(uint16(src.Data[i]) | uint16(src.Data[i+1])<<8)
			planes[i/2%src.Channels] = append(planes[i/2%src.Channels], float64(s)/32768.0)
		}
	case 32:
		for i := 0; i+3 < len(src.Data); i += 4 {
			v := uint32(src.Data[i]) | uint32(src.Data[i+1])<<8 | uint32(src.Data[i+2])<<16 | uint32(src.Data[i+3])<<24
			s := int32(v)
			planes[i/4%src.Channels] = append(planes[i/4%src.Channels], float64(s)/2147483648.0)
		}
	default:
		// DecodeWAV admits only 16/32; this is the converter's own backstop.
	}
	return planes
}

// pickSourceChannel maps a target channel onto the source: mono sources
// feed every target channel; a stereo target's channel c is the source's
// own channel c. (A 1→1 or 2→2 mapping is the identity; there is no other
// product case, and there is no matrixing to invent.)
func pickSourceChannel(src [][]float64, ch, srcChannels int) []float64 {
	if srcChannels == 1 {
		return src[0]
	}
	return src[ch]
}

// resampleLinear resamples x from inRate to outRate by linear
// interpolation (time-domain; see ConvertToHW for the product rationale).
// The output has round(len(x)*outRate/inRate) samples.
func resampleLinear(x []float64, inRate, outRate int) []float64 {
	if inRate == outRate {
		return append([]float64(nil), x...)
	}
	if len(x) == 0 {
		return nil
	}
	if len(x) == 1 {
		// Nothing to interpolate across; hold the single sample for the
		// whole output span.
		n := len1(len(x), inRate, outRate)
		out := make([]float64, n)
		for i := range out {
			out[i] = x[0]
		}
		return out
	}
	n := len1(len(x), inRate, outRate)
	out := make([]float64, n)
	ratio := float64(inRate) / float64(outRate)
	last := len(x) - 1
	for i := range out {
		pos := float64(i) * ratio
		i0 := int(pos)
		if i0 > last {
			i0 = last
		}
		i1 := i0 + 1
		if i1 > last {
			i1 = last
		}
		frac := pos - float64(i0)
		out[i] = (1-frac)*x[i0] + frac*x[i1]
	}
	return out
}

// len1 is the output length of a rate conversion: the sample count that
// keeps the duration, rounded to the nearest frame.
func len1(inLen, inRate, outRate int) int {
	return (inLen*outRate + inRate/2) / inRate
}

// ApplyGain scales S16_LE interleaved PCM by g (clamped to the sample
// range). g = 0 is silence and is applied, not skipped (the runtime's
// master-volume rule: a stored 0 is the user's silence calibration).
func ApplyGain(data []byte, g float64) []byte {
	if g < 0 {
		g = 0
	}
	if g > 1 {
		g = 1
	}
	if g == 1 {
		return data
	}
	out := make([]byte, len(data))
	for i := 0; i+1 < len(data); i += 2 {
		s := int16(uint16(data[i]) | uint16(data[i+1])<<8)
		v := int(math.Round(float64(s) * g))
		if v > 32767 {
			v = 32767
		}
		if v < -32768 {
			v = -32768
		}
		o := int16(v)
		out[i], out[i+1] = byte(uint16(o)&0xff), byte(uint16(o)>>8)
	}
	return out
}
