package events

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

// validEnvelope returns an envelope that passes Validate. Tests mutate one
// field and expect a specific rejection, so the baseline has to be valid
// rather than assembled from defaults.
func validEnvelope(t *testing.T) Envelope {
	t.Helper()
	return Envelope{
		SchemaVersion: SchemaVersion,
		EventID:       games.NewID(),
		SessionID:     "session-101",
		TraceID:       "trace-a9",
		CausationID:   games.NewID(),
		Name:          DecisionResolved,
		Seq:           1,
		OccurredAt:    time.Date(2026, time.October, 16, 12, 30, 10, 0, time.UTC),
		Data:          json.RawMessage(`{"observation_id":"obs-8","accepted":true}`),
	}
}

// The event vocabulary is the single source of truth in docs/spec/01 "High-level
// events (single source of truth)"; a name missing here is an event a later
// milestone cannot publish.
func TestRegisteredNamesCoverTheSpecVocabulary(t *testing.T) {
	want := []string{
		"frame.observed", "region.changed", "text.observed", "message.candidates",
		"decision.requested", "decision.resolved", "message.committed",
		"speech.requested", "speech.ready", "speech.started", "speech.cancelled",
		"speech.ended", "session.scene_changed", "provider.degraded",
		"agent.job.progress", "agent.review_required", "system.health_changed",
		// docs/spec/08 additions named in this milestone.
		"profile.published", "agent.step", "tool.started", "tool.completed",
		"review.required", "events.resync_required", "system.heartbeat",
	}
	have := map[string]bool{}
	for _, n := range Names() {
		have[n] = true
	}
	for _, n := range want {
		if !have[n] {
			t.Errorf("event name %q is not registered (Names = %v)", n, Names())
		}
	}
}

func TestValidateAcceptsEveryRegisteredName(t *testing.T) {
	for _, name := range Names() {
		e := validEnvelope(t)
		e.Name = name
		if err := e.Validate(); err != nil {
			t.Errorf("Validate() on %q: %v", name, err)
		}
	}
}

func TestValidateRejects(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(e *Envelope)
	}{
		{"unknown name", func(e *Envelope) { e.Name = "decision.decided" }},
		{"empty name", func(e *Envelope) { e.Name = "" }},
		{"name without a category", func(e *Envelope) { e.Name = "heartbeat" }},
		{"uppercase name", func(e *Envelope) { e.Name = "Frame.Observed" }},
		{"name with a newline", func(e *Envelope) { e.Name = "speech.started\nid: 9" }},
		{"schema version zero", func(e *Envelope) { e.SchemaVersion = 0 }},
		{"schema version from the future", func(e *Envelope) { e.SchemaVersion = SchemaVersion + 1 }},
		{"missing event id", func(e *Envelope) { e.EventID = "" }},
		{"event id that is not a ULID", func(e *Envelope) { e.EventID = "evt-1003" }},
		{"missing occurred_at", func(e *Envelope) { e.OccurredAt = time.Time{} }},
		{"seq zero", func(e *Envelope) { e.Seq = 0 }},
		{"negative seq", func(e *Envelope) { e.Seq = -1 }},
		{"no data", func(e *Envelope) { e.Data = nil }},
		{"empty data", func(e *Envelope) { e.Data = json.RawMessage("") }},
		{"data is an array", func(e *Envelope) { e.Data = json.RawMessage(`[1,2]`) }},
		{"data is a scalar", func(e *Envelope) { e.Data = json.RawMessage(`"hi"`) }},
		{"data is invalid json", func(e *Envelope) { e.Data = json.RawMessage(`{"a":}`) }},
		{"data is truncated json", func(e *Envelope) { e.Data = json.RawMessage(`{"a":1`) }},
		{"data over the size limit", func(e *Envelope) {
			e.Data = json.RawMessage(fmt.Sprintf(`{"blob":"%s"}`, strings.Repeat("x", MaxDataSize)))
		}},
		{"session id with a separator", func(e *Envelope) { e.SessionID = "session 101" }},
		{"session id with a control byte", func(e *Envelope) { e.SessionID = "s\x001" }},
		{"oversized trace id", func(e *Envelope) { e.TraceID = strings.Repeat("t", maxIDSize+1) }},
		{"causation id with markup", func(e *Envelope) { e.CausationID = "<script>" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validEnvelope(t)
			tc.mutate(&e)
			err := e.Validate()
			if err == nil {
				t.Fatalf("Validate() accepted %s: %v", tc.name, e)
			}
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("Validate() error = %v, want it to wrap ErrValidation", err)
			}
		})
	}
}

func TestValidateAcceptsGlobalEventWithoutSession(t *testing.T) {
	e := validEnvelope(t)
	e.SessionID = ""
	e.CausationID = ""
	e.TraceID = ""
	if err := e.Validate(); err != nil {
		t.Fatalf("a global event (no session) must validate: %v", err)
	}
}

func TestValidateAcceptsDataAtTheSizeLimit(t *testing.T) {
	e := validEnvelope(t)
	// Exactly MaxDataSize bytes, which the boundary of "<= 64 KiB" allows.
	pad := MaxDataSize - len(`{"blob":""}`)
	e.Data = json.RawMessage(fmt.Sprintf(`{"blob":"%s"}`, strings.Repeat("x", pad)))
	if len(e.Data) != MaxDataSize {
		t.Fatalf("test setup: data is %d bytes, want exactly %d", len(e.Data), MaxDataSize)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("Validate() rejected data at the limit: %v", err)
	}
}

