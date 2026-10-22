package session

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
)

// ErrStopped reports work offered to a session that has already been stopped. It
// is returned rather than swallowed because a caller that keeps feeding a dead
// session has a lifecycle bug, and "silently accepted" is how that bug becomes a
// memory leak in the inbox.
var ErrStopped = errors.New("session: reducer stopped")

// Reducer serializes every state change in one session and owns the active
// utterance generation (docs/spec/01 "One session event reducer serializes state
// changes and owns active utterance generation counter").
//
// Concurrency shape, stated exactly: Send is safe from any goroutine and never
// blocks; Run drains the inbox on one goroutine; Apply is safe from any goroutine
// and applies one message under a mutex, in the order it acquires the lock. That
// mutex is why "single goroutine" is expressed as a *serialization guarantee*
// rather than as a thread identity — what the invariants need is that no two
// messages are interpreted against each other's view of the state, and a test
// that calls Apply directly (docs/spec/12's scripted timelines) must exercise the
// identical code path the live loop does, not a single-threaded variant of it.
//
// State fields are guarded by applyMu; the exported readers (State, Stats,
// Generation) copy under the same lock.
type Reducer struct {
	cfg   Config
	pub   Publisher
	clock Clock
	ids   IDGen

	inbox *inbox
	wake  chan struct{}

	applyMu sync.Mutex
	st      State
	stats   Stats

	// trackers and regions are only touched under applyMu.
	regions    map[string]*region
	trackerCfg recognition.Config
	// kindOf names a committed message's text class (dialogue, sign, item, …)
	// from the candidate index the session was built against; nil — a session
	// with no corpus view — means "no class is ever known", which the class
	// filter reads as "never veto on class": a filter that cannot name a line
	// must not silence it.
	kindOf func(messageID string) (games.TextKind, bool)
	// committedKeys remembers occurrence identities that have already been
	// committed, oldest-evicted, so a replayed or repeated observation cannot
	// commit the same line twice even after the box has moved on.
	committedKeys []committedIdentity
	log           EventLog
	// lastSessionEventID is the causation anchor for events that belong to no one
	// region, i.e. session.stopped.
	lastSessionEventID string
}

// committedIdentity is the dedupe key of one spoken occurrence: which box, which
// lifetime of that box, and which content. Content is identified by canonical
// message id when one is known and by the normalized text otherwise, because
// docs/spec/12's "same text spoken by two actors" case must dedupe on the *id*
// (different occurrences of one string) while the "repeated OCR of one box" case
// must dedupe on the string (one occurrence, fifty observations).
//
// The struct is compared with ==, so it carries only comparable, representation-
// stable fields: a time.Time in this key would make two records of one occurrence
// unequal whenever they were stamped through different monotonic readings, which is
// a dedupe bug dressed as a timestamp.
type committedIdentity struct {
	regionID string
	lifetime uint64
	content  string
}

