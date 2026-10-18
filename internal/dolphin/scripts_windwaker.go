package dolphin

import (
	"context"
	"image"
	"time"
)

// WindWakerNewGameDrive is the dev-harness new-game drive for the Wind Waker
// image (docs/spec/11 step 3; the task's M6 drive-to-first-dialogue), as a
// SCREEN-DRIVEN LOOP (ADR-0043 amendment 5): boot -> title -> START -> the
// memory-card prompt (cursor to Yes via the MAIN STICK, strict
// cursor-on-Yes) -> create save -> the save-creation notice -> the
// name entry (START moves the cursor to END, then A; never A alone on this
// screen) -> the name confirm (if raised) -> the opening (story scroll /
// cutscenes: A only while a text box is on screen) -> the first in-game
// dialogue box (the goal class).
//
// The loop itself (RunDrive) classifies every frame, sends the ONE action
// the screen prescribes, and logs (screen, action) -> next screen; there are
// no pre-satisfied or skipped steps. Termination: the dialogue-box class
// (success), the same screen >= 4 consecutive iterations with no visible
// change ("loop detected"), or the iteration/time budget (failure with the
// full transition log and the last PNG).
//
// Input facts measured on this host (ADR-0043 amendments 2-5; the retained
// drive evidence is the record):
//
//   - the WOTW Yes/No cursor is NOT driven by the d-pad; the MAIN STICK is
//     (measured: stick left, MainX=-1, 900 ms hold, then neutral, moves the
//     cursor No -> Yes, left.blob 0.0296 / right.blob 0.000; 200 ms is
//     measured NOT to move it). Every cursor move here is that measured
//     input.
//   - the create-save prompt and the save-failure continue-anyway dialog are
//     the same measured class (the dark dialog family carrying the near-white
//     cursor blob), so one prompt action handles both — including the
//     save attempt the name confirmation raises when the card is not usable
//     (the measured flow the drive must survive either way).
//   - the quest-log file selection (when it appears on the path) advances on
//     A+START together (measured: A alone and START alone are no-ops).
//   - the name entry confirms on START (the cursor moves to the END button)
//     then A (measured: A alone TYPES the selected letter into the name —
//     the 2026-10-17 run's nine A presses typed "AAAAAA" and never left the
//     screen — so A alone is never the name-entry action).
//   - START on the title registers only on the title's dim "press START"
//     phase and is ignored on the bright logo phase (measured); the loop
//     re-sends START on every title iteration, so a press landing on the
//     bright phase is retried until one lands on the dim phase and the
//     prompt appears — the goal screen is measured, so a title repeat can
//     never pass as the prompt.
func WindWakerNewGameDrive() ActionTable {
	// unknownSideStreak counts consecutive prompt iterations where the cursor
	// side was not measured (the cursor blinks off, measured ~1 s period, so
	// a 2 s cadence samples the side in ~50% of frames). The measured default
	// cursor side of the WOTW prompts on this path is NO (the create-save
	// prompt) or YES (the continue-anyway prompt), and the measured cursor
	// move is the main stick LEFT (No -> Yes; from Yes it either stays or
	// wraps back to No — both converge on Yes-A within a few iterations), so
	// after 3 consecutive no-side iterations the drive sends the measured
	// stick move instead of waiting: it can never press A onto an
	// unmeasured side, and a screen that ignores the stick still fails the
	// loop detector (same key, no change) instead of consuming the budget.
	var unknownSideStreak int
	return func(screen DriveScreen, side CursorSide, _ DriveScreen, _ bool, frame image.Image) DriveAction {
		if screen != DriveScreenPrompt && screen != DriveScreenNameConfirm {
			unknownSideStreak = 0 // a new screen: the blink counter restarts
		} else if side != CursorNone {
			unknownSideStreak = 0 // the side measured: the blink counter restarts
		}
		switch {
		case screen == DriveScreenDialogueBox:
			// The goal — the loop terminates before an action is needed.
			return DriveAction{Wire: "done", Run: func(context.Context, Bridge) error { return nil }}

		case screen == DriveScreenTitle:
			// START (the dim-phase press that registers; the loop retries on
			// every title iteration until the prompt appears).
			return pressAction("START", []Button{ButtonStart})

		case screen == DriveScreenPrompt || screen == DriveScreenNameConfirm:
			switch side {
			case CursorRight:
				// Cursor on No: move it to Yes with the measured main-stick
				// input (900 ms hold — the measured move; 200 ms is measured
				// NOT to move the cursor).
				return stickLeftAction()
			case CursorLeft:
				// Cursor on Yes (measured): confirm.
				return pressAction("A", []Button{ButtonA})
			default:
				// The cursor is blinked off (the measured prompt blinks: blob
				// on/off at ~1 s period) and the side is unmeasured: never
				// guess A onto an unmeasured side. After 3 consecutive
				// no-side iterations send the measured stick move (it is the
				// measured cursor input on both measured prompts, and from a
				// Yes cursor it either stays or wraps to No — the next
				// measured side converges on Yes-A either way).
				unknownSideStreak++
				if unknownSideStreak >= 3 {
					return DriveAction{Wire: "STICK(no-side)", Run: stickLeftAction().Run}
				}
				return waitAction(2 * time.Second)
			}

		case screen == DriveScreenSaveNotice:
			// The blobless system dialog (the "A save file has been created."
			// notice, the "no save file" info): A advances it (measured: A on
			// the no-save info goes on to the file selection / name entry).
			return pressAction("A", []Button{ButtonA})

		case screen == DriveScreenFileSelect:
			// The quest-log list advances only on A+START together (measured).
			return pressAction("A+START", []Button{ButtonA, ButtonStart})

		case screen == DriveScreenNameEntry:
			// The name entry (ADR-0043 amendment 5, the 2026-10-17 re-run):
			// the keyboard grid with the cursor on the first letter. The
			// measured confirm is A-then-START-then-A: A TYPES the letter under
			// the cursor into the name (A alone on this screen NEVER confirms —
			// it only types, which is why the 2026-10-17 run's nine A presses
			// typed "AAAAAA" and never left the screen), START moves the cursor
			// to the END button, and the final A confirms the name. The
			// 2026-10-17 re-run proved that START+A with NO letter typed (an
			// empty name) is REJECTED by the game and bounces back to the
			// quest-log file selection — the name must carry at least one typed
			// letter before the END confirm. One compound action, logged as one
			// transition: A (type the first letter) -> START (cursor to END) ->
			// A (confirm the name).
			return DriveAction{
				Wire: "A+START+A(name)",
				Run: func(ctx context.Context, b Bridge) error {
					// Type the letter under the cursor (the first letter) into the name.
					if err := b.Inject(ctx, Buttons{A: true}, Axes{}, 500*time.Millisecond); err != nil {
						return err
					}
					if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
						return err
					}
					// Move the cursor to the END button.
					if err := b.Inject(ctx, Buttons{Start: true}, Axes{}, 500*time.Millisecond); err != nil {
						return err
					}
					if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
						return err
					}
					// Confirm the name.
					return b.Inject(ctx, Buttons{A: true}, Axes{}, 500*time.Millisecond)
				},
			}

		case screen == DriveScreenCutscene:
			// The opening plays (story scroll / cutscenes): advance text with
			// A ONLY when a text box (a glyph-inked lower-third band) is on
			// screen; otherwise wait (a long stick hold would walk away from
			// the boat scene).
			if cutsceneTextPresent(frame) {
				return pressAction("A(text)", []Button{ButtonA})
			}
			return waitAction(2 * time.Second)

		case screen == DriveScreenStorySubtitle:
			// The opening legend's narration subtitle (amendment 6): NOTHING
			// is pressed — the legend advances on its own and a press may
			// SKIP the narration the pipeline must record. The drive waits
			// (one iteration) and keeps sampling; RunDrive closes the run on
			// the first text-measured frame (success) and records each
			// subtitle's fade-in/hold/fade-out from the wait-only samples.
			return waitAction(2 * time.Second)

		default:
			// An unmeasured frame (a fade, a blank): wait one iteration and
			// re-classify. The loop detector still bounds this: an unknown
			// screen with no change for 4 iterations fails the run.
			return waitAction(2 * time.Second)
		}
	}
}

