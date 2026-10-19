//go:build unix

package dolphin

// savestate_fixture_test.go is the M6 real gate for the savestate
// checkpoints (ADR-0054 amendment 1): it boots the REAL user-supplied image,
// drives it with the screen-driven new-game drive, and saves two savestates —
// the opening legend's first subtitle (checkpoint "legend_start") and the
// first in-game dialogue box (checkpoint "first_dialogue") — under the
// fixture root's gitignored states/ dir. From those states it records the
// two dense fixtures the M6 scenarios replay:
//
//   - windwaker-legend-press: the legend ridden from legend_start with a
//     PRESS PACED advance — one A press when each subtitle's typewriter has
//     settled (the per-subtitle press intervals are MEASURED from this run:
//     the inputs block timestamps every press against the capture frames);
//     the fixture ends where the first dialogue box appears.
//   - windwaker-first-dialogue: loaded from first_dialogue, the box's
//     typewriter reveal, one A press per settled line (advance to the next
//     box), and the box's close — dense (≥10 Hz) throughout.
//
// The savestates are referenced in the fixtures' manifests by SHA-256 only
// (the files live under $GAME_NARRATOR_FIXTURE_DIR/states/, gitignored,
// AGENTS.md rule 5).
//
// Golden-file semantics (ADR-0053): fixtures that self-verify (frames hash,
// states hash) are stable artifacts — the gate verifies them instead of
// re-recording, so the scenarios' pinned manifest hashes stay valid across
// gate runs. A missing or corrupt fixture re-records from the savestates;
// its NEW manifest hash is what the scenarios' pins must be updated to.
//
// NOT RUN when the image or the host tools are missing — and never labelled
// as unit coverage.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"image"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

const (
	// FixtureIDLegendPress is the press-paced legend fixture id.
	FixtureIDLegendPress = "windwaker-legend-press"
	// FixtureIDFirstDialogue is the first in-game dialogue fixture id.
	FixtureIDFirstDialogue = "windwaker-first-dialogue"

	// StateRelLegendStart / StateRelFirstDialogue are the savestate file
	// names under the fixture root's states/ dir (gitignored, AGENTS.md
	// rule 5 — referenced by the fixtures' manifests by hash). Both
	// checkpoints ride the ONE hotkey-bound savestate slot (the keyboard
	// layout binds slot 1 only, ADR-0043): the slot is saved twice and each
	// save is copied to its stable file before the next save overwrites it.
	StateRelLegendStart   = "states/windwaker-legend-start.state"
	StateRelFirstDialogue = "states/windwaker-first-dialogue.state"
	stateSaveSlot         = 1 // the hotkey-bound slot (both checkpoints)
)