// region is one ROI's worth of session state: the text machine that describes it,
// the occurrence currently inside it, and what has happened to that occurrence's
// audio.
type region struct {
	id string
	tr *recognition.Tracker

	// lifetime is the box lifetime the fields below belong to. It comes from the
	// tracker, and any change to it voids the occurrence: that is how "the same
	// text in a box that came back" becomes a new dialogue_occurrence_id
	// (docs/spec/12 "repeat NPC after menu") without the session having to watch
	// for the dismissal itself.
	lifetime uint64
	traceID  string

	occurrenceID string
	messageID    string
	text         string
	key          string
	generation   int64
	phase        SpeechPhase
	predictive   bool

	// settling is the catch-up policy's settling lines (issue #25, ADR-0122
	// decision 2/8): an occurrence whose audio is still live after a newer
	// occurrence committed on this region. The region's single phase is the
	// current occurrence's; a line that committed a successor keeps its own
	// phase (and its message id, for the terminal's payload) here so its
	// terminal (it ends when its playback does, not when the box advances) can
	// still be applied and emitted. Bounded; entries leave on their terminal.
	// latest_only never settles a line this way: its commit cancels the older
	// audio at the source, so the older occurrence reaches its terminal
	// (cancelled) before a successor commits.
	settling map[string]settlingLine

	// lastKeySeq / lastTextSeq drive text.observed: the event is emitted when the
	// accepted text changes or the box's state machine moves, not once per frame.
	// At 5 Hz OCR an unfiltered text.observed would be the loudest event in the
	// system while saying nothing new, and docs/spec/01's backpressure policy
	// exists to stop exactly that kind of traffic from reaching the UI.
	lastKey      string
	lastTransSeq int64
	lastObsSeq   int64
	keySinceSeq  int64
	// emittedKey is the accepted key the last text.observed described. A frame that
	// repeats that key at the same state is not news, which is the perception-side
	// half of docs/spec/01's backpressure rule ("drops redundant frame previews
	// first, not state transitions").
	emittedKey string
	// lastEventID is this region's causation anchor: the id of the most recent
	// event emitted for it, so candidates name the observation they answer and the
	// commitment names the candidate list it came out of (docs/spec/01
	// "derivations refer to causation_id").
	lastEventID string

	// rankings holds the recent ranking replies for this box, keyed by the
	// observation sequence they answer. Bounded (maxRankingsPerRegion): the
	// ranking worker is asynchronous, and an unbounded map of replies addressed to
	// observations from ten minutes ago is a leak with a plausible excuse.
	rankings  []rankedObservation
	decisions []answeredObservation
}

// maxRankingsPerRegion bounds how many in-flight ranking replies the reducer will
// remember. Five frames of one stable line is the realistic worst case for
// "decision arrived after the next frame"; anything older is answering a screen
// that no longer exists.
const maxRankingsPerRegion = 8

// maxSettlingLines bounds the catch-up policy's settling lines (ADR-0122
// decision 2): the occurrences whose audio is still live after a successor
// committed on the region. Each leaves on its terminal, so a healthy session
// holds only the handful of lines inside one batch window; the bound is the
// backstop for a wedged audio path, the same shape as the runtime's cancel
// table cap (ADR-0122 decision 4).
const maxSettlingLines = 16

// settlingLine is one catch-up line still settling on a region: its speech
// phase and the message id its terminal's payload must name (the region's own
// message id belongs to the successor, not to the line still being heard).
type settlingLine struct {
	phase     SpeechPhase
	messageID string
}

type rankedObservation struct {
	obsSeq  int64
	text    string
	scene   string
	rank    matcher.Ranking
	applied bool
}

type answeredObservation struct {
	obsSeq  int64
	msgID   string
	speaker string
	known   bool
	// speakerConfidence is the adjudicator's confidence for the speaker the
	// answer named (the chosen option's probability; 0 when absent or unknown).
	// It is the speaker gate's evidence (ADR-0156), carried onto the commit
	// payload that decides whether the line's voice is the answer's speaker.
	speakerConfidence float64
	// speakerKind is the adjudicator's apparent-kind answer (ADR-0158): the
	// chosen speaker_kind option's id, or "" when the answer was absent. It is
	// the kind gate's evidence on the same fall-back path the speaker
	// confidence gates, so a kind the frame did not establish stays unnamed.
	speakerKind string
	// speakerKindConfidence is the adjudicator's confidence for the kind it
	// chose (the chosen option's probability; 0 when absent or unknown).
	speakerKindConfidence float64
	// eligible is the model's safe_to_speak answer; vetoOnly means the commit
	// rules may read it as a way to say "not this", never as a way to say "yes".
	eligible bool
	accepted bool
	// attributionOnly marks the answer as carrying attribution only (ADR-0160):
	// the early-speaker answer asked for the line's speaker, and the line itself
	// was already settled by the local evidence's confident top candidate, so
	// the answer may not narrow the commit's candidate choice.
	attributionOnly bool
	provider        string
	model           string
}

// SpeechPhase is where one committed utterance has got. It is a phase rather than
// a bool because "was it cancelled" and "was it ever audible" answer different
// questions in the acceptance report (docs/spec/12 "duplicate speech", "stale
// speech after advance", "prediction misfires").
type SpeechPhase string

