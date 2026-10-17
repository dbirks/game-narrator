package dolphin

import (
	"errors"
	"os"
	"time"
)

// EnvDevDolphin is the explicit developer opt-in (docs/spec/01 invariant 7):
// only when it is "1" may this package launch Dolphin or inject controller
// input, and only then does the runtime admit the "dolphin" frame source and
// the API serve the /api/v1/dev/dolphin routes. Production builds never set it.
const EnvDevDolphin = "GAME_NARRATOR_DEV_DOLPHIN"

// EnvGameImage names the user-supplied GameCube image. The path is read from
// the environment only, referenced read-only, never copied, and never logged;
// only its SHA-256 and size are recorded (ADR-0007 provenance rule).
const EnvGameImage = "GAME_NARRATOR_GAME_IMAGE_PATH"

// DevOptIn reports whether the developer opt-in is set. lookupEnv is a seam
// for tests; nil reads os.LookupEnv.
func DevOptIn(lookupEnv func(string) (string, bool)) bool {
	if lookupEnv == nil {
		lookupEnv = os.LookupEnv
	}
	v, ok := lookupEnv(EnvDevDolphin)
	return ok && (v == "1" || v == "true")
}

// State is the bridge lifecycle state.
type State string

const (
	// StateStopped: nothing is running; Launch may be called.
	StateStopped State = "stopped"
	// StateLaunching: Xvfb/Dolphin are coming up; no render window yet.
	StateLaunching State = "launching"
	// StateRunning: the render window is resolved and frames are capturable.
	StateRunning State = "running"
	// StateFailed: the launch or run died; LastError says why.
	StateFailed State = "failed"
)

// Region is a pixel rectangle inside the X root window (the render window's
// absolute geometry, resolved through xwininfo — never the whole root,
// ADR-0007: "measure the render window, not the root window").
type Region struct {
	X, Y, W, H int
}

// Status is the bridge's self-report (the API's GET /api/v1/dev/dolphin). It
// carries host paths only where a trusted developer on the box needs them
// (user_dir, evidence files) and never the game image path — that is
// recorded as image_sha256 plus its size.
type Status struct {
	State      State     `json:"state"`
	Display    string    `json:"display,omitempty"`
	DolphinPID int       `json:"dolphin_pid,omitempty"`
	XvfbPID    int       `json:"xvfb_pid,omitempty"`
	WindowID   string    `json:"render_window,omitempty"`
	Region     Region    `json:"region,omitempty"`
	PipeDevice string    `json:"pipe_device,omitempty"`
	PipeConn   bool      `json:"pipe_connected"`
	UserDir    string    `json:"user_dir,omitempty"`
	ImageSHA   string    `json:"image_sha256,omitempty"`
	ImageSize  int64     `json:"image_size,omitempty"`
	FirstFrame time.Time `json:"first_game_frame_at,omitempty"`
	LastError  string    `json:"last_error,omitempty"`
}

// Sentinel errors, each one a distinct operator condition rather than a
// free-text that a client could not branch on.
var (
	// ErrNotLaunched: a method that needs a live bridge was called while it
	// is stopped or failed.
	ErrNotLaunched = errors.New("dolphin: the bridge is not running (launch it first)")
	// ErrAlreadyLaunched: a second Launch on the same bridge. Stop first.
	ErrAlreadyLaunched = errors.New("dolphin: the bridge is already launched or launching; stop it first")
	// ErrNoImage: the game image env var is unset or the path is unreadable.
	ErrNoImage = errors.New("dolphin: no game image (set GAME_NARRATOR_GAME_IMAGE_PATH)")
	// ErrUserDirNotIsolated: the requested user dir is the real Dolphin dir.
	ErrUserDirNotIsolated = errors.New("dolphin: the user dir must be a fresh isolated dir, never the real ~/.local/share/dolphin-emu")
	// ErrPipeNotConnected: Dolphin has not opened the Pipes FIFO yet (or is
	// gone), so a controller command cannot reach it.
	ErrPipeNotConnected = errors.New("dolphin: the controller pipe is not connected (Dolphin has not opened it)")
	// ErrNotRunning is an alias callers may check: the bridge state is not
	// "running" (launching does not accept input).
	ErrNotRunning = errors.New("dolphin: the bridge is not in the running state")
	// ErrSnapshotLabel: the label is not a safe file name.
	ErrSnapshotLabel = errors.New("dolphin: snapshot label must be 1-32 characters of [A-Za-z0-9._-]")
)
