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
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// thermalZones reads /sys/class/thermal/thermal_zone*/temp (millidegrees C)
// with the zone type from the zone's "type" file where present.
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
		mv, err := strconv.Atoi(strings.TrimSpace(string(b)))
		if err != nil {
			continue
		}
		typ := ""
		if tb, err := os.ReadFile(filepath.Join("/sys/class/thermal", e.Name(), "type")); err == nil {
			typ = strings.TrimSpace(string(tb))
		}
		out = append(out, fmt.Sprintf("%s(%s): %d mC", e.Name(), orDash(typ), mv/10))
	}
	return out
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

// moduleRoot is the repository root: this package lives at
// <root>/internal/hwsmoke. It reports an error rather than a test failure so
// the helper stays usable outside a test body.
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
