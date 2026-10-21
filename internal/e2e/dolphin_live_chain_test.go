// Package e2e — the assertion stage of the live Dolphin E2E
// (task m6-live-e2e). Split across files so no single write exceeds the
// tool-call bound; log-safe per AGENTS.md rule 5: log lines carry counts,
// ids, hashes and timings — the dialogue text stays in the evidence bundle.

package e2e

import (
	"encoding/json"
	"testing"
)

// dolphinChain is the correlated evidence set of one occurrence: every event
// found on the commit's trace plus the Clef request joined the way the
// product does — decision.requested rides no trace, the developer UI joins
// it on observation_seq + generation (internal/runtime/decision.go).
type dolphinChain struct {
	trace        string
	commit       *devEnvelope
	text         *devEnvelope
	candidates   *devEnvelope
	decRequested *devEnvelope
	decResolved  *devEnvelope
	speechReq    *devEnvelope
	speechStart  *devEnvelope
	speechEnd    *devEnvelope

	textData struct {
		Text          string `json:"text"`
		FrameSeq      int64  `json:"frame_seq"`
		RegionPresent bool   `json:"region_present"`
		CaptureTSNS   int64  `json:"capture_timestamp_ns"`
		FrameAsset    string `json:"frame_asset"`
		CropAsset     string `json:"crop_asset"`
		OccurrenceID  string `json:"dialogue_occurrence_id"`
	}
	candData struct {
		ObservationSeq int64  `json:"observation_seq"`
		Generation     int64  `json:"generation"`
		RegionID       string `json:"region_id"`
		Text           string `json:"text"`
		OccurrenceID   string `json:"dialogue_occurrence_id"`
		Candidates     []struct {
			MessageID   string  `json:"message_id"`
			Probability float64 `json:"probability"`
		} `json:"candidates"`
	}
	commitData struct {
		MessageID    string `json:"message_id"`
		OccurrenceID string `json:"dialogue_occurrence_id"`
	}
	decReqData struct {
		Provider       string `json:"provider"`
		Model          string `json:"model"`
		ObservationSeq int64  `json:"observation_seq"`
		Generation     int64  `json:"generation"`
	}
	decResData struct {
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		RequestID string `json:"request_id"`
		Accepted  bool   `json:"accepted"`
		LatencyMS int64  `json:"latency_ms"`
		MessageID string `json:"message_id"`
	}
	speechEndData struct {
		Result     string `json:"result"`
		VoiceID    string `json:"voice_id"`
		Occurrence string `json:"dialogue_occurrence_id"`
	}
}

// chainEvent finds one event of name on the trace; lastTrace falls back to
// the session-wide last event of the name (the speech workers republish with
// the commit's trace, so the fallback only serves out-of-order reads).
func chainEvent(t *testing.T, evs []devEnvelope, name, trace string, requireNonEmptyData bool) *devEnvelope {
	t.Helper()
	var last *devEnvelope
	for i := range evs {
		e := &evs[i]
		if e.Name != name {
			continue
		}
		if requireNonEmptyData && len(e.Data) == 0 {
			continue
		}
		if e.TraceID == trace {
			return e
		}
		last = e
	}
	return last
}

// chainEventJSON is chainEvent with the payload decoded; the pointer is nil
// (not a zero event) when the link is absent. out may be nil (link found,
// payload not needed).
func chainEventJSON(t *testing.T, evs []devEnvelope, name, trace string, out any) (*devEnvelope, bool) {
	t.Helper()
	e := chainEvent(t, evs, name, trace, true)
	if e == nil {
		return nil, false
	}
	if out != nil {
		if err := json.Unmarshal(e.Data, out); err != nil {
			t.Fatalf("%s payload decode: %v", name, err)
		}
	}
	return e, true
}

// chainEventSeq finds the event of name on the trace whose payload's
// (observation_seq, generation) pair equals the join keys — the product's
// documented join (the same keys decision.requested is joined on). Returns
// nil when no event on the trace carries them.
func chainEventSeq(t *testing.T, evs []devEnvelope, name, trace string, obsSeq, gen int64) *devEnvelope {
	t.Helper()
	for i := range evs {
		e := &evs[i]
		if e.Name != name || e.TraceID != trace {
			continue
		}
		var d struct {
			ObservationSeq int64 `json:"observation_seq"`
			Generation     int64 `json:"generation"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			continue
		}
		if d.ObservationSeq == obsSeq && d.Generation == gen {
			return e
		}
	}
	return nil
}

// dolphinChainEvidence gathers the correlated set for the commit the poll
// loop found. The poll loop (findDolphinChain) already proved the trace-level
// links exist, so a nil here is a race with a late page read, not a missing
// stage — the stage asserts below name which link fell out.
func dolphinChainEvidence(t *testing.T, a *dolphinAsserts) *dolphinChain {
	t.Helper()
	trace := a.commit.TraceID
	ch := &dolphinChain{trace: trace, commit: &a.commit}
	if e, _ := chainEventJSON(t, a.evidenceEvents, "message.candidates", trace, &ch.candData); e != nil {
		ch.candidates = e
	}
	// The text link is the observation the candidates' join key names: the
	// trace spans the occurrence's whole text_state lifecycle (appearing ->
	// revealing -> stabilizing -> ready -> committed) and the FIRST
	// text.observed on it is the region-detection frame, which legally
	// carries no text (the subtitle's typewriter is still filling — the
	// 2026-10-21 liveui run measured 15 s between the empty "appearing"
	// frame and the "revealing" reading on the same trace).
	if e := chainEventSeq(t, a.evidenceEvents, "text.observed", trace, ch.candData.ObservationSeq, ch.candData.Generation); e != nil {
		if err := json.Unmarshal(e.Data, &ch.textData); err != nil {
			t.Fatalf("text.observed payload decode: %v", err)
		}
		ch.text = e
	} else if e, _ := chainEventJSON(t, a.evidenceEvents, "text.observed", trace, &ch.textData); e != nil {
		ch.text = e // no join-key match (late page read): the trace's first observation
	}
	if e, _ := chainEventJSON(t, a.evidenceEvents, "decision.resolved", trace, &ch.decResData); e != nil {
		ch.decResolved = e
	}
	if e, _ := chainEventJSON(t, a.evidenceEvents, "speech.requested", trace, nil); e != nil {
		ch.speechReq = e
	}
	if e, _ := chainEventJSON(t, a.evidenceEvents, "speech.started", trace, nil); e != nil {
		ch.speechStart = e
	}
	if e, _ := chainEventJSON(t, a.evidenceEvents, "speech.ended", trace, &ch.speechEndData); e != nil {
		ch.speechEnd = e
	}
	if err := json.Unmarshal(a.commit.Data, &ch.commitData); err != nil {
		t.Fatalf("commit payload decode: %v", err)
	}
	// The Clef request's join keys: observation_seq + generation, the
	// candidates event's (the product's documented join, not a trace).
	for i := range a.evidenceEvents {
		e := &a.evidenceEvents[i]
		if e.Name != "decision.requested" {
			continue
		}
		var d struct {
			ObservationSeq int64 `json:"observation_seq"`
			Generation     int64 `json:"generation"`
		}
		if err := json.Unmarshal(e.Data, &d); err != nil {
			continue
		}
		if d.ObservationSeq == ch.candData.ObservationSeq && d.Generation == ch.candData.Generation {
			ch.decRequested = e
			if err := json.Unmarshal(e.Data, &ch.decReqData); err != nil {
				t.Fatalf("decision.requested payload decode: %v", err)
			}
			break
		}
	}
	return ch
}
