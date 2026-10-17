package runtime

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Event names the runtime publishes around a session lifecycle (docs/spec/08
// lists session.started/stopped in the event catalogue; the names are
// registered at construction, the M1 invariant events.Register enforces).
const (
	EventSessionStarted = "session.started"
	EventSessionFailed  = "session.failed"
)

// Session states (docs/spec/08 "Example session workflow" response examples).
const (
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
	StateStopped  = "stopped"
	StateFailed   = "failed"
)

// Terminal stop reasons. "requested" is an operator stop; "source_end" is the
// replay recording or live capture ending on its own; "error" means the run
// failed (see the session.failed event for the class) and "closed" means the
// server shut the runtime down.
const (
	StopReasonRequested = "requested"
	StopReasonSourceEnd = "source_end"
	StopReasonError     = "error"
	StopReasonClosed    = "closed"
)

// Sentinel errors, each mapping to one documented HTTP answer so the API
// layer never re-interprets free-text (docs/spec/08 problem type).
var (
	ErrAnotherSessionRunning = errors.New("runtime: another session is already running")
	ErrProfileNotFound       = errors.New("runtime: profile not found")
	ErrProfileNotPublished   = errors.New("runtime: profile revision is not the published revision")
	ErrProfileNotRunnable    = errors.New("runtime: published profile has no regions to observe")
	ErrInvalidMode           = errors.New("runtime: mode must be conservative or predictive")
	ErrUnknownSourceKind     = errors.New("runtime: unknown frame source kind")
	ErrSourceUnsupported     = errors.New("runtime: frame source kind is not implemented in this build")
	ErrSourceNotAllowed      = errors.New("runtime: frame source path is outside the configured allowlist")
	ErrSourceMissing         = errors.New("runtime: frame source path does not exist")
	ErrDevTraceUnavailable   = errors.New("runtime: developer trace storage is not configured")
	ErrSessionNotFound       = errors.New("runtime: no such session")
	ErrManagerClosed         = errors.New("runtime: the runtime is closed")
)

// ModeConservative and ModePredictive are the session decision modes
// (docs/spec/01 "Conservative/predictive modes").
const (
	ModeConservative = "conservative"
	ModePredictive   = "predictive"
)

// StartRequest is the POST /api/v1/sessions body after decoding: the profile,
// the frame source, the decision mode and the dev-trace toggle.
type StartRequest struct {
	ProfileID   string
	FrameSource FrameSource
	Mode        string
	DevTrace    bool
}

// FrameSource names the session's input. Dir is meaningful for replay.
type FrameSource struct {
	Kind string
	Dir  string
}

// Options wires the runtime into its neighbours. Store and Hub are mandatory;
// Jobs makes the foreground-busy pause observable to the background worker;
// AssetsRoot is where per-session dev-trace evidence lives (empty = dev trace
// is unavailable, which the API answers 503 for — a session started with
// dev_trace by an unconfigured server would be a lie); OCR and Tracker are
// test seams: production takes Tesseract and the mode's default tracker.
type Options struct {
	Store *store.Store
	Hub   *events.Hub
	Jobs  *jobs.Runner

	AssetsRoot    string
	ReplayRoots   []string
	DevTraceQuota int64 // 0 = DefaultDevTraceQuota

	OCR     ocr.Engine
	Tracker recognition.Config // zero = the mode's documented defaults

	Log *slog.Logger
}

// Manager owns the one running session (docs/spec/01 "Key invariants": one
// active session at a time), its lifecycle events and its dev-trace assets.
type Manager struct {
	opts  Options
	log   *slog.Logger
	quota int64

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// New registers the lifecycle event names and returns a Manager ready to
// accept sessions. It never blocks and never starts anything.
func New(opts Options) (*Manager, error) {
	if opts.Store == nil {
		return nil, errors.New("runtime: Options.Store is required")
	}
	if opts.Hub == nil {
		return nil, errors.New("runtime: Options.Hub is required")
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if err := events.Register(EventSessionStarted, EventSessionFailed); err != nil {
		return nil, err
	}
	quota := opts.DevTraceQuota
	if quota <= 0 {
		quota = DefaultDevTraceQuota
	}
	return &Manager{
		opts:     opts,
		log:      opts.Log,
		quota:    quota,
		sessions: map[string]*session{},
	}, nil
}
