package packaging

// ADR-0118 (issue #24) packaging tests:
//
//  1. Install simulation of the deb postinst's first-install-only enablement
//     of game-narrator-audiodsp-recover.service (the ADR 0081 contract): the
//     real block from the shipped script runs under set -eu against a stub
//     systemctl that logs its calls — an upgrade must not re-enable.
//
//  2. Shell tests of the helper's decision logic (packaging/scripts/
//     audiodsp-recover.sh) against fake /proc and /sys trees via its
//     GN_AUDIO_* env overrides: non-UNO Q no-op, healthy-boot no-op,
//     no-signature no-op, the bounded one-shot ADSP restart, and the
//     success path when the card appears during the wait. A simulation,
//     labelled as one: it proves the decision logic and the boundedness —
//     the real remoterproc stop/start on the board is the hardware gate's
//     job (AGENTS.md rule 3).

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func audioDSPScript(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "packaging/scripts/audiodsp-recover.sh")
}

// runPostinstBlock executes the extracted postinst block with the maintainer
// script argument (install | upgrade) as $1, stubbing systemctl on PATH so
// the simulation never touches the host's systemd.
func runPostinstBlock(t *testing.T, root, binDir, block, maintArg string) string {
	t.Helper()
	block = strings.ReplaceAll(block, "/var/lib/game-narrator", root)
	sh := exec.Command("sh", "-c", "set -eu\n"+block, "sh", maintArg)
	sh.Env = append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out bytes.Buffer
	sh.Stdout = &out
	sh.Stderr = &out
	if err := sh.Run(); err != nil {
		t.Fatalf("the block did not run cleanly ($1=%s):\n%s", maintArg, out.String())
	}
	b, _ := os.ReadFile(filepath.Join(root, "systemctl-calls"))
	return string(b)
}

