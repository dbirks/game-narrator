package dolphin

import (
	"context"
	"crypto/sha256"
	"fmt"
	"image"
	"image/png"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/ffmpegpipe"
)

// defaultHold is the Inject hold when the caller passes none: the spike's
// measured 0.5 s spans several llvmpipe frames and was sufficient for START.
const defaultHold = 500 * time.Millisecond

// defaultBackend is the proven one (ADR-0007): OGL/llvmpipe under Xvfb.
const defaultBackend = "OGL"

var snapshotLabelRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// Bridge is the dev-only Dolphin automation bridge (doc.go). One bridge owns
// at most one emulator run at a time; Stop releases everything so the same
// bridge can Launch again with a fresh isolated dir.
//
// Dev-only by construction (docs/spec/01 invariant 7): the only callers that
// exist are the developer API routes (gated on GAME_NARRATOR_DEV_DOLPHIN=1
// plus the developer role) and the tests. Nothing in the production path
// imports this package's methods.
type bridge struct {
	opts Options
	log  *slog.Logger

	mu           sync.Mutex
	st           Status
	launched     bool // a Launch attempt has finished (running or failed)
	spec         LaunchSpec
	pipe         *pipeFIFO // the game pad's controller FIFO — single consumer (approach D: the savestate hotkeys ride the keyboard device, not this pipe)
	stopDolphin  func() error
	stopXvfb     func() error
	xvfbOwned    bool
	stopWatcher  chan struct{} // closed by Stop so the lifetime watcher exits
	emuLogF      *os.File
	baseline     image.Image // pre-Dolphin screen grab (the "bare" reference)
	baselineHash string
}

// Launch implements Bridge.Launch. It is the whole isolation contract in one
// place: fresh user dir, private display, proven config, FIFO, supervised
// emulator process, render-window resolution, and the pre-Dolphin baseline
// the game-frame predicate compares against.
//
// The image path is never logged: only its SHA-256 and size are (ADR-0007).
func (b *bridge) Launch(ctx context.Context, spec LaunchSpec) error {
	b.mu.Lock()
	if b.st.State == StateLaunching || b.st.State == StateRunning {
		b.mu.Unlock()
		return ErrAlreadyLaunched
	}
	if spec.Backend == "" {
		spec.Backend = defaultBackend
	}
	b.spec = spec
	b.launched = true // Stop knows it has real work to do, either outcome
	b.setStateLocked(StateLaunching, "")
	b.mu.Unlock()

	if err := b.launch(ctx, spec); err != nil {
		b.mu.Lock()
		b.setStateLocked(StateFailed, err.Error())
		b.mu.Unlock()
		return err
	}
	b.mu.Lock()
	b.setStateLocked(StateRunning, "")
	b.mu.Unlock()
	b.watchLifetime(ctx, spec)
	return nil
}

// watchLifetime starts the goroutine that implements "every child dies with
// its parent" at the lifecycle level: when the run's lifetime context
// (spec.Lifetime, or the launch ctx when the caller named none) is
// cancelled, the bridge runs its full Stop — process groups signalled, FIFO
// closed, state cleared. The stop channel lets Stop tell the watcher to
// exit quietly once the caller has torn things down, so at most one watcher
// is live per run and none survives a Stop on a never-cancelled lifetime.
// (OS-level abrupt parent death is covered separately by Pdeathsig on every
// spawned child, which needs no Go-side cooperation at all.)
func (b *bridge) watchLifetime(launchCtx context.Context, spec LaunchSpec) {
	lifetime := spec.Lifetime
	if lifetime == nil {
		lifetime = launchCtx
	}
	b.mu.Lock()
	watcher := make(chan struct{})
	b.stopWatcher = watcher
	b.mu.Unlock()
	go func() {
		select {
		case <-lifetime.Done():
			b.log.Info("dolphin: lifetime context cancelled; stopping the bridge")
			b.Stop()
		case <-watcher:
		}
	}()
}

