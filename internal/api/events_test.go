package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// sseFrame is one parsed SSE record: either an event (id/event/data fields) or
// a comment block (the heartbeat).
type sseFrame struct {
	ID      string
	Name    string
	Data    string
	Comment string
}

// sseStream reads the wire format as the SSE rules define it — records
// separated by a blank line, "field: value" lines, ":" introducing a comment —
// from a real connection, so the assertions are about bytes on the wire and not
// about a handler's internal state.
type sseStream struct {
	t      *testing.T
	resp   *http.Response
	cancel context.CancelFunc
	r      *bufio.Reader
}

// openEventStream issues GET /api/v1/events against a live server.
//
// Cleanup ordering matters: httptest.Server.Close waits for outstanding
// requests, and an SSE handler is outstanding until its client goes away, so
// every caller must let the cancel registered here run before the server's own
// Close.
func openEventStream(t *testing.T, srv *httptest.Server, query string, headers map[string]string) *sseStream {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+eventsPath+query, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET %s: %v", eventsPath+query, err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		cancel()
		t.Fatalf("status = %d, want 200 (body %s)", resp.StatusCode, body)
	}
	return &sseStream{t: t, resp: resp, cancel: cancel, r: bufio.NewReader(resp.Body)}
}

// next returns the next frame. The read runs in a goroutine because a stalled
// server must fail the test rather than hang the suite; that goroutine is
// unblocked when the request context is cancelled. (The production handler
// spawns nothing — the leak test below proves it for the code under test.)
func (s *sseStream) next(deadline time.Duration) (sseFrame, error) {
	s.t.Helper()
	type result struct {
		f   sseFrame
		err error
	}
	done := make(chan result, 1)
	go func() {
		var f sseFrame
		var comments []string
		var data strings.Builder
		for {
			line, err := s.r.ReadString('\n')
			if err != nil {
				done <- result{err: err}
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if line == "" {
				switch {
				case data.Len() > 0 || f.ID != "" || f.Name != "":
					f.Comment = strings.Join(comments, " ")
					f.Data = data.String()
					done <- result{f: f}
					return
				case len(comments) > 0:
					done <- result{f: sseFrame{Comment: strings.Join(comments, " ")}}
					return
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, ":"):
				comments = append(comments, strings.TrimSpace(line[1:]))
			case strings.HasPrefix(line, "id:"):
				f.ID = strings.TrimSpace(line[3:])
			case strings.HasPrefix(line, "event:"):
				f.Name = strings.TrimSpace(line[6:])
			case strings.HasPrefix(line, "data:"):
				if data.Len() > 0 {
					data.WriteString("\n") // SSE joins multi-line data with \n
				}
				data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			default:
				done <- result{err: fmt.Errorf("unparsable SSE line %q", line)}
				return
			}
		}
	}()

	select {
	case res := <-done:
		return res.f, res.err
	case <-time.After(deadline):
		return sseFrame{}, fmt.Errorf("no frame within %v", deadline)
	}
}

// envelope decodes the frame's data field.
func (s sseFrame) envelope(t *testing.T) events.Envelope {
	t.Helper()
	var e events.Envelope
	if err := json.Unmarshal([]byte(s.Data), &e); err != nil {
		t.Fatalf("data is not an event envelope: %v (data %q)", err, s.Data)
	}
	return e
}

// cursor returns the frame's seq as the client would use it for Last-Event-ID.
func (s sseFrame) cursor(t *testing.T) int64 {
	t.Helper()
	if s.ID == "" {
		t.Fatalf("frame carries no id: %+v", s)
	}
	var seq int64
	if _, err := fmt.Sscanf(s.ID, "%d", &seq); err != nil {
		t.Fatalf("id %q is not a numeric cursor", s.ID)
	}
	if e := s.envelope(t); e.Seq != seq {
		t.Fatalf("id %q does not match the envelope seq %d", s.ID, e.Seq)
	}
	return seq
}

// stop disconnects this client the way a closed tab does.
func (s *sseStream) stop() {
	s.resp.Body.Close()
	s.cancel()
}

// newEventServer returns a live server plus the hub behind it. The heartbeat is
// shortened so a test can observe one; everything else is production default.
func newEventServer(t *testing.T, opts ...events.HubOption) (*httptest.Server, *events.Hub) {
	t.Helper()
	opts = append([]events.HubOption{events.WithHeartbeat(50 * time.Millisecond)}, opts...)
	hub := events.New(opts...)
	srv := httptest.NewServer(newHandler(t, WithEvents(hub)))
	t.Cleanup(srv.Close)
	return srv, hub
}

func publishTestEvent(t *testing.T, h *events.Hub, name, sessionID string) events.Envelope {
	t.Helper()
	e, err := h.Publish(context.Background(), events.Envelope{
		Name:      name,
		SessionID: sessionID,
		TraceID:   "trace-a9",
		Data:      json.RawMessage(`{"text":"hello"}`),
	})
	if err != nil {
		t.Fatalf("Publish(%s): %v", name, err)
	}
	return e
}

func TestEventsWithoutHubIs503(t *testing.T) {
	rec := do(t, newHandler(t), http.MethodGet, eventsPath)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body %s)", rec.Code, rec.Body.String())
	}
	requireContentType(t, rec, ContentTypeProblem)
	body := decodeJSON(t, rec)
	if body["status"] != float64(http.StatusServiceUnavailable) {
		t.Fatalf("problem status = %v, want 503", body["status"])
	}
	if id, ok := body["request_id"].(string); !ok || id == "" {
		t.Fatalf("problem has no request_id: %v", body)
	}
	if !strings.Contains(fmt.Sprint(body["type"]), "events-unavailable") {
		t.Fatalf("problem type = %v, want urn:game-narrator:error:events-unavailable", body["type"])
	}
}

