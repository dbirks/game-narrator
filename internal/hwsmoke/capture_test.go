package hwsmoke

// TestHWCaptureDevice is the executable half of docs/spec/10 "Capture one
// live screenshot … verify last frame freshness and dropped-frame rate",
// and of the "matching via USB VID/PID and stable path; never assume
// /dev/video0" discovery rule.
//
// On a host with no capture node the test is NOT RUN (a dev host without a
// dongle is a normal state, per internal/doctor), never a vacuous pass:
// the check needs a real UVC device to measure freshness against.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/devices"
	"github.com/dbirks/game-narrator/internal/capture/ffmpegpipe"
)

// grabWindow is the measurement interval docs/spec/10 asks for the
// dropped-frame rate ("verify … dropped-frame rate"): long enough to be
// meaningful, short enough that the hardware gate stays a smoke test.
const grabWindow = 10 * time.Second

// maxFreshness is the "last frame freshness" bound: a live capture card
// delivering at 15 fps cannot be more than 2 s stale when the reader is
// actively draining. A camera pointed at a dead HDMI source still ticks —
// this check fails on the pipeline, not on the picture content (the
// non-blank check is separate and allows a black game frame).
const maxFreshness = 2 * time.Second

// nonBlankEntropyBits and nonBlankLumaValues are the non-blank threshold:
// a live sensor's noise floor alone spreads luma over this range. Values
// are measured and logged at run time, so a host that fails has the
// evidence for a threshold review, not a bare FAIL.
const (
	nonBlankEntropyBits = 1.0
	nonBlankLumaValues  = 8
)

// maxDropRate is the smoke bound on the dropped-frame rate: the
// single-slot live queue drops when the consumer stalls, so a healthy
// pipeline across 10 s should deliver clean. Sustained play (docs/spec/16
// M8) measures the longer-horizon number; this bounds the smoke window.
const maxDropRate = 0.10

func TestHWCaptureDevice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*grabWindow+30*time.Second)
	defer cancel()

	devs, err := devices.List(ctx)
	if err != nil {
		if errors.Is(err, devices.ErrToolUnavailable) {
			notRun(t, "v4l2-ctl is not installed",
				"install v4l-utils (pacman -S v4l-utils / apt install v4l-utils) and re-run")
		}
		t.Fatalf("v4l2 enumeration failed: %v", err)
	}
	if len(devs) == 0 {
		notRun(t, "no V4L2 capture device found (verified with v4l2-ctl --list-devices)",
			"attach the UVC HDMI capture dongle through the USB hub (docs/spec/10 physical "+
				"connection) and re-run make test-hardware")
	}

	// Record the full enumeration: identity, stable names, and every
	// format/size/fps the driver advertises — the evidence docs/spec/10
	// says preflight must record.
	for i := range devs {
		logDevice(t, &devs[i])
	}

	dev, why, missing := selectDevice(devs)
	if missing != "" {
		notRun(t, missing, howToFixCapture(missing))
	}
	t.Logf("selected device: %s name=%q (selection: %s)", dev.Path, dev.Name, why)

	// Pick the mode: prefer mjpeg (what UVC cards actually deliver and what
	// FFmpeg's v4l2 demuxer handles), then yuyv422, at the largest size the
	// format offers, at its first advertised rate.
	pixFmt, size, fps := pickMode(dev)
	if pixFmt == "" {
		notRun(t, fmt.Sprintf("device %s advertises no mjpeg/yuyv422 mode", dev.Path),
			"record the advertised modes above and file the gap against the capture card; "+
				"ffmpegpipe.V4L2 refuses formats it cannot hand to FFmpeg")
	}
	t.Logf("negotiating %s %dx%d @ %s fps", pixFmt, size.W, size.H, rateLabel(fps))

	src, err := ffmpegpipe.V4L2(dev.Path, size.W, size.H, fps, pixFmt)
	if err != nil {
		t.Fatalf("ffmpeg source construction failed: %v", err)
	}
	frames, err := src.Start(ctx)
	if err != nil {
		st := src.Stats()
		t.Fatalf("capture start failed (state=%s last_error=%s): %v", st.State, st.LastError, err)
	}
	defer src.Close()

	// Drain for the measurement window, or until the source ends.
	windowStart := time.Now()
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	var (
		delivered int64
		dropped   int64
		last      capture.Frame
	)
