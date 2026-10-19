// Package ffmpegpipe runs FFmpeg as a supervised subprocess that writes raw
// rgb24 frames to a pipe (ADR-0012 part 1: FFmpeg is a child process with an
// argv interface, never cgo, never a shell string).
//
// One implementation serves every transport, which is what makes
// docs/spec/11 "E2E verification steps" item 4 true — "Feed Dolphin video to
// same FrameSource consumer as production capture":
//
//	V4L2(device, …)     UNO Q capture card   -f v4l2    -input_format … -i /dev/videoN
//	X11(display, …)     dev-host Dolphin     -f x11grab                -i :1.0+X,Y
//	TestSource(w,h,fps) FFmpeg test pattern  -f lavfi   -i testsrc2=size=…:rate=…
//
// They differ only in input arguments: the reader, the timestamps, the
// single-slot delivery, the fps meter and the supervisor are the same code, so
// an X11 test is real evidence about the V4L2 path rather than a rehearsal of it.
//
// Supervision, per docs/spec/05 ("detect fps/dropped frames, device
// unplug/reconnect") and docs/spec/13's "Capture removed" row:
//
//   - the child lives in its own process group, so cancellation and the stall
//     watchdog kill FFmpeg *and anything it spawned* (ADR-0012 §2);
//   - if the child exits, or stalls — no frame for StallTimeout, the frozen
//     splitter case where FFmpeg happily keeps a pipe that will never deliver
//     again — state goes to reconnecting and the child is respawned with
//     exponential backoff capped at 10 s;
//   - Restarts counts respawns, so "the card was nudged once" and "capture is
//     flapping" are different numbers in the inspector;
//   - a missing FFmpeg binary is StateFailed rather than StateReconnecting,
//     because backoff cannot fix it — and the loop keeps retrying at the cap
//     anyway, so installing the package on a live appliance recovers capture
//     without restarting the service.
package ffmpegpipe

