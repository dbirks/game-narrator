package devices

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/dbirks/game-narrator/internal/capture"
)

// Recorded v4l2-ctl output, kept verbatim in shape from `v4l2-ctl --list-devices`
// and `v4l2-ctl -d DEV --list-formats-ext` on real hosts. These strings are the
// only way to test enumeration on a machine with no capture card attached — and
// the parsing they exercise is the same code that decides which /dev/videoN the
// UNO Q opens, so "we assume /dev/video0" would show up here as a failure to
// distinguish the nodes at all (docs/spec/10 "Hardware discovery and preflight").

// modernListOutput is the grouped layout produced by v4l-utils ≥1.22, which both
// the Arch dev host (1.32) and the Debian release target (1.22) emit. The card is
// a typical cheap UVC converter: two video nodes (capture + metadata) and a media
// controller node.
const modernListOutput = `USB Video: USB Video (usb-vif-1):
	Capture devices:
		/dev/video0
		/dev/video1
	Media controller devices:
		/dev/media0

HCL HD Webcam: HCL HD Webcam (usb-0000:00:14.0-5):
	Capture devices:
		/dev/video2
	Media controller devices:
		/dev/media1
`

// legacyListOutput is the pre-1.22 flat layout. The release target's older
// v4l-utils makes a parser that only understands the modern shape report "no
// capture hardware" on every UNO Q, which is the worst kind of silent failure.
const legacyListOutput = `MS Media Center IR Transceiver: usb-0000:00:1d.7-1
	/dev/video0
HCL HD Webcam (usb-0000:00:14.0-3):
	/dev/video1
	/dev/video2
	/dev/media0
`

// mjpegYUYVFormats is a UVC card offering both formats docs/spec/05 names, with
// a per-size framerate list and one stepwise interval range.
const mjpegYUYVFormats = `ioctl: VIDIOC_ENUM_FMT
	Type: Video Capture

	[0]: 'YUYV' (YUYV 4:2:2)
		Size: Discrete 640x480
			Interval: Discrete 0.033s (30.000 fps)
			Interval: Discrete 0.040s (25.000 fps)
			Interval: Discrete 0.050s (20.000 fps)
			Interval: Discrete 0.100s (10.000 fps)
		Size: Discrete 320x240
			Interval: Discrete 0.033s (30.000 fps)
		Size: Discrete 160x120
			Interval: Discrete 0.016s (60.000 fps)
	[1]: 'MJPG' (Motion-JPEG, compressed)
		Size: Discrete 1920x1080
			Interval: Discrete 0.033s (30.000 fps)
		Size: Discrete 1280x720
			Interval: Discrete 0.033s (30.000 fps)
		Size: Discrete 640x480
			Interval: Discrete 0.016s (60.000 fps)
			Interval: Discrete 0.033s (30.000 fps)
		Size: Stepwise 32x32 - 1920x1080 with step 8/8
			Interval: Stepwise 0.000s - 0.033s [2.000 - 30.000 fps]
`

// multiplanarOnly is what a card exposing only the multiplanar API reports; the
// parser must still read it (some UVC bridge chips do this).
const multiplanarOnly = `ioctl: VIDIOC_ENUM_FMT
	Type: Video Capture Multiplanar

	[0]: 'NV12' (Y/CbCr 4:2:0)
		Size: Discrete 1280x720
			Interval: Discrete 0.050s (20.000 fps)
`

// noFormats is what a metadata node says when asked for modes.
const noFormats = `ioctl: VIDIOC_ENUM_FMT
	Type: Video Capture

`

