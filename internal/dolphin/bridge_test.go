//go:build unix

package dolphin

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Unit tests with a fake process runner (docs/spec/12 "Unit" mode): the
// config files, the FIFO protocol bytes, the isolation rules, the readiness
// predicate and the idempotent Stop are all asserted against fakes — no
// emulator, no display, no X server. The real-hardware run lives in
// real_test.go and skips (NOT RUN) where the env or tools are absent.

// testEnv wires a bridge to fake processes and a scripted Grab.
type testEnv struct {
	b              Bridge
	dataDir        string
	imagePath      string
	dolphinArgs    []string
	dolphinDisplay string
	xvfbDisplay    string
	stoppedDolphin int32
	stoppedXvfb    int32
	grabCalls      int32
}

// solidImage is a flat-luma image (entropy 0 — the "not yet rendered" case).
func solidImage(w, h, v int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := (y*w + x) * 4
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = byte(v), byte(v), byte(v), 0xff
		}
	}
	return img
}

// noiseImage is a deterministic LCG-noise image: high entropy, and different
// per seed (two seeds differ by a large RMSE, as real frames do).
func noiseImage(w, h, seed int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	x := uint32(seed + 1)
	for y := 0; y < h; y++ {
		for px := 0; px < w; px++ {
			x = x*1103515245 + 12345
			r := byte((x >> 16) % 256)
			x = x*1103515245 + 12345
			g := byte((x >> 16) % 256)
			i := (y*w + px) * 4
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, 255-r, 0xff
		}
	}
	return img
}

// brightNoiseImage is a full-frame bright scene — the stand-in for a title
// screen whose sky/sea fills the whole window (the letterbox bands measure
// bright, as the spike measured 0.33-0.60 on the real title).
func brightNoiseImage(w, h, seed int) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	x := uint32(seed + 1)
	for y := 0; y < h; y++ {
		for px := 0; px < w; px++ {
			x = x*1103515245 + 12345
			r := 110 + byte((x>>16)%146)
			x = x*1103515245 + 12345
			g := 110 + byte((x>>16)%146)
			x = x*1103515245 + 12345
			b := 110 + byte((x>>16)%146)
			i := (y*w + px) * 4
			img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = r, g, b, 0xff
		}
	}
	return img
}

// letterboxedImage is a high-entropy centre with black top/bottom bands —
// the shape of the opening movie the classifier must recognise.
func letterboxedImage(w, h, seed int) image.Image {
	img := noiseImage(w, h-60, seed)
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var c color.Color
			if y >= 30 && y < h-30 {
				c = img.At(x, y-30)
			} else {
				c = image.Black
			}
			r, g, b, a := c.RGBA()
			i := (y*w + x) * 4
			out.Pix[i], out.Pix[i+1], out.Pix[i+2], out.Pix[i+3] = byte(r>>8), byte(g>>8), byte(b>>8), byte(a>>8)
		}
	}
	return out
}

