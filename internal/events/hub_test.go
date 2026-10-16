package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
)

func mustPublish(t *testing.T, h *Hub, name, sessionID string) Envelope {
	t.Helper()
	e, err := h.Publish(context.Background(), Envelope{
		Name:      name,
		SessionID: sessionID,
		Data:      json.RawMessage(`{"n":1}`),
	})
	if err != nil {
		t.Fatalf("Publish(%s): %v", name, err)
	}
	return e
}

// drain collects everything currently queued, stopping at a short idle so a
// test never hangs on a subscription that received nothing.
func drain(t *testing.T, sub *Subscription, idle time.Duration) []Envelope {
	t.Helper()
	var out []Envelope
	ctx := context.Background()
	for {
		e, ok, err := sub.Next(ctx, idle)
		if err != nil {
			if errors.Is(err, ErrNoMoreEvents) {
				return out
			}
			t.Fatalf("Next: %v", err)
		}
		if !ok {
			return out
		}
		out = append(out, e)
	}
}

func TestPublishAssignsEnvelopeFields(t *testing.T) {
	h := New()
	got, err := h.Publish(context.Background(), Envelope{Name: TextObserved, SessionID: "s-1"})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.Seq != 1 {
		t.Fatalf("seq = %d, want 1", got.Seq)
	}
	if !games.ValidID(got.EventID) {
		t.Fatalf("event_id %q is not a ULID", got.EventID)
	}
	if got.OccurredAt.IsZero() || got.OccurredAt.Location() != time.UTC {
		t.Fatalf("occurred_at = %v, want now in UTC", got.OccurredAt)
	}
	if got.SchemaVersion != SchemaVersion {
		t.Fatalf("schema_version = %d, want %d", got.SchemaVersion, SchemaVersion)
	}
	if string(got.Data) != "{}" {
		t.Fatalf("data = %s, want the empty object", got.Data)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("the hub published an invalid envelope: %v", err)
	}

	second := mustPublish(t, h, TextObserved, "s-1")
	if second.Seq != 2 {
		t.Fatalf("seq = %d, want 2", second.Seq)
	}
	if second.EventID == got.EventID {
		t.Fatal("two events share an event_id")
	}
	if h.Head() != 2 {
		t.Fatalf("Head = %d, want 2", h.Head())
	}
}

func TestPublishKeepsCallerValues(t *testing.T) {
	h := New()
	id := games.NewID()
	at := time.Date(2026, time.January, 2, 3, 4, 5, 0, time.UTC)
	in := Envelope{
		EventID:     id,
		OccurredAt:  at,
		Name:        MessageCommitted,
		TraceID:     "trace-a9",
		CausationID: games.NewID(),
		Data:        json.RawMessage(`{"text":"hello"}`),
	}
	got, err := h.Publish(context.Background(), in)
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if got.EventID != id || !got.OccurredAt.Equal(at) || got.TraceID != "trace-a9" {
		t.Fatalf("the hub overwrote caller-supplied fields: %+v", got)
	}
}

