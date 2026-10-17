package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/providers"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestPriorityLiveBeforeBackground: with one slot held by a Background
// caller, a Live caller queued *after* an earlier Background waiter is
// served first (docs/spec/06: background generation receives lower priority
// than runtime).
func TestPriorityLiveBeforeBackground(t *testing.T) {
	h := newHarness(t, Options{Concurrency: map[providers.Role]int{fast: 1}}, fast)
	ctx := context.Background()

	var mu sync.Mutex
	var order []string
	enter := func(tag string) { mu.Lock(); order = append(order, tag); mu.Unlock() }

	// A Background holder takes the single slot and blocks inside the call.
	holderEntered := make(chan struct{})
	releaseHolder := make(chan struct{})
	go Do(ctx, h.r, fast, CallOptions{Priority: PriorityBackground}, func(context.Context, Target) (string, error) {
		close(holderEntered)
		<-releaseHolder
		enter("holder")
		return "", nil
	})
	<-holderEntered

	// A Background waiter queues next.
	bgDone := make(chan struct{})
	go Do(ctx, h.r, fast, CallOptions{Priority: PriorityBackground}, func(context.Context, Target) (string, error) {
		enter("bg-waiter")
		close(bgDone)
		return "", nil
	})
	waitFor(t, "background waiter queued", func() bool {
		_, bg := h.r.roles[fast].slots.waiting()
		return bg == 1
	})

	// A Live waiter queues behind the background waiter.
	liveDone := make(chan struct{})
	go Do(ctx, h.r, fast, CallOptions{Priority: PriorityLive}, func(context.Context, Target) (string, error) {
		enter("live-waiter")
		close(liveDone)
		return "", nil
	})
	waitFor(t, "live waiter queued", func() bool {
		l, bg := h.r.roles[fast].slots.waiting()
		return l == 1 && bg == 1
	})

	close(releaseHolder)
	select {
	case <-liveDone:
	case <-time.After(2 * time.Second):
		t.Fatal("live waiter was not served while a background waiter was queued ahead of it")
	}
	waitFor(t, "background waiter served", func() bool {
		select {
		case <-bgDone:
			return true
		default:
			return false
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("order = %v", order)
	}
	liveIdx, bgIdx := -1, -1
	for i, tag := range order {
		switch tag {
		case "live-waiter":
			liveIdx = i
		case "bg-waiter":
			bgIdx = i
		}
	}
	if liveIdx < 0 || bgIdx < 0 || liveIdx > bgIdx {
		t.Fatalf("order = %v, want live-waiter before bg-waiter", order)
	}
}

// TestBackgroundPause: SetBackgroundPaused blocks Background callers — even
// when a slot is free — while Live callers proceed; unpausing releases the
// queued background work (docs/spec/01 "Runtime and setup isolation").
func TestBackgroundPause(t *testing.T) {
	h := newHarness(t, Options{Concurrency: map[providers.Role]int{fast: 1}}, fast)
	ctx := context.Background()

	var mu sync.Mutex
	var order []string
	enter := func(tag string) { mu.Lock(); order = append(order, tag); mu.Unlock() }

	// A (background) holds the slot.
	aEnter := make(chan struct{})
	aRelease := make(chan struct{})
	go Do(ctx, h.r, fast, CallOptions{Priority: PriorityBackground}, func(context.Context, Target) (string, error) {
		close(aEnter)
		<-aRelease
		enter("A")
		return "", nil
	})
	<-aEnter

	h.r.SetBackgroundPaused(true)

	// B (background) queues and must not run even after A releases.
	bEnter := make(chan struct{})
	bDone := make(chan struct{})
	go Do(ctx, h.r, fast, CallOptions{Priority: PriorityBackground}, func(context.Context, Target) (string, error) {
		close(bEnter)
		enter("B")
		close(bDone)
		return "", nil
	})
	waitFor(t, "background caller queued", func() bool {
		_, bg := h.r.roles[fast].slots.waiting()
		return bg == 1
	})

	close(aRelease) // slot becomes free, but the background stays paused.

	// C (live) must proceed while the pause is active.
	cDone := make(chan struct{})
	go Do(ctx, h.r, fast, CallOptions{Priority: PriorityLive}, func(context.Context, Target) (string, error) {
		enter("C")
		close(cDone)
		return "", nil
	})
	waitFor(t, "live caller served while background paused", func() bool {
		select {
		case <-cDone:
			return true
		default:
			return false
		}
	})
	select {
	case <-bEnter:
		t.Fatal("background caller ran while paused")
	default:
	}

	h.r.SetBackgroundPaused(false)
	waitFor(t, "paused background caller served after unpause", func() bool {
		select {
		case <-bDone:
			return true
		default:
			return false
		}
	})

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 3 || order[0] != "A" || order[1] != "C" || order[2] != "B" {
		t.Fatalf("order = %v, want [A C B]", order)
	}
}

// TestPerAttemptTimeout: the role timeout bounds each attempt (docs/spec/06
// "Timeout and cancellation defaults"), so a hung provider cannot consume
// the whole multi-attempt budget in one go.
func TestPerAttemptTimeout(t *testing.T) {
	reg := providers.NewRegistry(map[providers.Role]providers.RoleConfig{
		fast: {Role: fast, Protocol: providers.ProtocolOpenAIChat,
			BaseURL: "http://example.invalid/v1", Model: "m", Timeout: 60 * time.Millisecond},
	})
	var sleeps []time.Duration
	r := New(reg, Options{
		Now:   time.Now,
		Sleep: func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil },
	})
	attempts := 0
	deadlineTooLong := 0
	_, err := Do(context.Background(), r, fast, CallOptions{Idempotent: true}, func(ctx context.Context, _ Target) (string, error) {
		if dl, ok := ctx.Deadline(); ok && time.Until(dl) > 100*time.Millisecond {
			deadlineTooLong++
		}
		select {
		case <-time.After(500 * time.Millisecond):
			return "late", nil
		case <-ctx.Done():
			attempts++
			return "", providers.NewError(providers.KindTimeout, 0, "per-attempt deadline exceeded")
		}
	})
	isKind(t, err, providers.KindTimeout)
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (default MaxAttempts)", attempts)
	}
	if deadlineTooLong != 0 {
		t.Fatalf("attempts with an over-long deadline = %d, want 0", deadlineTooLong)
	}
	if len(sleeps) != 2 {
		t.Fatalf("backoffs = %d, want 2", len(sleeps))
	}
}