func TestParseListDevicesModernLayout(t *testing.T) {
	devs := ParseListDevices(modernListOutput)
	if len(devs) != 3 {
		t.Fatalf("parsed %d devices, want 3 (video0, video1, video2; media nodes are not devices): %+v", len(devs), devs)
	}
	byPath := map[string]Device{}
	for _, d := range devs {
		byPath[d.Path] = d
	}
	for _, want := range []string{"/dev/video0", "/dev/video1", "/dev/video2"} {
		if _, ok := byPath[want]; !ok {
			t.Errorf("missing %s", want)
		}
	}
	if _, ok := byPath["/dev/media0"]; ok {
		t.Error("/dev/media0 parsed as a device: opening the media controller for capture is the 'device exists, read fails' bug")
	}
	if got := byPath["/dev/video0"].Name; got != "USB Video: USB Video" && got != "USB Video" {
		t.Errorf("video0 name = %q", got)
	}
	if got := byPath["/dev/video0"].BusInfo; got != "usb-vif-1" {
		t.Errorf("video0 bus info = %q, want usb-vif-1", got)
	}
	if got := byPath["/dev/video2"].BusInfo; got != "usb-0000:00:14.0-5" {
		t.Errorf("video2 bus info = %q, want usb-0000:00:14.0-5", got)
	}
	if got := byPath["/dev/video2"].Name; !strings.Contains(got, "HCL HD Webcam") {
		t.Errorf("video2 name = %q; the card name is what preflight shows the user", got)
	}
	if got := byPath["/dev/video0"].Kind; !strings.Contains(got, "Capture") {
		t.Errorf("video0 kind = %q, want a Capture category (this is the filter that picks the node)", got)
	}
}

func TestParseListDevicesLegacyLayout(t *testing.T) {
	devs := ParseListDevices(legacyListOutput)
	if len(devs) != 3 {
		t.Fatalf("parsed %d devices, want 3: %+v", len(devs), devs)
	}
	if devs[0].Path != "/dev/video0" {
		t.Errorf("first device = %s", devs[0].Path)
	}
	// The flat layout's header is "Name: bus-info" with the bus outside any
	// parentheses — the shape that differs from the modern one.
	if !strings.Contains(devs[0].Name, "MS Media Center IR Transceiver") {
		t.Errorf("legacy name = %q; the flat 'Name: bus-info' header must still name the card", devs[0].Name)
	}
	if !strings.Contains(devs[0].BusInfo, "usb-0000:00:1d.7-1") {
		t.Errorf("legacy bus info = %q, want the usb-0000:00:1d.7-1 tail", devs[0].BusInfo)
	}
	if devs[1].Name != "HCL HD Webcam" || devs[1].BusInfo != "usb-0000:00:14.0-3" {
		t.Errorf("legacy parenthesised header parsed as %q/%q", devs[1].Name, devs[1].BusInfo)
	}
	var kindSet []string
	for _, d := range devs {
		kindSet = append(kindSet, d.Kind)
	}
	t.Logf("legacy layout kinds: %v (empty is expected: the old format has no category labels)", kindSet)
}

func TestParseListDevicesEmptyAndNoise(t *testing.T) {
	for name, out := range map[string]string{
		"empty":          "",
		"whitespace":     "\n\n  \n",
		"no devices":     "Cannot open device.\n",
		"v4l2-ctl quiet": "# produced by v4l2-ctl with no cards attached\n",
	} {
		if got := ParseListDevices(out); len(got) != 0 {
			t.Errorf("%s: parsed %+v, want no devices", name, got)
		}
	}
}