// A non-GET must not start a stream: the 405 is the same problem+json shape the
// rest of the API answers, with an Allow header.
// A non-GET must not start a stream. POST and DELETE are refused by the router's
// fallback (the same problem+json shape every other route uses), and HEAD —
// which ServeMux routes to a GET pattern — is refused by the handler, because a
// stream has no HEAD semantics and answering HEAD here would open a live
// response with no body.
func TestEventsMethodNotAllowed(t *testing.T) {
	h := newHandler(t, WithEvents(events.New()))
	for _, tc := range []struct {
		method string
		allow  string
	}{
		{http.MethodPost, "GET, HEAD"},
		{http.MethodDelete, "GET, HEAD"},
		{http.MethodHead, "GET"},
	} {
		rec := do(t, h, tc.method, eventsPath)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s: status = %d, want 405 (body %s)", tc.method, rec.Code, rec.Body.String())
		}
		if got := rec.Header().Get("Allow"); got != tc.allow {
			t.Fatalf("%s: Allow = %q, want %q", tc.method, got, tc.allow)
		}
		requireContentType(t, rec, ContentTypeProblem)
		if got := rec.Header().Get("Content-Type"); strings.Contains(got, ContentTypeEventStream) {
			t.Fatalf("%s: answered with an event stream content type", tc.method)
		}
	}
}

// docs/spec/08 "Event stream reliability": JSON envelope, `id:` cursor, no proxy
// buffering, no cache.
func TestEventsWireFormatAndHeaders(t *testing.T) {
	srv, hub := newEventServer(t)
	published := publishTestEvent(t, hub, events.MessageCommitted, "session-101")

	stream := openEventStream(t, srv, "?session_id=session-101", nil)
	defer stream.stop()

	h := stream.resp.Header
	if got := h.Get("Content-Type"); got != ContentTypeEventStream {
		t.Fatalf("Content-Type = %q, want %q", got, ContentTypeEventStream)
	}
	if got := h.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := h.Get("X-Accel-Buffering"); got != "no" {
		t.Fatalf("X-Accel-Buffering = %q, want no", got)
	}

	f, err := stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if f.ID != fmt.Sprint(published.Seq) {
		t.Fatalf("id = %q, want the seq %d", f.ID, published.Seq)
	}
	if f.Name != events.MessageCommitted {
		t.Fatalf("event = %q, want %q", f.Name, events.MessageCommitted)
	}
	if strings.Contains(f.Data, "\n") {
		t.Fatalf("data must be exactly one line, got %q", f.Data)
	}
	e := f.envelope(t)
	if e.Seq != published.Seq || e.EventID != published.EventID || e.Name != published.Name {
		t.Fatalf("envelope = %+v, want the published %+v", e, published)
	}
	if e.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurred_at decoded in %v, want UTC", e.OccurredAt.Location())
	}
	if got := string(e.Data); got != `{"text":"hello"}` {
		t.Fatalf("payload = %s, want the publisher's object", got)
	}
	if e.TraceID != "trace-a9" {
		t.Fatalf("trace_id = %q, want the publisher's correlation id", e.TraceID)
	}
}

