// Package capture turns a video signal into frames the perception pipeline can
// consume (docs/spec/01 "Process domains": internal/capture owns "V4L2, ffmpeg,
// image replayer, Dolphin-dev capture").
//
// One contract, three implementations, deliberately indistinguishable to the
// code downstream. docs/spec/11 "E2E verification steps" item 4 is the reason:
// "Feed Dolphin video to same FrameSource consumer as production capture. No
// fixture-specific branches in OCR/matcher." A source that leaked its transport
// into Frame — an emulator frame with a different clock, a replayed frame that
// arrived on a device clock — would be a fork in every consumer, which is what
// that sentence forbids.
//
// The three implementations live in subpackages so this file stays the whole
// vocabulary:
//
//   - replay   — recorded PNG/JPEG frames plus their recorded timestamps
//     (docs/spec/12 "Frame replay": the gate that runs on every PR).
//   - ffmpegpipe — a supervised FFmpeg subprocess writing raw frames to a pipe,
//     used for both the UNO Q V4L2 capture card and the dev-host X11 grab of
//     Dolphin (ADR-0012 part 1: native media tools are subprocesses, never cgo).
//   - devices  — v4l2-ctl enumeration (docs/spec/10 "Hardware discovery and
//     preflight": "never assume /dev/video0").
//
// docs/spec/01 "Key invariants" item 1 binds everything here: every frame
// carries an explicit SourceClock and CaptureTimestampNS rather than a
// wall-clock-only label, because the dialogue tracker's stability window is
// measured on a clock that must not jump when NTP corrects the system time
// (internal/recognition rejects an Observation that violates it).
//
// docs/spec/01 "Parallelism and deadline contracts" binds the delivery
// mechanism: "One capture loop: bounded single-slot latest-frame queue for live
// runtime, unlike lossless recording" — see LatestSlot and Lossless in slot.go.
package capture

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"time"
)

// Source clock names. These are the only strings Frame.SourceClock may hold,
// because internal/recognition.Observation.Validate refuses an empty clock and
// the developer inspector has to be able to say which clock a latency number
// was measured on (docs/spec/13 "Correlation").
const (
	// ClockMonotonic stamps CaptureTimestampNS as nanoseconds since process
	// start, read from Go's monotonic clock. Used by every live source: FFmpeg
	// hands us raw pixels with no presentation timestamps, so the honest claim
	// is "this frame arrived at this monotonic instant", not "the device said
	// so".
	ClockMonotonic = "monotonic"
	// ClockReplay marks a timestamp recovered from a recording's own capture
	// clock. The number is the original device/monotonic reading stored at
	// record time, so a replayed timeline reproduces the original inter-frame
	// gaps — which is the entire point of fixture replay.
	ClockReplay = "replay"
	// ClockDevice is reserved for a capture path that reads the driver's own
	// buffer timestamps (direct V4L2 DQBUF, deferred by ADR-0012 part 1
	// §5). Nothing here produces it yet; it exists so the day it does, the
	// vocabulary does not change under the tracker.
	ClockDevice = "device"
)

// PixFmt names the pixel format of a frame as its producer delivered it, using
// FFmpeg's spelling where one exists. It is recorded verbatim rather than
// normalised to a Go type because docs/spec/10 tells preflight to record the
// negotiated "pixel format" of the capture chain, and because the string is
// what a format-negotiation failure ("the converter will not do 480i") is
// reported in.
type PixFmt string

// Known pixel formats. The rawvideo/rgb24 case is the one FFmpeg pipe sources
// deliver; mjpeg and yuyv422 are the two UVC formats docs/spec/05 prefers
// ("prefer UVC MJPEG or YUYV depending on hardware").
const (
	PixUnknown PixFmt = ""
	PixRGB24   PixFmt = "rgb24"
	PixRGBA    PixFmt = "rgba"
	PixYCbCr   PixFmt = "yuv420p"
	PixGray    PixFmt = "gray8"
	PixMJPEG   PixFmt = "mjpeg"
	PixYUYV422 PixFmt = "yuyv422"
	PixPNG     PixFmt = "png"
	PixJPEG    PixFmt = "jpeg"
)

// PixFmtOf derives the format name from an image's Go representation, for
// sources (replay) that decode through image/png and image/jpeg and only know
// the result type.
func PixFmtOf(img image.Image) PixFmt {
	switch img.(type) {
	case *image.RGBA:
		return PixRGBA
	case *image.YCbCr:
		return PixYCbCr
	case *image.Gray:
		return PixGray
	case nil:
		return PixUnknown
	default:
		// NRGBA (what image/png yields for RGB inputs), image.Image windows and
		// anything a future engine injects: named by its Go type rather than
		// guessed at, because a wrong format claim is worse than an odd one.
		return PixFmt(fmt.Sprintf("%T", img))
	}
}