// newTestBridge wires a bridge to fake processes and a scripted Grab;
// mutate tweaks the Options (tests shorten the ProveInput windows).
func newTestBridge(t *testing.T, grab func(call int, r Region) image.Image, mutate ...func(*Options)) *testEnv {
	t.Helper()
	dataDir := t.TempDir()
	imgPath := filepath.Join(dataDir, "image.ciso")
	if err := os.WriteFile(imgPath, []byte("fake user-supplied game image bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(EnvGameImage, imgPath)
	t.Setenv(EnvDevDolphin, "1")

	env := &testEnv{dataDir: dataDir, imagePath: imgPath}
	opts := Options{
		DataDir: dataDir,
		StartXvfb: func(display, geometry string, logf io.Writer) (int, func() error, error) {
			env.xvfbDisplay = display
			return 9999, func() error { atomic.AddInt32(&env.stoppedXvfb, 1); return nil }, nil
		},
		StartDolphin: func(display string, args []string, logf io.Writer) (int, func() error, error) {
			env.dolphinDisplay = display
			env.dolphinArgs = append([]string{}, args...)
			return 9998, func() error { atomic.AddInt32(&env.stoppedDolphin, 1); return nil }, nil
		},
		FindWindow:     func(string) (string, error) { return "0x1234", nil },
		WindowGeometry: func(string, string) (Region, error) { return Region{X: 10, Y: 20, W: 640, H: 480}, nil },
		ScreenSize:     func(string) (int, int, error) { return 1280, 1024, nil },
		DisplayUp:      func(string) bool { return true },
		Grab: func(display string, r Region) (image.Image, error) {
			n := int(atomic.AddInt32(&env.grabCalls, 1))
			if grab != nil {
				return grab(n, r), nil
			}
			return solidImage(r.W, r.H, 0), nil
		},
	}
	for _, m := range mutate {
		m(&opts)
	}
	b, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	env.b = b
	return env
}

func mustLaunch(t *testing.T, env *testEnv, spec LaunchSpec) {
	t.Helper()
	ctx := context.Background()
	if err := env.b.Launch(ctx, spec); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if st := env.b.State(); st.State != StateRunning {
		t.Fatalf("state = %s, want running (last error: %s)", st.State, st.LastError)
	}
}

func TestLaunchConfigFilesAndIsolation(t *testing.T) {
	env := newTestBridge(t, nil)
	mustLaunch(t, env, LaunchSpec{})

	st := env.b.State()

	// Isolation: the user dir is under the app data dir, never the real one.
	if !strings.HasPrefix(st.UserDir, env.dataDir) {
		t.Fatalf("user dir %s is not under the data dir %s", st.UserDir, env.dataDir)
	}
	if home, _ := os.UserHomeDir(); home != "" &&
		(strings.HasPrefix(st.UserDir, filepath.Join(home, ".local", "share", "dolphin-emu")) ||
			strings.HasPrefix(st.UserDir, filepath.Join(home, ".config", "dolphin-emu"))) {
		t.Fatalf("the bridge touched the real Dolphin dir: %s", st.UserDir)
	}

	// Dolphin.ini: the proven config, BackgroundInput mandatory, and the Slot
	// A raw memory card bound to a file inside the isolated user dir
	// (ADR-0043 amendment 2: with the 2609 GCI-folder defaults the measured
	// new-game save failed — the explicit card is the fix).
	dini, err := os.ReadFile(filepath.Join(st.UserDir, "Config", "Dolphin.ini"))
	if err != nil {
		t.Fatalf("read Dolphin.ini: %v", err)
	}
	for _, want := range []string{"[Core]", "GFXBackend = OGL", "SlotA = 1", "MemcardAPath = " + st.MemcardPath, "[Input]", "BackgroundInput = True", "Fullscreen = False", "[Interface]", "OnScreenDisplayMessages = False"} {
		if !strings.Contains(string(dini), want) {
			t.Errorf("Dolphin.ini missing %q:\n%s", want, dini)
		}
	}
	if st.MemcardPath == "" {
		t.Errorf("the status records no memcard path")
	} else if !strings.HasPrefix(st.MemcardPath, st.UserDir) {
		t.Errorf("the memcard path %s is not inside the isolated user dir %s", st.MemcardPath, st.UserDir)
	}
	// The card file's PARENT dir exists before launch (the file itself is
	// created by Dolphin on first save — the real test's exit check asserts
	// its non-zero size as the use proof).
	if fi, err := os.Stat(filepath.Dir(st.MemcardPath)); err != nil || !fi.IsDir() {
		t.Errorf("the memcard parent dir %s was not created: %v", filepath.Dir(st.MemcardPath), err)
	}

	// GCPadNew.ini: the Pipes pad with the proven mapping. The device line
	// carries the MEASURED index (the raw readdir order of <user>/Pipes —
	// 2609 numbers devices by enumeration position, ADR-0043), not an
	// assumed one.
	if st.PipeDevice == "" {
		t.Fatalf("the status records no gcpad device string")
	}
	gc, err := os.ReadFile(filepath.Join(st.UserDir, "Config", "GCPadNew.ini"))
	if err != nil {
		t.Fatalf("read GCPadNew.ini: %v", err)
	}
	for _, want := range []string{
		"[GCPad1]", "Device = " + st.PipeDevice,
		"Buttons/A = `Button A`", "Buttons/Start = `Button START`",
		"Main Stick/Up = `Axis MAIN Y +`",
		"Triggers/L = `Axis L +`", "D-Pad/Up = `Button D_UP`",
	} {
		if !strings.Contains(string(gc), want) {
			t.Errorf("GCPadNew.ini missing %q", want)
		}
	}

	// Logger.ini: file logging so dolphin.log proves the booted title.
	li, err := os.ReadFile(filepath.Join(st.UserDir, "Config", "Logger.ini"))
	if err != nil || !strings.Contains(string(li), "WriteToFile = True") {
		t.Errorf("Logger.ini missing or lacks WriteToFile = True: %v", err)
	}

	// The FIFO (ADR-0043: the default launch is the proven single-pipe path —
	// the game pad's pipe1 only; the hotkey FIFO exists only when the launch
	// opts in, which TestLaunchHotkeysOptIn covers).
	fi, err := os.Lstat(filepath.Join(st.UserDir, "Pipes", "pipe1"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("Pipes/pipe1 is not a FIFO: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(st.UserDir, "Pipes", "hotkeys")); !os.IsNotExist(err) {
		t.Errorf("Pipes/hotkeys must not exist on the default path (err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(st.UserDir, "Config", "Hotkeys.ini")); !os.IsNotExist(err) {
		t.Errorf("Config/Hotkeys.ini must not exist on the default path (err %v)", err)
	}
	if strings.Contains(string(dini), "HotkeysRequireFocus") {
		t.Errorf("Dolphin.ini must not carry the hotkey focus gate on the default path:\n%s", dini)
	}
	if st.HotkeyDevice != "" {
		t.Errorf("the status records a hotkey device string on the default path: %q", st.HotkeyDevice)
	}

	// The image is recorded by hash + size, not by path.
	wantSHA := sha256.Sum256([]byte("fake user-supplied game image bytes"))
	if st.ImageSHA != hex.EncodeToString(wantSHA[:]) {
		t.Errorf("image_sha256 = %s, want %s", st.ImageSHA, hex.EncodeToString(wantSHA[:]))
	}
	if st.ImageSize != int64(len("fake user-supplied game image bytes")) {
		t.Errorf("image_size = %d", st.ImageSize)
	}

	// The emulator argv carries the proven flags (the image path itself is a
	// process argument, not a log line — the bridge masks it in logs).
	joined := strings.Join(env.dolphinArgs, " ")
	for _, want := range []string{
		"--user=" + st.UserDir, "--batch", "--video_backend=OGL",
		"--exec=" + env.imagePath,
		"--config=Dolphin.Input.BackgroundInput=True",
		"--config=GPU.Settings.InternalResolution=1",
		"--config=GPU.Hardware.VSync=False",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("dolphin argv missing %q (got %s)", want, joined)
		}
	}
	if st.DolphinPID != 9998 || st.XvfbPID != 9999 {
		t.Errorf("pids = %d/%d, want 9998/9999", st.DolphinPID, st.XvfbPID)
	}
	if !strings.HasPrefix(st.Display, ":") || st.Display != env.xvfbDisplay {
		t.Errorf("display %s != xvfb display %s", st.Display, env.xvfbDisplay)
	}
	if st.WindowID != "0x1234" || st.Region != (Region{X: 10, Y: 20, W: 640, H: 480}) {
		t.Errorf("window/region = %s %+v", st.WindowID, st.Region)
	}

	env.b.Stop()
}

// TestLaunchHotkeysOptIn: LaunchSpec.Hotkeys=true opts in to the savestate
// keyboard hotkeys (ADR-0043 am7, approach D) — the launch keeps the proven
// single-pipe layout (ONE FIFO, Pipes/pipe1, single-consumer; the hotkeys
// do NOT ride it), Hotkeys.ini is bound to the X11 KEYBOARD device
// (hotkeyKeyboardDevice) with the slot-1 key bindings, and Dolphin.ini
// gains the [General] HotkeysRequireFocus gate.
func TestLaunchHotkeysOptIn(t *testing.T) {
	env := newTestBridge(t, nil)
	mustLaunch(t, env, LaunchSpec{Hotkeys: true})
	st := env.b.State()

	fi, err := os.Lstat(filepath.Join(st.UserDir, "Pipes", "pipe1"))
	if err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("Pipes/pipe1 is not a FIFO: %v", err)
	}
	// The keyboard path has NO second FIFO: the status records the hotkey
	// device as the keyboard device string, distinct from the pipe device.
	if _, err := os.Lstat(filepath.Join(st.UserDir, "Pipes", "hotkeys")); !os.IsNotExist(err) {
		t.Errorf("Pipes/hotkeys must not exist on the keyboard path (err %v)", err)
	}
	if st.HotkeyDevice == "" {
		t.Fatalf("the status records no hotkey device string: %q", st.HotkeyDevice)
	}
	if st.HotkeyDevice != hotkeyKeyboardDevice {
		t.Errorf("hotkey device = %q, want the keyboard device %q", st.HotkeyDevice, hotkeyKeyboardDevice)
	}
	if st.HotkeyDevice == st.PipeDevice {
		t.Errorf("the hotkey device %q must not ride the game pad's pipe (single-consumer FIFO)", st.HotkeyDevice)
	}
	hk, err := os.ReadFile(filepath.Join(st.UserDir, "Config", "Hotkeys.ini"))
	if err != nil {
		t.Fatalf("read Hotkeys.ini: %v", err)
	}
	if !strings.Contains(string(hk), "Device = "+st.HotkeyDevice+"\n") {
		t.Errorf("Hotkeys.ini missing the keyboard device line %q:\n%s", st.HotkeyDevice, hk)
	}
	if !strings.Contains(string(hk), "Save State/Save State Slot 1 = `"+saveHotkeyKey+"`\n") ||
		!strings.Contains(string(hk), "Load State/Load State Slot 1 = `"+loadHotkeyKey+"`\n") {
		t.Errorf("Hotkeys.ini missing the slot-1 key bindings:\n%s", hk)
	}
	dini, err := os.ReadFile(filepath.Join(st.UserDir, "Config", "Dolphin.ini"))
	if err != nil {
		t.Fatalf("read Dolphin.ini: %v", err)
	}
	if !strings.Contains(string(dini), "[General]\nHotkeysRequireFocus = False\n") {
		t.Errorf("Dolphin.ini (hotkeys on) missing the focus gate:\n%s", dini)
	}

	env.b.Stop()
}

func TestLaunchRejectsRealUserDir(t *testing.T) {
	env := newTestBridge(t, nil)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	for _, bad := range []string{
		filepath.Join(home, ".local", "share", "dolphin-emu"),
		filepath.Join(home, ".local", "share", "dolphin-emu", "sub"),
		filepath.Join(home, ".config", "dolphin-emu"),
	} {
		err := env.b.Launch(context.Background(), LaunchSpec{UserDir: bad})
		if !isErr(err, ErrUserDirNotIsolated) {
			t.Errorf("Launch(userDir=%s) = %v, want ErrUserDirNotIsolated", bad, err)
		}
	}
	if atomic.LoadInt32(&env.stoppedDolphin) != 0 {
		t.Error("a rejected Launch must not have started the emulator")
	}
}

func isErr(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}

// pipeReader drains the FIFO read end (the role Dolphin plays) and records
// every command line, so the test asserts the exact protocol bytes.
func pipeReader(t *testing.T, path string) <-chan string {
	t.Helper()
	lines := make(chan string, 64)
	go func() {
		f, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			t.Logf("pipe reader: %v", err)
			return
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			lines <- sc.Text()
		}
	}()
	return lines
}

func TestFIFOProtocolBytes(t *testing.T) {
	env := newTestBridge(t, nil)
	mustLaunch(t, env, LaunchSpec{})
	st := env.b.State()
	lines := pipeReader(t, filepath.Join(st.UserDir, "Pipes", "pipe1"))

	// Drain until the persistent writer has connected.
	select {
	case <-time.After(2 * time.Second):
	}

	ctx := context.Background()
	// Buttons: order is the documented wire order; releases are reversed.
	if err := env.b.Inject(ctx, Buttons{A: true, Start: true}, Axes{}, 50*time.Millisecond); err != nil {
		t.Fatalf("Inject(buttons): %v", err)
	}
	got := []string{drain(lines), drain(lines), drain(lines), drain(lines)}
	want := []string{"PRESS A", "PRESS START", "RELEASE START", "RELEASE A"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("button protocol = %v, want %v", got, want)
		}
	}

	// Axes: sticks map -1..1 to the protocol's 0..1 (0.5 = centred);
	// triggers are already 0..1; the hold ends with a full reset.
	if err := env.b.Inject(ctx, Buttons{}, Axes{MainX: 1, L: 1}, 50*time.Millisecond); err != nil {
		t.Fatalf("Inject(axes): %v", err)
	}
	got = []string{drain(lines), drain(lines), drain(lines), drain(lines), drain(lines), drain(lines)}
	want = []string{"SET MAIN 1 0.5", "SET L 1", "SET MAIN 0.5 0.5", "SET C 0.5 0.5", "SET L 0", "SET R 0"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("axis protocol = %v, want %v", got, want)
		}
	}

	// A no-op Inject is a no-op on the wire.
	if err := env.b.Inject(ctx, Buttons{}, Axes{}, 0); err != nil {
		t.Fatalf("Inject(nop): %v", err)
	}

	// Stop closes the pipe; a further Inject reports not running.
	env.b.Stop()
	if err := env.b.Inject(ctx, Buttons{A: true}, Axes{}, 0); err == nil {
		t.Fatal("Inject after Stop must fail")
	}
	if _, err := os.Lstat(filepath.Join(st.UserDir, "Pipes", "pipe1")); !os.IsNotExist(err) {
		t.Errorf("FIFO still present after Stop: %v", err)
	}
}

