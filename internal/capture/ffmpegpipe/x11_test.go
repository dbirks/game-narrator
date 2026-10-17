//go:build unix

package ffmpegpipe

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// The X11 gate: a real Xvfb display, painted by real X clients, captured through
// the same ffmpegpipe.Source the UNO Q capture card uses.
//
// This is the test that makes docs/spec/11 "E2E verification steps" item 4 more
// than a sentence. The Dolphin bridge (ADR-0007) is `ffmpeg -f x11grab` on an
// Xvfb display, cropped to the render window's geometry. Nothing about that path
// is emulator-specific, so a pass here proves the transport Milestone 3 will
// depend on, and a failure there can be localised by comparing against this.
//
// The painted-colour assertion is the pixel-fidelity half of docs/spec/12
// "Emulator/hardware": the pipeline must receive the colours that were on the
// screen, not a tinted or offset approximation of them. A format-conversion bug
// in FFmpeg, a wrong stride assumption in the reader, or an ignored crop all
// present as plausible-looking frames and would otherwise be discovered by OCR
// quality three layers away.

const (
	xvfbW, xvfbH = 320, 240
	grabW, grabH = 64, 48
	// ImageMagick's window paints its image inset by a two-pixel frame, so
	// assertions sample well inside the picture rather than at its edge.
	paintInset = 4
)

var (
	paintRed   = color.RGBA{220, 20, 20, 255}
	paintGreen = color.RGBA{20, 200, 20, 255}
	paintBlue  = color.RGBA{30, 40, 210, 255}
)

func TestX11GrabCapturesPaintedPixels(t *testing.T) {
	disp, cleanup := startXvfb(t)
	defer cleanup()
	display := fmt.Sprintf(":%d", disp)

	// A solid window is the smallest thing that can prove the whole chain: XCB
	// grab, bgr0→rgb24 conversion, pipe framing, and the reader's geometry
	// assumptions. Anything wrong shows as a colour or a shear.
	stop := paintSolid(t, display, paintRed)
	defer stop()

	s, err := X11(display, grabW, grabH, 15, paintInset, paintInset)
	if err != nil {
		t.Fatalf("X11: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start on Xvfb %s: %v", display, err)
	}
	defer s.Close()

	got := waitForFrame(t, ch, 10*time.Second, func(f capture.Frame) bool {
		return near(f, paintRed, 16)
	})
	if got == nil {
		st := s.Stats()
		t.Fatalf("no frame matched the painted red on %s (state=%s delivered=%d restarts=%d last_error=%q)",
			display, st.State, st.Delivered, st.Restarts, st.LastError)
	}
	if got.Width != grabW || got.Height != grabH {
		t.Fatalf("grabbed %dx%d, want %dx%d", got.Width, got.Height, grabW, grabH)
	}
	if got.PixFmt != capture.PixRGB24 {
		t.Errorf("grabbed format = %q, want rgb24", got.PixFmt)
	}
	if got.SourceClock != capture.ClockMonotonic {
		t.Errorf("clock = %q, want monotonic", got.SourceClock)
	}
	t.Logf("x11grab %s: seq=%d %dx%d fmt=%s matched painted %v", display, got.Seq, got.Width, got.Height, got.PixFmt, paintRed)

	// Second half: change the screen and require the source to follow. A single
	// correct screenshot proves nothing about a *live* source; following a
	// repaint is the property that distinguishes it from a replay, and the
	// mirror image of the freeze case docs/spec/12 lists.
	stop()
	stop = paintSolid(t, display, paintGreen)
	defer stop()

	redFrames, greenFrames := 0, 0
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) && greenFrames == 0 {
		select {
		case f, ok := <-ch:
			if !ok {
				t.Fatal("X11 frame channel closed while the display was live")
			}
			switch {
			case near(f, paintGreen, 16):
				greenFrames++
			case near(f, paintRed, 16):
				redFrames++
			}
		case <-time.After(100 * time.Millisecond):
			if s.Stats().State == capture.StateFailed {
				t.Fatalf("source failed: %s", s.Stats().LastError)
			}
		}
	}
	if greenFrames == 0 {
		t.Errorf("screen repainted to %v and the source never delivered it (%d stale red frames still arriving)", paintGreen, redFrames)
	} else {
		t.Logf("x11grab followed the repaint: %d red frames then %d green", redFrames, greenFrames)
	}
}

