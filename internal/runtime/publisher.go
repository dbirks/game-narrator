package runtime

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// Settle bounds for the bounded waits Stop and Close perform: the run they
// await drains bounded worker pools and closes one source, so a healthy run
// settles in well under a second; the bounds are the ceiling for a wedged one,
// after which the manager logs and moves on rather than hanging shutdown
// (docs/spec/01 "Deadline/cancellation propagation").
const (
	stopSettleTimeout    = 30 * time.Second
	closeSettleTimeout   = 60 * time.Second
	errorPayloadMaxRunes = 300
)

// tracePublisher sits between the pipeline's session reducer and the event
// hub. It does two things, both documented as dev-trace-only behaviour
// (docs/spec/13 "Logging/privacy"):
//
//  1. It attaches the stored frame/crop evidence to text.observed — the
//     frame_asset and crop_asset sha256 references — so the developer
//     inspector can open exactly the pixels one OCR read was made on.
//     With no recorder (dev_trace off) this path stores and references no
//     images at all.
//  2. It suppresses the reducer's own session.stopped: the Manager publishes
//     exactly one terminal event, with the authoritative reason, and a
//     second would make "did it end and why" ambiguous in the event log.
type tracePublisher struct {
	hub       *events.Hub
	rec       *devTraceRecorder // nil when dev_trace is off
	sessionID string
	// lastRead is the most recent read this session resolved evidence for —
	// the text.observed whose frame_asset / crop_asset the inspector opens —
	// and so the read the next message.committed answers (issue #46).
	lastRead *devFrameSample
}

// Publish implements session.Publisher.
func (p *tracePublisher) Publish(ctx context.Context, e events.Envelope) (events.Envelope, error) {
	if e.Name == events.SessionStopped {
		// The reducer emits this on a control stop the pipeline never sends;
		// the Manager's terminal event is the one the log keeps.
		return e, nil
	}
	if p.rec != nil {
		switch e.Name {
		case events.TextObserved:
			e.Data = p.attach(e.Data)
		case events.MessageCommitted:
			// Issue #46: the read that just committed is the evidence the
			// operator is asking about — the frame and the crops the OCR read
			// was made on are "why did OCR read X", and the rolling idle
			// eviction must not take them while the line is still the one the
			// session is discussing. The recorder pins the committing read and
			// the last reads before it, oldest pinned first, under the session's
			// pinned budget; without a recorder (dev_trace off) there is nothing
			// to pin.
			p.pinCommittedRead(ctx)
		}
	}
	return p.hub.Publish(ctx, e)
}

// pinCommittedRead pins the evidence behind the read a message.committed
// answered: the read just resolved (the committing read) and the last reads
// before it. A pin lands on the evidence, not on the policy — a recorder whose
// store has already evicted the read the commit answered pins nothing and lets
// the answer fall through, because there is no evidence left to keep.
func (p *tracePublisher) pinCommittedRead(ctx context.Context) {
	if p.rec == nil || p.lastRead == nil {
		return
	}
	_ = p.rec.PinSample(ctx, p.lastRead)
}

// attach resolves the observation's capture timestamp to the nearest stored
// sample and adds the evidence references. It degrades to the unmodified
// payload on any miss: an event must not fail to publish because its debug
// evidence is unavailable.
func (p *tracePublisher) attach(data json.RawMessage) json.RawMessage {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return data
	}
	ts, _ := number(m["capture_timestamp_ns"])
	regionID, _ := m["region_id"].(string)
	s, ok := p.rec.lookup(int64(ts))
	if !ok {
		return data
	}
	p.lastRead = s
	return attachDevTraceData(data, s, regionID)
}

// mustJSON marshals a payload that is known to be a JSON object; on the
// impossible error it still returns a valid object so an event publish can
// never be refused for a malformed body of the runtime's own making.
func mustJSON(v any) json.RawMessage {
	out, err := json.Marshal(v)
	if err != nil {
		out, _ = json.Marshal(map[string]string{"error": "unmarshalable runtime payload"})
	}
	return out
}

// truncateError bounds the session.failed payload: the full error stays in
// the box log, the event carries the class and the head of the message.
func truncateError(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if len(s) > errorPayloadMaxRunes {
		s = s[:errorPayloadMaxRunes] + "…"
	}
	return s
}

// number reads a JSON number out of a decoded payload as float64.
func number(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// devTraceDir is one session's private dev-trace namespace under the
// runtime's assets root: <root>/<session-id>. The per-session directory is
// what makes "the 64 MiB quota is per session" a directory-sized truth and
// what lets retention (or a restart) drop one session's evidence without
// touching another's.
func devTraceDir(root, sessionID string) string {
	return filepath.Join(root, sessionID)
}
