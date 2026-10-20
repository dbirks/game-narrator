package usb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeSysfs builds a minimal sysfs tree under a temp root:
//
//	root/class/video4linux/videoN/device -> <devices dir>
//	root/devices/<...>/usbX/X-Y/speed (+ maxportspeed, idVendor, idProduct)
//
// The symlink target is an absolute path inside the temp dir so
// EvalSymlinks resolves it without leaving the tree.
func fakeSysfs(t *testing.T, node string, usbRel string, speed, maxport string) string {
	t.Helper()
	root := t.TempDir()
	usbDir := filepath.Join(root, "devices", usbRel)
	if err := os.MkdirAll(usbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"speed":  speed,
		"busnum": "3",
	}
	if maxport != "" {
		files["maxportspeed"] = maxport
	}
	for name, v := range files {
		if err := os.WriteFile(filepath.Join(usbDir, name), []byte(v+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(usbDir, "idVendor"), []byte("1bcf\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbDir, "idProduct"), []byte("2d3d\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The class entry: video4linux/<node>/device is a symlink chain; one level
	// (usbX) of indirection between the class target and the device dir, the
	// shape real sysfs uses (…/usb3/3-1/3-1.2).
	classDir := filepath.Join(root, "class", "video4linux", node)
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	mid := filepath.Dir(usbDir)
	if err := os.MkdirAll(mid, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(usbDir, filepath.Join(classDir, "device")); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestLinkSpeedForReadsNegotiatedSpeed(t *testing.T) {
	root := fakeSysfs(t, "video0", "usb3/3-1/3-1.2", "5000", "5000")
	info, err := LinkSpeedFor("/dev/video0", root)
	if err != nil {
		t.Fatalf("LinkSpeedFor: %v", err)
	}
	if info.SpeedMBps != 5000 || info.MaxPortSpeedMBps != 5000 {
		t.Errorf("speed = %d/%d, want 5000/5000", info.SpeedMBps, info.MaxPortSpeedMBps)
	}
	if !info.SuperSpeed() {
		t.Errorf("a 5000 Mb/s link must classify SuperSpeed, got %q", info.Class())
	}
	if info.Downgraded() {
		t.Error("a card at its port ceiling must not report Downgraded")
	}
	if info.VendorID != "1bcf" || info.ProductID != "2d3d" {
		t.Errorf("ids = %s/%s, want 1bcf/2d3d", info.VendorID, info.ProductID)
	}
}

func TestLinkSpeedForFlagsSuperSpeedCardDowngradedToHighSpeed(t *testing.T) {
	// The fault the whole package exists for: the port ceiling is 5000 (the
	// host has a USB 3 port) but the card negotiated 480 — a USB 2 hub in the
	// middle. YUYV 1080p60 is impossible on this link and the doctor must say.
	root := fakeSysfs(t, "video1", "usb2/2-1/2-1.1", "480", "5000")
	info, err := LinkSpeedFor("/dev/video1", root)
	if err != nil {
		t.Fatalf("LinkSpeedFor: %v", err)
	}
	if info.SpeedMBps != SpeedHighSpeed {
		t.Fatalf("speed = %d, want %d", info.SpeedMBps, SpeedHighSpeed)
	}
	if !info.Downgraded() {
		t.Error("480 Mb/s on a 5000 Mb/s port must report Downgraded")
	}
	if info.SuperSpeed() {
		t.Error("a HighSpeed negotiation must not classify SuperSpeed")
	}
}

func TestLinkSpeedForHighSpeedOnHighSpeedPortIsNotDowngraded(t *testing.T) {
	// A USB 2 port capping a USB 2 card is the link's normal state: a USB 2
	// card's own maxportspeed is 480, so there is no downgrade to name.
	root := fakeSysfs(t, "video2", "usb1/1-1", "480", "480")
	info, err := LinkSpeedFor("/dev/video2", root)
	if err != nil {
		t.Fatalf("LinkSpeedFor: %v", err)
	}
	if info.Downgraded() {
		t.Error("a card running at its port ceiling must not report Downgraded")
	}
}

func TestLinkSpeedForWalksUpFromInterfaceNode(t *testing.T) {
	// Some drivers register the video node's device at the *interface* level,
	// one directory above the device node: the walk must find speed above.
	root := t.TempDir()
	dev := filepath.Join(root, "devices", "platform", "soc", "usb")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		t.Fatal(err)
	}
	iface := filepath.Join(dev, "usb2", "2-1.3")
	if err := os.MkdirAll(iface, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dev, "speed"), []byte("480\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	classDir := filepath.Join(root, "class", "video4linux", "video3")
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(iface, filepath.Join(classDir, "device")); err != nil {
		t.Fatal(err)
	}
	info, err := LinkSpeedFor("/dev/video3", root)
	if err != nil {
		t.Fatalf("LinkSpeedFor: %v", err)
	}
	if info.SpeedMBps != 480 {
		t.Errorf("speed = %d, want 480 (found by walking up)", info.SpeedMBps)
	}
}

func TestLinkSpeedForMissingNodeIsNotAUSBError(t *testing.T) {
	root := t.TempDir()
	_, err := LinkSpeedFor("/dev/video9", root)
	if err == nil {
		t.Fatal("a node with no class entry must error")
	}
	if !errors.Is(err, ErrNoVideoClassEntry) {
		t.Errorf("a missing class entry must report %v, got %v", ErrNoVideoClassEntry, err)
	}
}

func TestLinkSpeedForNonUSBChainReportsNoUSBDriver(t *testing.T) {
	// A venus-style platform chain: class entry resolves but no speed file
	// anywhere up the tree — the node is a codec, not a USB capture card.
	root := t.TempDir()
	plat := filepath.Join(root, "devices", "platform", "soc:qcom-venus")
	if err := os.MkdirAll(plat, 0o755); err != nil {
		t.Fatal(err)
	}
	classDir := filepath.Join(root, "class", "video4linux", "video0")
	if err := os.MkdirAll(classDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(plat, filepath.Join(classDir, "device")); err != nil {
		t.Fatal(err)
	}
	_, err := LinkSpeedFor("/dev/video0", root)
	if !errors.Is(err, ErrNoUSBDriver) {
		t.Fatalf("a non-USB device chain must report %v, got %v", ErrNoUSBDriver, err)
	}
}
