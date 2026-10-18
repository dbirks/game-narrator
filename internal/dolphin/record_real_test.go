//go:build unix

package dolphin

// record_real_test.go is the M6 recorder gate (docs/spec/12 "Dolphin emulator
// mode"): it boots the REAL user-supplied image, runs the screen-driven
// new-game drive (WindWakerNewGameDrive) through the fixture recorder, and
// finalizes the replayable fixture under $GAME_NARRATOR_FIXTURE_DIR (default
// the gitignored artifacts/fixtures/, AGENTS.md rule 5). NOT RUN when the
// image or the host tools are missing — and never labelled as unit coverage.
//
// The fixture this test records is what the committed scenarios under
// testdata/scenarios/dolphin reference by id + manifest hash: after the run,
// the manifest's SHA-256 (printed below) is the value those scenarios carry.
// Re-running the test re-records the fixture (the drive's frames are what
// they are on this host); a re-recorded fixture changes the manifest hash and
// the scenarios report NOT RUN until they are re-authorized against it.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// FixtureIDWindWaker is the fixture id the M6 new-game drive records: boot to
// the opening legend's narration subtitle.
const FixtureIDWindWaker = "windwaker-newgame"

// fixtureRootForTest resolves the fixture root the way the replay side does:
// the env var wins, otherwise the gitignored artifacts/fixtures/ under the
// repository root (the test's CWD is internal/dolphin, so the CWD-relative
// default would land in the wrong tree).
func fixtureRootForTest(t *testing.T) string {
	t.Helper()
	if v := os.Getenv(EnvFixtureDir); v != "" {
		return v
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "artifacts", "fixtures")
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s: cannot locate the repository root", cwd)
		}
	}
}

func TestRealRecordFixture(t *testing.T) {
	img := os.Getenv(EnvGameImage)
	if img == "" {
		t.Skipf("NOT RUN: %s is not set — point it at the user-supplied game image (dev-only, the image is never logged)", EnvGameImage)
	}
	for _, bin := range []string{"dolphin-emu", "Xvfb", "ffmpeg", "xwininfo", "tesseract"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("NOT RUN: %s is not on PATH (dev host dependencies: dolphin-emu, xorg-server-xvfb, ffmpeg, x11-utils, tesseract)", bin)
		}
	}
	if err := strayDolphinProcesses(); err != nil {
		t.Skipf("NOT RUN: %v", err)
	}

	root := fixtureRootForTest(t)

	// Golden-file semantics (ADR-0053): a fixture that self-verifies is a
	// stable artifact — the gate verifies it (frames against the on-disk
	// manifest) instead of re-recording, so the dolphin scenarios' pinned
	// manifest hashes stay valid across gate runs. A missing or corrupt
	// fixture falls through to a fresh recording; its NEW manifest hash is
	// then what the scenarios' pins must be updated to (the run's log line
	// carries it). Delete the fixture directory to force a re-record.
	if man, err := VerifyFixture(filepath.Join(root, FixtureIDWindWaker)); err == nil {
		if sha, err := ManifestSHA256(filepath.Join(root, FixtureIDWindWaker)); err == nil {
			t.Logf("fixture present and verified: frames=%d manifest_sha256=%s — NOT re-recorded", len(man.Frames), sha)
			return
		}
	}

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Stop()

	rec, err := NewFixtureRecorder(b, root, FixtureIDWindWaker)
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	st := rec.State()
	t.Logf("launched: display=%s dolphin_pid=%d image_sha256=%s (the image path is never logged)", st.Display, st.DolphinPID, st.ImageSHA)

	// The screen-driven drive through the recorder: every iteration's
	// screenshot becomes a fixture frame, every input a logged wire event.
	drv, err := RunDrive(ctx, rec, DriveOptions{}, WindWakerNewGameDrive())
	for _, tr := range drv.Transitions {
		t.Logf("iter %02d: %s:%s --(%s)→", tr.Index, tr.Screen, tr.Side, tr.Action)
	}
	t.Logf("drive: ok=%v reason=%q elapsed=%s", drv.OK, drv.Reason, drv.Finished.Sub(drv.Started).Round(time.Second))
	if !drv.OK {
		// The fixture is still honest (it records what happened), but the
		// M6 scenario set is authored against a SUCCESSFUL drive to the
		// legend narration — fail the gate with the drive's own report.
		t.Fatalf("drive to the legend narration stopped: %s (fixture kept at %s for inspection)", drv.Reason, rec.Dir())
	}
	if drv.SuccessScreen != DriveScreenStorySubtitle && drv.SuccessScreen != DriveScreenDialogueBox {
		t.Fatalf("drive success screen = %q, want the opening legend's narration subtitle or the first dialogue box", drv.SuccessScreen)
	}

	summary, err := rec.Finalize(ctx, drv)
	if err != nil {
		t.Fatalf("Finalize: %v", err)
	}
	t.Logf("fixture: dir=%s frames=%d lines=%d manifest_sha256=%s", summary.Dir, summary.Frames, summary.Lines, summary.ManifestSHA256)
	t.Logf("fixture: update the dolphin scenarios' input.manifest_sha256 to the value above if you re-recorded the fixture")
	if summary.Lines == 0 {
		t.Fatal("the fixture recorded no narration line: Tesseract read nothing on the subtitle frames (check the OCR recipe / the drive's subtitle span)")
	}
	if summary.Frames < 10 {
		t.Fatalf("the fixture has %d frames, want a drive recording (>= 10 iterations)", summary.Frames)
	}
	// The verification gate the replay side runs — exercised here so a
	// corrupt recording fails at record time, not in a scenario.
	if _, err := VerifyFixture(rec.Dir()); err != nil {
		t.Fatalf("VerifyFixture (fresh): %v", err)
	}
}
