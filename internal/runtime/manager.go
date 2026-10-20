package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"

	"github.com/dbirks/game-narrator/internal/capture"
	"github.com/dbirks/game-narrator/internal/capture/stack"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/store"
	"github.com/dbirks/game-narrator/internal/telemetry"
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
	// ErrClipsUnavailable (ADR-0073): a session asked for a stored clip by id
	// but this process has no managed clip store wired — the id names nothing
	// here, so the answer is the documented degraded shape, not a 404 that
	// blames a clip nobody installed.
	ErrClipsUnavailable = errors.New("runtime: the managed clip store is not configured in this process")

	// The v4l2 (UVC HDMI capture dongle) frame source (ADR-0078): the device
	// is resolved against the live enumeration by stable id — the
	// enumeration is the allowlist, the same discipline the replay roots
	// apply to directories — and the mode is the operator's pinned choice
	// (while it stays an offered mode) or the ADR-0076 policy's pick.
	// ErrCaptureUnavailable is the unwired answer (a nil wiring, which
	// production never is): a process without capture wiring refuses the
	// kind rather than faking a device, the same no-simulation-theater rule
	// the dolphin gate applies to the emulator.
	ErrCaptureDeviceAbsent    = errors.New("runtime: no usable capture device is attached")
	ErrCaptureDeviceUnknown   = errors.New("runtime: the requested capture device is not in the current enumeration")
	ErrCaptureModeUnavailable = errors.New("runtime: no offered capture mode fits the selection policy")
	ErrCaptureUnavailable     = errors.New("runtime: no capture device wiring is configured in this process")
	// ErrInvalidClipID (ADR-0073): the clip id a session named is not a clip id
	// at all (the id contract is the traversal boundary, docs/spec/14
	// "Filesystem"). The answer is 400 with the rule named, never a path the
	// id almost spelled.
	ErrInvalidClipID = errors.New("runtime: the clip id is not a valid clip id")
	// ErrSessionNotRunning: a live control (pause, resume, settings) addressed
	// to a session that has already reached a terminal state. The control is
	// refused rather than silently dropped: a pause the session cannot take is
	// not a pause, and the operator needs the 409 to know the run is over.
	ErrSessionNotRunning = errors.New("runtime: the session is not running")
	// ErrInvalidSessionSettings: a mid-session settings request whose mode or
	// narrated classes are outside the documented vocabulary. The answer is a
	// 400 naming the rule (the same shape as ErrInvalidAudioSettings), never a
	// best-guess interpretation of a value the policy cannot read.
	ErrInvalidSessionSettings = errors.New("runtime: invalid session settings")
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

// FrameSource names the session's input. Dir is meaningful for replay. ClipID
// (ADR-0073) selects a clip from the managed clip store by id instead of by
// path: the id is the only client-controlled component, it is checked against
// the clip id contract before any filesystem access, and the directory it
// resolves to still has to pass the replay-root allowlist — the store is a
// root, so it passes — so an id cannot name a directory outside the store.
type FrameSource struct {
	Kind   string
	Dir    string
	ClipID string
}

// SessionSettings is the mid-session operator policy of PUT
// /api/v1/sessions/{id}/settings (docs/spec/08 "…/settings", docs/spec/09
// "/play": the narrated classes and the conservative/predictive toggle).
// Every field is a pointer: a nil field keeps the session's current value, so
// a client that only flips the mode does not have to re-send the classes it
// never touched — the same partial-body rule the audio route follows.
//
// Mode is one of the Mode* constants ("" or an unknown word is a 400);
// NarratedClasses is the set of games.TextKind the session may narrate. The
// session reducer force-includes dialogue (it is the product), so a request
// for "signs only" is applied as "dialogue and signs": the runtime records
// the normalized set on the snapshot, which is what the UI's toggles read.
type SessionSettings struct {
	// Mode is the decision mode: conservative or predictive. nil keeps the
	// session's current value (the partial-body contract).
	Mode *string `json:"mode,omitempty"`
	// NarratedClasses is the narrated-class vocabulary: the games.TextKind
	// words, normalized by the runtime (dialogue first, always present).
	// nil keeps the session's current value.
	NarratedClasses *[]string `json:"narrated_classes,omitempty"`
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
	// ClipsDir is the managed clip store (ADR-0073): the directory inside the
	// data directory the API installs uploaded clips into. When non-empty the
	// manager admits it as a replay root in addition to ReplayRoots — the
	// product path by which a clip reaches a running appliance without an
	// operator editing the service environment — and sessions may select a
	// clip by id against it. Empty means this process has no clip store: a
	// session that names a clip id then answers ErrClipsUnavailable.
	ClipsDir string
	// ReplaySpeed paces a replayed session's frames: 0 (the default) drains
	// the recording as fast as the pipeline consumes it, the documented
	// Frame-replay-gate behaviour (docs/spec/12: the decision is a pure
	// function of the frames, never of machine speed). 1.0 paces real time
	// from the recording's own deltas; only a caller that deliberately wants
	// its evidence on the wall clock (the live session test's audio half) sets
	// it (ADR-0039).
	ReplaySpeed   float64
	DevTraceQuota int64 // 0 = DefaultDevTraceQuota

	// Metrics is the process's telemetry surface (docs/spec/13 "Metrics",
	// internal/telemetry): when set, every session the manager starts wires
	// its pipeline and voice stages into it, and the manager's own active
	// session count rides on it as the session gauge. nil (the default) is
	// the off shape — no hooks, no gauge — and every caller of the hooks is
	// nil-safe, so a manager built without a catalog behaves exactly as
	// before.
	Metrics *telemetry.Product

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

	// Capture wires the v4l2 (UVC HDMI capture dongle) frame source
	// (ADR-0078): Resolve answers the session's device and mode (the
	// capture/stack layer in production: the stable-id allowlist, the
	// persisted selection, the ADR-0076 policy), and Source builds the
	// live source for the resolved device (ffmpegpipe.V4L2 in production,
	// the labelled-synthetic testsrc2 seam under EnvFake). nil (the default)
	// keeps kind v4l2 answered ErrCaptureUnavailable: the production wiring
	// is always present, so the nil case is an unwired process refusing
	// rather than faking a device.
	Capture *CaptureOptions

	// Speech wires the per-session voice pipeline (docs/spec/07, ADR-0039):
	// committed lines resolve the speaker's approved voice and play through
	// the TTS cache, the fenced scheduler and the configured sink. nil
	// (the default) runs sessions silent: the voice workflow is not
	// configured in this process, and the audio API answers 503.
	Speech *SpeechOptions
	Log    *slog.Logger
}

