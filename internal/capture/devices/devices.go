// Package devices answers "which capture card am I actually attached to" —
// docs/spec/10 "Hardware discovery and preflight": enumerate with v4l2-ctl,
// match by USB bus identity and stable path, and "never assume /dev/video0".
//
// The reason it is a subprocess and not an ioctl is ADR-0012 part 1: v4l-utils
// is already an explicit OS dependency, /dev/video* access is a group-permission
// problem the deployment has to solve anyway, and enumeration happens at
// preflight and on hotplug — not per frame — so a 20 ms exec is not a cost.
//
// Two facts this package must keep straight:
//
//   - A UVC card is usually two or three nodes (capture, metadata, media
//     controller). Picking the wrong one is the classic "the device is there but
//     every read fails" failure, so every node is recorded and the caller picks
//     by format support, not by index.
//   - /dev/videoN is reassigned by probe order. Two identical capture cards make
//     that visible: plugging one in later moves the other. StableID carries the
//     /dev/v4l/by-id (or by-path) symlink target when udev created one, because
//     that is the only name in the system that survives a reboot (docs/spec/10:
//     "reselect by stable ID on reboot").
package devices

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultBin is the v4l-utils enumeration tool, probed on PATH at every call
// rather than assumed (ADR-0012 §3).
const DefaultBin = "v4l2-ctl"

// DefaultTimeout bounds one enumeration call. Enumeration is interactive
// (doctor, hotplug), so it must not be able to hang a startup.
const DefaultTimeout = 5 * time.Second

// Stable-by and by-path directories populated by udev.
const (
	ByIDDir   = "/dev/v4l/by-id"
	ByPathDir = "/dev/v4l/by-path"
)

// ErrToolUnavailable means v4l2-ctl is not on PATH. Callers degrade to
// "no capture hardware discovered" plus an actionable message rather than
// treating it as an absent card (docs/spec/13 wants the operator action).
var ErrToolUnavailable = errors.New("devices: v4l2-ctl is not installed or not on PATH")

// Size is one resolution a device offers, with the frame rates valid at it.
type Size struct {
	W   int       `json:"w"`
	H   int       `json:"h"`
	FPS []float64 `json:"fps,omitempty"`
}

// Format is one pixel format and the sizes available under it.
type Format struct {
	// PixFmt is FFmpeg's/V4L2's name for the fourcc: "mjpeg", "yuyv422".
	PixFmt string `json:"pix_fmt"`
	// Description is v4l2-ctl's human text in parentheses, kept because
	// "MJPG" alone does not tell an operator whether compression is involved.
	Description string `json:"description,omitempty"`
	Sizes       []Size `json:"sizes,omitempty"`
}

// Device is one /dev/video* node with its identity and capabilities.
type Device struct {
	// Path is the node to open, e.g. /dev/video0. Never derived from an
	// assumption about index.
	Path string `json:"path"`
	// Name is the card name v4l2-ctl printed ("HD Webcam C270").
	Name string `json:"name"`
	// BusInfo is the parenthesised bus string ("usb-0000:00:14.0-5"), the USB
	// VID/PID-adjacent identity docs/spec/10 asks to match on.
	BusInfo string `json:"bus_info"`
	// StableID is the by-id symlink path naming this node, if udev made one.
	StableID string `json:"stable_id,omitempty"`
	// ByPath is the by-path symlink, the fallback when a card reports no serial
	// (many cheap UVC converters do not, so by-id is absent and by-path is the
	// only stable name — matching on physical port rather than identity).
	ByPath string `json:"by_path,omitempty"`
	// Formats is empty when enumeration of this node failed or was skipped; an
	// empty list is not evidence the device is unusable.
	Formats []Format `json:"formats,omitempty"`
	// FormatsError records why Formats is empty, so a permissions problem
	// (/dev/video* group membership is the top deployment blocker named in
	// docs/spec/10) is distinguishable from a card with no modes.
	FormatsError string `json:"formats_error,omitempty"`
	// Kind is the node type as labelled by v4l2-ctl ("Capture devices",
	// "Media controller devices", "Output devices"…), used to filter.
	Kind string `json:"kind,omitempty"`
}

// Usable reports whether the node looks like a video capture source with at
// least one negotiated mode.
func (d Device) Usable() bool {
	return strings.Contains(d.Kind, "Capture") && len(d.Formats) > 0
}

