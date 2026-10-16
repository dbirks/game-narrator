// Package events defines the event envelope that every state change travels
// in, the in-process fan-out hub behind it, and the bounded replay log that
// lets an SSE client resume with a Last-Event-ID.
//
// The contract lives in docs/spec/01-system-architecture.md ("High-level
// events", "Parallelism and deadline contracts"), docs/spec/08-api-and-events.md
// ("Event stream reliability") and docs/spec/17-example-contracts.md ("Event
// envelope"). Three properties are load-bearing and are the reason this is a
// package rather than a struct in the API layer:
//
//  1. One global order. `seq` is monotonic across every publisher, so a browser,
//     the developer inspector and the durable `events` table (migration 0003)
//     all agree on what happened next. docs/spec/12 lists "duplicate SSE
//     delivery" and "history gap" as failure modes to test for.
//  2. Backpressure that cannot stall capture. docs/spec/01: "slow browsers
//     cannot stall capture"; docs/spec/08: "drops redundant frame previews
//     first, not state transitions or critical speech events". The hub, not the
//     caller, decides what a slow subscriber loses.
//  3. An explicit gap signal. When a cursor cannot be honoured, the stream says
//     so with `events.resync_required` instead of silently skipping history —
//     a UI that guessed would double-play or lose speech (docs/spec/09 "UX
//     robustness").
package events

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// SchemaVersion is the envelope shape emitted by this build. The field is
// carried per event (docs/spec/17 "Event envelope": `schema_version`) because
// consumers must decode an older log entry after an upgrade rather than guess.
const SchemaVersion = 1

// MaxDataSize bounds Envelope.Data. The events table stores the payload as
// JSON text in SQLite, and the same events cross an SSE connection to a
// browser: an unbounded payload turns one chatty producer into memory pressure
// on every subscriber. Larger payloads (frame crops, audio) travel by reference
// — a hash or an asset URL in Data — per docs/spec/02 "Persist bounded metadata
// and sampled image/audio references".
const MaxDataSize = 64 << 10

// maxIDSize bounds identifier-like fields. The events table has no length
// CHECK, so the bound is enforced here, before a hostile or buggy publisher
// can write a megabyte "session id" into the operational store.
const maxIDSize = 128

// ErrValidation reports an envelope that must not be published or stored. It is
// a distinct sentinel so the API can answer 400 rather than 500.
var ErrValidation = errors.New("events: invalid envelope")

func validationf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrValidation, fmt.Sprintf(format, args...))
}

// Envelope is the common frame around every event (docs/spec/17 "Event
// envelope"). Field order here is the order they are written in JSON, matching
// the documented example.
//
// An Envelope value is treated as immutable once published: the hub fans the
// same Data slice out to every subscriber, and Publish copies it defensively,
// so a publisher that keeps mutating its own buffer is the one bug that would
// show up as two browsers seeing different payloads.
type Envelope struct {
	// SchemaVersion is the version of Data's shape, not of this struct.
	SchemaVersion int `json:"schema_version"`
	// EventID is the public identifier (a ULID, per docs/spec/08 "UUID/ULID
	// identifiers" and the events table comment); unique across restarts.
	EventID string `json:"event_id"`
	// SessionID is empty for global events — job, diagnostics, config and
	// system changes belong to no recognition session (migration 0003:
	// "NULL for global events").
	SessionID string `json:"session_id,omitempty"`
	// Seq is the authoritative global cursor. It is what the SSE `id:` field
	// carries and what Last-Event-ID resumes from.
	Seq int64 `json:"seq"`
	// TraceID correlates one decision or job across events and provider calls
	// (docs/spec/09 "Link everything using trace_id").
	TraceID string `json:"trace_id,omitempty"`
	// CausationID names the event that caused this one, for the developer
	// inspector's causal chain.
	CausationID string `json:"causation_id,omitempty"`
	// Name selects the shape of Data and the drop policy (see Droppable).
	Name string `json:"name"`
	// OccurredAt is the origin's wall clock in UTC. Ordering never depends on
	// it — Seq does — because a second-resolution timestamp cannot order
	// concurrent writers (migration 0003 explains the same problem).
	OccurredAt time.Time `json:"occurred_at"`
	// Data is the event payload: always a JSON object.
	Data json.RawMessage `json:"data"`
}

