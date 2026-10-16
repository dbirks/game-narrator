package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// ContentTypeEventStream is the SSE media type (WHATWG HTML, "Server-sent
// events").
const ContentTypeEventStream = "text/event-stream"

// HeaderLastEventID is the resume cursor an EventSource client sends back after
// a reconnect. It is not an idempotency key and never a client-chosen value: it
// is the `id:` we wrote, i.e. a global seq.
const HeaderLastEventID = "Last-Event-ID"

// queryLastEventID is the fallback for clients that cannot set headers — the
// EventSource polyfills and, more practically, a curl-driven smoke test or a
// Playwright route assertion that wants to name the cursor explicitly.
const queryLastEventID = "last_event_id"

// eventsPath is documented in docs/spec/08 "APIs".
const eventsPath = "/api/v1/events"

// handleEvents serves GET /api/v1/events: the multiplexed SSE stream
// (docs/spec/08 "Event stream reliability", docs/spec/01 "SSE fanout ring
// buffer is bounded … reconnect via Last-Event-ID or persisted cursor").
//
// Three behaviours are contractual rather than incidental:
//
//   - Resume. Every frame carries `id: <seq>`, so a reconnect with Last-Event-ID
//     replays what the client missed from the hub's bounded log, and where the
//     log cannot cover the cursor the first frame is events.resync_required
//     instead of a silent hole (docs/spec/12 "UI/operations": "SSE reconnect and
//     Last-Event-ID; history gap").
//   - Isolation. The subscription is pulled, not pushed, and closing it on the
//     way out touches nothing but that subscriber's queue. A disconnecting
//     browser must never affect publishers or capture (docs/spec/01), and never
//     stops playback (docs/spec/08).
//   - Liveness. A heartbeat comment on the hub's interval keeps proxies from
//     closing an idle stream, and the comment is a comment: it consumes no
//     cursor and creates no event.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		s.writeProblem(w, r, http.StatusMethodNotAllowed, "method-not-allowed",
			"Method Not Allowed",
			fmt.Sprintf("method %s is not allowed on %s; the event stream is GET only",
				r.Method, eventsPath))
		return
	}
	if s.hub == nil {
		// Honest 503: no event log is wired, so there is nothing to stream and
		// nothing to replay. A UI that saw 200 here would believe it was
		// watching a session it was not.
		s.writeProblem(w, r, http.StatusServiceUnavailable, "events-unavailable",
			"Event Stream Unavailable",
			"no event hub is configured in this process; start a session or check the serve wiring")
		return
	}

	q := r.URL.Query()
	filter, err := parseEventFilter(q)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-event-filter",
			"Bad Request", err.Error())
		return
	}
	cursor, err := eventCursor(r)
	if err != nil {
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-last-event-id",
			"Bad Request", err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		// No streaming on this connection (an httptest.ResponseRecorder without
		// a flusher, or an exotic wrapper). Answering 200 and buffering the whole
		// stream would look like a working SSE endpoint and never deliver.
		s.writeProblem(w, r, http.StatusServiceUnavailable, "streaming-unsupported",
			"Streaming Unsupported",
			"the response writer does not support flushing; the event stream requires it")
		return
	}

	ctx := r.Context()
	sub, err := s.hub.Subscribe(ctx, filter, cursor)
	if err != nil {
		s.streamFailure(w, r, "subscribe", err)
		return
	}
	// Disconnecting is only this: the hub drops the queue, publishes keep
	// flowing to everyone else, and nothing here outlives the request.
	defer sub.Close()

	h := w.Header()
	h.Set("Content-Type", ContentTypeEventStream)
	// no-store, not no-cache: a replayed event stream from a browser cache would
	// duplicate speech on remount (docs/spec/09 "UX robustness").
	h.Set("Cache-Control", "no-store")
	h.Set("Pragma", "no-cache")
	// Tells nginx-family proxies not to buffer, or the stream stalls until the
	// proxy's buffer fills (docs/spec/10 deployment topology).
	h.Set("X-Accel-Buffering", "no")
	// No Connection header: net/http owns connection management, and an SSE
	// response that asked for keep-alive would be silently rewritten anyway.
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	beat := s.hub.Heartbeat()
	s.log.Debug("sse stream opened",
		"request_id", RequestIDFromContext(ctx),
		"subscriber_id", sub.ID(),
		"after_seq", cursor,
		"session_id", filter.SessionID,
		"heartbeat_ms", beat.Milliseconds(),
	)

	if err := s.streamEvents(ctx, w, flusher, sub, beat); err != nil && !isStreamEnd(ctx, err) {
		s.log.Warn("sse stream ended with an error",
			"request_id", RequestIDFromContext(ctx),
			"subscriber_id", sub.ID(),
			"error", err,
		)
	}
}

// streamEvents pumps one subscription to the wire. It runs on the request
// goroutine: no goroutine is spawned per stream, so a client that vanishes
// leaves nothing behind to settle.
func (s *Server) streamEvents(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, sub *events.Subscription, beat time.Duration) error {
	for {
		e, ok, err := sub.Next(ctx, beat)
		switch {
		case errors.Is(err, events.ErrNoMoreEvents):
			// Drained and closed — hub shutdown, or this subscriber was retired
			// as lagged after its final resync notice. Clean end.
			return nil
		case err != nil:
			// ctx cancellation is the ordinary client-disconnect path.
			return err
		case !ok:
			// Idle: heartbeat comment, which the client's EventSource ignores.
			if _, err := fmt.Fprintf(w, ": hb %d\n\n", time.Now().Unix()); err != nil {
				return err
			}
			flusher.Flush()
			continue
		}

		frame, err := encodeSSEFrame(e)
		if err != nil {
			// A frame we cannot write is a gap this client must be told about,
			// not one it has to discover by noticing a missing seq.
			s.log.Error("cannot encode event for SSE stream",
				"event_id", e.EventID, "name", e.Name, "seq", e.Seq, "error", err)
			notice := events.GapNotice(sub.Filter().SessionID, "encode_failed", e.Seq, e.Seq)
			nb, nerr := encodeSSEFrame(notice)
			if nerr != nil {
				return nerr
			}
			if _, werr := w.Write(nb); werr != nil {
				return werr
			}
			flusher.Flush()
			continue
		}
		if _, err := w.Write(frame); err != nil {
			// Writing to a gone client is the normal end of an SSE stream.
			return err
		}
		flusher.Flush()
	}
}