// TestRealRecordSavestateFixtures is the M6 savestate checkpoint + fixture
// recording gate (real Dolphin only; NOT RUN without the image/tools).
func TestRealRecordSavestateFixtures(t *testing.T) {
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

	// Golden check: both fixtures verified (frame hashes) and their
	// referenced savestates present and hashing to the manifest's claim →
	// NOT re-recorded (ADR-0053).
	if ok, why := verifiedFixturePair(t, root); ok {
		t.Logf("fixture pair present and verified (states: %s) — NOT re-recorded", why)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()

	// Phase 1: the two savestate checkpoints (one cold launch, then a
	// re-launch from checkpoint one ridden wait-only to checkpoint two).
	legendState, dialogueState, err := recordSavestates(ctx, t, root)
	if err != nil {
		t.Fatalf("savestates: %v (states kept at %s for inspection)", err, filepath.Join(root, "states"))
	}
	t.Logf("savestates: legend_start=%s first_dialogue=%s", legendState, dialogueState)

	// Phase 2: the press-paced legend fixture, loaded from legend_start.
	pressSummary, err := recordLegendPressFixture(ctx, t, root, legendState)
	if err != nil {
		t.Fatalf("legend-press fixture: %v (fixture kept at %s for inspection)", err, filepath.Join(root, FixtureIDLegendPress))
	}

	// Phase 3: the first-dialogue fixture, loaded from first_dialogue.
	dlgSummary, err := recordFirstDialogueFixture(ctx, t, root, dialogueState)
	if err != nil {
		t.Fatalf("first-dialogue fixture: %v (fixture kept at %s for inspection)", err, filepath.Join(root, FixtureIDFirstDialogue))
	}

	t.Logf("fixtures: %s frames=%d lines=%d manifest_sha256=%s", pressSummary.ID, pressSummary.Frames, pressSummary.Lines, pressSummary.ManifestSHA256)
	t.Logf("fixtures: %s frames=%d lines=%d manifest_sha256=%s", dlgSummary.ID, dlgSummary.Frames, dlgSummary.Lines, dlgSummary.ManifestSHA256)
	t.Logf("fixtures: update the dolphin scenarios' input.manifest_sha256 to the values above")
}

// verifiedFixturePair reports whether BOTH fixtures exist, verify (every
// frame hashes), and reference savestates that exist and hash to the
// manifest's claim. The state files are checked here (not by VerifyFixture)
// because the manifest's states block is a reference the recorder wrote: a
// fixture whose savestate was deleted mid-run is not a stable artifact.
func verifiedFixturePair(t *testing.T, root string) (bool, string) {
	t.Helper()
	var details []string
	for _, id := range []string{FixtureIDLegendPress, FixtureIDFirstDialogue} {
		dir := filepath.Join(root, id)
		man, err := VerifyFixture(dir)
		if err != nil {
			return false, ""
		}
		if len(man.States) == 0 {
			return false, ""
		}
		for _, st := range man.States {
			p := filepath.Join(filepath.Dir(dir), st.File)
			sum, err := sha256File(p)
			if err != nil {
				return false, ""
			}
			if sum != st.SHA256 {
				return false, ""
			}
			details = append(details, st.Label+"@"+st.File)
		}
	}
	return true, joinStrings(details, ", ")
}

// recordSavestates boots the image once, drives to the legend's first
// subtitle and saves the (hotkey-bound) savestate slot there; stops,
// re-launches from that state, rides the rest of the legend WAIT-ONLY (a
// press would skip a subtitle and move the checkpoint off the measured
// timeline — the drive's own press policy is what the past-legend ride
// exists to bypass) and saves the same slot again the moment the first
// dialogue box measures on screen. Each save is copied to its stable file
// under the fixture root's states/ dir before the next save overwrites the
// slot; it returns the two copied paths.
func recordSavestates(ctx context.Context, t *testing.T, root string) (string, string, error) {
	t.Helper()
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		return "", "", fmt.Errorf("New: %v", err)
	}
	defer b.Stop()

	statesDir := filepath.Join(root, "states")
	if err := os.MkdirAll(statesDir, 0o755); err != nil {
		return "", "", err
	}

	// Phase 1a: cold boot → the legend's first subtitle → save the slot.
	if err := b.Launch(ctx, LaunchSpec{Hotkeys: true}); err != nil {
		return "", "", fmt.Errorf("launch (hotkeys): %v", err)
	}
	drv, err := RunDrive(ctx, b, DriveOptions{}, WindWakerNewGameDrive())
	if !drv.OK {
		return "", "", fmt.Errorf("drive to the legend's first subtitle: %s (last screen %s)", drv.Reason, drv.LastKey)
	}
	t.Logf("savestates: drive ok (reason %q, %s) — saving legend_start at the first subtitle", drv.Reason, drv.Finished.Sub(drv.Started).Round(time.Second))
	slot1, err := b.SaveState(ctx, stateSaveSlot)
	if err != nil {
		return "", "", fmt.Errorf("save %s (legend_start): %v", StateRelLegendStart, err)
	}
	legendState := filepath.Join(statesDir, "windwaker-legend-start.state")
	if err := copyFileVerified(slot1, legendState); err != nil {
		return "", "", fmt.Errorf("copy legend_start state: %v", err)
	}
	b.Stop()

	// Phase 1b: re-launch from legend_start; ride the legend wait-only; save
	// the slot again the moment the first dialogue box appears.
	if err := b.Launch(ctx, LaunchSpec{SaveState: legendState, Hotkeys: true}); err != nil {
		return "", "", fmt.Errorf("launch (save_state=legend_start, hotkeys): %v", err)
	}
	deadline := time.Now().Add(15 * time.Minute)
	var lastClass ScreenClass
	for {
		if ctx.Err() != nil {
			return "", "", ctx.Err()
		}
		if time.Now().After(deadline) {
			return "", "", fmt.Errorf("the first dialogue box did not appear within 15 m of the legend_start state (last class %s)", lastClass)
		}
		img, err := b.Screenshot(ctx)
		if err != nil {
			return "", "", fmt.Errorf("screenshot: %v", err)
		}
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		lastClass = c
		if c == ClassBox {
			break
		}
		time.Sleep(2 * time.Second)
	}
	t.Logf("savestates: the first dialogue box appeared — saving first_dialogue (slot 1 again — the checkpoint files are the stable copies)")
	slot2, err := b.SaveState(ctx, stateSaveSlot)
	if err != nil {
		return "", "", fmt.Errorf("save %s (first_dialogue): %v", StateRelFirstDialogue, err)
	}
	dialogueState := filepath.Join(statesDir, "windwaker-first-dialogue.state")
	if err := copyFileVerified(slot2, dialogueState); err != nil {
		return "", "", fmt.Errorf("copy first_dialogue state: %v", err)
	}
	return legendState, dialogueState, nil
}