// Options tunes enumeration. The zero value means "run the real tool against the
// real /dev", which is what List uses.
type Options struct {
	// Bin overrides the v4l2-ctl path; empty means DefaultBin on PATH.
	Bin string
	// ByIDDir/ByPathDir override the stable-name directories, so a test can
	// fabricate a udev layout in a temp dir.
	ByIDDir   string
	ByPathDir string
	// Timeout bounds one tool call; 0 means DefaultTimeout.
	Timeout time.Duration
	// Run, when set, replaces the subprocess entirely and receives the argv
	// after the binary name. Tests use it to feed recorded v4l2-ctl output
	// without pretending the tool exists on a host that lacks it.
	Run func(ctx context.Context, args ...string) ([]byte, error)
}

// List enumerates capture devices. No devices is an empty slice and a nil error
// — an appliance with the card unplugged is a real, expected configuration that
// the UI has to show as "input offline", not as a crash (docs/spec/13).
func List(ctx context.Context) ([]Device, error) { return ListWith(ctx, Options{}) }

// ListWith is List with the host details injected.
func ListWith(ctx context.Context, o Options) ([]Device, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	run := o.Run
	if run == nil {
		bin := o.Bin
		if bin == "" {
			bin = DefaultBin
		}
		path, err := exec.LookPath(bin)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrToolUnavailable, err)
		}
		run = func(ctx context.Context, args ...string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, path, args...)
			var out bytes.Buffer
			cmd.Stdout = &out
			cmd.Stderr = &out // v4l2-ctl writes both; the parser ignores what it cannot use
			if err := cmd.Run(); err != nil {
				// A non-zero exit with parseable output is still useful, so the
				// bytes are returned alongside the error.
				return out.Bytes(), err
			}
			return out.Bytes(), nil
		}
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	listCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	out, err := run(listCtx, "--list-devices")
	// "No device found" is an empty list, not a failure. Anything else keeps
	// the error so doctor can say which.
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) && len(bytes.TrimSpace(out)) == 0 {
		if !bytes.Contains(out, []byte("/dev/video")) {
			return nil, fmt.Errorf("devices: v4l2-ctl --list-devices failed: %w", err)
		}
	}
	devs := ParseListDevices(string(out))
	if len(devs) == 0 {
		return nil, nil
	}

	byID := stableNames(o.stableDir(o.ByIDDir, ByIDDir))
	byPath := stableNames(o.stableDir(o.ByPathDir, ByPathDir))
	for i := range devs {
		if !strings.Contains(devs[i].Kind, "Capture") {
			// Media-controller and metadata nodes are recorded but not queried
			// for formats: enumerating them costs a subprocess each and a
			// metadata node has no modes.
			continue
		}
		devs[i].StableID = lookup(byID, devs[i].Path)
		devs[i].ByPath = lookup(byPath, devs[i].Path)
		fmtOut, ferr := run(listCtx, "-d", devs[i].Path, "--list-formats-ext")
		if ferr != nil && len(bytes.TrimSpace(fmtOut)) == 0 {
			devs[i].FormatsError = ferr.Error()
			continue
		}
		devs[i].Formats = ParseFormatsExt(string(fmtOut))
		if len(devs[i].Formats) == 0 && ferr != nil {
			devs[i].FormatsError = ferr.Error()
		}
	}
	sort.Slice(devs, func(i, j int) bool { return devs[i].Path < devs[j].Path })
	return devs, nil
}

// stableDir applies the real /dev/v4l root unless the caller overrode it. The
// override is per-field on purpose: a test that fabricates only by-id must not
// accidentally start reading the host's by-path entries, and a production caller
// that leaves both empty must actually look at /dev/v4l — a card with a serial
// but no StableID is unselectable after a replug, which is the failure
// docs/spec/10's "reselect by stable ID on reboot" exists to prevent.
func (o Options) stableDir(set, def string) string {
	if set != "" {
		return set
	}
	return def
}

// DefaultDirs reports the stable-name roots actually used, for doctor's output.
func DefaultDirs() (byID, byPath string) { return ByIDDir, ByPathDir }

// --- parsing ---------------------------------------------------------------

