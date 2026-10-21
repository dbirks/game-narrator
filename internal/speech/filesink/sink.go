// Package filesink is the evidence AudioSink (docs/spec/07 "audio sink
// abstraction also includes dummy/file/capture for CI and tests";
// docs/spec/11: "a WAV file is not proof it was audible; assert file sink
// samples AND playback device/sink events"). It records, in real time,
// exactly the samples that WOULD have been delivered: when the playback is
// stopped, the written WAV is exactly the delivered prefix — shorter than the
// source — so a test can diff delivered samples against the source instead of
// trusting a "file exists" check.
//
// Semantics:
//   - pacing is real time (one utterance of N ms takes N ms), in ~10 ms
//     slices, so Stop() is honored within one slice + the requested fade;
//   - Gain is applied to the recorded samples (the evidence is "what would
//     have been heard", including the device-level volume);
//   - FadeOutMs fades the natural end; Stop(fade) fades the last `fade` of
//     the delivered tail (0 = hard cut);
//   - the output WAV is written only at completion/stop, to
//     <Dir>/gen<generation>-<seq>.wav, so a half-played utterance never
//     leaves a half-valid file.
package filesink

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// Sink writes what would have played, one WAV per utterance.
type Sink struct {
	Dir string

	mu  sync.Mutex
	seq int
}

// New returns a file sink writing into dir (created if missing).
func New(dir string) (*Sink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("filesink: %w", err)
	}
	return &Sink{Dir: dir}, nil
}

// Close is a no-op for the file sink: evidence files are written by each
// playback and no handles are held between utterances (AudioSink contract).
func (s *Sink) Close() error { return nil }

// Play is a filesink.PlayHandle with the output path exposed for assertions.
type Play struct {
	h    *handle
	path string // final WAV path (valid after Done)
}

// Path returns the final WAV file path.
func (p *Play) Path() string { return p.path }

// Stop implements speech.PlayHandle.
func (p *Play) Stop(fade time.Duration) { p.h.Stop(fade) }

// Done implements speech.PlayHandle.
func (p *Play) Done() <-chan speech.PlayResult { return p.h.Done() }

type handle struct {
	done chan speech.PlayResult
	stop chan time.Duration
	once sync.Once
}

func (h *handle) Stop(fade time.Duration) {
	select {
	case h.stop <- fade:
	default:
	}
}

func (h *handle) Done() <-chan speech.PlayResult { return h.done }

// Play implements speech.AudioSink.
func (s *Sink) Play(ctx context.Context, req speech.PlayRequest) (speech.PlayHandle, error) {
	pcm, err := speech.DecodeWAV(req.WAV)
	if err != nil {
		return nil, err
	}
	frameBytes := pcm.Channels * (pcm.BitsPerSample / 8)
	if frameBytes == 0 {
		return nil, fmt.Errorf("filesink: empty audio")
	}
	s.mu.Lock()
	s.seq++
	seq := s.seq
	s.mu.Unlock()

	name := fmt.Sprintf("gen%d-%s-%d.wav", req.Generation, SafeName(req.UtteranceID), seq)
	path := filepath.Join(s.Dir, name)
	h := &handle{done: make(chan speech.PlayResult, 1), stop: make(chan time.Duration, 1)}
	gain := req.Gain
	if gain <= 0 {
		gain = 1.0
	}
	fadeOut := req.FadeOutMs
	if fadeOut < 0 {
		fadeOut = 0
	}

	go s.run(ctx, h, pcm, frameBytes, path, gain, fadeOut)
	return &Play{h: h, path: path}, nil
}

