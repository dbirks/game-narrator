package hwsmoke

// Host-probe helpers for the resource-budget smoke: the /proc and /sys
// readers, the serve-session process plumbing and the bounded output
// capture. Kept out of the test file so the test body stays a straight
// narrative of the docs/spec/10 preflight steps.

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// processRSSMiB reads VmRSS from /proc/<pid>/status — the kernel's own
// number, no ps/parsing of a man page in between. ok is false when /proc
// has no entry (process gone) or the file cannot be read.
func processRSSMiB(pid int) (int, bool) {
	f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "VmRSS:") {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 {
				v, err := strconv.Atoi(fields[1])
				if err == nil {
					return v / 1024, true
				}
			}
			return 0, false
		}
	}
	return 0, false
}

// memTotalKiBFromProc reads MemTotal kB from /proc/meminfo.
func memTotalKiBFromProc() uint64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				v, _ := strconv.ParseUint(fields[1], 10, 64)
				return v
			}
		}
	}
	return 0
}

// loadAverage parses /proc/loadavg (1/5/15 min).
func loadAverage() [3]float64 {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return [3]float64{}
	}
	var out [3]float64
	fields := strings.Fields(string(b))
	for i := 0; i < 3 && i < len(fields); i++ {
		out[i], _ = strconv.ParseFloat(fields[i], 64)
	}
	return out
}

func loadString(l [3]float64) string {
	if l[0] == 0 && l[1] == 0 && l[2] == 0 {
		return "unrecorded"
	}
	return fmt.Sprintf("%.1f/%.1f/%.1f", l[0], l[1], l[2])
}

// thermalZones reads /sys/class/thermal/thermal_zone*/temp with the zone
// type from the zone's "type" file where present. The reading is converted
// to degrees Celsius through thermalCelsius, which is unit-aware: this log
// line must never print an unconverted raw value (the first UNO Q run read
// 3590 and logged "3590 mC" = 3.6 °C — the zone's real 35.9 °C).
func thermalZones() []string {
	entries, err := os.ReadDir("/sys/class/thermal")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "thermal_zone") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/sys/class/thermal", e.Name(), "temp"))
		if err != nil {
			continue
		}
		raw, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			continue
		}
		typ := ""
		if tb, err := os.ReadFile(filepath.Join("/sys/class/thermal", e.Name(), "type")); err == nil {
			typ = strings.TrimSpace(string(tb))
		}
		c := thermalCelsius(raw)
		var label string
		if c.ok {
			label = fmt.Sprintf("%.1f °C", c.celsius)
			if c.rawAsIs {
				label += " (raw value already in °C)"
			}
		} else {
			label = fmt.Sprintf("unit unknown (raw %d is out of 0–120 °C under every supported unit)", raw)
		}
		out = append(out, fmt.Sprintf("%s(%s): %s", e.Name(), orDash(typ), label))
	}
	return out
}

// thermalReading is the converted form of one raw /sys/class/thermal temp
// value: the plausibility-checked temperature, whether the raw value was
// already in °C, and whether the unit could be determined at all.
type thermalReading struct {
	celsius float64
	rawAsIs bool
	ok      bool
}

// thermalFloorCelsius is the lower plausibility bound for a millidegrees
// reading: a powered SoC sensor sits at or above ambient, and a millidegree
// value below 5 °C names a sensor colder than any ambient this appliance is
// run in — which is the signature of the wrong unit, not of a cold room.
// (The 0.01 °C readings have no such floor: 35.9 °C and 4 °C are both
// plausible centidegree temperatures, so that case falls through to the
// 0–120 °C band instead.)
const thermalFloorCelsius = 5.0