// timeFormat is RFC 3339 with fixed three-digit fractional seconds in UTC.
// Fixed width (rather than Go's trailing-zero-trimmed RFC3339Nano) keeps
// log lines and the durable `occurred_at` TEXT column the same shape as the
// strftime-guarded columns in migration 0003, and stays valid RFC 3339 for
// generated TypeScript clients.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

// wire is the JSON projection of Envelope. It exists so occurred_at is always
// rendered in UTC with a fixed shape: a publisher in a +02:00 zone would
// otherwise emit "…+02:00", and RFC 3339 strings with different offsets do not
// sort chronologically.
type wire struct {
	SchemaVersion int             `json:"schema_version"`
	EventID       string          `json:"event_id"`
	SessionID     string          `json:"session_id,omitempty"`
	Seq           int64           `json:"seq"`
	TraceID       string          `json:"trace_id,omitempty"`
	CausationID   string          `json:"causation_id,omitempty"`
	Name          string          `json:"name"`
	OccurredAt    string          `json:"occurred_at"`
	Data          json.RawMessage `json:"data"`
}

// MarshalJSON renders the documented field order and a UTC timestamp.
func (e Envelope) MarshalJSON() ([]byte, error) {
	w := wire{
		SchemaVersion: e.SchemaVersion,
		EventID:       e.EventID,
		SessionID:     e.SessionID,
		Seq:           e.Seq,
		TraceID:       e.TraceID,
		CausationID:   e.CausationID,
		Name:          e.Name,
		Data:          e.Data,
	}
	if !e.OccurredAt.IsZero() {
		w.OccurredAt = e.OccurredAt.UTC().Format(timeFormat)
	}
	return json.Marshal(w)
}

// UnmarshalJSON accepts any RFC 3339 offset and normalizes occurred_at to UTC.
func (e *Envelope) UnmarshalJSON(b []byte) error {
	var w wire
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*e = Envelope{
		SchemaVersion: w.SchemaVersion,
		EventID:       w.EventID,
		SessionID:     w.SessionID,
		Seq:           w.Seq,
		TraceID:       w.TraceID,
		CausationID:   w.CausationID,
		Name:          w.Name,
		Data:          w.Data,
	}
	if w.OccurredAt != "" {
		t, err := time.Parse(time.RFC3339Nano, w.OccurredAt)
		if err != nil {
			return fmt.Errorf("events: occurred_at %q is not RFC 3339: %w", w.OccurredAt, err)
		}
		e.OccurredAt = t.UTC()
	}
	return nil
}

// Validate reports whether this is a complete, publishable record: everything
// in validateContent plus the seq that only the hub or the durable log can
// assign. A row read back from SQLite, an envelope returned by Publish, and a
// hand-built record all go through here.
func (e Envelope) Validate() error {
	if e.Seq < 1 {
		return validationf("seq %d must be >= 1", e.Seq)
	}
	return e.validateContent()
}

// validateContent is Validate without the seq, for the moment between a caller
// handing an envelope to the hub and the store numbering it. Publishing order
// matters here: checking the content first means a rejected envelope never
// costs a seq, which would otherwise leave a hole the size of every typo.
func (e Envelope) validateContent() error {
	if err := ValidateName(e.Name); err != nil {
		return err
	}
	if e.SchemaVersion < 1 || e.SchemaVersion > SchemaVersion {
		return validationf("schema_version %d is outside 1..%d", e.SchemaVersion, SchemaVersion)
	}
	if e.EventID == "" {
		return validationf("event_id is required")
	}
	if !games.ValidID(e.EventID) {
		return validationf("event_id %q is not a 26-character ULID", e.EventID)
	}
	if e.OccurredAt.IsZero() {
		return validationf("occurred_at is required")
	}
	for _, f := range []struct{ name, value string }{
		{"session_id", e.SessionID},
		{"trace_id", e.TraceID},
		{"causation_id", e.CausationID},
	} {
		if len(f.value) > maxIDSize {
			return validationf("%s is longer than %d characters", f.name, maxIDSize)
		}
		if !validIDChars(f.value) {
			return validationf("%s %q contains characters outside [A-Za-z0-9._:-]", f.name, trunc(f.value))
		}
	}
	return validateData(e.Data)
}

