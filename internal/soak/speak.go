// speak.go is the decision/TTS work one sidecar commit triggers, and the
// sidecar's event publisher: ask the chaos decision gateway, apply the
// answer through the session's fences, and when it is applied, request the
// voice from the chaos TTS gateway. The fences are the session's own — a
// reply that lands after the dialogue advanced names the older generation and
// is discarded, which is the mechanism behind docs/spec/12's "zero stale
// audio starts".

package soak

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/providers/tts"
	"github.com/dbirks/game-narrator/internal/session"
)

// toneEndDelay is the fake utterance's playback length: the gateways answer
// with a 0.1 s tone (the scenario's arrangement), so the audio device would
// report the end 100 ms after generation. The sidecar models that end with a
// timer; when the dialogue advanced first, the phase is cancelled and the
// session's fence refuses the end — the fence decides, not the timer.
const toneEndDelay = 100 * time.Millisecond

// handleCommit is the provider work one commit triggers. It runs on its own
// goroutine so a chaos-delayed decision (up to providerTimeout) cannot hold
// up the sidecar's pacing; the wg drain at run end collects the leftovers.
func (h *harness) handleCommit(ctx context.Context, c commitInfo, stamp passStamp) {
	defer h.wg.Done()
	defer func() {
		if p := recover(); p != nil {
			h.panics.Add(1)
		}
	}()

	h.emit(ctx, events.DecisionRequested, map[string]any{
		"message_id":      c.messageID,
		"occurrence_id":   c.occurrence,
		"generation":      c.generation,
		"region_id":       c.region,
		"provider":        "systemone",
		"provider_origin": "chaos",
		"pass_clip":       stamp.clip,
		"pass_index":      stamp.pass,
	})

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	req := systemone.Request{
		Model: soakDecisionModel,
		State: map[string]any{
			"capture_ts_ms":  c.tsNS / 1e6,
			"confirm_choice": "confirmed",
			"text":           c.text,
		},
		Questions: []systemone.Question{{
			ID:           "line_confirm",
			Type:         systemone.TypeChoice,
			Instructions: "Confirm which profile line the textbox shows; choose exactly one.",
			Criteria: []systemone.Option{
				{ID: "confirmed", Description: c.text},
				{ID: "unknown", Description: "the text matches no profile line"},
			},
		}},
	}
	dec, err := h.dec.Decide(pctx, req)
	if err != nil {
		kind := classifyKind(err)
		h.providerErrs.Add(1)
		counterInc(&h.errKinds, kind)
		h.emit(ctx, events.DecisionResolved, map[string]any{
			"message_id":    c.messageID,
			"occurrence_id": c.occurrence,
			"generation":    c.generation,
			"accepted":      false,
			"applied":       false,
			"reason":        "provider_error",
			"error":         kind,
			"provider":      "systemone",
		})
		h.setDegraded("decision", true, kind)
		// Abstain, not guess: docs/spec/06 — on provider error the runtime
		// proceeds without Clef; the matcher's commitment stands, but a named
		// voice without the decision's evidence is silence, not a guess.
		return
	}
	h.setDegraded("decision", false, "recovered")

	msgID := c.messageID
	if dec.Answers["line_confirm"].Choice == "unknown" {
		msgID = session.UnknownMessage
	}

	// The answer goes through the session's fences under the generation it was
	// asked with: a reply that lands after the dialogue advanced is stale and
	// discarded, never applied. The observation seq is stamped at reply time,
	// the way the scenario does: a healthy immediate reply must not race the
	// next observation for the reducer's lock.
	obsNow := c.obsSeq
	if st := h.sess.State(); len(st.Regions) > 0 {
		obsNow = st.Regions[0].ObservationSeq
	}
	dm := session.DecisionMsg{
		Header:         h.header(c.generation),
		RegionID:       c.region,
		ObservationSeq: obsNow,
		MessageID:      msgID,
		SpeechEligible: msgID != session.UnknownMessage,
		Accepted:       true,
		Provider:       "systemone",
		Model:          dec.Model,
	}
	res, err := h.sess.Apply(ctx, dm)
	applied := err == nil && res.Applied

	// decision_duplicate (the scenario's shape, chaos rate): the same reply,
	// same seq, delivered twice; the session's seq fence must refuse the
	// second delivery.
	if applied && h.cfg.Chaos.Enabled && h.chaos.float64() < h.cfg.Chaos.DuplicateRate {
		h.recordFault(FaultDecisionDuplicate, "decision",
			fmt.Sprintf("decision reply for %s delivered twice (same seq)", c.messageID))
		res2, err2 := h.sess.Apply(ctx, dm)
		h.emit(ctx, events.DecisionResolved, map[string]any{
			"message_id":    c.messageID,
			"occurrence_id": c.occurrence,
			"generation":    c.generation,
			"accepted":      true,
			"applied":       err2 == nil && res2.Applied,
			"reason":        res2.Reason,
			"provider":      "systemone",
			"delivery":      2,
		})
	}

	h.emit(ctx, events.DecisionResolved, map[string]any{
		"message_id":    c.messageID,
		"occurrence_id": c.occurrence,
		"generation":    c.generation,
		"accepted":      true,
		"applied":       applied,
		"reason":        res.Reason,
		"model":         dec.Model,
		"provider":      "systemone",
	})
	if msgID != session.UnknownMessage && applied {
		h.speak(ctx, c, stamp)
	}
}

