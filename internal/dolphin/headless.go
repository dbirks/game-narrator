package dolphin

import (
	"context"
	"errors"
	"strings"
)

// errProbeOutput is returned when the display-free probe ran but printed
// nothing: the binary was reached (no exec or abort failure) yet produced no
// version banner, which no known build does.
var errProbeOutput = errors.New("dolphin-emu probe produced no output")

// ProbeArgs is the argv of the display-free CLI probe of the dolphin-emu
// binary (issue #62). The only CLI surface the tooling probes outside a live
// bridge session is the version banner; the full CLI surface (and any real
// session) goes through the bridge, which owns its own display.
func ProbeArgs() []string { return []string{"--version"} }

// ProbeEnv is the environment a dolphin-emu CLI probe must run under when no
// display server is available. dolphin-emu 2609 core-dumps (abort, exit 134,
// no output) on --version/--help without a display because it initialises the
// Qt windowing stack before CLI argument handling (issue #62, dev host, build
// 2609). QT_QPA_PLATFORM=offscreen was measured on that host: exit 0,
// "Dolphin 2609" — and it avoids spawning a private Xvfb just to read a
// banner.
func ProbeEnv() []string { return []string{"QT_QPA_PLATFORM=offscreen"} }

// RunProbe runs one command with explicit env additions; the probe helper
// and the doctor's Exec seam both wrap an implementation of this.
type RunProbe func(ctx context.Context, name string, args []string, env []string) (stdout string, err error)

// HeadlessVersion probes the emulator binary's version banner without any
// display server: it runs the binary with ProbeArgs under ProbeEnv and
// returns the first non-empty output line (e.g. "Dolphin 2609"). The argv and
// env contract is asserted by headless_test.go so a future change that drops
// the offscreen platform (and restores the core dump) fails the suite.
func HeadlessVersion(ctx context.Context, bin string, run RunProbe) (string, error) {
	out, err := run(ctx, bin, ProbeArgs(), ProbeEnv())
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t, nil
		}
	}
	return "", errProbeOutput
}
