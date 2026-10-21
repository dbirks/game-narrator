package pipeline

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/telemetry"
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
	// smallBoardCPUs is the core count at or under which live mode serialises
	// OCR (Config.MaxConcurrentOCR auto-default, issue #8): the release target
	// is a 4-core A53 board, where two concurrent Tesseract processes thrash
	// each other (measured 19 s per read against 1.1 s alone — 17×).
	smallBoardCPUs = 4
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

	// DefaultEscalationRetryAfter is the source-clock interval between escalation
	// attempts for one uncertain occurrence (ADR-0115 §1): ten seconds is the
	// measured settle time of a stable box's reads (a correct line re-OCR'd every
	// 2–4 s on the 2026-10-20 feed), so the re-ask lands after the noise settles
	// and never in the middle of it.
	DefaultEscalationRetryAfter = 10 * time.Second
	// DefaultEscalationMaxAttempts bounds one occurrence's total rankings
	// (first attempt included, ADR-0115 §1): three attempts at a ten-second
	// cadence is the docs/spec/06 budget applied to escalation — enough for a
	// slow Clef to land twice more, bounded against a line that simply will not
	// resolve.
	DefaultEscalationMaxAttempts = 3
	// DefaultDecisionCandidates is the top-k the escalation's decision request
	// offers the Decider (ADR-0115 §1): five is the shortlist the bundled
	// adjudication questions render, and it stays under the provider's own
	// option cap without crowding the speaker question's options out of it.
	DefaultDecisionCandidates = 5
	// DefaultPerceptualSkipSettle is the quiet-run length before the perceptual
	// skip arms (ADR-0115 §3): the skip requires this many consecutive crops
	// within tolerance of the region's last read before it stops re-reading.
	// The settle is the guard a single distance cannot give — on a 9×8 grid a
	// one-word typewriter growth moves 0–7 bits, inside the noise tolerance,
	// so "within tolerance" alone cannot tell a noisy stable box from a line
	// still typing; three quiet crops in a row can, because a line that is
	// still typing keeps moving.
	DefaultPerceptualSkipSettle = 3
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
	// Deterministic selects the bench's load-invariant replay semantics
	// (issue #17, ADR-0064 amendment 2, ADR-0096 amendment 1): ModeReplay with
	// the wall-clock belts removed. ModeReplay already delivers every frame in
	// order and OCRs each region strictly sequentially in frame order
	// (waitInflight — coalescing and supersede-cancel never fire in replay), so
	// the remaining paths by which host load could change a replay's outcome
	// are the wall-clock bounds that drop or fence a frame's evidence: the
	// probe-tick age-out (an observation outrun by its read is dropped and
	// counted), the retrieval-hold budget (a slow reply is let past and then
	// fenced stale) and the end-of-stream grace (an in-flight reply at the tail
	// is abandoned). In Deterministic mode none of those fires: a read is
	// waited for, never dropped on a timer, and a reply is applied before the
	// next observation of its region, so it cannot land stale. The bounds that
	// remain are the load-independent ones — the per-OCR-call deadline with the
	// replay retry budget (the wedge guard: a dead engine still fails the frame
	// and the failure is counted) and the caller's context (the run's
	// patience). The measured stage latencies (Stats.Latency) are wall-clock
	// in every mode and are reported separately, never graded on. Valid only
	// with ModeReplay; Validate refuses it elsewhere.
	Deterministic bool
	// SessionID and ProfileRevision stamp every message the pipeline produces.
	// The reducer fences messages from another session or another revision, so
	// these must name the session the Publisher is serving.
	SessionID       string
	ProfileRevision string

	// Metrics is the process's telemetry surface (docs/spec/13 "Metrics",
	// internal/telemetry): when non-nil the pipeline books its hot-path
	// counters and stage latencies there, labelled by MetricsGame and
	// ProfileRevision — the bounded label pair the spec names (game, profile
	// revision). nil (the default) instruments nothing: the hooks are
	// nil-safe, so a harness that builds no catalog keeps the same runner.
	// ProfileRevision is the revision stamp above; MetricsGame is the game
	// the profile's edition belongs to (the runtime resolves it at session
	// start; empty exposes as the "none" label value).
	Metrics     *telemetry.Product
	MetricsGame string

	// Regions are the ROIs to watch. RegionType and the rect are used; the
	// rest of the row is the catalogue's business.
	Regions []games.Region

	// OCR is the recognition engine. ocr.Tesseract in production, a scripted
	// fake in tests; both are the same interface so nothing downstream branches.
	OCR ocr.Engine

	// OCRFallback wraps OCR with an ocr.FallbackEngine when set: OCR stays the
	// primary read and OCRFallback.Secondary — a VLM behind the
	// vision_ocr_fallback provider role — is consulted only when the primary
	// read is unresolved (docs/spec/05 "optional VLM fallback for difficult
	// fonts", ADR-0039). The runtime sets this only when that role is
	// configured; nil (the default) keeps the single-engine behaviour, which
	// is what the role's preflight "NOT CONFIGURED" continues to mean.
	OCRFallback *OCRFallback

	// Decider is the optional Clef adjudication (docs/spec/05 "candidate
	// retrieval → graph-aware scoring → optional Clef/VLM adjudication →
	// commitment policy", ADR-0050): when set, a locally ambiguous ranking
	// (one that abstained, or whose margin over the runner-up falls below the
	// session's adjudication threshold — see Config.Reducer's Adjudicate and
	// AdjudicateMargin) is offered to the Decider before the commit for the
	// same observation, and the reply is applied to the session's reducer
	// before the commitment policy reads it. nil (the default) commits purely
	// on the local matcher evidence, which is what the decision role's
	// preflight "NOT CONFIGURED" continues to mean. The pipeline holds no
	// provider client (the same rule as OCRFallback.Secondary).
	Decider Decider

	// Speakers are the declared speaker options the adjudication's speaker
	// question is rendered with (in profile order, newest attribution
	// first is the runtime's business). Only meaningful when Decider is set;
	// empty means "no declared speakers", which the Decider renders as the
	// unknown option alone.
	Speakers []SpeakerOption

	// Index is the compiled candidate corpus. Safe to share across Runners.
	Index *matcher.Index

	// Controls is the operator's live-control half (pause/resume/settings,
	// docs/spec/08 "…/pause, …/resume, …/settings"), delivered to the session's
	// single writer. The runtime owns the channel and the reducer consumes it
	// through Apply, on the same goroutine that applies observations and
	// rankings — a control that reached the pipeline but not the reducer would
	// be a pause the operator pressed and the session never felt. nil (the
	// default) means "no live controls": a session the harness or a test does
	// not need to steer, and Run simply never selects the channel. The channel
	// is a plain *send* interface (session.Msg) because the controls the
	// runtime can express today are all ControlMsg, and the reducer is the
	// authority on what a message means — the pipeline forwards, it does not
	// interpret.
	Controls chan session.Msg

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

	// Workers is the OCR pool size, clamped to [1, MaxWorkers]. It bounds the
	// jobs buffer, not the recognitions: how many engine calls may run at the
	// same time is MaxConcurrentOCR.
	Workers int
	// MaxConcurrentOCR bounds how many OCR engine calls — and therefore how
	// many engine child processes (Tesseract spawns one per call) — may run
	// concurrently, clamped to [1, Workers]. Zero takes the measured default:
	// 1 in live mode on a machine with four or fewer cores, Workers otherwise.
	// The board measurement behind the default (issue #8, ADR-0096): two
	// concurrent Tesseract reads on the UNO Q's A53 slow each other to 19 s
	// against 1.1 s alone (unpinned cores; pinned to disjoint cores the same
	// runs cost 2.1 s), which is what pushes every live read past its deadline.
	MaxConcurrentOCR int
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

	// PerceptualSkipTolerance is the Hamming distance, in bits of 64, within
	// which a new crop counts as perceptually unchanged from the crop the region
	// last read, so its OCR call is skipped (ADR-0115 §3). The exact-hash skip
	// stays the zero-tolerance half; this is the tolerant one that an analog
	// feed — which never delivers two identical crops — needs. Zero takes
	// vision.DefaultPerceptualTolerance (10: the measured gap between analog
	// noise and a line change); negative disables the tolerant skip, and the
	// dedupe key is the exact hash alone.
	PerceptualSkipTolerance int
	// PerceptualSkipSettle is the quiet-run length the perceptual skip requires
	// before it arms (ADR-0115 §3): this many consecutive crops within
	// PerceptualSkipTolerance of the region's last read, after which further
	// quiet crops skip. The settle is what keeps the skip from eating a
	// typewriter: a one-word growth moves 0–7 bits on the 9×8 grid — inside
	// the tolerance — so tolerance alone cannot tell noise from a line still
	// typing, while a run of quiet crops can: growth keeps producing changes
	// beyond the quiet run. Zero takes DefaultPerceptualSkipSettle; 16 (the
	// clamp) is the most the settle is ever allowed to demand, so a slow
	// steady-state line cannot spend minutes re-reading itself.
	PerceptualSkipSettle int

	// EscalationRetryAfter is the source-clock interval between escalation
	// attempts for one uncertain occurrence (ADR-0115 §1). The first ranking of
	// an occurrence is attempt one; while the occurrence's ranking stays
	// uncertain (the session policy's NeedsAdjudication) and attempts remain, a
	// later stable observation re-ranks it once at least this much source time
	// has passed since the previous attempt's reply landed. Zero takes
	// DefaultEscalationRetryAfter.
	EscalationRetryAfter time.Duration
	// EscalationMaxAttempts bounds one occurrence's total rankings, first
	// attempt included. Zero takes DefaultEscalationMaxAttempts; one disables
	// the retry (one ranking per occurrence, the pre-ADR-0115 behaviour).
	EscalationMaxAttempts int
	// DecisionCandidates is the top-k the escalation's decision request offers
	// the Decider (ADR-0115 §1): the shortlist the model is offered is the
	// question, and the session's decide still validates the chosen id against
	// the full local ranking. Zero takes DefaultDecisionCandidates.
	DecisionCandidates int

	// Presence parameterizes the textbox-present detector.
	Presence PresenceConfig

	// SceneID seeds matcher observations (matcher.Observation.SceneID). Empty
	// means "unknown", which scores the scene component neutrally.
	SceneID string
	// TextKindHint is the fallback text-kind hint the matcher's text-kind
	// component scores against for a region whose on-screen role names no
	// class of its own (subtitle, speaker label, HUD, other); a region whose
	// role does (textbox → dialogue, sign, item, menu, tutorial —
	// games.RegionType.TextKindHint) ranks its readings with the role's class
	// instead, so a sign region's reads score against the corpus's sign
	// messages without the caller naming a per-region hint (docs/spec/04
	// "Visual profiles"). games.TextDialogue for the fallback is the honest
	// default.
	TextKindHint games.TextKind

	// Trace retains a bounded in-memory frame→state→text→candidate→event timeline
	// readable with Runner.Trace(). Off by default: the event log is the durable
	// record, this is the one a test prints.
	Trace bool

	// TraceMax bounds the trace ring's row count; zero keeps the runtime default
	// (defaultTraceMax). A harness that reads the ring after a long replay sizes
	// it to the run so no row is evicted and asserts Stats.TraceDropped is 0
	// (ADR-0058 amendment 3). Negative is a configuration error.
	TraceMax int

	// Now stamps Stats latency buckets. nil uses time.Now; tests may inject.
	Now func() time.Time
}

