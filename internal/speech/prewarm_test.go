package speech

import (
	"context"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers/tts"
)

// TestBackgroundYieldsToCommitted is the admission half of FR-11: with the
// generation pool saturated, a queued background prewarm (PriorityBackground)
// never pushes a newly committed line (PriorityCurrent) behind it — when the
// in-flight job releases the slot, the committed line is admitted first.
// The limiter never preempts a RUNNING job (the provider contract), so the
// guarantee is exactly this admission order, which the test asserts
// deterministically.
func TestBackgroundYieldsToCommitted(t *testing.T) {
	ctx := context.Background()
	c, _, _ := newTestCache(t, 1) // one slot: everything queues

	k1, k2, k3 := testKey(t, "a"), testKey(t, "b"), testKey(t, "c")

	releaseFirst := make(chan struct{})
	firstStarted := make(chan struct{})
	firstGen := func(_ context.Context) (tts.Audio, error) {
		close(firstStarted)
		<-releaseFirst
		return toneAudio(), nil
	}

	var order []string
	mark := func(name string, f func(context.Context) (tts.Audio, error)) func(context.Context) (tts.Audio, error) {
		return func(ctx context.Context) (tts.Audio, error) {
			order = append(order, name)
			return f(ctx)
		}
	}

	// Job 1 (an in-flight generation, e.g. the just-committed line's own
	// synthesis) holds the slot.
	done1 := make(chan struct{})
	go func() {
		defer close(done1)
		if _, err := c.GetOrGenerate(ctx, k1, PriorityCurrent, firstGen); err != nil {
			t.Errorf("job1: %v", err)
		}
	}()
	<-firstStarted

	// A background prewarm queues behind job 1, then a committed line
	// arrives while both wait.
	done2 := make(chan struct{})
	go func() {
		defer close(done2)
		if _, err := c.GetOrGenerate(ctx, k2, PriorityBackground, mark("prewarm", func(context.Context) (tts.Audio, error) {
			return toneAudio(), nil
		})); err != nil {
			t.Errorf("prewarm: %v", err)
		}
	}()
	done3 := make(chan struct{})
	go func() {
		defer close(done3)
		if _, err := c.GetOrGenerate(ctx, k3, PriorityCurrent, mark("committed", func(context.Context) (tts.Audio, error) {
			return toneAudio(), nil
		})); err != nil {
			t.Errorf("committed: %v", err)
		}
	}()

	// Let both waiters register in the queue (the limiter reports them),
	// then release job 1's slot: whichever waiter the release wakes first
	// is the admission decision under test.
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats().PendingGen != 2 {
		if time.Now().After(deadline) {
			t.Fatalf("the queued waiters never reached %d (pending %d): the test cannot assert the admission order", 2, c.Stats().PendingGen)
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseFirst)
	select {
	case <-done1:
	case <-time.After(10 * time.Second):
		t.Fatal("job 1 did not finish after its release")
	}
	<-done2
	<-done3

	// The slot released to the committed line BEFORE the queued prewarm.
	want := []string{"committed", "prewarm"}
	if len(order) != len(want) {
		t.Fatalf("admission order = %v, want %v (the committed line must not queue behind a prewarm)", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("admission order = %v, want %v (the committed line must not queue behind a prewarm)", order, want)
		}
	}
}

// TestBackgroundPrewarmCancelsAtSource is the other half of FR-11 at the
// cache boundary: a background prewarm whose context is cancelled (the fence
// moved) stops at the source — the generation is not run to completion and
// its audio is not committed to the cache.
func TestBackgroundPrewarmCancelsAtSource(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, _, _ := newTestCache(t, 1)
	key := testKey(t, "prewarm")

	entered := make(chan struct{})
	gen := func(ctx context.Context) (tts.Audio, error) {
		close(entered)
		<-ctx.Done()
		return tts.Audio{}, ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		_, err := c.GetOrGenerate(ctx, key, PriorityBackground, gen)
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("the cancelled prewarm returned success; want its context error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the cancelled prewarm did not return")
	}

	// Nothing was committed: the prewarmed key is unknown to the cache, so a
	// later commit of the line pays the round trip (the documented
	// degradation).
	if _, ok, err := c.Get(context.Background(), key, false); err != nil || ok {
		t.Fatalf("the cancelled prewarm left a cache entry (ok=%v, err=%v); want nothing", ok, err)
	}
}