func TestParseFormatsExt(t *testing.T) {
	fmts := ParseFormatsExt(mjpegYUYVFormats)
	if len(fmts) != 2 {
		t.Fatalf("parsed %d formats, want 2: %+v", len(fmts), fmts)
	}
	yuyv, mjpeg := fmts[0], fmts[1]
	if yuyv.PixFmt != "yuyv422" {
		t.Errorf("format 0 = %q, want yuyv422 (the name ffmpegpipe.V4L2 accepts as -input_format)", yuyv.PixFmt)
	}
	if mjpeg.PixFmt != "mjpeg" {
		t.Errorf("format 1 = %q, want mjpeg", mjpeg.PixFmt)
	}
	if !strings.Contains(mjpeg.Description, "Motion-JPEG") {
		t.Errorf("mjpeg description = %q", mjpeg.Description)
	}

	if len(yuyv.Sizes) != 3 {
		t.Fatalf("yuyv sizes = %+v, want 3", yuyv.Sizes)
	}
	first := yuyv.Sizes[0]
	if first.W != 640 || first.H != 480 {
		t.Errorf("yuyv size 0 = %dx%d, want 640x480", first.W, first.H)
	}
	want := []float64{30, 25, 20, 10}
	if len(first.FPS) != len(want) {
		t.Fatalf("yuyv 640x480 framerates = %v, want %v", first.FPS, want)
	}
	for i, w := range want {
		if first.FPS[i] != w {
			t.Errorf("framerate %d = %v, want %v", i, first.FPS[i], w)
		}
	}
	if got := yuyv.Sizes[2]; got.W != 160 || got.H != 120 || len(got.FPS) != 1 || got.FPS[0] != 60 {
		t.Errorf("yuyv 160x120 = %+v, want [60] fps", got)
	}

	// The stepwise size: bounds recorded, one entry per distinct rate.
	step := mjpeg.Sizes[len(mjpeg.Sizes)-1]
	if step.W != 1920 || step.H != 1080 {
		t.Errorf("stepwise size end = %dx%d, want the range's 1920x1080 maximum", step.W, step.H)
	}
	if len(step.FPS) != 2 || step.FPS[0] != 30 || step.FPS[1] != 2 {
		t.Errorf("stepwise framerates = %v, want [30 2] (both bounds of [2.000 - 30.000 fps])", step.FPS)
	}
	// Each size belongs to its own format: a parser that attached intervals to
	// the wrong index would make preflight claim a mode the card lacks.
	for _, f := range fmts {
		for _, sz := range f.Sizes {
			if len(sz.FPS) == 0 {
				t.Errorf("%s %dx%d has no framerate recorded", f.PixFmt, sz.W, sz.H)
			}
		}
	}
}

func TestParseFormatsExtMultiplanarAndEmpty(t *testing.T) {
	fmts := ParseFormatsExt(multiplanarOnly)
	if len(fmts) != 1 || fmts[0].PixFmt != "nv12" {
		t.Fatalf("multiplanar parse = %+v, want one nv12 format", fmts)
	}
	if len(fmts[0].Sizes) != 1 || fmts[0].Sizes[0].FPS[0] != 20 {
		t.Errorf("multiplanar sizes = %+v", fmts[0].Sizes)
	}
	if got := ParseFormatsExt(noFormats); len(got) != 0 {
		t.Errorf("metadata-node output parsed as %+v, want nothing", got)
	}
	if got := ParseFormatsExt(""); len(got) != 0 {
		t.Errorf("empty output parsed as %+v", got)
	}
	// Garbage must not panic or invent modes.
	if got := ParseFormatsExt("random text\n[0]: broken\n\tSize: Discrete\n"); len(got) != 0 {
		t.Errorf("garbage parsed as %+v", got)
	}
}

func TestFourccNamingMatchesFFmpegInputFormats(t *testing.T) {
	// devices.List output feeds ffmpegpipe.V4L2's inputFormat argument, and that
	// constructor only accepts mjpeg|yuyv422. If the fourcc mapping drifts, the
	// enumeration result becomes unusable by the capture path — so the two
	// packages' vocabularies are checked against each other here rather than in
	// a comment.
	cases := map[string]string{
		"YUYV": "yuyv422",
		"YUY2": "yuyv422",
		"MJPG": "mjpeg",
		"UYVY": "uyvy422",
		"NV12": "nv12",
		"RGB3": "rgb24",
	}
	// The expected values are capture's own constants, not string literals: that
	// is what makes this a cross-package vocabulary check rather than a test of
	// two files that happened to be written with the same typos.
	const (
		wantMJPEG = string(capture.PixMJPEG)
		wantYUYV  = string(capture.PixYUYV422)
	)
	if wantMJPEG != "mjpeg" || wantYUYV != "yuyv422" {
		t.Fatalf("capture.PixMJPEG/PixYUYV422 = %q/%q; ffmpeg's -input_format accepts mjpeg and yuyv422", wantMJPEG, wantYUYV)
	}
	for cc, want := range cases {
		if got := fourcc(cc); got != want {
			t.Errorf("fourcc(%s) = %q, want %q", cc, got, want)
		}
	}
	if got := fourcc("GREY"); got != "grey" {
		t.Errorf("unknown fourcc GREY = %q, want it lowercased through unchanged", got)
	}
}

