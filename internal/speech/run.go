package speech

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// activePlay is one in-flight playback as tracked by the loop.
type activePlay struct {
	u      UtteranceReady
	handle PlayHandle
}

// atomicBool is a tiny named wrapper so the scheduler's lifecycle flags read
// clearly at their call sites.
type atomicBool struct{ v atomic.Bool }

func (b *atomicBool) get() bool  { return b.v.Load() }
func (b *atomicBool) set(x bool) { b.v.Store(x) }

// run is the scheduler loop: one goroutine, one active playback, fence
// checked before start and on a ticker during playback, streaming sources
// fenced at every chunk boundary.
func (s *Scheduler) run(ctx context.Context) {
	defer close(s.stopped)
	ticker := time.NewTicker(s.fencePeriod)
	defer ticker.Stop()

	var active *activePlay
	var activeDone <-chan PlayResult

loop:
	for {
		if active != nil {
			select {
			case res := <-activeDone:
				s.finishActive(active, res)
				active, activeDone = nil, nil
			case c := <-s.cancel:
				s.applyCancel(c, active)
			case <-ticker.C:
				// Fence at every ~100 ms boundary while playing.
				if !s.fence.IsCurrent(active.u.Generation) {
					s.stopStale(active)
				}
			case <-ctx.Done():
				if active != nil {
					active.handle.Stop(s.stopFade)
				}
				break loop
			}
			continue
		}

		select {
		case u := <-s.inbox:
			// Fence BEFORE starting (fencing point 1).
			if !s.fence.IsCurrent(u.Generation) {
				s.drop(u, "stale_generation")
				continue
			}
			if reason := s.takeCancelled(u.Generation); reason != "" {
				s.drop(u, "cancelled:"+reason)
				continue
			}
			if u.Source != nil {
				assembled, ok := s.streamInto(ctx, u)
				if !ok {
					continue // already dropped + evented
				}
				u = assembled
			}
			handle, err := s.sink.Play(ctx, s.requestOf(u))
			if err != nil {
				s.publishEnded(u.SessionID, u.Generation, u.UtteranceID, "failed", fmt.Sprintf("play_error:%v", err), nil)
				continue
			}
			active = &activePlay{u: u, handle: handle}
			activeDone = handle.Done()
			s.publishStarted(ctx, u, s.clock())
		case c := <-s.cancel:
			s.applyCancel(c, nil)
		case <-ctx.Done():
			break loop
		}
	}
}

// applyCancel records the cancel and stops active playback of that generation.
func (s *Scheduler) applyCancel(c cancelReq, active *activePlay) {
	s.rememberCancelled(c.generation, c.reason)
	if active != nil && active.u.Generation == c.generation {
		s.mu.Lock()
		s.stopReqAt[c.generation] = s.clock()
		s.mu.Unlock()
		s.publishCancelled(active.u.SessionID, c.generation, c.reason, active.u.UtteranceID)
		active.handle.Stop(s.stopFade)
		return
	}
	// No active playback of that generation: queued items are dropped when
	// popped (takeCancelled), and the event tells the UI the request died.
	s.publishCancelled("", c.generation, c.reason, "")
}

// stopStale stops an active playback whose generation the fence just
// rejected. The stop is measured against the same 200 ms target as an
// explicit cancel (docs/spec/12: "Zero stale audio starts ... local stop
// event target <200 ms").
func (s *Scheduler) stopStale(active *activePlay) {
	s.mu.Lock()
	s.stopReqAt[active.u.Generation] = s.clock()
	s.mu.Unlock()
	s.publishCancelled(active.u.SessionID, active.u.Generation, "stale_generation", active.u.UtteranceID)
	active.handle.Stop(s.stopFade)
}

// finishActive reports the terminal result of a playback.
func (s *Scheduler) finishActive(active *activePlay, res PlayResult) {
	u := active.u
	switch {
	case res.Err != nil:
		s.publishEnded(u.SessionID, u.Generation, u.UtteranceID, "failed", fmt.Sprintf("sink_error:%v", res.Err), &res)
	default:
		reason := ""
		if res.Interrupted {
			reason = "interrupted"
		}
		s.publishEnded(u.SessionID, u.Generation, u.UtteranceID, "ended", reason, &res)
	}
}

// drop sheds an item without playing it and events it as failed.
func (s *Scheduler) drop(u UtteranceReady, reason string) {
	s.statsAdd(&s.stats.DroppedStale, 1)
	s.publishEnded(u.SessionID, u.Generation, u.UtteranceID, "failed", reason, nil)
}

// streamInto fences a streaming source at every chunk boundary and assembles
// the WAV. It returns (ready, false) — with the drop evented — when the
// generation goes stale, the stream is cancelled, or the stream errors or
// stalls (docs/spec/12 "TTS partly streams before cancellation": after
// cancellation the source stops receiving Next calls and no late audio plays).
func (s *Scheduler) streamInto(ctx context.Context, u UtteranceReady) (UtteranceReady, bool) {
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var buf bytes.Buffer
	for {
		// Fence at every chunk boundary (before pulling the next chunk).
		if !s.fence.IsCurrent(u.Generation) {
			cancel()
			s.drop(u, "stale_during_stream")
			return u, false
		}
		if reason := s.takeCancelled(u.Generation); reason != "" {
			cancel()
			s.drop(u, "cancelled:"+reason)
			return u, false
		}
		cctx, ccancel := context.WithTimeout(streamCtx, s.maxChunkWait)
		chunk, err := u.Source.Next(cctx)
		ccancel()
		// The contract allows EOF either way (Next error OR Chunk.Err);
		// honor both so a well-behaved source can never busy-loop the loop.
		var streamErr error
		switch {
		case err != nil:
			streamErr = err
		case chunk.Err != nil:
			streamErr = chunk.Err
		}
		if streamErr != nil {
			if errors.Is(streamErr, io.EOF) {
				break
			}
			cancel()
			why := "stream_error"
			if streamCtx.Err() != nil {
				why = "stale_during_stream"
			}
			s.drop(u, why)
			return u, false
		}
		buf.Write(chunk.PCM)
	}
	if buf.Len() == 0 {
		s.drop(u, "empty_stream")
		return u, false
	}
	// Final fence before the assembled WAV can ever play.
	if !s.fence.IsCurrent(u.Generation) {
		s.drop(u, "stale_generation")
		return u, false
	}
	u.WAV = EncodeWAV(buf.Bytes(), u.Source.SampleRate(), u.Source.Channels(), u.Source.Bits())
	u.Source = nil
	return u, true
}

func (s *Scheduler) requestOf(u UtteranceReady) PlayRequest {
	return PlayRequest{
		UtteranceID: u.UtteranceID,
		Generation:  u.Generation,
		WAV:         u.WAV,
		Gain:        1.0,
	}
}

// rememberCancelled records an explicit Cancel for a generation.
func (s *Scheduler) rememberCancelled(generation int64, reason string) {
	s.mu.Lock()
	s.cancelled[generation] = reason
	if len(s.cancelled) > 128 { // prune: keep at most the last 64
		keys := make([]int64, 0, len(s.cancelled))
		for k := range s.cancelled {
			keys = append(keys, k)
		}
		for i := 0; i < len(keys)-64; i++ {
			delete(s.cancelled, keys[i])
		}
	}
	s.mu.Unlock()
}

// takeCancelled returns the cancel reason for a generation, if one is
// pending, and forgets it.
func (s *Scheduler) takeCancelled(generation int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.cancelled[generation]
	if ok {
		delete(s.cancelled, generation)
	}
	return r
}