// TestPostinstEnablesTheAudioDSPRecoverOnFirstInstall runs the deb postinst's
// boot-start block: on a fresh install BOTH the service and the audio-DSP
// recover oneshot get enabled (ADR 0081 contract, ADR 0118); on an upgrade
// neither enable runs, so an admin-disabled unit stays disabled.
func TestPostinstEnablesTheAudioDSPRecoverOnFirstInstall(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, binDir := stubRoot(t)
	// The stub systemctl logs its arguments (assertion target).
	if err := os.WriteFile(filepath.Join(binDir, "systemctl"),
		[]byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+mustJoin(root, "systemctl-calls")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	block := extractBlock(t,
		filepath.Join(repoRoot(t), "packaging/deb/postinst.sh"),
		"# Boot start: the appliance's product is this service",
		`echo "game-narrator: install complete`)

	// Fresh install: both units enabled.
	calls := runPostinstBlock(t, root, binDir, block, "install")
	if !strings.Contains(calls, "enable game-narrator.service") {
		t.Fatalf("the service enable is missing (calls: %q)", calls)
	}
	if !strings.Contains(calls, "enable game-narrator-audiodsp-recover.service") {
		t.Fatalf("the audio-DSP recover enable is missing (calls: %q)", calls)
	}

	// Upgrade: no enablement traffic at all.
	_ = os.Remove(filepath.Join(root, "systemctl-calls"))
	calls = runPostinstBlock(t, root, binDir, block, "upgrade")
	if strings.Contains(calls, "enable") {
		t.Fatalf("the upgrade path re-enabled a unit (calls: %q)", calls)
	}
}

// mustJoin is filepath.Join without the (t) helper context; kept tiny so the
// stub-content string stays one line.
func mustJoin(parts ...string) string { return filepath.Join(parts...) }

// fakeAudioDSPHost builds the fake tree the helper reads:
//
//	<root>/proc/device-tree/compatible  (UNO Q or not)
//	<root>/proc/asound/cards            (with or without the HPH card)
//	<root>/sys/class/remoteproc/...     (the adsp remoteproc, or none)
//	<root>/dmesg                        (with or without the signature)
//
// and returns the cards path so a test can make the card appear mid-wait.
func fakeAudioDSPHost(t *testing.T, unoQ bool, cardPresent bool, withADSP bool, dmesgSignature bool) (root, cardsPath string) {
	t.Helper()
	root = t.TempDir()
	proc := filepath.Join(root, "proc")
	sys := filepath.Join(root, "sys")
	for _, d := range []string{
		filepath.Join(proc, "device-tree"),
		filepath.Join(proc, "asound"),
		filepath.Join(sys, "class", "remoteproc", "remoteproc1"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	compat := "qcom,qcm2290\x00qcom,qrb2210\x00"
	if unoQ {
		compat = "arduino,imola\x00" + compat
	}
	if err := os.WriteFile(filepath.Join(proc, "device-tree", "compatible"), []byte(compat), 0o444); err != nil {
		t.Fatal(err)
	}
	cardsPath = filepath.Join(proc, "asound", "cards")
	cards := "  0 [Video          ]: USB-Audio - USB3.0 Video\n"
	if cardPresent {
		cards += "  1 [ArduinoImolaHPH]: qcm2290 - Arduino-Imola-HPH-LOUT\n"
	}
	if err := os.WriteFile(cardsPath, []byte(cards), 0o644); err != nil {
		t.Fatal(err)
	}
	if withADSP {
		adsp := filepath.Join(sys, "class", "remoteproc", "remoteproc1")
		if err := os.WriteFile(filepath.Join(adsp, "name"), []byte("adsp\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(adsp, "state"), []byte("running\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	dmesg := "[    2.411002] remoteproc remoteproc1: adsp is now up\n"
	if dmesgSignature {
		dmesg += "[   42.020665] platform a7c0000.pinctrl: deferred probe pending: qcom-sm6115-lpass-lpi-pinctrl: Failed to get clk 'audio'\n"
	}
	if err := os.WriteFile(filepath.Join(root, "dmesg"), []byte(dmesg), 0o444); err != nil {
		t.Fatal(err)
	}
	return root, cardsPath
}

// runAudioDSPHelper runs the real helper script against the fake tree.
func runAudioDSPHelper(t *testing.T, root, waitSecs string) (code int, out string) {
	t.Helper()
	cmd := exec.Command("sh", audioDSPScript(t))
	cmd.Env = append(os.Environ(),
		"GN_AUDIO_PROC_ROOT="+filepath.Join(root, "proc"),
		"GN_AUDIO_SYS_ROOT="+filepath.Join(root, "sys"),
		"GN_AUDIO_SETTLE_SECS=0",
		"GN_AUDIO_WAIT_SECS="+waitSecs,
		"GN_AUDIO_POLL_SECS=1",
		"GN_AUDIO_DMESG_FILE="+filepath.Join(root, "dmesg"),
	)
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	out = buf.String()
	code = 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("the helper did not run: %v\n%s", err, out)
	}
	return code, out
}

func stateOf(t *testing.T, root string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(root, "sys/class/remoteproc/remoteproc1/state"))
	return strings.TrimSpace(string(b))
}

// TestHelperIsANoOffUNOQ: other hardware is a logged no-op, nothing touched.
func TestHelperNoOpOffUNOQ(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, _ := fakeAudioDSPHost(t, false, false, true, true)
	code, out := runAudioDSPHelper(t, root, "1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (no-op):\n%s", code, out)
	}
	if !strings.Contains(out, "not a UNO Q") {
		t.Fatalf("missing the no-op log:\n%s", out)
	}
	if st := stateOf(t, root); st != "running" {
		t.Fatalf("the remoteproc state was touched off-UNO Q: %q", st)
	}
}

// TestHelperNoOpOnHealthyBoot: card present => no action, even with the
// signature in dmesg (healthy boots can carry the transient deferral line).
func TestHelperNoOpOnHealthyBoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, _ := fakeAudioDSPHost(t, true, true, true, true)
	code, out := runAudioDSPHelper(t, root, "1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (healthy no-op):\n%s", code, out)
	}
	if !strings.Contains(out, "healthy boot, no action") {
		t.Fatalf("missing the healthy no-op log:\n%s", out)
	}
	if st := stateOf(t, root); st != "running" {
		t.Fatalf("the remoteproc state was touched on a healthy boot: %q", st)
	}
}

// TestHelperNoOpWithoutSignature: card absent but the signature missing —
// a different fault; the helper must not blindly restart the ADSP.
func TestHelperNoOpWithoutSignature(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, _ := fakeAudioDSPHost(t, true, false, true, false)
	code, out := runAudioDSPHelper(t, root, "1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (no-signature no-op):\n%s", code, out)
	}
	if !strings.Contains(out, "not acting") {
		t.Fatalf("missing the no-signature log:\n%s", out)
	}
	if st := stateOf(t, root); st != "running" {
		t.Fatalf("the remoteproc state was touched without the signature: %q", st)
	}
}