// OCRFallback is the pipeline's view of the VLM fallback (ADR-0039): a
// secondary engine plus its policy. The pipeline does not construct the
// secondary itself — building an ocr.VLMEngine over a role-configured
// provider client is the runtime's job, because the runtime owns the
// provider clients — so this field is the seam the runtime passes later.
type OCRFallback struct {
	// Secondary is consulted when the primary read is unresolved. Required
	// when OCRFallback is set (Validate reports a nil one).
	Secondary ocr.Engine
	// Policy tunes the trigger threshold, rate limit and crop cache. Zero
	// fields take the documented defaults (ocr package).
	Policy ocr.FallbackPolicy
}

// SpeakerOption is one declared speaker option for the adjudication's
// speaker question. It mirrors the provider adapter's own speaker shape
// without importing it: the pipeline stays provider-free (the same rule
// that keeps OCRFallback.Secondary an ocr.Engine, not a client).
type SpeakerOption struct {
	ID          string
	Description string
}

// DecisionRequest is one locally ambiguous ranking the pipeline offers to
// the Decider (docs/spec/05 "candidate retrieval → graph-aware scoring →
// optional Clef/VLM adjudication → commitment policy"): the ranking that
// just finished scoring, the observation it answers and the context the
// bundled adjudication questions are rendered from. The pipeline owns the
// ordering — the call sits between the local scoring and the commit for
// this observation — and the fences: the reply the Decider returns is
// applied to the session's reducer before the commit, stamped with the
// generation it was asked under, so a reply that lands after a newer commit
// is discarded by the session's own generation fence.
type DecisionRequest struct {
	// SessionID and ProfileRevision are the session the request belongs to;
	// the Decider stamps them onto the message it returns.
	SessionID       string
	ProfileRevision string
	// RegionID names the box; ObservationSeq and Generation are the fences
	// the reply has to carry back (docs/spec/01 invariant 2).
	RegionID       string
	ObservationSeq int64
	Generation     int64
	// SceneID is the scene the ranking scored against ("" when unknown).
	SceneID string
	// Text and RawText are the accepted and raw readings the ranking was
	// computed from; Confidence is the OCR confidence of the raw reading.
	Text       string
	RawText    string
	Confidence float64
	// Ranking is the local scoring's answer: the ordered candidates, the
	// margin over the runner-up and the matcher's own verdict.
	Ranking matcher.Ranking
	// Speakers are the declared speaker options for the speaker question, in
	// profile order. Empty means "no declared speakers", which the Decider
	// renders as the unknown option alone.
	Speakers []SpeakerOption
	// LastMessage is the id of the last committed message, when known; the
	// request's context carries it the way docs/spec/05's example context does.
	LastMessage string
}

