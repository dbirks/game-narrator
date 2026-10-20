// Package pipewire is the production AudioSink: it plays a validated WAV
// through a PipeWire sink by subprocess (pw-play by default, paplay as the
// PulseAudio-compatible fallback), targeting a NAMED sink (Target) so the
// output device identity is stable (docs/spec/07 "Audio scheduler": "Use
// USB ALSA/PipeWire sink by stable device identity").
//
// Design (ADR-0023):
//   - argv only: the sink spawns a fixed binary with fixed flags and pipes
//     the WAV into its stdin ("-" input); no shell is ever involved;
//   - stop = stop writing + SIGTERM (the task's "simplest acceptable":
//     stop writing / kill). A subprocess boundary cannot apply a true
//     sample-accurate fade; the measured stop latency is what the tests
//     report and what the ADR records (docs/spec/00: local stop target
//     200 ms, device buffering excluded and reported);
//   - evidence: on a virtual null sink, samples reaching the sink are
//     asserted by recording its monitor source (parecord) — a WAV file on
//     disk is NOT proof of playback (docs/spec/11).
package pipewire

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// Sink plays to one named PipeWire sink.
type Sink struct {
	// Target is the sink NAME (e.g. "auto_null", or the USB speaker sink on
	// hardware). Required: playing to whatever happens to be default would
	// violate the stable-device-identity rule.
	Target string
	// Binary is "pw-play" (default) or "paplay".
	Binary string
	// StreamLatency is the pw-play --latency value (the negotiated stream
	// buffer). Smaller = snappier stop; default 50ms.
	StreamLatency time.Duration
	// KillGrace is how long Stop waits for a clean exit before SIGKILL.
	KillGrace time.Duration

	// RunPath lets tests point the sink at a stand-in binary (argv evidence
	// without a PipeWire server). Empty means exec.LookPath.
	RunPath string

	mu       sync.Mutex
	children map[*exec.Cmd]struct{}
	closed   bool
}

// Play is a pipewire.PlayHandle (a speech.PlayHandle).
type PlayHandle struct {
	h *handle
}

// Stop implements speech.PlayHandle: stop writing, then SIGTERM (SIGKILL
// after KillGrace if the child misbehaves).
func (p *PlayHandle) Stop(fade time.Duration) { p.h.Stop(fade) }

// Done implements speech.PlayHandle.
func (p *PlayHandle) Done() <-chan speech.PlayResult { return p.h.Done() }

type handle struct {
	sink    *Sink
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	done    chan speech.PlayResult
	stop    chan time.Duration
	started time.Time
	// firstWrite is the unixNano stamp of the first successful write to the
	// player's stdin (0 = never): the moment the device buffer accepted
	// samples. The scheduler reports it as the playback's output latency
	// (ADR-0074, docs/spec/13 "output latency").
	firstWrite atomic.Int64
	once       sync.Once
	stopped    atomic.Bool
}

func (h *handle) Stop(fade time.Duration) {
	select {
	case h.stop <- fade:
	default:
	}
}

func (h *handle) Done() <-chan speech.PlayResult { return h.done }

// New returns a sink for target.
func New(target string) (*Sink, error) {
	if target == "" {
		return nil, fmt.Errorf("pipewire: Target is required (stable device identity, docs/spec/07)")
	}
	return &Sink{
		Target:        target,
		Binary:        "pw-play",
		StreamLatency: 50 * time.Millisecond,
		KillGrace:     150 * time.Millisecond,
		children:      map[*exec.Cmd]struct{}{},
	}, nil
}