// TestHelperRestartsWithNoADSP: the signature is present but there is no
// adsp remoteproc to restart — a bounded no-op, not an error.
func TestHelperNoOpWithoutADSP(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, _ := fakeAudioDSPHost(t, true, false, false, true)
	code, out := runAudioDSPHelper(t, root, "1")
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (no-ADSP no-op):\n%s", code, out)
	}
	if !strings.Contains(out, "no ADSP remoteproc") {
		t.Fatalf("missing the no-ADSP log:\n%s", out)
	}
}

// TestHelperBoundedRestartFailsWithoutCard: the issue #24 fault — the helper
// restarts the ADSP exactly once (stop then start land in the state file)
// and, the card never appearing, fails after the bounded wait.
func TestHelperBoundedRestartFailsWithoutCard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, _ := fakeAudioDSPHost(t, true, false, true, true)
	code, out := runAudioDSPHelper(t, root, "1")
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (card still absent after the restart):\n%s", code, out)
	}
	if !strings.Contains(out, "restarting the ADSP remoteproc") {
		t.Fatalf("missing the restart log:\n%s", out)
	}
	if st := stateOf(t, root); st != "start" {
		t.Fatalf("state file = %q, want the final 'start' write (stop then start)", st)
	}
	if !strings.Contains(out, "still absent") {
		t.Fatalf("missing the bounded-failure log:\n%s", out)
	}
}

// sleepAndWrite waits, then makes the card appear (the board's card1 line,
// issue #24): the success path of the bounded wait.
func sleepAndWrite(d time.Duration, cardsPath string) error {
	time.Sleep(d)
	return os.WriteFile(cardsPath,
		[]byte("  0 [Video          ]: USB-Audio - USB3.0 Video\n"+
			"  1 [ArduinoImolaHPH]: qcm2290 - Arduino-Imola-HPH-LOUT\n"), 0o644)
}

// TestHelperSucceedsWhenTheCardAppears: the restart works — the card appears
// mid-wait, the helper exits 0 with the success log.
func TestHelperSucceedsWhenTheCardAppears(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX sh simulation")
	}
	root, cardsPath := fakeAudioDSPHost(t, true, false, true, true)
	errCh := make(chan error, 1)
	go func() {
		// Let the helper pass the no-card checks and issue stop/start, then
		// make the card appear. 0.4 s lands inside the first 1 s poll window
		// of the 3 s budget.
		errCh <- sleepAndWrite(400*time.Millisecond, cardsPath)
	}()
	code, out := runAudioDSPHelper(t, root, "3")
	if err := <-errCh; err != nil {
		t.Fatalf("the card write did not land: %v", err)
	}
	if code != 0 {
		t.Fatalf("exit = %d, want 0 (card appeared after the restart):\n%s", code, out)
	}
	if !strings.Contains(out, "success") {
		t.Fatalf("missing the success log:\n%s", out)
	}
}