// The frame writer, exercised on the input that breaks a naive implementation:
// JSON allows whitespace between tokens, an SSE record does not allow a newline
// inside a field.
func TestSSEFrameCompactsPrettyPrintedData(t *testing.T) {
	e := events.Envelope{
		SchemaVersion: events.SchemaVersion,
		EventID:       "01J4ZZZZZZZZZZZZZZZZZZZZZZ",
		SessionID:     "session-101",
		Name:          events.MessageCommitted,
		Seq:           7,
		OccurredAt:    time.Date(2026, time.October, 16, 12, 0, 0, 0, time.UTC),
		Data:          json.RawMessage("{\n  \"text\": \"hello\",\n  \"n\": 1\n}"),
	}
	frame, err := encodeSSEFrame(e)
	if err != nil {
		t.Fatalf("sseFrame: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(string(frame), "\n\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("frame is %d lines, want exactly id/event/data:\n%q", len(lines), string(frame))
	}
	if lines[0] != "id: 7" || lines[1] != "event: message.committed" {
		t.Fatalf("frame header lines = %q", lines[:2])
	}
	if !strings.HasPrefix(lines[2], "data: {") || !strings.HasSuffix(lines[2], "}") {
		t.Fatalf("data line = %q", lines[2])
	}
	var back events.Envelope
	if err := json.Unmarshal(bytes.TrimPrefix([]byte(lines[2]), []byte("data: ")), &back); err != nil {
		t.Fatalf("data line is not the envelope: %v", err)
	}
	if back.Seq != 7 || string(back.Data) != `{"text":"hello","n":1}` {
		t.Fatalf("decoded = %+v (data %s)", back, back.Data)
	}
}

func TestSSEFrameRejectsInvalidData(t *testing.T) {
	e := events.Envelope{
		SchemaVersion: events.SchemaVersion,
		EventID:       "01J4ZZZZZZZZZZZZZZZZZZZZZZ",
		Name:          events.MessageCommitted,
		Seq:           1,
		OccurredAt:    time.Now().UTC(),
		// json.Valid rejects this, so it must never reach a browser as a frame.
		Data: json.RawMessage(`{"text":"unterminated`),
	}
	if _, err := encodeSSEFrame(e); err == nil {
		t.Fatal("encodeSSEFrame produced a frame out of invalid JSON")
	}
}

// docs/spec/12 "UI/operations": SSE reconnect and Last-Event-ID; "Robustness":
// duplicate SSE delivery.
func TestEventsResumeWithLastEventID(t *testing.T) {
	srv, hub := newEventServer(t)

	const total = 5
	want := make([]int64, 0, total)
	for i := 0; i < total; i++ {
		want = append(want, publishTestEvent(t, hub, events.MessageCommitted, "session-101").Seq)
	}

	// First connection: consume three events, then the tab closes.
	first := openEventStream(t, srv, "", nil)
	delivered := make([]int64, 0, total)
	for i := 0; i < 3; i++ {
		f, err := first.next(3 * time.Second)
		if err != nil {
			first.stop()
			t.Fatalf("frame %d: %v", i, err)
		}
		delivered = append(delivered, f.cursor(t))
	}
	cursor := delivered[len(delivered)-1]
	first.stop()

	// A reconnect with that cursor must see exactly what it missed: seqs 4 and 5,
	// once each, in order, with nothing from before the cursor.
	reconnect := openEventStream(t, srv, "", map[string]string{HeaderLastEventID: fmt.Sprint(cursor)})
	defer reconnect.stop()
	for _, seq := range want[3:] {
		f, err := reconnect.next(3 * time.Second)
		if err != nil {
			t.Fatalf("resumed frame for seq %d: %v", seq, err)
		}
		delivered = append(delivered, f.cursor(t))
	}

	if got := delivered; !equalSeqs(got, want) {
		t.Fatalf("delivered %v across the reconnect, want exactly %v (no duplicates, no gaps)", got, want)
	}
}

// A second reconnect from the same cursor re-delivers the same tail: at-least-
// once delivery with a cursor the client can dedupe on, which is what the
// event reducer in docs/spec/09 is required to do.
func TestEventsReconnectFromTheEndIsQuiet(t *testing.T) {
	srv, hub := newEventServer(t)
	last := publishTestEvent(t, hub, events.MessageCommitted, "s")

	stream := openEventStream(t, srv, fmt.Sprintf("?last_event_id=%d", last.Seq), nil)
	defer stream.stop()

	// Nothing to catch up on: the next frame must be a keepalive or a genuinely
	// new event, never a replay of what the client already has.
	for i := 0; i < 6; i++ {
		f, err := stream.next(100 * time.Millisecond)
		if err != nil {
			break // idle, which is the expected outcome
		}
		if f.Comment == "" {
			t.Fatalf("reconnect from the head re-delivered seq %s (%s), want only live events", f.ID, f.Name)
		}
	}
	next := publishTestEvent(t, hub, events.MessageCommitted, "s")
	f, err := stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("live frame after the quiet reconnect: %v", err)
	}
	if f.cursor(t) != next.Seq {
		t.Fatalf("live frame = seq %d, want %d", f.cursor(t), next.Seq)
	}
}