// --- ListWith with an injected runner ---------------------------------------

func TestListWithEndToEndUsingRecordedToolOutput(t *testing.T) {
	root := t.TempDir()
	// A fabricated udev stable-name layout: two identical UVC cards, where by-id
	// exists for both and by-path names the physical port. This is the case
	// docs/spec/10 is written for — /dev/videoN is reassigned by probe order, so
	// replugging one card moves the other, and only the stable name survives a
	// reboot. The nodes themselves are not created here: the parser reads link
	// targets, which is exactly what lets this run without root or hardware.
	byID := filepath.Join(root, "by-id")
	byPath := filepath.Join(root, "by-path")
	for _, d := range []string{byID, byPath} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	// The links are dangling on purpose: enumeration must be able to name a
	// card's stable identity from the link alone, and a test that needed the
	// target node to exist could only ever run on a machine with the hardware.
	mustLink(t, filepath.Join(byID, "usb-0bda_0x58f0-video-index0"), "/dev/video0")
	mustLink(t, filepath.Join(byID, "usb-0bda_0x58f0-video-index1"), "/dev/video1")
	mustLink(t, filepath.Join(byPath, "pci-0000_00_14.0-usb-0_5-video-index0"), "/dev/video0")
	mustLink(t, filepath.Join(byPath, "pci-0000_00_1d.0-usb-0_1-video-index0"), "/dev/video1")

	calls := 0
	runner := func(ctx context.Context, args ...string) ([]byte, error) {
		calls++
		if ctx == nil {
			t.Error("runner got a nil context")
		}
		switch {
		case len(args) == 1 && args[0] == "--list-devices":
			return []byte(modernListOutput), nil
		case len(args) == 3 && args[0] == "-d" && args[2] == "--list-formats-ext":
			switch args[1] {
			case "/dev/video0":
				return []byte(mjpegYUYVFormats), nil
			case "/dev/video1":
				return []byte(noFormats), nil
			default:
				return nil, fmt.Errorf("v4l2-ctl: can't open '%s': No such file or directory", args[1])
			}
		}
		t.Errorf("unexpected v4l2-ctl invocation: %v", args)
		return nil, errors.New("unexpected invocation")
	}

	devs, err := ListWith(context.Background(), Options{
		Run:       runner,
		ByIDDir:   byID,
		ByPathDir: byPath,
	})
	if err != nil {
		t.Fatalf("ListWith: %v", err)
	}
	// media0 is filtered out of the format query but still listed; video0,
	// video1 have nodes on disk, video2 does not exist on this fake host.
	if len(devs) == 0 {
		t.Fatal("no devices parsed")
	}
	var paths []string
	for _, d := range devs {
		paths = append(paths, d.Path)
	}
	sort.Strings(paths)
	t.Logf("enumerated: %v in %d tool calls", paths, calls)

	// video0 is the fully described card: capture kind, two formats, stable ids.
	var v0 *Device
	for i := range devs {
		if devs[i].Path == "/dev/video0" {
			v0 = &devs[i]
		}
	}
	if v0 == nil {
		t.Fatalf("video0 missing from %+v", devs)
	}
	if !v0.Usable() {
		t.Errorf("video0 not usable: kind=%q formats=%d", v0.Kind, len(v0.Formats))
	}
	if len(v0.Formats) != 2 {
		t.Errorf("video0 formats = %+v, want mjpeg + yuyv", v0.Formats)
	}
	if v0.StableID == "" || !strings.Contains(v0.StableID, "usb-0bda") {
		t.Errorf("video0 stable id = %q; a card with a serial must be selectable by name across reboots", v0.StableID)
	}
	if v0.ByPath == "" {
		t.Errorf("video0 by-path = %q, want the physical-port name", v0.ByPath)
	}
	// The second card has a by-path name but its by-id alias points at video1,
	// so a listing that matched stable ids by *position* rather than by target
	// would show up here.
	var v2 *Device
	for i := range devs {
		if devs[i].Path == "/dev/video2" {
			v2 = &devs[i]
		}
	}
	if v2 == nil {
		t.Fatal("video2 missing")
	}
	if v2.StableID != "" || v2.ByPath != "" {
		t.Errorf("video2 got stable ids %q/%q with no link pointing at it; the card it belongs to would be misidentified after a replug", v2.StableID, v2.ByPath)
	}

	// A node whose enumeration failed records the reason instead of failing the
	// whole listing: one unreadable node must not make preflight report "no
	// hardware".
	var v1 *Device
	for i := range devs {
		if devs[i].Path == "/dev/video1" {
			v1 = &devs[i]
		}
	}
	if v1 != nil && v1.Usable() {
		t.Errorf("video1 reported usable with no formats: %+v", v1)
	}
}