// TestX11OffsetCropGrabsTheRequestedWindow checks the crop specifically: the
// Dolphin bridge grabs the render window's geometry, not the screen, and
// ADR-0007 is explicit that the whole root window must never be captured — a
// grab that ignored the offset would pass a solid-colour test and then feed the
// OCR pipeline the wrong part of the screen every time.
func TestX11OffsetCropGrabsTheRequestedWindow(t *testing.T) {
	disp, cleanup := startXvfb(t)
	defer cleanup()
	display := fmt.Sprintf(":%d", disp)

	// Left half red, right half green, both full height: an offset inside the
	// right half must return green and never red, and vice versa. No tolerance
	// band can hide a misapplied offset.
	img := image.NewRGBA(image.Rect(0, 0, xvfbW, xvfbH))
	for y := 0; y < xvfbH; y++ {
		for x := 0; x < xvfbW; x++ {
			if x < xvfbW/2 {
				img.Set(x, y, paintRed)
			} else {
				img.Set(x, y, paintGreen)
			}
		}
	}
	stop := paintImage(t, display, img)
	defer stop()

	for _, tc := range []struct {
		name string
		x, y int
		want color.RGBA
		deny color.RGBA
	}{
		// Sampled well inside each half: the window's own two-pixel border is
		// not part of the picture and must not be mistaken for a wrong crop.
		{"left-of-centre", 20, 60, paintRed, paintGreen},
		{"right-of-centre", xvfbW/2 + 40, 60, paintGreen, paintRed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := X11(display, 32, 32, 15, tc.x, tc.y)
			if err != nil {
				t.Fatalf("X11: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ch, err := s.Start(ctx)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Close()

			var matched, denied int
			deadline := time.Now().Add(6 * time.Second)
			for matched < 3 && time.Now().Before(deadline) {
				select {
				case f, ok := <-ch:
					if !ok {
						t.Fatal("channel closed")
					}
					switch {
					case near(f, tc.want, 16):
						matched++
					case near(f, tc.deny, 16):
						denied++
					}
				case <-time.After(50 * time.Millisecond):
				}
			}
			if denied > 0 {
				t.Errorf("%d/%d frames showed the far half of the screen: the +,%d,%d crop is not being applied", denied, matched+denied, tc.x, tc.y)
			}
			if matched < 3 {
				st := s.Stats()
				t.Errorf("only %d/%d frames matched the expected colour (state=%s delivered=%d)", matched, matched+denied, st.State, st.Delivered)
			}
		})
	}
}

// TestX11FrozenScreenIsDetectable is the docs/spec/12 "video loss/freeze" case
// seen from the source side: when nothing is being painted, the frames must be
// *identical*, which is what makes a freeze distinguishable from live video.
// Without this, a capture path that happened to emit the same buffer forever
// would look healthy in every other test here.
func TestX11FrozenScreenIsDetectable(t *testing.T) {
	disp, cleanup := startXvfb(t)
	defer cleanup()
	display := fmt.Sprintf(":%d", disp)
	stop := paintSolid(t, display, paintBlue)
	defer stop()

	s, _ := X11(display, grabW, grabH, 15, paintInset, paintInset)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := s.Start(ctx)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	frames := collect(t, ch, 8, 8*time.Second)
	if len(frames) < 8 {
		t.Fatalf("only %d frames", len(frames))
	}
	hashes := map[string]int{}
	for _, f := range frames {
		hashes[f.Hash]++
	}
	if len(hashes) != 1 {
		t.Errorf("a static screen produced %d distinct frame hashes: Frame.Hash is not a content hash, so a real freeze could never be detected", len(hashes))
	}
	if first, second := frames[0].CaptureTimestampNS, frames[1].CaptureTimestampNS; second <= first {
		t.Error("timestamps identical across a frozen screen; they should still advance (a freeze is *time passing with no pixels*)")
	}
	t.Logf("frozen screen: %d frames, 1 distinct hash, timestamps advanced %d ns", len(frames), frames[1].CaptureTimestampNS-frames[0].CaptureTimestampNS)
}

// --- X test harness --------------------------------------------------------

// startXvfb starts a private Xvfb on a free display number. The number is
// probed rather than assumed, because colliding with a developer's own :0 (or a
// parallel test) would make the failure look like a capture bug.
func startXvfb(t *testing.T) (num int, cleanup func()) {
	t.Helper()
	bin, err := exec.LookPath("Xvfb")
	if err != nil {
		t.Skipf("NOT RUN: Xvfb not installed (%v); it is a dev dependency (xorg-server-xvfb in scripts/bootstrap-arch.sh)", err)
	}
	if _, err := exec.LookPath("magick"); err != nil {
		if _, err2 := exec.LookPath("display"); err2 != nil {
			t.Skipf("NOT RUN: no ImageMagick to paint the display with (%v / %v)", err, err2)
		}
	}
	for n := 90; n < 200; n++ {
		sock := fmt.Sprintf("/tmp/.X11-unix/X%d", n)
		if _, err := os.Stat(sock); err == nil {
			continue // taken by a live server
		}
		_ = os.Remove(fmt.Sprintf("/tmp/.X%d-lock", n)) // stale lock from a killed server
		cmd := exec.Command(bin, fmt.Sprintf(":%d", n), "-screen", "0",
			fmt.Sprintf("%dx%dx24", xvfbW, xvfbH), "-nolisten", "tcp")
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		logFile, lerr := os.Create(filepath.Join(t.TempDir(), "xvfb.log"))
		if lerr != nil {
			t.Fatalf("xvfb log: %v", lerr)
		}
		cmd.Stdout, cmd.Stderr = logFile, logFile
		if err := cmd.Start(); err != nil {
			t.Fatalf("start Xvfb: %v", err)
		}
		ready := false
		for i := 0; i < 100; i++ {
			if _, err := os.Stat(sock); err == nil {
				ready = true
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if !ready {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			b, _ := os.ReadFile(logFile.Name())
			t.Skipf("NOT RUN: Xvfb :%d never came up (%s)", n, b)
		}
		t.Cleanup(func() {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			_ = cmd.Wait()
			_ = logFile.Close()
			// A killed Xvfb leaves its socket and lock behind; leaving them would
			// make the next run believe these displays are all taken.
			_ = os.Remove(sock)
			_ = os.Remove(fmt.Sprintf("/tmp/.X%d-lock", n))
		})
		return n, func() {}
	}
	t.Skip("NOT RUN: no free X display number in 90..199")
	return 0, func() {}
}

// paintImage maps a real top-level X window containing img and returns a
// function that closes it. A real window is what the dev-host path actually
// grabs (Dolphin's render window), so the test grabs a real window too.
//
// Two alternatives were tried on this host and rejected with measurements:
// `xsetroot -solid` sets the root window's background pixmap without repainting
// the framebuffer, and with no window manager to generate exposures a grab kept
// returning the old pixels (0 of 3072 sampled pixels matched the requested
// colour); `magick display -window root` paints into the root but then only
// matched at offset +0,0 and returned black for every cropped grab — which would
// have made the crop test below pass by accident rather than by proving the
// offset. A mapped window matches at any offset, reproducibly.
func paintImage(t *testing.T, display string, img image.Image, geomExtra ...string) func() {
	t.Helper()
	path := filepath.Join(t.TempDir(), fmt.Sprintf("paint-%d.png", time.Now().UnixNano()))
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("paint file: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatalf("paint png: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("paint png close: %v", err)
	}
	bin := "magick"
	argv := []string{"display", "-border", "0", "-geometry", fmt.Sprintf("%dx%d+0+0", xvfbW, xvfbH)}
	if _, err := exec.LookPath(bin); err != nil {
		bin = "display" // ImageMagick 6
		argv = argv[1:]
	}
	argv = append(argv, geomExtra...)
	argv = append(argv, path)
	cmd := exec.Command(bin, argv...)
	cmd.Env = append(os.Environ(), "DISPLAY="+display)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	devnull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("devnull: %v", err)
	}
	cmd.Stdout, cmd.Stderr = devnull, devnull
	if err := cmd.Start(); err != nil {
		_ = devnull.Close()
		t.Fatalf("start painter (%s %v): %v", bin, argv, err)
	}
	stopped := false
	return func() {
		if stopped {
			return
		}
		stopped = true
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-done
		}
		_ = devnull.Close()
		// Give the server a moment to reclaim the pixels before a test paints
		// the same rectangle again.
		time.Sleep(100 * time.Millisecond)
	}
}

func paintSolid(t *testing.T, display string, c color.RGBA) func() {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, xvfbW, xvfbH))
	for y := 0; y < xvfbH; y++ {
		for x := 0; x < xvfbW; x++ {
			img.Set(x, y, c)
		}
	}
	return paintImage(t, display, img)
}

// near reports whether every sampled pixel is within tol of want.
func near(f capture.Frame, want color.RGBA, tol uint8) bool {
	if f.Image == nil {
		return false
	}
	b := f.Image.Bounds()
	if b.Dx() < 8 || b.Dy() < 8 {
		return false // the size itself is asserted by the caller
	}
	for y := b.Min.Y; y < b.Max.Y; y += 2 {
		for x := b.Min.X; x < b.Max.X; x += 2 {
			r, g, bb, _ := f.Image.At(x, y).RGBA()
			if !close8(uint8(r>>8), want.R, tol) || !close8(uint8(g>>8), want.G, tol) || !close8(uint8(bb>>8), want.B, tol) {
				return false
			}
		}
	}
	return true
}

func close8(a, b, tol uint8) bool {
	if a > b {
		return a-b <= tol
	}
	return b-a <= tol
}

func waitForFrame(t *testing.T, ch <-chan capture.Frame, within time.Duration, p func(capture.Frame) bool) *capture.Frame {
	t.Helper()
	deadline := time.NewTimer(within)
	defer deadline.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok {
				return nil
			}
			if p(f) {
				got := f
				return &got
			}
		case <-deadline.C:
			return nil
		}
	}
}