import (
	"context"
	"errors"
	"fmt"
	"image"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// Defaults, as constants so doctor and tests can name them.
const (
	// DefaultStallTimeout is how long a live source may deliver nothing before
	// its FFmpeg is considered wedged. Three frame periods at 10 fps would be
	// tighter, but a converter renegotiating EDID goes quiet for a second or two
	// routinely (docs/spec/13 "Bad analog converter mode"), and a respawn is far
	// cheaper than a lost dialogue line.
	DefaultStallTimeout = 5 * time.Second
	// DefaultRestartDelay is the first backoff step; DefaultMaxRestartDelay is
	// its ceiling — the cap docs/spec/13's "reconnect with backoff" asks for.
	DefaultRestartDelay    = 200 * time.Millisecond
	DefaultMaxRestartDelay = 10 * time.Second
	// DefaultBin is the binary resolved on PATH at every attempt (ADR-0012 §3:
	// probed at runtime, never assumed).
	DefaultBin = "ffmpeg"
	// StderrTailBytes bounds how much FFmpeg complaint is kept verbatim, the
	// same bounded-pipe rule ADR-0012 §2 states for OCR.
	StderrTailBytes = 2048
)

// ErrAlreadyStarted is returned by a second Start on the same Source.
var ErrAlreadyStarted = errors.New("ffmpegpipe: source already started")

// Input describes how to ask FFmpeg for frames. A struct rather than a free-form
// argv slice, because the point of the type is that a caller cannot inject an
// argument the source does not understand or a shell metacharacter: the output
// side is fixed to `-f rawvideo -pix_fmt rgb24 -`, which is what lets the reader
// compute an exact frame size, and it is not configurable.
type Input struct {
	// Format is FFmpeg's demuxer name: "v4l2", "x11grab", "lavfi".
	Format string
	// Location is the -i argument: a device path, ":1.0+0,0", or a filtergraph.
	Location string
	// V4L2Format is the -input_format value (mjpeg | yuyv422). Empty means let
	// FFmpeg choose, which a preflight-checked pipeline should never do
	// (docs/spec/10 wants the negotiated format recorded) but a hand-wired
	// source may.
	V4L2Format string
	// Paced adds -re. Needed for lavfi, which otherwise produces frames as fast
	// as the CPU can and would measure the pipe instead of the supervision;
	// wrong for real devices, whose real rate is the thing under test.
	Paced bool
}

// Source is a capture.Source backed by a piped FFmpeg child.
//
// The zero value is not useful (Input and Width/Height are required); use New,
// V4L2, X11 or TestSource and then adjust the tuning fields.
type Source struct {
	// IDName overrides the reported identity.
	IDName string
	// Bin is the FFmpeg binary; empty means DefaultBin on PATH.
	Bin string
	// Input is the transport.
	Input Input
	// Width, Height are the requested geometry; FPS the requested rate (0 =
	// whatever the device gives). Stats.FPS is always the *measured* rate.
	Width, Height int
	FPS           float64
	// DrawMouse applies only to x11grab and defaults to false: a hardware
	// cursor blinking inside the ROI is exactly the animated-icon noise
	// docs/spec/05 tells the pipeline to mask, and on the dev host that cursor
	// sits over Dolphin's window.
	DrawMouse bool
	// StallTimeout, RestartDelay and MaxRestartDelay tune supervision.
	StallTimeout    time.Duration
	RestartDelay    time.Duration
	MaxRestartDelay time.Duration
	// LosslessQueue switches delivery from the single-slot latest-frame queue
	// to a bounded FIFO that blocks FFmpeg rather than dropping frames —
	// docs/spec/01's "unlike lossless recording". The capacity is the memory
	// ceiling; 0 keeps the live slot.
	LosslessQueue int

	mu      sync.Mutex
	state   capture.State
	stats   capture.Stats
	cmdPID  int
	closed  bool
	started bool
	cancel  context.CancelFunc
	done    chan struct{}
	out     chan capture.Frame
	slot    *capture.LatestSlot
	loss    *capture.Lossless
	window  fpsWindow
	stderr  tailBuf
	beat    atomic.Int64 // unix nanos of the last frame read, for the watchdog
	seq     atomic.Int64
}

// New builds a source from an Input. Callers normally use V4L2, X11 or
// TestSource.
func New(in Input, w, h int, fps float64) *Source {
	if in.Format == "" {
		in.Format = "lavfi"
	}
	return &Source{
		Input:           in,
		Width:           w,
		Height:          h,
		FPS:             fps,
		StallTimeout:    DefaultStallTimeout,
		RestartDelay:    DefaultRestartDelay,
		MaxRestartDelay: DefaultMaxRestartDelay,
	}
}

// V4L2 builds the UNO Q capture-card source. device is a path discovered by
// devices.List, never a hardcoded /dev/video0 (docs/spec/05, docs/spec/10).
// inputFormat is "mjpeg" or "yuyv422" (docs/spec/05: "prefer UVC MJPEG or YUYV
// depending on hardware"); anything else is refused here rather than handed to
// FFmpeg, because a typo FFmpeg silently falls back from would report the format
// the pipeline is not getting.
func V4L2(device string, w, h int, fps float64, inputFormat string) (*Source, error) {
	switch inputFormat {
	case "", "mjpeg", "yuyv422":
	default:
		return nil, fmt.Errorf("ffmpegpipe: v4l2 input format %q: want mjpeg or yuyv422", inputFormat)
	}
	if device == "" {
		return nil, errors.New("ffmpegpipe: v4l2 device path is required (docs/spec/10: never assume /dev/video0)")
	}
	s := New(Input{Format: "v4l2", Location: device, V4L2Format: inputFormat}, w, h, fps)
	s.IDName = "v4l2:" + device
	return s, nil
}

// X11 builds the dev-host grab of Dolphin's window. display is an X display
// such as ":1" or ":1.0"; offX/offY crop to the render window's geometry as
// resolved through xwininfo, because grabbing the whole root window would feed
// the pipeline other windows' pixels (ADR-0007: "never the whole root window").
func X11(display string, w, h int, fps float64, offX, offY int) (*Source, error) {
	if display == "" {
		return nil, errors.New("ffmpegpipe: x11grab display is required")
	}
	loc := fmt.Sprintf("%s+%d,%d", display, offX, offY)
	s := New(Input{Format: "x11grab", Location: loc}, w, h, fps)
	s.IDName = "x11:" + loc
	return s, nil
}

// TestSource builds an animated FFmpeg test pattern. The capture tests run
// against it and doctor uses it to prove FFmpeg works with no camera attached,
// so it is paced with -re: an unthrottled lavfi source emits thousands of frames
// a second and would measure neither a real rate nor a real stall.
func TestSource(w, h int, fps float64) *Source {
	if fps <= 0 {
		fps = 15
	}
	s := New(Input{
		Format:   "lavfi",
		Location: fmt.Sprintf("testsrc2=size=%dx%d:rate=%s", w, h, rateString(fps)),
		Paced:    true,
	}, w, h, fps)
	s.IDName = fmt.Sprintf("lavfi:testsrc2-%dx%d@%s", w, h, rateString(fps))
	return s
}

// Args returns the exact FFmpeg argv (without argv[0]) for the current
// configuration. Exported because the inspector and the evidence bundle record
// the command that produced a frame — the difference between "we captured
// something" and "we captured this, at this format" (docs/spec/12 "fixture
// provenance") — and because asserting "no shell in argv" is then one line.
func (s *Source) Args() []string {
	in := s.Input
	args := []string{"-hide_banner", "-loglevel", "error", "-nostats", "-nostdin"}
	if in.Paced {
		args = append(args, "-re")
	}
	args = append(args, "-f", in.Format)
	if in.Format == "v4l2" && in.V4L2Format != "" {
		args = append(args, "-input_format", in.V4L2Format)
	}
	if in.Format == "x11grab" {
		args = append(args, "-draw_mouse", b01(s.DrawMouse))
	}
	// -framerate is an *input* option only some demuxers accept. lavfi rejects it
	// outright ("Option framerate not found", exit 8 — measured, not theorised)
	// because its rate lives inside the filtergraph, so it is only passed to the
	// device demuxers that negotiate with it.
	if s.FPS > 0 && in.Format != "lavfi" {
		args = append(args, "-framerate", rateString(s.FPS))
	}
	if s.Width > 0 && s.Height > 0 && in.Format != "lavfi" {
		// lavfi carries its size in the filtergraph; -video_size there is
		// redundant for testsrc2 and an error for some inputs.
		args = append(args, "-video_size", strconv.Itoa(s.Width)+"x"+strconv.Itoa(s.Height))
	}
	args = append(args, "-i", in.Location)
	// Fixed output side: rgb24 to stdout, no container, so one frame is exactly
	// Width*Height*3 bytes and the reader parses nothing but pixels.
	return append(args, "-map", "0:v:0", "-an", "-sn", "-f", "rawvideo", "-pix_fmt", "rgb24", "-")
}

// ID implements capture.Source.
func (s *Source) ID() string {
	if s.IDName != "" {
		return s.IDName
	}
	return "ffmpeg:" + s.Input.Format + ":" + s.Input.Location
}

// PID returns the current FFmpeg child's pid, or 0 while none is running.
// Exported for the supervision test (which kills it to simulate a disappearing
// device) and for doctor; it is a snapshot, not a lease.
func (s *Source) PID() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cmdPID
}

