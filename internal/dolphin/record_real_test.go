//go:build unix

package dolphin

// record_real_test.go is the M6 recorder gate (docs/spec/12 "Dolphin emulator
// mode"): it boots the REAL user-supplied image, runs the screen-driven
// new-game drive (WindWakerNewGameDrive) through the fixture recorder, then
// rides the WHOLE opening legend with WatchLegend under the parallel
// adaptive capture (denseCapturePlan, ≥10 Hz on the subtitle scenes), and
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
//
// The recording rides the WHOLE legend at the dense rate on purpose (the
// 2026-10-20 bench decision, ADR-0124): the original fixture sampled on the
// drive's ~2-3 s grid, so a legend line could appear in a SINGLE frame and
// the recognition tracker (which needs ≥2 observations — a STABILIZING frame
// plus one more for STABILIZING→READY) can never commit it. The bench then
// graded those single-frame lines as recognition misses (the 50% newgame
// holdout) although they are sampling artifacts, not pipeline failures
// (record_dense.go's header). Every legend line sampled at ≥10 Hz is
// committable, so the holdout carries the legend's full labelled line set.

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
	skipUnlessTestDolphinGate(t)
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
	// The dense holdout (ADR-0124): the parallel adaptive capture runs the
	// whole recording, so the legend lines land at ≥10 Hz (a line seen in
	// ≥2 frames is committable by the recognition tracker) instead of on
	// the drive's ~2-3 s grid (a line in a single frame is never
	// committable — the 50% holdout the bench decision replaced).
	rec.SetCapturePlan(denseCapturePlan)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	st := rec.State()
	t.Logf("launched: display=%s dolphin_pid=%d image_sha256=%s (the image path is never logged)", st.Display, st.DolphinPID, st.ImageSHA)

	// The parallel adaptive capture starts BEFORE the drive (the dense
	// fixture's proven order, record_dense_test.go): the plan is adaptive
	// (≥10 Hz on the subtitle scenes, the low rate elsewhere), so the
	// drive-cadence screens record cheaply and the legend lines land at
	// the committable rate. Without the StartCapture the manifest's
	// capture block is absent and the holdout's committability is lost.
	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}

	// The screen-driven drive through the recorder: every iteration's
	// screenshot becomes a fixture frame, every input a logged wire event.
	drv, err := RunDrive(ctx, rec, DriveOptions{}, WindWakerNewGameDrive())
	for _, tr := range drv.Transitions {
		t.Logf("iter %02d: %s:%s --(%s)→", tr.Index, tr.Screen, tr.Side, tr.Action)
	}
	t.Logf("drive: ok=%v reason=%q elapsed=%s", drv.OK, drv.Reason, drv.Finished.Sub(drv.Started).Round(time.Second))
	if !drv.OK {
		rec.StopCapture()
		// The fixture is still honest (it records what happened), but the
		// M6 scenario set is authored against a SUCCESSFUL drive to the
		// legend narration — fail the gate with the drive's own report.
		t.Fatalf("drive to the legend narration stopped: %s (fixture kept at %s for inspection)", drv.Reason, rec.Dir())
	}
	if drv.SuccessScreen != DriveScreenStorySubtitle && drv.SuccessScreen != DriveScreenDialogueBox {
		t.Fatalf("drive success screen = %q, want the opening legend's narration subtitle or the first dialogue box", drv.SuccessScreen)
	}

	// Phase 2: ride the WHOLE opening legend (ADR-0124). The drive left the
	// recorder on the first subtitle; the capture loop keeps the ≥10 Hz
	// recording going and the watch ends it when the measured gameplay
	// screen appears (or the budget spends). Without this the fixture holds
	// only the first one or two legend lines (the drive's observation
	// window), and the bench holdout grades the recording's truncated
	// single-frame tail line as a recognition miss.
	watch, err := WatchLegend(ctx, rec, 15*time.Minute)
	t.Logf("watch: %+v", watch)
	if err != nil {
		rec.StopCapture()
		t.Fatalf("legend watch: %s (fixture kept at %s for inspection)", err, rec.Dir())
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
	// The dense property (ADR-0124): the manifest's capture block states the
	// rate; the bench holdout's committability depends on it.
	if summary.Capture == nil || !summary.Capture.Enabled {
		t.Fatal("the manifest's capture block is missing or not enabled: the dense recording did not run the parallel capture")
	}
	if summary.Capture.HighHz < 10 {
		t.Fatalf("capture plan high rate = %g Hz, want >= 10 (the holdout's committability requirement, ADR-0124)", summary.Capture.HighHz)
	}
	// The verification gate the replay side runs — exercised here so a
	// corrupt recording fails at record time, not in a scenario.
	if _, err := VerifyFixture(rec.Dir()); err != nil {
		t.Fatalf("VerifyFixture (fresh): %v", err)
	}
}
