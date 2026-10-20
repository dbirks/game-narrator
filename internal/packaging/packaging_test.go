package packaging

// Install-simulation for the ADR-0091 Bluetooth session state (the linger
// and the WirePlumber headless override in the maintainer scripts). The
// scripts target absolute appliance paths and root-owned ownership, so the
// test extracts the ADR-0091 block from each real script, rewrites the
// /var/lib/game-narrator prefix to a temp root, stubs the root-only verbs
// (loginctl, chown, chmod, the install -d subset) on PATH, and executes
// the block with /bin/sh under set -eu. A simulation, labelled as one: it
// proves the block's POSIX-sh validity, its idempotency, its only-if-missing
// contract and its byte-exact override contents — not the appliance's
// systemd (that is the hardware gate's job). The full scripts are
// additionally linted with sh -n.

import (
	"bufio"
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// overrideContent is the byte-exact recipe ADR 0084 verified on the real
// board (the sink appeared only with this file present).
const overrideContent = "wireplumber.profiles = { main = { monitor.bluez.seat-monitoring = disabled } }\n"

const overrideRelPath = ".config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf"

// repoRoot walks up from the test's package directory (internal/packaging)
// to the checkout root.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, this, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve the test file path")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(this)))
}

// extractBlock pulls the script body from the ADR-0091 marker comment up to
// the given terminator line (exclusive), so the simulation runs exactly the
// shipped lines — a drift in the script (the block lost or reordered) fails
// the test rather than the appliance install.
func extractBlock(t *testing.T, script, marker, terminator string) string {
	t.Helper()
	b, err := os.ReadFile(script)
	if err != nil {
		t.Fatalf("reading %s: %v", script, err)
	}
	text := string(b)
	start := strings.Index(text, marker)
	if start < 0 {
		t.Fatalf("%s: the ADR-0091 marker %q is missing (the block was lost?)", script, marker)
	}
	rest := text[start:]
	end := strings.Index(rest, terminator)
	if end < 0 {
		t.Fatalf("%s: the terminator %q is missing after the marker", script, terminator)
	}
	return rest[:end]
}

// stubRoot builds the temp root with the stubbed verbs on PATH.
func stubRoot(t *testing.T) (root, binDir string) {
	t.Helper()
	root = t.TempDir()
	binDir = filepath.Join(root, "stubbin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// loginctl: record the call (the assertion target).
	loginctl := "#!/bin/sh\necho \"$1 $2\" >> " + filepath.Join(root, "loginctl-calls") + "\n"
	if err := os.WriteFile(filepath.Join(binDir, "loginctl"), []byte(loginctl), 0o755); err != nil {
		t.Fatal(err)
	}
	// chown/chmod/systemctl: root-only or irrelevant to the assertions —
	// accept and succeed.
	for _, tool := range []string{"chown", "chmod", "systemctl"} {
		if err := os.WriteFile(filepath.Join(binDir, tool), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// install: the -d subset the block uses (ownership flags dropped — the
	// simulation asserts on the path and the file contents, not ownership).
	install := "#!/bin/sh\nwhile [ $# -gt 1 ]; do shift; done\nmkdir -p \"$1\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "install"), []byte(install), 0o755); err != nil {
		t.Fatal(err)
	}
	return root, binDir
}

// runBlock executes the extracted block as POSIX sh (set -eu, like the
// scripts themselves) against the temp root.
func runBlock(t *testing.T, root, binDir, block string) {
	t.Helper()
	block = strings.ReplaceAll(block, "/var/lib/game-narrator", root)
	sh := exec.Command("sh", "-c", "set -eu\n"+block)
	sh.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	sh.Stdout = &out
	sh.Stderr = &out
	if err := sh.Run(); err != nil {
		t.Fatalf("the block did not run cleanly:\n%s", out.String())
	}
}

func loginctlCalls(t *testing.T, root string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "loginctl-calls"))
	if err != nil {
		return ""
	}
	return string(b)
}