// Stats implements capture.Source.
func (s *Source) Stats() capture.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	if s.slot != nil {
		st.DroppedSlot = s.slot.Drops()
	}
	if s.state == "" {
		st.State = capture.StateStarting
	} else {
		st.State = s.state
		// A source that has stopped reconnecting and is simply closed must not
		// keep reporting the stale error that preceded the shutdown.
		if s.closed {
			st.State = capture.StateStopped
		}
	}
	return st
}

// Start implements capture.Source.
func (s *Source) Start(ctx context.Context) (<-chan capture.Frame, error) {
	if s.Width <= 0 || s.Height <= 0 {
		return nil, fmt.Errorf("ffmpegpipe: %s needs a positive size, got %dx%d", s.ID(), s.Width, s.Height)
	}
	if s.Input.Location == "" {
		return nil, fmt.Errorf("ffmpegpipe: %s has no input location", s.ID())
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil, ErrAlreadyStarted
	}
	s.started = true
	s.state = capture.StateStarting
	s.out = make(chan capture.Frame)
	s.done = make(doneChan)
	if s.LosslessQueue > 0 {
		s.loss = capture.NewLossless(s.LosslessQueue)
	} else {
		s.slot = capture.NewLatestSlot()
	}
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.beat.Store(time.Now().UnixNano())
	s.mu.Unlock()

	go s.supervise(runCtx)
	go s.pump(runCtx)
	return s.out, nil
}

