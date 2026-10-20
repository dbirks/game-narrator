package logbuf

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

func TestRingKeepsTheNewestUpToCapacity(t *testing.T) {
	r := New(3)
	log := slog.New(r.Handler())
	for i := 0; i < 10; i++ {
		log.Info(fmt.Sprintf("line %d", i), "seq", i)
	}
	tail := r.Tail(0)
	if len(tail) != 3 {
		t.Fatalf("the ring keeps its capacity, got %d records", len(tail))
	}
	// Oldest first, newest last: the cap evicted the oldest.
	if tail[0].Msg != "line 7" || tail[2].Msg != "line 9" {
		t.Fatalf("expected the three newest lines oldest-first, got %q..%q", tail[0].Msg, tail[2].Msg)
	}
	if tail[2].Attrs["seq"] != "9" {
		t.Fatalf("attribute rendering lost the value: %v", tail[2].Attrs)
	}
}

func TestRingTailLimitsAndOrder(t *testing.T) {
	r := New(10)
	log := slog.New(r.Handler())
	for i := 0; i < 5; i++ {
		log.Info(fmt.Sprintf("m%d", i))
	}
	two := r.Tail(2)
	if len(two) != 2 || two[0].Msg != "m3" || two[1].Msg != "m4" {
		t.Fatalf("Tail(2) must be the two newest, oldest first: %v", two)
	}
	all := r.Tail(99)
	if len(all) != 5 {
		t.Fatalf("a limit above the ring length asks for the whole ring, got %d", len(all))
	}
}

func TestRingIsConcurrentSafe(t *testing.T) {
	r := New(64)
	log := slog.New(r.Handler())
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				log.Info("concurrent line")
			}
		}()
	}
	wg.Wait()
	if got := len(r.Tail(0)); got > 64 {
		t.Fatalf("the ring overflowed its capacity under concurrency: %d > 64", got)
	}
}

func TestFanoutForwardsToEveryHandlerAndRespectsLevels(t *testing.T) {
	a := New(16)
	b := New(16)
	fan := slog.New(Fanout(a.Handler(), b.Handler()))
	fan.Info("to both")
	fan.Error("also to both")
	if len(a.Tail(0)) != 2 || len(b.Tail(0)) != 2 {
		t.Fatalf("fanout must forward every record to every handler: a=%d b=%d", len(a.Tail(0)), len(b.Tail(0)))
	}
	if a.Tail(1)[0].Msg != "also to both" {
		t.Fatalf("order must be preserved through the fanout: %q", a.Tail(1)[0].Msg)
	}
}