func TestEventsHeartbeat(t *testing.T) {
	srv, _ := newEventServer(t)
	stream := openEventStream(t, srv, "?session_id=nobody-publishes-here", nil)
	defer stream.stop()

	// newEventServer sets a 50 ms heartbeat; a 15 s default would make this test
	// a 15 s test.
	for i := 0; i < 20; i++ {
		f, err := stream.next(time.Second)
		if err != nil {
			t.Fatalf("heartbeat %d: %v", i, err)
		}
		if f.Comment == "" {
			continue
		}
		if !strings.HasPrefix(f.Comment, "hb ") {
			t.Fatalf("comment frame = %q, want `hb <unix>`", f.Comment)
		}
		var unix int64
		if _, err := fmt.Sscanf(f.Comment, "hb %d", &unix); err != nil {
			t.Fatalf("heartbeat %q carries no unix time: %v", f.Comment, err)
		}
		if unix < time.Now().Add(-time.Hour).Unix() {
			t.Fatalf("heartbeat timestamp %d is not now", unix)
		}
		return
	}
	t.Fatal("no heartbeat comment arrived in 20 frames of a 50 ms-interval stream")
}

// docs/spec/08: a cursor the server cannot interpret is refused. Treating it as
// "from the beginning" would replay history a browser already acted on — the
// duplicate-speech failure in docs/spec/09 — and treating it as "current" would
// be the silent gap.
func TestEventsRejectsBadCursor(t *testing.T) {
	cases := []struct {
		name    string
		query   string
		headers map[string]string
	}{
		{"header is not numeric", "", map[string]string{HeaderLastEventID: "evt-1002"}},
		{"header is negative", "", map[string]string{HeaderLastEventID: "-4"}},
		{"header overflows int64", "", map[string]string{HeaderLastEventID: "99999999999999999999999"}},
		{"header is not a plain integer", "", map[string]string{HeaderLastEventID: "1.5"}},
		{"header is hexadecimal", "", map[string]string{HeaderLastEventID: "0x10"}},
		{"query is not numeric", "?last_event_id=abc", nil},
		{"query is a signed cursor", "?last_event_id=%2B7", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newEventServer(t)
			rec := requestEvents(t, srv, tc.query, tc.headers)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			requireContentType(t, rec, ContentTypeProblem)
			var p problem
			if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.RequestID == "" || !strings.Contains(p.Type, "invalid-last-event-id") {
				t.Fatalf("problem = %+v, want an invalid-last-event-id urn with a request_id", p)
			}
		})
	}
}