// --- the two fixture recordings --------------------------------------------

// pressPace is the measured-press policy both fixtures ride: one A press when
// the current text's typewriter has SETTLED (its glyph ink stopped growing),
// so each press lands at the text's measured reveal-complete instant. A text
// that fades before the settle window elapses is SKIPPED (no press) — the
// pressed/skipped mix the press-skip scenario asserts emerges from the run
// (the game's own measured timing), not from a script.
type pressPace struct {
	// cooldown is the minimum spacing between two presses: a pressed line's
	// advance takes a beat, and a re-press on the SAME settled line would
	// skip the next text.
	cooldown  time.Duration
	lastPress time.Time
	lastInk   float64
	inkWindow []float64 // the recent glyph-ink readings (the settle window)
	presses   int
	skipped   int
}

// onInk is fed one glyph-ink reading per sample while the text's region is on
// screen. It reports true exactly when the typewriter has settled (the
// recent window is quiet) and the cooldown has elapsed — the press instant.
func (p *pressPace) onInk(ink float64) bool {
	if p.lastInk > 0 && ink > p.lastInk+inkGrowthFloor {
		// The typewriter added ink since the last sample: not settled.
		p.lastInk = ink
		p.inkWindow = append(p.inkWindow, ink)
		if len(p.inkWindow) > settleWindowSamples {
			p.inkWindow = p.inkWindow[len(p.inkWindow)-settleWindowSamples:]
		}
		return false
	}
	p.lastInk = ink
	p.inkWindow = append(p.inkWindow, ink)
	if len(p.inkWindow) > settleWindowSamples {
		p.inkWindow = p.inkWindow[len(p.inkWindow)-settleWindowSamples:]
	}
	if len(p.inkWindow) < 2 {
		return false
	}
	for i := 1; i < len(p.inkWindow); i++ {
		if p.inkWindow[i] > p.inkWindow[i-1]+inkGrowthFloor {
			return false // growth inside the window: the typewriter is running
		}
	}
	if time.Since(p.lastPress) < p.cooldown {
		return false
	}
	p.lastPress = time.Now()
	p.inkWindow = nil
	p.lastInk = 0
	p.presses++
	return true
}

