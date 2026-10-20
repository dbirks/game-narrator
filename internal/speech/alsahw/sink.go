package alsahw

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/dbirks/game-narrator/internal/speech"
)

// PCM is the device surface the sink writes to. The production
// implementation is the kernel node (kernelPCM); the tests inject a fake
// that records the negotiated params, the write stream (bytes and timing)
// and the op sequence, which is what the hw_params contract, the keep-alive
// cadence and the resample assertions read.
type PCM interface {
	// Setup negotiates the stream with the exact requested hw_params and
	// starts it. A non-nil error aborts the sink setup.
	Setup(p Params) error
	// Status reports the stream state (the SNDRV_PCM_STATE_* values) and
	// the hardware pointer.
	Status() (state int, hwPtr uint64, err error)
	// Restart recovers from an xrun (drop + prepare + start).
	Restart() error
	// WriteFrames transfers S16_LE interleaved frames; it reports the
	// frames written and, on a xrun, EPIPE (or EIO).
	WriteFrames(b []byte) (int, error)
	// ReadyForWrite reports whether the DMA buffer has room now; a timeout
	// answers nil (the caller re-checks its signals and retries).
	ReadyForWrite(timeout time.Duration) error
	// Close releases the device.
	Close() error
}

// pollInterval bounds one pacing wait: the writer sleeps at most this long
// between writes, which bounds the Stop latency (one interval plus one
// period) and gives Close a bounded grace.
const pollInterval = 20 * time.Millisecond

// Sink is one ALSA-hardware output: the named device, the exact hw_params,
// and one writer goroutine that owns the stream for the sink's lifetime —
// the stream is opened once, stays RUNNING between utterances (silence
// keeps the DMA and the receiver's ELD lock alive, the keep-alive), and is
// released by Close. One in-flight playback at a time (the scheduler's
// guarantee; the queue bounds the rest).
type Sink struct {
	// Device is the stable identity (hw:<card name>,<device>).
	Device string
	// Params is the exact hw_params the sink negotiates (per device; the
	// profiles in internal/audio carry the proven sets).
	Params Params
	// KeepAlive is the idle silence cadence: while no utterance is being
	// written, one period of silence goes out every KeepAlive so the
	// hardware pointer keeps advancing and the HDMI receiver stays locked
	// (0 = no keep-alive writes; the stream stays open but frozen — the
	// receiver's lock is not guaranteed to hold).
	KeepAlive time.Duration

	// Open creates the PCM surface for the device node. Tests inject a
	// fake; the default is the kernel node.
	Open func(node string) (PCM, error)

	mu        sync.Mutex
	pcm       PCM
	startOnce sync.Once
	startErr  error
	closed    bool
	// closeCh is the Close() signal the writer watches; writerDone closes
	// when the writer has left.
	closeCh    chan struct{}
	writerDone chan struct{}
	workQ      chan *work
	underruns  atomic.Int64
}

// New returns a sink for the named device. The device node is not opened
// until the first Play: building a sink must not touch hardware the
// process may never use.
func New(device string, p Params, keepAlive time.Duration) (*Sink, error) {
	if _, err := ParseDevice(device); err != nil {
		return nil, err
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	if keepAlive < 0 {
		keepAlive = 0
	}
	return &Sink{
		Device:     device,
		Params:     p,
		KeepAlive:  keepAlive,
		Open:       func(node string) (PCM, error) { return openKernelPCM(node) },
		closeCh:    make(chan struct{}),
		writerDone: make(chan struct{}),
		workQ:      make(chan *work, 16),
	}, nil
}

// Underruns counts the xruns recovered since the sink was opened (the
// docs/spec/13 "playback underruns" surface, reported by diagnostics).
func (s *Sink) Underruns() int64 { return s.underruns.Load() }

type work struct {
	ctx context.Context
	req speech.PlayRequest
	h   *handle
}

// handle is one in-flight playback.
type handle struct {
	done    chan speech.PlayResult
	stop    chan time.Duration // fade request (buffer 1)
	started time.Time
	firstW  atomic.Int64 // unixNano of the first successful frame write
	stopped atomic.Bool
	once    sync.Once
}

// PlayHandle is a speech.PlayHandle for one alsahw playback.
type PlayHandle struct {
	h *handle
}

// Stop implements speech.PlayHandle: fade the delivered tail and end the
// playback; the writer applies it at frame granularity.
func (p *PlayHandle) Stop(fade time.Duration) {
	select {
	case p.h.stop <- fade:
	default:
	}
}

// Done implements speech.PlayHandle.
func (p *PlayHandle) Done() <-chan speech.PlayResult { return p.h.done }

// Play implements speech.AudioSink: it queues the utterance and returns at
// once; the writer goroutine converts (in-process) and does the device I/O
// (the sink's own pacing, the AudioSink contract).
func (s *Sink) Play(ctx context.Context, req speech.PlayRequest) (speech.PlayHandle, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, fmt.Errorf("alsahw: sink closed")
	}
	s.startOnce.Do(func() {
		s.startErr = s.startDevice()
	})
	if s.startErr != nil {
		s.mu.Unlock()
		return nil, s.startErr
	}
	w := &work{ctx: ctx, req: req, h: &handle{
		done:    make(chan speech.PlayResult, 1),
		stop:    make(chan time.Duration, 1),
		started: time.Now(),
	}}
	select {
	case s.workQ <- w:
	case <-ctx.Done():
		s.finish(w.h, ctx.Err(), true)
		return nil, ctx.Err()
	}
	s.mu.Unlock()
	return &PlayHandle{h: w.h}, nil
}