const (
	PhaseNone      SpeechPhase = ""
	PhaseReady     SpeechPhase = "ready"
	PhaseStarted   SpeechPhase = "started"
	PhaseEnded     SpeechPhase = "ended"
	PhaseFailed    SpeechPhase = "failed"
	PhaseCancelled SpeechPhase = "cancelled"
)

// active reports whether audio for this utterance could still be heard or is
// pending, i.e. whether cancelling it would mean something.
func (p SpeechPhase) active() bool { return p == PhaseReady || p == PhaseStarted }

// Option configures a Reducer.
type Option func(*Reducer)

// WithConfig sets the reducer's own policy (queue bound, predictive opt-in,
// candidate bound). Options are applied before normalization, so a caller may
// hand a partially filled Config and have its zero fields take defaults — which
// is what Config's documented meaning of "zero means default" requires.
func WithConfig(c Config) Option {
	return func(r *Reducer) { r.cfg = c }
}

// WithClock replaces the event timestamp source. Logical time never uses it.
func WithClock(c Clock) Option {
	return func(r *Reducer) {
		if c != nil {
			r.clock = c
		}
	}
}

// WithIDs replaces the identifier source so a test can assert "one occurrence, one
// id" and "a new box lifetime, a different id" without racing a clock.
func WithIDs(g IDGen) Option {
	return func(r *Reducer) {
		if g != nil {
			r.ids = g
		}
	}
}

// WithTrackerConfig sets the recognition thresholds for every region this session
// tracks (docs/spec/02 gives regions their own OCR settings; a profile overrides
// them per region at construction time, and nothing here is game knowledge).
func WithTrackerConfig(c recognition.Config) Option {
	return func(r *Reducer) {
		for _, reg := range r.regions {
			reg.tr = recognition.NewTracker(reg.id, c)
		}
		r.trackerCfg = c
	}
}

// WithMessageKind names a committed message's text class (dialogue, sign, item, …)
// for the commitment policy's narrated-class filter (docs/spec/09 "narrated
// classes"). The wiring is the candidate index's KindFor: the corpus the
// session was built against is the only authority on a message's class, and a
// session with no corpus view passes nil and simply never vetoes on class. It
// is an Option rather than a Config field because it is a per-session
// *capability* (a function over the session's own index), not a policy bound
// the operator tunes.
func WithMessageKind(f func(messageID string) (games.TextKind, bool)) Option {
	return func(r *Reducer) {
		r.kindOf = f
	}
}

// WithEventLog retains the published envelopes for inspection. The hub is the
// durable path; this is the in-process one a test or a dev inspector reads.
type EventLog interface {
	Append(e events.Envelope)
}

// WithLog attaches an EventLog. Production wiring passes nil and reads the hub's
// subscription instead, so that there is exactly one authoritative event store
// (docs/spec/01 "Persist bounded metadata").
func WithLog(l EventLog) Option {
	return func(r *Reducer) {
		r.log = l
	}
}

// New prepares a session reducer. sessionID must already exist (it is a foreign
// key to the sessions table, migration 0003); an empty one is rejected rather
// than invented, because events with no session cannot be filtered by the SSE
// endpoint a browser is watching.
func New(sessionID, profileRevision string, pub Publisher, opts ...Option) (*Reducer, error) {
	if sessionID == "" {
		return nil, fmt.Errorf("session: session_id is required")
	}
	if err := events.ValidateSessionID(sessionID); err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	if pub == nil {
		return nil, fmt.Errorf("session: a publisher is required (docs/spec/01: the reducer is the only writer of session state, and its events are the only outward face)")
	}
	r := &Reducer{
		cfg:        DefaultConfig(),
		pub:        pub,
		clock:      realClock{},
		ids:        NewIDGen(),
		regions:    map[string]*region{},
		wake:       make(chan struct{}, 1),
		trackerCfg: recognition.DefaultConfig(),
		st: State{
			SessionID:       sessionID,
			ProfileRevision: profileRevision,
			SceneID:         "",
		},
	}
	for _, o := range opts {
		o(r)
	}
	r.cfg = r.cfg.normalize()
	r.inbox = newInbox(r.cfg.InboxSize, true)
	return r, nil
}