// launch is the body of Launch with the error path funnelled through
// teardown, so a half-launched bridge never leaves a process, a FIFO or an
// Xvfb behind (the "Stop leaves no dolphin-emu/Xvfb" rule applies to
// failures too — a leaked Xvfb is a stolen display number).
func (b *bridge) launch(ctx context.Context, spec LaunchSpec) (err error) {
	defer func() {
		if err != nil {
			b.teardown()
		}
	}()

	// 1. The image: env only, referenced read-only, never logged.
	imagePath, err := b.resolveImage(spec.ImagePath)
	if err != nil {
		return err
	}
	sha, size, err := hashFile(imagePath)
	if err != nil {
		return fmt.Errorf("dolphin: game image unreadable: %w", err)
	}

	// 2. The isolated user dir (never the real Dolphin dirs).
	userDir, err := isolateUserDir(b.opts.DataDir, spec.UserDir)
	if err != nil {
		return err
	}

	// 3. The config the spike proved: BackgroundInput mandatory, the Pipes
	//    pad bound to the MEASURED device string (raw readdir order), file
	//    logging on, and — only when the launch opts in (ADR-0043) — the
	//    hotkey FIFO's config. The measured indices are recorded here (state
	//    + log) because they are the one fact the exit-rewrite comparison at
	//    Stop re-verifies: if Dolphin's enumeration ever differs from this
	//    measurement, that is the mismatch to find first (START ignored =
	//    wrong device index).
	gcpadDev, hotkeyDev, werr := writeUserConfig(userDir, spec.Backend, spec.Hotkeys)
	if werr != nil {
		return werr
	}
	b.log.Info("dolphin: Pipes device indices resolved from the raw directory order",
		"gcpad_device", gcpadDev, "hotkey_device", hotkeyDev)

	// 4. The private display. A caller-supplied Display is the caller's
	//    problem to own; the default is a Xvfb this bridge started and stops.
	display := spec.Display
	if display == "" {
		display, err = freeDisplay()
		if err != nil {
			return err
		}
		xvfbPID, stopXvfb, err := b.opts.StartXvfb(display, xvfbDefaultGeom, io.Discard)
		if err != nil {
			return err
		}
		b.mu.Lock()
		b.stopXvfb = stopXvfb
		b.xvfbOwned = true
		b.st.XvfbPID = xvfbPID
		// Record the display as soon as it is OURS, not only when the
		// emulator starts (step 7 re-sets the same value): a launch failure
		// between here and step 7 must still leave Stop able to clean up
		// the Xvfb artifacts of the display it owns.
		b.st.Display = display
		b.mu.Unlock()
		if err := b.waitDisplay(ctx, display); err != nil {
			return err
		}
		b.log.Info("dolphin: Xvfb up", "display", display, "pid", xvfbPID)
	}

	// 5. The pre-Dolphin baseline: the "bare" screen the game-frame predicate
	//    must differ from. Taken before the emulator exists, at full screen
	//    (no window yet to crop).
	scrW, scrH, err := b.opts.ScreenSize(display)
	if err != nil {
		return fmt.Errorf("dolphin: root size on %s: %w", display, err)
	}
	baseline, err := b.opts.Grab(display, Region{X: 0, Y: 0, W: scrW, H: scrH})
	if err != nil {
		return fmt.Errorf("dolphin: baseline grab on %s: %w", display, err)
	}
	b.mu.Lock()
	b.baseline = baseline
	b.baselineHash = capture.HashFrame(baseline)
	b.mu.Unlock()

	// 6. The controller FIFO, with its persistent writer connecting in the
	//    background (Dolphin is the one that opens the read end). It is the
	//    ONLY FIFO and stays SINGLE-CONSUMER on every path: the savestate
	//    hotkeys ride the keyboard device (Hotkeys.ini + XTest keys, approach
	//    D, ADR-0043 am7), never the pipe.
	pipe, err := openPipe(userDir)
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.pipe = pipe
	b.mu.Unlock()
	pipe.connect(ctx)

	// 7. The emulator itself.
	logPath := filepath.Join(userDir, "bridge-emu.log")
	logF, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("dolphin: create emulator log: %w", err)
	}
	args := b.dolphinArgs(userDir, imagePath, spec)
	pid, stopDolphin, err := b.opts.StartDolphin(display, args, logF)
	if err != nil {
		logF.Close()
		return err
	}
	b.mu.Lock()
	b.stopDolphin = stopDolphin
	b.emuLogF = logF
	b.st.DolphinPID = pid
	b.st.Display = display
	b.st.UserDir = userDir
	b.st.ImageSHA = sha
	b.st.ImageSize = size
	b.st.PipeDevice = gcpadDev
	b.st.HotkeyDevice = hotkeyDev
	b.st.MemcardPath = memcardAPath(userDir)
	b.mu.Unlock()
	// The image path is masked in every logged form of the argv (task rule:
	// never logged; the hash is the provenance).
	b.log.Info("dolphin: launching emulator",
		"display", display, "pid", pid,
		"image_sha256", sha, "image_size", size,
		"args", maskExecArg(args, sha))

	// 8. The render window, re-resolvable: batch mode re-parents the
	//    framebuffer window when the game takes over (ADR-0007), so the id
	//    and geometry are resolved, not assumed, and re-resolved on demand.
	winID, err := b.waitForWindow(ctx, display, pid)
	if err != nil {
		return err
	}
	region, err := b.waitForGeometry(ctx, display, winID)
	if err != nil {
		return err
	}
	if spec.Width > 0 && spec.Height > 0 && (spec.Width != region.W || spec.Height != region.H) {
		b.log.Warn("dolphin: render window differs from the expected size; the resolved geometry wins",
			"expected", fmt.Sprintf("%dx%d", spec.Width, spec.Height),
			"resolved", fmt.Sprintf("%dx%d", region.W, region.H))
	}
	b.mu.Lock()
	b.st.WindowID = winID
	b.st.Region = region
	b.mu.Unlock()
	b.log.Info("dolphin: render window resolved", "window", winID, "region", fmt.Sprintf("%+v", region))
	return nil
}