func validateData(data json.RawMessage) error {
	if len(data) == 0 {
		return validationf("data is required (use {} for an event with no payload)")
	}
	if len(data) > MaxDataSize {
		return validationf("data is %d bytes, over the %d byte limit", len(data), MaxDataSize)
	}
	if !json.Valid(data) {
		return validationf("data is not valid JSON")
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		// A scalar or array payload would decode into every consumer
		// differently and could never carry named fields later; the documented
		// envelope always uses an object.
		return validationf("data must be a JSON object")
	}
	// json.Valid proved the shape; a full decode here would only cost time on the
	// publish path. Duplicate keys and stray non-UTF-8 inside a string are the
	// remaining surprises, and both are the owning consumer's contract to check —
	// docs/spec/06 gives every event name its own schema anyway.
	return nil
}

// ValidateSessionID checks a session id used in a Filter or a query parameter.
// Exported so the HTTP layer can answer 400 with the same rule the hub applies,
// instead of a second copy of the alphabet that could drift.
func ValidateSessionID(s string) error {
	if s == "" {
		return nil
	}
	if len(s) > maxIDSize {
		return validationf("session_id is longer than %d characters", maxIDSize)
	}
	if !validIDChars(s) {
		return validationf("session_id %q contains characters outside [A-Za-z0-9._:-]", trunc(s))
	}
	return nil
}

// validIDChars accepts the conservative identifier alphabet. Control bytes and
// separators would otherwise be echoed into slog lines, the SSE `event:`/`id:`
// fields (where a newline is a frame injection) and the developer inspector's
// HTML.
func validIDChars(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.' || c == '_' || c == '-' || c == ':':
		default:
			return false
		}
	}
	return true
}

func trunc(s string) string {
	if len(s) > 32 {
		return s[:32] + "…"
	}
	return s
}

// The event names from docs/spec/01 "High-level events (single source of
// truth)". That list is the vocabulary; the registry below only adds the names
// docs/spec/08 introduced for the agent chat and lifecycle flows, so a
// publisher cannot invent a name by typo and a consumer can switch on it.
const (
	FrameObserved = "frame.observed"
	RegionChanged = "region.changed"
	TextObserved  = "text.observed"

	MessageCandidates = "message.candidates"
	MessageCommitted  = "message.committed"

	DecisionRequested = "decision.requested"
	DecisionResolved  = "decision.resolved"

	SpeechRequested = "speech.requested"
	SpeechReady     = "speech.ready"
	SpeechStarted   = "speech.started"
	SpeechCancelled = "speech.cancelled"
	SpeechEnded     = "speech.ended"

	SessionSceneChanged  = "session.scene_changed"
	SessionStopped       = "session.stopped"
	ProviderDegraded     = "provider.degraded"
	AgentJobProgress     = "agent.job.progress"
	AgentReviewRequired  = "agent.review_required"
	SystemHealthChanged  = "system.health_changed"
	SystemHeartbeat      = "system.heartbeat"
	ProfilePublished     = "profile.published"
	VoiceAuditionReady   = "voice.audition.ready"
	SourceProposed       = "source.proposed"
	ProfileDiff          = "profile.diff"
	AgentStep            = "agent.step"
	ToolStarted          = "tool.started"
	ToolCompleted        = "tool.completed"
	ReviewRequired       = "review.required"
	EventsResyncRequired = "events.resync_required"
)

// droppableNames are the events a slow subscriber may lose without changing
// what the user hears or the state a reducer would compute. Every one of them
// is either *redundant by nature* — the next frame observation supersedes the
// last, and docs/spec/01 already funnels live capture through a bounded
// single-slot latest-frame queue — or *pure keepalive*, which carries no state
// at all. Everything else is critical: docs/spec/08 "Backpressure policy drops
// redundant frame previews first, not state transitions or critical speech
// events", so message.committed, speech.*, decision.*, job and profile events
// are never shed.