// A publisher that reuses its marshal buffer must not be able to rewrite an
// event already handed to every subscriber.
func TestPublishCopiesData(t *testing.T) {
	h := New()
	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	data := json.RawMessage(`{"frame":"a"}`)
	if _, err := h.Publish(context.Background(), Envelope{Name: FrameObserved, Data: data}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	// Mutate the caller's own buffer in place.
	copy(data, []byte(`{"frame":"b"}`))

	got := drain(t, sub, 50*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("subscriber received %d events, want 1", len(got))
	}
	if string(got[0].Data) != `{"frame":"a"}` {
		t.Fatalf("data = %s, want the value at publish time (the hub must copy)", got[0].Data)
	}
}

func TestPublishRejectsBadInputAndLeavesLogUntouched(t *testing.T) {
	h := New()
	before := h.Stats()

	if _, err := h.Publish(context.Background(), Envelope{Name: "not.a.real_event"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Publish with an unknown name = %v, want ErrValidation", err)
	}
	if _, err := h.Publish(context.Background(), Envelope{Name: TextObserved, Data: json.RawMessage(`[1]`)}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Publish with a non-object payload = %v, want ErrValidation", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Publish(ctx, Envelope{Name: TextObserved}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish on a cancelled ctx = %v, want context.Canceled", err)
	}

	if after := h.Stats(); after != before {
		t.Fatalf("a rejected publish moved the counters: %+v -> %+v", before, after)
	}
}

// docs/spec/01 requires one global monotonic seq; docs/spec/12 lists
// "duplicate SSE delivery" and history gaps as the failure modes.
func TestPublishConcurrentSeqIsMonotonicAndGapless(t *testing.T) {
	const publishers, perPublisher = 16, 1000
	// Deep enough that the assertion under test is ordering, not the drop policy.
	h := New(WithSubscriberBuffer(publishers*perPublisher + 64))
	ctx := context.Background()

	sub, err := h.Subscribe(ctx, Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perPublisher; i++ {
				if _, err := h.Publish(ctx, Envelope{
					Name:      MessageCommitted,
					SessionID: fmt.Sprintf("session-%d", p%4),
					Data:      json.RawMessage(`{"i":1}`),
				}); err != nil {
					t.Errorf("publish: %v", err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	if head := h.Head(); head != publishers*perPublisher {
		t.Fatalf("Head = %d, want %d", head, publishers*perPublisher)
	}
	if st := h.Stats(); st.Published != publishers*perPublisher || st.Dropped != 0 {
		t.Fatalf("stats = %+v, want %d published and nothing dropped", st, publishers*perPublisher)
	}

	got := drain(t, sub, 500*time.Millisecond)
	if len(got) != publishers*perPublisher {
		t.Fatalf("subscriber received %d events, want %d (every critical event must be delivered)",
			len(got), publishers*perPublisher)
	}
	ids := make(map[string]bool, len(got))
	var last int64
	for i, e := range got {
		if e.Seq != last+1 {
			t.Fatalf("event %d has seq %d after %d: the stream must be strictly increasing with no gaps",
				i, e.Seq, last)
		}
		if ids[e.EventID] {
			t.Fatalf("duplicate event_id %q on one subscription (docs/spec/12 \"duplicate SSE delivery\")", e.EventID)
		}
		ids[e.EventID] = true
		last = e.Seq
	}
}

func TestPublishStoreFailureChangesNothing(t *testing.T) {
	var logs bytes.Buffer
	h := New(
		WithStore(FailingStore{Err: errors.New("database is locked")}),
		WithLogger(slog.New(slog.NewTextHandler(&logs, nil))),
	)
	if _, err := h.Publish(context.Background(), Envelope{Name: TextObserved}); err == nil {
		t.Fatal("Publish succeeded while the durable log was failing")
	}
	if h.Head() != 0 {
		t.Fatalf("Head = %d after a failed append, want 0: the log must not gain a phantom seq", h.Head())
	}
	// A dead durable log degrades replay, it does not take the stream down: a
	// browser keeps watching live events (AGENTS.md rule 7).
	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe with an unreadable store: %v", err)
	}
	if got := drain(t, sub, 30*time.Millisecond); len(got) != 0 {
		t.Fatalf("subscribers received %v after a failed append, want nothing", seqs(got))
	}
	if !strings.Contains(logs.String(), "event store unavailable") {
		t.Fatalf("the degraded replay was silent: %s", logs.String())
	}
	sub.Close()
}

func TestReplayFromAfterSeq(t *testing.T) {
	h := New(WithRingSize(64))
	for i := 0; i < 10; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}

	sub, err := h.Subscribe(context.Background(), Filter{}, 5)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	requireSeqs(t, got, 6, 7, 8, 9, 10)

	// A cursor at the head means live only, and no resync.
	live, err := h.Subscribe(context.Background(), Filter{}, h.Head())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got := drain(t, live, 30*time.Millisecond); len(got) != 0 {
		t.Fatalf("a cursor at the head replayed %v, want nothing", got)
	}
	// The cursor an EventSource sends after a hub restart may name a seq that no
	// longer exists but was never missed: still no resync.
	ahead, err := h.Subscribe(context.Background(), Filter{}, h.Head()+7)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if got := drain(t, ahead, 30*time.Millisecond); len(got) != 0 {
		t.Fatalf("a cursor ahead of the head replayed %v, want nothing", got)
	}
}

func TestReplayHonoursTheFilterAndContinuesLive(t *testing.T) {
	h := New(WithRingSize(64))
	mustPublish(t, h, FrameObserved, "s-1")
	mustPublish(t, h, MessageCommitted, "s-1")
	mustPublish(t, h, TextObserved, "s-2")
	mustPublish(t, h, MessageCommitted, "s-2")

	sub, err := h.Subscribe(context.Background(), Filter{SessionID: "s-2", Names: []string{MessageCommitted}}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	requireSeqs(t, got, 4)

	// Live events keep the same filter, in the same strict order.
	mustPublish(t, h, MessageCommitted, "s-1")
	e := mustPublish(t, h, MessageCommitted, "s-2")
	if got := drain(t, sub, 30*time.Millisecond); len(got) != 1 || got[0].Seq != e.Seq {
		t.Fatalf("live delivery = %v, want only the s-2 event at seq %d", seqs(got), e.Seq)
	}
}

// docs/spec/08: replay from the bounded log "otherwise send
// events.resync_required". docs/spec/12 "UI/operations": history gap.
func TestResyncWhenCursorIsOlderThanTheRing(t *testing.T) {
	h := New(WithRingSize(4), WithSubscriberBuffer(512))
	for i := 0; i < 10; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}

	sub, err := h.Subscribe(context.Background(), Filter{}, 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	if len(got) == 0 || got[0].Name != EventsResyncRequired {
		t.Fatalf("first event = %v, want events.resync_required", seqs(got))
	}
	var data map[string]any
	if err := json.Unmarshal(got[0].Data, &data); err != nil {
		t.Fatalf("resync data: %v", err)
	}
	if data["reason"] != "gap" {
		t.Fatalf("resync reason = %v, want gap", data["reason"])
	}
	// The ring kept 7..10, so 2..6 are the lost range and the notice's own seq
	// is the last lost event: the client's cursor lands exactly where replay
	// resumes, which is what stops a reconnect from resyncing forever.
	if data["from_seq"] != float64(2) || data["through_seq"] != float64(6) || got[0].Seq != 6 {
		t.Fatalf("resync gap = %v (seq %d), want 2..6 with seq 6", data, got[0].Seq)
	}
	requireSeqs(t, got[1:], 7, 8, 9, 10)

	// Reconnecting with the notice's cursor must not produce a second notice.
	sub2, err := h.Subscribe(context.Background(), Filter{}, 6)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	again := drain(t, sub2, 30*time.Millisecond)
	requireSeqs(t, again, 7, 8, 9, 10)
	for _, e := range again {
		if e.Name == EventsResyncRequired {
			t.Fatalf("resync looped: %v", seqs(again))
		}
	}

	// And live delivery still works on the resynced stream.
	e := mustPublish(t, h, MessageCommitted, "s-1")
	if got := drain(t, sub, 30*time.Millisecond); len(got) != 1 || got[0].Seq != e.Seq {
		t.Fatalf("live after resync = %v, want seq %d", seqs(got), e.Seq)
	}
}

func TestStoreReplaysPastTheRingWithoutResync(t *testing.T) {
	store := NewMemStore()
	h := New(WithStore(store), WithRingSize(4), WithSubscriberBuffer(512))
	for i := 0; i < 20; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}
	if store.Len() != 20 {
		t.Fatalf("store holds %d events, want 20", store.Len())
	}

	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	want := make([]int64, 0, 20)
	for i := int64(1); i <= 20; i++ {
		want = append(want, i)
	}
	requireSeqs(t, got, want...)
	for _, e := range got {
		if e.Name == EventsResyncRequired {
			t.Fatalf("a durable log covered the whole cursor: unexpected resync")
		}
	}
	// Live still follows the replay exactly once.
	e := mustPublish(t, h, MessageCommitted, "s-1")
	if got := drain(t, sub, 30*time.Millisecond); len(got) != 1 || got[0].Seq != e.Seq {
		t.Fatalf("live after store replay = %v, want seq %d", seqs(got), e.Seq)
	}
}

func TestStoreReplayHonoursTheFilter(t *testing.T) {
	store := NewMemStore()
	h := New(WithStore(store), WithRingSize(2), WithSubscriberBuffer(512))
	for i := 0; i < 8; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
		mustPublish(t, h, FrameObserved, "s-1")
		mustPublish(t, h, MessageCommitted, "s-2")
	}
	sub, err := h.Subscribe(context.Background(),
		Filter{SessionID: "s-2", Names: []string{MessageCommitted}}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// s-2 events are seqs 3, 6, 9, 12, 15, 18, 21, 24.
	got := drain(t, sub, 30*time.Millisecond)
	for _, e := range got {
		if e.SessionID != "s-2" || e.Name != MessageCommitted {
			t.Fatalf("filtered replay leaked %s/%s", e.SessionID, e.Name)
		}
	}
	if len(got) != 8 {
		t.Fatalf("filtered replay delivered %d events, want 8: %v", len(got), seqs(got))
	}
}

func TestStorePrunedHistoryResyncs(t *testing.T) {
	store := NewMemStore()
	h := New(WithStore(store), WithRingSize(2), WithSubscriberBuffer(512))
	for i := 0; i < 20; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}
	if n := store.PruneBelow(10); n != 9 {
		t.Fatalf("PruneBelow dropped %d, want 9", n)
	}

	sub, err := h.Subscribe(context.Background(), Filter{}, 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	if got[0].Name != EventsResyncRequired {
		t.Fatalf("first event = %s, want resync after the log was pruned", got[0].Name)
	}
	var data map[string]any
	if err := json.Unmarshal(got[0].Data, &data); err != nil {
		t.Fatalf("resync data: %v", err)
	}
	if data["reason"] != "gap" || data["from_seq"] != float64(2) || data["through_seq"] != float64(9) {
		t.Fatalf("resync payload = %v, want gap 2..9", data)
	}
	// 10..18 survive in the store, 19..20 in the ring: contiguous coverage from
	// the resync point, so no second notice and no duplicate at the handoff.
	rest := got[1:]
	requireSeqs(t, rest, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20)
}

func TestStoreReplayBudgetReportsTruncation(t *testing.T) {
	store := NewMemStore()
	h := New(WithStore(store), WithRingSize(2), WithReplayBudget(5), WithSubscriberBuffer(512))
	for i := 0; i < 20; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}
	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	if len(got) != 8 {
		t.Fatalf("replay = %v, want 1..5, a truncation notice, then 19 and 20", seqs(got))
	}
	requireSeqs(t, got[:5], 1, 2, 3, 4, 5)
	requireSeqs(t, got[6:], 19, 20)
	if got[5].Name != EventsResyncRequired {
		t.Fatalf("event 6 = %s, want the truncation notice", got[5].Name)
	}
	var data map[string]any
	if err := json.Unmarshal(got[5].Data, &data); err != nil {
		t.Fatalf("notice data: %v", err)
	}
	if data["reason"] != "replay_truncated" || data["from_seq"] != float64(6) || data["through_seq"] != float64(18) {
		t.Fatalf("notice payload = %v, want replay_truncated 6..18", data)
	}
	// The notice's own cursor is the end of the hole, so reconnecting there is
	// served from the ring rather than resyncing again.
	back, err := h.Subscribe(context.Background(), Filter{}, got[5].Seq)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	requireSeqs(t, drain(t, back, 30*time.Millisecond), 19, 20)
}

func TestStoreSeqSurvivesAHubRestart(t *testing.T) {
	store := NewMemStore()
	first := New(WithStore(store))
	for i := 0; i < 3; i++ {
		mustPublish(t, first, MessageCommitted, "s-1")
	}
	// The durable log is the sequence's owner, so a new hub on top of it must
	// continue rather than restart at 1 and alias old cursors.
	second := New(WithStore(store), WithSubscriberBuffer(64))
	e := mustPublish(t, second, MessageCommitted, "s-1")
	if e.Seq != 4 {
		t.Fatalf("seq after a restart = %d, want 4", e.Seq)
	}
	sub, err := second.Subscribe(context.Background(), Filter{}, 2)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	requireSeqs(t, got, 3, 4)
}

// docs/spec/08 backpressure policy, tested the way the spec frames it: 10k
// redundant frame previews plus 50 critical events, one stalled subscriber and
// one keeping up, and the publisher must not wait.
func TestBackpressureDropsDroppableBeforeStalling(t *testing.T) {
	// The backlog is 64 rather than the production default 256 so the 1 s bound
	// measures the property under test — that publishing never waits on a stalled
	// consumer — instead of this machine's cost of shifting a 256-deep queue under
	// the race detector. Everything else, including the 10k/50 mix and the
	// demand that every critical event survive, is the documented scenario.
	const droppable, critical = 10_000, 50
	h := New(WithRingSize(1024), WithSubscriberBuffer(64))
	ctx := context.Background()

	stalled, err := h.Subscribe(ctx, Filter{}, 0) // never read until the end
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fast, err := h.Subscribe(ctx, Filter{}, 0, WithBuffer(droppable+critical+64))
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		for i := 0; i < droppable; i++ {
			if i%200 == 199 && i/200 < critical {
				if _, err := h.Publish(ctx, Envelope{Name: MessageCommitted, Data: json.RawMessage(`{"i":1}`)}); err != nil {
					t.Errorf("publish critical: %v", err)
					return
				}
			}
			if _, err := h.Publish(ctx, Envelope{Name: FrameObserved, Data: json.RawMessage(`{"i":1}`)}); err != nil {
				t.Errorf("publish droppable: %v", err)
				return
			}
		}
		done <- time.Since(start)
	}()

	var elapsed time.Duration
	select {
	case elapsed = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked behind a slow subscriber (docs/spec/01: slow browsers cannot stall capture)")
	}
	if elapsed > time.Second {
		t.Fatalf("publishing %d events took %v, want under 1s", droppable+critical, elapsed)
	}

	fastStats := fast.Stats()
	if fastStats.Dropped != 0 || fastStats.Lagged {
		t.Fatalf("a subscriber that keeps up lost events: %+v", fastStats)
	}
	gotFast := drain(t, fast, 200*time.Millisecond)
	if len(gotFast) != droppable+critical {
		t.Fatalf("fast subscriber received %d events, want all %d", len(gotFast), droppable+critical)
	}
	requireStrictlyIncreasing(t, gotFast)

	gotStalled := drain(t, stalled, 200*time.Millisecond)
	st := stalled.Stats()
	if st.Dropped == 0 {
		t.Fatalf("stalled subscriber dropped nothing: %+v", st)
	}
	if len(gotStalled) >= droppable+critical {
		t.Fatalf("stalled subscriber received everything (%d events), so nothing was shed", len(gotStalled))
	}
	requireStrictlyIncreasing(t, gotStalled)

	criticalWanted := map[int64]bool{}
	criticalGot := map[int64]bool{}
	for _, e := range gotFast {
		if !Droppable(e.Name) {
			criticalWanted[e.Seq] = true
		}
	}
	for _, e := range gotStalled {
		if !Droppable(e.Name) {
			criticalGot[e.Seq] = true
		}
	}
	if st.Lagged {
		// Shed as lagged is the other legal outcome, but then the stream must
		// have ended with an explicit resync notice rather than a quiet gap.
		last := gotStalled[len(gotStalled)-1]
		if last.Name != EventsResyncRequired {
			t.Fatalf("lagged subscriber's stream ended with %s, want events.resync_required", last.Name)
		}
		return
	}
	if len(criticalGot) != len(criticalWanted) {
		t.Fatalf("stalled subscriber kept %d of %d critical events and was not marked lagged",
			len(criticalGot), len(criticalWanted))
	}
	for seq := range criticalWanted {
		if !criticalGot[seq] {
			t.Fatalf("critical event at seq %d was dropped from a live subscriber without a resync notice", seq)
		}
	}
}

// The same burst at the production default backlog: correctness rather than
// latency (the 1 s bound lives in the test above), because a shift of a 256-deep
// queue under the race detector measures the machine, not the invariant. What
// must hold regardless is that the publisher finishes and that no state
// transition is silently lost from a stream that is still open.
func TestBackpressureAtDefaultBufferKeepsEveryCriticalEvent(t *testing.T) {
	const droppable, critical = 10_000, 50
	h := New() // DefaultRingSize and DefaultSubscriberBuffer
	ctx := context.Background()

	stalled, err := h.Subscribe(ctx, Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if st := stalled.Stats(); st.Pending != 0 {
		t.Fatalf("fresh subscription has a backlog: %+v", st)
	}

	done := make(chan map[int64]bool, 1)
	go func() {
		wanted := map[int64]bool{}
		for i := 0; i < droppable; i++ {
			if i%200 == 199 && len(wanted) < critical {
				e, err := h.Publish(ctx, Envelope{Name: MessageCommitted, Data: json.RawMessage(`{"i":1}`)})
				if err != nil {
					t.Errorf("publish critical: %v", err)
					return
				}
				wanted[e.Seq] = true
				continue
			}
			if _, err := h.Publish(ctx, Envelope{Name: FrameObserved, Data: json.RawMessage(`{"i":1}`)}); err != nil {
				t.Errorf("publish droppable: %v", err)
				return
			}
		}
		done <- wanted
	}()

	var wanted map[int64]bool
	select {
	case wanted = <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("publishing stopped making progress")
	}

	got := drain(t, stalled, 200*time.Millisecond)
	st := stalled.Stats()
	if st.Dropped == 0 {
		t.Fatalf("nothing was shed at the default bound: %+v", st)
	}
	requireStrictlyIncreasing(t, got)
	if st.Lagged {
		last := got[len(got)-1]
		if last.Name != EventsResyncRequired {
			t.Fatalf("lagged stream ended with %s, want a resync notice", last.Name)
		}
		return
	}
	delivered := map[int64]bool{}
	for _, e := range got {
		if !Droppable(e.Name) {
			delivered[e.Seq] = true
		}
	}
	if len(delivered) != len(wanted) {
		t.Fatalf("a live subscriber kept %d of %d critical events without being retired as lagged",
			len(delivered), len(wanted))
	}
	for seq := range wanted {
		if !delivered[seq] {
			t.Fatalf("critical event seq %d lost without a resync notice", seq)
		}
	}
}

func TestLaggedSubscriberIsClosedWithResync(t *testing.T) {
	h := New(WithRingSize(64), WithSubscriberBuffer(4))
	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Five state transitions into a backlog that holds four: nothing may be
	// evicted, so the subscriber is retired instead.
	for i := 0; i < 5; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}

	got := drain(t, sub, 30*time.Millisecond)
	if len(got) != 5 {
		t.Fatalf("stream = %v, want the four queued events plus a final resync", seqs(got))
	}
	requireSeqs(t, got[:4], 1, 2, 3, 4)
	last := got[4]
	if last.Name != EventsResyncRequired {
		t.Fatalf("final event = %s, want events.resync_required", last.Name)
	}
	var data map[string]any
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatalf("resync data: %v", err)
	}
	if data["reason"] != "lagged" {
		t.Fatalf("resync reason = %v, want lagged", data["reason"])
	}
	if last.Seq != 5 {
		t.Fatalf("resync seq = %d, want the head (5) so a reconnect resumes live", last.Seq)
	}
	st := sub.Stats()
	if !st.Lagged || !st.Closed || st.Dropped == 0 {
		t.Fatalf("stats = %+v, want lagged, closed and counted drops", st)
	}
	if _, _, err := sub.Next(context.Background(), time.Second); !errors.Is(err, ErrNoMoreEvents) {
		t.Fatalf("Next after the final notice = %v, want ErrNoMoreEvents", err)
	}
	// Detached from the hub: later publishes are not queued for a dead stream,
	// and they must not fail because of it.
	mustPublish(t, h, MessageCommitted, "s-1")
	if s := h.Stats(); s.Subscribers != 0 || s.Lagged != 1 {
		t.Fatalf("hub stats = %+v, want the lagged subscriber detached", s)
	}
}