// Play implements speech.AudioSink. The child is spawned and the WAV is
// streamed to its stdin in ~100 ms chunks; Play returns after spawn, so it
// never blocks the caller for the duration of the utterance.
func (s *Sink) Play(ctx context.Context, req speech.PlayRequest) (speech.PlayHandle, error) {
	pcm, err := speech.DecodeWAV(req.WAV)
	if err != nil {
		return nil, fmt.Errorf("pipewire: %w", err)
	}
	_ = pcm // format is validated; pw-play/paplay negotiate conversion
	bin := s.Binary
	if bin == "" {
		bin = "pw-play"
	}
	var args []string
	switch bin {
	case "pw-play":
		args = []string{"--target=" + s.Target, "--latency=" + s.StreamLatency.String(), "-"}
	case "paplay":
		args = []string{"--device=" + s.Target, "-"}
	default:
		return nil, fmt.Errorf("pipewire: unsupported binary %q", bin)
	}
	path := bin
	if s.RunPath != "" {
		path = s.RunPath
	}
	cmd := exec.CommandContext(ctx, path, args...)
	// The user-instance seam (ADR-0091): the A2DP sink node (and any
	// PipeWire target the selection names) lives in the service user's
	// *user* PipeWire instance, which the appliance's lingering user
	// session keeps running headless. The system service carries no
	// XDG_RUNTIME_DIR (the uid is sysusers-assigned, not static), so the
	// child resolves the documented fallback itself; an explicitly set
	// variable (env file, tests) always wins.
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		uid := os.Getuid()
		cmd.Env = append(os.Environ(), fmt.Sprintf("XDG_RUNTIME_DIR=/run/user/%d", uid))
	}
	var stdinW io.WriteCloser
	stdinW, err = cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("pipewire: stdin pipe: %w", err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = stdinW.Close()
		return nil, fmt.Errorf("pipewire: sink closed")
	}
	s.children[cmd] = struct{}{}
	s.mu.Unlock()

	if err := cmd.Start(); err != nil {
		s.dropChild(cmd)
		return nil, fmt.Errorf("pipewire: spawn %s: %w", path, err)
	}
	h := &handle{
		sink:    s,
		cmd:     cmd,
		stdin:   stdinW,
		done:    make(chan speech.PlayResult, 1),
		stop:    make(chan time.Duration, 1),
		started: time.Now(), // parent-side spawn; first sample is later (device latency excluded)
	}

	// Stream the WAV in ~100 ms chunks (docs/spec/07: "deliver samples in
	// order with bounded buffer"; the pipe itself is the bounded buffer).
	go func() {
		chunk := 100 * int(pcm.SampleRate) * pcm.Channels * (pcm.BitsPerSample / 8) / 1000
		if chunk < 32 {
			chunk = 32
		}
		for off := 0; off < len(req.WAV); {
			end := off + chunk
			if end > len(req.WAV) {
				end = len(req.WAV)
			}
			if _, err := stdinW.Write(req.WAV[off:end]); err != nil {
				break // EPIPE on stop is expected
			}
			// The first write that lands is the first samples the device
			// buffer accepted: stamp it for the output-latency measurement.
			// Later writes leave the stamp untouched.
			h.firstWrite.CompareAndSwap(0, time.Now().UnixNano())
			off = end
			select {
			case <-ctx.Done():
			case <-time.After(10 * time.Millisecond):
			}
		}
		_ = stdinW.Close()
	}()

	go s.reap(h)
	s.armStop(h)
	return &PlayHandle{h: h}, nil
}

// reap waits for the child and reports the terminal result.
func (s *Sink) reap(h *handle) {
	var res speech.PlayResult
	res.Started = h.started
	if ns := h.firstWrite.Load(); ns != 0 {
		res.FirstSampleAt = time.Unix(0, ns)
	}
	waitErr := h.cmd.Wait()
	res.Ended = time.Now()
	h.once.Do(func() {
		if h.stopped.Load() {
			res.Interrupted = true
		} else if waitErr != nil {
			res.Err = waitErr
		}
		h.done <- res
	})
	s.dropChild(h.cmd)
}

// stopLoop watches the stop channel and applies stop = stop writing + SIGTERM.
func (s *Sink) armStop(h *handle) {
	go func() {
		fade, ok := <-h.stop
		if !ok {
			return
		}
		_ = fade
		h.stopped.Store(true)
		// (1) stop writing: closing stdin ends the input stream.
		_ = h.stdin.Close()
		// (2) SIGTERM so the stream is released now, not after the child
		//     drains its whole internal buffer.
		if err := h.cmd.Process.Signal(os.Interrupt); err != nil {
			return // already exited
		}
		// (3) SIGKILL backstop.
		select {
		case <-h.done:
			return
		case <-time.After(s.KillGrace):
			_ = h.cmd.Process.Kill()
		}
	}()
}

// Close kills any in-flight children.
func (s *Sink) Close() error {
	s.mu.Lock()
	s.closed = true
	kids := make([]*exec.Cmd, 0, len(s.children))
	for c := range s.children {
		kids = append(kids, c)
	}
	s.mu.Unlock()
	for _, c := range kids {
		_ = c.Process.Kill()
	}
	return nil
}

func (s *Sink) dropChild(c *exec.Cmd) {
	s.mu.Lock()
	delete(s.children, c)
	s.mu.Unlock()
}