// docs/spec/17 "Event envelope" fixes the JSON field names, and docs/spec/08
// fixes RFC3339 UTC timestamps. A renamed field is a broken TypeScript client
// and a changed timestamp shape is a log line that no longer sorts.
func TestEnvelopeJSONMatchesTheDocumentedContract(t *testing.T) {
	e := validEnvelope(t)
	e.OccurredAt = time.Date(2026, time.October, 16, 14, 30, 10, 123000000,
		time.FixedZone("CEST", 2*60*60))

	raw, err := json.Marshal(e)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	body := string(raw)

	wantFields := []string{
		`"schema_version":1`,
		`"event_id":"` + e.EventID + `"`,
		`"session_id":"session-101"`,
		`"seq":1`,
		`"trace_id":"trace-a9"`,
		`"causation_id":"` + e.CausationID + `"`,
		`"occurred_at":"2026-10-16T12:30:10.123Z"`, // converted from +02:00
		`"name":"decision.resolved"`,
		`"data":{"observation_id":"obs-8","accepted":true}`,
	}
	for _, w := range wantFields {
		if !strings.Contains(body, w) {
			t.Errorf("marshalled envelope is missing %s\n got %s", w, body)
		}
	}
	// Field order is the order docs/spec/17 lists them in, so the stream and a
	// captured fixture read the same.
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc) != len(wantFields) {
		t.Fatalf("envelope has %d fields, want %d: %s", len(doc), len(wantFields), body)
	}

	var back Envelope
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("round-trip unmarshal: %v", err)
	}
	if !back.OccurredAt.Equal(e.OccurredAt) {
		t.Fatalf("occurred_at round-trip = %v, want %v", back.OccurredAt, e.OccurredAt)
	}
	if back.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurred_at decoded in %v, want UTC", back.OccurredAt.Location())
	}
	back.OccurredAt = e.OccurredAt.UTC()
	if back.EventID != e.EventID || back.Name != e.Name || back.Seq != e.Seq ||
		back.SchemaVersion != e.SchemaVersion || string(back.Data) != string(e.Data) {
		t.Fatalf("round-trip lost fields: %+v vs %+v", back, e)
	}
}

func TestEnvelopeJSONRejectsMalformedTimestamp(t *testing.T) {
	var e Envelope
	err := json.Unmarshal([]byte(`{"schema_version":1,"event_id":"x","name":"speech.started","occurred_at":"16/10/2026"}`), &e)
	if err == nil {
		t.Fatal("unmarshal accepted a non-RFC3339 occurred_at")
	}
}

func TestDroppableClassification(t *testing.T) {
	droppable := []string{FrameObserved, RegionChanged, SystemHeartbeat}
	for _, n := range droppable {
		if !Droppable(n) {
			t.Errorf("Droppable(%q) = false, want true (docs/spec/08: redundant frame previews first)", n)
		}
	}
	// Everything that changes state or leads to speech is critical: docs/spec/08
	// "not state transitions or critical speech events".
	critical := []string{
		TextObserved, MessageCandidates, MessageCommitted,
		DecisionRequested, DecisionResolved,
		SpeechRequested, SpeechReady, SpeechStarted, SpeechCancelled, SpeechEnded,
		SessionSceneChanged, SessionStopped, ProviderDegraded,
		AgentJobProgress, AgentReviewRequired, SystemHealthChanged,
		ProfilePublished, AgentStep, ToolStarted, ToolCompleted, ReviewRequired,
		EventsResyncRequired,
	}
	for _, n := range critical {
		if Droppable(n) {
			t.Errorf("Droppable(%q) = true, want false", n)
		}
	}
	// An unregistered name is critical: shedding a state transition nobody has
	// classified yet is the expensive failure.
	if Droppable("brand.new_event") {
		t.Error("an unknown event name must default to critical")
	}
}

func TestRegisterAddsNamesAndRejectsMalformed(t *testing.T) {
	if err := Register("speech.experiment"); err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() {
		nameMu.Lock()
		delete(names, "speech.experiment")
		nameMu.Unlock()
	})
	if err := ValidateName("speech.experiment"); err != nil {
		t.Fatalf("ValidateName after Register: %v", err)
	}
	e := validEnvelope(t)
	e.Name = "speech.experiment"
	if err := e.Validate(); err != nil {
		t.Fatalf("a registered name must validate: %v", err)
	}

	for _, bad := range []string{"Nope", "speech.", ".speech", "speech", "speech.Experiment", ""} {
		if err := Register(bad); err == nil {
			t.Errorf("Register(%q) accepted a malformed name", bad)
		} else if !errors.Is(err, ErrValidation) {
			t.Errorf("Register(%q) error = %v, want ErrValidation", bad, err)
		}
	}
}

func TestGapNoticeIsValidEnvelope(t *testing.T) {
	e := GapNotice("session-101", "gap", 12, 40)
	// The notice carries the seq the client resumes from, so its own cursor
	// lands where delivery continues (see the function's doc).
	if e.Seq != 40 {
		t.Fatalf("notice seq = %d, want 40", e.Seq)
	}
	if e.Name != EventsResyncRequired {
		t.Fatalf("notice name = %q", e.Name)
	}
	if err := e.Validate(); err != nil {
		t.Fatalf("a resync notice the client must parse is invalid: %v", err)
	}
	var data struct {
		Reason     string `json:"reason"`
		FromSeq    int64  `json:"from_seq"`
		ThroughSeq int64  `json:"through_seq"`
		Cursor     int64  `json:"cursor"`
		Snapshot   string `json:"snapshot"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("notice data: %v", err)
	}
	if data.Reason != "gap" || data.FromSeq != 12 || data.ThroughSeq != 40 || data.Cursor != 40 {
		t.Fatalf("notice payload = %+v", data)
	}
	if !strings.HasPrefix(data.Snapshot, "/api/") {
		t.Fatalf("notice must name the snapshot endpoint to rebuild from, got %q", data.Snapshot)
	}
}