// Frame is one immutable observation of the screen. internal/recognition.Observation
// consumes it (FrameSeq ← Seq, CaptureTimestampNS, SourceClock) and the ROI
// worker derives its region hash from Image, so the fields below are the
// producer side of that contract (docs/spec/05 "Runtime stages").
type Frame struct {
	// Seq is a per-source monotonic frame number starting at 1. It never
	// restarts across an FFmpeg reconnect, so a gap in Seq is a gap in video
	// and the inspector can show it as one; the source, not the consumer,
	// owns it.
	Seq int64
	// SourceClock names the clock CaptureTimestampNS belongs to — one of the
	// Clock* constants. Required (docs/spec/01 invariant 1).
	SourceClock string
	// CaptureTimestampNS is nanoseconds on SourceClock. Required and positive.
	CaptureTimestampNS int64
	// Width/Height are the delivered picture's pixel dimensions. A change is a
	// real event, not noise: docs/spec/12 lists "framebuffer size change" as a
	// perception case, and a resize invalidates every normalized ROI crop.
	Width, Height int
	// PixFmt names the delivered format.
	PixFmt PixFmt
	// Image is the picture. Implementations hand out a private buffer per
	// frame — never a reused slice — because a frame in flight to an ROI
	// worker must not be rewritten underneath it (docs/spec/05: a crop is a
	// copy, not a window onto a caller-owned buffer).
	Image image.Image
	// Hash is the cheap content hash of the whole frame (see HashFrame). Equal
	// hashes mean "the picture did not change"; different hashes mean it
	// definitely did. It is a liveness signal, not an OCR dedupe key — see
	// HashFrame for why the two are different hashes.
	Hash string
	// Dropped counts frames this source had and did not deliver since the
	// previous delivered frame: overwrites in the single-slot queue, truncated
	// reads, and rejected frames. Zero means a clean interval — the field the
	// "dropped-frame rate" in docs/spec/10 and the "video loss/freeze" case in
	// docs/spec/12 are computed from, carried per frame so a trace shows *when*
	// video went missing rather than only that it did.
	Dropped int64
}

// Validate is the producer-side mirror of recognition.Observation.Validate.
// Sources call it before publishing so a broken frame is a counted DecodeError
// rather than a tracker ERROR several milliseconds later, far from its cause.
func (f Frame) Validate() error {
	switch {
	case f.Image == nil:
		return errors.New("frame has no pixels")
	case f.SourceClock == "":
		return errors.New("source_clock is required (docs/spec/01 invariant 1)")
	case f.CaptureTimestampNS <= 0:
		return fmt.Errorf("capture_timestamp_ns must be positive on clock %q", f.SourceClock)
	case f.Width <= 0 || f.Height <= 0:
		return fmt.Errorf("bogus frame size %dx%d", f.Width, f.Height)
	case f.Seq < 0:
		return fmt.Errorf("frame seq %d is negative", f.Seq)
	default:
		return nil
	}
}

// Bounds reports the picture's size, preferring the explicit fields so a source
// that knows its negotiated geometry is not at the mercy of a mis-set Rect.
func (f Frame) Bounds() (w, h int) {
	if f.Width > 0 && f.Height > 0 {
		return f.Width, f.Height
	}
	if f.Image == nil {
		return 0, 0
	}
	r := f.Image.Bounds()
	return r.Dx(), r.Dy()
}

// put32/monotonic helpers are in clock.go.

// State is the lifecycle state reported in Stats. The set is fixed by
// docs/spec/13's resilience matrix: a source that vanishes must be seen to be
// reconnecting rather than silently idle ("Capture removed | Mark input
// offline, pause detection … and reconnect with backoff").
type State string

const (
	StateStarting     State = "starting"
	StateRunning      State = "running"
	StateReconnecting State = "reconnecting"
	StateStopped      State = "stopped"
	StateFailed       State = "failed"
)

// Stats is a source's self-report, the machine-readable half of
// docs/spec/10's "verify last frame freshness and dropped-frame rate" and of
// the "video loss/freeze" case in docs/spec/12 "Visual perception".
type Stats struct {
	// Delivered counts frames handed to the consumer since Start.
	Delivered uint64 `json:"delivered"`
	// DroppedSlot counts frames overwritten in the single-slot queue — the
	// live-runtime drop metric docs/spec/01's capture loop exists to make
	// visible. Always 0 for a source running in Lossless mode, which blocks
	// instead of dropping.
	DroppedSlot uint64 `json:"dropped_slot"`
	// DecodeErrors counts frames that arrived short, unreadable or invalid.
	DecodeErrors uint64 `json:"decode_errors"`
	// Restarts counts (re)spawns of a supervised subprocess after the first,
	// including failed spawn attempts — the number that separates "the card
	// was unplugged once" from "capture is flapping".
	Restarts uint64 `json:"restarts"`
	// FPS is the measured arrival rate over the trailing window, not the
	// requested rate. Zero until enough frames have arrived.
	FPS float64 `json:"fps"`
	// LastFrameAt is the wall time the newest frame arrived; its distance from
	// time.Now is the freshness check (docs/spec/10).
	LastFrameAt time.Time `json:"last_frame_at"`
	// State is the lifecycle state.
	State State `json:"state"`
	// LastError is the most recent failure text, kept because
	// docs/spec/13 requires an actionable operator action, and "it stopped" is
	// not one.
	LastError string `json:"last_error,omitempty"`
}

