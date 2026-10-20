// Package session owns the runtime's one serializable truth: what is currently
// committed, which generation of speech that belongs to, and what the outside
// world is allowed to learn about it.
//
// It is the `SessionReducer` of docs/spec/17 "Engine state and idempotency" and
// the "One session event reducer serializes state changes and owns active
// utterance generation counter" worker of docs/spec/01. Everything in this file
// is the *shape* of that contract — the state, the messages that may change it,
// the events it emits. reducer.go is the rules; inbox.go is the bounded
// interface to the capture loop.
//
// The rules the design is built out of, each traceable to the spec:
//
//   - Models never mutate session state (docs/spec/01). A ranking, a Clef answer
//     or a TTS acknowledgement arrives as an immutable message, and it is applied
//     only if its session, profile revision and generation still name the session
//     that is running now. The alternative — a goroutine that finishes a decision
//     request after the player advanced the page and commits the answer anyway —
//     is the "stale audio after advance" failure docs/spec/12 measures.
//   - Generation is the fence (docs/spec/01 invariant 2: "commit_id and
//     utterance_generation fence stale asynchronous work"). It increments on
//     every new committed textbox or page, and the previous generation's audio is
//     cancelled in the same step. docs/spec/05 calls the same counter an "epoch";
//     it is one number, not two, because two counters can disagree and a fence
//     that can disagree is not a fence.
//   - A committed dialogue occurrence is spoken once. The identity is
//     (region, box lifetime, canonical message or normalized text), so fifty
//     repeated observations of one line produce one message.committed
//     (docs/spec/12 "No duplicated speech for repeated observations of same
//     dialogue occurrence"), while the same line in a box that vanished and came
//     back produces two (docs/spec/12 "repeat NPC after menu").
//   - Abstention is an answer. When the matcher abstains, the occurrence commits
//     with message_id "unknown" and speech_eligible false rather than inventing a
//     line (docs/spec/05 "Never invent dialogue to fill uncertainty"), and it
//     commits *once*, so a stuck box does not emit a stream of unknowns.
//   - Events are the only outward face. Every emission carries the envelope
//     docs/spec/01 requires, with one trace_id per dialogue occurrence and a
//     causation_id naming the event it answered, because docs/spec/09 and
//     docs/spec/11 both demand that a frame, a candidate list, a speaker and an
//     audio clip be provably the same occurrence.
//
// What is deliberately NOT here: frame capture, OCR calls, candidate retrieval,
// decision providers and speech synthesis (docs/spec/01 splits those into their
// own workers, and requirement "no simulation theater" is why this package takes
// their results as messages instead of importing them). There is no game-specific
// logic and no wall-clock timer: the logical clock arrives inside messages, so a
// test can replay a 20-advance timeline and get the same events (docs/spec/12
// "Unit … state reducer … fakes").
package session

import (
	"context"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
)

// Publisher is the sink for emitted events. *events.Hub satisfies it, which is
// the point: the reducer numbers and fans out through the same log a browser
// replays from, so there is no second, private event path whose order could
// disagree with the one the UI shows.
//
// The returned envelope is retained: its EventID is the causation_id of the next
// event in the chain, and its Seq is what a restart resumes from. A publisher
// that hands back an envelope with no EventID breaks the causal chain, which the
// reducer detects and counts rather than silently emitting a chain of empty
// causes.
type Publisher interface {
	Publish(ctx context.Context, e events.Envelope) (events.Envelope, error)
}

// Clock stamps events. Logical time never comes from it — stability windows and
// deadlines are computed from each observation's capture_timestamp_ns on its own
// source clock (docs/spec/01 invariant 1) — so the only reason to inject one is
// to make an event log byte-comparable in a test.
type Clock interface {
	Now() time.Time
}

// realClock is the production default.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// IDGen mints identifiers (dialogue occurrence ids, trace ids, event ids). The
// indirection exists so a test can prove "the same occurrence twice" is the same
// id and "a new box lifetime" is a different one without depending on the
// wall-clock component of a ULID.
type IDGen func() string

// NewIDGen returns the production identifier source: 26-character ULIDs, the
// same alphabet the events envelope and the catalogue already require
// (docs/spec/02 "UUIDs or ULIDs for public IDs").
func NewIDGen() IDGen { return games.NewID }

// ValidID reports whether s is in the shared identifier alphabet. Exported
// because a caller that supplies its own occurrence ids (a replay harness, a
// correction tool) must be checked against the same rule the envelope applies,
// not a second copy of it.
func ValidID(s string) bool { return games.ValidID(s) }

// UnknownMessage is the canonical "we do not know which line this is" value in a
// message.committed payload. docs/spec/01 "High-level events" requires the
// canonical message ID "(or unknown)"; a distinct token rather than "" means a
// consumer cannot confuse "abstained" with "the field was never written", and
// docs/spec/17's reason code ABSTAINED then says *why* it is unknown.
const UnknownMessage = "unknown"