// startDevice opens the node and negotiates the stream. Called once via
// startOnce, under s.mu.
func (s *Sink) startDevice() error {
	dev, _ := ParseDevice(s.Device)
	open := s.Open
	if open == nil {
		open = func(node string) (PCM, error) { return openKernelPCM(node) }
	}
	pcm, err := open(dev.Node())
	if err != nil {
		return err
	}
	if err := pcm.Setup(s.Params); err != nil {
		_ = pcm.Close()
		return err
	}
	s.pcm = pcm
	go s.writer()
	return nil
}

// writer is the single device-owner loop: it drains the work queue, paces
// the writes (poll for room, write a period, recover xruns), writes the
// keep-alive silence between utterances, and ends every handle. It leaves
// only when the sink is closed.
func (s *Sink) writer() {
	defer close(s.writerDone)
	pcm := s.pcm
	periodBytes := int(s.Params.PeriodSize) * s.Params.Channels * 2
	silence := make([]byte, periodBytes) // one period of zeros

	var keepTick <-chan time.Time
	if s.KeepAlive > 0 {
		keepTicker := time.NewTicker(s.KeepAlive)
		defer keepTicker.Stop()
		keepTick = keepTicker.C
	}

	for {
		select {
		case w := <-s.workQ:
			s.playWork(pcm, w, periodBytes)
			continue
		case <-s.closeCh:
			s.closeHandles()
			return
		case <-keepTick:
			// The stream only stays healthy while it is fed: a period of
			// silence per interval keeps the hardware pointer advancing
			// and the receiver's ELD lock alive between utterances.
			if _, err := pcm.WriteFrames(silence); err != nil {
				s.recoverXrun(pcm, err)
			}
		}
	}
}

// playWork delivers one utterance: convert in-process, then write period
// chunks until the utterance is done, a stop arrives, or the device gives
// out.
func (s *Sink) playWork(pcm PCM, w *work, periodBytes int) {
	h := w.h
	params := s.Params
	data, err := ConvertToHW(w.req.WAV, params)
	if err != nil {
		s.finish(h, fmt.Errorf("alsahw: %w", err), false)
		return
	}
	if g := w.req.Gain; g != 1.0 {
		data = ApplyGain(data, g)
	}
	frameBytes := params.Channels * 2

	off := 0
	for off < len(data) {
		// Stop signals, checked at every chunk: the handle's Stop, the
		// request's context, and the sink's close.
		var (
			fade    time.Duration
			stopped bool
		)
		select {
		case fade = <-h.stop:
			stopped = true
		default:
		}
		if !stopped {
			select {
			case <-w.ctx.Done():
				stopped = true
				h.stopped.Store(true)
			default:
			}
		}
		if !stopped {
			select {
			case <-s.closeCh:
				s.finish(h, fmt.Errorf("alsahw: sink closed mid-playback"), true)
				return
			default:
			}
		}
		if stopped {
			s.finishStopped(h, data, off, fade, pcm, frameBytes)
			return
		}

		end := off + periodBytes
		if end > len(data) {
			end = len(data)
		}
		chunk := data[off:end]

		if err := pcm.ReadyForWrite(pollInterval); err != nil {
			s.finish(h, fmt.Errorf("alsahw: poll: %w", err), h.stopped.Load())
			return
		}
		n, werr := pcm.WriteFrames(chunk)
		if n > 0 {
			h.firstW.CompareAndSwap(0, time.Now().UnixNano())
			off += n * frameBytes
		}
		if werr == nil {
			continue
		}
		if errors.Is(werr, syscall.EAGAIN) || errors.Is(werr, syscall.EWOULDBLOCK) {
			continue
		}
		// EPIPE/EIO class: ask the stream what happened.
		state, hwPtr, serr := pcm.Status()
		switch {
		case serr == nil && state == stateXRun:
			if rerr := pcm.Restart(); rerr != nil {
				s.finish(h, fmt.Errorf("alsahw: xrun recovery: %w", rerr), h.stopped.Load())
				return
			}
			s.underruns.Add(1)
			// Retry the chunk the kernel rejected.
			continue
		case serr == nil && (state == stateDisconnected || state == stateSuspended):
			s.finish(h, fmt.Errorf("alsahw: the stream left state %d (hw_ptr %d)", state, hwPtr), h.stopped.Load())
			return
		case serr == nil && (state == stateRunning || state == statePrepared || state == stateDraining):
			// A transient write error with a live stream: back off briefly
			// and retry (bounded by the utterance's own pacing).
			time.Sleep(10 * time.Millisecond)
			continue
		default:
			s.finish(h, fmt.Errorf("alsahw: write: %w (stream state %d, status: %v)", werr, state, serr), h.stopped.Load())
			return
		}
	}
	s.finish(h, nil, false)
}