// onFade records that the text's region vanished (a text that faded without
// a settle press — a skip — or the box closing).
func (p *pressPace) onFade() {
	if p.lastInk > 0 && len(p.inkWindow) > 0 {
		p.skipped++
	}
	p.inkWindow = nil
	p.lastInk = 0
}

const (
	// inkGrowthFloor is the glyph-ink delta (fraction of the window's pixels
	// with a horizontal gradient) that counts as "the typewriter added ink"
	// between two samples. Measured: a settled line holds its ink within
	// ±0.002; a typewriter increment moves it ≥ 0.005 (the dense legend
	// recording's subtitle frames).
	inkGrowthFloor = 0.004
	// settleWindowSamples is how many recent samples the settle check spans
	// (the driver's sample cadence is ~2 s of the recording's clock; the
	// window is the last few samples, not a wall-clock span).
	settleWindowSamples = 2

	// anchorMovedFloor is the pixel-RMSE floor (0..1, at the classifier's
	// 320x240 working size) under which a post-press sample is "the same
	// screen as before the press". Measured: a static screen's
	// render-to-render RMSE is ≤ 0.025 (the double-restore control on the
	// static box, 2026-10-18 15:16); a real advance (a page turn, a new
	// line, the box closing) measures ≥ 0.05 (the advance-visibility
	// threshold the savestate round trip uses — its measured advance is
	// 0.225). The floor sits between the two.
	anchorMovedFloor = 0.05
	// noResponseStagnant is how many consecutive "did not move" samples,
	// AFTER A PRESS, declare the screen unresponsive (the advance is over —
	// the legend's final page holds without advancing, the measured run's
	// 2026-10-18 15:55 presses 20-22: the same page, same ink, for eight
	// seconds). Three samples at the driver's ~2-3 s cadence is 6-9 s of
	// silence; a live advance answers within the first sample.
	noResponseStagnant = 3
)

type noRespWatch struct {
	anchor image.Image // the pre-press frame (nil until a press anchors one)
	silent int         // consecutive post-press samples inside the anchor's RMSE floor
}

// anchorForPress stores the pre-press frame (call with the last screenshot
// taken before the press injects).
func (n *noRespWatch) anchorForPress(img image.Image) {
	n.anchor = img
	n.silent = 0
}

// observe reports whether the screen has NOT responded to the last press
// (noResponseStagnant consecutive samples inside the anchor's RMSE floor).
// A screen that moved clears the watch (the advance happened; the next
// press re-anchors).
func (n *noRespWatch) observe(img image.Image) bool {
	if n.anchor == nil {
		return false
	}
	if rmse01(img, n.anchor, normW, normH) < anchorMovedFloor {
		n.silent++
		return n.silent >= noResponseStagnant
	}
	n.anchor = nil
	n.silent = 0
	return false
}

// waiting reports whether a press is still awaiting its response (the anchor
// is set and the screen has not moved). The drivers hold their press policy
// while this is true: re-pressing a screen that has not moved would reset
// the no-response count and press a static screen forever (the measured
// 2026-10-18 15:55 run did exactly that on the legend's final page —
// presses 20-22 on the same page).
func (n *noRespWatch) waiting() bool { return n.anchor != nil }

