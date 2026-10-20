//go:build unix

package dolphin

import (
	"context"
	"fmt"
	"image"
	"math/bits"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRealSavestateRoundTripGameplay is the real round trip issue #12 asks
// for: a savestate written by the harness MID-GAME (the deck scene, after
// the first lookout dialogue is closed) must restore into the SAME scene on
// a fresh boot. The path mirrors the issue's reproduction exactly:
//
//  1. boot from the proven windwaker-first-dialogue state (the ONLY state
//     that restored before this fix — the fixture's, recorded by the M6
//     gate);
//  2. advance the whole dialogue to the gameplay scene with the screen-
//     driven CLOSE step (A on the 2.5 s cadence while the screen is not the
//     settled full-frame scene; the boot guard + three-sample band window
//     of CloseDetector), capturing the saved scene's reference frame X;
//  3. save the deck state on slot 1 (the F1 hotkey), validate its header
//     (the game ID + the Dolphin state version — the two inputs 2609's
//     ValidateHeaders silently rejects with the fresh-boot fallback);
//  4. stop Dolphin (the kill of the reproduction);
//  5. a FRESH boot (--save_state on a new isolated user dir);
//  6. assert the restored frame matches the saved scene: the perceptual
//     luma RMSE against X within the control tolerance (the harness's own
//     metric — the measured render-to-render noise is <= 0.025, any other
//     scene measures >= 0.05 away) and the full-frame band (the boot's
//     black/copyright/title frames are not the scene).
//
// The failure evidence on a broken restore is the silent-fallback
// diagnosis (issue #12's suspects): the emulator log tails of BOTH sessions
// (2609 prints the load verdict there in --batch mode), the state header
// facts, and the landing frame's measured class. NOT RUN when the image or
// the host tools are missing — and never labelled as unit coverage.
//
// restoreTol is the perceptual tolerance for the restored-vs-saved frame
// comparison: crossStepMax (0.10) is the harness's control threshold for
// "the same settled scene" (readiness.go), measured against the double-
// restore's 0.000 (llvmpipe renders byte-identical frames) and the
// scene-change floor of 0.05.
const restoreTol = crossStepMax

// restoreHashTol is the maximum Hamming distance (out of the 64 bits of the
// aHash64 perceptual hashes) between the saved scene X and the restored
// frame. Same-scene frames measure within a few bits (the deck is stable and
// llvmpipe renders near-identical frames); a different scene (the title boot
// screen) flips most of the 64 bits. 12 sits between the two with margin on
// both sides.
const restoreHashTol = 12

// aHash64 is a coarse perceptual hash (average hash): the frame's luma is
// block-averaged to 8x8 and each cell is thresholded against the block's
// mean, yielding a 64-bit fingerprint. It is the "perceptual hash" half of
// the round-trip assertion (independent of the RMSE metric): two frames of
// the same scene hash within a small Hamming distance even when the renders
// differ slightly; different scenes hash far apart.
func aHash64(img image.Image) uint64 {
	w, h, px := luma(img)
	if px == nil {
		return 0
	}
	const n = 8
	cell := make([]float64, n*n)
	cnt := make([]float64, n*n)
	for y := 0; y < h; y++ {
		gy := y * n / h
		for x := 0; x < w; x++ {
			gx := x * n / w
			i := gy*n + gx
			cell[i] += float64(px[y*w+x])
			cnt[i]++
		}
	}
	var m float64
	for i := 0; i < n*n; i++ {
		if cnt[i] > 0 {
			m += cell[i] / cnt[i]
		}
	}
	m /= float64(n * n)
	var bitsv uint64
	for i := 0; i < n*n; i++ {
		if cnt[i] > 0 && cell[i]/cnt[i] > m {
			bitsv |= 1 << uint(n*n-1-i)
		}
	}
	return bitsv
}

// hamming counts the differing bits between two 64-bit hashes.
func hamming(a, b uint64) int {
	return bits.OnesCount64(a ^ b)
}

func TestRealSavestateRoundTripGameplay(t *testing.T) {
	skipUnlessTestDolphinGate(t)
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "xdotool"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, xdotool)", bin)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	root := fixtureRootForTest(t)
	fixtureState := filepath.Join(root, StateRelFirstDialogue)
	if _, err := os.Stat(fixtureState); err != nil {
		t.Skipf("NOT RUN: the %s fixture state is missing (%v) — record it first via TestRealRecordSavestateFixtures (GN_GATE=test-dolphin)", StateRelFirstDialogue, err)
	}

	evOut := filepath.Join("artifacts", "runs", "dolphin-savestate-"+time.Now().UTC().Format("2006-01-02-150405"))
	if err := os.MkdirAll(evOut, 0o755); err != nil {
		t.Fatalf("evidence dir %s: %v", evOut, err)
	}
	t.Logf("evidence: %s", evOut)

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	defer b.Stop()

	// Session 1: the restore of the proven first-dialogue state, the CLOSE
	// advance to the gameplay scene, and the deck-state save.
	if err := b.Launch(ctx, LaunchSpec{SaveState: fixtureState, Hotkeys: true}); err != nil {
		t.Fatalf("launch (save_state=first-dialogue, hotkeys): %v", err)
	}
	st := b.State()
	t.Logf("session 1: display=%s dolphin_pid=%d user_dir=%s hotkey_device=%s", st.Display, st.DolphinPID, st.UserDir, st.HotkeyDevice)

	// The first dialogue box (the state's own screen — the fixture's dense
	// recording holds it unchanged for 10+ minutes, so the restore lands on
	// it inside the budget; the box's typewriter reveal needs its 1-2 s).
	boxDeadline := time.Now().Add(10 * time.Minute)
	boxSeen := false
	for !boxSeen {
		if time.Now().After(boxDeadline) {
			dumpSavestateFailureDiagnosis(t, st, "the first dialogue box never appeared within 10 m of the first-dialogue restore")
			t.Fatalf("first dialogue box did not appear within 10 m of the %s restore (the fixture state itself failed to land on the box — the reproduction cannot start)", StateRelFirstDialogue)
		}
		shot, err := b.Screenshot(ctx)
		if err == nil {
			if sig := ScreenSigOf(shot); sig.RealBoxForm() {
				boxSeen = true
				t.Logf("box: %s", sig.SigLine())
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}

	// The CLOSE step: A on the 2.5 s cadence while the screen is not the
	// settled full-frame gameplay scene (the measured recipe — the boot
	// guard gates detection only, so the dialogue advances through it).
	xRef, err := CloseAdvance(ctx, b, evOut)
	if err != nil {
		dumpSavestateFailureDiagnosis(t, st, "CLOSE advance failed: "+err.Error())
		t.Fatalf("CLOSE: %v", err)
	}
	xSig := ScreenSigOf(xRef)
	xHash := aHash64(xRef)
	t.Logf("gameplay scene (the saved scene X): %s", xSig.SigLine())
	t.Logf("saved scene X perceptual hash: %016x", xHash)
	writeEvidencePNG(t, evOut, "deck-x-saved-scene.png", xRef)

	// The deck-state save (the F1 hotkey): slot 1, mtime-verified.
	statePath, err := b.SaveState(ctx, stateSaveSlot)
	if err != nil {
		dumpSavestateFailureDiagnosis(t, st, "deck save failed: "+err.Error())
		t.Fatalf("deck save: %v", err)
	}
	t.Logf("deck state saved (slot 1): %s", statePath)
	// The header oracle (issue #12's silent-fallback suspects, checked
	// BEFORE the fresh boot spends 20 minutes on it): the state must name
	// the same game and the same Dolphin state version the boot will run.
	hdr, err := ParseStateHeader(statePath, "GZLE01")
	if err != nil {
		dumpSavestateFailureDiagnosis(t, st, "deck state header: "+err.Error())
		t.Fatalf("deck state header: %v", err)
	}
	t.Logf("deck state header: game=%s version=%q cookie=%#x compression=%d uncompressed=%d file=%d",
		hdr.GameID, hdr.VersionString, hdr.VersionCookie, hdr.Compression, hdr.UncompressedSize, hdr.FileSize)

	// The kill of the reproduction: stop session 1 (graceful; the abrupt-
	// kill contract is separately proven by TestRealAbruptContextKill).
	b.Stop()
	if err := strayDolphinProcesses(); err != nil {
		t.Fatalf("after the session-1 stop: %v", err)
	}

	// Session 2: the FRESH boot from the deck state (a new isolated user
	// dir; the --save_state path the reproduction uses).
	if err := b.Launch(ctx, LaunchSpec{SaveState: statePath}); err != nil {
		t.Fatalf("fresh boot (save_state=deck state): %v", err)
	}
	st2 := b.State()
	t.Logf("session 2: display=%s dolphin_pid=%d user_dir=%s", st2.Display, st2.DolphinPID, st2.UserDir)

	// The landing: the restored frame must match the saved scene X — the
	// perceptual RMSE within the control tolerance AND the full-frame band
	// (the boot's black/copyright/title frames fail both clauses).
	landingDeadline := time.Now().Add(3 * time.Minute)
	var lastDetail string
	for {
		if time.Now().After(landingDeadline) {
			dumpSavestateFailureDiagnosis(t, st2, "fresh boot did not land on the saved scene within 3 m")
			t.Fatalf("issue #12 reproduced: the fresh --save_state boot from the deck state did NOT land on the saved scene within 3 m (last: %s)", lastDetail)
		}
		shot, err := b.Screenshot(ctx)
		if err == nil {
			sig := ScreenSigOf(shot)
			c, _ := sig.ClassOf()
			d := rmse01(shot, xRef, normW, normH)
			dHash := hamming(xHash, aHash64(shot))
			lastDetail = fmt.Sprintf("class=%s lb=%.3f mean=%.1f rmse(vs X)=%.3f hashdist=%d", c, sig.Letterbox, sig.Mean, d, dHash)
			if d <= restoreTol && dHash <= restoreHashTol && sig.Letterbox > closeBandMin {
				t.Logf("RESTORED: %s — the restored frame matches the saved scene (rmse %.3f <= %.2f, perceptual-hash dist %d <= %d, full-frame band %.3f > %.2f)",
					lastDetail, d, restoreTol, dHash, restoreHashTol, sig.Letterbox, closeBandMin)
				writeEvidencePNG(t, evOut, "restored-deck.png", shot)
				// The negative clause the issue's failure mode names: the
				// landing must NOT be the copyright/title boot screen (the
				// silent-fallback symptom). The RMSE clause already implies
				// it (the title measures far from the dark deck scene), but
				// the class names the failure for the report.
				if c == ClassTitle {
					t.Fatalf("the fresh boot landed on the TITLE screen (rmse %.3f <= %.2f against X is impossible on the title — the X reference is wrong)", d, restoreTol)
				}
				break
			}
			t.Logf("landing sample: %s", lastDetail)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("context done: %v", ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}

	// Session 2's exit-rewrite check (the config the fresh boot kept is the
	// proven layout — the memcard binding included).
	checkExitRewriteFiles(t, b, st2.UserDir, st2.PipeDevice, st2.HotkeyDevice)
}

// dumpSavestateFailureDiagnosis is the issue #12 failure contract: when a
// savestate phase fails, the evidence that separates the suspects (load
// timing vs slot-vs-path vs version vs wait-for-game-frame) is the
// EMULATOR'S OWN VERDICT — 2609 prints the load verdict to the console in
// --batch mode ("Loaded State from ...", "The savestate could not be
// loaded", "State belongs to a different game (ID ...)", the LZ4
// decompression errors) and to the file log — plus the config it booted
// with (the --save_state path itself). Paths only; never state bytes
// (they embed game RAM).
func dumpSavestateFailureDiagnosis(t *testing.T, st Status, why string) {
	t.Helper()
	t.Logf("savestate failure diagnosis: %s", why)
	for _, rel := range []string{"bridge-emu.log", filepath.Join("Logs", "dolphin.log")} {
		p := filepath.Join(st.UserDir, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Logf("savestate failure diagnosis: %s absent: %v", rel, err)
			continue
		}
		lines := []string{}
		for _, l := range splitLines(string(b)) {
			// Keep only the load-relevant lines (the verdict, the boot
			// args, the state/savestate mentions) — the full log is MBs of
			// shader compilation.
			keep := false
			lower := strings.ToLower(l)
			for _, pat := range []string{"save_state", "save state", "savestate", "loaded state", "could not be loaded", "different game", "incompatible version", "lz4", "decompressing", "exec="} {
				if strings.Contains(lower, pat) {
					keep = true
					break
				}
			}
			if keep {
				lines = append(lines, l)
			}
			if len(lines) > 40 {
				break
			}
		}
		if len(lines) == 0 {
			t.Logf("savestate failure diagnosis: %s carries no load-verdict lines (tail):\n%s", rel, tailLines(string(b), 12))
		} else {
			t.Logf("savestate failure diagnosis: %s load lines:\n%s", rel, joinNewline(lines))
		}
	}
	// The config the session wrote (the --save_state path and the boot
	// args are in bridge-emu.log; the Dolphin.ini shows the core config).
	if ini, err := os.ReadFile(filepath.Join(st.UserDir, "Config", "Dolphin.ini")); err == nil {
		t.Logf("savestate failure diagnosis: Dolphin.ini:\n%s", string(ini))
	}
}

func splitLines(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == '\n' {
			out = append(out, cur)
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}

func tailLines(s string, n int) string {
	lines := splitLines(s)
	if len(lines) <= n {
		return joinNewline(lines)
	}
	return joinNewline(lines[len(lines)-n:])
}

func joinNewline(lines []string) string {
	out := ""
	for i, l := range lines {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