// speak requests the voice for one committed occurrence, at most once per
// occurrence: a duplicated provider reply must not produce a second
// utterance. The dedupe map is bounded (spokenCap): dead entries are
// reclaimed by the prune sweep, and the session's occurrence fence is the
// backstop for whatever the bounded map cannot hold.
func (h *harness) speak(ctx context.Context, c commitInfo, stamp passStamp) {
	if h.noteSpoken(c.occurrence, stamp) {
		return
	}

	pctx, cancel := context.WithTimeout(ctx, providerTimeout)
	defer cancel()
	audio, err := h.tts.Generate(pctx, tts.GenerateRequest{Text: c.text, Voice: soakVoiceID})
	if err != nil {
		kind := classifyKind(err)
		h.providerErrs.Add(1)
		counterInc(&h.errKinds, kind)
		h.setDegraded("tts", true, kind)
		h.applySpeech(ctx, c, session.SpeechFailed, kind)
		return
	}
	h.setDegraded("tts", false, "recovered")

	key := fmt.Sprintf("text=%s|voice=%s", c.text, soakVoiceID)
	latency := int64(audio.Latency / time.Millisecond)
	// One fresh header per lifecycle step: the messages share the occurrence
	// but not the sequence (reusing one would trip the duplicate-seq fence
	// and the utterance would never start).
	m := session.SpeechMsg{
		Header:        h.header(c.generation),
		RegionID:      c.region,
		OccurrenceID:  c.occurrence,
		Kind:          session.SpeechReady,
		VoiceID:       soakVoiceID,
		VoiceRevision: 1,
		CacheKey:      key,
		LatencyMS:     latency,
	}
	if _, err := h.sess.Apply(ctx, m); err != nil {
		return
	}
	m.Kind = session.SpeechStarted
	m.Header = h.header(c.generation)
	if _, err := h.sess.Apply(ctx, m); err != nil {
		return
	}
	// The ready/started events were filed into the pass slice by the
	// publisher (trackPass), which is also what marks the utterance started
	// for the settlement test — no manual bookkeeping here.

	// The utterance's end: the fake tone plays for toneEndDelay, then the
	// lifecycle closes. endOnce is idempotent through the session's fence:
	// a second end (the run-end cleanup racing the timer) names a closed
	// phase and is refused, so no double end reaches the timeline.
	h.wg.Add(1)
	go func() {
		defer h.wg.Done()
		select {
		case <-time.After(toneEndDelay):
		case <-ctx.Done():
			return
		}
		h.endOnce(ctx, c.generation, c.region, c.occurrence)
	}()
}

// endOnce applies the terminal end for one occurrence at most once: the
// session's phase fence is the gate (a cancelled or ended phase cannot be
// ended, and a second end for a closed phase is refused); the harness needs
// no set of its own, because the ended event itself is what the pass slice
// records as the terminal (trackPass).
func (h *harness) endOnce(ctx context.Context, gen int64, region, occ string) {
	m := session.SpeechMsg{
		Header:       h.header(gen),
		RegionID:     region,
		OccurrenceID: occ,
		Kind:         session.SpeechEnded,
		VoiceID:      soakVoiceID,
	}
	_, _ = h.sess.Apply(ctx, m)
}

// applySpeech pushes one lifecycle step through the session's speech fence.
func (h *harness) applySpeech(ctx context.Context, c commitInfo, kind session.SpeechKind, failKind string) {
	m := session.SpeechMsg{
		Header:       h.header(c.generation),
		RegionID:     c.region,
		OccurrenceID: c.occurrence,
		Kind:         kind,
		VoiceID:      soakVoiceID,
	}
	if failKind != "" {
		m.Error = failKind + ": " + soakVoiceID
	}
	// A refused speech step is the fence working: it is counted in the
	// session stats, and no event is emitted for a state change that did not
	// happen.
	_, _ = h.sess.Apply(ctx, m)
}

// finishUtterances closes the utterances still live at the end of the run:
// the last line's speech ends cleanly; a cancelled one is refused by the
// session's phase fence, which is exactly "no stale audio starts".
func (h *harness) finishUtterances(ctx context.Context) {
	h.passMu.Lock()
	var live []struct {
		gen int64
		occ string
	}
	for _, ps := range h.passStates {
		for occ, gen := range ps.started {
			if !ps.terminal[occ] {
				live = append(live, struct {
					gen int64
					occ string
				}{gen, occ})
			}
		}
	}
	h.passMu.Unlock()
	for _, l := range live {
		h.endOnce(ctx, l.gen, h.profile.Regions[0].ID, l.occ)
	}
}