// thermalCelsius converts one raw temp value to degrees Celsius. The kernel
// ABI (Documentation/ABI/testing/sysfs-class-thermal) fixes the unit at
// millidegrees, but platform firmware can disagree with the ABI: the UNO
// Q's Qualcomm SoC zones have been observed reporting 0.01 °C values (raw
// 3590 = 35.9 °C idle — plausible), while other zones on the same machine
// report millidegrees (33700 = 33.7 °C — Arduino forum evidence for the
// same 10-zone layout). The conversion therefore:
//
//  1. takes the raw value as-is when it is already a plausible Celsius
//     temperature (0–120); some zones report °C directly;
//  2. otherwise reads it as millidegrees — the documented unit — when the
//     result is plausible (thermalFloorCelsius–120 °C);
//  3. otherwise reads it as 0.01 °C when that is plausible (the observed
//     UNO Q unit);
//  4. otherwise reports the unit as unknown rather than inventing a
//     number: a diagnostic that prints 3.6 °C for a 35.9 °C SoC is
//     worse than one that says the unit is unknown.
//
// The plausibility bound is 0–120 °C: every zone on a handheld SoC that is
// meant to be read by a thermal guard sits in that band when the device is
// powered, and nothing above it is a sensor reading a maintenance tool
// should quote.
func thermalCelsius(raw int) thermalReading {
	const hi = 120.0
	if v := float64(raw); v >= 0 && v <= hi {
		return thermalReading{celsius: v, rawAsIs: true, ok: true}
	}
	if v := float64(raw) / 1000; v >= thermalFloorCelsius && v <= hi {
		return thermalReading{celsius: v, ok: true}
	}
	if v := float64(raw) / 100; v >= 0 && v <= hi {
		return thermalReading{celsius: v, ok: true}
	}
	return thermalReading{}
}

// freeSpaceKiB reports the unprivileged-free space of dataDir's filesystem
// (Bavail, not Bfree — the number a process without CAP_SYS_ADMIN can use).
func freeSpaceKiB(dir string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Frsize) / 1024, nil
}

// serveBinary resolves the serve binary the 60 s budget session runs, in
// the documented precedence:
//
//  1. GAME_NARRATOR_BINARY — an explicit path, when the test is pinned to a
//     particular build;
//  2. game-narrator on PATH — the installed appliance binary. On the UNO Q
//     this is the .deb's /usr/bin/game-narrator, so the smoke budgets the
//     binary the appliance actually runs, not a throwaway build;
//  3. a fresh build of ./cmd/game-narrator from the module root — the dev
//     host, where nothing is installed and the repo's testdata fixtures are
//     available as the session's replay roots.
//
// When the binary comes from 1 or 2 the replay roots are left at the
// production default (none): pointing them at a fixture path the appliance
// does not have would only be rejected input, and the session's RSS is the
// measurement, not the replay traffic. When none of the three is available
// missing explains the NOT RUN: the session cannot run without a binary.
func serveBinary(t *testing.T) (bin, replayRoot, missing string) {
	t.Helper()
	if p := strings.TrimSpace(os.Getenv(EnvBinary)); p != "" {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			return p, "", ""
		}
		return "", "", fmt.Sprintf("%s is set to %q, which does not name an existing file", EnvBinary, p)
	}
	if p, err := exec.LookPath("game-narrator"); err == nil {
		return p, "", ""
	}
	root, err := moduleRoot()
	if err != nil {
		return "", "", fmt.Sprintf(
			"no serve binary to budget: %s is unset, game-narrator is not on PATH, and the module root cannot be resolved (%v)",
			EnvBinary, err)
	}
	bin = filepath.Join(t.TempDir(), "game-narrator")
	build := exec.Command("go", "build", "-o", bin, "./cmd/game-narrator")
	build.Dir = root
	var buildOut bytes.Buffer
	build.Stdout = &buildOut
	build.Stderr = &buildOut
	if err := build.Run(); err != nil {
		return "", "", fmt.Sprintf("go build of ./cmd/game-narrator failed: %v (output tail: %s)",
			err, tail(buildOut.Bytes(), 400))
	}
	t.Logf("budget session: binary built at %s", bin)
	return bin, filepath.Join(root, "testdata"), ""
}

// moduleRoot is the repository root: this package lives at
// <root>/internal/hwsmoke. It reports an error rather than a test failure so
// the helper stays usable outside a test body. It is only reached for the
// dev-host build path of serveBinary — on the appliance the installed
// binary is used and the module root need not exist.
func moduleRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root := filepath.Join(filepath.Dir(filepath.Dir(wd)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		return "", err
	}
	return root, nil
}

// boundedWriter is an io.Writer that stops recording past cap (keeping the
// first cap bytes), so a talkative child cannot grow the test without bound.
type boundedWriter struct {
	buf *bytes.Buffer
	cap int
}

func (w *boundedWriter) Write(p []byte) (int, error) {
	if w.buf.Len() < w.cap {
		w.buf.Write(p[:min(len(p), w.cap-w.buf.Len())])
	}
	return len(p), nil
}

func tail(b []byte, n int) string {
	s := string(b)
	if len(s) > n {
		s = "…" + s[len(s)-n:]
	}
	return s
}
