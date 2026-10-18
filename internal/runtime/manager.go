package runtime

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/pipeline"
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
	// ErrAudioUnavailable: the session has no speech pipeline — either the
	// process has no voice workflow configured, or the session's sink could
	// not be built. The answer says so rather than pretending a control was
	// applied to nothing (docs/spec/13: degraded status is reported).
	ErrAudioUnavailable = errors.New("runtime: no speech pipeline on this session")
	// ErrInvalidAudioSettings: a control value outside its documented range
	// (volume 0..1, speech_delay_ms 0..speechDelayMaxMS).
	ErrInvalidAudioSettings = errors.New("runtime: invalid audio settings")
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

	AssetsRoot  string
	ReplayRoots []string
	// ReplaySpeed paces a replayed session's frames: 0 (the default) drains
	// the recording as fast as the pipeline consumes it, the documented
	// Frame-replay-gate behaviour (docs/spec/12: the decision is a pure
	// function of the frames, never of machine speed). 1.0 paces real time
	// from the recording's own deltas; only a caller that deliberately wants
	// its evidence on the wall clock (the live session test's audio half) sets
	// it (ADR-0039).
	ReplaySpeed   float64
	DevTraceQuota int64 // 0 = DefaultDevTraceQuota

	OCR     ocr.Engine
	Tracker recognition.Config // zero = the mode's documented defaults

	// OCRFallback is the VLM fallback wiring (docs/spec/05 "Compare
	// lightweight OCR against an optional VLM fallback for difficult fonts",
	// ADR-0040): when its Secondary is non-nil — an ocr.VLMEngine over the
	// vision_ocr_fallback role's OpenAI-compatible client — each session's
	// OCR engine is wrapped in an ocr.FallbackEngine: the local read always
	// runs first and for free, and the VLM is consulted only for unresolved
	// reads, or for every frame when one of the session's regions flags
	// difficult_font in its ocr_settings. Policy's zero fields take the
	// documented ocr-package defaults (threshold 0.6, 1 call/s burst 3,
	// crop cache 64); the per-session DifficultFont is merged in here from
	// the session's regions. nil (the default) keeps the single-engine
	// behaviour, which is what the role's preflight "NOT CONFIGURED"
	// continues to mean.
	OCRFallback *pipeline.OCRFallback

	// Decision is the Clef adjudication wiring (docs/spec/05 "Clef
	// adjudication and commit policy", ADR-0050): when its Role is a
	// configured System One role, each session's pipeline runs the
	// commitment policy with the adjudication on — a locally ambiguous
	// ranking holds its commit for one adjudication (bounded by the role's
	// timeout, at most one in flight), and the answer narrows the commit
	// (CLEF_DISAMBIGUATED) or vetoes the utterance; a failure abstains, the
	// commit proceeds on the local evidence and the session reports
	// degraded. nil (the default) keeps sessions local, which is what the
	// role's preflight "NOT CONFIGURED" continues to mean.
	Decision *DecisionOptions

	// DevDolphin is the dev-only seam that hands a running Dolphin bridge's
	// frame source to a session (docs/spec/11, ADR-0007). It is called only
	// for a kind:dolphin request on an explicit GAME_NARRATOR_DEV_DOLPHIN opt-
	// in; nil (the default) keeps that kind a 501, which is the production
	// shape: the core runtime never imports or launches the emulator itself.
	DevDolphin func() (capture.Source, error)

	// ManualSource is the test seam for an explicitly-driven frame source
	// (docs/spec/12 "deterministic delay-injection suite"): a kind:manual
	// session runs the source this closure returns, and the caller pushes
	// frames at its own pace, waiting on specific events between pushes, so
	// a test asserts an ordering on the event stream instead of a wall-clock
	// margin. nil (the default) keeps the kind refused (ErrSourceUnsupported)
	// — production wires no closure, so the seam is unreachable there.
	ManualSource func() (capture.Source, error)

	// Speech wires the per-session voice pipeline (docs/spec/07, ADR-0039):
	// committed lines resolve the speaker's approved voice and play through
	// the TTS cache, the fenced scheduler and the configured sink. nil
	// (the default) runs sessions silent: the voice workflow is not
	// configured in this process, and the audio API answers 503.
	Speech *SpeechOptions
	Log    *slog.Logger
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
	if opts.Speech != nil && opts.Speech.Voices == nil {
		return nil, errors.New("runtime: Speech options require the voice service (Speech.Voices)")
	}
	// A half-configured fallback is a wiring bug, not a degraded mode: the
	// degraded mode is a nil OCRFallback (the role is not configured), and
	// the pipeline refuses a set fallback without its secondary — so does
	// the Manager, at construction rather than at the first session start.
	if opts.OCRFallback != nil && opts.OCRFallback.Secondary == nil {
		return nil, errors.New("runtime: OCR fallback options require a secondary engine (OCRFallback.Secondary)")
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
