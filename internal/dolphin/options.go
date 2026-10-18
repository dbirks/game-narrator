package dolphin

import (
	"fmt"
	"image"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options wires the bridge into its host. Every long-running process and every
// external tool is a seam: production fills them with the real subprocess and
// X11/FFmpeg helpers, tests fill them with fakes so the config files, the FIFO
// protocol bytes, the isolation rules and the idempotent Stop can be asserted
// without an emulator or a display (docs/spec/12 "Unit" mode).
type Options struct {
	// DataDir is the root under which a fresh isolated Dolphin user dir is
	// created for every run (LaunchSpec.UserDir empty). It must never be the
	// real Dolphin dir; empty = os.TempDir()/game-narrator-dolphin.
	DataDir string
	// ImageEnv is the env var naming the game image; empty = EnvGameImage.
	ImageEnv string
	// Log receives launch/stop/evidence lines; nil = slog.Default().
	Log *slog.Logger

	// Binary names, resolved on PATH at launch (probed at runtime, never
	// assumed — ADR-0012 §3 applied to the emulator host tools).
	DolphinBin string // default "dolphin-emu"
	XvfbBin    string // default "Xvfb"
	FFmpegBin  string // default "ffmpeg"
	XdotoolBin string // default "xdotool" (approach D savestate hotkeys)

	// StartXvfb starts a virtual X server on display (":91") with screen
	// geometry (e.g. "1280x1024x24"), logging to logf. It returns the server
	// pid and a stop function that kills it and reaps it. Production is the
	// real Xvfb; a fake records the call.
	StartXvfb func(display, geometry string, logf io.Writer) (pid int, stop func() error, err error)
	// StartDolphin starts dolphin-emu with DISPLAY=display and the given args,
	// logging to logf. Same contract: pid + stop function (TERM, bounded wait,
	// KILL, reap).
	StartDolphin func(display string, args []string, logf io.Writer) (pid int, stop func() error, err error)

	// FindWindow returns the render window id (xwininfo hex id) on the display.
	FindWindow func(display string) (string, error)
	// WindowGeometry resolves a window's absolute root-window rectangle.
	WindowGeometry func(display, windowID string) (Region, error)
	// ScreenSize is the root window's pixel size (the grab fallback before a
	// window exists).
	ScreenSize func(display string) (w, h int, err error)
	// DisplayUp reports whether an X server answers on the display. Nil →
	// the real socket/lock check (displayIsUp).
	DisplayUp func(display string) bool
	// Grab captures one still image of region r on the display (X11 grab).
	// It returns a fresh image per call; the caller owns it.
	Grab func(display string, r Region) (image.Image, error)

	// SendKey injects one XTest key event (keydown, hold, keyup) into the
	// display via xdotool — the savestate hotkey injector (approach D,
	// ADR-0043 am7). Production is real xdotool on PATH; tests wire a fake
	// that records the call. Nil → realSendKey(XdotoolBin).
	SendKey func(display, key string, hold time.Duration) error

	// LaunchTimeout caps how long Launch waits for the render window and its
	// geometry; 0 = DefaultLaunchTimeout. Slow llvmpipe hosts (the UNO Q
	// class) need the raised budget the real test sets.
	LaunchTimeout time.Duration

	// ProveInputSamples / ProveInputCadence tune ProveInput's sample windows;
	// 0 = the spike's 15 samples at a true 1 s cadence. Tests shorten them so
	// the unit suite does not pay the llvmpipe budget; production keeps the
	// measured settings (the cadence rule, not the count, is the load-bearing
	// part — a window sampled faster than the emulated game moves proves less).
	ProveInputSamples int
	ProveInputCadence time.Duration
}

// DefaultLaunchTimeout caps the render-window wait inside Launch. The spike
// measured first window well inside this on llvmpipe (the game frame itself
// can take ~20 s, but that is WaitForGameFrame's budget, not the window's).
const DefaultLaunchTimeout = 60 * time.Second

// DefaultDataDir is where isolated user dirs live when Options.DataDir is
// empty: a per-user temp location, deliberately NOT the real Dolphin dirs.
func DefaultDataDir() string {
	return filepath.Join(os.TempDir(), "game-narrator-dolphin")
}

// New validates opts, fills production defaults for every nil seam and returns
// a bridge in the stopped state. It never starts anything and never touches
// the filesystem — Launch does the work. Callers hold the Bridge interface;
// the concrete type is unexported because nothing outside this package should
// need to.
func New(opts Options) (Bridge, error) {
	if opts.DataDir == "" {
		opts.DataDir = DefaultDataDir()
	}
	if opts.ImageEnv == "" {
		opts.ImageEnv = EnvGameImage
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.DolphinBin == "" {
		opts.DolphinBin = "dolphin-emu"
	}
	if opts.XvfbBin == "" {
		opts.XvfbBin = "Xvfb"
	}
	if opts.FFmpegBin == "" {
		opts.FFmpegBin = "ffmpeg"
	}
	if opts.XdotoolBin == "" {
		opts.XdotoolBin = "xdotool"
	}
	if opts.LaunchTimeout <= 0 {
		opts.LaunchTimeout = DefaultLaunchTimeout
	}
	if opts.StartXvfb == nil {
		opts.StartXvfb = realStartXvfb(opts.XvfbBin)
	}
	if opts.StartDolphin == nil {
		opts.StartDolphin = realStartDolphin(opts.DolphinBin)
	}
	if opts.SendKey == nil {
		opts.SendKey = realSendKey(opts.XdotoolBin)
	}
	if opts.FindWindow == nil {
		opts.FindWindow = realFindWindow
	}
	if opts.WindowGeometry == nil {
		opts.WindowGeometry = realWindowGeometry
	}
	if opts.ScreenSize == nil {
		opts.ScreenSize = realScreenSize
	}
	if opts.DisplayUp == nil {
		opts.DisplayUp = displayIsUp
	}
	if opts.Grab == nil {
		opts.Grab = realGrab(opts.FFmpegBin)
	}
	b := &bridge{opts: opts, log: opts.Log}
	return b, nil
}

// isolateUserDir resolves userDir (creating a fresh one under DataDir when
// empty) and refuses anything that is or contains the real Dolphin dirs.
// The real dirs are the one filesystem this package must never touch
// (ADR-0007 isolation rule; the task boundary repeats it).
func isolateUserDir(dataDir string, requested string) (string, error) {
	userDir := requested
	if userDir == "" {
		userDir = filepath.Join(dataDir, fmt.Sprintf("dolphin-%d", time.Now().UnixNano()))
	}
	abs, err := filepath.Abs(userDir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		resolved = abs // a fresh dir does not exist yet
	}
	for _, real := range realDolphinDirs() {
		rel, err := filepath.Rel(real, resolved)
		// rel starting with ".." (however nested) means resolved is outside
		// the real dir; anything else (".", "sub/...", "") means resolved is
		// the real dir or under it — refuse.
		if err == nil && !strings.HasPrefix(rel, "..") {
			return "", ErrUserDirNotIsolated
		}
	}
	if err := os.MkdirAll(filepath.Join(resolved, "Config"), 0o755); err != nil {
		return "", fmt.Errorf("dolphin: create isolated user dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(resolved, "Pipes"), 0o755); err != nil {
		return "", fmt.Errorf("dolphin: create isolated pipes dir: %w", err)
	}
	return resolved, nil
}

// realDolphinDirs are the paths a real Dolphin installation writes. The
// bridge's isolation promise is "never these" (ADR-0007).
func realDolphinDirs() []string {
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out,
			filepath.Join(home, ".local", "share", "dolphin-emu"),
			filepath.Join(home, ".config", "dolphin-emu"),
		)
	}
	return out
}

func hasDotDotPrefix(rel string) bool {
	return rel == ".." || len(rel) > 2 && rel[:2] == ".."+string(filepath.Separator)
}
