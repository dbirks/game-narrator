// Package replay is the FrameSource that runs on recorded frames — the source
// behind docs/spec/12's "Frame replay" gate, the one that runs on every PR
// because it needs no emulator, no capture card and no model endpoint.
//
// Two properties make a replay a substitute for live video rather than a
// slideshow:
//
//   - Timestamps are the recording's own. Frames carry the capture_timestamp_ns
//     stored when they were captured, on SourceClock "replay", so the dialogue
//     tracker's stability windows and a typewriter's reveal rate reproduce the
//     original timings (docs/spec/01 invariant 1). A replay that re-stamped
//     frames with the wall clock would pass while testing nothing about
//     timing-sensitive behaviour.
//   - Pacing is exact and optional. Speed 1.0 reproduces real time from the
//     recorded deltas; Speed 0 drains as fast as the consumer takes, which is
//     what makes a 4 000-frame fixture cost milliseconds in CI — and is also the
//     "frames skipped at varied fps" failure-injection knob (docs/spec/12
//     "Failure injection").
//
// Delivery is lossless and back-pressuring, deliberately unlike the live
// single-slot path: a fixture replay that dropped a frame would be testing a
// different recording than the one that was captured.
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // recorded frames may be JPEG
	_ "image/png"  // recorded frames are usually PNG
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// ErrNoFrames is returned when the manifest lists nothing.
var ErrNoFrames = errors.New("replay: no frames in manifest")

// ErrAlreadyStarted is returned by a second Start on the same Source.
var ErrAlreadyStarted = errors.New("replay: source already started")

// Entry is one row of timestamps.json: the recorded capture time of one frame.
type Entry struct {
	// File is the frame's name, relative to the source directory.
	File string `json:"file"`
	// TsNS is the frame's capture timestamp in nanoseconds on the replay clock.
	TsNS int64 `json:"ts_ns"`
}

// Source replays a directory of frames paced by a timestamps.json manifest.
//
// The zero value is not useful — Dir is required. Speed 0 is meaningful and
// means "as fast as the consumer takes", which is why there is no default of 1.0
// hidden in a nil check: a test that forgot to set Speed would otherwise silently
// become a real-time test.
type Source struct {
	// Dir holds the frames named by the manifest.
	Dir string
	// TimestampsFile defaults to <Dir>/timestamps.json.
	TimestampsFile string
	// Speed scales the recorded deltas: 1.0 = real time, 4.0 = four times as
	// fast, 0 (or negative) = no pacing.
	Speed float64
	// Loop restarts the sequence when it ends, standing in for "the game is
	// idling on this screen" in a long-running test. Timestamps keep advancing
	// across passes (by the recording's length plus one frame step) so a
	// consumer can still measure elapsed time against them, and Seq never
	// restarts.
	Loop bool
	// Sleeper paces emission. nil paces on the real clock; tests inject a
	// recorder and assert the schedule instead of waiting for it — the same
	// reason ADR-0007 forbids wall-clock sleeps in emulator timing, applied to
	// this package's own tests.
	Sleeper func(ctx context.Context, d time.Duration) error

	mu      sync.Mutex
	state   capture.State
	stats   capture.Stats
	started bool
	closed  bool
	cancel  context.CancelFunc
	out     chan capture.Frame
	done    chan struct{}

	// Loaded manifest.
	manifest []Entry
	loaded   bool
	loadErr  error
	seq      int64
}