// finishStopped applies the stop: a 0 (or negative) fade is a hard cut; a
// positive fade delivers the tail scaled 1→0 over the fade (capped at the
// fade's frame count — the rest of the utterance is discarded, not played).
func (s *Sink) finishStopped(h *handle, data []byte, off int, fade time.Duration, pcm PCM, frameBytes int) {
	h.stopped.Store(true)
	if fade > 0 && off < len(data) {
		fadeFrames := int(fade.Seconds() * float64(s.Params.Rate))
		if fadeFrames > 1 {
			tail := data[off:]
			if len(tail) > fadeFrames*frameBytes {
				tail = tail[:fadeFrames*frameBytes]
			}
			tail = fadeTail(tail)
			// The faded tail is short (bounded by the fade); write it in
			// one go, best effort — the stop has already been decided.
			for len(tail) > 0 {
				if err := pcm.ReadyForWrite(pollInterval); err != nil {
					break
				}
				n, err := pcm.WriteFrames(tail)
				if err != nil || n == 0 {
					break
				}
				tail = tail[n*frameBytes:]
			}
		}
	}
	s.finish(h, nil, true)
}

// fadeTail scales the tail 1→0 linearly over its whole length (the tail was
// already capped at the fade's frame count by finishStopped).
func fadeTail(b []byte) []byte {
	if len(b) < 4 {
		return b
	}
	frames := len(b) / 2 // 16-bit samples; the mono/stereo split does not
	// change a per-sample scale.
	out := make([]byte, len(b))
	for i := 0; i+1 < len(b); i += 2 {
		sample := i / 2
		g := 1.0
		if frames > 1 {
			g = float64(frames-1-sample) / float64(frames-1)
		}
		if g < 0 {
			g = 0
		}
		s := int16(uint16(b[i]) | uint16(b[i+1])<<8)
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

// finish publishes the terminal result exactly once (the buffered done
// channel carries it; the close is the terminal signal's order guarantee).
func (s *Sink) finish(h *handle, err error, interrupted bool) {
	h.once.Do(func() {
		res := speech.PlayResult{}
		res.Started = h.started
		if ns := h.firstW.Load(); ns != 0 {
			res.FirstSampleAt = time.Unix(0, ns)
		}
		res.Ended = time.Now()
		res.Interrupted = interrupted
		res.Err = err
		h.done <- res
		close(h.done)
	})
}

// closeHandles finalizes every handle the writer has not ended yet (the
// sink closed mid-utterance).
func (s *Sink) closeHandles() {
	for {
		select {
		case w := <-s.workQ:
			s.finish(w.h, fmt.Errorf("alsahw: sink closed mid-playback"), true)
		default:
			return
		}
	}
}

// recoverXrun recovers the idle (keep-alive) stream from a write error: a
// xrun is dropped/re-prepared/started and counted; anything else is left to
// the next utterance's writer loop, which sees it on its first write.
func (s *Sink) recoverXrun(pcm PCM, err error) {
	if !errors.Is(err, syscall.EPIPE) && !errors.Is(err, syscall.EIO) {
		return
	}
	state, _, serr := pcm.Status()
	if serr != nil || state != stateXRun {
		return
	}
	if rerr := pcm.Restart(); rerr == nil {
		s.underruns.Add(1)
	}
}

// Close releases the device and ends every in-flight handle. It is
// idempotent and bounded: it signals the writer, which leaves within one
// poll interval of the signal (or at its current chunk boundary), and the
// PCM close unblocks any write the writer is inside.
func (s *Sink) Close() error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.closeCh)
	}
	s.mu.Unlock()
	select {
	case <-s.writerDone:
	case <-time.After(2 * time.Second):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pcm != nil {
		err := s.pcm.Close()
		s.pcm = nil
		return err
	}
	return nil
}