// TestConcurrentCallersRaceFree: 64 concurrent callers hammer one role with
// the default concurrency of 2, every caller transiently failing twice
// before succeeding. This is the -race harness for the breaker, slots,
// health counters and latency ring. The breaker threshold is raised so the
// aggregate failures (128) cannot fail-fast any caller; the final clean
// call clears the transient streak.
func TestConcurrentCallersRaceFree(t *testing.T) {
	h := newHarness(t, Options{BreakerThreshold: 1000}, fast)
	ctx := context.Background()

	const n = 64
	errs := make([]error, n)
	var wg sync.WaitGroup
	for g := 0; g < n; g++ {
		f := &fakeCalls{script: []error{transientErr(), transientErr(), nil}}
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			res, err := Do(ctx, h.r, fast, CallOptions{Idempotent: true}, f.fn())
			if err != nil {
				errs[g] = err
				return
			}
			if res != "ok" {
				errs[g] = errors.New("unexpected result")
			}
		}(g)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("caller %d: %v", i, e)
		}
	}
	// A final clean call leaves the role ok (the streak is cleared).
	res, err := Do(ctx, h.r, fast, CallOptions{}, func(context.Context, Target) (string, error) {
		return "final", nil
	})
	if res != "final" || err != nil {
		t.Fatalf("final call = %q, %v", res, err)
	}
	ph := h.r.Health()[fast]
	if ph.Calls != int64(n)*3+1 {
		t.Fatalf("Calls = %d, want %d", ph.Calls, n*3+1)
	}
	if ph.Failures != int64(n)*2 {
		t.Fatalf("Failures = %d, want %d", ph.Failures, n*2)
	}
	if ph.Retries != int64(n)*2 {
		t.Fatalf("Retries = %d, want %d", ph.Retries, n*2)
	}
	if ph.State != StateOK {
		t.Fatalf("state = %s, want ok after recovery", ph.State)
	}
	if ph.P95 < 0 {
		t.Fatalf("P95 = %s", ph.P95)
	}
}
