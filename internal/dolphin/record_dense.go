package dolphin

// record_dense.go is the dense-legend recording of ADR-0058 amendment 1
// (the M7 follow-through the ADR's adjustment clause named: "capture at
// ≥10 Hz with the fixture factory and re-bench"). The original
// windwaker-newgame fixture samples on the drive's ~2.1 s grid, so a legend
// line can appear in a single frame and the recognition tracker (which
// needs ≥2 observations) can never commit it; the bench's single-frame
// lines are then sampling artifacts, not pipeline failures. This file
// provides the screen-driven wait that rides the WHOLE opening legend
// (every subtitle, until the measured gameplay screen or the budget),
// paired with the recorder's parallel adaptive capture (CapturePlan,
// schema 2) so the subtitle scenes land at ≥10 Hz while the budget bounds
// the fixture's size. The real gate that records the fixture is
// TestRealRecordLegendDense (record_dense_test.go, unix).
//
// AGENTS.md rule 5: the recorded frames and text live only in the
// gitignored fixture dir; nothing derived from the game lands in tracked
// files.

import (
	"context"
	"fmt"
	"time"
)

// FixtureIDWindWakerDense is the dense fixture id: the drive to the legend
// plus the whole opening legend at ≥10 Hz.
const FixtureIDWindWakerDense = "windwaker-legend-dense"

// denseCapturePlan is the recording's capture plan: the high rate is the
// task's ≥10 Hz dialogue requirement, the low rate bounds the long
// non-dialogue spans, and the duration/byte budgets bound the run (a
// budget stop is honest: the manifest records the window it consumed). The
// duration counts from StartCapture (before the drive), so it must cover
// the drive to the legend (measured up to ~8 min: the title alone can take
// ~2.5 min) plus the whole legend (measured 40+ s of emulation).
var denseCapturePlan = CapturePlan{
	HighHz:      10,
	LowHz:       1,
	MaxDuration: 30 * time.Minute,
	MaxBytes:    2 << 30, // 2 GiB of encoded frames is far past any sane legend
}

// legendWatchCadence is the WatchLegend's own sampling period (its frames
// also land in the fixture, interleaved with the capture loop's). It only
// decides WHEN the watch notices the legend has ended; the dense recording
// itself is the capture loop's work.
const legendWatchCadence = time.Second

// legendEndedScreen is the measured screen that ends the opening legend:
// the first in-game dialogue box (the new-game drive's other success
// screen). The legend's interludes (black spans, the storybook panel)
// measure as cutscene/unknown, so a "no subtitle for N seconds" rule would
// stop mid-legend; a class change to the dialogue box is the measured
// boundary.
const legendEndedScreen = DriveScreenDialogueBox

// LegendWatchResult is the watch's report: what the wait saw.
type LegendWatchResult struct {
	Samples    int    // screen samples taken
	LastScreen string // the screen measured at the last sample
	Reason     string // why the watch ended
}

// WatchLegend samples the screen on a fixed cadence and waits until the
// opening legend has ended (the measured gameplay screen) or the budget
// spent. It presses NOTHING on the legend (the legend advances on its own;
// a press may skip a subtitle — the same measured fact the drive's
// wait-only action encodes). The capture loop (FixtureRecorder's parallel
// stream) is what records the legend densely; WatchLegend is the
// termination oracle for that recording.
//
// b is the bridge to sample through (usually the FixtureRecorder itself, so
// the watch's own samples become fixture frames in capture order).
func WatchLegend(ctx context.Context, b Bridge, budget time.Duration) (LegendWatchResult, error) {
	deadline := time.Now().Add(budget)
	var res LegendWatchResult
	for {
		if ctx.Err() != nil {
			return res, fmt.Errorf("dolphin: legend watch cancelled: %w", ctx.Err())
		}
		if time.Now().After(deadline) {
			res.Reason = fmt.Sprintf("budget exhausted after %d samples (last screen %s)", res.Samples, res.LastScreen)
			return res, nil
		}
		img, err := b.Screenshot(ctx)
		if err != nil {
			return res, fmt.Errorf("dolphin: legend watch screenshot: %w", err)
		}
		sig := ScreenSigOf(img)
		screen, _ := DriveScreenOf(sig)
		res.Samples++
		res.LastScreen = string(screen)
		if screen == legendEndedScreen {
			res.Reason = fmt.Sprintf("legend ended: the measured gameplay screen (%s) appeared after %d samples", screen, res.Samples)
			return res, nil
		}
		select {
		case <-ctx.Done():
			return res, fmt.Errorf("dolphin: legend watch cancelled: %w", ctx.Err())
		case <-time.After(legendWatchCadence):
		}
	}
}
