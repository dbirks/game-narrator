package store

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
)

// newEventTestStore returns a migrated store plus its durable event log.
func newEventTestStore(t *testing.T) (*Store, *EventStore) {
	t.Helper()
	s := migratedStore(t)
	return s, NewEventStore(s)
}

// env builds a publishable envelope; the hub assigns seq/event_id, but a
// direct Append must supply everything Validate demands.
func env(t *testing.T, name string, n int) events.Envelope {
	t.Helper()
	return events.Envelope{
		SchemaVersion: events.SchemaVersion,
		EventID:       mustID(t),
		Name:          name,
		OccurredAt:    time.Now().UTC(),
		Data:          json.RawMessage(fmt.Sprintf(`{"n": %d}`, n)),
	}
}

func mustID(t *testing.T) string {
	t.Helper()
	return games.NewID()
}

func TestEventStoreAppendAssignsMonotonicSeq(t *testing.T) {
	ctx := testCtx(t)
	_, es := newEventTestStore(t)

	var last int64
	for i := 1; i <= 5; i++ {
		seq, err := es.Append(ctx, env(t, events.AgentStep, i))
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		if seq != int64(i) {
			t.Fatalf("append %d returned seq %d, want %d", i, seq, i)
		}
		if seq <= last {
			t.Fatalf("seq %d did not exceed previous %d", seq, last)
		}
		last = seq
	}
}

func TestEventStoreAppendRejectsInvalidEnvelopes(t *testing.T) {
	ctx := testCtx(t)
	_, es := newEventTestStore(t)

	bad := env(t, events.AgentStep, 1)
	bad.Name = "not.registered"
	if _, err := es.Append(ctx, bad); err == nil {
		t.Fatal("append of unregistered name succeeded")
	}

	huge := env(t, events.AgentStep, 1)
	huge.Data = json.RawMessage(`{"blob":"` + strings.Repeat("x", events.MaxDataSize) + `"}`)
	if _, err := es.Append(ctx, huge); err == nil {
		t.Fatal("append over MaxDataSize succeeded")
	}

	// A rejected envelope consumed nothing: the next seq is still 1.
	if seq, err := es.Append(ctx, env(t, events.AgentStep, 1)); err != nil || seq != 1 {
		t.Fatalf("append after rejection = (%d, %v), want seq 1", seq, err)
	}
}

