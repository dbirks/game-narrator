package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePolicy fabricates one cpufreq policy directory under the fake sysfs
// root. available is the whitespace-separated scaling_available_frequencies
// content; an empty string omits the file.
func writePolicy(t *testing.T, root, name, governor, min, max, cur, available string) {
	t.Helper()
	dir := filepath.Join(root, "devices", "system", "cpu", "cpufreq", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"scaling_governor":              governor,
		"scaling_min_freq":              min,
		"scaling_max_freq":              max,
		"scaling_cur_freq":              cur,
		"scaling_available_frequencies": available,
	} {
		if content == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// fakeCpufreqRoot builds a sysfs tree with one policy and, when zones > 0,
// that many thermal zones, and returns the root.
func fakeCpufreqRoot(t *testing.T, governor, min, max, cur, available string, zones int) string {
	t.Helper()
	root := t.TempDir()
	if governor != "" {
		writePolicy(t, root, "policy0", governor, min, max, cur, available)
	}
	for i := 0; i < zones; i++ {
		zdir := filepath.Join(root, "class", "thermal", "thermal_zone"+string(rune('0'+i)))
		if err := os.MkdirAll(zdir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(zdir, "temp"), []byte("45200"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func cpufreqCheck(t *testing.T, root string) Check {
	t.Helper()
	r := newRunner(Options{SysfsRoot: root})
	return r.checkCPUGovernor(context.Background())
}

const availFreqs = "614400 806400 1000000 1300000 1600000 2016000"

func TestCPUGovernorNotConfiguredWithoutSysfs(t *testing.T) {
	c := cpufreqCheck(t, t.TempDir())
	if c.Status != StatusNotConfigured {
		t.Fatalf("status = %s, want NOT CONFIGURED: %+v", c.Status, c)
	}
}

func TestCPUGovernorPerformanceOK(t *testing.T) {
	root := fakeCpufreqRoot(t, "performance", "2016000", "2016000", "2016000", availFreqs, 1)
	c := cpufreqCheck(t, root)
	if c.Status != StatusOK {
		t.Fatalf("status = %s, want OK: %+v", c.Status, c)
	}
	if !strings.Contains(c.Summary, "1 cpufreq policy") {
		t.Errorf("summary = %q", c.Summary)
	}
	if !strings.Contains(c.Detail, "thermal: zone0=45.2C") {
		t.Errorf("detail = %q, want the thermal sample", c.Detail)
	}
}

func TestCPUGovernorParkedAtFloorIsDegraded(t *testing.T) {
	// The community case: a non-performance governor with the floor at the
	// policy minimum — the 614 MHz regime that caps the UNO Q at ~7.5 fps.
	root := fakeCpufreqRoot(t, "powersave", "614400", "2016000", "614400", availFreqs, 0)
	c := cpufreqCheck(t, root)
	if c.Status != StatusDegraded {
		t.Fatalf("status = %s, want DEGRADED: %+v", c.Status, c)
	}
	if !strings.Contains(c.Remedy, "cpu-tune.sh") {
		t.Errorf("remedy = %q, must name scripts/board/cpu-tune.sh", c.Remedy)
	}
}

func TestCPUGovernorSchedutilWithRaisedFloorIsOK(t *testing.T) {
	// The accepted tuning from issue #58: schedutil with scaling_min_freq
	// above the floor — the clock can no longer park in the slow regime.
	root := fakeCpufreqRoot(t, "schedutil", "1600000", "2016000", "1800000", availFreqs, 0)
	c := cpufreqCheck(t, root)
	if c.Status != StatusOK {
		t.Fatalf("status = %s, want OK: %+v", c.Status, c)
	}
}

func TestCPUGovernorSchedutilAtFloorIsDegraded(t *testing.T) {
	root := fakeCpufreqRoot(t, "schedutil", "614400", "2016000", "614400", availFreqs, 0)
	c := cpufreqCheck(t, root)
	if c.Status != StatusDegraded {
		t.Fatalf("status = %s, want DEGRADED (schedutil can park at the floor): %+v", c.Status, c)
	}
}

func TestCPUGovernorMultiplePolicies(t *testing.T) {
	root := t.TempDir()
	writePolicy(t, root, "policy0", "schedutil", "1600000", "2016000", "1600000", availFreqs)
	writePolicy(t, root, "policy1", "performance", availFreqs, availFreqs, "2016000", availFreqs)
	c := cpufreqCheck(t, root)
	if c.Status != StatusOK {
		t.Fatalf("status = %s, want OK: %+v", c.Status, c)
	}
	if !strings.Contains(c.Summary, "2 cpufreq policy") {
		t.Errorf("summary = %q", c.Summary)
	}
}
