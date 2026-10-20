package speech

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// UtteranceReady is the scheduler's input (docs/spec/07 "Audio scheduler":
// "Input UtteranceReady with session/generation token, cancel deadline,
// expected text duration, voice ID, priority"). Exactly one of WAV or Source
// is set: WAV for cached (buffered) utterances — the preferred path — and
// Source for valid streaming TTS, fenced at every chunk boundary.
type UtteranceReady struct {
	SessionID   string
	Generation  int64
	UtteranceID string
	Priority    Priority
	// WAV is a complete, validated utterance (preferred).
	WAV []byte
	// Source is a streaming TTS source; when non-nil it replaces WAV.
	Source ChunkSource
	// ExpectedDuration is what the rendering plan expected (UI + QA signal).
	ExpectedDuration time.Duration
}

// Publisher is the event boundary the scheduler reports playback outcomes on
// (speech.started / speech.ended / speech.cancelled). The session event hub
// implements it; tests use a recorder. NOTE: the session reducer also
// publishes speech.* events for TTS lifecycle (SpeechMsg ready/failed); the
// wiring must make the scheduler the single owner of device-level playback
// events to avoid double emission (ADR-0023).
type Publisher interface {
	Publish(ctx context.Context, e events.Envelope) (events.Envelope, error)
}

// StopTarget is the docs/spec/00 target a cancel must meet: local stop event
// < 200 ms (audio device buffering excluded and reported, docs/spec/12).
const StopTarget = 200 * time.Millisecond

// Defaults for the scheduler.
const (
	DefaultInboxSize    = 16
	DefaultFencePeriod  = 100 * time.Millisecond // "at every chunk boundary" ≈ 100 ms
	DefaultStopFade     = 50 * time.Millisecond  // well inside the 200 ms budget
	DefaultMaxChunkWait = 5 * time.Second        // a stalled TTS stream is a failure, not a wait

	// stopJoinBudget bounds how long shutdown joins the active playback's
	// terminal result (see the loop's ctx.Done branch): a stopped sink must
	// finish its final delivery — the filesink's evidence write included —
	// well inside the bound.
	stopJoinBudget = 2 * time.Second
)

// Scheduler is the generation-fenced audio scheduler. It owns exactly one
// active playback at a time (docs/spec/07 "speech scheduler owns exactly one
// active"), plays only what the fence approves as current, and runs on its
// own goroutine with a bounded inbox so Submit never blocks the caller and
// the session event loop (docs/spec/07: "Audio sink must never block session
// event loop").
//
// Fencing points (docs/spec/01 invariant 2; docs/spec/12 "zero stale audio
// starts"): BEFORE starting (after the pop from the inbox), AT EVERY chunk
// boundary (streaming source), and on a 100 ms ticker during any playback.
// A stale verdict at any point means: stop within StopTarget, emit
// speech.cancelled, and never deliver more samples.
type Scheduler struct {
	sink  AudioSink
	fence Fence
	pub   Publisher

	inboxSize    int
	fencePeriod  time.Duration
	stopFade     time.Duration
	maxChunkWait time.Duration
	clock        func() time.Time

	inbox  chan UtteranceReady
	cancel chan cancelReq

	runCtx    context.Context
	runCancel context.CancelFunc
	started   atomicBool
	stopped   chan struct{}

	mu    sync.Mutex
	stats Stats
	// cancelled remembers explicit Cancel()s so queued items of a cancelled
	// generation are dropped even if the fence has not moved yet.
	cancelled map[int64]string
	stopReqAt map[int64]time.Time
}

type cancelReq struct {
	generation int64
	reason     string
}

// Stats are scheduler decision counters and the last measured stop latency.
type Stats struct {
	Started         int64         `json:"started"`
	Ended           int64         `json:"ended"`
	Failed          int64         `json:"failed"`
	Cancelled       int64         `json:"cancelled"`
	DroppedStale    int64         `json:"dropped_stale"`
	DroppedInbox    int64         `json:"dropped_inbox"`
	LastStopLatency time.Duration `json:"last_stop_latency_ns,string"`
}