// Reasons is the vocabulary of commitment causes carried in
// message.committed.reason. docs/spec/17 "Confidence combination strategy" names
// MATCH_EXACT, GRAPH_NEIGHBOR, CLEF_DISAMBIGUATED, PREDICTIVE_PREFIX, ABSTAINED:
// those arrive verbatim inside Ranking.Reason and are copied through, because one
// string greppable across the matcher's logs, the events table and the dev
// inspector is worth more than a tidy local enum (docs/spec/13 "Correlate
// … occurrence_id, generation_id, trace_id"). COMMITTED_* names which *claim* the
// reducer made on top of that evidence — matching and committing are different
// decisions and a trace must show both.
const (
	ReasonCommittedExact   = "COMMITTED_EXACT"
	ReasonCommittedGraph   = "COMMITTED_GRAPH"
	ReasonCommittedFuzzy   = "COMMITTED_FUZZY"
	ReasonCommittedKnown   = "COMMITTED_KNOWN"
	ReasonCommittedAbstain = "COMMITTED_ABSTAINED"
	// ReasonCommittedNotNarrated: the line is a known message, but its class
	// (sign, item, …) is not in this session's narrated classes — the box
	// commits and the text machine advances, the line is just not a line the
	// operator elected to hear (docs/spec/09 "/play": narrated classes).
	ReasonCommittedNotNarrated = "COMMITTED_NOT_NARRATED"
	ReasonPredictivePrefix     = "PREDICTIVE_PREFIX"
	ReasonClefDisambiguated    = "CLEF_DISAMBIGUATED"
)

// Cancel reasons for speech.cancelled. dialogue_advanced is the one
// docs/spec/05 names explicitly ("COMMITTED→ADVANCING: … cancel previous speech
// generation"); the rest are the other ways a generation dies before its audio
// finished, each of which docs/spec/12 requires to be distinguishable in a
// report because "why did it stop talking" is the first question a reviewer asks.
const (
	CancelDialogueAdvanced = "dialogue_advanced"
	CancelRegionDismissed  = "region_vanished"
	CancelPaused           = "paused"
	CancelStopped          = "session_stopped"
	CancelProfileChanged   = "profile_changed"
	CancelPredictionWrong  = "prediction_misfire"
	CancelSuperseded       = "superseded"
)

// Config tunes the reducer. Every field is a bound or a policy switch; there is
// deliberately no scoring threshold here, because the reducer does not score —
// the matcher ranks, the decision layer judges, and this package commits or
// doesn't (docs/spec/05 keeps those stages separate on purpose).
type Config struct {
	// InboxSize bounds the queue between the capture/worker goroutines and the
	// reducer. It is measured in messages, not bytes: the workers hand over
	// small values, and what has to be bounded is how much un-applied *intent*
	// can pile up behind a slow decision. 512 is ~100 s of 5 Hz observations
	// plus the ranking replies they expect, which is the point at which a
	// backlog stops being jitter and starts being a dead worker.
	InboxSize int
	// RecentMessages is the confirmed-message history offered to future ranking
	// workers and exposed in State() (docs/spec/05 "Track previous N=10
	// confirmed messages"). Bounded, oldest evicted.
	RecentMessages int
	// PredictiveMode is this session's opt-in to committing a typewriter *prefix*
	// (docs/spec/05 "Default conservative full-line readiness, enable predictive
	// mode explicitly"). Off by default; when on it still requires the matcher's
	// PREDICTIVE_PREFIX reason and a margin, so the switch alone can never speak
	// a guess.
	PredictiveMode bool
	// MinPredictiveMargin is how much the prefix's winning candidate must beat
	// the runner-up by, in probability points (docs/spec/05 "normalized observed
	// prefix has sufficient nontrivial length and margin to runner-up"). It is
	// separate from the matcher's own RankOptions.MinMargin because the risk of
	// speaking the wrong continuation is not a retrieval-quality question.
	MinPredictiveMargin float64
	// MaxCandidates bounds how many candidates a message.candidates event
	// carries. The envelope payload limit is 64 KB (events.MaxDataSize) and a
	// 10k-message corpus must not be able to make one event enormous.
	MaxCandidates int
	// Adjudicate turns on the commitment policy's "needs adjudication" signal
	// (docs/spec/05 "Clef adjudication and commit policy", ADR-0050): when true,
	// a ranking the local scoring cannot settle on its own — one it abstained
	// on, or one whose margin over the runner-up falls below AdjudicateMargin —
	// is offered to the configured decision provider before the commit for the
	// same observation, and the answer narrows the commit (CLEF_DISAMBIGUATED).
	// Off by default: the matcher's commitment stands and no decision call is
	// made, the documented "NOT CONFIGURED" shape of the decision role.
	Adjudicate bool
	// AdjudicateMargin is the margin over the runner-up below which a
	// non-abstaining ranking still counts as ambiguous (docs/spec/05 "If model
	// confidence low"). It is separate from the matcher's RankOptions.MinMargin:
	// that floor keeps the local policy from committing on weak evidence, this
	// one decides when weak-but-accepted evidence is worth a model call. Zero
	// takes the documented default: the matcher's own acceptance floor, so by
	// default only locally-abstained rankings are offered to Clef.
	AdjudicateMargin float64
	// AllowCommitWhilePaused stops observations from committing during a pause.
	// The default (false) is the conservative reading of docs/spec/05 "belief
	// decays or resets on … pause": a paused runtime should not be deciding what
	// to say next.
	AllowCommitWhilePaused bool
	// NarratedKinds is the operator's narrated-class setting (docs/spec/09
	// "/play": narrated classes — dialogue, +signs, +items): the set of text
	// classes the commitment policy may narrate. A committed message whose
	// class is outside the set still commits (the box advances, the text
	// machine knows) but is marked not speech-eligible, which the speech
	// layer reads as "no audio for this line". dialogue is always in the set
	// — it is the product — so a caller that sends "signs only" gets
	// "dialogue and signs", never a silent narrator. The zero value takes
	// the default: dialogue alone.
	NarratedKinds []games.TextKind
}