// setDegraded tracks the provider.degraded state per role: the first failure
// publishes the degradation, the first success after it the recovery.
func (h *harness) setDegraded(role string, degraded bool, reason string) {
	v, _ := h.degraded.LoadOrStore(role, false)
	cur, _ := v.(bool)
	if cur == degraded {
		return
	}
	h.degraded.Store(role, degraded)
	h.emit(context.Background(), events.ProviderDegraded, map[string]any{
		"role":     role,
		"degraded": degraded,
		"reason":   reason,
	})
}

// emit publishes one harness event onto the shared timeline.
func (h *harness) emit(ctx context.Context, name string, data map[string]any) {
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	_, _ = h.hub.Publish(ctx, events.Envelope{
		SessionID: soakSessionID,
		Name:      name,
		Data:      payload,
	})
}

// sidecarPub is the sidecar session's publisher (the scenario's shape): the
// speech lifecycle events are forwarded onto the shared timeline (they are
// the provider layer's evidence) and counted for the sampler; everything
// else stays local, numbered the way the hub would, so the sidecar's
// causation chain stays inspectable without touching the merged seq space.
type sidecarPub struct {
	hub *events.Hub
	h   *harness

	mu  sync.Mutex
	seq int64
}

// Publish implements session.Publisher.
func (p *sidecarPub) Publish(ctx context.Context, e events.Envelope) (events.Envelope, error) {
	switch e.Name {
	case events.SpeechReady, events.SpeechStarted:
		p.h.speechStarted.Add(1)
		p.trackPass(e)
		return p.hub.Publish(ctx, e)
	case events.SpeechCancelled, events.SpeechEnded:
		p.h.speechEnded.Add(1)
		p.trackPass(e)
		return p.hub.Publish(ctx, e)
	default:
		// Local path: the sidecar's own events (observations, rankings, its
		// commits) stay out of the merged timeline, but they are numbered and
		// identified the way the hub would — an un-identified event breaks the
		// session's causation chain, and the session counts that.
		p.mu.Lock()
		p.seq++
		e.Seq = p.seq
		if e.EventID == "" {
			e.EventID = games.NewID()
		}
		p.mu.Unlock()
		return e, nil
	}
}

// trackPass files one speech event under the pass its occurrence committed
// in: the pass slice the scenario assertions evaluate. The event is routed
// through the occurrence's pass while the slice is still pending; a slice
// that has already been graded owes no further events (its utterances all
// reached a terminal), so a late event for it is dropped from the assertion
// view — the merged timeline (the hub) still carries it.
func (p *sidecarPub) trackPass(e events.Envelope) {
	var payload struct {
		Occurrence string `json:"dialogue_occurrence_id"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil || payload.Occurrence == "" {
		return
	}
	p.h.passMu.Lock()
	stamp, ok := p.h.occPass[payload.Occurrence]
	if ok {
		p.h.filePassEvent(stamp, e)
	}
	p.h.passMu.Unlock()
}

// addPassEvent files the sidecar's own commit event under its pass. The
// commit events of a pass are all filed while the pass is being fed (the
// ranking is applied synchronously in feedRow), so a pass is never graded
// with a commit still on the way.
func (p *sidecarPub) addPassEvent(stamp passStamp, e events.Envelope) {
	p.h.passMu.Lock()
	p.h.filePassEvent(stamp, e)
	p.h.passMu.Unlock()
}

// spokenCap bounds the spoken-dedupe map. Entries are reclaimed by the
// prune sweep (settled or unrouted occurrences); the final clear is the
// pathological fallback — every entry's utterance still open — and the
// session's occurrence fence is the backstop that makes clearing safe.
const spokenCap = 4096

// noteSpoken records the occurrence as spoken and reports whether it was
// already spoken (the double-speak guard). It keeps the dedupe map at its
// bound: a new entry is admitted only after the prune sweep has made room,
// and the sweep's clear fallback (a fully unsettled map) is safe because the
// session's occurrence fence is the backstop. The unit test drives this
// method directly with N ≫ spokenCap occurrences.
func (h *harness) noteSpoken(occ string, stamp passStamp) bool {
	h.spokenMu.Lock()
	defer h.spokenMu.Unlock()
	if _, ok := h.spoken[occ]; ok {
		return true
	}
	h.spoken[occ] = stamp
	if len(h.spoken) > spokenCap {
		h.pruneSpokenLocked()
	}
	return false
}

// pruneSpokenLocked evicts the spoken entries that are dead: the occurrence
// reached a terminal in a settled slice, or its occurrence routing is gone
// (the slice was graded, or the occurrence never routed a speech event). If
// the sweep still leaves the map at its bound — every utterance it covers
// still open, a stall the provider budget should not produce — it clears
// the map: the session's occurrence fence is the backstop, so a cleared
// entry cannot re-open a double speak it has not already passed.
// Called with h.spokenMu held.
func (h *harness) pruneSpokenLocked() {
	for occ, stamp := range h.spoken {
		h.passMu.Lock()
		ps := h.passStates[stamp]
		_, routed := h.occPass[occ]
		done := !routed || (ps != nil && ps.settled(time.Now()))
		h.passMu.Unlock()
		if done {
			delete(h.spoken, occ)
		}
	}
	if len(h.spoken) >= spokenCap {
		h.spoken = map[string]passStamp{}
	}
}
