package pipeline

import (
	"errors"
	"fmt"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// Bounds on the knobs a caller may set. They are here rather than inline so a
// misconfigured profile produces one actionable error at construction instead of
// an unbounded goroutine count on a 4-core appliance (docs/spec/13 "backpressure
// and memory ceilings").
const (
	// DefaultWorkers is docs/spec/01's "ROI worker pool of 1–2".
	DefaultWorkers = 2
	// MaxWorkers is the same sentence's upper bound; asking for more is clamped,
	// not refused, because "as many as you allow" is the intent.
	MaxWorkers = 4
	// DefaultOCRTimeout is ocr.DefaultTimeout — a hard kill for one wedged child
	// so the frame loop cannot stall behind it.
	DefaultOCRTimeout = ocr.DefaultTimeout
	// DefaultReplayOCRTimeout is the per-call budget one ModeReplay recognition
	// gets. It is generous on purpose: a replay must be the recording it claims
	// to be (docs/spec/12 "Frame replay"), so a slow host that makes a four
	// second read take nine is a load spike, not a lost frame — and the retry
	// policy that follows a timeout exists to say exactly that.
	DefaultReplayOCRTimeout = 30 * time.Second
	// ReplayOCRMaxAttempts bounds one replay read's total engine calls: the
	// initial attempt plus two retries. A read still failing after its budget
	// is a degraded frame, not a silently dropped one (Stats.ReplayDegraded
	// Frames names it and the scenario's replay_not_degraded assertion fails
	// the report).
	ReplayOCRMaxAttempts = 3
	// DefaultFrameProbeHz is the low-rate "the box has not changed, keep checking"
	// cadence docs/spec/05 asks for instead of per-frame OCR.
	DefaultFrameProbeHz = 5
	// Min/MaxFrameProbeHz bound the probe. Below 1 Hz a dismissed box is reported
	// late; above 50 Hz the probe is a busy loop with a name.
	MinFrameProbeHz = 1
	MaxFrameProbeHz = 50

	// DefaultMaxPendingOCR bounds how many unresolved observations one region may
	// hold. It is the ceiling on "OCR is slower than frames": beyond it the oldest
	// in-flight recognition is cancelled and the frame it belonged to is dropped
	// and counted, which is the honest live-runtime behaviour.
	DefaultMaxPendingOCR = 4
	// DefaultInboundQueue bounds the reducer's inbound mailbox (observations and
	// ranking replies share it, so one sequence space fences them — see Runner).
	DefaultInboundQueue = 256
)

// ErrNoEngine / ErrNoIndex name the two inputs without which the chain is a
// capture loop that talks to nobody. They are sentinel errors so a caller can
// distinguish "the profile has no messages yet" from "the caller forgot a field".
var (
	ErrNoEngine = errors.New("pipeline: an ocr.Engine is required")
	ErrNoIndex  = errors.New("pipeline: a matcher.Index is required")
	ErrNoRegion = errors.New("pipeline: at least one region is required")
)

// PresenceConfig parameterizes "is a textbox actually on screen inside this
// ROI", which the dialogue state machine needs (recognition.Observation.
// RegionPresent drives APPEARING and DISMISSED) and which nothing upstream
// provides: the capture layer knows pixels, not semantics.
//
// The rule is deliberately crude and game-agnostic — the share of a raw crop
// whose luma is below DarkLuma must reach MinDarkShare. A dialogue box is a
// translucent panel, so it is darker than the scene behind it; a scene that
// happens to be dark makes the region read "present" with no text, which the
// tracker handles as an empty read rather than as a line. A profile that needs a
// different detector supplies its own thresholds rather than a code path
// (docs/spec/11 step 4 forbids fixture-specific branches).
type PresenceConfig struct {
	// DarkLuma is the luma below which a pixel counts as panel, in [0,255].
	DarkLuma uint8
	// MinDarkShare is the fraction of the crop that must be dark.
	MinDarkShare float64
}

// DefaultPresence is the panel detector described above. Both numbers are
// measured on the synthetic corpus (scripts/fixtures/gen-synthetic-replay.sh),
// which is an invented clip, not a game — see TestPresenceMatchesCorpusTruth.
func DefaultPresence() PresenceConfig {
	return PresenceConfig{DarkLuma: 90, MinDarkShare: 0.30}
}

func (p PresenceConfig) normalize() PresenceConfig {
	if p.DarkLuma == 0 {
		p.DarkLuma = DefaultPresence().DarkLuma
	}
	if !(p.MinDarkShare > 0) { // unset, NaN or negative
		p.MinDarkShare = DefaultPresence().MinDarkShare
	}
	if p.MinDarkShare > 1 {
		p.MinDarkShare = 1
	}
	return p
}

// QueuePolicy selects the frame queue between the capture loop and the ROI
// stage. The two policies are docs/spec/01's single sentence: a bounded
// single-slot latest-frame queue for live runtime, *unlike* lossless recording.
type QueuePolicy uint8

const (
	// QueueLatest drops to the newest frame: what a live player needs, and the
	// reason a stalled OCR cannot make the runtime analyse a frame from a second
	// ago. Drops are counted in Stats.FramesDropped.
	QueueLatest QueuePolicy = iota
	// QueueLossless delivers every frame in order, applying backpressure to the
	// source. A fixture replay must be the recording it claims to be (docs/spec/12
	// "Frame replay"), and a determinism assertion cannot hold over a queue whose
	// drops depend on worker timing.
	QueueLossless
)

// Mode selects the Runner's runtime semantics. It is the code-level form of
// docs/spec/01 "Parallelism and deadline contracts", whose first two bullets
// draw the same line: a live runtime runs a bounded single-slot latest-frame
// queue and cancels an in-flight recognition on a newer observation, *unlike*
// lossless recording, where every frame must be delivered in order and nothing
// may be dropped or cancelled on the way.
type Mode uint8

const (
	// ModeLive is the default. Live-runtime semantics: Queue is honoured as set
	// (default QueueLatest), and a newer frame of the same region supersedes the
	// in-flight recognition of an older one — the superseded read is killed and
	// counted (Stats.OCRCancelled / OCRStale), which is what keeps the runtime
	// analysing the current screen rather than a second-old one.
	ModeLive Mode = iota
	// ModeReplay is the lossless fixture-replay semantics docs/spec/12 "Frame
	// replay" requires of the CI gate: delivery is QueueLossless whatever Queue
	// said (a replay that dropped a frame is a different recording), and each
	// region's OCR runs to completion in frame order — a newer frame never
	// cancels an in-flight recognition, it waits for it. The per-call timeout
	// still bounds a wedged engine. A clean replay ends with ocr_cancelled,
	// ocr_stale and frames_dropped all zero, and its committed messages and
	// their capture timestamps are identical run to run, on a fast box or a
	// slow one: the outcome depends on the frames and the engine, not on CPU
	// speed.
	ModeReplay
)

// Config is the Runner's whole input. Zero fields mean defaults except where a
// value is genuinely required (Engine, Index, Regions). Zero Mode is ModeLive.
type Config struct {
	// Mode selects live versus lossless-replay semantics; see Mode. ModeReplay
	// forces Queue to QueueLossless in normalize, so a caller cannot ask for a
	// replay that drops frames.
	Mode Mode
	// SessionID and ProfileRevision stamp every message the pipeline produces.
	// The reducer fences messages from another session or another revision, so
	// these must name the session the Publisher is serving.
	SessionID       string
	ProfileRevision string

	// Regions are the ROIs to watch. RegionType and the rect are used; the
	// rest of the row is the catalogue's business.
	Regions []games.Region

	// OCR is the recognition engine. ocr.Tesseract in production, a scripted
	// fake in tests; both are the same interface so nothing downstream branches.
	OCR ocr.Engine

	// Index is the compiled candidate corpus. Safe to share across Runners.
	Index *matcher.Index

	// Reducer and Tracker are the settings of the two state machines this
	// pipeline feeds. They are separate configs because they answer separate
	// questions (what does the box read? what may we say about it?).
	Reducer session.Config
	Tracker recognition.Config

	// RankOptions tunes retrieval. RecentMessageIDs in a ranking request come
	// from the reducer's own history, not from here.
	RankOptions matcher.RankOptions

	// Preprocess and Crop are the vision recipe. A profile stores per-region OCR
	// settings; until the profile loader feeds them, one recipe applies to every
	// region — and ImageHash dedupe is computed *after* preprocessing so two
	// recipes never share a hash namespace (see Steps.Hash).
	Preprocess vision.Steps
	Crop       vision.CropOptions

	// Workers is the OCR pool size, clamped to [1, MaxWorkers].
	Workers int
	// OCRTimeout bounds one engine call. A newer frame cancels it earlier when it
	// can; this is the backstop when no newer frame arrives. In ModeReplay the
	// per-call budget is ReplayOCRTimeout instead: there no frame supersedes the
	// one being read, so a call that outlives OCRTimeout is a load spike to
	// retry, not a stale answer to discard.
	OCRTimeout time.Duration
	// ReplayOCRTimeout is the per-call OCR budget in ModeReplay only; zero takes
	// DefaultReplayOCRTimeout. ModeLive ignores it and keeps OCRTimeout.
	ReplayOCRTimeout time.Duration
	// FrameProbeHz is the idle re-check cadence, and the tick on which held
	// observations age out (see Runner.probe).
	FrameProbeHz int

	// Queue selects the capture→ROI queue. Default QueueLatest.
	Queue QueuePolicy
	// InboundQueue bounds the mailbox between the ROI/ranking workers and the
	// reducer goroutine. Full means the oldest observation is shed and counted.
	InboundQueue int
	// MaxPendingOCR bounds unresolved observations per region.
	MaxPendingOCR int
	// MaxRankingJobs bounds in-flight rankings per region. Default 1: the
	// reducer can only commit the newest evidence, so a queue of rankings for
	// superseded text is work the appliance cannot use.
	MaxRankingJobs int

	// Presence parameterizes the textbox-present detector.
	Presence PresenceConfig

	// SceneID seeds matcher observations (matcher.Observation.SceneID). Empty
	// means "unknown", which scores the scene component neutrally.
	SceneID string
	// TextKindHint seeds the matcher's text-kind component. games.TextDialogue
	// for a textbox region is the honest default.
	TextKindHint games.TextKind

	// Trace retains a bounded in-memory frame→state→text→candidate→event timeline
	// readable with Runner.Trace(). Off by default: the event log is the durable
	// record, this is the one a test prints.
	Trace bool

	// Now stamps Stats latency buckets. nil uses time.Now; tests may inject.
	Now func() time.Time
}

// normalize clamps rather than rejecting, except for the three required inputs,
// which Validate reports. A negative worker count would mean an unusable pool and
// a zero OCR timeout would mean no bound at all — both have an obvious intended
// reading, so they take defaults and the effective values are readable back from
// Runner.Config().
func (c Config) normalize() Config {
	if c.Mode == ModeReplay {
		// A replay must be the recording it claims to be (docs/spec/12 "Frame
		// replay"): backpressure instead of drops, whatever Queue asked for.
		c.Queue = QueueLossless
	}
	if c.Workers <= 0 {
		c.Workers = DefaultWorkers
	}
	if c.Workers > MaxWorkers {
		c.Workers = MaxWorkers
	}
	if c.OCRTimeout <= 0 {
		c.OCRTimeout = DefaultOCRTimeout
	}
	if c.Mode == ModeReplay && c.ReplayOCRTimeout <= 0 {
		c.ReplayOCRTimeout = DefaultReplayOCRTimeout
	}
	if c.FrameProbeHz < 1 {
		c.FrameProbeHz = DefaultFrameProbeHz
	}
	if c.FrameProbeHz < MinFrameProbeHz {
		c.FrameProbeHz = MinFrameProbeHz
	}
	if c.FrameProbeHz > MaxFrameProbeHz {
		c.FrameProbeHz = MaxFrameProbeHz
	}
	if c.InboundQueue <= 0 {
		c.InboundQueue = DefaultInboundQueue
	}
	if c.InboundQueue > 1<<14 {
		c.InboundQueue = 1 << 14
	}
	if c.MaxPendingOCR <= 0 {
		c.MaxPendingOCR = DefaultMaxPendingOCR
	}
	if c.MaxPendingOCR > 32 {
		c.MaxPendingOCR = 32
	}
	if c.MaxRankingJobs <= 0 {
		c.MaxRankingJobs = 1
	}
	if c.MaxRankingJobs > 8 {
		c.MaxRankingJobs = 8
	}
	c.Presence = c.Presence.normalize()
	c.Preprocess = c.Preprocess.Clamp()
	if c.Preprocess.Scale == 0 {
		c.Preprocess = vision.DefaultSteps()
	}
	if c.TextKindHint == "" {
		c.TextKindHint = games.TextDialogue
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Validate reports the mistakes that have no safe default. It is called once by
// New, so a Runner that exists is a Runner that can run.
func (c Config) Validate() error {
	switch {
	case c.OCR == nil:
		return ErrNoEngine
	case c.Index == nil:
		return ErrNoIndex
	case len(c.Regions) == 0:
		return ErrNoRegion
	case c.SessionID == "":
		return errors.New("pipeline: session_id is required")
	}
	seen := map[string]bool{}
	for i, r := range c.Regions {
		if r.ID == "" {
			return fmt.Errorf("pipeline: region %d has no id", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("pipeline: region %q appears twice", r.ID)
		}
		seen[r.ID] = true
		if vision.RectOf(r).Empty() {
			return fmt.Errorf("pipeline: region %q has an empty rect", r.ID)
		}
	}
	return nil
}