// headerRe matches v4l2-ctl's device header line:
//
//	HD WebCam: HD WebCam (usb-0000:00:14.0-5):
//	USB Video (usb-vif-1):
//
// i.e. anything that ends in ':' at column 0. The name is everything before the
// paren, the bus info is inside it.
var headerRe = regexp.MustCompile(`^([^:\t].*?):\s*$`)
var nameBusRe = regexp.MustCompile(`^(.*?)\s*\(([^()]*)\):\s*$`)

// ParseListDevices parses `v4l2-ctl --list-devices` output. It tolerates both
// the modern grouped layout
//
//	USB Video (usb-vif-1):
//	        Capture devices:
//	                /dev/video0
//	                /dev/media0
//
// and the older flat one
//
//	MS Media Center IR Transceiver: usb-0000:00:1d.7-1
//	        /dev/video0
//
// because the dev host runs v4l-utils 1.32 and the Debian release target runs
// 1.22 (docs/adr/0012 records the version gap); a parser that only understood
// one would silently report "no capture hardware" across the whole release path.
func ParseListDevices(out string) []Device {
	var (
		devs []Device
		name string
		bus  string
		kind string
	)
	flush := func() {
		name, bus, kind = "", "", ""
	}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		trimmed := strings.TrimLeft(line, " \t")
		isIndent := len(trimmed) < len(line)

		if strings.HasPrefix(trimmed, "/dev/") {
			path := strings.Fields(trimmed)[0]
			// Only video nodes are devices here. /dev/media0 is a controller and
			// opening it for capture is the "device exists, read fails" bug.
			if !strings.Contains(path, "video") {
				continue
			}
			devs = append(devs, Device{Path: path, Name: name, BusInfo: bus, Kind: kind})
			continue
		}
		if !isIndent {
			// Three header shapes, in the order they appear in the wild:
			//
			//	USB Video: USB Video (usb-vif-1):        modern, with bus info
			//	HCL HD Webcam (usb-0000:00:14.0-3):      modern, one name
			//	MS Media Center IR Transceiver: usb-0000:00:1d.7-1   legacy flat
			//
			// The last one is the reason the parser is not just "line ends in a
			// colon": on the Debian release target's v4l-utils 1.22 output it is
			// the difference between naming the card and naming nothing.
			if m := nameBusRe.FindStringSubmatch(line); m != nil {
				name, bus = strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
			} else if headerRe.MatchString(line) {
				name = strings.TrimSuffix(strings.TrimSpace(line), ":")
				bus = ""
			} else if i := strings.Index(line, ": "); i > 0 && !strings.Contains(line[i+2:], " ") {
				name, bus = strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+2:])
			} else {
				// "Cannot open device" noise and the like: not a header.
				flush()
			}
			kind = ""
			continue
		}
		// Indented non-path line: a category label ("Capture devices:").
		if strings.HasSuffix(strings.TrimSpace(trimmed), ":") {
			kind = strings.TrimSuffix(strings.TrimSpace(trimmed), ":")
		}
	}
	return devs
}

var (
	fmtRe     = regexp.MustCompile(`^\[(\d+)\]:\s*'([^']*)'\s*(?:\(([^)]*)\))?`)
	sizeRe    = regexp.MustCompile(`(?i)^\s*Size:\s*(?:Discrete|Stepwise)\s+(\d+)x(\d+)`)
	sizeEndRe = regexp.MustCompile(`(?i)^\s*Size:\s*Stepwise\s+\d+x\d+\s*-\s*(\d+)x(\d+)`)
	fpsRe     = regexp.MustCompile(`(?i)\(\s*([\d.]+)\s*fps\s*\)`)
	fpsRangeC = regexp.MustCompile(`(?i)\[\s*([\d.]+)\s*-\s*([\d.]+)\s*fps\s*\]`)
)

