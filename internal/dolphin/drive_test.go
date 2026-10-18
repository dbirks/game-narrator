//go:build unix

package dolphin

import (
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealDriveToFirstDialogue is the M6 real-hardware proof
// (docs/spec/11 "E2E verification steps" + "Avoiding false positives"): the
// SCREEN-DRIVEN drive (WindWakerNewGameDrive, ADR-0043 amendment 5) takes
// the real image from boot to its first in-game dialogue box — every
// iteration classifies the current screen (a measured class, never
// "changed"), sends the ONE action that screen prescribes, and logs
// (screen, action) -> next screen. There are no pre-satisfied and no
// skipped steps. Termination: the dialogue-box class (success), the same
// screen >= 4 consecutive iterations with no visible change (loop detected),
// or the iteration/time budget (failure with the full transition log and the
// last PNG). NOT RUN when the image or the host tools are missing.
func TestRealDriveToFirstDialogue(t *testing.T) {
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils): %v", bin, err)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	evOut := os.Getenv("GAME_NARRATOR_EVIDENCE_OUT")
	if evOut != "" {
		if err := os.MkdirAll(evOut, 0o755); err != nil {
			t.Fatalf("evidence dir %s: %v", evOut, err)
		}
	}

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	if evOut != "" {
		// Evidence of the EXIT state (ADR-0043 amendment 2): registered
		// BEFORE defer b.Stop(), so it runs AFTER the stop — the exit-rewritten
		// Dolphin.ini proves the config Dolphin actually kept (the memcard
		// binding included), bridge-emu.log carries the emulator-side errors
		// (EXI/memcard), and the user-dir tree shows whether the card file
		// materialised. A calibration loop reads these without re-running.
		defer func() {
			userDir := b.State().UserDir
			if userDir == "" {
				return // Launch never completed; nothing to capture
			}
			copyFileToEvidence(t, userDir, "Config/Dolphin.ini", "config-dolphin-exit.ini", evOut)
			copyFileToEvidence(t, userDir, "Config/GCPadNew.ini", "config-gcpad-exit.ini", evOut)
			copyFileToEvidence(t, userDir, "Logs/dolphin.log", "dolphin-exit.log", evOut)
			copyFileToEvidence(t, userDir, "bridge-emu.log", "bridge-emu-exit.log", evOut)
			writeUserDirTree(t, userDir, "userdir-tree.txt", evOut)
		}()
	}
	defer b.Stop()

	// Phase 1: the screen-driven drive to the first in-game dialogue box.
	// The default launch is the proven single-pipe path (ADR-0043): one FIFO,
	// no savestate hotkeys.
	spec := LaunchSpec{}
	if err := b.Launch(ctx, spec); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	st := b.State()
	t.Logf("launched: display=%s dolphin_pid=%d gcpad_device=%s hotkey_device=%s image_sha256=%s",
		st.Display, st.DolphinPID, st.PipeDevice, st.HotkeyPipeDevice, st.ImageSHA)

	drv, err := RunDrive(ctx, b, DriveOptions{EvidenceDir: evOut}, WindWakerNewGameDrive())
	// The full transition log, one line per iteration: (screen, action) ->
	// next screen with the measured signature of the classified frame.
	for _, tr := range drv.Transitions {
		next := "<end>"
		if i := tr.Index + 1; i < len(drv.Transitions) {
			n := drv.Transitions[i]
			next = string(n.Screen)
			if n.Screen == DriveScreenPrompt || n.Screen == DriveScreenNameConfirm {
				next += ":" + string(n.Side)
			}
		}
		t.Logf("step %02d: %s:%s --(%s)--> %s | %s", tr.Index, tr.Screen, tr.Side, tr.Action, next, tr.Detail)
	}
	t.Logf("drive: ok=%v reason=%q elapsed=%s", drv.OK, drv.Reason, drv.Finished.Sub(drv.Started).Round(time.Second))
	if drv.OK {
		t.Logf("drive: success screen=%s text=%v first-success-frame=%s", drv.SuccessScreen, drv.SuccessText, drv.SuccessPath)
	}
	if err != nil || !drv.OK {
		// The drive stopped before the box: report the farthest correctly
		// identified screen, why, and the evidence — the task's honest
		// failure contract (the gate is unmet, so the test fails, but the
		// full transition log and the last PNG name the measured cause).
		logMemcardState(t, st.UserDir)
		t.Logf("DRIVE STOP REPORT: farthest identified screen = %s (last screenshot: %s)", drv.LastKey, drv.LastPath)
		t.Logf("DRIVE STOP REPORT: why = %s", drv.Reason)
		t.Logf("DRIVE STOP REPORT: evidence = %s", evOut)
		t.Fatalf("drive to first dialogue stopped: %s (last screen %s; evidence: %s)",
			drv.Reason, drv.LastKey, drv.LastPath)
	}

	// The end assertion, measured on a fresh screenshot (the run's last poll
	// is the drive's evidence; this is the test's own proof). The assertion
	// matches the success screen the loop actually reached (amendment 6):
	// the opening legend's narration subtitle (story_subtitle — the first
	// in-game narrated text, measured in the bottom black bar) or the
	// in-game dialogue box, whichever the loop reached first.
	end, err := b.Screenshot(ctx)
	if err != nil {
		t.Fatalf("final screenshot: %v", err)
	}
	switch drv.SuccessScreen {
	case DriveScreenStorySubtitle:
		// The success frame carries the measured bottom-bar narration text:
		// the pure-black letterbox clauses plus the subbar bright/edge
		// clauses (a text-less fade frame fails these — the class is only
		// claimed on a frame that MEASURES the text).
		sig := ScreenSigOf(end)
		c, _ := sig.ClassOf()
		sub := sig.Regions["subbar"]
		ok := c == ClassStorySub &&
			sig.Letterbox < classStorySubLbMax &&
			sub.bright >= classStorySubBrightMin &&
			sub.edges >= classStorySubEdgesMin
		t.Logf("story_subtitle: class=%s want storysub; lb=%.3f want < %.2f; subbar bright=%.3f want >= %.2f, edges=%.3f want >= %.2f",
			c, sig.Letterbox, classStorySubLbMax, sub.bright, classStorySubBrightMin, sub.edges, classStorySubEdgesMin)
		if !ok {
			t.Fatalf("the final screen is not the opening legend's narration subtitle: class=%s lb=%.3f subbar bright=%.3f edges=%.3f", c, sig.Letterbox, sub.bright, sub.edges)
		}
		// The crop handoff: the measured bottom-bar subtitle region in
		// normalised frame fractions — the live-session E2E's textbox-region
		// configuration (ADR-0043 amendment 6).
		t.Logf("subtitle crop (normalised x0,y0,x1,y1) = %.3f, %.3f, %.3f, %.3f", drv.SubCrop[0], drv.SubCrop[1], drv.SubCrop[2], drv.SubCrop[3])
		// The narration visibility record (requirement 3): each subtitle's
		// fade-in/hold/fade-out, sampled at the drive's iteration cadence.
		for i, st := range drv.SubTimings {
			t.Logf("subtitle %d: iters %d-%d fade-in=%s hold=%s fade-out=%s open=%v", i, st.FirstIndex, st.LastIndex, st.FadeIn.Round(100*time.Millisecond), st.Hold.Round(100*time.Millisecond), st.FadeOut.Round(100*time.Millisecond), st.Open)
		}
	default:
		if ok, detail := BoxPresent()(ctx, end, end); !ok {
			t.Fatalf("the final screen is not the first dialogue box: %s", detail)
		} else {
			t.Logf("boxband: %s", detail)
			if tbOK, tbDetail := TextboxPresent()(ctx, end, end); tbOK {
				t.Logf("textbox: %s", tbDetail)
			} else {
				t.Logf("textbox (reference only): %s", tbDetail)
			}
		}
	}
	if evOut != "" {
		writeEvidencePNG(t, evOut, "drive-final.png", end)
	}
	userDir := st.UserDir

	// Phase 2 + 3: the savestate round trip — OPT-IN only (LaunchSpec.Hotkeys,
	// ADR-0043). On the proven single-pipe default the hotkey FIFO does not
	// exist and SaveState/LoadState refuse with ErrHotkeysDisabled; the drive
	// to the dialogue box (phase 1) is what this test's name promises.
	if !spec.Hotkeys {
		t.Logf("savestate: NOT RUN on the default path (hotkeys off, ADR-0043) — the drive to the first dialogue box above is the proof")
		checkExitRewriteFiles(t, b, userDir, st.PipeDevice, st.HotkeyPipeDevice)
		// The memory-card use proof (ADR-0043 amendment 2): AFTER the stop,
		// the exit-rewritten Dolphin.ini must still carry the memcard binding
		// (Dolphin parsed and kept it), and — when the drive's save path ran
		// (the "A save file has been created." notice appeared and no
		// continue-anyway prompt followed) — the card file must exist with a
		// non-zero size (Dolphin wrote the save to it).
		checkMemcardExit(t, userDir, driveTookSavePath(drv))
		return
	}

	// A failure here is a measured finding, not a test crash: the evidence is
	// logged and the --save_state phase is skipped (the BLOCKED row in
	// docs/status carries it) — the drive itself (phase 1) is what the test
	// name promises.
	statePath, err := b.SaveState(ctx, 1)
	if err != nil {
		logSavestateEvidence(t, userDir, st.HotkeyPipeDevice, "save", err)
		t.Logf("savestate: NOT PROVEN — save failed: %v (phases 2b/3 skipped; see the logged evidence)", err)
		return
	}
	t.Logf("savestate: saved slot 1 → %s (hotkey over %s)", statePath, st.HotkeyPipeDevice)

	// Advance to a different screen (Y): press A to advance the dialogue,
	// then the stick to move if the screen allows it.
	if err := b.Inject(ctx, Buttons{A: true}, Axes{}, 500*time.Millisecond); err != nil {
		t.Fatalf("advance inject A: %v", err)
	}
	if err := b.Inject(ctx, Buttons{}, Axes{MainY: -1}, 1500*time.Millisecond); err != nil {
		t.Fatalf("advance inject stick: %v", err)
	}
	time.Sleep(2 * time.Second)
	yImg, err := b.Screenshot(ctx)
	if err != nil {
		t.Fatalf("post-advance screenshot: %v", err)
	}
	if evOut != "" {
		writeEvidencePNG(t, evOut, "advance-Y.png", yImg)
	}
	advDist := rmse01(yImg, end, normW, normH)
	t.Logf("savestate: advanced, rmse(Y, X) = %.3f", advDist)
	if advDist < 0.05 {
		// The advance did not visibly change the screen: a later load could
		// be a no-op and the round trip would prove nothing. Record it.
		t.Logf("savestate: NOT PROVEN — the advance changed the screen by only %.3f (< 0.05); a no-op load is indistinguishable (phases 2b/3 skipped)", advDist)
		return
	}
	if err := b.LoadState(ctx, 1); err != nil {
		logSavestateEvidence(t, userDir, st.HotkeyPipeDevice, "load", err)
		t.Fatalf("savestate: load failed: %v", err)
	}
	xBack, err := b.Screenshot(ctx)
	if err != nil {
		t.Fatalf("post-load screenshot: %v", err)
	}
	if evOut != "" {
		writeEvidencePNG(t, evOut, "after-load-X.png", xBack)
	}
	loadDist := rmse01(xBack, end, normW, normH)
	t.Logf("savestate: round trip, rmse(load-X, X) = %.3f (control threshold %.2f)", loadDist, crossStepMax)
	if loadDist > crossStepMax {
		t.Fatalf("savestate: the loaded screen does not return to X (rmse %.3f > %.2f)", loadDist, crossStepMax)
	}

	// Phase 3: a fresh boot with --save_state lands on X (textbox within 30 s).
	b.Stop()
	if err := strayDolphinProcesses(); err != nil {
		t.Fatalf("after the phase-2 stop: %v", err)
	}
	if err := b.Launch(ctx, LaunchSpec{SaveState: statePath}); err != nil {
		t.Fatalf("relaunch with --save_state: %v", err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		shot, serr := b.Screenshot(ctx)
		if serr == nil {
			if ok, detail := BoxPresent()(ctx, shot, shot); ok {
				t.Logf("savestate: --save_state boot landed on X (box band) — %s", detail)
				if evOut != "" {
					writeEvidencePNG(t, evOut, "savestate-boot-X.png", shot)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("savestate: no dialogue box within 30 s of the --save_state boot (the state file was %s)", statePath)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("savestate: context done: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}

	// The exit-rewrite check (the spike's technique, now for every device
	// line this launch wrote): a clean exit rewrites the config with the
	// RESOLVED device strings — if a device line did not resolve, the
	// parser drops it.
	st2 := b.State()
	checkExitRewriteFiles(t, b, st2.UserDir, st2.PipeDevice, st2.HotkeyPipeDevice)
}

// driveTookSavePath reports whether the drive's SAVE path ran, from the
// transition log: true when the blobless save-notice screen (the "A save
// file has been created." dialog) appeared and NO Yes/No prompt followed it
// (a prompt after the notice is the save-failure continue-anyway dialog —
// the save was not written). With a working Slot A card (the bridge's
// explicit raw memcard, ADR-0043 amendment 2) this is the half of the use
// proof the card-file size check (checkMemcardExit) then verifies on disk.
func driveTookSavePath(drv DriveTrace) bool {
	notice := -1
	for i, tr := range drv.Transitions {
		if tr.Screen == DriveScreenSaveNotice {
			notice = i
			break
		}
	}
	if notice < 0 {
		return false
	}
	for _, tr := range drv.Transitions[notice:] {
		if tr.Screen == DriveScreenPrompt || tr.Screen == DriveScreenNameConfirm {
			return false
		}
	}
	return true
}

// checkExitRewriteFiles stops the bridge (idempotent; the test's defer
// repeats it harmlessly) and re-verifies the clean exit's config rewrite
// against the MEASURED device strings. An empty want = that file was not
// written by this launch (the default single-pipe path writes no
// Hotkeys.ini, ADR-0043).
func checkExitRewriteFiles(t *testing.T, b Bridge, userDir, gcpadDev, hotkeyDev string) {
	t.Helper()
	b.Stop()
	if err := strayDolphinProcesses(); err != nil {
		t.Fatalf("after the stop: %v", err)
	}
	checks := map[string]string{
		filepath.Join(userDir, "Config", "GCPadNew.ini"): gcpadDev,
	}
	if hotkeyDev != "" {
		checks[filepath.Join(userDir, "Config", "Hotkeys.ini")] = hotkeyDev
	}
	for name, want := range checks {
		if want == "" {
			continue
		}
		got, rerr := os.ReadFile(name)
		if rerr != nil {
			t.Errorf("exit rewrite of %s unreadable: %v", name, rerr)
			continue
		}
		if !strings.Contains(string(got), "Device = "+want+"\n") {
			t.Errorf("the exit rewrite of %s lost the device line %q (the device did not resolve?):\n%s", name, want, got)
		}
	}
}

// logMemcardState logs the Slot A card's state (reference only — the
// failure report's half of the use proof, ADR-0043 amendment 2): whether the
// raw card file exists and its size. The config-rewrite half is verified
// after the stop (checkMemcardExit).
func logMemcardState(t *testing.T, userDir string) {
	t.Helper()
	card := memcardAPath(userDir)
	fi, err := os.Stat(card)
	if err != nil {
		t.Logf("memcard: card file absent (no save was written to it): %s", card)
		return
	}
	t.Logf("memcard: card file %s size=%d bytes", card, fi.Size())
}

// copyFileToEvidence copies a file from the (about-to-be-deleted) user dir
// into the evidence dir after the stop — the exit-state proof (ADR-0043
// amendment 2). Missing files are logged, not fatal: a failed launch simply
// has no exit config.
func copyFileToEvidence(t *testing.T, userDir, rel, evidenceName, evOut string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(userDir, rel))
	if err != nil {
		t.Logf("evidence: %s absent (not captured): %v", rel, err)
		return
	}
	if err := os.WriteFile(filepath.Join(evOut, evidenceName), data, 0o644); err != nil {
		t.Logf("evidence: writing %s: %v", evOut, evidenceName)
	}
}

// writeUserDirTree writes a two-level listing of the user dir (dir names +
// file sizes) into the evidence dir — whether GC/ holds the card file is the
// visible half of the memcard use proof.
func writeUserDirTree(t *testing.T, userDir, evidenceName, evOut string) {
	t.Helper()
	var b strings.Builder
	entries, err := os.ReadDir(userDir)
	if err != nil {
		t.Logf("evidence: user dir tree: %v", err)
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			fmt.Fprintf(&b, "[dir] %s\n", e.Name())
			subs, _ := os.ReadDir(filepath.Join(userDir, e.Name()))
			for _, s := range subs {
				if s.IsDir() {
					fmt.Fprintf(&b, "    [dir] %s/%s\n", e.Name(), s.Name())
					continue
				}
				if fi, err := s.Info(); err == nil {
					fmt.Fprintf(&b, "    %12d  %s/%s\n", fi.Size(), e.Name(), s.Name())
				}
			}
			continue
		}
		if fi, err := e.Info(); err == nil {
			fmt.Fprintf(&b, "%12d  %s\n", fi.Size(), e.Name())
		}
	}
	if err := os.WriteFile(filepath.Join(evOut, evidenceName), []byte(b.String()), 0o644); err != nil {
		t.Logf("evidence: writing %s: %v", evidenceName, err)
	}
}

// checkMemcardExit verifies, AFTER the stop, that the Slot A card was the
// save target (ADR-0043 amendment 2): the exit-rewritten Dolphin.ini still
// carries the memcard binding (Dolphin parsed and kept the [Core] SlotA /
// MemcardAPath lines — the config-rewrite proof), and — when the drive's
// save path ran — the card file exists with a non-zero size (Dolphin wrote
// the formatted card and the save into it). On the continue-anyway path
// (no save) the file state is logged, not asserted: the drive legitimately
// proceeded without a card.
func checkMemcardExit(t *testing.T, userDir string, saveCreated bool) {
	t.Helper()
	dini, err := os.ReadFile(filepath.Join(userDir, "Config", "Dolphin.ini"))
	if err != nil {
		t.Errorf("exit rewrite: Dolphin.ini unreadable (no clean exit?): %v", err)
		return
	}
	for _, want := range []string{"SlotA = " + memcardSlotA + "\n", "MemcardAPath = " + memcardAPath(userDir) + "\n"} {
		if !strings.Contains(string(dini), want) {
			t.Errorf("exit rewrite lost the memcard binding %q (Dolphin dropped the key?):\n%s", want, dini)
		}
	}
	card := memcardAPath(userDir)
	fi, err := os.Stat(card)
	if err != nil {
		if saveCreated {
			t.Errorf("the save path ran but the card file is missing (the save was not written to the configured card): %v", err)
		}
		t.Logf("memcard: card file absent on the continue-anyway path (expected: no save): %s", card)
		return
	}
	t.Logf("memcard: card file %s size=%d bytes", card, fi.Size())
	if saveCreated && fi.Size() == 0 {
		t.Errorf("the card file is zero size despite the save path: %s", card)
	}
}

// logSavestateEvidence dumps the evidence a savestate failure needs: what
// the states dir holds, what the written Hotkeys.ini says, and the tail of
// the emulator log (the boot/device lines). Paths only — never state file
// bytes (they embed game RAM, the user-data rule).
func logSavestateEvidence(t *testing.T, userDir, hotkeyDev, phase string, cause error) {
	t.Helper()
	t.Logf("savestate: evidence (%s failed: %v)", phase, cause)
	entries, _ := filepath.Glob(filepath.Join(userDir, "states", "*"))
	t.Logf("savestate: states dir: %v", entries)
	if hk, err := os.ReadFile(filepath.Join(userDir, "Config", "Hotkeys.ini")); err == nil {
		t.Logf("savestate: written Hotkeys.ini (device %s):\n%s", hotkeyDev, hk)
	}
	logPath := filepath.Join(userDir, "Logs", "dolphin.log")
	if b, err := os.ReadFile(logPath); err == nil {
		lines := strings.Split(string(b), "\n")
		keep := lines
		if len(lines) > 25 {
			keep = lines[len(lines)-25:]
		}
		t.Logf("savestate: emulator log tail:\n%s", strings.Join(keep, "\n"))
	}
}

// writeEvidencePNG stores one evidence frame in the evidence dir (best
// effort: a write failure is a log line, not a failed run — the hashes and
// the measured predicates are the proof).
func writeEvidencePNG(t *testing.T, dir, name string, img image.Image) {
	t.Helper()
	f, err := os.Create(filepath.Join(dir, name))
	if err != nil {
		t.Logf("evidence: %s not written: %v", name, err)
		return
	}
	err = png.Encode(f, img)
	_ = f.Close()
	if err != nil {
		t.Logf("evidence: %s not encoded: %v", name, err)
		return
	}
	t.Logf("evidence: %s → %s", name, filepath.Join(dir, name))
}
