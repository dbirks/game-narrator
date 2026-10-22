// The pitch leg of the A/V harness's voice-classification (docs/adr/0162):
// the median fundamental frequency (F0) of one utterance's clip, measured
// over its voiced frames with the YIN estimator, in pure Go — no numpy, no
// external pitch library. The measured F0 feeds the play-loop's per-line
// voice class (adult male / adult female / child) so the orchestrator can
// tell whether a line sounded like the designed character or fell back to
// the narrator's or another voice's pitch.

package speech

import (
	"encoding/binary"
	"math"
)

// PitchVoiceClass bounds the voiced-frame F0 the way the ADR-0162 voice
// classes name them: below the adult-male floor the voice reads "low
// (adult male)", from the adult-female floor to the child ceiling it reads
// "mid (adult female)", above the child ceiling it reads "high (child)".
const (
	PitchLowAdultMaleHz = 165.0
	PitchHighChildHz    = 255.0
	PitchVoicedFrameRMS = 0.01 // the voiced-frame floor, ~-40 dBFS (the harness's onset threshold)
)

// Pitch is one utterance's pitch measurement: the median F0 (Hz) over the
// clip's voiced frames and the number of frames that contributed (0 when no
// frame was voiced — a silent or unmeasurable clip measures no pitch, never
// a fabricated one).
type Pitch struct {
	F0           float64 // median voiced-frame F0 in Hz; 0 when no frame was voiced
	VoicedFrames int     // the count of voiced frames the median was taken over
}

// MeasurePitch transcribes-free: decodes the WAV, frames it, and returns
// the median F0 over the frames whose RMS clears the voiced-frame floor.
// It never errors on audio content — an unmeasurable clip returns zero (the
// caller reports the line as unscored, not as a pitch).
func MeasurePitch(wav []byte) (Pitch, error) {
	pcm, err := DecodeWAV(wav)
	if err != nil {
		return Pitch{}, err
	}
	if pcm.SampleRate <= 0 || pcm.BitsPerSample != 16 || pcm.Frames() == 0 {
		return Pitch{}, nil
	}
	frameMs := 45.0 // the voiced-frame window: two periods of a 60 Hz voice plus the lag span YIN searches
	strideMs := 10.0
	frameSamples := int(math.Round(float64(pcm.SampleRate) * frameMs / 1000.0))
	stride := int(math.Round(float64(pcm.SampleRate) * strideMs / 1000.0))
	if frameSamples < 2 {
		return Pitch{}, nil
	}
	var f0s []float64
	for start := 0; start+frameSamples <= int(pcm.Frames()); start += stride {
		f0, ok := yinFrame(pcm.Data, start, frameSamples, pcm)
		if ok {
			f0s = append(f0s, f0)
		}
	}
	if len(f0s) == 0 {
		return Pitch{}, nil
	}
	vals := append([]float64(nil), f0s...)
	sortFloats(vals)
	n := len(vals)
	median := vals[n/2]
	if n%2 == 0 {
		median = (vals[n/2-1] + vals[n/2]) / 2
	}
	return Pitch{F0: median, VoicedFrames: n}, nil
}

// yinFrame measures one frame's F0 with YIN (the autocorrelation-based
// estimator: cumulative-difference threshold on the lag search, parabolic
// interpolation of the period, the periodicity criterion for voicing) and
// reports whether the frame voiced.
func yinFrame(data []byte, start, frameSamples int, pcm WAVPCM) (float64, bool) {
	sr := pcm.SampleRate
	base := start * pcm.Channels * 2
	var sum, sumSq float64
	for i := 0; i < frameSamples; i++ {
		idx := base + i*pcm.Channels*2
		if idx+2 > len(data) {
			return 0, false
		}
		x := float64(int16(binary.LittleEndian.Uint16(data[idx:idx+2]))) / 32768.0
		sum += x
		sumSq += x * x
	}
	rms := math.Sqrt(sumSq / float64(frameSamples))
	if rms < PitchVoicedFrameRMS {
		return 0, false // below the voiced-frame floor: silence, not voice
	}
	x := make([]float64, frameSamples)
	for i := range x {
		idx := base + i*pcm.Channels*2
		x[i] = float64(int16(binary.LittleEndian.Uint16(data[idx:idx+2]))) / 32768.0
	}
	f0 := yinF0(x, sr)
	if f0 <= 0 {
		return 0, false
	}
	return f0, true
}

// yinF0 estimates one frame's fundamental frequency (Hz) with YIN (de
// Cheveigné & Kawahara 2002): the difference function d(τ) = Σ (x[i] −
// x[i+τ])², its cumulative-mean normalization d'(τ), the first τ whose d'
// dips under the absolute threshold (taken at its local minimum, which is
// what keeps the estimator off the octave above), and parabolic
// interpolation of that minimum. Returns 0 when the frame does not voice.
func yinF0(x []float64, sr int) float64 {
	const (
		fMin      = 60.0  // lowest voice F0 measured
		fMax      = 600.0 // highest (a child's squeal)
		threshold = 0.12
	)
	maxLag := int(float64(sr) / fMin)
	minLag := int(float64(sr) / fMax)
	w := len(x) - maxLag
	if minLag < 2 || w < maxLag/2 {
		return 0 // the frame is too short for two periods of the lowest F0
	}
	d := make([]float64, maxLag+2)
	for tau := 1; tau <= maxLag+1 && tau < len(x)-w+1; tau++ {
		var s float64
		for i := 0; i < w; i++ {
			v := x[i] - x[i+tau]
			s += v * v
		}
		d[tau] = s
	}
	cmnd := make([]float64, len(d))
	cmnd[0] = 1
	var run float64
	for tau := 1; tau < len(d); tau++ {
		run += d[tau]
		if run == 0 {
			cmnd[tau] = 1
		} else {
			cmnd[tau] = d[tau] * float64(tau) / run
		}
	}
	tau := 0
	for t := minLag; t < maxLag; t++ {
		if cmnd[t] < threshold {
			for t+1 < maxLag && cmnd[t+1] < cmnd[t] {
				t++
			}
			tau = t
			break
		}
	}
	if tau == 0 {
		return 0
	}
	period := float64(tau)
	if tau > 1 && tau+1 < len(cmnd) {
		a, b, c := cmnd[tau-1], cmnd[tau], cmnd[tau+1]
		if den := a - 2*b + c; den != 0 {
			period += 0.5 * (a - c) / den
		}
	}
	f0 := float64(sr) / period
	if f0 < fMin || f0 > fMax {
		return 0
	}
	return f0
}

// VoiceClass names an F0's voice class (ADR-0162): below PitchLowAdultMaleHz
// the voice is "low (adult male)", from the adult-female floor up to the
// child ceiling it is "mid (adult female)", above the child ceiling it is
// "high (child)".
func VoiceClass(f0 float64) string {
	switch {
	case f0 > 0 && f0 < PitchLowAdultMaleHz:
		return "low (adult male)"
	case f0 >= PitchLowAdultMaleHz && f0 <= PitchHighChildHz:
		return "mid (adult female)"
	case f0 > PitchHighChildHz:
		return "high (child)"
	default:
		return "unmeasured"
	}
}

// sortFloats sorts a slice of floats ascending (small local helper, the
// speech package does not otherwise carry one).
func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		key := v[i]
		j := i - 1
		for j >= 0 && v[j] > key {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = key
	}
}
