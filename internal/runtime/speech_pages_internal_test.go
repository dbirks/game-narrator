package runtime

// White-box tests for page-aware speech (ADR-0155): a committed message that
// spans several display boxes speaks only the box the player reached, under a
// page-scoped cache identity, and a single-page message keeps the whole-text
// behaviour.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// The invented three-page message under test: page one, the page the player
// reached, and the page after it.
const (
	pageOneText   = "The lantern flickered across the harbour wall."
	pageTwoText   = "A stranger asked about the lost ferry."
	pageThreeText = "The tide was already turning against the dock."
	multiPageText = pageOneText + " " + pageTwoText + " " + pageThreeText
)

// commitPage feeds the wiring one message.committed envelope with the given
// OCR reading (raw_text) and the tracker's comparison text.
func (e *fenceEnv) commitPage(t *testing.T, occ, msgID, region string, generation int64, text, rawText string, eligible bool) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"dialogue_occurrence_id": occ,
		"message_id":             msgID,
		"region_id":              region,
		"generation":             generation,
		"text":                   text,
		"raw_text":               rawText,
		"speech_eligible":        eligible,
	})
	e.w.onCommit(events.Envelope{
		SchemaVersion: 1,
		SessionID:     e.w.id,
		TraceID:       "trace-" + occ,
		Name:          events.MessageCommitted,
		Data:          data,
	})
}

// waitReady polls the session's event log until the generation's
// speech.ready lands (or the deadline passes).
func (e *fenceEnv) waitReady(t *testing.T, gen int64, deadline time.Duration) []events.Envelope {
	t.Helper()
	return e.waitEvents(t, "speech ready", deadline, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name == events.SpeechReady && genOfEv(t, ev) == gen {
				return true
			}
		}
		return false
	})
}

func TestCommitSpeaksOnlyTheReachedPage(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	e.w.initPrewarm(nil, map[string]string{"m-multi": multiPageText}, map[string][]string{
		"m-multi": {pageOneText, pageTwoText, pageThreeText},
	}, nil)

	// Page 2: the box the player reached speaks page 2's text only.
	e.commitPage(t, "occ-p2", "m-multi", "region-box", 2, pageTwoText, pageTwoText, true)
	evs := e.waitReady(t, 2, 10*time.Second)
	var ready map[string]any
	for _, ev := range evs {
		if ev.Name != events.SpeechReady {
			continue
		}
		if genOfEv(t, ev) != 2 {
			continue
		}
		ready = evPayload(t, ev)
	}
	if ready == nil {
		t.Fatal("no speech.ready was emitted for generation 2")
	}
	if got, _ := ready["message_id"].(string); got != "m-multi#p2" {
		t.Fatalf("speech.ready message_id = %q, want the page-scoped id \"m-multi#p2\"", got)
	}
	if got, _ := ready["cache_key"].(string); got == "" {
		t.Fatalf("speech.ready carries no cache_key: %v", ready)
	}

	// Page 1 of the same message speaks page 1's text under its own page key:
	// the next box of one script message is a different utterance, not a
	// duplicate of the message's first box.
	e.commitPage(t, "occ-p1", "m-multi", "region-box", 3, pageOneText, pageOneText, true)
	evs = e.waitReady(t, 3, 10*time.Second)
	var ready1 map[string]any
	for _, ev := range evs {
		if ev.Name != events.SpeechReady {
			continue
		}
		if genOfEv(t, ev) != 3 {
			continue
		}
		ready1 = evPayload(t, ev)
	}
	if got, _ := ready1["message_id"].(string); got != "m-multi#p1" {
		t.Fatalf("speech.ready message_id = %q, want the page-scoped id \"m-multi#p1\"", got)
	}
	if got, _ := ready1["cache_key"].(string); got == "" {
		t.Fatalf("speech.ready carries no cache_key: %v", ready1)
	}
	if ready["cache_key"] == ready1["cache_key"] {
		t.Fatalf("the page keys are identical (%q): the pages must be distinct utterances", ready["cache_key"])
	}

	// The commit of page 2's box speaks page 2's text — the OCR reading the
	// commit carried, through the canonical page text it resolved.
	var requested map[string]any
	for _, ev := range e.sessionEvents() {
		if ev.Name != events.SpeechRequested || genOfEv(t, ev) != 2 {
			continue
		}
		requested = evPayload(t, ev)
	}
	if requested == nil {
		t.Fatal("no speech.requested was emitted for generation 2")
	}
	if got, _ := requested["message_id"].(string); got != "m-multi#p2" {
		t.Fatalf("speech.requested message_id = %q, want the page-scoped id \"m-multi#p2\"", got)
	}
	if got, _ := requested["text"].(string); got != pageTwoText {
		t.Fatalf("speech.requested text = %q, want the reached page's text %q", got, pageTwoText)
	}
}

func TestCommitSinglePageMessageSpeaksWholeText(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	e.w.initPrewarm(nil, map[string]string{"m-a": pageOneText}, nil, nil)

	e.commitPage(t, "occ-a", "m-a", "region-box", 1, pageOneText, pageOneText, true)
	evs := e.waitReady(t, 1, 10*time.Second)
	var requested map[string]any
	for _, ev := range evs {
		if ev.Name != events.SpeechRequested || genOfEv(t, ev) != 1 {
			continue
		}
		requested = evPayload(t, ev)
	}
	if requested == nil {
		t.Fatal("no speech.requested was emitted for generation 1")
	}
	if got, _ := requested["message_id"].(string); got != "m-a" {
		t.Fatalf("the single-page commit's message_id = %q, want the bare message id %q", got, "m-a")
	}
	if got, _ := requested["text"].(string); got != pageOneText {
		t.Fatalf("the single-page commit's text = %q, want the whole message text %q", got, pageOneText)
	}
}

// Six invented sentences: two boxes at the fallback's four-sentence box
// limit, with the last box holding the final two sentences.
const fallbackText = "One lantern flickered across the harbour wall. A stranger asked about the lost ferry. The tide was already turning against the dock. The market bell rang once. The harbour master closed the gate. The night watch began its round."

func TestPagesForFallsBackToASplitOfTheCanonicalText(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	e.w.initPrewarm(nil, map[string]string{"m-multi": fallbackText}, nil, nil)

	pages := e.w.pagesFor("m-multi")
	if len(pages) != 2 {
		t.Fatalf("pagesFor split %q into %d pages, want 2", fallbackText, len(pages))
	}
	if got := pages[0]; got != "One lantern flickered across the harbour wall. A stranger asked about the lost ferry. The tide was already turning against the dock. The market bell rang once." {
		t.Fatalf("fallback page 1 = %q", got)
	}
	if got := pages[1]; got != "The harbour master closed the gate. The night watch began its round." {
		t.Fatalf("fallback page 2 = %q", got)
	}

	// A single-sentence message splits to a single page: the pre-change
	// behaviour is the fallback's own single-page answer.
	e.w.initPrewarm(nil, map[string]string{"m-a": pageOneText}, nil, nil)
	if pages := e.w.pagesFor("m-a"); len(pages) != 1 || pages[0] != pageOneText {
		t.Fatalf("fallback pages of a single-sentence message = %q, want [%q]", pages, pageOneText)
	}
}