// resolveImage admits the path only from the environment (or from the
// caller's already-env-derived LaunchSpec.ImagePath): a request-supplied
// path would let the API point the emulator at arbitrary bytes, and the
// spike's rule is the image is user-owned and referenced read-only.
func (b *bridge) resolveImage(specPath string) (string, error) {
	envPath := os.Getenv(b.opts.ImageEnv)
	path := envPath
	if path == "" && specPath != "" {
		path = specPath
	}
	if path == "" {
		return "", fmt.Errorf("%w: %s is not set", ErrNoImage, b.opts.ImageEnv)
	}
	fi, err := os.Stat(path)
	if err != nil || fi.Size() == 0 {
		return "", fmt.Errorf("%w: %s does not name a readable game image", ErrNoImage, b.opts.ImageEnv)
	}
	return path, nil
}

// dolphinArgs is the proven command line (ADR-0007), with the ini-backed
// BackgroundInput repeated on the CLI because the emulator reads both.
func (b *bridge) dolphinArgs(userDir, imagePath string, spec LaunchSpec) []string {
	args := []string{
		"--user=" + userDir,
		"--batch",
		"--exec=" + imagePath,
		"--video_backend=" + spec.Backend,
		"--config=Dolphin.Input.BackgroundInput=True",
		"--config=GPU.Settings.InternalResolution=1",
		"--config=GPU.Hardware.VSync=False",
	}
	if spec.SaveState != "" {
		args = append(args, "--save_state="+spec.SaveState)
	}
	if spec.Movie != "" {
		args = append(args, "--movie="+spec.Movie)
	}
	return args
}

// maskExecArg returns the argv for logging with the game image path replaced
// by its hash prefix — the path itself is never written anywhere.
func maskExecArg(args []string, imageSHA string) string {
	prefix := imageSHA
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	for i, a := range args {
		if strings.HasPrefix(a, "--exec=") {
			args[i] = "--exec=<image sha256:" + prefix + ">"
		}
	}
	return strings.Join(args, " ")
}

// waitDisplay polls the socket like the spike's `test -S`, bounded by ctx.
func (b *bridge) waitDisplay(ctx context.Context, display string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b.opts.DisplayUp(display) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	return fmt.Errorf("dolphin: Xvfb %s never came up (socket missing after 10s)", display)
}