// Send hands a message to the reducer. It never blocks, which is the whole reason
// the capture loop may call it: docs/spec/01 requires that nothing downstream of
// the frame source can stall the frame source. When the inbox is full, the
// oldest message of a less important class leaves the queue first (see inbox);
// what is returned says whether *this* message got in.
func (r *Reducer) Send(m Msg) bool {
	if m == nil {
		return false
	}
	accepted, _ := r.inbox.push(m)
	select {
	case r.wake <- struct{}{}:
	default:
	}
	_ = accepted
	return accepted
}

// Wake notifies Run that work arrived. Exposed for a producer that batches
// several Sends and wants one wakeup; Send already does this itself.
func (r *Reducer) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run drains the inbox until ctx is cancelled or a ControlMsg{Stop} is applied.
// It returns nil on a clean stop and ctx.Err() otherwise, so a supervisor can
// tell "the session ended" from "the process is going away" (docs/spec/01
// "graceful degraded modes", AGENTS.md "deadline/cancellation propagation").
func (r *Reducer) Run(ctx context.Context) error {
	for {
		if m, ok := r.inbox.pop(); ok {
			if _, err := r.Apply(ctx, m); err != nil && !errors.Is(err, context.Canceled) {
				r.markPublishError(err)
			}
			if r.Stopped() {
				return nil
			}
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-r.wake:
		}
	}
}

func (r *Reducer) markPublishError(err error) {
	r.applyMu.Lock()
	r.stats.PublishErrors++
	r.applyMu.Unlock()
	_ = err
}

// InboxDepth reports queued-but-unapplied messages, for health and for the
// docs/spec/13 metric that a dead worker looks like.
func (r *Reducer) InboxDepth() int { return r.inbox.depth() }

// Result is what one Apply call did.
type Result struct {
	// Applied is false when the message changed nothing, with Reason saying why in
	// one of the six discard classes. A test asserts on this rather than inferring
	// from state, because "the reducer silently did nothing" and "the reducer
	// refused for a recorded reason" must not look the same from outside.
	Applied bool
	Kind    Kind
	Reason  string
	// Events are the envelopes this message caused, in publication order, already
	// numbered by the publisher. Causation chains and trace ids are visible here,
	// which is how the tests assert correlation instead of eyeballing a log.
	Events []events.Envelope
}

// StaleNotice records a discarded message. docs/spec/17 says "record(stale),
// discard" — the discard is the safety property, the record is what makes a
// provider's 30-second-latency bug visible in the developer UI instead of a
// mystery silence (docs/spec/13 "explain degraded status").
type StaleNotice struct {
	Kind       Kind      `json:"kind"`
	Generation int64     `json:"generation"`
	Active     int64     `json:"active_generation"`
	Seq        int64     `json:"seq"`
	RegionID   string    `json:"region_id,omitempty"`
	At         time.Time `json:"at"`
	Why        string    `json:"why"`
}

// maxStaleNotices bounds the retained notices. They are diagnostics, not history:
// the durable count is Stats.Stale, and a UI that needs more reads the events.
const maxStaleNotices = 64

// State is the reducer's readable state. It is a copy.
type State struct {
	SessionID       string `json:"session_id"`
	ProfileRevision string `json:"profile_revision"`
	Generation      int64  `json:"generation"`
	AppliedSeq      int64  `json:"applied_seq"`
	Paused          bool   `json:"paused"`
	Stopped         bool   `json:"stopped"`
	// PredictiveMode and NarratedKinds are the session's live operator policy
	// (docs/spec/09): the conservative/predictive half of the decision mode and
	// the narrated classes the commitment policy applies. They move on a
	// settings control and are read by the pipeline's ranking gate from here —
	// the single source the policy and the evidence half agree on. CatchUp is
	// the speech side's half of the same control (issue #25, ADR-0122): the
	// catch-up narration policy the audio side runs, moved on the same settings
	// control and shown in the same snapshot.
	PredictiveMode bool             `json:"predictive_mode"`
	NarratedKinds  []games.TextKind `json:"narrated_kinds"`
	CatchUp        string           `json:"catch_up"`
	SceneID        string           `json:"scene_id,omitempty"`
	Recent         []string         `json:"recent_message_ids,omitempty"`
	Regions        []RegionState    `json:"regions"`
	Stale          []StaleNotice    `json:"recent_stale,omitempty"`
}