func TestListStableIDLookupMissesQuietly(t *testing.T) {
	// No by-id directory at all (the common case on a host without udev rules or
	// with a card that has no serial): StableID stays empty and the listing still
	// succeeds.
	devs, err := ListWith(context.Background(), Options{
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			if len(args) == 1 {
				return []byte(modernListOutput), nil
			}
			return []byte(noFormats), nil
		},
		ByIDDir:   filepath.Join(t.TempDir(), "absent"),
		ByPathDir: filepath.Join(t.TempDir(), "absent"),
	})
	if err != nil {
		t.Fatalf("ListWith: %v", err)
	}
	for _, d := range devs {
		if d.StableID != "" || d.ByPath != "" {
			t.Errorf("%s got stable ids %q/%q with no stable-name directories present", d.Path, d.StableID, d.ByPath)
		}
	}
}

func TestListWithPropagatesToolFailure(t *testing.T) {
	// A failing tool with no output is a real fault with an actionable message
	// (docs/spec/13 wants the operator action, not silence).
	_, err := ListWith(context.Background(), Options{
		Run: func(ctx context.Context, args ...string) ([]byte, error) {
			return nil, errors.New("v4l2-ctl: Permission denied")
		},
	})
	if err == nil {
		t.Fatal("a failing enumerator returned no error")
	}
	if !strings.Contains(err.Error(), "Permission denied") {
		t.Errorf("error %q lost the tool's message", err)
	}
}