// recordLegendPressFixture loads the legend_start state and rides the legend
// with the measured press pace: dense capture the whole way, one A press per
// settled subtitle, ending where the first dialogue box appears.
func recordLegendPressFixture(ctx context.Context, t *testing.T, root, legendState string) (FixtureSummary, error) {
	t.Helper()
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		return FixtureSummary{}, err
	}
	defer b.Stop()

	rec, err := NewFixtureRecorder(b, root, FixtureIDLegendPress)
	if err != nil {
		return FixtureSummary{}, err
	}
	rec.SetCapturePlan(denseCapturePlan)
	rec.SetLineIDPrefix("ww-legend")
	if err := rec.Launch(ctx, LaunchSpec{SaveState: legendState}); err != nil {
		return FixtureSummary{}, fmt.Errorf("launch (save_state=legend_start): %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		return FixtureSummary{}, err
	}
	st := rec.State()
	t.Logf("legend-press: launched (display=%s image_sha256=%s)", st.Display, st.ImageSHA)

	pace := &pressPace{cooldown: 1500 * time.Millisecond}
	deadline := time.Now().Add(12 * time.Minute)
	var lastClass ScreenClass
	noResp := &noRespWatch{} // the press-response watch (the no-response stop)
	for {
		if ctx.Err() != nil {
			rec.StopCapture()
			return FixtureSummary{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			rec.StopCapture()
			return FixtureSummary{}, fmt.Errorf("the legend did not reach the first dialogue box within 12 m (last class %s, %d presses)", lastClass, pace.presses)
		}
		img, err := rec.Screenshot(ctx)
		if err != nil {
			rec.StopCapture()
			return FixtureSummary{}, err
		}
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		lastClass = c
		if noResp.observe(img) {
			rec.StopCapture()
			t.Logf("legend-press: the screen stopped responding to presses after %d presses (%d skipped) — the legend's advance is over; ending the legend fixture", pace.presses, pace.skipped)
			break
		}
		switch c {
		case ClassStorySub:
			sub := sig.Regions["subbar"]
			// While a press awaits its response the settle policy is not fed:
			// the current screen has not moved, and its "settle" is the no-
			// response itself, not a new typewriter.
			if !noResp.waiting() && pace.onInk(sub.edges) {
				noResp.anchorForPress(img)
				if err := rec.Inject(ctx, Buttons{A: true}, Axes{}, 400*time.Millisecond); err != nil {
					rec.StopCapture()
					return FixtureSummary{}, err
				}
				t.Logf("legend-press: press %d at %s (subbar ink %.3f — the typewriter had settled)",
					pace.presses, time.Now().Format("15:04:05.000"), sub.edges)
			}
		case ClassBox:
			// The legend is over: the first dialogue box appears. One more
			// boundary frame lands in the fixture; the dialogue has its own
			// fixture.
			t.Logf("legend-press: the first dialogue box appeared after %d presses (%d skipped) — ending the legend fixture", pace.presses, pace.skipped)
			time.Sleep(1 * time.Second)
			if _, err := rec.Screenshot(ctx); err != nil {
				rec.StopCapture()
				return FixtureSummary{}, err
			}
			rec.StopCapture()
			break
		default:
			// An interlude (a page turn, a fade): the previous text, if any,
			// is gone without a settle press — a skip.
			pace.onFade()
		}
		if c == ClassBox {
			break
		}
		time.Sleep(2 * time.Second)
	}

	if pace.presses == 0 {
		rec.StopCapture()
		return FixtureSummary{}, fmt.Errorf("no press landed: no subtitle measured a settled typewriter (the press-pace oracle is blind here)")
	}
	// The state reference must land BEFORE Finalize builds the manifest —
	// a states entry added afterwards is dropped (the measured 2026-10-18
	// 17:03 run: the legend-press manifest shipped with no states block
	// because this call ran after the manifest was written).
	if err := rec.AddState("legend_start", stateSaveSlot, StateRelLegendStart, string(DriveScreenStorySubtitle)); err != nil {
		return FixtureSummary{}, fmt.Errorf("add legend_start state reference: %w", err)
	}
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		return FixtureSummary{}, err
	}
	t.Logf("legend-press: %d presses, %d skipped subtitles, %d frames, %d lines", pace.presses, pace.skipped, summary.Frames, summary.Lines)
	return summary, nil
}