// RegionState is one region's readable state, including the occurrence the
// session is currently standing behind.
type RegionState struct {
	RegionID             string            `json:"region_id"`
	TextState            recognition.State `json:"text_state"`
	Text                 string            `json:"text"`
	NormalizedKey        string            `json:"normalized_key"`
	RawText              string            `json:"raw_text"`
	Confidence           float64           `json:"confidence"`
	Lifetime             uint64            `json:"lifetime"`
	ObservationSeq       int64             `json:"observation_seq"`
	TraceID              string            `json:"trace_id,omitempty"`
	DialogueOccurrenceID string            `json:"dialogue_occurrence_id,omitempty"`
	MessageID            string            `json:"message_id,omitempty"`
	CommittedText        string            `json:"committed_text,omitempty"`
	Generation           int64             `json:"generation,omitempty"`
	Speech               SpeechPhase       `json:"speech_phase,omitempty"`
	Predictive           bool              `json:"predictive,omitempty"`
}

// Stats counts what the reducer decided, in the classes an acceptance report
// needs (docs/spec/12 "duplicate speech", "stale speech after advance";
// docs/spec/16 M2 "repeat observation doesn't create duplicate speech,
// out-of-order results discarded"). Every counter is a *decision*, never a
// bookkeeping tick, so that a number in the report means something a reviewer can
// act on.
type Stats struct {
	Applied                  int64  `json:"applied"`
	Invalid                  int64  `json:"invalid"`
	OtherSession             int64  `json:"discarded_other_session"`
	Stale                    int64  `json:"stale"`
	ProfileMismatch          int64  `json:"discarded_profile_revision_mismatch"`
	Duplicated               int64  `json:"duplicated"`
	RejectedState            int64  `json:"rejected_transition"`
	Commits                  int64  `json:"commits"`
	CommitBlocked            int64  `json:"commit_blocked"`
	SceneChanges             int64  `json:"scene_changes"`
	Cancellations            int64  `json:"cancellations"`
	DuplicateContent         int64  `json:"duplicate_content_suppressed"`
	OCRSkippedSameHash       int64  `json:"ocr_skipped_same_hash"`
	Events                   int64  `json:"events"`
	PublishErrors            int64  `json:"publish_errors"`
	PublishErrorsLast        string `json:"publish_errors_last,omitempty"`
	DroppedAfterStop         int64  `json:"dropped_after_stop"`
	FutureGeneration         int64  `json:"discarded_future_generation"`
	RejectedOccurrence       int64  `json:"rejected_unknown_occurrence"`
	CommitRejected           int64  `json:"commit_rejected_by_text_machine"`
	BrokenCausation          int64  `json:"broken_causation"`
	InboxDepth               int64  `json:"inbox_depth"`
	DroppedInboxObservations int64  `json:"dropped_inbox_observations"`
	DroppedInboxRankings     int64  `json:"dropped_inbox_rankings"`
	DroppedInboxSpeech       int64  `json:"dropped_inbox_speech"`
	DroppedInboxControl      int64  `json:"dropped_inbox_control"`
}

// Generation reports the active utterance generation. Producers that must stamp
// their work (a TTS request that will outlive the frame it came from) read it
// before dispatch and carry it in the message they later send back — that stamp
// is the fence docs/spec/01 invariant 2 is about.
func (r *Reducer) Generation() int64 {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	return r.st.Generation
}

// Stopped reports whether a Stop has been applied.
func (r *Reducer) Stopped() bool {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	return r.st.Stopped
}

// Stats copies the counters plus the current queue depth.
func (r *Reducer) Stats() Stats {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	s := r.stats
	s.InboxDepth = int64(r.inbox.depth())
	s.OCRSkippedSameHash = r.ocrSkippedLocked()
	var drops [kindCount]int64
	if r.inbox != nil {
		drops = r.inbox.drops()
	}
	s.DroppedInboxObservations = drops[KindObservation]
	s.DroppedInboxRankings = drops[KindRanking]
	s.DroppedInboxSpeech = drops[KindSpeech]
	s.DroppedInboxControl = drops[KindControl]
	return s
}

