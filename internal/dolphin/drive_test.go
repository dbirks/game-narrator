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
// last PNG).
//
// The drive runs PAST-LEGEND (ADR-0054 amendment 1): the opening legend's
// first narration subtitle keeps its success credit (the timing record's
// first bracket), but the run rides the rest of the legend wait-only (a
// press may skip a subtitle) until the measured first in-game dialogue box
// appears — the test's name is the promise. NOT RUN when the image or the
// host tools are missing.
func TestRealDriveToFirstDialogue(t *testing.T) {
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "xdotool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, xdotool): %v", bin, err)
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
			// The exit-rewritten Hotkeys.ini (opt-in path only): the standing
			// oracle for the keyboard hotkey layout — the RESOLVED device
			// line plus the re-serialized bindings (a binding that failed to
			// parse is absent from the rewrite; ADR-0043 am7).
			copyFileToEvidence(t, userDir, "Config/Hotkeys.ini", "config-hotkeys-exit.ini", evOut)
			copyFileToEvidence(t, userDir, "Logs/dolphin.log", "dolphin-exit.log", evOut)
			copyFileToEvidence(t, userDir, "bridge-emu.log", "bridge-emu-exit.log", evOut)
			writeUserDirTree(t, userDir, "userdir-tree.txt", evOut)
			// The StateSaves dir listing after the run (the save files, if any —
			// names/sizes only, never the bytes; they embed game RAM).
			if entries, _ := filepath.Glob(filepath.Join(userDir, stateSavesDir, "*")); len(entries) > 0 {
				for _, e := range entries {
					if fi, err := os.Stat(e); err == nil {
						t.Logf("savestate: state file %s size=%d mtime=%s", filepath.Base(e), fi.Size(), fi.ModTime().Format(time.RFC3339))
					}
				}
			}
		}()
	}
	defer b.Stop()

	// Phase 1: the screen-driven drive to the first in-game dialogue box.
	// The launch opts in to the savestate keyboard hotkeys (ADR-0043 am7,
	// approach D): Hotkeys.ini binds slot 1's save/load to F1/F2 on the X11
	// keyboard device and SaveState/LoadState fire them with XTest (xdotool)
	// — the controller FIFO stays single-consumer — so the savestate phases
	// below (2 + 3) run in the same test.
	spec := LaunchSpec{Hotkeys: true}
	if err := b.Launch(ctx, spec); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	st := b.State()
	t.Logf("launched: display=%s dolphin_pid=%d gcpad_device=%s hotkey_device=%s image_sha256=%s",
		st.Display, st.DolphinPID, st.PipeDevice, st.HotkeyDevice, st.ImageSHA)

	drv, err := RunDrive(ctx, b, DriveOptions{EvidenceDir: evOut, PastLegend: true}, WindWakerNewGameDrive())
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
	// matches the success screen the loop actually reached — but that screen
	// is still SETTLING when the drive's last poll fires, and the drive's
	// stop frame can itself be early (acceptance.md gap 12): the box's
	// typewriter reveal keeps the boxband's horizontal-edge fraction below
	// the measured floor for its first ~1-2 s; the pre-box animated scene
	// carries a bright box band (mean 76-152) that can even match the BRIGHT
	// box clauses on a passing frame (run-2's stop frame: boxband
	// mean=130 edges=0.066 — a scene, not a box); and the first box frame
	// the 2 s drive cadence samples can be the BRIEF box that auto-advances
	// within 1-2 s (run-1/3 stop frames: boxband mean=62/59) before the
	// first NPC dialogue box — the held box, the fixture's box 1 — appears
	// 20-40 s later and A-HOLDS unchanged for 10+ minutes (the 64 s no-input
	// sample: steady at boxband 46/0.031). The recheck therefore polls a
	// bounded window and stops at the first frame that MEASURES the real
	// box shape — the classifier's DARK box form (bb.mean below the bright
	// floor; the animated scene's dips to 76-80 carry no glyph edges and
	// are rejected by the classifier, not just the band) — and that accepted
	// frame becomes the savestate round-trip's X reference. The window is
	// the measured 20-40 s gap from the drive's stop to the held box with
	// margin; the opening legend branch keeps the 10 s settle window (the
	// legend's own text frames and measured interludes arrive within it).
	legendFrame := func(img image.Image) (bool, string) {
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		sub := sig.Regions["subbar"]
		// A text-bearing subtitle frame: the strict measured clauses (the
		// class is only claimed on a frame that MEASURES the bottom-bar
		// text).
		textFrame := c == ClassStorySub &&
			sig.Letterbox < classStorySubLbMax &&
			sub.bright >= classStorySubBrightMin &&
			sub.edges >= classStorySubEdgesMin
		// A text-less legend interlude (between subtitles, a page turn, the
		// storybook panel's fade): the measured interlude classes over the
		// pure-black-bar letterbox (the dense recording's 415-508 frames:
		// cutscene/dark/unknown, lb 0.000, full-frame mean 49-96) — not a
		// system screen, not the title, not the box.
		interlude := (c == ClassDark || c == ClassScene || c == ClassLetter || c == ClassUnknown) &&
			sig.Letterbox < classStorySubLbMax && sig.Mean < 130
		return textFrame || interlude,
			fmt.Sprintf("class=%s lb=%.3f mean=%.1f subbar bright=%.3f edges=%.3f (text frame or measured legend interlude)",
				c, sig.Letterbox, sig.Mean, sub.bright, sub.edges)
	}
	end, err := b.Screenshot(ctx)
	if err != nil {
		t.Fatalf("final screenshot: %v", err)
	}
	var ok bool
	var detail string
	var frames int
	switch drv.SuccessScreen {
	case DriveScreenStorySubtitle:
		end, ok, detail, frames = pollSettled(t, ctx, b, end, settleRecheckWindow, settleRecheckInterval, legendFrame)
		if !ok {
			t.Fatalf("the final screen is not the opening legend: %s (no frame accepted in %d of the %s recheck window)", detail, frames, settleRecheckWindow)
		}
		t.Logf("story_subtitle: %s (accepted at frame %d of the %s recheck window)", detail, frames, settleRecheckWindow)
		// The recorded success frame carries the measured text (the oracle's
		// own record — the fresh screenshot may have caught the legend
		// between subtitles).
		if !drv.SuccessText {
			t.Fatalf("the drive's success frame measured no bottom-bar text (SuccessText=false): the legend's first subtitle was not measured")
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
		end, ok, detail, frames = pollSettled(t, ctx, b, end, boxRecheckWindow, settleRecheckInterval,
			func(img image.Image) (bool, string) {
				sig := ScreenSigOf(img)
				c, _ := sig.ClassOf()
				bb := sig.Regions["boxband"]
				isBox := c == ClassBox && bb.mean < boxBandMeanMin
				return isBox, fmt.Sprintf("class=%s lb=%.3f boxband[mean=%.0f edges=%.3f] (real box = the classifier's dark form: bb.mean < %.0f)", c, sig.Letterbox, bb.mean, bb.edges, boxBandMeanMin)
			})
		if !ok {
			t.Fatalf("the final screen is not the first dialogue box: %s (no frame accepted in %d of the %s recheck window)", detail, frames, boxRecheckWindow)
		}
		t.Logf("boxband: %s (accepted at frame %d of the %s recheck window)", detail, frames, boxRecheckWindow)
		if tbOK, tbDetail := TextboxPresent()(ctx, end, end); tbOK {
			t.Logf("textbox: %s", tbDetail)
		} else {
			t.Logf("textbox (reference only): %s", tbDetail)
		}
	}
	if evOut != "" {
		writeEvidencePNG(t, evOut, "drive-final.png", end)
	}
	userDir := st.UserDir

	// Phase 2 + 3: the savestate round trip over the keyboard hotkeys
	// (ADR-0043 am7, approach D: F1/F2 XTest-injected into the Xvfb display,
	// the hotkey scheduler's own thread is the only reader of that key
	// state). The launch opts in (LaunchSpec.Hotkeys); a launch that does NOT
	// opt in has no Hotkeys.ini and SaveState/LoadState refuse with
	// ErrHotkeysDisabled — the drive to the dialogue box (phase 1) is still
	// what this test's name promises on such a launch.
	if !spec.Hotkeys {
		t.Logf("savestate: NOT RUN on the default path (hotkeys off, ADR-0043) — the drive to the first dialogue box above is the proof")
		checkExitRewriteFiles(t, b, userDir, st.PipeDevice, st.HotkeyDevice)
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
	//
	// The savestate is anchored on the HELD box — the stable measured
	// screen — not on the drive's first box frame. Measured (runs 1-2
	// 2026-10-18 + the fixture's dense timeline): the pre-box animated
	// scene carries a bright box band (mean 76-152) for 10-70 s and can
	// even match the bright box clauses (the drive's run-2 stop frame,
	// boxband mean=130 edges=0.066), a BRIEF box appears and auto-advances
	// within a few seconds (run-1 step 133, boxband mean=62), and only then
	// does the first NPC dialogue box (the fixture's box 1) appear and
	// A-HOLD unchanged for 10+ minutes (the 64 s no-input sample: the
	// signature steady at boxband 46/0.031). The restore's fixed 8 s settle
	// is deterministic only on that held screen — run 1's restore pair
	// straddled the transient (rmse 0.087 on the moving scene) and run 2's
	// straddled the box's appearance (rmse 0.253). So the save waits, at
	// the drive's 2 s cadence, for the DARK box band (the measured real-box
	// shape: the ClassBox classifier's dark form, bb.mean < the bright
	// floor — the animated scene's dips to 76-80 carry no glyph edges and
	// are rejected by the classifier, not just the band) to HOLD for
	// stableBoxSamples consecutive samples (the brief box auto-advances in
	// fewer; the held box holds for minutes).
	stableBoxSamples := 3
	stableBoxWindow := 120 * time.Second
	stableStreak := 0
	stableFrames := 0
	stableDetail := ""
	stableDeadline := time.Now().Add(stableBoxWindow)
	for {
		shot, err := b.Screenshot(ctx)
		if err == nil {
			stableFrames++
			sig := ScreenSigOf(shot)
			c, _ := sig.ClassOf()
			bb := sig.Regions["boxband"]
			detail := fmt.Sprintf("class=%s lb=%.3f boxband[mean=%.0f edges=%.3f]", c, sig.Letterbox, bb.mean, bb.edges)
			stableDetail = detail
			if c == ClassBox && bb.mean < boxBandMeanMin {
				stableStreak++
				if stableStreak >= stableBoxSamples {
					t.Logf("savestate: the held box is stable (dark box band for %d consecutive 2 s samples, %d frames in %s) — %s — anchoring the save here", stableStreak, stableFrames, stableBoxWindow, detail)
					break
				}
			} else {
				stableStreak = 0
			}
			t.Logf("savestate: held-box anchor: frame %d (streak %d/%d): %s", stableFrames, stableStreak, stableBoxSamples, detail)
		}
		if time.Now().After(stableDeadline) {
			t.Fatalf("savestate: no held box (dark box band stable for %d consecutive 2 s samples) within %s (%d frames; last: %s) — the first NPC dialogue box did not hold", stableBoxSamples, stableBoxWindow, stableFrames, stableDetail)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("savestate: context done: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
	statePath, err := b.SaveState(ctx, 1)
	if err != nil {
		logSavestateEvidence(t, userDir, st.HotkeyDevice, "save", err)
		t.Logf("savestate: NOT PROVEN — save failed: %v (phases 2b/3 skipped; see the logged evidence)", err)
		return
	}
	t.Logf("savestate: saved slot 1 → %s (hotkey key %s over %s)", statePath, saveHotkeyKey, st.HotkeyDevice)

	// advanceToY sends the measured advance (A to advance the dialogue, then
	// the stick to move if the screen allows it) and screenshots the result.
	advanceToY := func(tag string) (image.Image, float64, error) {
		if err := b.Inject(ctx, Buttons{A: true}, Axes{}, 500*time.Millisecond); err != nil {
			return nil, 0, fmt.Errorf("advance inject A: %w", err)
		}
		if err := b.Inject(ctx, Buttons{}, Axes{MainY: -1}, 1500*time.Millisecond); err != nil {
			return nil, 0, fmt.Errorf("advance inject stick: %w", err)
		}
		time.Sleep(2 * time.Second)
		yImg, err := b.Screenshot(ctx)
		if err != nil {
			return nil, 0, fmt.Errorf("post-advance screenshot: %w", err)
		}
		if evOut != "" {
			writeEvidencePNG(t, evOut, tag+".png", yImg)
		}
		dist := rmse01(yImg, end, normW, normH)
		t.Logf("savestate: %s, rmse(Y, X) = %.3f", tag, dist)
		return yImg, dist, nil
	}
	// restoreToX fires the load hotkey and screenshots after a FIXED total
	// settle (LoadState's 3 s floor + the extra sleep = restoreSettle), so
	// every restore below lands on the SAME point of the restored timeline —
	// the double-restore comparison is deterministic only at matched offsets.
	restoreToX := func(tag string) (image.Image, error) {
		if err := b.LoadState(ctx, 1); err != nil {
			return nil, err
		}
		time.Sleep(restoreSettle - hotkeyLoadSettle)
		shot, err := b.Screenshot(ctx)
		if err != nil {
			return nil, fmt.Errorf("post-load screenshot: %w", err)
		}
		if evOut != "" {
			writeEvidencePNG(t, evOut, tag+".png", shot)
		}
		sig := ScreenSigOf(shot)
		c, _ := sig.ClassOf()
		t.Logf("savestate: %s, class=%s | %s", tag, c, sig.SigLine())
		return shot, nil
	}

	// Advance to a different screen (Y). The advance must be VISIBLE against
	// X: a no-op advance would make a no-op load indistinguishable.
	_, advDist, err := advanceToY("advance-Y")
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if advDist < 0.05 {
		t.Logf("savestate: NOT PROVEN — the advance changed the screen by only %.3f (< 0.05); a no-op load is indistinguishable (phases 2b/3 skipped)", advDist)
		return
	}

	// The round trip as a DOUBLE RESTORE. The success screen is the animated
	// opening legend (or a rendered scene whose box the A-advance dismisses),
	// so a pixel comparison of the load against X measures the restored
	// timeline's own motion, not the load: the measured run (2026-10-18
	// run-drive3) restored the legend correctly — the after-load frame
	// classified storysub with the SAME panel art (blobs at the same
	// centroids, matching mean/std/entropy) — yet the 3 s timeline offset
	// alone measured 0.21 RMSE against X. The proof is therefore
	// deterministic: restore the SAME state twice, run each restore for the
	// same settle, and the two X frames must match within the control
	// threshold. A no-op load stays on the (different-class) Y screen and
	// the class clause fails; a restore of a different state lands on a
	// different timeline point and the rmse clause fails.
	x1, err := restoreToX("after-load-X1")
	if err != nil {
		logSavestateEvidence(t, userDir, st.HotkeyDevice, "load-1", err)
		t.Fatalf("savestate: load 1 failed: %v", err)
	}
	if _, _, err := advanceToY("advance-Y2"); err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	x2, err := restoreToX("after-load-X2")
	if err != nil {
		logSavestateEvidence(t, userDir, st.HotkeyDevice, "load-2", err)
		t.Fatalf("savestate: load 2 failed: %v", err)
	}
	loadDist := rmse01(x1, x2, normW, normH)
	t.Logf("savestate: double restore, rmse(X1, X2) = %.3f (control threshold %.2f)", loadDist, crossStepMax)
	if loadDist > crossStepMax {
		t.Fatalf("savestate: the two restores of the same state differ (rmse %.3f > %.2f) — the load did not restore the saved state", loadDist, crossStepMax)
	}
	// The semantic half: the restored scene is X's SCENE again (a no-op
	// load would stay on the advance's Y screen). Measured over the SAME
	// bounded settle poll as the end assertion (acceptance.md gap 12): the
	// restore can land inside the box's own transition-in — the save fires
	// async over the hotkey and the box AUTO-APPEARS (the drive's wait-only
	// phase), so the saved moment can be the box's transition-in, whose
	// measured signature is the bright smooth box-band (the drive's own log:
	// the pre-box scene boxband mean=145 edges=0.008, the transition frame
	// mean=130 edges=0.036) — the same family the restore lands on 8 s later
	// (run-drive 2026-10-18 20:05: boxband mean=143-144 edges 0.002-0.004).
	// A single frame sampled at machine timing inside that transition reads
	// class=unknown while X reads class=box, so the poll accepts EITHER X's
	// class OR the measured box-transition family (lb < 0.08 keeps out the
	// letterboxed system screens; the settled dark box band 46-64 is below
	// the bright floor, and the legend's box band 47-69, so only the
	// box's own transition family matches).
	cEnd, _ := ScreenSigOf(end).ClassOf()
	restoreFrame := func(img image.Image) (bool, string) {
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		bb := sig.Regions["boxband"]
		var accepted bool
		if cEnd == ClassBox {
			accepted = c == ClassBox ||
				(sig.Letterbox < classBoxDarkLbMax && bb.mean >= boxBandMeanMin)
		} else {
			// X is the legend's own measured family (the end assertion's
			// legendFrame predicate: a text-bearing frame or a measured
			// interlude — the legend advances on its own).
			accepted, _ = legendFrame(img)
		}
		return accepted, fmt.Sprintf("class=%s lb=%.3f boxband[mean=%.0f edges=%.3f] (X's class %s)", c, sig.Letterbox, bb.mean, bb.edges, cEnd)
	}
	_, restoreOK, restoreDetail, restoreFrames := pollSettled(t, ctx, b, x1, settleRecheckWindow, settleRecheckInterval, restoreFrame)
	if !restoreOK {
		t.Fatalf("savestate: the restored scene is not X's scene: %s (no frame accepted in %d of the %s recheck window)", restoreDetail, restoreFrames, settleRecheckWindow)
	}
	t.Logf("savestate: restored scene is X's scene again — %s (accepted at frame %d of the %s recheck window)", restoreDetail, restoreFrames, settleRecheckWindow)

	// Phase 3: a fresh boot with --save_state lands on the drive's SUCCESS
	// scene (the saved state is exactly the X frame the drive reached): the
	// first in-game dialogue box (the past-legend drive's goal — a stable
	// measured screen: the dense recording held it unchanged for 10 minutes,
	// so the boot lands on the box within the settle) or, on a budget close
	// before the box, the letterboxed legend with its narration subtitle
	// (the story_subtitle class, measured bottom-bar text). The state file
	// is the measured path from the save hotkey; the boot takes over the
	// game's timeline at the save point, so the success screen is present
	// in the first seconds.

	// Phase 3: a fresh boot with --save_state lands on X (the success scene
	// within 30 s).
	b.Stop()
	if err := strayDolphinProcesses(); err != nil {
		t.Fatalf("after the phase-2 stop: %v", err)
	}
	if err := b.Launch(ctx, LaunchSpec{SaveState: statePath}); err != nil {
		t.Fatalf("relaunch with --save_state: %v", err)
	}
	// The boot lands on the drive's SUCCESS scene: the first in-game
	// dialogue box (the past-legend drive's goal — BoxPresent's measured
	// dark-box form, ADR-0054 amendment 1) or the letterboxed legend
	// (story_subtitle — the same measured bottom-bar text clauses the end
	// assertion uses; BoxPresent's box-band calibration cannot match the
	// letterboxed legend, measured boxband mean 60 < 100).
	sceneName := "dialogue box"
	if drv.SuccessScreen == DriveScreenStorySubtitle {
		sceneName = "the opening legend (story subtitle)"
	}
	deadline := time.Now().Add(30 * time.Second)
	bootFrames := 0
	for {
		shot, serr := b.Screenshot(ctx)
		if serr == nil {
			bootFrames++
			var ok bool
			var detail string
			if drv.SuccessScreen == DriveScreenStorySubtitle {
				sig := ScreenSigOf(shot)
				c, _ := sig.ClassOf()
				sub := sig.Regions["subbar"]
				ok = c == ClassStorySub &&
					sig.Letterbox < classStorySubLbMax &&
					sub.bright >= classStorySubBrightMin &&
					sub.edges >= classStorySubEdgesMin
				detail = fmt.Sprintf("class=%s lb=%.3f subbar bright=%.3f edges=%.3f", c, sig.Letterbox, sub.bright, sub.edges)
			} else {
				ok, detail = BoxPresent()(ctx, shot, shot)
			}
			if ok {
				t.Logf("savestate: --save_state boot landed on X (%s) at frame %d of the 30 s recheck window — %s", sceneName, bootFrames, detail)
				if evOut != "" {
					writeEvidencePNG(t, evOut, "savestate-boot-X.png", shot)
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("savestate: no %s within 30 s of the --save_state boot (%d frames; the state file was %s)", sceneName, bootFrames, statePath)
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
	// parser drops it (the keyboard hotkey device included, approach D).
	st2 := b.State()
	checkExitRewriteFiles(t, b, st2.UserDir, st2.PipeDevice, st2.HotkeyDevice)
}

// restoreSettle is the fixed total settle after a LoadState in the
// double-restore round trip: LoadState itself waits hotkeyLoadSettle (3 s),
// and the test sleeps the remainder. Both restores use the SAME total, so
// the two X frames are the restored timeline at the SAME offset — the
// rmse(X1, X2) comparison is deterministic (llvmpipe render noise + a small
// screenshot jitter, both far below the control threshold; the measured
// animation rate of the restored legend is ~0.07 RMSE/s, run-drive3).
// settleRecheckWindow / settleRecheckInterval bound the post-drive rechecks
// (the end assertion above and the --save_state boot landing below): the
// success screen is still settling when the drive's last poll fires (the
// box's typewriter reveal, the legend's own subtitle fades), so a single
// frame sampled at machine timing is not a stable proof (acceptance.md gap
// 12). The rechecks poll at the interval and stop at the first frame the
// same measured clauses accept.
const (
	settleRecheckWindow   = 10 * time.Second
	settleRecheckInterval = 1 * time.Second
	// boxRecheckWindow bounds the end assertion's box recheck: the measured
	// 20-40 s gap from the drive's stop frame to the first NPC dialogue box
	// (the held box) — the brief box and the bright-form scene frames the
	// drive can stop on are NOT the box the assertion proves — plus margin.
	boxRecheckWindow = 120 * time.Second
)

// pollSettled re-screenshots over a bounded window, starting from the frame
// the caller already captured (counted as frame 1), and returns the FIRST
// frame the ok predicate accepts (the same measured clauses as the drive's
// success gates — no looser recheck), whether a frame was accepted, the
// detail line of the frame returned, and the number of frames sampled. A
// screenshot error inside the window keeps the previous frame (a capture
// hiccup is not a screen change); a cancelled context returns the last
// frame with ok=false.
func pollSettled(t *testing.T, ctx context.Context, b Bridge, first image.Image, window, interval time.Duration, ok func(image.Image) (bool, string)) (image.Image, bool, string, int) {
	t.Helper()
	okFlag, detail := ok(first)
	if okFlag {
		return first, true, detail, 1
	}
	deadline := time.Now().Add(window)
	frame := first
	n := 1
	for {
		select {
		case <-ctx.Done():
			return frame, false, "context done: " + ctx.Err().Error(), n
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return frame, false, detail, n
		}
		shot, err := b.Screenshot(ctx)
		if err != nil {
			t.Logf("settle recheck: frame %d: screenshot error (keeping frame %d): %v", n+1, n, err)
			continue
		}
		n++
		frame = shot
		if okFlag, detail = ok(shot); okFlag {
			return frame, true, detail, n
		}
		t.Logf("settle recheck: frame %d not accepted: %s", n, detail)
	}
}

const restoreSettle = 8 * time.Second

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
	entries, _ := filepath.Glob(filepath.Join(userDir, stateSavesDir, "*"))
	t.Logf("savestate: StateSaves dir: %v", entries)
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