// waitForWindow polls FindWindow on a 1 s cadence. A dead emulator is
// reported as such rather than waited out: the window will never arrive.
func (b *bridge) waitForWindow(ctx context.Context, display string, dolphinPID int) (string, error) {
	deadline := time.Now().Add(b.opts.LaunchTimeout)
	for time.Now().Before(deadline) {
		if id, err := b.opts.FindWindow(display); err == nil {
			return id, nil
		}
		if dolphinPID > 0 && !alive(dolphinPID) {
			return "", fmt.Errorf("dolphin: the emulator exited before opening a window (see the bridge-emu.log in the user dir)")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return "", fmt.Errorf("dolphin: no render window appeared on %s within %s", display, b.opts.LaunchTimeout)
}

// waitForGeometry resolves the window's absolute rect, retrying while the
// window exists but is not yet placed.
func (b *bridge) waitForGeometry(ctx context.Context, display, winID string) (Region, error) {
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := b.opts.WindowGeometry(display, winID)
		if err == nil && r.W > 0 && r.H > 0 {
			return r, nil
		}
		if time.Now().After(deadline) {
			return Region{}, fmt.Errorf("dolphin: no geometry for window %s on %s: %v", winID, display, err)
		}
		select {
		case <-ctx.Done():
			return Region{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// alive reports whether a pid is live (unix signal-0 probe; portable stub
// answers false off-unix, which only disables the early-exit shortcut).
func alive(pid int) bool { return procAlive(pid) }

// hashFile streams the SHA-256 (4 MiB chunks) and size of a file. The image
// can be >1 GiB; never load it whole.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	n, err := io.CopyBuffer(h, f, buf)
	if err != nil {
		return "", 0, err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), n, nil
}

// Inject implements Bridge.Inject: PRESS the buttons, SET the axes, hold,
// RELEASE, reset the axes. The wire bytes follow the upstream protocol
// exactly (pipe.go); the hold is a wall-clock sleep because the emulated
// time at llvmpipe speed is the variable, not the hold (ADR-0007: timing
// assertions belong to the emulated progress, not here).
func (b *bridge) Inject(ctx context.Context, buttons Buttons, axes Axes, hold time.Duration) error {
	b.mu.Lock()
	state := b.st.State
	pipe := b.pipe
	b.mu.Unlock()
	if state != StateRunning {
		return ErrNotRunning
	}
	if hold <= 0 {
		hold = defaultHold
	}
	pressed := pressedTokens(buttons)
	axSet := axisCommands(axes)
	if len(pressed) == 0 && len(axSet) == 0 {
		return nil // a no-op Inject still proves the state check ran
	}
	for _, tok := range pressed {
		if err := pipe.writeLine(ctx, "PRESS "+tok); err != nil {
			return err
		}
	}
	for _, cmd := range axSet {
		if err := pipe.writeLine(ctx, cmd); err != nil {
			return err
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(hold):
	}
	for i := len(pressed) - 1; i >= 0; i-- {
		if err := pipe.writeLine(ctx, "RELEASE "+pressed[i]); err != nil {
			return err
		}
	}
	if len(axSet) > 0 {
		for _, cmd := range axisResetCommands() {
			if err := pipe.writeLine(ctx, cmd); err != nil {
				return err
			}
		}
	}
	b.log.Debug("dolphin: injected input", "buttons", pressed, "axes", axSet, "hold", hold)
	return nil
}

// Screenshot implements Bridge.Screenshot: an X11 grab of the render window's
// current region (re-resolved, because the window can be re-parented). The
// image is fresh per call; the caller decides what to do with it (the API
// serves it and does not store it unless a dev trace asks for it).
func (b *bridge) Screenshot(ctx context.Context) (image.Image, error) {
	b.mu.Lock()
	state := b.st.State
	display := b.st.Display
	b.mu.Unlock()
	if state != StateRunning {
		return nil, ErrNotRunning
	}
	// ctx is advisory here: the grab carries its own bounded timeout and the
	// window re-resolution is a fast lookup; a cancelled request simply stops
	// the readiness loop, it cannot interrupt an in-flight grab.
	region, err := b.currentRegion(display)
	if err != nil {
		return nil, err
	}
	img, err := b.opts.Grab(display, region)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// currentRegion re-resolves the window and its geometry, falling back to the
// last resolved region when the lookup fails once (a slow xwininfo is not a
// lost window; a missing window is, and that is reported as not running).
func (b *bridge) currentRegion(display string) (Region, error) {
	b.mu.Lock()
	lastWin := b.st.WindowID
	lastRegion := b.st.Region
	b.mu.Unlock()

	if winID, err := b.opts.FindWindow(display); err == nil {
		if region, err := b.opts.WindowGeometry(display, winID); err == nil && region.W > 0 {
			if winID != lastWin {
				b.log.Info("dolphin: render window re-resolved", "was", lastWin, "now", winID, "region", fmt.Sprintf("%+v", region))
			}
			b.mu.Lock()
			b.st.WindowID = winID
			b.st.Region = region
			b.mu.Unlock()
			return region, nil
		}
	}
	if lastRegion.W > 0 {
		return lastRegion, nil
	}
	return Region{}, fmt.Errorf("dolphin: no render window on %s: %w", display, ErrNotRunning)
}

// FrameSource implements Bridge.FrameSource: the same supervised FFmpeg
// x11grab the production pipeline consumes (docs/spec/11 step 4), cropped to
// the render window's absolute rect. No fixture-specific branch: whatever
// the OCR/tracker sees here is exactly what the V4L2 path would see.
func (b *bridge) FrameSource(fps int) (capture.Source, error) {
	b.mu.Lock()
	state := b.st.State
	display := b.st.Display
	b.mu.Unlock()
	if state != StateRunning {
		return nil, ErrNotRunning
	}
	if fps <= 0 {
		fps = 10
	}
	region, err := b.currentRegion(display)
	if err != nil {
		return nil, err
	}
	src, err := ffmpegpipe.X11(display, region.W, region.H, float64(fps), region.X, region.Y)
	if err != nil {
		return nil, fmt.Errorf("dolphin: frame source: %w", err)
	}
	return src, nil
}

// SaveSnapshot implements Bridge.Snapshot: a labelled PNG of the current
// render window under <userDir>/snapshots. Savestate capture runs over the
// hotkey pipe instead (SaveState, ADR-0043, opt-in); the snapshot is the
// picture half of any savestate round-trip evidence.
func (b *bridge) SaveSnapshot(label string) (string, error) {
	if !snapshotLabelRE.MatchString(label) {
		return "", ErrSnapshotLabel
	}
	b.mu.Lock()
	state := b.st.State
	userDir := b.st.UserDir
	b.mu.Unlock()
	if state != StateRunning {
		return "", ErrNotRunning
	}
	img, err := b.Screenshot(context.Background())
	if err != nil {
		return "", err
	}
	dir := filepath.Join(userDir, "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%s.png", label, time.Now().UTC().Format("20060102T150405.000"))
	path := filepath.Join(dir, name)
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return "", fmt.Errorf("dolphin: encode snapshot: %w", err)
	}
	b.log.Info("dolphin: snapshot stored", "path", path, "label", label)
	return path, nil
}

// State implements Bridge.State.
func (b *bridge) State() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.st
}

// Stop implements Bridge.Stop: close the FIFO (Dolphin sees EOF on the pad
// and tears the device down), stop the emulator gracefully, stop the Xvfb the
// bridge owns, and clear the state so a re-Launch starts clean. Idempotent:
// a second Stop returns without touching anything.
func (b *bridge) Stop() {
	b.mu.Lock()
	if !b.launched && b.stopDolphin == nil && b.stopXvfb == nil && b.pipe == nil {
		b.mu.Unlock()
		return
	}
	launched := b.launched
	b.launched = false
	watcher := b.stopWatcher
	b.stopWatcher = nil
	pipe := b.pipe
	stopDolphin := b.stopDolphin
	stopXvfb := b.stopXvfb
	xvfbOwned := b.xvfbOwned
	logF := b.emuLogF
	display := b.st.Display
	userDir := b.st.UserDir
	gcpadDev := b.st.PipeDevice
	hotkeyDev := b.st.HotkeyDevice
	b.stopDolphin = nil
	b.stopXvfb = nil
	b.xvfbOwned = false
	b.emuLogF = nil
	b.pipe = nil
	b.baseline = nil
	b.baselineHash = ""
	b.mu.Unlock()

	if watcher != nil {
		close(watcher)
	}
	if pipe != nil {
		pipe.close()
	}
	if stopDolphin != nil {
		if err := stopDolphin(); err != nil {
			b.log.Warn("dolphin: stopping the emulator", "error", err)
		}
	}
	// A clean exit rewrites Config/GCPadNew.ini and Config/Hotkeys.ini with
	// the device strings the parser RESOLVED (an unresolvable device line is
	// dropped). If the rewritten line no longer matches the measured
	// string, the pre-boot index did not survive Dolphin's enumeration —
	// log it loudly: that is the wrong-device-index failure mode (START
	// the wrong-device-index failure mode (START silently ignored,
	// ADR-0043).
	if stopDolphin != nil && userDir != "" {
		b.checkExitRewrite(userDir, gcpadDev, hotkeyDev)
	}
	if xvfbOwned && stopXvfb != nil {
		if err := stopXvfb(); err != nil {
			b.log.Warn("dolphin: stopping Xvfb", "display", display, "error", err)
		} else if display != "" {
			// A clean exit removes the lock itself; the SIGKILL escalation
			// (and an abruptly killed run) leave it. Only run after a
			// SUCCESSFUL stop — a failed stop means the server may still be
			// up, and its lock stays (issue #7, our-own cleanup half).
			removeOwnedXvfbArtifacts(display)
		}
	}
	if logF != nil {
		_ = logF.Close()
	}
	b.mu.Lock()
	b.st.State = StateStopped
	b.st.DolphinPID = 0
	b.st.XvfbPID = 0
	b.st.WindowID = ""
	b.st.Region = Region{}
	b.st.PipeConn = false
	b.st.FirstFrame = time.Time{}
	b.mu.Unlock()
	if launched {
		b.log.Info("dolphin: stopped", "display", display)
	}
}

// teardown is the failure path of launch: same order as Stop, reached at
// most once per failed attempt.
func (b *bridge) teardown() {
	b.Stop()
}

// checkExitRewrite re-verifies, after Dolphin's clean exit, that the config
// files still carry the MEASURED device strings. It is the structural
// counterpart of the pre-boot measurement: two facts that should agree
// (my device strings, Dolphin's enumeration) are compared and a mismatch is
// an error-level log with the full rewritten file, not a silent drift.
// A missing file (the exit was not clean, or the section was never
// written) is a warn, not an error: there is nothing to compare yet.
func (b *bridge) checkExitRewrite(userDir, gcpadDev, hotkeyDev string) {
	if gcpadDev != "" {
		b.checkDeviceLine(filepath.Join(userDir, "Config", "GCPadNew.ini"), gcpadDev)
	}
	if hotkeyDev != "" {
		// The keyboard device string (approach D): an unresolvable Device
		// line is dropped silently by the parser, so its presence in the
		// rewrite is the "the hotkey device resolved" proof.
		b.checkDeviceLine(filepath.Join(userDir, "Config", "Hotkeys.ini"), hotkeyDev)
	}
}

// checkDeviceLine reads one rewritten config and compares its Device line
// against the measured string; the mismatch is logged at error level with
// the file content (the human-readable half of the evidence bundle).
func (b *bridge) checkDeviceLine(path, want string) {
	got, err := os.ReadFile(path)
	if err != nil {
		b.log.Warn("dolphin: exit rewrite unreadable (no clean exit?); device comparison skipped",
			"file", filepath.Base(path), "error", err)
		return
	}
	if !strings.Contains(string(got), "Device = "+want+"\n") {
		b.log.Error("dolphin: EXIT-REWRITE MISMATCH — the measured device string is gone or changed; the pre-boot Pipes index did not match Dolphin's enumeration (inputs may have been silently dropped)",
			"file", filepath.Base(path), "measured", want, "rewritten", string(got))
		return
	}
	b.log.Info("dolphin: exit rewrite matches the measured device string",
		"file", filepath.Base(path), "device", want)
}

// setStateLocked writes the state and (when non-empty) its failure text.
// Callers hold b.mu.
func (b *bridge) setStateLocked(state State, lastErr string) {
	b.st.State = state
	if lastErr != "" {
		b.st.LastError = lastErr
	}
}