drain:
	for {
		select {
		case f, ok := <-frames:
			if !ok {
				break drain
			}
			if err := f.Validate(); err != nil {
				t.Fatalf("delivered frame failed Validate: %v", err)
			}
			delivered++
			dropped += f.Dropped
			last = f
		case <-ticker.C:
			if time.Since(windowStart) >= grabWindow {
				break drain
			}
		}
	}
	st := src.Stats()
	age := time.Duration(0)
	if delivered > 0 && last.SourceClock == capture.ClockMonotonic {
		age = time.Duration(capture.MonotonicNS()-last.CaptureTimestampNS) * time.Nanosecond
	}
	t.Logf("measured over %s: delivered=%d dropped=%d decoded_errors=%d restarts=%s measured_fps=%.1f state=%s",
		roundDuration(time.Since(windowStart)), delivered, dropped, st.DecodeErrors,
		restartLabel(st.Restarts), st.FPS, st.State)

	if delivered == 0 {
		t.Fatalf("no frames delivered in %s (state=%s last_error=%s): the FFmpeg capture path produced nothing",
			grabWindow, st.State, st.LastError)
	}

	// Freshness: the last delivered frame's own monotonic stamp, measured
	// against the same clock the source stamped it with — not a wall clock
	// that NTP can step.
	if last.SourceClock != capture.ClockMonotonic {
		t.Fatalf("last frame stamped on clock %q, want %q", last.SourceClock, capture.ClockMonotonic)
	}
	if age > maxFreshness {
		t.Fatalf("last frame is stale: %s old (bound %s)", roundDuration(age), maxFreshness)
	}
	t.Logf("freshness: last frame %s old (bound %s)", roundDuration(age), maxFreshness)

	// Non-blank: entropy of the last frame's luma. A dead HDMI source can
	// still tick frames at full rate, so rate alone proves nothing.
	entropy, distinct := lumaEntropy(last.Image)
	t.Logf("non-blank: luma entropy %.2f bits (bound %.1f), distinct luma values %d (bound %d), hash=%s",
		entropy, nonBlankEntropyBits, distinct, nonBlankLumaValues, last.Hash)
	if entropy < nonBlankEntropyBits || distinct < nonBlankLumaValues {
		t.Fatalf("frame looks blank (entropy %.2f bits, %d distinct luma values): check the "+
			"converter output and the capture input", entropy, distinct)
	}

	// Dropped-frame rate: the spec's own metric, computed from the per-frame
	// Dropped counter the source owns (docs/spec/10).
	rate := 0.0
	if total := delivered + dropped; total > 0 {
		rate = float64(dropped) / float64(total)
	}
	if rate > maxDropRate {
		t.Fatalf("dropped-frame rate %.1f%% over %s exceeds the %.0f%% smoke bound "+
			"(delivered=%d dropped=%d): the consumer or the card cannot keep the rate",
			rate*100, grabWindow, maxDropRate*100, delivered, dropped)
	}
	t.Logf("dropped-frame rate: %.1f%% (delivered=%d dropped=%d, bound %.0f%%)",
		rate*100, delivered, dropped, maxDropRate*100)
}

// logDevice records one enumerated node with its identity, stable names and
// every advertised mode — the "record formats, frame sizes, fps" line of
// docs/spec/10's preflight.
func logDevice(t *testing.T, d *devices.Device) {
	t.Helper()
	t.Logf("device: %s name=%q kind=%q bus=%q by-id=%q by-path=%q",
		d.Path, d.Name, d.Kind, d.BusInfo, d.StableID, d.ByPath)
	for _, f := range d.Formats {
		parts := make([]string, len(f.Sizes))
		for j, s := range f.Sizes {
			parts[j] = fmt.Sprintf("%dx%d@%s", s.W, s.H, fpsList(s.FPS))
		}
		t.Logf("  format %s (%s): %s", f.PixFmt, f.Description, strings.Join(parts, ", "))
	}
	if d.FormatsError != "" {
		t.Logf("  formats error for %s: %s", d.Path, d.FormatsError)
	}
}
