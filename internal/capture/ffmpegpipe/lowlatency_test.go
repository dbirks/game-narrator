package ffmpegpipe

import (
	"bytes"
	"context"
	"image"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// TestLowLatencyV4L2Args asserts the live-card argv the ADR-0076 policy names:
// low-latency demux/decoder flags, the small decode queue, the declared luma
// range per format, and the pre-decode decimation (the analysis rate is the
// v4l2 input's -framerate, so the card delivers at it). argv-only, so it runs
// on a host with no camera and no ffmpeg.
func TestLowLatencyV4L2Args(t *testing.T) {
	mj, err := V4L2("/dev/video1", 640, 480, 10, "mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(mj.Args(), "\x00")
	for _, want := range []string{"-fflags", "nobuffer", "-flags", "low_delay", "-thread_queue_size", "2",
		"-input_format", "mjpeg", "-vf", "setparams=range=full", "-framerate", "10", "-video_size", "640x480"} {
		if !strings.Contains(joined, want) {
			t.Errorf("mjpeg argv missing %q in %q", want, joined)
		}
	}
	// The analysis rate is the requested input rate: that is the pre-decode
	// decimation — the card emits 10 frames, the pipeline does not drop 50.
	if v := argAfter(mj.Args(), "-framerate"); v != "10" {
		t.Errorf("-framerate = %q, want 10 (the analysis rate)", v)
	}

	yu, err := V4L2("/dev/video1", 1920, 1080, 30, "yuyv422")
	if err != nil {
		t.Fatal(err)
	}
	joined = strings.Join(yu.Args(), "\x00")
	for _, want := range []string{"-input_format", "yuyv422", "-vf", "setparams=range=tv"} {
		if !strings.Contains(joined, want) {
			t.Errorf("yuyv argv missing %q in %q", want, joined)
		}
	}

	// Deinterlace composes with the range declaration, range first.
	yu.Input.Deinterlace = DeinterlaceBwdif
	if v := argAfter(yu.Args(), "-vf"); v != "setparams=range=tv,bwdif" {
		t.Errorf("-vf = %q, want setparams=range=tv,bwdif", v)
	}
	yu.Input.Deinterlace = DeinterlaceYadif
	if v := argAfter(yu.Args(), "-vf"); v != "setparams=range=tv,yadif=0:-1:0" {
		t.Errorf("-vf = %q, want the yadif chain", v)
	}

	// X11 and lavfi sources keep their argv: the live-card flags are a V4L2
	// default, not a global one (x11grab has no compressed stream to decimate).
	x11 := mustX11(t, ":1", 640, 480, 30, 0, 0)
	if j := strings.Join(x11.Args(), "\x00"); strings.Contains(j, "nobuffer") || strings.Contains(j, "thread_queue_size") {
		t.Errorf("x11grab argv gained live-card flags: %q", j)
	}
	tv := TestSource(320, 240, 15)
	if j := strings.Join(tv.Args(), "\x00"); strings.Contains(j, "nobuffer") {
		t.Errorf("lavfi argv gained live-card flags: %q", j)
	}
}

func TestVideoFilterValidation(t *testing.T) {
	s, err := V4L2("/dev/video0", 640, 480, 10, "mjpeg")
	if err != nil {
		t.Fatal(err)
	}
	s.Input.Deinterlace = "filmweave"
	if _, err := s.Start(context.Background()); err == nil {
		t.Error("Start accepted an unknown deinterlace filter")
	}
	s2, _ := V4L2("/dev/video0", 640, 480, 10, "mjpeg")
	s2.Input.ColorRange = "ultrawide"
	if _, err := s2.Start(context.Background()); err == nil {
		t.Error("Start accepted an unknown color range")
	}
}

// whiteRows counts the rows of an rgb24 frame that are more than half white.
func whiteRows(f *image.RGBA) int {
	n := 0
	for y := 0; y < f.Rect.Dy(); y++ {
		white := 0
		for x := 0; x < f.Rect.Dx(); x++ {
			p := f.Pix[(y*f.Rect.Dx()+x)*4]
			if p > 200 {
				white++
			}
		}
		if white > f.Rect.Dx()/2 {
			n++
		}
	}
	return n
}

// runRawFFmpeg feeds b into ffmpeg's rawvideo pipe input and returns the raw
// rgb24 bytes it wrote. vf is a full filter chain ("", "bwdif", …); a non-empty
// chain is prefixed with format=yuv420p, mirroring the production V4L2 path
// where the decoded card frames are YUV before the rgb24 conversion.
func runRawFFmpeg(t *testing.T, w, h int, b []byte, vf string) []byte {
	t.Helper()
	if vf != "" {
		vf = "format=yuv420p," + vf
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-f", "rawvideo",
		"-pix_fmt", "rgb24", "-video_size", strconv.Itoa(w) + "x" + strconv.Itoa(h),
		"-framerate", "30", "-i", "pipe:0"}
	if vf != "" {
		args = append(args, "-vf", vf)
	}
	args = append(args, "-frames:v", "30", "-f", "rawvideo", "-pix_fmt", "rgb24", "-")
	cmd := exec.Command(DefaultBin, args...)
	cmd.Stdin = bytes.NewReader(b)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg %q: %v", vf, err)
	}
	return out.Bytes()
}

// combFrames builds 30 frames of a 320x240 synthetic interlaced source: a
// white 2-row bar (x 40..w-40) moving down 2 rows per frame, encoded the way a
// 480i source combs — the even-row field shows the bar one row higher than the
// odd-row field. The displayed frame therefore carries TWO white rows; a
// deinterlaced frame carries ONE.
func combFrames(t *testing.T, w, h int) []byte {
	t.Helper()
	var buf bytes.Buffer
	for t0 := 0; t0 < 30; t0++ {
		top := 20 + 2*t0 // the even (field 0) row of the bar
		for y := 0; y < h; y++ {
			whiteRow := (y%2 == 0 && y == top) || (y%2 == 1 && y == top+1)
			for x := 0; x < w; x++ {
				if whiteRow && x >= 40 && x < w-40 {
					buf.WriteString("\xff\xff\xff")
				} else {
					buf.WriteString("\x00\x00\x00")
				}
			}
		}
	}
	return buf.Bytes()
}

// TestDeinterlaceOnSyntheticInterlacedSource runs the real FFmpeg on the
// synthetic comb above: unfiltered, the frame keeps its two white rows;
// bwdif and yadif deinterlace it to one. This is the docs/spec/12
// "capture deinterlacing" case proven without a 480i converter.
func TestDeinterlaceOnSyntheticInterlacedSource(t *testing.T) {
	requireFFmpeg(t)
	const w, h = 320, 240
	frames := combFrames(t, w, h)

	mk := func(vf string) int {
		out := runRawFFmpeg(t, w, h, frames, vf)
		if len(out) != 30*w*h*3 {
			t.Fatalf("output %d bytes, want %d", len(out), 30*w*h*3)
		}
		// The last frame (t=29): enough context for bwdif to have decided.
		last := out[(29*w*h)*3:]
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		copy(img.Pix, expandRGBA(last, w, h))
		return whiteRows(img)
	}

	base := mk("")
	if base < 2 {
		t.Fatalf("the unfiltered comb frame has %d white rows, want 2 — the fixture is wrong", base)
	}
	for _, vf := range []string{"bwdif", "yadif=0:-1:0"} {
		if got := mk(vf); got >= base {
			t.Errorf("-vf %s: %d white rows, want fewer than the unfiltered %d (the comb must be gone)", vf, got, base)
		}
	}
}

// TestColorRangeNormalization runs the real FFmpeg on studio-range YUV bytes
// (Y=16, the BT.601 limited black): declaring the range limited must decode
// them to true black, declaring them full must leave the 16 visible — the
// measurable difference the setparams declaration makes (ADR-0076).
func TestColorRangeNormalization(t *testing.T) {
	requireFFmpeg(t)
	const w, h = 8, 8
	// yuv420p: Y plane w*h, then U and V planes w/2*h/2. All Y=16, U=V=128.
	var yuv []byte
	for i := 0; i < w*h; i++ {
		yuv = append(yuv, 16)
	}
	for i := 0; i < w/2*h/2; i++ {
		yuv = append(yuv, 128, 128)
	}
	dec := func(vf string) int {
		args := []string{"-hide_banner", "-loglevel", "error", "-f", "rawvideo",
			"-pix_fmt", "yuv420p", "-video_size", strconv.Itoa(w) + "x" + strconv.Itoa(h),
			"-framerate", "30", "-i", "pipe:0"}
		if vf != "" {
			args = append(args, "-vf", vf)
		}
		args = append(args, "-frames:v", "1", "-f", "rawvideo", "-pix_fmt", "rgb24", "-")
		cmd := exec.Command(DefaultBin, args...)
		cmd.Stdin = bytes.NewReader(yuv)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("ffmpeg %q: %v", vf, err)
		}
		if len(out.Bytes()) != w*h*3 {
			t.Fatalf("output %d bytes, want %d", out.Len(), w*h*3)
		}
		sum := 0
		for i := 0; i < len(out.Bytes()); i += 3 {
			sum += int(out.Bytes()[i]) // luma of the red channel on a grey frame
		}
		return sum / (w * h)
	}
	limited := dec("setparams=range=tv")
	full := dec("setparams=range=full")
	if limited > 4 {
		t.Errorf("limited-range decode of studio black Y=16 is luma %d, want ~0", limited)
	}
	if full < 10 || full < limited {
		t.Errorf("full-range decode of Y=16 is luma %d, limited is %d: the declaration must make the difference (full must keep the 16)", full, limited)
	}
	t.Logf("studio Y=16: limited→%d luma, full→%d luma", limited, full)
}

// expandRGBA copies packed rgb24 into image.RGBA stride (4 bytes/px).
func expandRGBA(rgb []byte, w, h int) []byte {
	out := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		out[i*4] = rgb[i*3]
		out[i*4+1] = rgb[i*3+1]
		out[i*4+2] = rgb[i*3+2]
		out[i*4+3] = 0xff
	}
	return out
}
