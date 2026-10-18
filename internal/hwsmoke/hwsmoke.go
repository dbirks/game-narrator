// Package hwsmoke is the executable form of docs/spec/10 "Hardware
// discovery and preflight" — the M8 hardware smoke gate behind
// `make test-hardware` (docs/spec/16: "hardware smoke and sustained play
// verified when actual hardware exists, otherwise explicitly NOT VERIFIED").
//
// Every test is named TestHW* and answers one preflight question with real
// evidence:
//
//   - TestHWCaptureDevice — enumerate V4L2 (never assuming /dev/video0),
//     record the negotiated formats, grab a fresh frame through the app's
//     supervised FFmpeg path (internal/capture/ffmpegpipe) and check it is
//     not blank, is fresh, and measure the dropped-frame rate over 10 s.
//   - TestHWAudioOutput — list PipeWire/ALSA sinks and record the sample
//     rate; the audible 440 Hz test tone is played only with explicit
//     consent (docs/spec/13: playback is a consented action).
//   - TestHWOCRControlledImage — run the real Tesseract engine
//     (internal/vision/ocr) over the controlled textbox image and bound the
//     character error rate.
//   - TestHWResourceBudget — RAM/CPU/temperature/storage preflight plus a
//     60 s run of the serve binary: the installed binary when one is
//     available (GAME_NARRATOR_BINARY, else game-narrator on PATH — the
//     appliance's .deb install), a build from the module root otherwise
//     (dev host); on the UNO Q profile (arm64, < 5 GB RAM) the serve RSS is
//     asserted against the documented budget in
//     docs/manuals/13-uno-q-hardware-smoke.md.
//   - TestHWPlatform — kernel/distro/arch report; the "is this the UNO Q
//     target" verdict is NOT RUN-explained on any other profile.
//
// Result vocabulary (scripts/gates/run-go-gate.sh convention): a test that
// cannot run because the hardware or consent is absent calls notRun, which
// skips with "NOT RUN: <exact missing thing>; <how to attach/configure
// it>". The gate summariser prints that as the setup instruction and exits
// 2. Nothing in this package passes vacuously: a PASS always means the
// named check measured something on this host, and every measurement is
// logged with t.Logf so the PASS/FAIL/NOT RUN line is evidence-backed.
//
// This package is read-only with respect to the rest of the tree: it drives
// the existing doctor-checked tools and the capture, speech and OCR
// packages — it adds no app behaviour. It runs on whatever host
// `make test-hardware` is invoked on (the Arch dev host, the single-node
// box, or the UNO Q itself): docs/spec/10 preflight is a discovery
// procedure, not a test that only makes sense on one machine. On the UNO Q
// the gate is run by scripts/hw/run-on-device.sh, which cross-compiles this
// package's test binary, copies it to the appliance's /tmp and runs it
// there with the service environment loaded (docs/manuals/13).
//
// CI rule: TestHW* never runs inside a `go test ./...` sweep. Every test
// calls hwGate first and skips as NOT RUN unless GAME_NARRATOR_HW_SMOKE=1
// is set; the two sanctioned runners (the Makefile test-hardware target and
// scripts/hw/run-on-device.sh) both set it. The un-gated unit tests in this
// package (the thermal-conversion and serve-binary-resolution tables) keep
// running in the sweep: they are host-agnostic and cheap.
package hwsmoke

