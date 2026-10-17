package session

import (
	"fmt"

	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/recognition"
)

// Kind names the five message classes the reducer accepts. It is a class, not a
// topic: the class decides eviction priority in the inbox (an observation is
// disposable, a Stop is not) and it decides which staleness rule applies.
type Kind uint8

const (
	KindObservation Kind = iota
	KindRanking
	KindDecision
	KindSpeech
	KindControl
)

func (k Kind) String() string {
	switch k {
	case KindObservation:
		return "observation"
	case KindRanking:
		return "ranking"
	case KindDecision:
		return "decision"
	case KindSpeech:
		return "speech"
	case KindControl:
		return "control"
	default:
		return fmt.Sprintf("kind(%d)", uint8(k))
	}
}

// Header is what every message must carry for the reducer to decide whether it
// may be applied at all. The four fields are exactly the checks in docs/spec/17
// "Engine state and idempotency" — session, generation, profile revision, seq —
// and nothing else, because a message that could smuggle in instructions about
// *how* to apply itself would make the reducer's decisions unauditable.
//
// A message is immutable once handed to Send. The reducer copies what it needs;
// it never retains a caller's slice (Ranking.Candidates in particular is the
// matcher's own allocation, and docs/spec/12's replay tests must be able to reuse
// one ranking for two attempts).
type Header struct {
	// SessionID names the session this message belongs to. A message for another
	// session is discarded: a restart, a subscription leak or a replayed durable
	// log all deliver them, and applying one would corrupt state that looks
	// healthy.
	SessionID string
	// Seq is the session-wide message sequence. docs/spec/17 dedupes on
	// "event.seq <= active.applied_seq", which is only meaningful if the sequence
	// is shared by every producer; Send stamps an unassigned Seq from the
	// reducer's own counter so that holds for the live path, and a replay tool may
	// stamp its own so that a re-delivered message is recognised as a duplicate
	// (docs/spec/12 "duplicate SSE delivery").
	Seq int64
	// Generation is the utterance generation the producer believed active when it
	// created this message. Anything below the active generation is stale by
	// definition and is discarded and counted (docs/spec/01 invariant 2).
	Generation int64
	// ProfileRevision is the published revision the producer matched against
	// (docs/spec/01 "Only published profile revisions are used for runtime"). A
	// mismatch is discarded: candidate ids from an old revision would name
	// different messages than the same ids mean now.
	ProfileRevision string
}

// Msg is one immutable input to the reducer.
type Msg interface {
	MsgKind() Kind
	MsgHeader() Header
	// Validate reports a message the reducer must not try to interpret. It runs
	// before any state is touched so a malformed message cannot half-apply.
	Validate() error
}

// observationSeqFloor bounds ObservationMsg.ObservationSeq; frame 0 is a
// legitimate frame number for a capture device, but a *message* sequence must be
// positive so that 0 stays available to mean "unassigned".
func (h Header) validate(sessionRequired bool) error {
	if sessionRequired && h.SessionID == "" {
		return fmt.Errorf("session_id is required")
	}
	if h.Generation < 0 {
		return fmt.Errorf("generation %d is negative", h.Generation)
	}
	if h.Seq < 0 {
		return fmt.Errorf("seq %d is negative", h.Seq)
	}
	return nil
}

// ObservationMsg carries one frame's look at one region into the reducer.
//
// The reducer owns the region's recognition.Tracker, so this message carries the
// raw observation and nothing derived from it: state, accepted text and lifetime
// are *computed* here, from the message stream, which is what makes the reducer's
// state reproducible from a log (docs/spec/12 "Unit … state reducer"). A message
// that carried a pre-computed snapshot would let two components disagree about
// what state the box was in, and the disagreement would be resolved by whichever
// one wrote last.
type ObservationMsg struct {
	Header
	// RegionID is which box this is about; the reducer keeps one tracker and one
	// occurrence per region (docs/spec/17's region list: "bottom", "choices"…).
	RegionID string
	// Observation is the immutable frame data: source clock, capture timestamp,
	// region presence, region hash, and the OCR result (nil when the hash was
	// unchanged and the OCR call was skipped).
	Observation recognition.Observation
}

