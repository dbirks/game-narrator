package e2e

// The developer dev-events REST page is the durable event log the
// developer UI polls mid-session (ADR 0024/0053): the session runtime and
// the speech scheduler persist into it while the inspector pages it. The
// page cursor contract (OpenAPI: "since_seq: first seq to include",
// "next_seq: pass as since_seq to continue") must be race-free — a row
// appended between two page reads exactly at the previous page's
// last_seq+1 must appear in the next page, not be skipped. The live
// per-character run (task per-character-live) lost ~half its session rows
// this way (including speech.ended), stalling the run's own advance driver
// on the 12-minute deadline; these in-process regressions keep the route
// honest without Dolphin or providers.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/api"
	"github.com/dbirks/game-narrator/internal/auth"
	"github.com/dbirks/game-narrator/internal/bootstrap"
	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/jobs"
	"github.com/dbirks/game-narrator/internal/runtime"
	"github.com/dbirks/game-narrator/internal/store"
)

// newDevEventsProbeServer wires the same store + hub + jobs runner + API
// server the live e2e tests build, minus the model providers and Dolphin:
// the dev-events route needs only the auth, the session runtime (the
// non-nil check) and the durable event store.
func newDevEventsProbeServer(t *testing.T, ctx context.Context) (http.Handler, *store.Store, *events.Hub, string) {
	t.Helper()
	disc := slog.New(slog.DiscardHandler)
	st, err := store.Open(ctx, store.DBPath(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	hub := events.New(events.WithStore(store.NewEventStore(st)), events.WithLogger(disc))
	t.Cleanup(func() { _ = hub.Close() })
	js := store.NewJobsStore(st)
	runner, err := jobs.NewRunner(js, hub, disc, jobs.WithPollInterval(50*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	stop := context.Background()
	t.Cleanup(func() { _ = runner.Stop(stop) })
	if err := runner.Start(ctx); err != nil {
		t.Fatal(err)
	}

	authSvc := auth.NewService(store.NewAuthStore(st), auth.WithPasswordCost(1000), auth.WithLogger(disc))
	if _, err := authSvc.Setup(ctx, "probe-admin", e2ePassword, "127.0.0.1:1", true); err != nil {
		t.Fatal(err)
	}
	dev, err := authSvc.CreateUser(ctx, "probe-dev", e2ePassword, auth.RoleDeveloper, nil, "probe")
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := authSvc.CreateToken(ctx, nil, auth.CreateTokenParams{
		UserID: dev.ID, Name: "probe", Role: auth.RoleDeveloper,
	}, "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := runtime.New(runtime.Options{
		Store:      st,
		Hub:        hub,
		Jobs:       runner,
		AssetsRoot: t.TempDir(),
		Log:        disc,
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(bootstrap.BuildInfo{Version: "dev-events-probe"}, time.Now(), disc,
		api.WithAuth(authSvc, store.NewAuthStore(st)),
		api.WithStore(st),
		api.WithJobs(js),
		api.WithEvents(hub),
		api.WithSessions(sessions),
		api.WithDevEvents(store.NewEventStore(st)),
	).Handler()
	return srv, st, hub, token
}

// getDevEventsPage pages the durable log through the developer route the
// way the inspector polls it: strict cursor, one page per call.
func getDevEventsPage(t *testing.T, srv http.Handler, token, sessionID string, sinceSeq int64) []events.Envelope {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/v1/dev/events?session_id=%s&since_seq=%d&limit=1000", sessionID, sinceSeq), nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET dev events = %d: %s", rec.Code, rec.Body.String())
	}
	var page struct {
		Events  []events.Envelope `json:"events"`
		NextSeq int64             `json:"next_seq"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("dev events page decode: %v", err)
	}
	return page.Events
}

// TestDevEventsPageBoundaryNoSkip is the deterministic half of the
// pagination regression: page one, append the row at exactly
// last_seq+1, page again with the page's own next_seq — the appended row
// must be in the second page (the old off-by-one skipped exactly it).
func TestDevEventsPageBoundaryNoSkip(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	srv, _, hub, token := newDevEventsProbeServer(t, ctx)
	sessID := games.NewID()

	publish := func(i int) {
		t.Helper()
		if _, err := hub.Publish(ctx, events.Envelope{
			SessionID: sessID, Name: events.TextObserved,
			TraceID: fmt.Sprintf("boundary-%d", i), Data: []byte(fmt.Sprintf(`{"i":%d}`, i)),
		}); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	for i := 0; i < 12; i++ {
		publish(i)
	}
	first := getDevEventsPage(t, srv, token, sessID, 0)
	if len(first) != 12 {
		t.Fatalf("first page = %d rows, want 12", len(first))
	}
	last := first[len(first)-1]
	// The racing row: committed AFTER the first page's read, at exactly
	// last.Seq+1 — the seq the page's own next_seq points at.
	publish(12)
	second := getDevEventsPage(t, srv, token, sessID, last.Seq+1)
	if len(second) != 1 || second[0].Seq != last.Seq+1 {
		t.Fatalf("second page (since_seq=%d) = %+v; the row appended at exactly last_seq+1 was skipped by the page cursor", last.Seq+1, second)
	}
}

// TestDevEventsPageConcurrentLoadNoLoss is the load half: a publisher
// writing continuously while the inspector's 500 ms cursor polls run in
// parallel; every persisted row must reach the polled stream (the live
// run lost ~half under this pattern before the cursor fix).
func TestDevEventsPageConcurrentLoadNoLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	srv, st, hub, token := newDevEventsProbeServer(t, ctx)
	sessID := games.NewID()
	es := store.NewEventStore(st)

	const total = 300
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < total; i++ {
			if _, err := hub.Publish(ctx, events.Envelope{
				SessionID: sessID, Name: events.TextObserved,
				TraceID: fmt.Sprintf("load-%03d", i),
				Data:    []byte(fmt.Sprintf(`{"i":%d}`, i)),
			}); err != nil {
				t.Errorf("publish %d: %v", i, err)
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	var (
		accum  []events.Envelope
		cursor int64
	)
	pollOne := func() {
		req := httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/api/v1/dev/events?session_id=%s&since_seq=%d&limit=1000", sessID, cursor), nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("probe GET dev events = %d: %s", rec.Code, rec.Body.String())
		}
		var page struct {
			Events  []events.Envelope `json:"events"`
			NextSeq int64             `json:"next_seq"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
			t.Fatalf("page decode: %v", err)
		}
		accum = append(accum, page.Events...)
		if len(page.Events) > 0 {
			cursor = page.NextSeq
		}
	}
	deadline := time.NewTimer(120 * time.Second)
	poll := time.NewTicker(500 * time.Millisecond)
	defer poll.Stop()
loop:
	for {
		select {
		case <-deadline.C:
			t.Fatal("probe polling deadline")
		case <-done:
			for i := 0; i < 4; i++ { // drain the tail
				pollOne()
				time.Sleep(100 * time.Millisecond)
			}
			deadline.Stop()
			poll.Stop()
			break loop
		case <-poll.C:
			pollOne()
		}
	}

	seen := map[int64]bool{}
	for _, e := range accum {
		seen[e.Seq] = true
	}
	direct, err := es.After(ctx, 0, events.Filter{SessionID: sessID}, 10000)
	if err != nil {
		t.Fatal(err)
	}
	var missing []int64
	for _, e := range direct {
		if !seen[e.Seq] {
			missing = append(missing, e.Seq)
		}
	}
	t.Logf("dev-events: published=%d route-accumulated=%d direct-store=%d missing=%d",
		total, len(accum), len(direct), len(missing))
	if len(missing) > 0 {
		head := missing
		if len(head) > 20 {
			head = head[:20]
		}
		t.Fatalf("the dev-events route dropped %d of %d rows under concurrent load (first missing seqs: %v); the page cursor must not skip a row appended between page reads",
			len(missing), len(direct), head)
	}
}