// Browsers strip whitespace from the Last-Event-ID value, and a client that has
// never seen an id sends the header absent. Both must open a stream from the
// head of what is retained, not be mistaken for a malformed cursor.
func TestEventsAcceptsBlankAndTrimmedCursors(t *testing.T) {
	srv, hub := newEventServer(t)
	first := publishTestEvent(t, hub, events.MessageCommitted, "s")
	second := publishTestEvent(t, hub, events.MessageCommitted, "s")

	// Absent and empty both mean "from the beginning of what is retained".
	for _, tc := range []struct {
		name    string
		headers map[string]string
	}{
		{"absent", nil},
		{"empty", map[string]string{HeaderLastEventID: ""}},
	} {
		stream := openEventStream(t, srv, "", tc.headers)
		f, err := stream.next(3 * time.Second)
		if err != nil {
			stream.stop()
			t.Fatalf("%s cursor: %v", tc.name, err)
		}
		if got := f.cursor(t); got != first.Seq {
			stream.stop()
			t.Fatalf("%s cursor resumed at %d, want the oldest retained (%d)", tc.name, got, first.Seq)
		}
		stream.stop()
	}

	// "%20" in front of the digits is the whitespace a browser would have
	// stripped: it must parse rather than answer 400.
	padded := openEventStream(t, srv, fmt.Sprintf("?last_event_id=%%20%d", first.Seq), nil)
	defer padded.stop()
	f, err := padded.next(3 * time.Second)
	if err != nil {
		t.Fatalf("padded cursor: %v", err)
	}
	if got := f.cursor(t); got != second.Seq {
		t.Fatalf("padded cursor %d resumed at %d, want %d", first.Seq, got, second.Seq)
	}
}

func TestEventsRejectsBadFilter(t *testing.T) {
	for _, tc := range []struct{ query, want string }{
		{"?names=made.up", "unknown event name"},
		{"?names=speech.started,made.up", "unknown event name"},
		{"?session_id=session%20one", "outside [A-Za-z0-9._:-]"},
		{"?session_id=a&session_id=b", "given once"},
	} {
		t.Run(tc.query, func(t *testing.T) {
			srv, _ := newEventServer(t)
			rec := requestEvents(t, srv, tc.query, nil)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 (body %s)", rec.Code, rec.Body)
			}
			requireContentType(t, rec, ContentTypeProblem)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("detail does not explain %q: %s", tc.want, rec.Body)
			}
		})
	}
}

// docs/spec/08: replay from the bounded log, "otherwise send
// events.resync_required and use REST snapshot".
func TestEventsResyncRequiredOnHistoryGap(t *testing.T) {
	srv, hub := newEventServer(t, events.WithRingSize(4))
	for i := 0; i < 10; i++ {
		publishTestEvent(t, hub, events.MessageCommitted, "s")
	}

	stream := openEventStream(t, srv, "?last_event_id=1", nil)
	defer stream.stop()

	f, err := stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("first frame: %v", err)
	}
	if f.Name != events.EventsResyncRequired {
		t.Fatalf("first event = %q, want %q", f.Name, events.EventsResyncRequired)
	}
	e := f.envelope(t)
	var data struct {
		Reason     string `json:"reason"`
		FromSeq    int64  `json:"from_seq"`
		ThroughSeq int64  `json:"through_seq"`
		Cursor     int64  `json:"cursor"`
		Snapshot   string `json:"snapshot"`
	}
	if err := json.Unmarshal(e.Data, &data); err != nil {
		t.Fatalf("resync data: %v", err)
	}
	if data.Reason != "gap" || data.FromSeq != 2 || data.ThroughSeq != 6 || data.Cursor != 6 {
		t.Fatalf("resync payload = %+v, want gap 2..6 with cursor 6", data)
	}
	if !strings.HasPrefix(data.Snapshot, "/api/v1/") {
		t.Fatalf("resync must name the snapshot endpoint to recover from, got %q", data.Snapshot)
	}
	// The notice is itself a cursor the client can safely resume from.
	if f.cursor(t) != data.ThroughSeq {
		t.Fatalf("resync id = %q, want %d", f.ID, data.ThroughSeq)
	}
	// Then delivery continues with what the log still holds.
	next, err := stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("frame after the notice: %v", err)
	}
	if next.cursor(t) != 7 {
		t.Fatalf("next event = %s, want the oldest retained (7)", next.ID)
	}
}