func drain(ch <-chan string) string {
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		return "<timeout>"
	}
}

func TestStopIdempotentAndReLaunch(t *testing.T) {
	env := newTestBridge(t, nil)
	mustLaunch(t, env, LaunchSpec{})
	st := env.b.State()
	pipePath := filepath.Join(st.UserDir, "Pipes", "pipe1")

	env.b.Stop()
	if d, x := atomic.LoadInt32(&env.stoppedDolphin), atomic.LoadInt32(&env.stoppedXvfb); d != 1 || x != 1 {
		t.Fatalf("stop counts after first Stop = dolphin %d / xvfb %d, want 1/1", d, x)
	}
	if _, err := os.Lstat(pipePath); !os.IsNotExist(err) {
		t.Errorf("FIFO left behind: %v", err)
	}
	// Second Stop: no second kill, same state.
	env.b.Stop()
	if d, x := atomic.LoadInt32(&env.stoppedDolphin), atomic.LoadInt32(&env.stoppedXvfb); d != 1 || x != 1 {
		t.Fatalf("stop counts after second Stop = %d/%d, want still 1/1", d, x)
	}
	if env.b.State().State != StateStopped {
		t.Fatalf("state = %s, want stopped", env.b.State().State)
	}
	// A double-Launch on a running bridge is refused.
	mustLaunch(t, env, LaunchSpec{})
	if err := env.b.Launch(context.Background(), LaunchSpec{}); !isErr(err, ErrAlreadyLaunched) {
		t.Fatalf("second Launch while running = %v, want ErrAlreadyLaunched", err)
	}
	env.b.Stop()
}