func TestListWithTreatsQuietEmptyOutputAsNoDevices(t *testing.T) {
	// An appliance with the capture card unplugged is a real, expected
	// configuration: empty list, nil error, and the UI shows "input offline"
	// rather than crashing (docs/spec/13 "Capture removed").
	devs, err := ListWith(context.Background(), Options{
		Run: func(ctx context.Context, args ...string) ([]byte, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("ListWith with no devices: %v", err)
	}
	if len(devs) != 0 {
		t.Errorf("devices = %+v, want none", devs)
	}
}

// TestListOnThisHost runs the real v4l2-ctl. On a host with no UVC device
// attached it must return an empty list and no error; if a card is present the
// same call must produce at least one node with a real path, and never invent
// /dev/video0.
func TestListOnThisHost(t *testing.T) {
	devs, err := List(context.Background())
	if err != nil {
		if errors.Is(err, ErrToolUnavailable) {
			t.Skipf("NOT RUN: %v (install v4l-utils per scripts/bootstrap-arch.sh)", err)
		}
		t.Fatalf("List: %v", err)
	}
	if len(devs) == 0 {
		t.Log("NOT VERIFIED against hardware: no capture device attached on this host (empty list, no error — the documented behaviour for an unplugged card)")
		return
	}
	for _, d := range devs {
		if !strings.HasPrefix(d.Path, "/dev/video") {
			t.Errorf("device path %q is not a video node", d.Path)
		}
		t.Logf("host device: %s name=%q bus=%q kind=%q formats=%d stable=%q",
			d.Path, d.Name, d.BusInfo, d.Kind, len(d.Formats), d.StableID)
	}
}

func TestUsableFiltersNonCaptureNodes(t *testing.T) {
	devs := ParseListDevices(modernListOutput)
	usable := 0
	for _, d := range devs {
		if d.Usable() {
			usable++
		}
	}
	if usable != 0 {
		t.Errorf("%d parsed nodes reported usable before format enumeration, want 0: usability needs a format list, not just a node", usable)
	}
	d := Device{Path: "/dev/video0", Kind: "Capture devices", Formats: []Format{{PixFmt: "mjpeg"}}}
	if !d.Usable() {
		t.Error("a capture node with a format was not usable")
	}
}

func TestStableNamesResolvesRelativeLinks(t *testing.T) {
	// udev writes relative targets ("../../video0"); resolving them against the
	// link's own directory is what makes the mapping work on real hardware, where
	// no absolute target ever appears in the directory listing.
	root := t.TempDir()
	dir := filepath.Join(root, "v4l", "by-id")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	mustLink(t, filepath.Join(dir, "usb-1234-video-index0"), "../../video0")
	// A regular file in the directory must be skipped rather than mapped.
	if err := os.WriteFile(filepath.Join(dir, "not-a-link"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	m := stableNames(dir)
	got, ok := m[filepath.Clean(filepath.Join(root, "video0"))]
	if !ok {
		t.Fatalf("relative link resolved to %v, want the video0 path", m)
	}
	if filepath.Base(got) != "usb-1234-video-index0" {
		t.Errorf("mapped to %q", got)
	}
	if stableNames(filepath.Join(root, "absent")) == nil {
		t.Error("a missing directory should yield an empty map, not nil-failure")
	}
	if len(stableNames("")) != 0 {
		t.Error("empty directory name should yield nothing")
	}
}

func mustLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

func TestStableDirDefaultsAreTheRealUdevRoots(t *testing.T) {
	// A caller that forgets to wire the stable-name roots silently loses the
	// reboot-surviving identity docs/spec/10 requires, and nothing downstream can
	// tell the difference until a replug picks the wrong node. The filesystem-level
	// version of this check would need to create /dev/v4l/by-id, i.e. root, which
	// this project forbids — so the resolution rule is tested directly and the
	// constants are pinned to the paths udev actually populates.
	if ByIDDir != "/dev/v4l/by-id" || ByPathDir != "/dev/v4l/by-path" {
		t.Errorf("stable roots = %q, %q; udev writes these paths", ByIDDir, ByPathDir)
	}
	if id, path := DefaultDirs(); id != ByIDDir || path != ByPathDir {
		t.Errorf("DefaultDirs() = (%q,%q), want the udev roots", id, path)
	}
	var o Options
	if got := o.stableDir(o.ByIDDir, ByIDDir); got != ByIDDir {
		t.Errorf("unset ByIDDir resolved to %q, want %q — the bug this guards is an empty path reading nothing", got, ByIDDir)
	}
	if got := (Options{ByIDDir: "/tmp/x"}).stableDir("/tmp/x", ByIDDir); got != "/tmp/x" {
		t.Errorf("an explicit override must win, got %q", got)
	}
	// Per-field resolution: fabricating only by-id must not keep reading the
	// host's by-path entries.
	o2 := Options{ByIDDir: "/tmp/fake-by-id"}
	if got := o2.stableDir(o2.ByIDDir, ByIDDir); got != "/tmp/fake-by-id" {
		t.Errorf("by-id = %q", got)
	}
	if got := o2.stableDir(o2.ByPathDir, ByPathDir); got != ByPathDir {
		t.Errorf("by-path = %q, want the host default because it was not overridden", got)
	}
}
