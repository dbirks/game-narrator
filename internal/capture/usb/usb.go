// Package usb answers "at what speed is the capture card's USB link actually
// running", from sysfs alone — docs/spec/10 "Hardware discovery and preflight"
// records the negotiated link, because the mode policy (internal/capture/mode)
// cannot ask for uncompressed YUYV 1080p60 unless the link is SuperSpeed:
// that mode is ~250 MB/s, more than double a HighSpeed (USB 2) port's 480 Mb/s.
//
// The read is read-only and needs no privileges: /sys/bus/usb/devices exposes
// every USB device's negotiated speed (speed, in Mbps) and the port's ceiling
// (maxportspeed). A card that is SuperSpeed-class but enumerated at 480 Mb/s —
// the classic "plugged into a USB 2 hub port" case — is a real, silent
// performance fault: the capture works, it just cannot deliver its best mode,
// so the doctor must say so (docs/spec/13 wants the operator action).
package usb

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// DefaultSysfsRoot is the production sysfs mount; tests point at a fabricated
// tree so the suite runs on a host with no USB device at all.
const DefaultSysfsRoot = "/sys"

// USB link speeds as sysfs reports them, in Mb/s (USB spec naming).
const (
	// SpeedLowSpeed is USB 1.1 low — no real capture card runs here.
	SpeedLowSpeed = 12
	// SpeedFullSpeed is USB 1.1 full — 12 Mb/s, a webcam class floor.
	SpeedFullSpeed = 12
	// SpeedHighSpeed is USB 2: 480 Mb/s. A SuperSpeed card negotiating here
	// is plugged into a USB 2 port or hub and cannot deliver raw 1080p.
	SpeedHighSpeed = 480
	// SpeedSuperSpeed is USB 3.0/3.1 Gen 1: 5000 Mb/s.
	SpeedSuperSpeed = 5000
	// SpeedSuperSpeedPlus is USB 3.1 Gen 2: 10000 Mb/s.
	SpeedSuperSpeedPlus = 10000
	// SpeedSuperSpeedPlus2 is USB 3.2 2x2: 20000 Mb/s.
	SpeedSuperSpeedPlus2 = 20000
)

// ErrNoVideoClassEntry means sysfs has no class entry for the node: the node
// is not a UVC card the kernel registered (a venus codec node, a stale name),
// so no USB link claim can be made.
var ErrNoVideoClassEntry = errors.New("usb: no sysfs class entry for the video node")

// ErrNoUSBDriver means the node exists but its device chain is not a USB
// device (platform or PCI attached): no speed file anywhere up the tree.
var ErrNoUSBDriver = errors.New("usb: the node's device chain has no USB device (speed unreadable)")

// Info is one node's USB link report.
type Info struct {
	// DevicePath is the sysfs directory of the USB device the node sits under.
	DevicePath string `json:"device_path"`
	// VendorID/ProductID are the device's USB ids ("1234"), the hardware
	// identity docs/spec/10's "match by USB bus identity" asks for.
	VendorID  string `json:"id_vendor,omitempty"`
	ProductID string `json:"id_product,omitempty"`
	// SpeedMBps is the negotiated link speed.
	SpeedMBps int `json:"speed_mbps"`
	// MaxPortSpeedMBps is the port's advertised ceiling; 0 when sysfs does
	// not expose it (older kernels).
	MaxPortSpeedMBps int `json:"max_port_speed_mbps,omitempty"`
}

// SuperSpeed reports whether the negotiated link is SuperSpeed or faster.
func (i Info) SuperSpeed() bool { return i.SpeedMBps >= SpeedSuperSpeed }

// Downgraded reports the fault this package exists to name: the link ceiling
// is SuperSpeed-class but the device negotiated HighSpeed — a USB 2 port,
// hub or cable. The card works; its best modes do not.
func (i Info) Downgraded() bool {
	return i.SpeedMBps == SpeedHighSpeed && i.MaxPortSpeedMBps >= SpeedSuperSpeed
}

// Class labels the speed for reports.
func (i Info) Class() string {
	switch {
	case i.SpeedMBps <= SpeedFullSpeed:
		return "full-speed"
	case i.SpeedMBps == SpeedHighSpeed:
		return "high-speed (USB 2)"
	case i.SpeedMBps == SpeedSuperSpeed:
		return "super-speed (USB 3)"
	default:
		return fmt.Sprintf("%d Mb/s", i.SpeedMBps)
	}
}

// maxHops bounds the walk up the sysfs tree: a real chain is at most a dozen
// directories deep, and a hostile tree must not make the walk unbounded.
const maxHops = 16

// LinkSpeedFor reports the USB link behind a capture node named like
// /dev/video0. root is the sysfs root (DefaultSysfsRoot in production).
//
// The resolution path mirrors the kernel's own: /sys/class/video4linux/videoN
// is the node's class symlink, whose device entry points at the parent device
// (the USB interface's device for a UVC card). The USB device node is the
// first ancestor that carries a speed file.
func LinkSpeedFor(videoNode, root string) (Info, error) {
	node := filepath.Base(videoNode)
	if node == "" || node == "." || node == "/" {
		return Info{}, fmt.Errorf("usb: empty video node name %q", videoNode)
	}
	entry := filepath.Join(root, "class", "video4linux", node, "device")
	resolved, err := filepath.EvalSymlinks(entry)
	if err != nil {
		if os.IsNotExist(err) {
			return Info{}, fmt.Errorf("%w: %s", ErrNoVideoClassEntry, entry)
		}
		return Info{}, fmt.Errorf("usb: resolve %s: %w", entry, err)
	}
	dir := resolved
	for hops := 0; hops < maxHops; hops++ {
		if isUSBDeviceDir(dir) {
			info, rerr := readInfo(dir)
			if rerr != nil {
				return Info{}, rerr
			}
			return info, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return Info{}, fmt.Errorf("%w: walked from %s", ErrNoUSBDriver, resolved)
}

// isUSBDeviceDir reports whether dir is a USB device node: it carries the
// speed file (and, on modern kernels, busnum alongside it).
func isUSBDeviceDir(dir string) bool {
	if fi, err := os.Stat(filepath.Join(dir, "speed")); err == nil && fi.Mode().IsRegular() {
		return true
	}
	return false
}

// readInfo reads the speed files out of one USB device directory. A missing
// id file is normal (some devices omit it) and yields an empty field; a
// missing or unparseable speed is an error — a speed-less "USB device" would
// be a claim the doctor could not defend.
func readInfo(dir string) (Info, error) {
	speed, err := readIntFile(filepath.Join(dir, "speed"))
	if err != nil {
		return Info{}, fmt.Errorf("usb: read %s: %w", filepath.Join(dir, "speed"), err)
	}
	info := Info{DevicePath: dir, SpeedMBps: speed}
	if max, err := readIntFile(filepath.Join(dir, "maxportspeed")); err == nil {
		info.MaxPortSpeedMBps = max
	}
	info.VendorID = readFileTrim(filepath.Join(dir, "idVendor"))
	info.ProductID = readFileTrim(filepath.Join(dir, "idProduct"))
	return info, nil
}

// readIntFile reads a small decimal sysfs file.
func readIntFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return v, nil
}

// readFileTrim reads a sysfs identity file, tolerating its absence.
func readFileTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