// Manifest parses (once) and returns timestamps.json. The fixture-provenance
// test uses it to assert the manifest and the directory agree.
func (s *Source) Manifest() ([]Entry, error) {
	s.mu.Lock()
	if s.loaded {
		m, err := s.manifest, s.loadErr
		s.mu.Unlock()
		return m, err
	}
	s.mu.Unlock()

	path := s.TimestampsFile
	if path == "" {
		path = filepath.Join(s.Dir, "timestamps.json")
	}
	var entries []Entry
	raw, err := os.ReadFile(path)
	switch {
	case err != nil:
		err = fmt.Errorf("replay: read manifest %s: %w", path, err)
	default:
		if err = json.Unmarshal(raw, &entries); err != nil {
			err = fmt.Errorf("replay: parse manifest %s: %w", path, err)
		} else if len(entries) == 0 {
			err = ErrNoFrames
		} else {
			for i, e := range entries {
				// A manifest is data, and data that names "../etc/passwd" is a
				// traversal attempt rather than a path (AGENTS.md rule 6:
				// fixtures are untrusted).
				if e.File == "" || !filepath.IsLocal(filepath.Clean(e.File)) {
					err = fmt.Errorf("replay: manifest %s entry %d has unsafe file %q", path, i, e.File)
					break
				}
				// Non-monotonic timestamps are allowed (a recording spliced from
				// two passes is a real thing), but a non-positive one is refused
				// downstream by recognition, so it is refused here where the
				// offending path can be named (docs/spec/01 invariant 1).
				if e.TsNS <= 0 {
					err = fmt.Errorf("replay: manifest %s entry %d has ts_ns %d, want > 0", path, i, e.TsNS)
					break
				}
			}
		}
	}

	s.mu.Lock()
	s.loaded, s.manifest, s.loadErr = true, entries, err
	if err != nil {
		s.state = capture.StateFailed
		s.stats.LastError = err.Error()
	}
	s.mu.Unlock()
	return entries, err
}

// ID implements capture.Source.
func (s *Source) ID() string {
	if s.Dir == "" {
		return "replay:(no directory)"
	}
	return "replay:" + filepath.Base(filepath.Clean(s.Dir))
}

// Stats implements capture.Source.
func (s *Source) Stats() capture.Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.stats
	if s.state == "" {
		st.State = capture.StateStarting
	} else {
		st.State = s.state
	}
	return st
}

// Start implements capture.Source. The channel closes at end-of-stream, which is
// how a consumer tells "the recording ended" from "the source died": a failure
// sets StateFailed and closes the channel too, but only after recording why.
func (s *Source) Start(ctx context.Context) (<-chan capture.Frame, error) {
	if s.Dir == "" {
		return nil, errors.New("replay: Dir is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	entries, err := s.Manifest()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return nil, ErrAlreadyStarted
	}
	s.started = true
	s.state = capture.StateStarting
	s.out = make(chan capture.Frame)
	s.done = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.mu.Unlock()

	go s.run(runCtx, entries)
	return s.out, nil
}

// Close implements capture.Source. Idempotent, safe before Start.
func (s *Source) Close() error {
	s.mu.Lock()
	s.closed = true
	if s.state != capture.StateFailed {
		s.state = capture.StateStopped
	}
	cancel := s.cancel
	done := s.done
	started := s.started
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if started {
		<-done
	}
	return nil
}

// run walks the manifest pass by pass. Pacing happens *before* the frame it
// separates the frame from, so the first frame of a recording is emitted
// immediately rather than after however far into the day its timestamp was
// recorded.
func (s *Source) run(ctx context.Context, entries []Entry) {
	defer close(s.out)
	defer close(s.done)

	sleep := s.Sleeper
	if sleep == nil {
		sleep = sleepCtx
	}
	speed := s.Speed
	offset := int64(0)
	stride := s.stride(entries)

	for {
		for i, e := range entries {
			if ctx.Err() != nil {
				return
			}
			if speed > 0 && !(offset == 0 && i == 0) {
				gap := float64(s.stepNS(entries, i, stride)) / speed
				if gap > 0 {
					if err := sleep(ctx, time.Duration(gap)); err != nil {
						return
					}
				}
			}
			if err := s.emit(ctx, e, offset); err != nil {
				if ctx.Err() == nil {
					s.fail(err)
				}
				return
			}
		}
		if !s.Loop {
			// A replay that ended reports stopped, not running: a consumer
			// polling Stats().Alive() must pause detection rather than wait for
			// frames that will never arrive (docs/spec/13 "Capture removed").
			s.finish()
			return
		}
		offset += stride
	}
}

// stride is the timestamp advance applied per pass of a looping replay: the
// recording's own length plus one frame step, so timestamps keep moving forward
// across the wrap. Invariant 1 does not pause for a loop, and a tracker that saw
// time go backwards would reset its stability window for the wrong reason.
func (s *Source) stride(entries []Entry) int64 {
	if len(entries) == 0 {
		return 0
	}
	return s.step(entries) + entries[len(entries)-1].TsNS - entries[0].TsNS
}

// step is the recorded cadence: the first inter-frame gap, falling back to 10 fps
// for a one-frame recording. It also paces the wrap of a looping replay, so an
// idling screen keeps its recorded frame rate rather than hammering the consumer.
func (s *Source) step(entries []Entry) int64 {
	if len(entries) < 2 {
		return 100_000_000
	}
	if d := entries[1].TsNS - entries[0].TsNS; d > 0 {
		return d
	}
	return 100_000_000
}

// stepNS is the gap that precedes entries[i] within a pass.
func (s *Source) stepNS(entries []Entry, i int, stride int64) int64 {
	if i == 0 {
		// Wrap-around gap: from the last frame of the previous pass to the first
		// of this one.
		if len(entries) >= 2 {
			return stride - (entries[len(entries)-1].TsNS - entries[0].TsNS)
		}
		return stride
	}
	d := entries[i].TsNS - entries[i-1].TsNS
	if d < 0 {
		return 0 // a spliced recording: emit as fast as the consumer takes
	}
	return d
}

// emit decodes one manifest entry and delivers it on the pass offset.
func (s *Source) emit(ctx context.Context, e Entry, offset int64) error {
	path := filepath.Join(s.Dir, filepath.Clean(e.File))
	img, format, err := decode(path)
	if err != nil {
		return fmt.Errorf("replay: frame %s: %w", path, err)
	}
	b := img.Bounds()
	f := capture.Frame{
		Seq:                s.nextSeq(),
		SourceClock:        capture.ClockReplay,
		CaptureTimestampNS: e.TsNS + offset,
		Width:              b.Dx(),
		Height:             b.Dy(),
		PixFmt:             format,
		Image:              img,
		Hash:               capture.HashFrame(img),
	}
	if err := f.Validate(); err != nil {
		s.countDecode(err)
		return err
	}
	select {
	case s.out <- f:
	case <-ctx.Done():
		return ctx.Err()
	}
	s.recordDelivered()
	return nil
}

func (s *Source) nextSeq() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	return s.seq
}