// Alive reports whether the source is currently producing video, which is the
// single question the degraded-mode logic asks (docs/spec/13 "Capture removed":
// pause detection, do not pretend the scene is unchanged).
func (s Stats) Alive() bool { return s.State == StateRunning }

// Source is the FrameSource contract of docs/spec/01. Implementations are safe
// for concurrent Stats, and Close is idempotent.
type Source interface {
	// ID is a stable, human-readable identity for logs and events
	// ("v4l2:/dev/video0", "x11::1+0,0", "replay:dialogue-advance-001"). It is
	// not unique across processes and is never a key.
	ID() string
	// Start begins producing frames on the returned channel and takes
	// ownership of ctx: cancellation stops the source and kills any child
	// process it supervises. The channel is closed when the source ends —
	// end-of-stream for a recording, Close/cancellation for a live source — so
	// a consumer can tell "the recording finished" from "I am still waiting".
	// Start is called at most once per Source value.
	Start(ctx context.Context) (<-chan Frame, error)
	// Stats returns a consistent snapshot; it never blocks on the pipeline.
	Stats() Stats
	// Close stops production, releases the child process, and closes the
	// channel returned by Start. It is safe to call more than once and safe to
	// call before Start.
	Close() error
}

// HashStride is the linear downsampling factor behind HashFrame.
const HashStride = 8

// HashFrame returns a cheap content hash of a whole frame: SHA-256 over the
// width, height and the RGB of every HashStride'th pixel on every HashStride'th
// row (a 1/8 linear downsample, i.e. 1/64 of the pixels).
//
// Why cheap, and what that buys and costs. A 640×480×3 frame is ~920 KB and a
// 1080p one is ~6 MB; hashing every byte at 30 fps on a 4-core ARM appliance is
// pure CPU that docs/spec/05 would rather spend on OCR. The 1/8 sample is
// enough to answer the question the *capture* layer asks — "is the picture
// frozen, or did a scene change happen?" (docs/spec/12 "video loss/freeze",
// "repeated identical frames") — because a freeze is bitwise identity and a
// scene change moves whole regions.
//
// It is explicitly NOT the hash docs/spec/05 uses to suppress duplicate OCR
// calls. One extra character of a typewriter reveal can land between two
// sampled pixels; that is invisible here and must not be invisible downstream,
// so the region-level dedupe key is vision.ImageHash, a lossless luma hash of
// the cropped ROI. Frame.Hash is a frame-level liveness signal; treat equality
// as "probably unchanged" and inequality as "definitely something moved".
func HashFrame(img image.Image) string { return DownsampleHash(img, HashStride) }

// DownsampleHash is HashFrame with the sampling factor exposed, for benchmarks
// of accuracy against cost. A stride ≤ 1 hashes every pixel.
func DownsampleHash(img image.Image, stride int) string {
	if img == nil {
		return ""
	}
	if stride < 1 {
		stride = 1
	}
	r := img.Bounds()
	h := sha256.New()
	// Dimensions are hashed first, so a picture and a crop of it that happen to
	// sample the same pixels still hash apart (docs/spec/12's "framebuffer size
	// change" case has to be visible in the frame identity).
	var head [8]byte
	binary.LittleEndian.PutUint32(head[0:4], uint32(r.Dx()))
	binary.LittleEndian.PutUint32(head[4:8], uint32(r.Dy()))
	h.Write(head[:])

	// One row of samples at a time into a reused buffer: the whole downsample of
	// a 1080p frame is 240×135×3 ≈ 97 KB, and allocating that per frame at 30 fps
	// is 3 MB/s of pure garbage on the appliance's only four cores.
	var row [4096 * 3]byte
	n := 0
	flush := func() {
		if n > 0 {
			h.Write(row[:n])
			n = 0
		}
	}
	for y := r.Min.Y; y < r.Max.Y; y += stride {
		for x := r.Min.X; x < r.Max.X; x += stride {
			if n+3 > len(row) {
				flush()
			}
			rr, gg, bb, _ := img.At(x, y).RGBA() // 16-bit, un-premultiplied
			row[n], row[n+1], row[n+2] = byte(rr>>8), byte(gg>>8), byte(bb>>8)
			n += 3
		}
		flush()
	}
	return hex.EncodeToString(h.Sum(nil))
}
