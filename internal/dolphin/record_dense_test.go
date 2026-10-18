//go:build unix

package dolphin

// record_dense_test.go is the real gate for the dense fixture (ADR-0058
// amendment 1): it boots the REAL user-supplied image, runs the
// screen-driven new-game drive (WindWakerNewGameDrive) through the fixture
// recorder with the parallel adaptive capture armed (≥10 Hz on the
// subtitle scenes, 1 Hz elsewhere, bounded by the denseCapturePlan
// budget), then rides the WHOLE opening legend with WatchLegend and
// finalizes the replayable fixture under $GAME_NARRATOR_FIXTURE_DIR
// (default the gitignored artifacts/fixtures/, AGENTS.md rule 5). NOT RUN
// when the image or the host tools are missing — and never labelled as
// unit coverage.
//
// Golden-file semantics (ADR-0053): a fixture that self-verifies is a
// stable artifact — the gate verifies it instead of re-recording, so the
// scenarios' pinned manifest hashes stay valid across gate runs. A missing
// or corrupt fixture falls through to a fresh recording; its NEW manifest
// hash is what the scenarios' pins must be updated to.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestRealRecordLegendDense(t *testing.T) {
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

	if man, err := VerifyFixture(filepath.Join(root, FixtureIDWindWakerDense)); err == nil {
		if sha, err := ManifestSHA256(filepath.Join(root, FixtureIDWindWakerDense)); err == nil {
			t.Logf("fixture present and verified: frames=%d capture=%+v manifest_sha256=%s — NOT re-recorded",
				len(man.Frames), man.Capture, sha)
			return
		}
	}

	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer b.Stop()

	rec, err := NewFixtureRecorder(b, root, FixtureIDWindWakerDense)
	if err != nil {
		t.Fatalf("NewFixtureRecorder: %v", err)
	}
	rec.SetCapturePlan(denseCapturePlan)

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Minute)
	defer cancel()
	if err := rec.Launch(ctx, LaunchSpec{}); err != nil {
		t.Fatalf("Launch (real): %v", err)
	}
	st := rec.State()
	t.Logf("launched: display=%s dolphin_pid=%d image_sha256=%s (the image path is never logged)", st.Display, st.DolphinPID, st.ImageSHA)

	if err := rec.StartCapture(ctx); err != nil {
		t.Fatalf("StartCapture: %v", err)
	}

	// Phase 1: the standard screen-driven drive to the legend's narration.
	drv, err := RunDrive(ctx, rec, DriveOptions{}, WindWakerNewGameDrive())
	for _, tr := range drv.Transitions {
		t.Logf("iter %02d: %s:%s --(%s)→", tr.Index, tr.Screen, tr.Side, tr.Action)
	}
	t.Logf("drive: ok=%v reason=%q elapsed=%s", drv.OK, drv.Reason, drv.Finished.Sub(drv.Started).Round(time.Second))
	if !drv.OK {
		rec.StopCapture()
		t.Fatalf("drive to the legend narration stopped: %s (fixture kept at %s for inspection)", drv.Reason, rec.Dir())
	}

	// Phase 2: ride the WHOLE opening legend. The drive left the recorder on
	// the first subtitle; the capture loop keeps the ≥10 Hz recording going
	// and the watch ends it when the measured gameplay screen appears (or
	// the budget spends — the measured legend runs 40+ s of emulation, so
	// 15 min is a wide fence, not the expected length). The watch samples
	// through the recorder, so its samples are fixture frames too
	// (interleaved in capture order).
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
	t.Logf("fixture: dir=%s frames=%d lines=%d capture=%+v manifest_sha256=%s",
		summary.Dir, summary.Frames, summary.Lines, summary.Capture, summary.ManifestSHA256)
	t.Logf("fixture: update the dolphin scenarios' input.manifest_sha256 to the value above if you re-recorded the fixture")
	if summary.Lines == 0 {
		t.Fatal("the fixture recorded no narration line: Tesseract read nothing on the subtitle frames (check the OCR recipe / the drive's subtitle span)")
	}
	// The dense property the ADR's adjustment clause exists for: the legend
	// subtitles must be sampled densely, not on the drive's 2 s grid (a line
	// seen in ≥2 frames is committable by the tracker). The manifest is the
	// record: the capture block states the rate, the frame labels the
	// subtitle spans.
	if summary.Capture == nil || !summary.Capture.Enabled {
		t.Fatal("the manifest's capture block is missing or not enabled: the dense recording did not run the parallel capture")
	}
	if summary.Capture.HighHz < 10 {
		t.Fatalf("capture plan high rate = %g Hz, want >= 10 (the task's dialogue requirement)", summary.Capture.HighHz)
	}
	man, err := VerifyFixture(rec.Dir())
	if err != nil {
		t.Fatalf("VerifyFixture (fresh): %v", err)
	}
	subFrames := 0
	for _, fr := range man.Frames {
		if fr.Screen == string(DriveScreenStorySubtitle) {
			subFrames++
		}
	}
	t.Logf("fixture: %d frames measured the story-subtitle class (of %d total)", subFrames, len(man.Frames))
	if subFrames == 0 {
		t.Fatal("no frame measured the story-subtitle class: the legend span was not recorded")
	}
}