// Close implements capture.Source. It cancels the child's context — which makes
// exec kill the process group — and waits for the supervisor to leave, so a
// returning Close means no FFmpeg of ours is alive.
func (s *Source) Close() error {
	s.mu.Lock()
	s.closed = true
	s.state = capture.StateStopped
	cancel := s.cancel
	done := s.done
	started := s.started
	if s.slot != nil {
		s.slot.Close()
	}
	if s.loss != nil {
		s.loss.Close()
	}
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		<-done
	}
	return nil
}

// --- supervision ----------------------------------------------------------

// supervise is the restart loop. It owns the FFmpeg child; pump owns delivery.
func (s *Source) supervise(ctx context.Context) {
	defer close(s.done)
	defer s.stopQueues()

	base, delay := s.RestartDelay, s.RestartDelay
	if base <= 0 {
		base, delay = DefaultRestartDelay, DefaultRestartDelay
	}
	maxDelay, stall := s.MaxRestartDelay, s.StallTimeout
	if maxDelay <= 0 {
		maxDelay = DefaultMaxRestartDelay
	}
	if stall <= 0 {
		stall = DefaultStallTimeout
	}

	for ctx.Err() == nil {
		frames, err := s.runOnce(ctx, stall)
		if ctx.Err() != nil {
			// Cancelled by Close: not a fault, and not a restart.
			return
		}
		if isPermanent(err) {
			// Failed rather than reconnecting, because backoff cannot fix a
			// missing package and the operator action is different
			// (docs/spec/13 wants an action per fault) — but the loop keeps
			// retrying at the cap, so `apt install ffmpeg` on a running
			// appliance recovers capture without restarting the service.
			s.setState(capture.StateFailed, err)
		} else {
			s.countRestart(err)
			s.setState(capture.StateReconnecting, err)
		}
		// A child that produced frames was working, so the next failure is again
		// "recent" and the backoff restarts; one that died instantly means the
		// condition is standing outside, and the delay must keep growing.
		if frames > 0 {
			delay = base
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		delay = nextDelay(delay, base, maxDelay)
	}
}

// nextDelay is the exponential step, with the ceiling docs/spec/13's
// "reconnect with backoff" implies: growth without a cap turns a card that is
// merely unplugged into a respawn every few seconds for the life of the process,
// and no cap at all turns a permanent fault into an unreachable log.
func nextDelay(delay, base, max time.Duration) time.Duration {
	if base <= 0 {
		base = DefaultRestartDelay
	}
	if max <= 0 {
		max = DefaultMaxRestartDelay
	}
	if delay <= 0 {
		return base
	}
	if delay *= 2; delay > max {
		return max
	}
	return delay
}

// isPermanent classifies the errors backoff cannot fix, so the state a human or
// the UI reads distinguishes "waiting for hardware" from "this host is missing
// a package" (docs/spec/13 wants an operator action per fault).
func isPermanent(err error) bool {
	if err == nil {
		return false
	}
	var ee *exec.Error
	if errors.As(err, &ee) {
		return true
	}
	return errors.Is(err, exec.ErrNotFound)
}

// stopQueues wakes the delivery goroutine on the way out of supervise.
func (s *Source) stopQueues() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slot != nil {
		s.slot.Close()
	}
	if s.loss != nil {
		s.loss.Close()
	}
}