// docs/spec/01 "slow browsers cannot stall capture" and docs/spec/08
// "Disconnecting browser never stops playback": a client that goes away costs
// nothing and changes nothing for anyone else.
func TestEventsDisconnectIsClean(t *testing.T) {
	// A short heartbeat is not a convenience here: it is the mechanism by which a
	// server notices that the browser is gone. A handler that never writes never
	// learns the client hung up, which is why docs/spec/08 asks for heartbeats.
	hub := events.New(events.WithHeartbeat(10 * time.Millisecond))
	srv := httptest.NewServer(newHandler(t, WithEvents(hub)))
	defer srv.Close()

	// The settle loop is the only honest way to compare goroutine counts: the
	// server's connection goroutine exits asynchronously after we hang up.
	settle := func(want int) bool {
		for i := 0; i < 200; i++ {
			if runtime.NumGoroutine() <= want {
				return true
			}
			time.Sleep(10 * time.Millisecond)
		}
		return false
	}
	runtime.GC()
	baseline := runtime.NumGoroutine()

	witness := subscribeTest(t, hub, "disconnect-test")
	defer witness.Close()

	var streams []*sseStream
	for i := 0; i < 3; i++ {
		streams = append(streams, openEventStream(t, srv, "?session_id=disconnect-test", nil))
	}
	publishTestEvent(t, hub, events.MessageCommitted, "disconnect-test")
	for _, s := range streams {
		if _, err := s.next(3 * time.Second); err != nil {
			t.Fatalf("frame: %v", err)
		}
	}
	for _, s := range streams {
		s.stop()
	}

	if !settle(baseline) {
		var buf [1 << 16]byte
		n := runtime.Stack(buf[:], true)
		t.Fatalf("goroutines went from %d to %d after the clients hung up; stacks:\n%s",
			baseline, runtime.NumGoroutine(), buf[:n])
	}
	// Publishers are unaffected, and the hub keeps numbering.
	for i := 0; i < 5; i++ {
		publishTestEvent(t, hub, events.MessageCommitted, "disconnect-test")
	}
	if head := hub.Head(); head != 6 {
		t.Fatalf("Head = %d, want 6 (one event before, five after the disconnects)", head)
	}
	// Whatever the departed clients never consumed is still replayable — the
	// events are the log's, not the connection's.
	late := subscribeTest(t, hub, "disconnect-test")
	defer late.Close()
	seen := 0
	for {
		e, ok, err := late.Next(t.Context(), 100*time.Millisecond)
		if err != nil || !ok {
			break
		}
		if e.SessionID != "disconnect-test" {
			t.Fatalf("filter leaked %q", e.SessionID)
		}
		seen++
	}
	if seen != 6 {
		t.Fatalf("replay after the disconnects yielded %d events, want 6", seen)
	}
}