// Droppable reports whether losing this event for one slow subscriber is
// survivable. The classifier is closed over the registered vocabulary: an
// unknown name is critical, because silently shedding a newly added state
// transition is the expensive failure and rejecting the publisher is the cheap
// one.
//
// It is a switch rather than a map lookup on purpose. This runs once per event
// per subscriber, and when a subscriber is stalled the search for the oldest
// droppable event can walk its entire backlog — hashing a string up to 64 times
// per publish is how a slow browser ends up taxing the capture loop, which is
// precisely what docs/spec/01 forbids.
func Droppable(name string) bool {
	switch name {
	case FrameObserved, RegionChanged, SystemHeartbeat:
		return true
	default:
		return false
	}
}

// The default registry, kept as a slice so Names() has a stable order for
// error messages, snapshots and generated docs.
var defaultNames = []string{
	FrameObserved, RegionChanged, TextObserved,
	MessageCandidates, MessageCommitted,
	DecisionRequested, DecisionResolved,
	SpeechRequested, SpeechReady, SpeechStarted, SpeechCancelled, SpeechEnded,
	SessionSceneChanged, SessionStopped, ProviderDegraded,
	AgentJobProgress, AgentReviewRequired,
	SystemHealthChanged, SystemHeartbeat,
	ProfilePublished, VoiceAuditionReady, SourceProposed, ProfileDiff,
	AgentStep, ToolStarted, ToolCompleted, ReviewRequired,
	EventsResyncRequired,
}

var (
	nameMu sync.RWMutex
	names  = map[string]bool{}
)

// Register adds event names to the vocabulary. Later milestones (runtime,
// voice, agent) own names this package cannot anticipate; registering them here
// would serialize feature work onto this file, and an unregistered name would
// simply fail to publish. Names are permanent: a name once published is in the
// durable log and in browsers' switch statements, so there is no Unregister.
func Register(eventNames ...string) error {
	nameMu.Lock()
	defer nameMu.Unlock()
	for _, n := range eventNames {
		if !validEventName(n) {
			return validationf("event name %q is not a dotted lower-case identifier", trunc(n))
		}
	}
	for _, n := range eventNames {
		names[n] = true
	}
	return nil
}

// Names lists the registered event names in registration order.
func Names() []string {
	nameMu.RLock()
	defer nameMu.RUnlock()
	out := make([]string, 0, len(names))
	for _, n := range defaultNames {
		if names[n] {
			out = append(out, n)
		}
	}
	// Any registered name outside the documented default list sorts after it,
	// so the output is stable whether or not extra names exist yet.
	var extra []string
	for n := range names {
		if !slices.Contains(defaultNames, n) {
			extra = append(extra, n)
		}
	}
	slices.Sort(extra)
	return append(out, extra...)
}

// ValidateName rejects a name that no consumer can decode.
func ValidateName(name string) error {
	nameMu.RLock()
	known := names[name]
	nameMu.RUnlock()
	if !known {
		return validationf("unknown event name %q", trunc(name))
	}
	return nil
}

// validEventName is the shape both sides of the wire use: lower-case words
// joined by single dots, optionally one underscore inside a final segment
// (session.scene_changed, agent.job.progress in docs/spec/01).
func validEventName(s string) bool {
	if s == "" || len(s) > maxIDSize {
		return false
	}
	segments := strings.Split(s, ".")
	if len(segments) < 2 {
		// At least a category and an action: every documented name has both,
		// and the developer UI groups by the first segment.
		return false
	}
	for i, seg := range segments {
		if seg == "" {
			return false
		}
		// Underscore is allowed in the final segment only (session.scene_changed,
		// events.resync_required). Anywhere else would let `job.progress` and
		// `job_progress` mean one event, and generated clients cannot tell them
		// apart from a case-analysis typo.
		allowUnderscore := i == len(segments)-1
		for j := 0; j < len(seg); j++ {
			c := seg[j]
			switch {
			case c >= 'a' && c <= 'z':
			case c == '_' && allowUnderscore:
			default:
				return false
			}
		}
	}
	return true
}

// init builds the registry. Splitting it from a package-level composite
// literal means Register can be called before or after any use of the
// registry, and the first use always sees the documented defaults.
func init() {
	for _, n := range defaultNames {
		names[n] = true
	}
}