// Options tune the scheduler. Zero values take the defaults above.
type Options struct {
	InboxSize    int
	FencePeriod  time.Duration
	StopFade     time.Duration
	MaxChunkWait time.Duration
	Clock        func() time.Time
}

// NewScheduler builds a scheduler. sink, fence and pub must be non-nil.
func NewScheduler(sink AudioSink, fence Fence, pub Publisher, opts Options) *Scheduler {
	if sink == nil || fence == nil || pub == nil {
		panic("speech: NewScheduler requires sink, fence and publisher")
	}
	inboxSize := opts.InboxSize
	if inboxSize <= 0 {
		inboxSize = DefaultInboxSize
	}
	fp := opts.FencePeriod
	if fp <= 0 {
		fp = DefaultFencePeriod
	}
	sf := opts.StopFade
	if sf <= 0 {
		sf = DefaultStopFade
	}
	mc := opts.MaxChunkWait
	if mc <= 0 {
		mc = DefaultMaxChunkWait
	}
	clk := opts.Clock
	if clk == nil {
		clk = time.Now
	}
	return &Scheduler{
		sink:         sink,
		fence:        fence,
		pub:          pub,
		inboxSize:    inboxSize,
		fencePeriod:  fp,
		stopFade:     sf,
		maxChunkWait: mc,
		clock:        clk,
		inbox:        make(chan UtteranceReady, inboxSize),
		cancel:       make(chan cancelReq, 8),
		cancelled:    map[int64]string{},
		stopReqAt:    map[int64]time.Time{},
	}
}

// Start launches the scheduler loop. It is idempotent.
func (s *Scheduler) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started.get() {
		s.mu.Unlock()
		return
	}
	s.started.set(true)
	s.stopped = make(chan struct{})
	runCtx, cancel := context.WithCancel(ctx)
	s.runCtx, s.runCancel = runCtx, cancel
	s.mu.Unlock()
	go s.run(runCtx)
}

// Stop shuts the scheduler down: cancels the loop, stops active playback with
// the stop fade, and waits (bounded: the loop exit plus stopJoinBudget for the
// active playback's terminal result) for the loop to exit. When Stop returns
// nil no delivery is in flight and the sink's terminal artifacts are on disk.
func (s *Scheduler) Stop() error {
	s.mu.Lock()
	if !s.started.get() {
		s.mu.Unlock()
		return nil
	}
	s.runCancel()
	stopped := s.stopped
	s.mu.Unlock()
	select {
	case <-stopped:
		return nil
	case <-time.After(2*time.Second + stopJoinBudget):
		return context.DeadlineExceeded
	}
}

// Submit enqueues u without blocking. It returns false (and publishes a
// speech.ended failed event) when the bounded inbox is full: shedding the
// newest non-critical item is the documented backpressure policy
// (docs/spec/01: bounded queues, explicit backpressure).
func (s *Scheduler) Submit(u UtteranceReady) bool {
	s.mu.Lock()
	if !s.started.get() {
		s.mu.Unlock()
		s.publishEnded("", u.Generation, u.UtteranceID, "failed", "scheduler_stopped", nil)
		return false
	}
	select {
	case s.inbox <- u:
		s.mu.Unlock()
		return true
	default:
		s.mu.Unlock()
		s.statsAdd(&s.stats.DroppedInbox, 1)
		s.publishEnded("", u.Generation, u.UtteranceID, "failed", "inbox_full", nil)
		return false
	}
}

// Cancel asks for generation to stop: active playback of that generation is
// stopped within StopTarget (the fade comes from Options.StopFade) and queued
// items of that generation are dropped. It never blocks.
func (s *Scheduler) Cancel(generation int64, reason string) {
	if reason == "" {
		reason = "cancelled"
	}
	select {
	case s.cancel <- cancelReq{generation: generation, reason: reason}:
	default:
		// Cancel channel full (8 in flight): apply the stop directly. This
		// path keeps cancellation lossless even under a burst.
		s.mu.Lock()
		s.cancelled[generation] = reason
		s.mu.Unlock()
		s.publishCancelled("", generation, reason, "")
	}
}

