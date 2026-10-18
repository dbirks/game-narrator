// Package dolphin implements the dev-only Dolphin emulator bridge defined
// by docs/spec/11-dolphin-automation.md ("Dev-only emulator bridge contract").
//
// Invariants: production never executes Dolphin controller actions
// (docs/spec/01 invariant 7); every entrypoint here requires the developer
// opt-in GAME_NARRATOR_DEV_DOLPHIN=1 plus the developer role. Each run uses
// a fresh isolated user dir (never ~/.local/share/dolphin-emu) and a private
// Xvfb display. The image path comes only from GAME_NARRATOR_GAME_IMAGE_PATH
// and is never logged. Process existence is never readiness: WaitForGameFrame
// and ProveInput return frame-level evidence (docs/spec/11, no false positives).
package dolphin

import (
	"context"
	"image"
	"time"

	"github.com/dbirks/game-narrator/internal/capture"
)

// LaunchSpec configures one isolated bridge launch.
type LaunchSpec struct {
	ImagePath string // from GAME_NARRATOR_GAME_IMAGE_PATH only; never logged
	UserDir   string // fresh isolated Dolphin user dir for this run
	Backend   string // render backend; "OGL"
	Display   string // X display; "" = start a private Xvfb
	Width     int
	Height    int
	SaveState string // optional savestate to load
	Movie     string // optional movie file to run
	// Hotkeys opts in to the savestate hotkey chords (ADR-0043 amendment 3,
	// approach B): Config/Hotkeys.ini binds the slot-1 save/load chords to
	// the game pad's OWN device (Pipe/0/pipe1 — 2609 binds the Hotkeys
	// virtual pad to the same PipeDevice the GCPad uses, so the single
	// FIFO carries both) and Dolphin.ini gains [General]
	// HotkeysRequireFocus = False. False (the default) is the proven
	// single-pipe path with no Hotkeys.ini and no focus gate — exactly the
	// d8394f3 config ProveInput was proven on. When false, SaveState/
	// LoadState refuse with ErrHotkeysDisabled.
	Hotkeys bool
	// Lifetime bounds the whole run, not just the launch: when it is
	// cancelled (for any reason) the bridge runs a full Stop — the process
	// group is signalled, the FIFO removed — and the children die with the
	// parent that asked for them. Zero value = the Launch ctx doubles as the
	// lifetime, so a caller that cancels its launch context also kills the
	// emulator. The dev API passes a process-lifetime context instead, so a
	// game session outlives the launch request and only POST /stop (or the
	// process's own death, covered by Pdeathsig) ends it.
	Lifetime context.Context
}

// Buttons names the digital controller inputs to inject via Pipes.
type Buttons struct {
	A, B, X, Y, Z, Start, L, R, Up, Down, Left, Right bool
}

// Axes carries stick and trigger positions (-1..1; L/R are 0..1 triggers).
type Axes struct {
	MainX, MainY, StickX, StickY float64
	L, R                         float64
}

// Evidence is frame-level proof (hashes, distances, timings) for readiness.
type Evidence struct {
	Hashes    []uint64
	Distances []float64
	Files     map[string]string // labeled evidence PNGs ("before"/"control"/"after")
	At        time.Time
}

// Bridge is the dev-only Dolphin automation bridge (docs/spec/11).
type Bridge interface {
	// Launch starts Dolphin (+Xvfb) isolated per LaunchSpec.
	Launch(ctx context.Context, spec LaunchSpec) error
	// Inject writes Buttons/Axes through the persistent Pipes FIFO writer,
	// holding for the given duration.
	Inject(ctx context.Context, buttons Buttons, axes Axes, hold time.Duration) error
	// Screenshot X11-grabs the render window region located via xwininfo.
	Screenshot(ctx context.Context) (image.Image, error)
	// FrameSource returns the capture.Source consumed exactly like production.
	FrameSource(fps int) (capture.Source, error)
	// SaveSnapshot writes a labeled screenshot PNG (+ savestate if supported).
	SaveSnapshot(label string) (string, error)
	// WaitForGameFrame returns when frames pass the entropy/differ predicate.
	WaitForGameFrame(ctx context.Context, timeout time.Duration) (Evidence, error)
	// ProveInput runs the title-screen START control-vs-input test.
	ProveInput(ctx context.Context) (Evidence, error)
	// SaveState triggers a savestate save of the slot over the hotkey chords
	// (the game pad's own device, ADR-0043 am3) and waits for the state file
	// on disk; it returns the file path (the value a --save_state boot
	// takes). Savestate hotkeys are opt-in (LaunchSpec.Hotkeys) and slot 1
	// is the only bound slot; SaveState/LoadState refuse with
	// ErrHotkeysDisabled when the launch did not opt in (Hotkeys false is
	// the default).
	SaveState(ctx context.Context, slot int) (string, error)

	// LoadState triggers a savestate load of the slot over the hotkey chords
	// and waits the settle floor. The perceptual round trip (save X, advance
	// to Y, load, X again) is assembled by callers out of SaveState, Inject
	// and Screenshot. Refuses with ErrHotkeysDisabled when the launch did
	// not opt in.
	LoadState(ctx context.Context, slot int) error

	// State reports a consistent snapshot of the bridge lifecycle and geometry.
	State() Status
	// Stop kills Dolphin + Xvfb and removes the FIFO (the game pad's pipe1;
	// on the opt-in path the hotkey chords ride it, so there is one FIFO).
	// Idempotent.
	Stop()
}
