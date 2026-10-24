package doctor

// The system.cpu_governor row (issue #58 / ADR-0183): a read-only look at the
// CPU DVFS state the UNO Q capture path actually runs under. Community
// characterization of the QRB2210 shows a light periodic workload (our ~10 fps
// tick with short bursts) leaving the frequency policy modal at its floor —
// ~614 MHz, a ~6x step-time difference from the 2.016 GHz full-clock regime —
// while free-running work holds the top clock. So this check does not ask
// "is the CPU fast" (it is, at full load); it asks "can the governor park the
// clock at the policy floor during a session", and degrades with a remedy
// when it can.
//
// The check is read-only by contract: it only reads sysfs and never writes a
// governor or frequency file. The fix is the one-shot, dry-run-by-default
// scripts/board/cpu-tune.sh (docs/manuals/19-install-on-uno-q.md "CPU
// frequency tuning"), which this remedy names.
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// governorHoldsClock reports whether the named governor keeps the clock high
// enough that the policy floor never gates a session. "performance" pins the
// maximum clock outright; every other governor (schedutil, powersave,
// ondemand, userspace) can idle the policy down to scaling_min_freq, which is
// why the DEGRADED condition below is "min equals the available floor AND the
// governor is not performance" — the raised-floor schedutil configuration
// (min_freq above the floor) is the other accepted tuning from the issue.
func governorHoldsClock(governor string) bool {
	return strings.TrimSpace(governor) == "performance"
}

// readSysfsInt reads one small sysfs number file (kHz or millidegrees C).
func readSysfsInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// cpufreqPolicy captures one policy's DVFS state, kHz.
type cpufreqPolicy struct {
	name     string
	governor string
	minK     int
	maxK     int
	curK     int
	floorK   int // lowest entry in scaling_available_frequencies; -1 if unreadable
}

func (r *runner) cpufreqPolicies() []cpufreqPolicy {
	pattern := filepath.Join(r.opts.SysfsRoot, "devices", "system", "cpu", "cpufreq", "policy*")
	dirs, _ := filepath.Glob(pattern)
	var out []cpufreqPolicy
	for _, dir := range dirs {
		governor, err := os.ReadFile(filepath.Join(dir, "scaling_governor"))
		if err != nil {
			continue
		}
		p := cpufreqPolicy{
			name:     filepath.Base(dir),
			governor: strings.TrimSpace(string(governor)),
			floorK:   -1,
		}
		if v, err := readSysfsInt(filepath.Join(dir, "scaling_min_freq")); err == nil {
			p.minK = v
		}
		if v, err := readSysfsInt(filepath.Join(dir, "scaling_max_freq")); err == nil {
			p.maxK = v
		}
		if v, err := readSysfsInt(filepath.Join(dir, "scaling_cur_freq")); err == nil {
			p.curK = v
		}
		if avail, err := os.ReadFile(filepath.Join(dir, "scaling_available_frequencies")); err == nil {
			for _, tok := range strings.Fields(string(avail)) {
				if f, err := strconv.Atoi(tok); err == nil && (p.floorK == -1 || f < p.floorK) {
					p.floorK = f
				}
			}
		}
		out = append(out, p)
	}
	return out
}

// thermalSample returns "zone0=45.2C zone1=44.8C" for the readable zones, or
// "" when the host exposes no thermal zones (the check must not depend on
// them: the UNO Q has headroom — 80.2 C peak over a 10-minute soak, no
// throttling observed — so temperature is context, not a gate).
func (r *runner) thermalSample() string {
	pattern := filepath.Join(r.opts.SysfsRoot, "class", "thermal", "thermal_zone*", "temp")
	zones, _ := filepath.Glob(pattern)
	var parts []string
	for i, z := range zones {
		milliC, err := readSysfsInt(z)
		if err != nil {
			continue
		}
		parts = append(parts, fmt.Sprintf("zone%d=%sC", i, strconv.FormatFloat(float64(milliC)/1000, 'f', 1, 32)))
	}
	return strings.Join(parts, " ")
}

func (r *runner) checkCPUGovernor(_ context.Context) Check {
	policies := r.cpufreqPolicies()
	if len(policies) == 0 {
		return Check{
			Status:  StatusNotConfigured,
			Summary: "no cpufreq policies visible (sysfs absent or no cpufreq driver)",
		}
	}
	var lines []string
	var parked []string
	for _, p := range policies {
		lines = append(lines, fmt.Sprintf("%s: governor=%s min=%d kHz max=%d kHz cur=%d kHz",
			p.name, p.governor, p.minK, p.maxK, p.curK))
		if !governorHoldsClock(p.governor) && p.floorK >= 0 && p.minK == p.floorK {
			parked = append(parked, fmt.Sprintf("%s (governor %s can park at the %d kHz floor)", p.name, p.governor, p.floorK))
		}
	}
	c := Check{
		Status:  StatusOK,
		Summary: fmt.Sprintf("%d cpufreq policy(ies) hold the session above the floor", len(policies)),
		Detail:  strings.Join(lines, "; "),
	}
	if t := r.thermalSample(); t != "" {
		c.Detail += "; thermal: " + t
	}
	if len(parked) > 0 {
		c.Status = StatusDegraded
		c.Summary = "CPU can park at the policy floor during a session: " + strings.Join(parked, ", ")
		c.Remedy = "raise the clock floor for the capture session: scripts/board/cpu-tune.sh --apply (schedutil with a raised scaling_min_freq, or the performance governor); see docs/manuals/19-install-on-uno-q.md"
	}
	return c
}