// Stats returns a copy of the decision counters.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}

func (s *Scheduler) statsAdd(p *int64, n int64) { s.mu.Lock(); *p += n; s.mu.Unlock() }
func (s *Scheduler) setLastStopLatency(d time.Duration) {
	s.mu.Lock()
	s.stats.LastStopLatency = d
	s.mu.Unlock()
}

// publishStarted / publishEnded / publishCancelled emit the event trio.
// Publish errors are counted, not fatal: a hub outage must never wedge the
// audio loop (docs/spec/13 "explain degraded status" — the counter is the
// explanation).
func (s *Scheduler) publishStarted(ctx context.Context, u UtteranceReady, started time.Time) {
	data, _ := json.Marshal(map[string]any{
		"utterance_id":         u.UtteranceID,
		"generation":           u.Generation,
		"priority":             u.Priority.String(),
		"started_at":           started.UTC().Format(time.RFC3339Nano),
		"expected_duration_ms": u.ExpectedDuration.Milliseconds(),
	})
	s.publish(events.SpeechStarted, u.SessionID, data)
	s.statsAdd(&s.stats.Started, 1)
}

func (s *Scheduler) publishEnded(sessionID string, generation int64, utteranceID, result, reason string, res *PlayResult) {
	data := map[string]any{
		"utterance_id": utteranceID,
		"generation":   generation,
		"result":       result,
	}
	if reason != "" {
		data["reason"] = reason
	}
	if res != nil {
		data["started_at"] = res.Started.UTC().Format(time.RFC3339Nano)
		data["ended_at"] = res.Ended.UTC().Format(time.RFC3339Nano)
		data["interrupted"] = res.Interrupted
		if lat := s.stopLatency(generation, res); lat > 0 {
			data["stop_latency_ms"] = lat.Milliseconds()
		}
		// Output latency (ADR-0074, docs/spec/13): how long the device side
		// took to accept the first samples, from the sink's own stamp. Only
		// sinks that report FirstSampleAt (the pipewire sink) populate it; a
		// silent field is the honest "not measured", not a fabricated 0.
		if !res.FirstSampleAt.IsZero() && !res.Started.IsZero() {
			if lat := res.FirstSampleAt.Sub(res.Started); lat > 0 {
				data["output_latency_ms"] = float64(lat.Nanoseconds()) / 1e6
			}
		}
	}
	b, _ := json.Marshal(data)
	s.publish(events.SpeechEnded, sessionID, b)
	if result == "failed" {
		s.statsAdd(&s.stats.Failed, 1)
	} else {
		s.statsAdd(&s.stats.Ended, 1)
	}
}

func (s *Scheduler) publishCancelled(sessionID string, generation int64, reason, utteranceID string) {
	data, _ := json.Marshal(map[string]any{
		"generation":   generation,
		"utterance_id": utteranceID,
		"reason":       reason,
	})
	s.publish(events.SpeechCancelled, sessionID, data)
	s.statsAdd(&s.stats.Cancelled, 1)
}

func (s *Scheduler) publish(name, sessionID string, data []byte) {
	ctx := s.runCtx
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := s.pub.Publish(ctx, events.Envelope{
		SchemaVersion: 1,
		SessionID:     sessionID,
		Name:          name,
		Data:          data,
	})
	if err != nil {
		// Count via Failed would mislabel; keep a separate path in the
		// stats is overkill — the hub's own Stats carry its drop counters.
		_ = err
	}
}

// stopLatency computes the measured local stop latency (Stop call → sink
// reports ended) for a generation whose stop we requested.
func (s *Scheduler) stopLatency(generation int64, res *PlayResult) time.Duration {
	if !res.Interrupted {
		return 0
	}
	s.mu.Lock()
	t, ok := s.stopReqAt[generation]
	s.mu.Unlock()
	if !ok || res.Ended.IsZero() {
		return 0
	}
	s.setLastStopLatency(res.Ended.Sub(t))
	return res.Ended.Sub(t)
}