// TestPostinstAppliesTheBluetoothSessionState runs the deb postinst's
// ADR-0091 block: it enables linger (again on re-run) and writes the
// byte-exact headless override only when missing.
func TestPostinstAppliesTheBluetoothSessionState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, binDir := stubRoot(t)
	block := extractBlock(t,
		filepath.Join(repoRoot(t), "packaging/deb/postinst.sh"),
		"# The lingering user session for Bluetooth audio (ADR 0091)",
		"# Reload systemd")

	overridePath := filepath.Join(root, overrideRelPath)

	// First install: the file appears with the verified bytes.
	runBlock(t, root, binDir, block)
	got, err := os.ReadFile(overridePath)
	if err != nil {
		t.Fatalf("the headless override was not written: %v", err)
	}
	if string(got) != overrideContent {
		t.Fatalf("override content = %q, want the byte-exact verified recipe %q", got, overrideContent)
	}
	if calls := loginctlCalls(t, root); !strings.Contains(calls, "enable-linger game-narrator") {
		t.Fatalf("linger was not enabled (loginctl calls: %q)", calls)
	}

	// The admin-placed file is never clobbered (only-if-missing): plant a
	// different body, re-run the block (the upgrade path), and assert the
	// admin's bytes survive.
	if err := os.WriteFile(overridePath, []byte("# admin's own override\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runBlock(t, root, binDir, block)
	if got, _ := os.ReadFile(overridePath); string(got) != "# admin's own override\n" {
		t.Fatalf("the admin-placed override was clobbered: %q", got)
	}
}

// TestPostrmDisablesLinger runs the real deb postrm: the disable goes with
// the removal ($1=3), and the upgrade path does not touch the linger.
func TestPostrmDisablesLinger(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, binDir := stubRoot(t)
	postrm := filepath.Join(repoRoot(t), "packaging/deb/postrm.sh")
	env := append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out bytes.Buffer
	sh := exec.Command("sh", postrm, "3")
	sh.Env = env
	sh.Stdout = &out
	sh.Stderr = &out
	if err := sh.Run(); err != nil {
		t.Fatalf("postrm (remove) %v:\n%s", err, out.String())
	}
	if calls := loginctlCalls(t, root); !strings.Contains(calls, "disable-linger game-narrator") {
		t.Fatalf("linger was not disabled on removal (calls: %q)", calls)
	}

	// The upgrade path: no linger traffic at all (fresh call log).
	_ = os.Remove(filepath.Join(root, "loginctl-calls"))
	out.Reset()
	sh = exec.Command("sh", postrm, "upgrade")
	sh.Env = env
	sh.Stdout = &out
	sh.Stderr = &out
	if err := sh.Run(); err != nil {
		t.Fatalf("postrm (upgrade) %v:\n%s", err, out.String())
	}
	if calls := loginctlCalls(t, root); strings.Contains(calls, "disable") {
		t.Fatalf("the upgrade path touched the linger (calls: %q)", calls)
	}
}

// TestArchHooksCarryTheSameBlock is the ADR-0053 parity rule: the Arch
// hooks apply the same linger + override state as the deb scripts (both
// install and upgrade — the upgrade hook runs the body on every upgrade),
// and post_remove reverses the linger.
func TestArchHooksCarryTheSameBlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, binDir := stubRoot(t)
	dir := filepath.Join(repoRoot(t), "packaging/arch")
	installBlock := extractBlock(t,
		filepath.Join(dir, "post_install.sh"),
		"# The lingering user session for Bluetooth audio (ADR 0091)",
		"# Reload systemd")
	upgradeBlock := extractBlock(t,
		filepath.Join(dir, "post_upgrade.sh"),
		"# Bluetooth session state (ADR 0091)",
		"echo \"game-narrator: upgrade complete")

	for name, block := range map[string]string{"post_install.sh": installBlock, "post_upgrade.sh": upgradeBlock} {
		if !strings.Contains(block, "loginctl enable-linger game-narrator") {
			t.Fatalf("%s: the linger enable is missing from the ADR-0091 block:\n%s", name, block)
		}
		if !strings.Contains(block, "80-bluez-headless.conf") {
			t.Fatalf("%s: the headless override is missing from the ADR-0091 block", name)
		}
		runBlock(t, root, binDir, block)
	}
	got, err := os.ReadFile(filepath.Join(root, overrideRelPath))
	if err != nil || string(got) != overrideContent {
		t.Fatalf("the Arch hooks did not write the verified override (err %v): %q", err, got)
	}
	if calls := loginctlCalls(t, root); strings.Count(calls, "enable-linger") != 2 {
		t.Fatalf("expected the enable from both hooks (calls: %q)", calls)
	}
	rm, err := os.ReadFile(filepath.Join(dir, "post_remove.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rm), "loginctl disable-linger game-narrator") {
		t.Fatal("post_remove.sh: the linger disable is missing")
	}
}

// TestTmpfilesAndScriptsLint: the tmpfiles unit carries the conf.d line at
// 0700 for the service user, and every maintainer script still parses as
// POSIX sh (a syntax slip would fail the appliance install, not this
// repo's CI).
func TestTmpfilesAndScriptsLint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	tmp, err := os.ReadFile(filepath.Join(repoRoot(t), "packaging/tmpfiles.d/game-narrator.conf"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	sc := bufio.NewScanner(bytes.NewReader(tmp))
	for sc.Scan() {
		if strings.Contains(sc.Text(), "d /var/lib/game-narrator/.config/wireplumber/wireplumber.conf.d 0700 game-narrator:game-narrator") {
			found = true
		}
	}
	if !found {
		t.Errorf("tmpfiles: the conf.d 0700 line is missing:\n%s", tmp)
	}
	for _, p := range []string{
		"packaging/deb/postinst.sh",
		"packaging/deb/postrm.sh",
		"packaging/arch/post_install.sh",
		"packaging/arch/post_upgrade.sh",
		"packaging/arch/post_remove.sh",
		"packaging/scripts/audiodsp-recover.sh",
	} {
		full := filepath.Join(repoRoot(t), p)
		if out, err := exec.Command("sh", "-n", full).CombinedOutput(); err != nil {
			t.Errorf("sh -n %s: %v\n%s", p, err, out)
		}
	}
}