func (s *Source) recordDelivered() {
	s.mu.Lock()
	s.stats.Delivered++
	s.stats.LastFrameAt = time.Now()
	if s.state != capture.StateRunning {
		s.state = capture.StateRunning
	}
	s.mu.Unlock()
}

func (s *Source) countDecode(err error) {
	s.mu.Lock()
	s.stats.DecodeErrors++
	s.stats.LastError = err.Error()
	s.state = capture.StateFailed
	s.mu.Unlock()
}

func (s *Source) finish() {
	s.mu.Lock()
	if s.state != capture.StateFailed {
		s.state = capture.StateStopped
	}
	s.mu.Unlock()
}

func (s *Source) fail(err error) {
	s.mu.Lock()
	s.state = capture.StateFailed
	s.stats.LastError = err.Error()
	s.mu.Unlock()
}

// decode reads one frame file and returns dense pixels. The reported format comes
// from the decoder that actually ran rather than from the file extension, because
// a fixture that lies about its own type is exactly the corruption a replay test
// should notice.
//
// Paletted PNGs — which is what a 20-colour synthetic corpus decodes to — are
// converted to RGBA before delivery. That is not tidiness: Frame.Image's contract
// is a dense picture, and a *image.Paletted frame would make every consumer
// choose between an At()-per-pixel path and a type switch, while the conversion
// makes a replayed frame byte-comparable with the same picture arriving from the
// capture card.
func decode(path string) (image.Image, capture.PixFmt, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	img, name, err := image.Decode(f)
	if err != nil {
		return nil, "", fmt.Errorf("decode: %w", err)
	}
	img = densify(img)
	switch name {
	case "png":
		return img, capture.PixPNG, nil
	case "jpeg":
		return img, capture.PixJPEG, nil
	default:
		if strings.Contains(name, "ycbcr") || strings.Contains(name, "yccr") {
			return img, capture.PixYCbCr, nil
		}
		return img, capture.PixFmt(name), nil
	}
}

// densify copies any image representation into an image.RGBA.
func densify(src image.Image) image.Image {
	if dst, ok := src.(*image.RGBA); ok {
		return dst
	}
	dst := image.NewRGBA(src.Bounds())
	draw.Draw(dst, dst.Bounds(), src, src.Bounds().Min, draw.Src)
	return dst
}

// sleepCtx is the default Sleeper.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var _ capture.Source = (*Source)(nil)
