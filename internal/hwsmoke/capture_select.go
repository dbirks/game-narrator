package hwsmoke

// Device and mode selection helpers for the capture smoke (docs/spec/10
// "matching via USB VID/PID and stable path; never assume /dev/video0").
// Kept out of the test file so the selection rules are readable and
// unit-drivable without the 10 s measurement window.

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/capture/devices"
)

// selectDevice applies docs/spec/10's device selection:
// GAME_NARRATOR_CAPTURE_DEVICE when set (exact path, or the udev stable
// by-id/by-path name — the rule that makes a replug resolvable), else the
// first capture-capable node. A capture-capable node is one v4l2-ctl labels
// Capture; a node whose format enumeration succeeded is preferred over one
// that failed, so a permission problem surfaces as NOT RUN with the
// permission remedy instead of a crash on a node this user cannot open.
// The returned missing string is the exact NOT RUN reason when no device
// can be selected; why explains the successful choice for the log.
func selectDevice(devs []devices.Device) (dev devices.Device, why, missing string) {
	capable := make([]devices.Device, 0, len(devs))
	for _, d := range devs {
		if strings.Contains(d.Kind, "Capture") {
			capable = append(capable, d)
		}
	}
	if len(capable) == 0 {
		return dev, "", "no capture-capable V4L2 node (v4l2-ctl labels none as Capture)"
	}
	if want := os.Getenv(EnvCaptureDevice); want != "" {
		wantBase := filepath.Base(want)
		for _, d := range capable {
			if d.Path == want || d.StableID == want || d.ByPath == want ||
				filepath.Base(d.StableID) == wantBase || filepath.Base(d.ByPath) == wantBase {
				return d, "GAME_NARRATOR_CAPTURE_DEVICE=" + want, ""
			}
		}
		found := make([]string, 0, len(capable))
		for _, d := range capable {
			found = append(found, d.Path)
		}
		return dev, "", fmt.Sprintf("%s=%s does not match any capture node (found: %s)",
			EnvCaptureDevice, want, strings.Join(found, ", "))
	}
	for _, d := range capable {
		if len(d.Formats) > 0 {
			why = "first capture-capable node with enumerated formats"
			if d.StableID != "" {
				why += " (stable id " + d.StableID + ")"
			}
			return d, why, ""
		}
	}
	// A capture node whose enumeration failed (permissions, unsupported
	// driver mode) is still selectable: the open attempt below proves or
	// disproves it, with the driver's own error as the evidence.
	d := capable[0]
	why = "first capture-capable node (formats unenumerated: " +
		orDash(d.FormatsError) + ")"
	return d, why, ""
}

// howToFixCapture turns selectDevice's missing reason into the attach or
// configure step the operator runs.
func howToFixCapture(missing string) string {
	switch {
	case strings.HasPrefix(missing, "no capture-capable"):
		return "attach the UVC HDMI capture dongle through the USB hub " +
			"(docs/spec/10 physical connection) and re-run make test-hardware"
	case strings.Contains(missing, "does not match"):
		return "set " + EnvCaptureDevice + " to one of the found paths/stable names, " +
			"or unset it to take the first capture-capable node"
	default:
		return "see the logged enumeration; add the node's stable name to " +
			EnvCaptureDevice + " once the card is attached"
	}
}

// pickMode chooses the negotiated mode: mjpeg preferred (UVC default and
// the format docs/spec/10's preflight records), then yuyv422; the largest
// size within that format; the first advertised discrete rate (0 when the
// driver lists a continuous range — FFmpeg then takes the device default,
// and the measured FPS in Stats is what the test reports).
func pickMode(d devices.Device) (string, devices.Size, float64) {
	for _, pref := range []string{"mjpeg", "yuyv422"} {
		for _, f := range d.Formats {
			if f.PixFmt != pref || len(f.Sizes) == 0 {
				continue
			}
			best := f.Sizes[0]
			for _, s := range f.Sizes {
				if s.W*s.H > best.W*best.H {
					best = s
				}
			}
			fps := 0.0
			if len(best.FPS) > 0 {
				fps = best.FPS[0]
			}
			return pref, best, fps
		}
	}
	return "", devices.Size{}, 0
}

func fpsList(fps []float64) string {
	if len(fps) == 0 {
		return "device-default"
	}
	parts := make([]string, len(fps))
	for i, f := range fps {
		parts[i] = strconv.FormatFloat(f, 'f', -1, 64)
	}
	return strings.Join(parts, "/")
}

func rateLabel(fps float64) string {
	if fps <= 0 {
		return "device-default"
	}
	return strconv.FormatFloat(fps, 'f', -1, 64)
}

func restartLabel(n uint64) string {
	if n > 0 {
		return fmt.Sprintf("%d (warn: the source restarted during the window)", n)
	}
	return "0"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func roundDuration(d time.Duration) string { return d.Round(100 * time.Millisecond).String() }