// CaptureOptions is the v4l2 (UVC HDMI capture dongle) wiring's seam pair
// (ADR-0078): both halves are closures so the runtime's tests run on a fake
// enumeration and a push-based source without touching v4l2-ctl or FFmpeg,
// and production passes the capture/stack layer's own functions unchanged.
type CaptureOptions struct {
	// Resolve resolves the session's capture device and mode. requestedID
	// is the session's frame_source.dir: the device's stable id (the API's
	// id field), or empty for the persisted selection (then the first
	// usable device). The answer is what the session runs, so a fallback is
	// visible in the snapshot, not guessed.
	Resolve func(ctx context.Context, requestedID string) (stack.Resolved, error)
	// Source builds the live source for the resolved device and mode
	// (production: the ffmpegpipe.V4L2 constructor, ADR-0076's low-latency
	// defaults; the source's ID is the "v4l2:<node>" identity the no-signal
	// wrapper keys on).
	Source func(ctx context.Context, res stack.Resolved) (capture.Source, error)
}

// Manager owns the one running session (docs/spec/01 "Key invariants": one
// active session at a time), its lifecycle events and its dev-trace assets.
type Manager struct {
	opts  Options
	log   *slog.Logger
	quota int64
	// metrics is the process's telemetry surface (docs/spec/13 "Metrics",
	// internal/telemetry); nil (the default) is the off shape and every hook
	// is nil-safe. The manager's active-session count rides on it as the
	// session gauge.
	metrics *telemetry.Product
	// masterVolume is the appliance's device-level master volume (ADR-0074),
	// initialised from SpeechOptions and updated live by SetMasterVolume. It
	// is float64 bits in a Uint64 (the stdlib atomic of this toolchain has no
	// Float64 type); the sessions' master sinks read it at every playback.
	masterVolume atomic.Uint64

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
	// ocrPool is the process-shared persistent OCR engine (issue #13,
	// ADR-0100): the warm Tesseract process pool every session's OCR region
	// reads through when no explicit engine is configured. It is created
	// lazily on the first session and lives for the process: the 0.59 s warm
	// startup is paid once at first use, not per session. A configured
	// Options.OCR (the test seams) is used as given, pool or not.
	ocrPool *ocr.Pooled
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
	// The device-level master volume defaults to unity: an unset value must
	// play what the session volume decides, and the atomic's zero value
	// (bits of 0.0) would read as a muted device.
	mv := 1.0
	if opts.Speech != nil && opts.Speech.MasterVolume > 0 {
		if opts.Speech.MasterVolume <= 1 {
			mv = opts.Speech.MasterVolume
		}
	}
	mgr := &Manager{
		opts:     opts,
		log:      opts.Log,
		quota:    quota,
		sessions: map[string]*session{},
		metrics:  opts.Metrics,
	}
	mgr.masterVolume.Store(math.Float64bits(mv))
	return mgr, nil
}

// ocrEngine resolves the session's recognition engine: the configured engine
// (a test seam) as given, else the process-shared warm pool (issue #13,
// ADR-0100). The pool keeps the Tesseract model loaded across reads — the
// live A53 read costs the inference only (~0.57 s measured) instead of the
// 1.13 s per-call process startup (ADR-0096) — and its per-process fallback
// keeps a broken engine degrading the way ADR-0012 always made it.
func (m *Manager) ocrEngine() ocr.Engine {
	if m.opts.OCR != nil {
		return m.opts.OCR
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ocrPool == nil {
		m.ocrPool = ocr.NewPooled(ocr.Tesseract{}, ocr.DefaultPoolSlots)
	}
	return m.ocrPool
}

// loadMasterVolume reports the manager's device-level master volume (ADR-0074)
// clamped to 0..1. It is the loader the sessions' master sinks and the API
// snapshot read through.
func (m *Manager) loadMasterVolume() float64 {
	v := math.Float64frombits(m.masterVolume.Load())
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

// SetMasterVolume applies the appliance's device-level master volume (0..1,
// ADR-0074). It takes effect from the next playback of every session (each
// master sink reads the value at Play time) and is the API's live update
// path for the Settings page's master-volume control. The bound is validated
// here so a caller cannot steer the samples out of range.
func (m *Manager) SetMasterVolume(v float64) error {
	if v < 0 || v > 1 {
		return fmt.Errorf("runtime: master volume must be in 0..1, got %v", v)
	}
	m.masterVolume.Store(math.Float64bits(v))
	return nil
}

// MasterVolume reports the manager's current device-level master volume
// (the value the sessions apply, clamped to 0..1).
func (m *Manager) MasterVolume() float64 { return m.loadMasterVolume() }
