package doctor

import (
	"strings"
)

// v4l2-ctl --info capability parsing for the capture check.
//
// The /dev/video* glob finds every video node the kernel exposes — including
// memory-to-memory codec nodes that cannot capture an external HDMI input.
// On the UNO Q the Qualcomm venus decoder registers /dev/video0 and
// /dev/video1 as "Qualcomm Venus video decoder" (driver qcom-venus): the
// first live device run counted those as "2 capture node(s)" while the
// hardware smoke correctly found no capture-capable node. The kernel ABI
// answer is per-node VIDIOC_QUERYCAP (device_caps): only a node whose
// capabilities include VIDEO_CAPTURE and include no memory-to-memory flag
// is capture-capable. v4l2-ctl -d <node> --info prints exactly those flags:
//
//	Capabilities     : 0x84204000
//		Video Memory-to-Memory Multiplanar
//		Streaming
//		Extended Pix Format
//		Device Capabilities
//
// for the venus nodes (recorded from the appliance, v4l-utils 1.30.1), and
// the "Video Capture" line instead for a UVC card.

// isCapabilityHeader marks the start of one capability list: a
// "Capabilities :" or "Device Caps :" line carrying the flag hex. The
// Device Caps list is VIDIOC_QUERYCAP.device_caps — the per-device
// capabilities — and the plain Capabilities line is .capabilities; both are
// printed and both may name the same flags, so scanning both is a superset
// that cannot add a flag the node does not have.
func isCapabilityHeader(line string) bool {
	if !strings.Contains(line, "0x") {
		return false
	}
	return strings.HasPrefix(line, "Capabilities") || strings.HasPrefix(line, "Device Caps")
}

// parseV4L2Capabilities reports, from one node's `v4l2-ctl -d <node> --info`
// stdout: capture (VIDEO_CAPTURE present, the M2M variant excluded), m2m
// (any memory-to-memory flag present) and driver (the card name, falling
// back to the driver name — the identity the ignored-node list shows the
// operator). An empty output or one without a capability list reports all
// false with the empty driver: the caller keeps such a node in the count
// (it cannot be proven non-capture) and records the failure instead.
func parseV4L2Capabilities(out []byte) (capture, m2m bool, driver string) {
	lines := strings.Split(string(out), "\n")
	card, driverName := "", ""
	inCaps := false
	for i := range lines {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		indented := strings.HasPrefix(line, "\t") || strings.HasPrefix(line, " ")
		if inCaps && !indented {
			// A non-indented line ends the current capability list; fall
			// through and re-classify this line (it may be the next header,
			// e.g. "Device Caps" under "Capabilities").
			inCaps = false
		}
		if isCapabilityHeader(trimmed) {
			inCaps = true
			continue
		}
		if inCaps {
			// An indented flag name under a capability header.
			flag := strings.ToLower(trimmed)
			switch {
			case strings.Contains(flag, "memory-to-memory") || strings.Contains(flag, "m2m"):
				m2m = true
			case strings.Contains(flag, "video capture"):
				// A plain capture flag: the M2M variants ("Video Capture
				// Memory-to-Memory", "Video Memory-to-Memory Multiplanar")
				// are caught by the first case above and must not count —
				// that node decodes, it does not capture.
				capture = true
			}
			continue
		}
		// Identity lines: the card name is the human identity, the driver
		// name the kernel module.
		switch {
		case strings.HasPrefix(trimmed, "Card type"):
			card = afterColon(trimmed)
		case strings.HasPrefix(trimmed, "Driver name") && driverName == "":
			driverName = afterColon(trimmed)
		}
	}
	driver = card
	if driver == "" {
		driver = driverName
	}
	return capture, m2m, driver
}

// afterColon extracts the value after the first colon of a
// "Field name      : value" v4l2-ctl line.
func afterColon(line string) string {
	i := strings.Index(line, ":")
	if i < 0 {
		return strings.TrimSpace(line)
	}
	return strings.TrimSpace(line[i+1:])
}