// TestStopOnLifetimeCancel is the unit-mode half of the "every child dies
// with its parent" contract (the real half, asserting on /proc, lives in
// real_test.go). Cancelling the context a run belongs to must run the full
// Stop on its own — both process groups signalled, state stopped — without
// the caller ever calling Stop. Both lifetime forms are covered: an
// explicit LaunchSpec.Lifetime, and the default (the launch ctx itself).
func TestStopOnLifetimeCancel(t *testing.T) {
	env := newTestBridge(t, nil)
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()

	if err := env.b.Launch(context.Background(), LaunchSpec{Lifetime: runCtx}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if st := env.b.State(); st.State != StateRunning {
		t.Fatalf("state = %s, want running (last error: %s)", st.State, st.LastError)
	}

	// Abrupt cancel of the run's lifetime: the watcher must tear down.
	runCancel()
	waitForState(t, env.b, StateStopped)
	if d, x := atomic.LoadInt32(&env.stoppedDolphin), atomic.LoadInt32(&env.stoppedXvfb); d != 1 || x != 1 {
		t.Fatalf("stop counts after lifetime cancel = dolphin %d / xvfb %d, want 1/1", d, x)
	}

	// The explicit Stop afterwards is a quiet no-op (the watcher retired by
	// the first teardown; nothing double-signalled).
	env.b.Stop()
	if d, x := atomic.LoadInt32(&env.stoppedDolphin), atomic.LoadInt32(&env.stoppedXvfb); d != 1 || x != 1 {
		t.Fatalf("stop counts after explicit Stop = %d/%d, want still 1/1", d, x)
	}

	// Default form: no LaunchSpec.Lifetime, so the launch ctx is the
	// lifetime. Cancelling it mid-run must stop the bridge too.
	launchCtx, launchCancel := context.WithCancel(context.Background())
	if err := env.b.Launch(launchCtx, LaunchSpec{}); err != nil {
		t.Fatalf("re-Launch: %v", err)
	}
	if st := env.b.State(); st.State != StateRunning {
		t.Fatalf("state = %s, want running (last error: %s)", st.State, st.LastError)
	}
	launchCancel()
	waitForState(t, env.b, StateStopped)
	if d, x := atomic.LoadInt32(&env.stoppedDolphin), atomic.LoadInt32(&env.stoppedXvfb); d != 2 || x != 2 {
		t.Fatalf("stop counts after launch-ctx cancel = %d/%d, want 2/2", d, x)
	}
}

// waitForState polls the bridge state until it reaches want or 2 s pass.
func waitForState(t *testing.T, b Bridge, want State) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.State().State == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("state = %s after cancel, want %s (last error: %s)", b.State().State, want, b.State().LastError)
}