// encodeSSEFrame renders one event in the SSE wire format:
//
//	id: <seq>
//	event: <name>
//	data: <envelope json>
//
// The data field must be a single line, so the envelope is compacted: JSON
// permits whitespace between tokens, and a publisher that handed us pretty-printed
// data would otherwise split one event across frames and the client would
// silently see two truncated ones.
func encodeSSEFrame(e events.Envelope) ([]byte, error) {
	raw, err := json.Marshal(e)
	if err != nil {
		return nil, fmt.Errorf("encode envelope %s: %w", e.EventID, err)
	}
	var buf bytes.Buffer
	buf.Grow(len(raw) + 24 + len(e.Name))
	// The cursor is the global seq, not the event_id: it is the only value the
	// hub can replay from (docs/spec/08 "Emit id: <session-or-global-cursor>").
	fmt.Fprintf(&buf, "id: %d\nevent: %s\ndata: ", e.Seq, e.Name)
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("compact envelope %s: %w", e.EventID, err)
	}
	buf.WriteString("\n\n")
	return buf.Bytes(), nil
}

// isStreamEnd reports the errors that mean "the client or the hub ended the
// stream", as opposed to ones worth a warning. A disconnected client is the
// ordinary way an SSE handler returns, so it must not read as a failure in the
// log (docs/spec/13: distinguish a degraded path from an error).
func isStreamEnd(ctx context.Context, err error) bool {
	return errors.Is(err, context.Canceled) ||
		errors.Is(err, events.ErrNoMoreEvents) ||
		errors.Is(err, http.ErrAbortHandler) ||
		errors.Is(err, net.ErrClosed) ||
		ctx.Err() != nil
}

// streamFailure maps a subscribe/append failure onto the error contract: a
// rejected filter is the caller's fault (400), a closed hub or a failing
// durable log is ours (503), and both are problem+json with a request id.
func (s *Server) streamFailure(w http.ResponseWriter, r *http.Request, op string, err error) {
	switch {
	case errors.Is(err, events.ErrValidation):
		s.writeProblem(w, r, http.StatusBadRequest, "invalid-event-request",
			"Bad Request", fmt.Sprintf("cannot subscribe to the event stream: %v", err))
	case errors.Is(err, events.ErrClosed):
		s.writeProblem(w, r, http.StatusServiceUnavailable, "events-unavailable",
			"Event Stream Unavailable", "the event hub is shutting down")
	default:
		s.log.Warn("event stream subscribe failed",
			"request_id", RequestIDFromContext(r.Context()), "op", op, "error", err)
		s.writeProblem(w, r, http.StatusServiceUnavailable, "event-store-unavailable",
			"Event Stream Unavailable",
			fmt.Sprintf("cannot replay the event log right now: %v", err))
	}
}

// parseEventFilter reads session_id and names from the query string. An
// unregistered name is a 400 rather than a stream that quietly yields nothing:
// a typo in a UI filter must fail visibly at connect time, not as an empty
// session view.
func parseEventFilter(q map[string][]string) (events.Filter, error) {
	var f events.Filter
	vals := q["session_id"]
	if len(vals) > 1 {
		return f, fmt.Errorf("session_id may be given once, got %d values", len(vals))
	}
	if len(vals) == 1 {
		f.SessionID = vals[0]
	}
	// The same rule the hub applies to a Filter, so the rejection is a 400 with
	// our detail instead of a wrapped internal error.
	if err := events.ValidateSessionID(f.SessionID); err != nil {
		return events.Filter{}, err
	}

	var names []string
	for _, raw := range q["names"] {
		for _, part := range strings.Split(raw, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if err := events.ValidateName(part); err != nil {
				return events.Filter{}, fmt.Errorf("names: %w", err)
			}
			names = append(names, part)
		}
	}
	f.Names = names
	return f, nil
}

// eventCursor resolves the resume position: the Last-Event-ID header first
// (what an EventSource sends), then the query-parameter form. A malformed
// cursor is a 400 — replaying from a cursor the server cannot interpret is
// exactly the "silent history gap" docs/spec/12 tests for, so it is refused
// rather than guessed at as 0 (which would replay the whole log).
func eventCursor(r *http.Request) (int64, error) {
	raw := strings.TrimSpace(r.Header.Get(HeaderLastEventID))
	source := HeaderLastEventID
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get(queryLastEventID))
		source = queryLastEventID
	}
	if raw == "" {
		return 0, nil
	}
	// A client that reconnects without a usable id sends nothing; anything else
	// must be an exact non-negative integer. No +/- signs, no spaces, no hex:
	// the SSE id field is free-form text, so leniency here would silently turn a
	// client bug into a wrong replay position.
	if len(raw) > 19 {
		return 0, fmt.Errorf("%s %q is not a valid event cursor", source, raw)
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, fmt.Errorf("%s %q is not a numeric event cursor", source, raw)
		}
	}
	seq, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a valid event cursor: %w", source, raw, err)
	}
	return seq, nil
}
