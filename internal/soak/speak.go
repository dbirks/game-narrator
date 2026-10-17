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
// utterance, and the session's occurrence fence is the backstop.
func (h *harness) speak(ctx context.Context, c commitInfo, stamp passStamp) {
	h.spokenMu.Lock()
	if h.spoken[c.occurrence] {
		h.spokenMu.Unlock()
		return
	}
	h.spoken[c.occurrence] = true
	if len(h.spoken) > 4096 {
		// Bounded: the backstop for this map is the session's occurrence
		// fence, so clearing settled entries cannot re-open a double speak.
		h.spoken = map[string]bool{}
	}
	h.spokenMu.Unlock()

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
	h.spokenMu.Lock()
	h.started[c.occurrence] = c.generation
	h.spokenMu.Unlock()

	// The utterance's end: the fake tone plays for toneEndDelay, then the
	// lifecycle closes. One end per occurrence: endOnce gates on the session's
	// fence and the harness's ended set, so the run-end cleanup cannot
	// double-end an occurrence the timer already closed (a double end would
	// trip the no_duplicate_speech assertion the run owes).
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
// ended set is the gate, the session's phase fence the backstop (a cancelled
// phase cannot be ended, and a second end for a closed phase is refused).
func (h *harness) endOnce(ctx context.Context, gen int64, region, occ string) {
	h.spokenMu.Lock()
	if h.ended[occ] {
		h.spokenMu.Unlock()
		return
	}
	m := session.SpeechMsg{
		Header:       h.header(gen),
		RegionID:     region,
		OccurrenceID: occ,
		Kind:         session.SpeechEnded,
		VoiceID:      soakVoiceID,
	}
	_, err := h.sess.Apply(ctx, m)
	if err == nil {
		h.ended[occ] = true
	}
	h.spokenMu.Unlock()
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
	h.spokenMu.Lock()
	live := make(map[string]int64, len(h.started))
	for occ, gen := range h.started {
		if h.ended[occ] {
			continue // the tone's end timer already closed this one
		}
		live[occ] = gen
	}
	h.spokenMu.Unlock()
	for occ, gen := range live {
		h.endOnce(ctx, gen, h.profile.Regions[0].ID, occ)
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
// in: the pass slice the scenario assertions evaluate at the end.
func (p *sidecarPub) trackPass(e events.Envelope) {
	var payload struct {
		Occurrence string `json:"dialogue_occurrence_id"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil || payload.Occurrence == "" {
		return
	}
	if v, ok := p.h.occPass.Load(payload.Occurrence); ok {
		stamp := v.(passStamp)
		p.h.passEventsMu.Lock()
		p.h.passEvents[stamp] = append(p.h.passEvents[stamp], e)
		p.h.passEventsMu.Unlock()
	}
}

// addPassEvent files the sidecar's own commit event under its pass.
func (p *sidecarPub) addPassEvent(stamp passStamp, e events.Envelope) {
	p.h.passEventsMu.Lock()
	p.h.passEvents[stamp] = append(p.h.passEvents[stamp], e)
	p.h.passEventsMu.Unlock()
}