func TestWaitForGameFramePredicate(t *testing.T) {
	// Grab sequence: #1 the pre-Dolphin baseline (black), then a rendered
	// noisy frame that CHANGES with every probe — the predicate must fire on
	// the second probe (the first sample has no predecessor to differ from
	// and is never accepted, spike-faithful).
	env := newTestBridge(t, func(call int, r Region) image.Image {
		if call == 1 {
			return solidImage(r.W, r.H, 0)
		}
		return noiseImage(r.W, r.H, 100+call)
	}, func(o *Options) {
		o.ProveInputSamples = 4
		o.ProveInputCadence = 100 * time.Millisecond
	})
	mustLaunch(t, env, LaunchSpec{})
	ev, err := env.b.WaitForGameFrame(context.Background(), 5*time.Second)
	if err != nil {
		t.Fatalf("WaitForGameFrame: %v", err)
	}
	if len(ev.Hashes) != 2 || len(ev.Distances) != 2 {
		t.Fatalf("evidence = %+v, want 2 hashes and 2 distances", ev)
	}
	if ev.Distances[0] < vsBaselineMin {
		t.Errorf("vs_baseline %.4f < %.3f", ev.Distances[0], vsBaselineMin)
	}
	if ev.Distances[1] <= vsPrevMin {
		t.Errorf("vs_prev %.6f <= %.3f: the accepted frame must differ from its predecessor", ev.Distances[1], vsPrevMin)
	}
	if env.b.State().FirstFrame.IsZero() {
		t.Error("FirstFrame not recorded")
	}
	env.b.Stop()
}

