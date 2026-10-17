package speech_test

import (
	"context"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/speech"
	"github.com/dbirks/game-narrator/internal/speech/dummy"
)

// chunkSrc is a scripted speech.ChunkSource: `chunks` chunks of `chunkMs`
// each, one chunk delivered every `pace`. It counts Next() entries
// (calls) and completed deliveries (delivered) so the test can assert that
// a cancelled stream STOPS being read: no new chunk is pulled after the
// fence switches / Cancel lands, and the partial audio never plays
// (docs/spec/12 "TTS partly streams before cancellation": audible playback
// stops even if the TTS backend ignores cancellation; late audio never plays).
type chunkSrc struct {
	rate, ch, bits int
	chunkMs, pace  time.Duration
	chunks         int

	mu        sync.Mutex
	calls     int // Next entries
	delivered int // chunks actually handed back
	nextCalls int64
}

func (s *chunkSrc) Next(ctx context.Context) (speech.Chunk, error) {
	s.mu.Lock()
	s.calls++
	if s.calls > s.chunks {
		s.mu.Unlock()
		// End of stream: the scheduler breaks on io.EOF (sink.go contract).
		return speech.Chunk{Err: io.EOF}, nil
	}
	s.mu.Unlock()
	// Deliver on a pace; respect ctx so a cancelled stream wakes Next fast.
	select {
	case <-ctx.Done():
		return speech.Chunk{}, ctx.Err()
	case <-time.After(s.pace):
	}
	if ctx.Err() != nil {
		return speech.Chunk{}, ctx.Err()
	}
	s.mu.Lock()
	s.delivered++
	s.mu.Unlock()
	atomic.AddInt64(&s.nextCalls, 1)

	frames := int(float64(s.rate) * s.chunkMs.Seconds())
	pcm := make([]byte, frames*s.ch*s.bits/8)
	for i := 0; i < frames; i++ {
		v := int16(0.25 * 32767 * math.Sin(2*math.Pi*440*float64(i)/float64(s.rate)))
		for c := 0; c < s.ch; c++ {
			off := (i*s.ch + c) * 2
			pcm[off] = byte(v)
			pcm[off+1] = byte(v >> 8)
		}
	}
	return speech.Chunk{PCM: pcm}, nil
}

func (s *chunkSrc) SampleRate() int { return s.rate }
func (s *chunkSrc) Channels() int   { return s.ch }
func (s *chunkSrc) Bits() int       { return s.bits }

func (s *chunkSrc) counts() (calls, delivered int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.delivered
}

func (s *chunkSrc) waited(t *testing.T, cond func() bool, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", d)
}