// DecisionReply is the Decider's answer to one DecisionRequest. The pipeline
// applies Msg to the session's reducer before the commit for the request's
// observation (a nil Msg is an abstain: the commit proceeds on the local
// scoring alone, and the reply's metadata still goes on the decision events
// so the abstention is explainable). The metadata fields are what the
// decision.requested/resolved events carry: the provider's request id and
// latency are the correlation the developer UI joins frame → OCR →
// candidates → Clef request → commit → audio on (docs/spec/13).
type DecisionReply struct {
	// Msg is the session's decision message, or nil when no message may be
	// applied (the role could not be asked or did not answer, and the
	// commit proceeds on the local evidence).
	Msg *session.DecisionMsg
	// Provider is the provider label for the events ("systemone").
	Provider string
	// RequestID is the provider's request id ("" when no provider call
	// was made: not configured, breaker open, skipped).
	RequestID string
	// RequestIDSource names which side issued RequestID ("provider" when the
	// gateway answered with its own request-id header, "client" when the
	// X-Request-Id the adapter minted is all that exists, internal/providers/
	// systemone); empty when the id's origin is unknown (a failed call's
	// error carries the effective id but not its origin). The developer UI
	// shows the correlation key with its origin on the decision row.
	RequestIDSource string
	// Latency is the call's duration (zero when no call was made).
	Latency time.Duration
	// Model is the model that answered ("" when none).
	Model string
	// Accepted reports whether the provider's answer survived the provider
	// adapter's own checks (a rejected answer is recorded but narrows
	// nothing, docs/spec/06).
	Accepted bool
	// Reason is the one-phrase outcome: "answered" when a valid answer
	// drove the reply, otherwise the failure class ("provider_error",
	// "not_configured", "skipped", …).
	Reason string
	// Candidates are the candidate ids offered to the model, in the order
	// the request declared them (c1..cN), so the events and the wire agree
	// on what an option id meant.
	Candidates []string
	// Choice is the candidate option id the model picked ("" = none /
	// missing), as rendered on the wire (cN), before mapping back to ids.
	Choice string
	// ChosenID is the message id the answer mapped back to ("" when the
	// model picked none or the answer was missing).
	ChosenID string
	// Probabilities are the model's raw candidate probabilities, keyed by
	// message id (docs/spec/06: signals for the commitment policy, not
	// reliability guarantees).
	Probabilities map[string]float64
	// Speaker is the speaker option id the model chose ("unknown" when it
	// could not be established).
	Speaker string
	// SpeakerKnown reports the model's speaker verdict.
	SpeakerKnown bool
	// SafeToSpeak is the model's safe_to_speak answer (nil when the answer
	// was missing).
	SafeToSpeak *bool
}