// run paces the samples and records the delivered prefix.
func (s *Sink) run(ctx context.Context, h *handle, pcm speech.WAVPCM, frameBytes int, path string, gain float64, fadeOutMs int) {
	total := pcm.Frames()
	buf := make([]byte, 0, len(pcm.Data))
	started := time.Time{}
	end := time.Time{}

	writeAll := func() {
		// Assemble the evidence WAV from the delivered prefix only.
		out := speech.EncodeWAV(buf, pcm.SampleRate, pcm.Channels, pcm.BitsPerSample)
		tmp := path + ".tmp"
		_ = os.WriteFile(tmp, out, 0o600)
		_ = os.Rename(tmp, path)
	}
	finish := func(interrupted bool, err error) {
		h.once.Do(func() {
			writeAll()
			h.done <- speech.PlayResult{Started: started, Ended: end, Interrupted: interrupted, Err: err}
		})
	}

	// Slice the stream into ~10 ms pieces; each slice is one delivery unit.
	sliceFrames := pcm.SampleRate / 100
	if sliceFrames < 1 {
		sliceFrames = 1
	}
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()

	for written := 0; written < total; {
		select {
		case <-ctx.Done():
			end = time.Now()
			finish(true, ctx.Err())
			return
		case fade := <-h.stop:
			end = time.Now().Add(fade)
			// Deliver this last slice, then fade its tail by `fade`.
			if written+sliceFrames > total {
				sliceFrames = total - written
			}
			buf = append(buf, pcm.Data[written*frameBytes:(written+sliceFrames)*frameBytes]...)
			scaleSamples16(buf[len(buf)-sliceFrames*frameBytes:], gain, pcm.BitsPerSample)
			if fade > 0 {
				fadeTail16(buf, pcm, int(fade.Milliseconds()))
			}
			written += sliceFrames
			time.Sleep(fade)
			finish(true, nil)
			return
		case <-t.C:
			if started.IsZero() {
				started = time.Now()
			}
			if written+sliceFrames > total {
				sliceFrames = total - written
			}
			buf = append(buf, pcm.Data[written*frameBytes:(written+sliceFrames)*frameBytes]...)
			scaleSamples16(buf[len(buf)-sliceFrames*frameBytes:], gain, pcm.BitsPerSample)
			written += sliceFrames
		}
	}
	// Natural end: fade the tail by fadeOutMs.
	if fadeOutMs > 0 {
		fadeTail16(buf, pcm, fadeOutMs)
	}
	end = time.Now()
	finish(false, nil)
}

// scaleSamples16 multiplies 16-bit LE samples in b by gain, clamped to full
// scale. 32-bit evidence files pass through unmodified (the evidence sink
// does not mix; it records).
func scaleSamples16(b []byte, gain float64, bits int) {
	if gain == 1.0 || bits != 16 {
		return
	}
	for i := 0; i+2 <= len(b); i += 2 {
		v := int16(uint16(b[i]) | uint16(b[i+1])<<8)
		f := float64(v) * gain
		if f > 32767 {
			f = 32767
		}
		if f < -32768 {
			f = -32768
		}
		s := int16(f)
		b[i], b[i+1] = byte(s), byte(s>>8)
	}
}

// fadeTail16 applies a linear ramp over the last nMs milliseconds of the
// 16-bit buffer (no-op for other depths).
func fadeTail16(buf []byte, pcm speech.WAVPCM, nMs int) {
	if nMs <= 0 || pcm.BitsPerSample != 16 {
		return
	}
	n := int(float64(pcm.SampleRate) * float64(nMs) / 1000)
	frames := pcm.Frames()
	if n > frames {
		n = frames
	}
	if n <= 0 {
		return
	}
	frameBytes := pcm.Channels * (pcm.BitsPerSample / 8)
	start := (frames - n) * frameBytes
	for f := 0; f < n; f++ {
		off := start + f*frameBytes
		if off+2 > len(buf) {
			return
		}
		scale := float64(f+1) / float64(n)
		v := int16(uint16(buf[off]) | uint16(buf[off+1])<<8)
		nv := int16(float64(v) * scale)
		buf[off], buf[off+1] = byte(nv), byte(nv>>8)
	}
}

// SafeName keeps utterance IDs usable as a filename component: the file the
// sink stages for one playback is gen<generation>-<SafeName(utteranceID)>
// -<seq>.wav, so the occurrence's SafeName is the occurrence's part of the
// evidence file name (the scenario harness's per-utterance evidence check
// reads it back, issue #36 ADR-0132).
func SafeName(s string) string {
	if s == "" {
		return "u"
	}
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s) && i < 48; i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_' {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return "u"
	}
	return string(out)
}