// DefaultConfig is the conservative runtime shape. InboxSize and RecentMessages
// follow docs/spec/05 (N=10 confirmed messages) and docs/spec/01 (bounded
// queues); the predictive numbers are placeholders until the labeled holdout
// sets in docs/spec/12 exist, and are documented as such rather than tuned.
func DefaultConfig() Config {
	return Config{
		InboxSize:              512,
		RecentMessages:         10,
		PredictiveMode:         false,
		MinPredictiveMargin:    0.2,
		MaxCandidates:          8,
		AllowCommitWhilePaused: false,
		NarratedKinds:          []games.TextKind{games.TextDialogue},
	}
}

// normalizeNarratedKinds is the operator's class list as the policy stores it:
// deduped, unknown classes dropped (a class the vocabulary does not know is
// not a veto — it is a request the policy cannot read), and dialogue force-
// included in first position, which is the documented "dialogue is the
// product" rule made mechanical. An empty or all-unknown input still yields
// the dialogue default, never an empty set (an empty set would narrate
// nothing, and that is a request no one made).
func normalizeNarratedKinds(kinds []games.TextKind) []games.TextKind {
	out := make([]games.TextKind, 0, len(kinds)+1)
	seen := make(map[games.TextKind]bool, len(kinds)+1)
	out = append(out, games.TextDialogue)
	seen[games.TextDialogue] = true
	for _, k := range kinds {
		if k.Valid() && !seen[k] {
			out = append(out, k)
			seen[k] = true
		}
	}
	return out
}

// normalize clamps c rather than rejecting it: a reducer that refused to start
// because a queue size was 0 would take the runtime down for a value that has an
// obvious intended meaning, and a negative bound here would silently mean
// "unbounded", which is the one thing the inbox may never be.
func (c Config) normalize() Config {
	d := DefaultConfig()
	if c.InboxSize <= 0 {
		c.InboxSize = d.InboxSize
	}
	if c.InboxSize > 1<<16 {
		c.InboxSize = 1 << 16
	}
	if c.RecentMessages <= 0 {
		c.RecentMessages = d.RecentMessages
	}
	if c.RecentMessages > 1024 {
		c.RecentMessages = 1024
	}
	if !(c.MinPredictiveMargin >= 0) { // also catches NaN
		c.MinPredictiveMargin = d.MinPredictiveMargin
	}
	if c.MinPredictiveMargin > 1 {
		c.MinPredictiveMargin = 1
	}
	if c.MaxCandidates <= 0 {
		c.MaxCandidates = d.MaxCandidates
	}
	if c.MaxCandidates > 64 {
		c.MaxCandidates = 64
	}
	// AdjudicateMargin: zero is "unset" (the documented default: the matcher's
	// own acceptance floor), a negative is a wiring mistake that clamps to the
	// same default, and a value above 1 would demand a margin that no
	// probability distribution can reach, which would adjudicate everything —
	// including a 0.99 winner — so it clamps to 1.
	if !(c.AdjudicateMargin >= 0) { // also catches NaN
		c.AdjudicateMargin = 0
	}
	if c.AdjudicateMargin > 1 {
		c.AdjudicateMargin = 1
	}
	c.NarratedKinds = normalizeNarratedKinds(c.NarratedKinds)
	return c
}
