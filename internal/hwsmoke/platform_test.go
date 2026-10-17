package hwsmoke

// TestHWPlatform reports the host identity docs/spec/10's preflight records
// ("Linux/kernel version … check actual Debian packages available on UNO Q
// image") and answers the deployment question: is this host the UNO Q
// target — Debian arm64 (docs/spec/10 "UNO Q software deployment: Go binary
// uses Linux arm64")?
//
// The generic checks (kernel, distro, arch) always run and are logged on
// every host. The target verdict is only meaningful on the target itself,
// so on any other profile the test skips as NOT RUN with the measured host
// identity in the explanation: the gate table never claims the deployment
// platform was verified where it was not.

import (
	"os"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestHWPlatform(t *testing.T) {
	kernel := kernelRelease()
	osrel := parseOSRelease()
	arch := runtime.GOARCH + "/" + runtime.GOOS

	identity := osrel.identity()
	t.Logf("platform: kernel=%s distro=%q arch=%s", kernel, identity, arch)

	if osrel.ID != "debian" || arch != "arm64/linux" {
		// The verdict is NOT RUN-explained on non-target hosts (the generic
		// checks above still ran). The UNO Q image is Debian and keys on
		// ID=, not ID_LIKE=: a derivative that only claims "like debian" is
		// not the release target (docs/spec/10).
		notRun(t, "this host is not the Debian arm64 UNO Q target platform",
			"run make test-hardware on the UNO Q itself (docs/spec/10); "+
				"this host is "+identity+" "+arch)
	}
	t.Logf("platform: UNO Q target platform verified (Debian arm64, kernel %s)", kernel)
}

// osRelease is the /etc/os-release fields the preflight keys on.
type osRelease struct {
	Pretty  string
	ID      string
	Version string
}

func (o osRelease) identity() string {
	p := o.Pretty
	if p == "" {
		p = o.ID
	}
	if o.Version != "" {
		return p + " " + o.Version
	}
	if p == "" {
		return "unavailable (/etc/os-release not readable)"
	}
	return p
}

// parseOSRelease reads /etc/os-release into the fields the preflight keys
// on. A missing file yields the zero value, which identity() renders as
// "unavailable" — the log line then carries the operator-facing evidence.
func parseOSRelease() osRelease {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return osRelease{}
	}
	var o osRelease
	for _, line := range strings.Split(string(b), "\n") {
		switch {
		case strings.HasPrefix(line, "PRETTY_NAME="):
			o.Pretty = unquote(strings.TrimPrefix(line, "PRETTY_NAME="))
		case strings.HasPrefix(line, "ID="):
			o.ID = strings.TrimPrefix(line, "ID=")
		case strings.HasPrefix(line, "VERSION_ID="):
			o.Version = unquote(strings.TrimPrefix(line, "VERSION_ID="))
		}
	}
	return o
}

// kernelRelease returns the running kernel release (uname -r) without
// shelling out: the same field the preflight records.
func kernelRelease() string {
	var uts syscall.Utsname
	if err := syscall.Uname(&uts); err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	return byteString(uts.Release[:])
}

func byteString(b []int8) string {
	out := make([]byte, 0, len(b))
	for _, c := range b {
		if c == 0 {
			break
		}
		out = append(out, byte(c))
	}
	return string(out)
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
