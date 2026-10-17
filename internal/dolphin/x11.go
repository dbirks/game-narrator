package dolphin

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The production FindWindow / WindowGeometry / ScreenSize / Grab seams.
// They shell out to the same host tools the spike proved (xdotool, xwininfo,
// ffmpeg x11grab) — ported logic, not a re-invention: the window search
// tolerates batch mode re-parenting the framebuffer window, the geometry is
// the window's absolute rect (never the root), and the grab is of that rect,
// because grabbing the whole root dilutes every difference (ADR-0007
// "Method notes and traps found").

const grabTimeout = 15 * time.Second

var (
	// xwininfo -root -tree line:
	//   0x02000003: id = 0x02000003 "Dolphin 2609 | ...": ("dolphin-emu" "Dolphin-Emu") at (+0,+0) ...
	treeWindowRE = regexp.MustCompile(`(?i)^\s*(0x[0-9a-f]+):\s+id\s*=\s*0x[0-9a-f]+\s+"([^"]*olphin[^"]*)":\s*\(\s*"dolphin-emu"`)
	// xwininfo geometry lines (window or root).
	absXRE   = regexp.MustCompile(`Absolute upper-left X:\s*(-?\d+)`)
	absYRE   = regexp.MustCompile(`Absolute upper-left Y:\s*(-?\d+)`)
	widthRE  = regexp.MustCompile(`(?m)^\s*Width:\s*(\d+)`)
	heightRE = regexp.MustCompile(`(?m)^\s*Height:\s*(\d+)`)
)