// TestWaitForGameFrameRejectsIdenticalSuccessives pins the t=6 s false
// positive: a sequence where two successive frames are identical
// (vs_prev = 0) must never be accepted as a game frame, even though it
// differs from the baseline and has rendered-scene entropy. The readiness
// conjunction demands motion — the emulated GPU producing frames, not a
// frozen window.
func TestWaitForGameFrameRejectsIdenticalSuccessives(t *testing.T) {
	env := newTestBridge(t, func(call int, r Region) image.Image {
		if call == 1 {
			return solidImage(r.W, r.H, 0) // the bare-GUI baseline
		}
		return noiseImage(r.W, r.H, 7) // a rendered scene… the same one, forever
	}, func(o *Options) {
		o.ProveInputSamples = 4
		o.ProveInputCadence = 100 * time.Millisecond
	})
	mustLaunch(t, env, LaunchSpec{})
	_, err := env.b.WaitForGameFrame(context.Background(), 2*time.Second)
	if err == nil {
		t.Fatal("an identical-successives (vs_prev=0) sequence must not be accepted as a game frame")
	}
	if !strings.Contains(err.Error(), "no game frame within") || !strings.Contains(err.Error(), "vs_prev=") {
		t.Fatalf("err = %v, want the no-game-frame timeout carrying the measured vs_prev", err)
	}
	env.b.Stop()
}

func TestWaitForGameFrameTimesOutOnBlack(t *testing.T) {
	// Everything stays black: the predicate must never pass, and the error
	// carries the measured numbers (evidence, not a bare "timed out").
	env := newTestBridge(t, nil) // default grab: all black
	mustLaunch(t, env, LaunchSpec{})
	_, err := env.b.WaitForGameFrame(context.Background(), 2*time.Second)
	if err == nil || !strings.Contains(err.Error(), "no game frame within") {
		t.Fatalf("err = %v, want the no-game-frame timeout with measured numbers", err)
	}
	for _, want := range []string{"entropy=", "vs_baseline=", "vs_prev="} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error missing %q: %v", want, err)
		}
	}
	env.b.Stop()
}