import (
	"fmt"
	"image"
	"math"
	"runtime"
	"testing"

	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Environment switches. The consent variable is new to this package; the
// capture-device and audio-device names reuse what the app already
// documents so one operator decision applies to the whole machine
// (GAME_NARRATOR_AUDIO_DEVICE is the named PipeWire sink in the serve
// docs, cmd/game-narrator "Audio" section).
const (
	// EnvCaptureDevice names the capture node to smoke: an exact /dev/video*
	// path, or a udev stable name (the /dev/v4l/by-id or /dev/v4l/by-path
	// symlink, by full path or base name). docs/spec/10: matching via USB
	// VID/PID and stable path, never assuming /dev/video0.
	EnvCaptureDevice = "GAME_NARRATOR_CAPTURE_DEVICE"
	// EnvAudioConsent must be exactly "1" for the audible test tone to play.
	// Absent or any other value: the audio check skips as NOT RUN with the
	// export instruction (docs/spec/13 — a diagnostic that hoots a living
	// room on every run is not a diagnostic).
	EnvAudioConsent = "GAME_NARRATOR_HW_AUDIO_CONSENT"
	// EnvAudioDevice is the app's named PipeWire sink for playback. Unset
	// means the first sink the host enumerates.
	EnvAudioDevice = "GAME_NARRATOR_AUDIO_DEVICE"
	// EnvBinary names the serve binary the resource-budget session runs.
	// Unset: the game-narrator binary on PATH (the appliance's .deb
	// install), else a build from the module root (dev host).
	EnvBinary = "GAME_NARRATOR_BINARY"
	// EnvSmokeGate must be exactly "1" for any TestHW* to run at all.
	// Absent or any other value: every hardware smoke test skips as
	// "NOT RUN: hardware smoke runs via make test-hardware /
	// scripts/hw/run-on-device.sh" so `go test ./...` (and CI's
	// go test -race ./...) never drives real capture hardware, real
	// Tesseract or a 60 s serve session. The Makefile test-hardware
	// target and scripts/hw/run-on-device.sh are the two sanctioned
	// runners and both set it.
	EnvSmokeGate = "GAME_NARRATOR_HW_SMOKE"
)

// serveRSSBudgetMiB is the documented serve-process RSS budget the UNO Q
// profile asserts (docs/manuals/13-uno-q-hardware-smoke.md, "Resource
// budget"). 512 MiB for the whole serve process — HTTP app, preview
// thumbnails, OCR worker, speech cache — leaves the rest of the UNO Q's
// 4 GB to the OS and headroom (docs/spec/10: "UNO Q 4 GB must remain
// stable when handling captured frame previews, OCR and HTTP app
// simultaneously").
const serveRSSBudgetMiB = 512

// unoQRAMCeilingGiB is the profile cut: a host is "the UNO Q profile" for
// the RSS assertion when it is arm64 with strictly less than 5 GB of RAM
// (the UNO Q ships 4 GB; docs/spec/10).
const unoQRAMCeilingGiB = 5

// notRun skips with the gate's NOT RUN vocabulary: the summariser treats a
// SKIP whose message starts with "NOT RUN" as NOT RUN (exit 2 overall,
// never green) and prints the message as the setup instruction. missing is
// the exact absent thing; how is the attach/configure step.
func notRun(t *testing.T, missing, how string) {
	t.Helper()
	t.Skip("NOT RUN: " + missing + "; " + how)
}

// lumaEntropy returns the Shannon entropy (bits) of the frame's luma
// histogram plus the number of distinct luma values. A blank or dead frame
// (all black, all white, one solid colour) scores ~0; a live sensor — even
// one pointed at a wall — has quantisation and compression noise spread
// over many luma values and scores well above 1 bit. The non-blank
// threshold lives in the capture test, next to the values it guards.
func lumaEntropy(img image.Image) (entropy float64, distinct int) {
	r := img.Bounds()
	hist := make([]int, 256)
	total := 0
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			red, green, blue, _ := img.At(x, y).RGBA()
			// 8-bit luma from the 16-bit sRGB components (BT.601 weights).
			luma := uint8((66*uint16(red) + 129*uint16(green) + 25*uint16(blue) + 128) >> 8)
			hist[luma]++
			total++
		}
	}
	// 1/8 stride over the histogram rows is not a thing — the histogram is
	// already the summary; count distinct values and Shannon entropy.
	for _, n := range hist {
		if n > 0 {
			distinct++
		}
	}
	if total == 0 {
		return 0, 0
	}
	e := 0.0
	for _, n := range hist {
		if n == 0 {
			continue
		}
		p := float64(n) / float64(total)
		e -= p * math.Log2(p)
	}
	return e, distinct
}

// isUNOQProfile reports whether this host matches the UNO Q profile the
// RSS budget assertion applies to: arm64 and < 5 GB RAM. memTotalKiB is
// /proc/meminfo MemTotal (0 when unreadable — never the profile).
func isUNOQProfile(memTotalKiB uint64) bool {
	return runtime.GOARCH == "arm64" &&
		memTotalKiB > 0 && memTotalKiB < uint64(unoQRAMCeilingGiB)*1024*1024
}

// cer is the character error rate of got against want, computed in the
// NormaliseForCompare form (case-folded, letters and digits only — the
// comparison form ADR-0039 §4 defines for reading discrepancies). CER is
// edit-distance / length-of-reference, the standard OCR metric: an exact
// read is 0, one wrong character in "NARRATOR 42" is ~5.3 %.
func cer(got, want string) float64 {
	g := ocr.NormaliseForCompare(got)
	w := ocr.NormaliseForCompare(want)
	if len(w) == 0 {
		return 0
	}
	return float64(levenshtein(g, w)) / float64(len(w))
}

// levenshtein is the classic two-row dynamic-programming edit distance.
// The inputs are OCR readings of a controlled string — tens of runes — so
// the cost is noise.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}

// round1 rounds x to one decimal place for log lines (fmt %.1f exists, but
// a helper keeps the call sites readable and unit-consistent).
func round1(x float64) float64 { return math.Round(x*10) / 10 }

// miB formats a kibibyte count as a MiB string for log lines.
func miB(kiB uint64) string { return fmt.Sprintf("%.0f MiB", float64(kiB)/1024) }