// recordFirstDialogueFixture loads the first_dialogue state and rides the
// first NPC conversation: dense capture, one A press per settled box line
// (the advance to the next box), until the box closes.
func recordFirstDialogueFixture(ctx context.Context, t *testing.T, root, dialogueState string) (FixtureSummary, error) {
	t.Helper()
	dataDir := t.TempDir()
	b, err := New(Options{DataDir: dataDir, LaunchTimeout: 180 * time.Second})
	if err != nil {
		return FixtureSummary{}, err
	}
	defer b.Stop()

	rec, err := NewFixtureRecorder(b, root, FixtureIDFirstDialogue)
	if err != nil {
		return FixtureSummary{}, err
	}
	rec.SetCapturePlan(denseCapturePlan)
	rec.SetLineIDPrefix("ww-first")
	if err := rec.Launch(ctx, LaunchSpec{SaveState: dialogueState}); err != nil {
		return FixtureSummary{}, fmt.Errorf("launch (save_state=first_dialogue): %v", err)
	}
	if err := rec.StartCapture(ctx); err != nil {
		return FixtureSummary{}, err
	}
	st := rec.State()
	t.Logf("first-dialogue: launched (display=%s image_sha256=%s)", st.Display, st.ImageSHA)

	pace := &pressPace{cooldown: 1500 * time.Millisecond}
	deadline := time.Now().Add(8 * time.Minute)
	boxGoneSince := time.Time{}
	boxSeen := false         // the box must be SEEN before the close-break may fire
	noResp := &noRespWatch{} // the press-response watch (the no-response stop)
	for {
		if ctx.Err() != nil {
			rec.StopCapture()
			return FixtureSummary{}, ctx.Err()
		}
		if time.Now().After(deadline) {
			rec.StopCapture()
			return FixtureSummary{}, fmt.Errorf("the box did not close within 8 m (%d presses, boxSeen=%v)", pace.presses, boxSeen)
		}
		img, err := rec.Screenshot(ctx)
		if err != nil {
			rec.StopCapture()
			return FixtureSummary{}, err
		}
		sig := ScreenSigOf(img)
		c, _ := sig.ClassOf()
		t.Logf("first-dialogue: sample at %s class=%s mean=%.1f lb=%.3f boxInk=%.4f (boxSeen=%v presses=%d)", time.Now().Format("15:04:05"), c, sig.Mean, sig.Letterbox, sig.Regions["boxband"].edges, boxSeen, pace.presses)
		if c == ClassBox && noResp.observe(img) {
			rec.StopCapture()
			t.Logf("first-dialogue: the box stopped responding to presses after %d presses — the conversation is over; ending the fixture", pace.presses)
			break
		}
		if c == ClassBox {
			boxSeen = true
			if !boxGoneSince.IsZero() {
				// A (re)appearance after an absence: the typewriter for the
				// new line is running — restart the settle window. (The reset
				// must key on the transition, not on boxGoneSince being zero:
				// the box clears it on every sample, so a zero-keyed reset
				// emptied the window every sample and the settle check never
				// accumulated its two samples — the measured 2026-10-18 17:00
				// run: box on screen ~2 m, zero presses.)
				pace.inkWindow = nil
				pace.lastInk = 0
			}
			boxGoneSince = time.Time{}
			ink := sig.Regions["boxband"].edges
			if !noResp.waiting() && pace.onInk(ink) {
				noResp.anchorForPress(img)
				if err := rec.Inject(ctx, Buttons{A: true}, Axes{}, 400*time.Millisecond); err != nil {
					rec.StopCapture()
					return FixtureSummary{}, err
				}
				t.Logf("first-dialogue: press %d at %s (boxband ink %.3f — the line had settled)",
					pace.presses, time.Now().Format("15:04:05.000"), ink)
			}
		} else if boxSeen {
			// The close-break only applies once the box was seen: the
			// post-load settle (llvmpipe rendering the saved scene) can take
			// longer than the 3 s close window (the measured 2026-10-18 16:32
			// run: the box was absent for the whole 20 s settle and the run
			// ended before the load had drawn the box).
			if boxGoneSince.IsZero() {
				boxGoneSince = time.Now()
				pace.onFade()
			} else if time.Since(boxGoneSince) >= 3*time.Second {
				// The box is gone for good: the conversation is over.
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if !boxSeen {
		rec.StopCapture()
		return FixtureSummary{}, fmt.Errorf("the box never appeared within 8 m of the first_dialogue state (the state did not land on the box screen)")
	}
	if pace.presses == 0 {
		rec.StopCapture()
		return FixtureSummary{}, fmt.Errorf("no press landed: the box's typewriters never measured a settled state")
	}
	// Before Finalize, same as the legend fixture: a states entry added
	// after the manifest is written is dropped.
	if err := rec.AddState("first_dialogue", stateSaveSlot, StateRelFirstDialogue, string(DriveScreenDialogueBox)); err != nil {
		return FixtureSummary{}, fmt.Errorf("add first_dialogue state reference: %w", err)
	}
	summary, err := rec.Finalize(ctx, DriveTrace{})
	if err != nil {
		return FixtureSummary{}, err
	}
	t.Logf("first-dialogue: %d presses, box closed, %d frames, %d lines", pace.presses, summary.Frames, summary.Lines)
	return summary, nil
}

// --- small helpers -----------------------------------------------------------

// copyFileVerified copies src to dst and re-reads the copy: the savestate is
// referenced by its hash, so a byte difference would silently corrupt the
// reference.
func copyFileVerified(src, dst string) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, raw, 0o644); err != nil {
		return err
	}
	back, err := os.ReadFile(dst)
	if err != nil {
		return err
	}
	if !bytes.Equal(raw, back) {
		return fmt.Errorf("copy %s -> %s: bytes differ after copy", filepath.Base(src), filepath.Base(dst))
	}
	return nil
}

func joinStrings(items []string, sep string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += sep
		}
		out += s
	}
	return out
}