// Decider is the pipeline's seam for the optional Clef adjudication
// (docs/spec/05 "Clef adjudication and commit policy", ADR-0050). The
// pipeline calls it from its dedicated decision worker for one locally
// ambiguous ranking — after the local scoring, before the commit for the
// same observation — and the worker's dedicatedness is the bounded
// concurrency: at most one adjudication is in flight per session, and the
// call's context carries the deadline (the decision role's timeout).
// The pipeline never builds a provider client (the same rule as
// OCRFallback.Secondary): the runtime constructs the implementation over
// the decision role's System One client, behind the model router.
type Decider interface {
	Decide(ctx context.Context, req DecisionRequest) DecisionReply
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
	// The concurrent-recognition bound (see the field): auto-default is the
	// measured board behaviour, not an assumption — a live read that must
	// share the four A53 cores with its neighbour costs 19 s instead of
	// 1.1 s (issue #8, ADR-0096), so live mode on a small board serialises OCR, and a
	// caller on a bigger machine (or an explicit value, or a replay, where
	// reads are per-region serial anyway) keeps the pool width.
	if c.MaxConcurrentOCR <= 0 {
		if c.Mode == ModeLive && runtime.NumCPU() <= smallBoardCPUs {
			c.MaxConcurrentOCR = 1
		} else {
			c.MaxConcurrentOCR = c.Workers
		}
	}
	if c.MaxConcurrentOCR < 1 {
		c.MaxConcurrentOCR = 1
	}
	if c.MaxConcurrentOCR > c.Workers {
		c.MaxConcurrentOCR = c.Workers
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
	if c.PerceptualSkipTolerance == 0 {
		c.PerceptualSkipTolerance = vision.DefaultPerceptualTolerance
	}
	// Negative disables the tolerant skip (the field says so); anything above
	// the hash width would skip every crop, which is a misconfiguration with an
	// obvious intended reading.
	if c.PerceptualSkipTolerance > 64 {
		c.PerceptualSkipTolerance = 64
	}
	if c.PerceptualSkipSettle <= 0 {
		c.PerceptualSkipSettle = DefaultPerceptualSkipSettle
	}
	if c.PerceptualSkipSettle > 16 {
		c.PerceptualSkipSettle = 16
	}
	if c.EscalationRetryAfter <= 0 {
		c.EscalationRetryAfter = DefaultEscalationRetryAfter
	}
	if c.EscalationMaxAttempts <= 0 {
		c.EscalationMaxAttempts = DefaultEscalationMaxAttempts
	}
	if c.EscalationMaxAttempts > 8 {
		c.EscalationMaxAttempts = 8
	}
	if c.DecisionCandidates <= 0 {
		c.DecisionCandidates = DefaultDecisionCandidates
	}
	if c.DecisionCandidates > 15 {
		c.DecisionCandidates = 15
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
	if c.OCRFallback != nil && c.OCRFallback.Secondary != nil {
		// The wrap happens here, on the normalised config, so Runner.Config()
		// reports the engine that actually runs — a caller reading back
		// "ocr-fallback" knows the fallback was live without guessing.
		c.OCR = &ocr.FallbackEngine{
			Primary:   c.OCR,
			Secondary: c.OCRFallback.Secondary,
			Policy:    c.OCRFallback.Policy,
		}
	}
	return c
}

// Validate reports the mistakes that have no safe default. It is called once by
// New, so a Runner that exists is a Runner that can run.
func (c Config) Validate() error {
	switch {
	case c.OCR == nil:
		return ErrNoEngine
	case c.OCRFallback != nil && c.OCRFallback.Secondary == nil:
		return errors.New("pipeline: ocr_fallback.secondary is required")
	case c.Index == nil:
		return ErrNoIndex
	case len(c.Regions) == 0:
		return ErrNoRegion
	case c.Deterministic && c.Mode != ModeReplay:
		return errors.New("pipeline: deterministic is a ModeReplay-only semantic (the bench's load-invariant replay, ADR-0064 amendment 2); live mode keeps its wall-clock belts")
	case c.SessionID == "":
		return errors.New("pipeline: session_id is required")
	case c.TraceMax < 0:
		return errors.New("pipeline: trace_max must be ≥ 0 (0 = default ring size)")
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