// runOnce spawns one FFmpeg, streams its frames into the delivery queue and
// returns the count read plus the reason it ended. It always leaves the child
// reaped — that is what keeps this from being the zombie leak ADR-0012 lists as
// a cost of the subprocess design.
func (s *Source) runOnce(ctx context.Context, stall time.Duration) (int64, error) {
	bin := s.Bin
	if bin == "" {
		bin = DefaultBin
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return 0, &exec.Error{Name: bin, Err: err}
	}

	// CommandContext, not Command: exec refuses a non-nil Cancel on a command
	// with no context, and Supervise's group-kill Cancel is the whole point of
	// the supervision (ADR-0012 §2).
	cmd := exec.CommandContext(ctx, path, s.Args()...)
	capture.Supervise(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return 0, fmt.Errorf("ffmpegpipe: stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return 0, fmt.Errorf("ffmpegpipe: stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("ffmpegpipe: start %s: %w", bin, err)
	}

	s.mu.Lock()
	s.cmdPID = cmd.Process.Pid
	pid := cmd.Process.Pid
	s.mu.Unlock()
	s.beat.Store(time.Now().UnixNano())

	// FFmpeg's progress text is not our concern, but its file descriptor is: an
	// unread stderr pipe blocks the child (ADR-0012's "pipe deadlock"), so it is
	// drained into a bounded tail. The tail is per-attempt: keeping the previous
	// child's message would blame a reconnect on the wrong failure.
	s.stderr.reset()
	tailDone := make(chan struct{})
	go func() {
		defer close(tailDone)
		_, _ = io.Copy(&s.stderr, stderr)
	}()

	stopWatch := s.watchStall(ctx, stall, pid)
	frames, readErr := s.readFrames(ctx, stdout)
	stalled := stopWatch()
	// Killing the group is how Wait returns promptly even if FFmpeg itself is
	// wedged in a driver call; it is idempotent when the child already exited.
	_ = capture.KillGroup(pid)
	waitErr := cmd.Wait()

	s.mu.Lock()
	s.cmdPID = 0
	tail := s.stderr.String()
	s.mu.Unlock()
	<-tailDone

	switch {
	case ctx.Err() != nil:
		return frames, ctx.Err()
	case stalled != nil:
		// The watchdog killed this child on purpose. Reporting the pipe error it
		// caused would blame the transport for a source that went quiet, and
		// docs/spec/13's table is only useful if the reason names the fault.
		return frames, stalled
	case readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF):
		return frames, readErr
	case waitErr != nil:
		return frames, fmt.Errorf("ffmpegpipe: %s exited: %w%s", s.ID(), waitErr, quoteTail(tail))
	default:
		// Clean EOF, no error: the input reached its end (a finite lavfi graph,
		// a device that closed). The supervision loop treats it as a fault to
		// retry, which is correct for a live source and irrelevant for a test
		// pattern that never ends.
		return frames, io.ErrClosedPipe
	}
}

// watchStall starts the watchdog and returns its stop function.
//
// The watchdog exists for the case a read error cannot catch: FFmpeg alive, the
// pipe open, and no pixels — a frozen splitter output or a UVC card that has
// stopped queueing buffers. Without it the capture loop waits forever on a
// Read that will return nothing, which is docs/spec/13's "video loss/freeze"
// fault presenting as silence rather than a state change.
func (s *Source) watchStall(ctx context.Context, stall time.Duration, pid int) func() error {
	wdCtx, cancel := context.WithCancel(ctx)
	var stalled atomic.Value // stores error
	go func() {
		t := time.NewTicker(stall / 4)
		defer t.Stop()
		for {
			select {
			case <-wdCtx.Done():
				return
			case <-t.C:
				last := s.beat.Load()
				if last == 0 {
					continue
				}
				if time.Since(time.Unix(0, last)) >= stall {
					reason := fmt.Errorf("ffmpegpipe: %s produced no frame within %s; killed pid %d", s.ID(), stall, pid)
					stalled.Store(reason)
					_ = capture.KillGroup(pid)
					return
				}
			}
		}
	}()
	return func() error {
		cancel()
		if v := stalled.Load(); v != nil {
			err := v.(error)
			s.noteStall(err)
			return err
		}
		return nil
	}
}

// --- frame reading and delivery -------------------------------------------