func TestWaitForGameFrameNotLaunched(t *testing.T) {
	env := newTestBridge(t, nil)
	if _, err := env.b.WaitForGameFrame(context.Background(), time.Second); !isErr(err, ErrNotLaunched) {
		t.Fatalf("err = %v, want ErrNotLaunched", err)
	}
	if _, err := env.b.ProveInput(context.Background()); !isErr(err, ErrNotRunning) {
		t.Fatalf("ProveInput err = %v, want ErrNotRunning", err)
	}
	if _, err := env.b.Screenshot(context.Background()); !isErr(err, ErrNotRunning) {
		t.Fatalf("Screenshot err = %v, want ErrNotRunning", err)
	}
	if _, err := env.b.FrameSource(10); !isErr(err, ErrNotRunning) {
		t.Fatalf("FrameSource err = %v, want ErrNotRunning", err)
	}
}

func TestFrameSourceUsesWindowRegion(t *testing.T) {
	env := newTestBridge(t, nil)
	mustLaunch(t, env, LaunchSpec{})
	defer env.b.Stop()

	src, err := env.b.FrameSource(12)
	if err != nil {
		t.Fatalf("FrameSource: %v", err)
	}
	// The source's ID is the x11grab identity: display + window offset.
	// (The unit test cannot Start it — that needs the live display the fake
	// Xvfb never opens — so the ID is the assertion; the real test in
	// real_test.go starts the pipeline for real.)
	if want := "x11:" + env.dolphinDisplay + "+10,20"; src.ID() != want {
		t.Errorf("FrameSource ID = %q, want %q", src.ID(), want)
	}
}

func TestSaveSnapshot(t *testing.T) {
	env := newTestBridge(t, nil)
	mustLaunch(t, env, LaunchSpec{})
	defer env.b.Stop()

	if _, err := env.b.SaveSnapshot("bad label!"); !isErr(err, ErrSnapshotLabel) {
		t.Fatalf("bad label: %v, want ErrSnapshotLabel", err)
	}
	path, err := env.b.SaveSnapshot("ok-label")
	if err != nil {
		t.Fatalf("SaveSnapshot: %v", err)
	}
	if !strings.HasPrefix(path, filepath.Join(env.b.State().UserDir, "snapshots")) {
		t.Errorf("snapshot %s is not under the user dir's snapshots", path)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open snapshot: %v", err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("snapshot is not a decodable PNG: %v", err)
	}
	if w, h := img.Bounds().Dx(), img.Bounds().Dy(); w != 640 || h != 480 {
		t.Errorf("snapshot is %dx%d, want the 640x480 window crop", w, h)
	}
}

func TestProveInputSceneChange(t *testing.T) {
	// The scripted frames: the title until the pre-check probe, then the
	// letterboxed scene (seed 8) — exactly what a START press produces on
	// this game's title. The title ORBITS: every sample is a different
	// framing (seed = call+7, RMSE ≈ 0.16 between any two samples — the real
	// title camera moves 0.10-0.40 per 1 s sample, never settles, and never
	// repeats a framing inside the window, which the no-input control guard
	// verifies). Call layout with the tuned windows (4 samples at 100 ms):
	// #1 baseline, #2-4 seek, #5-8 control, #9 pre-check, #10-13 the
	// post-press window.
	const pressAt = 10
	env := newTestBridge(t, func(call int, r Region) image.Image {
		if call == 1 {
			return solidImage(r.W, r.H, 0) // baseline
		}
		if call < pressAt {
			return brightNoiseImage(r.W, r.H, call+7) // the orbiting title
		}
		return letterboxedImage(r.W, r.H, 8) // after the press
	}, func(o *Options) {
		o.ProveInputSamples = 4
		o.ProveInputCadence = 100 * time.Millisecond
	})
	mustLaunch(t, env, LaunchSpec{})
	defer env.b.Stop()

	// The reader plays Dolphin's role: it holds the FIFO read end open, which
	// is what lets the persistent writer connect and the Inject writes go out.
	lines := pipeReader(t, filepath.Join(env.b.State().UserDir, "Pipes", "pipe1"))
	_ = lines

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ev, err := env.b.ProveInput(ctx)
	if err != nil {
		t.Fatalf("ProveInput: %v", err)
	}
	if len(ev.Distances) != 2 {
		t.Fatalf("evidence distances = %v, want 2 (control dispersion, input max)", ev.Distances)
	}
	if ev.Distances[1] <= ev.Distances[0] {
		t.Errorf("input max distance %.4f must exceed the control dispersion %.4f", ev.Distances[1], ev.Distances[0])
	}
	for _, label := range []string{"before", "control", "after"} {
		p, ok := ev.Files[label]
		if !ok {
			t.Errorf("evidence missing the %q PNG", label)
			continue
		}
		if fi, err := os.Stat(p); err != nil || !strings.HasPrefix(p, env.b.State().UserDir) {
			t.Errorf("evidence %q is not on disk under the user dir: %v", label, err)
			_ = fi
		}
	}
}