// A client that is connected but never reads must not slow the runtime down.
// The kernel absorbs a modest burst; what matters is that publishing completes
// and the hub accounts for the load it shed.
func TestEventsSlowClientDoesNotStallPublishers(t *testing.T) {
	srv, hub := newEventServer(t, events.WithRingSize(64))
	stream := openEventStream(t, srv, "", nil)
	defer stream.stop()

	start := time.Now()
	for i := 0; i < 500; i++ {
		name := events.FrameObserved
		if i%50 == 0 {
			name = events.SpeechStarted
		}
		publishTestEvent(t, hub, name, "s")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("publishing behind an unread client took %v, want under 1s", elapsed)
	}
	if st := hub.Stats(); st.Dropped == 0 {
		t.Fatalf("hub stats = %+v, want the stalled stream's droppable events shed", st)
	}
	// The stream still works for the events it did get.
	if _, err := stream.next(2 * time.Second); err != nil {
		t.Fatalf("the stalled stream stopped working entirely: %v", err)
	}
}

// docs/spec/08 and bootstrap.Server.Run: shutdown has to stop the stream, or
// net/http's drain waits forever on a request that is in flight by design.
func TestEventsEndsOnHubClose(t *testing.T) {
	hub := events.New(events.WithHeartbeat(time.Hour))
	srv := httptest.NewServer(newHandler(t, WithEvents(hub)))
	defer srv.Close()

	stream := openEventStream(t, srv, "", nil)
	defer stream.stop()
	publishTestEvent(t, hub, events.MessageCommitted, "s")
	if _, err := stream.next(3 * time.Second); err != nil {
		t.Fatalf("frame: %v", err)
	}

	if err := hub.Close(); err != nil {
		t.Fatalf("hub.Close: %v", err)
	}
	// The handler returns, which the client sees as the end of the body rather
	// than a stalled connection holding the process open.
	if _, err := stream.next(3 * time.Second); err == nil {
		t.Fatal("the stream kept producing frames after the hub closed")
	} else if !errors.Is(err, io.EOF) && !strings.Contains(err.Error(), "EOF") {
		t.Fatalf("stream error = %v, want the end of the body", err)
	}
}

func TestEventsSessionFilterIsEnforced(t *testing.T) {
	srv, hub := newEventServer(t)
	publishTestEvent(t, hub, events.MessageCommitted, "other")
	mine := publishTestEvent(t, hub, events.MessageCommitted, "mine")

	stream := openEventStream(t, srv, "?session_id=mine&names=message.committed", nil)
	defer stream.stop()

	f, err := stream.next(3 * time.Second)
	if err != nil {
		t.Fatalf("frame: %v", err)
	}
	if got := f.envelope(t); got.Seq != mine.Seq {
		t.Fatalf("delivered seq %d from session %q, want only mine's %d", got.Seq, got.SessionID, mine.Seq)
	}
	// An unrelated session's event must not reach this stream.
	publishTestEvent(t, hub, events.MessageCommitted, "other")
	if f, err := stream.next(200 * time.Millisecond); err == nil && f.Comment == "" {
		t.Fatalf("another session's event leaked into the stream: %s %s", f.ID, f.Name)
	}
}

// TestEventsHeadIsNotAStream pins the distinction the middleware leaves to the
// handler: HEAD on a GET route is dispatched here, and must be refused before
// any body is written.
func TestEventsHeadIsNotAStream(t *testing.T) {
	h := newHandler(t, WithEvents(events.New()))
	srv := httptest.NewServer(h)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, srv.URL+eventsPath, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("HEAD status = %d, want 405", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); got != ContentTypeProblem {
		t.Fatalf("HEAD Content-Type = %q, want problem+json", got)
	}
}

// helpers ---------------------------------------------------------------

func subscribeTest(t *testing.T, h *events.Hub, sessionID string) *events.Subscription {
	t.Helper()
	sub, err := h.Subscribe(context.Background(), events.Filter{SessionID: sessionID}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	return sub
}

// requestEvents performs a whole non-streaming request and returns the response
// as a recorder, so the error paths can reuse requireContentType/decodeJSON.
func requestEvents(t *testing.T, srv *httptest.Server, query string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+eventsPath+query, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	rec := httptest.NewRecorder()
	for k, vs := range resp.Header {
		for _, v := range vs {
			rec.Header().Add(k, v)
		}
	}
	rec.Code = resp.StatusCode
	rec.Body = bytes.NewBuffer(body)
	return rec
}

func equalSeqs(got, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