// ParseFormatsExt parses `v4l2-ctl -d DEV --list-formats-ext`:
//
//	ioctl: VIDIOC_ENUM_FMT
//		Type: Video Capture
//
//		[0]: 'YUYV' (YUYV 4:2:2)
//			Size: Discrete 640x480
//				Interval: Discrete 0.033s (30.000 fps)
//		[1]: 'MJPG' (Motion-JPEG, compressed)
//			Size: Discrete 1280x720
//				Interval: Stepwise 0.000s - 0.033s [2.000 - 30.000 fps]
//
// A stepwise interval contributes its bounds, which is the honest reading: the
// driver accepts anything in between, and a preflight that records 30 but
// negotiates 29.97 has not been told a difference, it has been told a lie
// (docs/spec/10: "Record fps, progressive/interlaced status, pixel format").
func ParseFormatsExt(out string) []Format {
	var (
		fmts  []Format
		cur   = -1
		lastC int
	)
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		t := strings.TrimSpace(line)
		if t == "" {
			continue
		}
		if m := fmtRe.FindStringSubmatch(t); m != nil {
			fmts = append(fmts, Format{PixFmt: fourcc(m[2]), Description: strings.TrimSpace(m[3])})
			cur = len(fmts) - 1
			lastC = 0
			continue
		}
		if cur < 0 {
			continue
		}
		switch {
		case strings.HasPrefix(t, "Size:"):
			if m := sizeEndRe.FindStringSubmatch(t); m != nil {
				w, _ := strconv.Atoi(m[1])
				h, _ := strconv.Atoi(m[2])
				fmts[cur].Sizes = append(fmts[cur].Sizes, Size{W: w, H: h})
				lastC = len(fmts[cur].Sizes) - 1
				continue
			}
			if m := sizeRe.FindStringSubmatch(t); m != nil {
				w, _ := strconv.Atoi(m[1])
				h, _ := strconv.Atoi(m[2])
				fmts[cur].Sizes = append(fmts[cur].Sizes, Size{W: w, H: h})
				lastC = len(fmts[cur].Sizes) - 1
			}
		case strings.HasPrefix(t, "Interval:"):
			if lastC < 0 || lastC >= len(fmts[cur].Sizes) {
				continue
			}
			sz := &fmts[cur].Sizes[lastC]
			if m := fpsRe.FindStringSubmatch(t); m != nil {
				if f, err := strconv.ParseFloat(m[1], 64); err == nil && f > 0 {
					sz.FPS = appendFPS(sz.FPS, f)
				}
				continue
			}
			if m := fpsRangeC.FindStringSubmatch(t); m != nil {
				lo, e1 := strconv.ParseFloat(m[1], 64)
				hi, e2 := strconv.ParseFloat(m[2], 64)
				if e1 == nil && e2 == nil && hi > 0 {
					sz.FPS = appendFPS(sz.FPS, hi)
					if lo > 0 && lo != hi {
						sz.FPS = appendFPS(sz.FPS, lo)
					}
				}
			}
		}
	}
	return fmts
}

func appendFPS(list []float64, f float64) []float64 {
	for _, x := range list {
		if x == f {
			return list
		}
	}
	return append(list, f)
}

// fourcc normalises a V4L2 fourcc to FFmpeg's pix_fmt naming where the two
// differ, so ffmpegpipe.V4L2 can be handed the value directly: ffmpeg's
// -input_format accepts "mjpeg" and "yuyv422", not "MJPG"/"YUYV".
func fourcc(s string) string {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "MJPG", "MJPEG":
		return "mjpeg"
	case "YUYV", "YUY2":
		return "yuyv422"
	case "UYVY":
		return "uyvy422"
	case "NV12", "NV21":
		return strings.ToLower(s)
	case "RGB3":
		return "rgb24" // ffmpeg names the raw packed form by content, not fourcc
	default:
		return strings.ToLower(strings.TrimSpace(s))
	}
}

// stableNames maps a device path to the stable symlink that points at it, by
// reading a by-id or by-path directory.
//
// The link *target* is read with Readlink rather than resolved with
// EvalSymlinks, on purpose: udev's links carry relative targets (../../video0),
// and a resolution that required the target to exist would drop every link for a
// card that is being enumerated right now but whose node the caller has not
// opened yet — and, more importantly, would make this untestable without root.
// A missing directory is not an error either: udev only creates it when a device
// is present, so a bare container or a host with no card has none at all.
func stableNames(dir string) map[string]string {
	m := map[string]string{}
	if dir == "" {
		return m
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return m
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	// First name wins in sorted order: by-id lists several aliases for one card
	// (one per interface), and "the first sorted" is at least reproducible across
	// runs of the same hardware, which "the shortest" is not.
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(dir, n)
		target, err := os.Readlink(p)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(dir, target)
		}
		target = filepath.Clean(target)
		if _, dup := m[target]; !dup {
			m[target] = p
		}
	}
	return m
}

func lookup(m map[string]string, path string) string {
	if path == "" {
		return ""
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	return m[filepath.Clean(abs)]
}