func (r *Reducer) ocrSkippedLocked() int64 {
	var n int64
	for _, reg := range r.regions {
		n += reg.tr.Stats().OCRSkippedSameHash
	}
	return n
}

// State copies the session state for a reader (developer UI, health check).
func (r *Reducer) State() State {
	r.applyMu.Lock()
	defer r.applyMu.Unlock()
	out := r.st
	out.Recent = append([]string(nil), r.st.Recent...)
	out.Regions = make([]RegionState, 0, len(r.regions))
	for _, reg := range r.regions {
		out.Regions = append(out.Regions, reg.view())
	}
	// Stable order for a byte-comparable snapshot: map iteration order is
	// randomized and a dev fixture that diffs a snapshot must not depend on it
	// (docs/spec/12 "determinism … under identical inputs").
	sortRegions(out.Regions)
	out.Stale = append([]StaleNotice(nil), r.st.Stale...)
	out.PredictiveMode = r.cfg.PredictiveMode
	out.NarratedKinds = append([]games.TextKind(nil), r.cfg.NarratedKinds...)
	out.CatchUp = r.cfg.CatchUp
	return out
}

func (rs RegionState) String() string {
	return fmt.Sprintf("%s:%s gen=%d %s", rs.RegionID, rs.TextState, rs.Generation, rs.MessageID)
}

func sortRegions(rs []RegionState) {
	for i := 1; i < len(rs); i++ {
		for j := i; j > 0 && rs[j].RegionID < rs[j-1].RegionID; j-- {
			rs[j], rs[j-1] = rs[j-1], rs[j]
		}
	}
}

func (reg *region) view() RegionState {
	snap := reg.tr.Snapshot()
	return RegionState{
		RegionID:             reg.id,
		TextState:            snap.State,
		Text:                 reg.text,
		NormalizedKey:        reg.key,
		RawText:              snap.Raw,
		Confidence:           snap.Confidence,
		Lifetime:             reg.lifetime,
		ObservationSeq:       reg.lastObsSeq,
		TraceID:              reg.traceID,
		DialogueOccurrenceID: reg.occurrenceID,
		MessageID:            reg.messageID,
		CommittedText:        snap.Committed,
		Generation:           reg.generation,
		Speech:               reg.phase,
		Predictive:           reg.predictive,
	}
}

// rankingFor returns the newest ranking reply that answers *this* text: it must
// name the same string the box currently reads, and it must be about an
// observation from when this text became the accepted one onward. Both conditions
// are needed. Without the first, a reply to an older reading of the same box could
// commit the current text under evidence for another one. Without the second, a
// stale reply that happened to carry the same words could commit text that had
// already been through a page turn and back.
func (reg *region) pendingRanking(text string, sinceSeq int64) (rankedObservation, bool) {
	var best rankedObservation
	found := false
	for _, rk := range reg.rankings {
		if rk.applied || rk.obsSeq < sinceSeq {
			continue
		}
		if rk.text != "" && text != "" && rk.text != text {
			continue
		}
		if !found || rk.obsSeq > best.obsSeq {
			best, found = rk, true
		}
	}
	return best, found
}

// consumeRanking marks the reply for one observation as used. It is called only
// where a commitment decision was actually reached — emitted, or suppressed as a
// duplicate — and never where the attempt was blocked (paused, missing margin, a
// text machine that refused). Evidence consumed by a blocked attempt is evidence
// that can never commit: a ranking worker that answers once for a line that was
// paused would otherwise leave that line uncommittable after the resume, which is
// a silent stall instead of a refusal anyone can see.
func (reg *region) consumeRanking(obsSeq int64) {
	for i := range reg.rankings {
		if reg.rankings[i].obsSeq == obsSeq {
			reg.rankings[i].applied = true
			return
		}
	}
}

// decisionFor returns the advisory answer for one observation, if one arrived.
func (reg *region) decisionFor(obsSeq int64) (answeredObservation, bool) {
	for i := len(reg.decisions) - 1; i >= 0; i-- {
		if reg.decisions[i].obsSeq == obsSeq {
			return reg.decisions[i], true
		}
	}
	return answeredObservation{}, false
}