// readFrames consumes Width*Height*3-byte chunks and puts each on the delivery
// queue. Rawvideo has no framing, so the chunk size *is* the framing, and a read
// that ends mid-frame is a truncated frame worth counting rather than one to
// stitch into the next (which would corrupt a picture and hide the fact).
func (s *Source) readFrames(ctx context.Context, r io.Reader) (int64, error) {
	frameBytes := s.Width * s.Height * 3
	buf := make([]byte, frameBytes)
	var n int64
	for {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		off := 0
		for off < frameBytes {
			read, err := r.Read(buf[off:])
			off += read
			if err != nil {
				if off == 0 && errors.Is(err, io.EOF) {
					return n, io.EOF
				}
				if off > 0 && off < frameBytes {
					s.countDecode(fmt.Errorf("ffmpegpipe: truncated frame: %d/%d bytes: %w", off, frameBytes, err))
				}
				return n, err
			}
		}
		img, err := rgb24(buf, s.Width, s.Height)
		if err != nil {
			s.countDecode(err)
			continue
		}
		f := capture.Frame{
			Seq: s.seq.Add(1),
			// The rawvideo pipe carries no presentation timestamps, so the honest
			// clock is "monotonic, at read time". Claiming "device" would put a
			// stamp on the frame that the device never supplied (docs/spec/01
			// invariant 1 is about honesty, not about the word).
			SourceClock:        capture.ClockMonotonic,
			CaptureTimestampNS: capture.MonotonicNS(),
			Width:              s.Width,
			Height:             s.Height,
			PixFmt:             capture.PixRGB24,
			Image:              img,
			Hash:               capture.HashFrame(img),
		}
		if err := f.Validate(); err != nil {
			s.countDecode(err)
			continue
		}
		s.beat.Store(time.Now().UnixNano())
		if !s.deliver(ctx, f) {
			return n, context.Canceled
		}
		n++
	}
}

// deliver hands a frame to the configured queue. False means shutdown.
func (s *Source) deliver(ctx context.Context, f capture.Frame) bool {
	s.mu.Lock()
	slot, loss := s.slot, s.loss
	s.mu.Unlock()
	switch {
	case loss != nil:
		// Lossless recording: block, and let FFmpeg's own pipe fill if the
		// consumer wedges. Producer-side slowdown is the signal (docs/spec/01).
		return loss.Put(ctx, f)
	case slot != nil:
		return slot.Put(f)
	default:
		return false
	}
}

// pump moves frames from the queue to the channel Start returned. It is separate
// from the reader so the reader never blocks on a slow consumer — it overwrites
// the slot and counts it — and so Frame.Dropped can be stamped with what the
// *consumer's* pace cost, which is the number docs/spec/10 asks preflight to
// report.
func (s *Source) pump(ctx context.Context) {
	defer close(s.out)
	var lastDrops uint64
	for {
		s.mu.Lock()
		slot, loss := s.slot, s.loss
		s.mu.Unlock()

		var (
			f  capture.Frame
			ok bool
		)
		switch {
		case loss != nil:
			f, ok = loss.Take(ctx)
		case slot != nil:
			f, ok = slot.Take(ctx)
			if d := slot.Drops(); d >= lastDrops {
				f.Dropped = int64(d - lastDrops)
				lastDrops = d
			}
		default:
			return
		}
		if !ok {
			return
		}
		// Count the hand-over *before* the send, not after: the unbuffered
		// send completes at the instant the consumer receives the frame, so an
		// increment parked behind it lets a concurrent Stats() read report
		// fewer deliveries than the consumer already has in hand — the stats
		// would undercount what the pipeline was fed (docs/spec/10 preflight
		// reads Delivered exactly to decide that).
		s.mu.Lock()
		s.stats.Delivered++
		s.mu.Unlock()
		select {
		case s.out <- f:
		case <-ctx.Done():
			// The consumer never got the frame, so uncount it: Delivered must
			// never claim a hand-over that did not happen.
			s.mu.Lock()
			s.stats.Delivered--
			s.mu.Unlock()
			return
		}
		s.recordDelivered()
	}
}