func (m ObservationMsg) MsgKind() Kind     { return KindObservation }
func (m ObservationMsg) MsgHeader() Header { return m.Header }
func (m ObservationMsg) String() string {
	return fmt.Sprintf("observation(%s seq=%d)", m.RegionID, m.Observation.FrameSeq)
}
func (m ObservationMsg) Validate() error {
	if m.RegionID == "" {
		return fmt.Errorf("region_id is required")
	}
	if m.Observation.RegionID != "" && m.Observation.RegionID != m.RegionID {
		return fmt.Errorf("region_id %q does not match observation region %q", m.RegionID, m.Observation.RegionID)
	}
	if err := m.Header.validate(true); err != nil {
		return err
	}
	return m.Observation.Validate()
}

// RankingMsg returns candidate retrieval for one observation. It is a reply, so
// it names the observation it answers (ObservationSeq) rather than carrying a new
// look at the screen — the distinction is what lets the reducer notice that the
// answer arrived after the screen moved on.
type RankingMsg struct {
	Header
	RegionID string
	// ObservationSeq is the ObservationMsg.Seq this ranking was computed for. A
	// ranking for an observation older than the newest one already ranked is stale
	// work arriving late (docs/spec/12 "two OCR futures finish out of order"), and
	// the reducer records it as stale instead of applying it.
	ObservationSeq int64
	// Text is the string that was ranked. Re-checking it against the region's
	// accepted text is cheap and catches a ranking worker that raced the reducer,
	// which would otherwise commit a line the box no longer shows.
	Text string
	// SceneID is the scene of the winning candidate, resolved by the ranking
	// worker from the profile it owns. The reducer does not look messages up:
	// scene *belief* is the matcher's business, scene *change* — the fact that the
	// player is somewhere else now — is a session fact worth an event
	// (docs/spec/01 session.scene_changed, docs/spec/05 "Scene belief graph").
	SceneID string
	// Ranking is the retrieval answer, carried in the matcher's own type.
	Ranking matcher.Ranking
}

func (m RankingMsg) MsgKind() Kind     { return KindRanking }
func (m RankingMsg) MsgHeader() Header { return m.Header }
func (m RankingMsg) String() string {
	return fmt.Sprintf("ranking(%s obs=%d cand=%d reason=%s)", m.RegionID, m.ObservationSeq, len(m.Ranking.Candidates), m.Ranking.Reason)
}
func (m RankingMsg) Validate() error {
	if m.RegionID == "" {
		return fmt.Errorf("region_id is required")
	}
	if m.ObservationSeq <= 0 {
		return fmt.Errorf("observation_seq %d must be positive", m.ObservationSeq)
	}
	if err := m.Header.validate(true); err != nil {
		return err
	}
	for _, code := range []string{m.Ranking.Reason} {
		if code == "" {
			return fmt.Errorf("ranking reason is required")
		}
	}
	return nil
}

// DecisionMsg is the advisory answer from the decision layer (Clef/VLM). It is
// advisory in the literal sense that the reducer never *asks* — it merges the
// answer into the pending observation and lets the commit rules read it.
// docs/spec/05: "model responses are advisory messages accepted only when event
// epoch, profile revision and generation ID match".
type DecisionMsg struct {
	Header
	RegionID string
	// ObservationSeq binds the answer to one observation. An answer for a
	// superseded observation is stale even when its generation still matches —
	// the player may have advanced within one generation.
	ObservationSeq int64
	// MessageID is the candidate the model chose, or UnknownMessage. It may
	// *narrow* a commit (naming the line the matcher already had) but never
	// invent one the ranking did not offer; see reducer.applyDecision.
	MessageID    string
	SpeakerID    string
	SpeakerKnown bool
	// SpeechEligible is the model's safe-to-speak answer. It may only turn a
	// would-be utterance *off*: docs/spec/05 allows silence as a degraded mode and
	// forbids speaking as a named actor without evidence, but a single high
	// probability is not why we start audio.
	SpeechEligible bool
	// Accepted reports whether the provider's answer survived the decision
	// layer's own checks. A rejected answer is still worth recording — it is the
	// difference between "the model abstained" and "the model never answered" in
	// the developer inspector — but it changes nothing.
	Accepted bool
	Provider string
	RouteID  string
	Model    string
	Reason   string
}

