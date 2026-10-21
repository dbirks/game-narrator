package jobs

// The visible foreground pause (ADR-0119, issue #27): SetForegroundBusy
// stamps when the pause began, and ForegroundPause reports the pause with
// the queued count read from the store — the state the API surfaces on the
// status snapshot and the job list, so a queue held by a live session is
// never a silent one.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/store"
)

// TestForegroundPauseState covers the stamp/clear discipline and the queued
// count: not paused (the idle shape), paused with a stamp and the store's
// count, a repeated true that must not move the clock, and the clear that
// drops both.
func TestForegroundPauseState(t *testing.T) {
	_, js := openMigrated(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	r := fastRunner(t, js, nil)
	if err := r.Register("prewarm", func(context.Context, Job, ProgressFn) (json.RawMessage, error) {
		return json.RawMessage(`{}`), nil
	}, RegisterOpts{Concurrency: 1}); err != nil {
		t.Fatal(err)
	}
	// Idle: no pause, no stamp, no count — the shape a box that is not
	// pausing anything reports (the runner is not started yet: nothing could
	// be claimed anyway, so the idle read is the same one the API would serve).
	idle := r.ForegroundPause(ctx)
	if idle.Paused || !idle.Since.IsZero() || idle.Queued != 0 {
		t.Fatalf("idle ForegroundPause = %+v, want not paused with a zero stamp and count", idle)
	}

	// The pause is set before the runner starts (the session runtime's
	// order: busy at session start), so no job can race the stamp: three
	// jobs sit on the queue the pause opens on.
	before := time.Now()
	r.SetForegroundBusy(true) // a live session owns the appliance
	mustStart(t, r, ctx)
	_, _, _ = js.Enqueue(ctx, store.EnqueueParams{Kind: "prewarm"})
	_, _, _ = js.Enqueue(ctx, store.EnqueueParams{Kind: "prewarm"})
	_, _, _ = js.Enqueue(ctx, store.EnqueueParams{Kind: "prewarm"})
	paused := r.ForegroundPause(ctx)
	if !paused.Paused {
		t.Fatalf("ForegroundPause = %+v, want paused", paused)
	}
	if paused.Since.IsZero() || paused.Since.Before(before.Add(-time.Second)) {
		t.Fatalf("pause since = %v, want a stamp at or after %v", paused.Since, before)
	}
	if paused.Queued != 3 {
		t.Fatalf("paused queued count = %d, want 3 (the store's read, not a kept counter)", paused.Queued)
	}

	// A repeated true holds the same pause: the second session that starts
	// while one is already live does not move the clock.
	first := paused.Since
	time.Sleep(50 * time.Millisecond)
	r.SetForegroundBusy(true)
	again := r.ForegroundPause(ctx)
	if again.Since != first {
		t.Fatalf("repeated SetForegroundBusy(true) moved the stamp: %v -> %v, want the first stamp kept", first, again.Since)
	}

	// The count is a read at call time: a job enqueued into the held queue
	// shows up in the next report.
	_, _, _ = js.Enqueue(ctx, store.EnqueueParams{Kind: "prewarm"})
	grown := r.ForegroundPause(ctx)
	if grown.Queued != 4 {
		t.Fatalf("queued count after an enqueue = %d, want 4", grown.Queued)
	}

	// The session ends: the flag and the stamp clear together.
	r.SetForegroundBusy(false)
	cleared := r.ForegroundPause(ctx)
	if cleared.Paused || !cleared.Since.IsZero() || cleared.Queued != 0 {
		t.Fatalf("after SetForegroundBusy(false) = %+v, want not paused with a cleared stamp", cleared)
	}
}