func TestProveInputSettledScene(t *testing.T) {
	// The settled branch, the shape the REAL save-file prompt produces:
	// after the press the game shows a STABLE full-frame scene (the prompt
	// over the same sky) that sits close in RMSE to the title framings —
	// ~0.16 here, below the scene threshold (~0.28), so the letterbox
	// far-branch cannot see it. Acceptance must come from the scene being
	// SETTLED: two consecutive samples stable (step < 0.10) while the
	// orbiting title never settles (its steps are 0.10-0.40), verified on
	// the no-input control window by hasSettledStretch.
	const pressAt = 10
	env := newTestBridge(t, func(call int, r Region) image.Image {
		if call == 1 {
			return solidImage(r.W, r.H, 0) // baseline
		}
		if call < pressAt {
			return brightNoiseImage(r.W, r.H, call+7) // the orbiting title
		}
		// The new scene: a STABLE full-frame frame (same seed every sample),
		// a different framing from any title sample — close in RMSE (≈ 0.16)
		// yet stable.
		return brightNoiseImage(r.W, r.H, 20)
	}, func(o *Options) {
		o.ProveInputSamples = 4
		o.ProveInputCadence = 100 * time.Millisecond
	})
	mustLaunch(t, env, LaunchSpec{})
	defer env.b.Stop()
	_ = pipeReader(t, filepath.Join(env.b.State().UserDir, "Pipes", "pipe1"))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ev, err := env.b.ProveInput(ctx)
	if err != nil {
		t.Fatalf("ProveInput: %v", err)
	}
	if len(ev.Distances) != 2 {
		t.Fatalf("evidence distances = %v, want 2 (control dispersion, input max)", ev.Distances)
	}
	// The settled scene is CLOSE to the title (below the threshold) — that
	// is what this test exists to accept. So the input max need NOT exceed
	// the control dispersion; the crossing came from the settle, not the
	// distance.
	if ev.Distances[1] > ev.Distances[0]+0.10 {
		t.Errorf("the fake settled scene is supposed to sit close to the title: input max %.4f, control dispersion %.4f", ev.Distances[1], ev.Distances[0])
	}
	if _, ok := ev.Files["after"]; !ok {
		t.Errorf("evidence missing the after PNG")
	}
}

func TestProveInputNoSceneChange(t *testing.T) {
	// The screen never leaves the (orbiting) title: the press is "sent" but
	// the scene predicate must refuse to call that acceptance. The orbiting
	// title (a fresh framing per sample) also keeps the no-input control
	// window from settling, so the rejection must come from the input window
	// showing no crossing — not from the control guard.
	env := newTestBridge(t, func(call int, r Region) image.Image {
		if call == 1 {
			return solidImage(r.W, r.H, 0)
		}
		return brightNoiseImage(r.W, r.H, call+7) // the orbiting title, forever
	}, func(o *Options) {
		o.ProveInputSamples = 4
		o.ProveInputCadence = 100 * time.Millisecond
	})
	mustLaunch(t, env, LaunchSpec{})
	defer env.b.Stop()
	_ = pipeReader(t, filepath.Join(env.b.State().UserDir, "Pipes", "pipe1"))

	_, err := env.b.ProveInput(context.Background())
	if err == nil || !strings.Contains(err.Error(), "did not change the scene") {
		t.Fatalf("err = %v, want the not-proven-accepted error", err)
	}
}

func TestDevOptIn(t *testing.T) {
	lookup := func(v map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { x, ok := v[k]; return x, ok }
	}
	cases := []struct {
		env  map[string]string
		want bool
	}{
		{nil, false},
		{map[string]string{EnvDevDolphin: "1"}, true},
		{map[string]string{EnvDevDolphin: "true"}, true},
		{map[string]string{EnvDevDolphin: "0"}, false},
		{map[string]string{EnvDevDolphin: ""}, false},
		{map[string]string{EnvDevDolphin: "yes"}, false},
	}
	for _, c := range cases {
		if got := DevOptIn(lookup(c.env)); got != c.want {
			t.Errorf("DevOptIn(%v) = %v, want %v", c.env, got, c.want)
		}
	}
}