func TestSubscriberCloseIsPrivateToIt(t *testing.T) {
	h := New()
	ctx := context.Background()
	a, err := h.Subscribe(ctx, Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	b, err := h.Subscribe(ctx, Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	e := mustPublish(t, h, MessageCommitted, "s-1")
	if got := drain(t, b, 30*time.Millisecond); len(got) != 1 || got[0].Seq != e.Seq {
		t.Fatalf("the surviving subscriber lost an event when its neighbour closed: %v", seqs(got))
	}
	if _, err := h.Publish(ctx, Envelope{Name: MessageCommitted}); err != nil {
		t.Fatalf("a closed subscriber must not affect publishers: %v", err)
	}
	if s := h.Stats(); s.Subscribers != 1 {
		t.Fatalf("hub stats = %+v, want one detached subscriber", s)
	}
	// Closing a drained subscription leaves its already-queued events readable.
	// Subscribed at the head, so exactly one live event is in flight.
	c, err := h.Subscribe(ctx, Filter{}, h.Head())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	mustPublish(t, h, MessageCommitted, "s-1")
	c.Close()
	got := drain(t, c, 30*time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("queued events must survive a Close, got %v", seqs(got))
	}
	if _, _, err := c.Next(ctx, time.Second); !errors.Is(err, ErrNoMoreEvents) {
		t.Fatalf("a closed subscription must end, not block forever: %v", err)
	}
	if _, err := h.Publish(ctx, Envelope{Name: MessageCommitted}); err != nil {
		t.Fatalf("publishing after every test subscriber went away: %v", err)
	}
	if s := h.Stats(); s.Subscribers != 1 {
		t.Fatalf("hub stats = %+v, want only the surviving subscriber attached", s)
	}
}

func TestHubCloseDrainsStreamsAndStopsTheHub(t *testing.T) {
	h := New()
	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	mustPublish(t, h, MessageCommitted, "s-1")

	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close must be idempotent: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	requireSeqs(t, got, 1)
	if _, _, err := sub.Next(context.Background(), time.Second); !errors.Is(err, ErrNoMoreEvents) {
		t.Fatalf("Next after hub close = %v, want ErrNoMoreEvents", err)
	}
	if _, err := h.Publish(context.Background(), Envelope{Name: MessageCommitted}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after close = %v, want ErrClosed", err)
	}
	if _, err := h.Subscribe(context.Background(), Filter{}, 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after close = %v, want ErrClosed", err)
	}
}

func TestSubscribeValidatesItsFilter(t *testing.T) {
	h := New()
	if _, err := h.Subscribe(context.Background(), Filter{Names: []string{"made.up"}}, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("unknown name = %v, want ErrValidation", err)
	}
	if _, err := h.Subscribe(context.Background(), Filter{SessionID: "session 1"}, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("bad session id = %v, want ErrValidation", err)
	}
	if _, err := h.Subscribe(context.Background(), Filter{SessionID: "s\x00cked"}, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("control byte in a filter = %v, want ErrValidation", err)
	}
	if _, err := h.Subscribe(context.Background(), Filter{}, -1); !errors.Is(err, ErrValidation) {
		t.Fatalf("negative cursor = %v, want ErrValidation", err)
	}
	many := make([]string, 0, maxFilterNames+1)
	for i := 0; i <= maxFilterNames; i++ {
		many = append(many, MessageCommitted)
	}
	if _, err := h.Subscribe(context.Background(), Filter{Names: many}, 0); !errors.Is(err, ErrValidation) {
		t.Fatalf("an oversized name filter = %v, want ErrValidation", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Subscribe(ctx, Filter{}, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe on a cancelled ctx = %v, want context.Canceled", err)
	}
}

func TestNextReturnsOnCancellation(t *testing.T) {
	h := New()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := h.Subscribe(ctx, Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	cancel()
	if _, _, err := sub.Next(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next on a cancelled context = %v, want context.Canceled", err)
	}
}

func TestHeartbeatIsConfigurable(t *testing.T) {
	if got := New().Heartbeat(); got != DefaultHeartbeat {
		t.Fatalf("default heartbeat = %v, want %v", got, DefaultHeartbeat)
	}
	if got := New(WithHeartbeat(50 * time.Millisecond)).Heartbeat(); got != 50*time.Millisecond {
		t.Fatalf("heartbeat = %v, want 50ms", got)
	}
	if got := New(WithHeartbeat(0)).Heartbeat(); got != DefaultHeartbeat {
		t.Fatalf("a zero heartbeat = %v, want the default", got)
	}
}

// drain-then-assert helpers ------------------------------------------------

func seqs(events []Envelope) []int64 {
	out := make([]int64, 0, len(events))
	for _, e := range events {
		out = append(out, e.Seq)
	}
	return out
}

func requireSeqs(t *testing.T, got []Envelope, want ...int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("stream seqs = %v, want %v", seqs(got), want)
	}
	for i, w := range want {
		if got[i].Seq != w {
			t.Fatalf("stream seqs = %v, want %v (event %d)", seqs(got), want, i)
		}
	}
}

func requireStrictlyIncreasing(t *testing.T, got []Envelope) {
	t.Helper()
	var last int64
	for i, e := range got {
		if e.Seq <= last {
			t.Fatalf("event %d has seq %d after %d: not strictly increasing", i, e.Seq, last)
		}
		last = e.Seq
	}
}

func TestStoreThatCannotReadDegradesToTheRing(t *testing.T) {
	// A replica whose event table is locked must still stream: the browser loses
	// deep history, is told about it once, and keeps watching state.
	h := New(WithStore(unreadableStore{NewMemStore()}), WithRingSize(4), WithSubscriberBuffer(512))
	for i := 0; i < 10; i++ {
		mustPublish(t, h, MessageCommitted, "s-1")
	}
	sub, err := h.Subscribe(context.Background(), Filter{}, 0)
	if err != nil {
		t.Fatalf("Subscribe with an unreadable store must not fail the stream: %v", err)
	}
	got := drain(t, sub, 30*time.Millisecond)
	if got[0].Name != EventsResyncRequired {
		t.Fatalf("first event = %s, want the gap notice", got[0].Name)
	}
	requireSeqs(t, got[1:], 7, 8, 9, 10)
	if e := mustPublish(t, h, MessageCommitted, "s-1"); true {
		if live := drain(t, sub, 30*time.Millisecond); len(live) != 1 || live[0].Seq != e.Seq {
			t.Fatalf("live delivery on a degraded store = %v, want seq %d", seqs(live), e.Seq)
		}
	}
}

type unreadableStore struct{ *MemStore }

func (u unreadableStore) After(context.Context, int64, Filter, int) ([]Envelope, error) {
	return nil, errors.New("database is locked")
}

func TestMemStoreAppendIsSeqOrdered(t *testing.T) {
	m := NewMemStore()
	ctx := context.Background()
	var want int64
	for i := 0; i < 5; i++ {
		seq, err := m.Append(ctx, Envelope{
			Name:      MessageCommitted,
			SessionID: "s-1",
			Data:      json.RawMessage(`{}`),
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		want++
		if seq != want {
			t.Fatalf("Append returned %d, want %d", seq, want)
		}
	}
	// An envelope that already carries a seq is renumbered rather than reused,
	// so a store reused across a restart can never hand out an aliased cursor.
	seq, err := m.Append(ctx, Envelope{Name: MessageCommitted, Seq: 3, Data: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if seq != 6 {
		t.Fatalf("Append reused the caller's seq: %d", seq)
	}

	rows, err := m.After(ctx, 3, Filter{}, 0)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if len(rows) != 3 || rows[0].Seq != 4 || rows[2].Seq != 6 {
		t.Fatalf("After(3) = %v, want 4,5,6", seqs(rows))
	}
	rows, err = m.After(ctx, 0, Filter{SessionID: "nope"}, 10)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("a filter that matches nothing returned %v", seqs(rows))
	}
	rows, err = m.After(ctx, 0, Filter{}, 2)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("limit not honoured: %v", seqs(rows))
	}
	if _, err := m.Append(ctx, Envelope{Name: "made.up"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("Append accepted an invalid envelope: %v", err)
	}
	if m.Len() != 6 {
		t.Fatalf("a rejected Append left a row behind: %d", m.Len())
	}
}