func (m DecisionMsg) MsgKind() Kind     { return KindDecision }
func (m DecisionMsg) MsgHeader() Header { return m.Header }
func (m DecisionMsg) String() string {
	return fmt.Sprintf("decision(%s obs=%d msg=%s speaker=%s)", m.RegionID, m.ObservationSeq, m.MessageID, m.SpeakerID)
}
func (m DecisionMsg) Validate() error {
	if m.RegionID == "" {
		return fmt.Errorf("region_id is required")
	}
	if m.ObservationSeq <= 0 {
		return fmt.Errorf("observation_seq %d must be positive", m.ObservationSeq)
	}
	return m.Header.validate(true)
}

// SpeechKind is the audio lifecycle stage being reported back. speech.requested
// is deliberately absent: the reducer does not schedule synthesis (that is
// internal/speech's job), it only needs to know what has happened to the audio it
// allowed.
type SpeechKind string

const (
	SpeechReady   SpeechKind = "ready"
	SpeechStarted SpeechKind = "started"
	SpeechEnded   SpeechKind = "ended"
	SpeechFailed  SpeechKind = "failed"
)

func (k SpeechKind) valid() bool {
	switch k {
	case SpeechReady, SpeechStarted, SpeechEnded, SpeechFailed:
		return true
	default:
		return false
	}
}

// SpeechMsg reports audio progress for one committed utterance. The Generation in
// its header is the generation the clip was made for, which is the whole
// mechanism behind docs/spec/12's "Zero stale audio starts after a new committed
// dialogue event": a clip synthesized for generation 11 that finishes streaming
// after generation 12 committed arrives with Generation 11 and is discarded here,
// not applied and then filtered from the UI.
type SpeechMsg struct {
	Header
	RegionID string
	// OccurrenceID must name the committed occurrence this clip belongs to. A
	// clip for an occurrence the reducer never committed is refused: it would put
	// an event on the wire that no state change backs, which is exactly the
	// uncorrelatable event docs/spec/09 tells the dev UI to distrust.
	OccurrenceID  string
	Kind          SpeechKind
	CacheKey      string
	VoiceID       string
	VoiceRevision int64
	Error         string
	LatencyMS     int64
}

func (m SpeechMsg) MsgKind() Kind     { return KindSpeech }
func (m SpeechMsg) MsgHeader() Header { return m.Header }
func (m SpeechMsg) String() string {
	return fmt.Sprintf("speech(%s %s gen=%d)", m.Kind, m.OccurrenceID, m.Generation)
}
func (m SpeechMsg) Validate() error {
	if !m.Kind.valid() {
		return fmt.Errorf("speech kind %q is not one of ready|started|ended|failed", string(m.Kind))
	}
	if m.OccurrenceID == "" {
		return fmt.Errorf("occurrence_id is required")
	}
	return m.Header.validate(true)
}

// ControlKind is an operator or lifecycle instruction. Controls are the only
// messages the inbox will never evict (docs/spec/01 "User progression always
// wins").
type ControlKind string

const (
	ControlPause          ControlKind = "pause"
	ControlResume         ControlKind = "resume"
	ControlStop           ControlKind = "stop"
	ControlProfileChanged ControlKind = "profile_changed"
)

func (k ControlKind) valid() bool {
	switch k {
	case ControlPause, ControlResume, ControlStop, ControlProfileChanged:
		return true
	default:
		return false
	}
}

// ControlMsg changes what the session is *for*, not what it believes.
type ControlMsg struct {
	Header
	Kind ControlKind
	// NewProfileRevision is the revision now in force, required for
	// ControlProfileChanged. It is carried separately from Header.ProfileRevision
	// because the header describes the world the message was *sent from*: after
	// this control applies, every message stamped with the old revision becomes
	// droppable, and conflating the two would make the first post-publish
	// observation look like a mismatch.
	NewProfileRevision string
	Reason             string
}

func (m ControlMsg) MsgKind() Kind     { return KindControl }
func (m ControlMsg) MsgHeader() Header { return m.Header }
func (m ControlMsg) String() string    { return fmt.Sprintf("control(%s)", string(m.Kind)) }
func (m ControlMsg) Validate() error {
	if !m.Kind.valid() {
		return fmt.Errorf("control kind %q is unknown", string(m.Kind))
	}
	if m.Kind == ControlProfileChanged && m.NewProfileRevision == "" {
		return fmt.Errorf("profile_changed requires new_profile_revision")
	}
	return m.Header.validate(true)
}

var (
	_ Msg = ObservationMsg{}
	_ Msg = RankingMsg{}
	_ Msg = DecisionMsg{}
	_ Msg = SpeechMsg{}
	_ Msg = ControlMsg{}
)