// pressAction builds a button-press action (one Inject, the measured 500 ms
// hold the bridge's spike calibrated) with its wire description.
func pressAction(wire string, buttons []Button) DriveAction {
	return DriveAction{
		Wire: wire,
		Run: func(ctx context.Context, b Bridge) error {
			var bs Buttons
			for _, btn := range buttons {
				switch btn {
				case ButtonA:
					bs.A = true
				case ButtonB:
					bs.B = true
				case ButtonX:
					bs.X = true
				case ButtonY:
					bs.Y = true
				case ButtonZ:
					bs.Z = true
				case ButtonStart:
					bs.Start = true
				case ButtonL:
					bs.L = true
				case ButtonR:
					bs.R = true
				case ButtonUp:
					bs.Up = true
				case ButtonDown:
					bs.Down = true
				case ButtonLeft:
					bs.Left = true
				case ButtonRight:
					bs.Right = true
				}
			}
			return b.Inject(ctx, bs, Axes{}, 500*time.Millisecond)
		},
	}
}

// stickLeftAction is the measured Yes/No cursor move: the main stick fully
// LEFT (MainX=-1 -> the pipe's SET MAIN 0.000 0.500), held 900 ms (the
// measured move; 200 ms is measured NOT to move the cursor), the bridge
// returning the stick to neutral after the hold.
func stickLeftAction() DriveAction {
	return DriveAction{
		Wire: "STICK(-1,0)x900ms",
		Run: func(ctx context.Context, b Bridge) error {
			return b.Inject(ctx, Buttons{}, Axes{MainX: -1, MainY: 0}, 900*time.Millisecond)
		},
	}
}

// waitAction is a no-input iteration (the cutscene and unknown screens
// wait for the screen to change on its own).
func waitAction(d time.Duration) DriveAction {
	return DriveAction{
		Wire: "wait",
		Run: func(ctx context.Context, _ Bridge) error {
			return sleepCtx(ctx, d)
		},
	}
}