// TestSchedulerStreamCancelPartway is the docs/spec/12 failure-injection case
// "TTS partly streams before cancellation": a chunked source is cancelled
// mid-way; the scheduler stops pulling chunks (the source is not read again),
// the partial audio never plays, and the drop is evented as a failure.
func TestSchedulerStreamCancelPartway(t *testing.T) {
	src := &chunkSrc{rate: 22050, ch: 1, bits: 16, chunkMs: 50 * time.Millisecond, pace: 20 * time.Millisecond, chunks: 20}
	sink := dummy.New(5*time.Millisecond, 5*time.Millisecond)
	pub := &recPub{}
	fence := &testFence{}
	fence.cur.Store(1) // generation 1 is current
	sched := speech.NewScheduler(sink, fence, pub, speech.Options{Clock: time.Now})
	ctx := context.Background()
	sched.Start(ctx)
	defer sched.Stop()

	sched.Submit(speech.UtteranceReady{
		SessionID: "s1", Generation: 1, UtteranceID: "u-stream",
		Priority: speech.PriorityCurrent, Source: src,
		ExpectedDuration: time.Duration(src.chunks) * src.chunkMs,
	})

	// Let a few chunks stream (partial progress), then cancel mid-stream.
	src.waited(t, func() bool { _, d := src.counts(); return d >= 3 }, 2*time.Second)
	callsAtCancel, deliveredAtCancel := src.counts()

	cancelAt := time.Now()
	fence.cur.Store(2)         // generation 1 is now stale
	sched.Cancel(1, "advance") // explicit cancel, same tick

	// The drop must be evented as a failed speech.ended.
	src.waited(t, func() bool {
		for _, e := range pub.byName(events.SpeechEnded) {
			if e.Data != nil && containsAll(string(e.Data), `"result":"failed"`) {
				return true
			}
		}
		return false
	}, 2*time.Second)
	_ = cancelAt

	// 1) No audio ever started (the partial stream was fenced before Play).
	for _, r := range sink.Records() {
		if !r.Started.IsZero() {
			t.Fatalf("stale streamed generation played (started %v): late audio must never play", r.Started)
		}
	}

	// 2) The source STOPS being read after cancellation: give the loop time,
	//    then the call count must be frozen (no new Next entries) and the
	//    stream must be incomplete (it was cut mid-way, not drained).
	time.Sleep(150 * time.Millisecond)
	callsNow, deliveredNow := src.counts()
	if callsNow > callsAtCancel+1 { // +1: at most one in-flight Next wakes up
		t.Fatalf("source kept being read after cancellation: Next calls %d → %d (cancellation was %v)",
			callsAtCancel, callsNow, time.Since(cancelAt))
	}
	if deliveredNow >= src.chunks {
		t.Fatalf("stream drained to the end after cancellation (delivered %d/%d): the partial stream must be cut", deliveredNow, src.chunks)
	}
	if deliveredAtCancel < 1 {
		t.Fatal("scenario produced no partial progress before the cancel; test is vacuous")
	}
	_ = deliveredAtCancel

	// 3) Event evidence: the cancel was reported and the drop failed with a
	//    stream reason (docs/spec/11: assert events, not just silence).
	if len(pub.byName(events.SpeechCancelled)) == 0 {
		t.Error("no speech.cancelled event for the cancelled stream")
	}
	t.Logf("stream cancel: %d/%d chunks delivered before cancel, source frozen at %d Next calls (want ≤ %d)",
		deliveredNow, src.chunks, callsNow, callsAtCancel+1)
}

// TestSchedulerStreamCompletesClean is the streaming happy path: a fenced
// chunked source that stays current assembles into a validated WAV and plays
// (docs/spec/07 "support chunked playback for valid streaming TTS while
// fencing each chunk").
func TestSchedulerStreamCompletesClean(t *testing.T) {
	src := &chunkSrc{rate: 22050, ch: 1, bits: 16, chunkMs: 50 * time.Millisecond, pace: 10 * time.Millisecond, chunks: 4}
	sink := dummy.New(5*time.Millisecond, 5*time.Millisecond)
	pub := &recPub{}
	fence := &testFence{}
	fence.cur.Store(7)
	sched := speech.NewScheduler(sink, fence, pub, speech.Options{Clock: time.Now})
	ctx := context.Background()
	sched.Start(ctx)
	defer sched.Stop()

	sched.Submit(speech.UtteranceReady{
		SessionID: "s1", Generation: 7, UtteranceID: "u-clean",
		Priority: speech.PriorityCurrent, Source: src,
		ExpectedDuration: time.Duration(src.chunks) * src.chunkMs,
	})

	// Wait for the playback to finish on its own (200 ms of tone + margins).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(pub.byName(events.SpeechEnded)) >= 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var ended events.Envelope
	endedSeen := false
	for _, e := range pub.byName(events.SpeechEnded) {
		ended = e
		endedSeen = true
	}
	if len(pub.byName(events.SpeechStarted)) != 1 {
		t.Fatalf("streamed utterance did not start (events: %d started)", len(pub.byName(events.SpeechStarted)))
	}
	if !endedSeen || !containsAll(string(ended.Data), `"result":"ended"`) {
		t.Fatalf("clean stream must end with result=ended, got %s", ended.Data)
	}
	recs := sink.Records()
	if len(recs) != 1 || recs[0].Started.IsZero() || recs[0].Interrupted {
		t.Fatalf("clean stream: unexpected sink records %+v", recs)
	}
	if got := recs[0].Ended.Sub(recs[0].Started); got < 150*time.Millisecond || got > 800*time.Millisecond {
		t.Fatalf("clean stream played %v, want ≈200ms (± device pacing)", got)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