func TestEventStoreAfterFiltersAndOrdering(t *testing.T) {
	ctx := testCtx(t)
	_, es := newEventTestStore(t)

	for i := 0; i < 6; i++ {
		name := events.AgentStep
		e := env(t, name, i)
		if i%2 == 0 {
			e.SessionID = "session.a"
			e.Name = events.TextObserved
		}
		if _, err := es.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	all, err := es.After(ctx, 0, events.Filter{}, 0)
	if err != nil {
		t.Fatalf("after all: %v", err)
	}
	if len(all) != 6 {
		t.Fatalf("got %d events, want 6", len(all))
	}
	for i, e := range all {
		if want := int64(i + 1); e.Seq != want {
			t.Fatalf("event %d has seq %d, want %d (ascending)", i, e.Seq, want)
		}
	}

	sess, err := es.After(ctx, 2, events.Filter{SessionID: "session.a"}, 10)
	if err != nil {
		t.Fatalf("after session: %v", err)
	}
	// session.a rows are seqs 1,3,5 (i=0,2,4); after seq 2 → 3,5.
	if len(sess) != 2 || sess[0].Seq != 3 || sess[1].Seq != 5 {
		t.Fatalf("session replay = %v, want seqs [3 5]", seqsOf(sess))
	}
	if sess[0].Name != events.TextObserved || string(sess[0].Data) != `{"n": 2}` {
		t.Fatalf("session row round-trip wrong: %s %s", sess[0].Name, sess[0].Data)
	}

	byName, err := es.After(ctx, 0, events.Filter{Names: []string{events.AgentStep}}, 1000)
	if err != nil {
		t.Fatalf("after names: %v", err)
	}
	if len(byName) != 3 {
		t.Fatalf("name filter returned %d, want 3", len(byName))
	}

	// limit is clamped to [1, 1000]: a page of 1 stays a page of 1, and 0 or
	// negative means the bound, not "unbounded" and not "nothing".
	if one, err := es.After(ctx, 0, events.Filter{}, 1); err != nil || len(one) != 1 || one[0].Seq != 1 {
		t.Fatalf("limit 1 = %d (%v), want exactly seq 1", len(one), err)
	}
	page, err := es.After(ctx, 0, events.Filter{}, 0)
	if err != nil || len(page) != 6 {
		t.Fatalf("default page = %d (%v), want all 6", len(page), err)
	}
	if tiny, err := es.After(ctx, 0, events.Filter{}, -5); err != nil || len(tiny) != 6 {
		t.Fatalf("negative limit = %d (%v), want the clamp to the bound", len(tiny), err)
	}
}

func TestEventStoreSeqSurvivesReopen(t *testing.T) {
	// AUTOINCREMENT is the whole point: a browser holding Last-Event-ID must
	// never have its cursor re-point at a different event after a restart.
	// The property that buys is *reuse-proofness of deleted maxima* — a bare
	// INTEGER PRIMARY KEY would hand rowid 2 back after the row holding it is
	// deleted, aliasing the cursor that survived in the browser.
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	ctx := testCtx(t)

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	es := NewEventStore(s)
	first, err := es.Append(ctx, env(t, events.AgentStep, 1))
	if err != nil {
		t.Fatalf("append 1: %v", err)
	}
	second, err := es.Append(ctx, env(t, events.AgentStep, 2))
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, "DELETE FROM events WHERE seq = ?", second); err != nil {
		t.Fatalf("delete max: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := openPath(t, path)
	third, err := NewEventStore(s2).Append(ctx, env(t, events.AgentStep, 3))
	if err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if third <= second {
		t.Fatalf("seq after reopen = %d, want > %d: the deleted maximum must not be reissued", third, second)
	}
	if first >= second || second >= third {
		t.Fatalf("seqs not monotonic across the restart: %d %d %d", first, second, third)
	}
}

func TestEventStorePruneEvents(t *testing.T) {
	ctx := testCtx(t)
	_, es := newEventTestStore(t)

	base := time.Now().UTC().Add(-10 * time.Minute)
	for i := 0; i < 10; i++ {
		e := env(t, events.AgentStep, i)
		e.OccurredAt = base.Add(time.Duration(i) * time.Minute)
		if _, err := es.Append(ctx, e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	// Older than base+6m are seqs 1..6; keepLast=2 additionally protects 9,10.
	deleted, err := es.PruneEvents(ctx, base.Add(6*time.Minute), 2)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if deleted != 6 {
		t.Fatalf("deleted %d, want 6", deleted)
	}
	rest, err := es.After(ctx, 0, events.Filter{}, 100)
	if err != nil {
		t.Fatalf("after prune: %v", err)
	}
	if len(rest) != 4 || rest[0].Seq != 7 {
		t.Fatalf("survivors = %v, want seqs 7..10", seqsOf(rest))
	}

	// Pruning again changes nothing: age cut and keepLast agree the tail stays.
	if n, err := es.PruneEvents(ctx, base.Add(6*time.Minute), 2); err != nil || n != 0 {
		t.Fatalf("second prune = %d (%v), want 0", n, err)
	}
}

// TestEventStoreHubResumeRoundTrip is the docs/spec/08 "Event stream
// reliability" acceptance path end-to-end at the storage layer: 3,000 events
// through the hub into SQLite, a ring half that size, and a client resuming
// from seq 10 — which the ring has long since evicted — receiving 11..3000
// exactly once, in order, from the durable log (docs/spec/12: "SSE reconnect
// and Last-Event-ID; history gap" must be a *test*, not a comment).
func TestEventStoreHubResumeRoundTrip(t *testing.T) {
	ctx := testCtx(t)
	_, es := newEventTestStore(t)

	const total = 3000
	const ring = 512
	hub := events.New(events.WithStore(es), events.WithRingSize(ring))

	for i := 1; i <= total; i++ {
		if _, err := hub.Publish(ctx, events.Envelope{
			Name: events.AgentStep,
			Data: json.RawMessage(fmt.Sprintf(`{"n": %d}`, i)),
		}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	sub, err := hub.Subscribe(ctx, events.Filter{}, 10)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer sub.Close()

	want := int64(11)
	got := 0
	for {
		e, ok, err := sub.Next(ctx, 5*time.Second)
		if err != nil {
			t.Fatalf("next at %d: %v", want, err)
		}
		if !ok {
			t.Fatalf("timeout waiting for seq %d after %d events", want, got)
		}
		if e.Name == events.EventsResyncRequired {
			t.Fatalf("resync notice on a fully retained log: %s", e.Data)
		}
		if e.Seq != want {
			t.Fatalf("out-of-order or aliased delivery: got seq %d, want %d", e.Seq, want)
		}
		got++
		want++
		if want > total {
			break
		}
	}
	if got != total-10 {
		t.Fatalf("delivered %d events, want %d", got, total-10)
	}
	// Nothing left inside the retained range: the next read must idle.
	if e, ok, err := sub.Next(ctx, 100*time.Millisecond); ok {
		t.Fatalf("unexpected extra event seq %d (%v)", e.Seq, err)
	}
}

func seqsOf(evs []events.Envelope) []int64 {
	out := make([]int64, len(evs))
	for i, e := range evs {
		out[i] = e.Seq
	}
	return out
}