var _ = sha256.Sum256 // the states' manifest references are SHA-256 (see AddState)

// TestRealReOCRFixture re-finalises the OCR LAYER of an existing fixture with
// the current record-time recipe — the recipe-revision path (ADR-0054
// ADR-0054 amendment, the adaptive box-text crop): the frames, inputs and savestate
// are the recorded evidence and are NOT touched (their hashes re-verify
// against the old manifest before anything is written); only the OCR truth,
// the profile and the manifest are rewritten by the SAME code path the live
// recording uses (ocrFrames, writeTruth, writeProfile, buildManifest), so a
// re-OCR cannot drift from what a fresh recording under the new recipe would
// produce for these pixels. It is explicit, not a gate phase: it runs only
// when GN_REOCR_FIXTURE names the fixture id.
func TestRealReOCRFixture(t *testing.T) {
	id := os.Getenv("GN_REOCR_FIXTURE")
	if id == "" {
		t.Skipf("NOT RUN: GN_REOCR_FIXTURE is not set (the re-OCR is an explicit recipe-revision step: set it to the fixture id under %s)", EnvFixtureDir)
	}
	root := fixtureRootForTest(t)
	dir := filepath.Join(root, id)
	old, err := VerifyFixture(dir) // the frames must hash to the old manifest before anything moves
	if err != nil {
		t.Fatalf("re-OCR: %v (the fixture is not the recording it claims; nothing was written)", err)
	}
	// The profile's line-id prefix ("ww-first-01" -> "ww-first"): the message
	// ids must stay the ones the scenarios' assertions name.
	prefix, err := profileIDPrefix(dir)
	if err != nil {
		t.Fatalf("re-OCR: %v", err)
	}

	rec := &FixtureRecorder{id: old.ID, root: dir, lineIDPrefix: prefix}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	texts, sub, box, subN, boxN, err := rec.ocrFrames(ctx, old.Frames)
	if err != nil {
		t.Fatalf("re-OCR: %v (nothing was written)", err)
	}

	st := Status{
		Display:    old.Dolphin.Display,
		UserDir:    old.Dolphin.UserDir,
		DolphinPID: old.Dolphin.PID,
		ImageSHA:   old.GameImageSHA256,
	}
	drv := DriveTrace{OK: old.Oracle.OK, Reason: old.Oracle.Reason, SuccessText: old.Oracle.SuccessText}
	for _, tr := range old.Oracle.Transitions {
		drv.Transitions = append(drv.Transitions, DriveTransition{
			Index:  tr.Index,
			Screen: DriveScreen(tr.Screen),
			Side:   CursorSide(tr.Side),
			Action: tr.Action,
			Detail: tr.Detail,
		})
	}
	for _, st2 := range old.Oracle.SubTimings {
		drv.SubTimings = append(drv.SubTimings, SubTiming{
			FirstIndex: st2.FirstIndex, LastIndex: st2.LastIndex,
			FadeIn: time.Duration(st2.FadeInNS), Hold: time.Duration(st2.HoldNS),
			FadeOut: time.Duration(st2.FadeOutNS), Open: st2.Open,
		})
	}
	recipe := FixtureOCRRecipe{Engine: "tesseract", PSM: ocr.DefaultPSM, Scale: ocrSubtitleScale, Region: StorySubCrop}
	var ocrBox *FixtureOCRRecipe
	if boxN > 0 {
		ocrBox = &FixtureOCRRecipe{
			Engine:   "tesseract",
			PSM:      ocr.DefaultPSM,
			Scale:    ocrSubtitleScale,
			Region:   boxTextCrop,
			Adaptive: &FixtureOCRAdaptive{InkFloor: boxInkFloor, Margin: boxCropMargin, ScanY0: boxInkScanY0, ScanY1: boxInkScanY1},
		}
	}
	man := rec.buildManifest(old.Frames, old.Inputs, st, drv, old.GameImageSHA256, old.Resolution, recipe, old.Capture, ocrBox, old.States)
	if err := writeJSON(filepath.Join(dir, "manifest.json"), man); err != nil {
		t.Fatalf("re-OCR: manifest: %v", err)
	}
	if err := rec.writeTruth(old.Frames, texts, sub, box); err != nil {
		t.Fatalf("re-OCR: truth: %v", err)
	}
	lines, err := rec.writeProfile(sub, box, subN, boxN, old.Resolution, prefix)
	if err != nil {
		t.Fatalf("re-OCR: profile: %v", err)
	}

	// The re-finalised fixture must self-verify exactly as the original did:
	// every frame hashes, and every referenced savestate exists and hashes.
	man2, err := VerifyFixture(dir)
	if err != nil {
		t.Fatalf("re-OCR: the re-finalised fixture does not verify: %v", err)
	}
	for _, s := range man2.States {
		sum, err := sha256File(filepath.Join(root, s.File))
		if err != nil {
			t.Fatalf("re-OCR: state %s: %v", s.File, err)
		}
		if sum != s.SHA256 {
			t.Fatalf("re-OCR: state %s hash mismatch (the re-finalise must not touch the states)", s.File)
		}
	}
	sha, err := ManifestSHA256(dir)
	if err != nil {
		t.Fatalf("re-OCR: manifest sha: %v", err)
	}
	t.Logf("re-OCR %s: %d lines (sub=%d box=%d), manifest_sha256=%s — update the dolphin scenarios' input.manifest_sha256 to this value", id, lines, subN, boxN, sha)
}

// profileIDPrefix reads the fixture's profile.json and recovers the line-id
// prefix from its first message id ("ww-first-01" -> "ww-first").
func profileIDPrefix(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "profile.json"))
	if err != nil {
		return "", fmt.Errorf("profile: %w", err)
	}
	var p struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", fmt.Errorf("profile: parse: %w", err)
	}
	if len(p.Messages) == 0 {
		return "", fmt.Errorf("profile %s: no messages (cannot recover the line-id prefix)", dir)
	}
	id := p.Messages[0].ID
	i := strings.LastIndex(id, "-")
	if i <= 0 {
		return "", fmt.Errorf("profile message id %q: no \"<prefix>-NN\" shape", id)
	}
	return id[:i], nil
}