// recordDelivered updates the freshness, fps estimate and running state after
// a frame has actually reached the consumer. Delivered itself is counted by
// the pump around the send (see pump), because only that ordering keeps the
// published number consistent with what the consumer has observed.
func (s *Source) recordDelivered() {
	now := time.Now()
	s.mu.Lock()
	s.stats.LastFrameAt = now
	s.window.add(now)
	s.stats.FPS = s.window.fps()
	started := s.state == capture.StateStarting || s.state == capture.StateReconnecting
	if started {
		s.state = capture.StateRunning
		s.stats.LastError = ""
	}
	s.mu.Unlock()
}

// --- helpers --------------------------------------------------------------

// fpsWindow is a fixed ring of arrival times: an FPS that responds to a change
// within a few frames instead of only at the end of a one-second bucket, with
// bounded memory on a 4 GB appliance (docs/spec/13 "memory ceilings").
//
// The first fpsWarmup arrivals are discarded. FFmpeg's -re pacing (and any
// device that queues a few buffers before streaming) emits a small burst the
// moment it starts, and an average taken across it reports a rate the source
// does not have — measured here as 18.5 fps reported for a 15 fps lavfi source
// when the burst was included. A rate read after warm-up is the number
// docs/spec/10's preflight is asking about.
const fpsWarmup = 3

type fpsWindow struct {
	stamp [16]time.Time
	n     int
	next  int
	seen  int
}

func (w *fpsWindow) add(t time.Time) {
	w.seen++
	if w.seen <= fpsWarmup {
		return
	}
	w.stamp[w.next] = t
	w.next = (w.next + 1) % len(w.stamp)
	if w.n < len(w.stamp) {
		w.n++
	}
}

func (w *fpsWindow) fps() float64 {
	if w.n < 2 {
		return 0
	}
	oldest := 0
	if w.n == len(w.stamp) {
		oldest = w.next // the ring is full, so the write cursor sits on the stalest
	}
	newest := (w.next - 1 + len(w.stamp)) % len(w.stamp)
	span := w.stamp[newest].Sub(w.stamp[oldest])
	if span <= 0 {
		return 0
	}
	return float64(w.n-1) / span.Seconds()
}

// tailBuf is a bounded, mutex-guarded stderr tail.
type tailBuf struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - StderrTailBytes; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuf) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

func (t *tailBuf) reset() {
	t.mu.Lock()
	t.buf = t.buf[:0]
	t.mu.Unlock()
}

func quoteTail(tail string) string {
	if tail == "" {
		return ""
	}
	return " (ffmpeg: " + tail + ")"
}

func (s *Source) countDecode(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.stats.DecodeErrors++
	s.mu.Unlock()
}

func (s *Source) countRestart(err error) {
	s.mu.Lock()
	s.stats.Restarts++
	if err != nil {
		s.stats.LastError = err.Error()
	}
	s.mu.Unlock()
}

func (s *Source) noteStall(err error) {
	s.mu.Lock()
	s.stats.LastError = err.Error()
	s.mu.Unlock()
}

func (s *Source) setState(st capture.State, err error) {
	s.mu.Lock()
	s.state = st
	if err != nil {
		s.stats.LastError = err.Error()
	}
	s.mu.Unlock()
}

// rgb24 expands FFmpeg's packed 3-byte pixels into image.RGBA. It is a copy
// (4 bytes out per 3 in): the alternative, a custom image type, would put a
// special case in every consumer of Frame.Image to save microseconds.
func rgb24(p []byte, w, h int) (*image.RGBA, error) {
	if len(p) < w*h*3 {
		return nil, fmt.Errorf("ffmpegpipe: rgb24 buffer %d bytes, want %d", len(p), w*h*3)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	di, si := 0, 0
	for i := 0; i < w*h; i++ {
		dst.Pix[di+0] = p[si+0]
		dst.Pix[di+1] = p[si+1]
		dst.Pix[di+2] = p[si+2]
		dst.Pix[di+3] = 0xff
		di += 4
		si += 3
	}
	return dst, nil
}

func rateString(fps float64) string {
	if fps == float64(int64(fps)) {
		return strconv.FormatInt(int64(fps), 10)
	}
	return strconv.FormatFloat(fps, 'g', -1, 64)
}

func b01(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// doneChan names the close-on-completion channel so Close's wait reads clearly.
type doneChan = chan struct{}

var _ capture.Source = (*Source)(nil)