func runTool(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// realFindWindow locates the render window on the display, in the spike's
// precedence order: xdotool visible-by-class, xdotool by-class, then a direct
// parse of the window tree (which works without EWMH hints and, because it
// matches on the class "dolphin-emu", survives the window being re-titled
// with the game name).
func realFindWindow(display string) (string, error) {
	ctx := context.Background()
	if _, err := exec.LookPath("xdotool"); err == nil {
		for _, flags := range [][]string{
			{"xdotool", "search", "--onlyvisible", "--class", "dolphin-emu"},
			{"xdotool", "search", "--class", "dolphin-emu"},
		} {
			cmd := exec.CommandContext(ctx, flags[0], flags[1:]...)
			// Put DISPLAY first so the private display wins over any stale
			// DISPLAY inherited from the developer's shell.
			cmd.Env = displayEnv(display)
			out, err := cmd.Output()
			if err != nil {
				continue
			}
			if id := lastNonEmptyLine(string(out)); id != "" {
				return id, nil
			}
		}
	}
	out, err := runTool(ctx, "xwininfo", "-display", display, "-root", "-tree")
	if err != nil {
		return "", fmt.Errorf("dolphin: no render window found on %s: %w", display, err)
	}
	for _, line := range strings.Split(out, "\n") {
		if m := treeWindowRE.FindStringSubmatch(line); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("dolphin: no dolphin-emu window found on %s (xwininfo tree)", display)
}

// displayEnv puts DISPLAY first in the environment: os.Environ() may carry a
// stale DISPLAY from the developer's shell, and the X11 tools must target the
// private display the bridge owns, not whatever else the box happens to have.
func displayEnv(display string) []string {
	return append([]string{"DISPLAY=" + display}, os.Environ()...)
}

// realWindowGeometry resolves a window's absolute rect through xwininfo
// (one tool gives position and size, unlike the spike's xwininfo+xdotool pair).
func realWindowGeometry(display, windowID string) (Region, error) {
	out, err := runTool(context.Background(), "xwininfo", "-display", display, "-id", windowID)
	if err != nil {
		return Region{}, err
	}
	r, err := parseXWinInfo(out)
	if err != nil {
		return Region{}, fmt.Errorf("dolphin: window %s on %s: %w", windowID, display, err)
	}
	return r, nil
}

// realScreenSize is the root window's rect — the grab fallback before the
// render window exists (and the baseline capture at launch).
func realScreenSize(display string) (int, int, error) {
	out, err := runTool(context.Background(), "xwininfo", "-display", display, "-root")
	if err != nil {
		return 0, 0, err
	}
	w, h, ok := parseRootSize(out)
	if !ok {
		return 0, 0, fmt.Errorf("dolphin: could not parse root size from xwininfo on %s", display)
	}
	return w, h, nil
}

// parseRootSize reads the Width/Height lines of an xwininfo dump.
func parseRootSize(out string) (int, int, bool) {
	w := mustGroup(widthRE, out)
	h := mustGroup(heightRE, out)
	if w == "" || h == "" {
		return 0, 0, false
	}
	n1, err1 := atoi(w)
	n2, err2 := atoi(h)
	if err1 != nil || err2 != nil || n1 <= 0 || n2 <= 0 {
		return 0, 0, false
	}
	return n1, n2, true
}

func parseXWinInfo(out string) (Region, error) {
	x, err1 := atoi(mustGroup(absXRE, out))
	y, err2 := atoi(mustGroup(absYRE, out))
	w, err3 := atoi(mustGroup(widthRE, out))
	h, err4 := atoi(mustGroup(heightRE, out))
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil || w <= 0 || h <= 0 {
		return Region{}, fmt.Errorf("geometry not found in xwininfo output")
	}
	return Region{X: x, Y: y, W: w, H: h}, nil
}

func mustGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

// lastNonEmptyLine is the last non-empty line of s (xdotool prints one id per
// line; the spike took `tail -1`, i.e. the most recent match).
func lastNonEmptyLine(s string) string {
	lines := strings.Split(s, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(lines[i]); line != "" {
			return line
		}
	}
	return ""
}

func atoi(s string) (int, error) {
	return strconv.Atoi(s)
}

// realGrab captures one still of region r on the display. The primary tool
// is ImageMagick `import` — the spike's grab (a native X11 XGetImage, which
// measures ~80 ms on this host). The ffmpeg x11grab fallback measures ~6 s
// here; at that cost the ProveInput 1 s cadence degenerates to back-to-back
// grabs, the title→press span runs into the attract movie, and the crossing
// the press should cause gets misread as the movie's (the bug 4 failure).
// `import` grabs the root and crops the region from it (the spike's `shot`
// + `window_crop` shape); ffmpeg stays for hosts without ImageMagick.
func realGrab(bin string) func(display string, r Region) (image.Image, error) {
	useImport := false
	if _, lerr := exec.LookPath("import"); lerr == nil {
		useImport = true
	}
	return func(display string, r Region) (image.Image, error) {
		if r.W <= 0 || r.H <= 0 {
			return nil, fmt.Errorf("dolphin: grab region is empty")
		}
		tmp, err := os.CreateTemp("", "dolphin-grab-*.png")
		if err != nil {
			return nil, err
		}
		defer os.Remove(tmp.Name())
		tmp.Close()

		ctx, cancel := context.WithTimeout(context.Background(), grabTimeout)
		defer cancel()
		var cmd *exec.Cmd
		if useImport {
			cmd = exec.CommandContext(ctx, "import",
				"-display", display, "-window", "root",
				"-crop", fmt.Sprintf("%dx%d+%d+%d", r.W, r.H, r.X, r.Y),
				tmp.Name())
		} else {
			cmd = exec.CommandContext(ctx, bin,
				"-hide_banner", "-loglevel", "error", "-nostdin",
				"-f", "x11grab", "-draw_mouse", "0",
				"-video_size", fmt.Sprintf("%dx%d", r.W, r.H),
				"-framerate", "1",
				"-i", fmt.Sprintf("%s+%d,%d", display, r.X, r.Y),
				"-frames:v", "1", "-y", tmp.Name())
		}
		cmd.Env = displayEnv(display)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("dolphin: grab %s+%d,%d %dx%d: %w: %s",
				display, r.X, r.Y, r.W, r.H, err, strings.TrimSpace(stderr.String()))
		}
		f, err := os.Open(tmp.Name())
		if err != nil {
			return nil, err
		}
		defer f.Close()
		img, err := png.Decode(f)
		if err != nil {
			return nil, fmt.Errorf("dolphin: decode grab PNG: %w", err)
		}
		return img, nil
	}
}

// envForDisplay is kept for the seam contract's symmetry with the spike's
// `on_display` helper; displayEnv is the form that actually wins precedence.
func envForDisplay(display string) []string { return displayEnv(display) }
