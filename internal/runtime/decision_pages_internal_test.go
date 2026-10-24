package runtime

// White-box tests for page-aware matching glue (ADR-0184): the decision
// request offers the box on screen (the page the matcher scored) as the
// candidate text, the candidate question says a message may be displayed one
// box at a time, and a commit that names a page speaks exactly that page
// even when the OCR read cannot be re-placed by containment.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
)

func TestDecisionOptionTextUsesTheMatchedPage(t *testing.T) {
	w := &decisionWiring{
		texts: map[string]string{"m1": "Page one words. Page two words."},
		pages: map[string][]string{"m1": {"Page one words.", "Page two words."}},
	}
	c := matcher.Candidate{MessageID: "m1", Page: 2,
		Components: map[string]float64{matcher.ComponentPage: 2}}
	if got := w.optionText(c); got != "Page two words." {
		t.Fatalf("optionText(page 2) = %q, want the page text %q", got, "Page two words.")
	}
	// No page recorded: the whole text, exactly as before the change.
	whole := matcher.Candidate{MessageID: "m1"}
	if got := w.optionText(whole); got != "Page one words. Page two words." {
		t.Fatalf("optionText(no page) = %q, want the whole text", got)
	}
	// A page ordinal past the end is no page: fall back, never index past it.
	bad := matcher.Candidate{MessageID: "m1", Page: 9}
	if got := w.optionText(bad); got != "Page one words. Page two words." {
		t.Fatalf("optionText(page 9) = %q, want the whole text", got)
	}
	// A message without pages: the whole text even with a stale page.
	noPages := &decisionWiring{
		texts: map[string]string{"m2": "One box only."},
		pages: map[string][]string{},
	}
	if got := noPages.optionText(matcher.Candidate{MessageID: "m2", Page: 9}); got != "One box only." {
		t.Fatalf("optionText(page-less message) = %q, want the whole text", got)
	}
}

func TestDecisionCandidateQuestionNotesBoxDisplay(t *testing.T) {
	bundle := systemone.BuildAdjudication(systemone.Context{OCR: "some read"},
		[]systemone.Candidate{{ID: "m1", Text: "Page two words."}}, nil)
	(&decisionWiring{}).noteBoxDisplay(bundle)
	for _, q := range bundle.Questions {
		if q.ID != systemone.QCandidate {
			continue
		}
		if q.Instructions == "Which candidate matches the visible text?" {
			t.Fatalf("the candidate question was not told about box-at-a-time display: %q", q.Instructions)
		}
		if want := "A message may be displayed one box (page) at a time"; !strings.Contains(q.Instructions, want) {
			t.Fatalf("candidate question %q lacks the box-at-a-time note %q", q.Instructions, want)
		}
		return
	}
	t.Fatal("no candidate question in the bundle")
}

// commitPageWithPage feeds one message.committed envelope that names the
// display page the matcher scored (the "page" key, ADR-0184).
func (e *fenceEnv) commitPageWithPage(t *testing.T, occ, msgID, region string, generation int64, text, rawText string, page int, eligible bool) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{
		"dialogue_occurrence_id": occ,
		"message_id":             msgID,
		"region_id":              region,
		"generation":             generation,
		"text":                   text,
		"raw_text":               rawText,
		"speech_eligible":        eligible,
		"page":                   page,
	})
	e.w.onCommit(events.Envelope{
		SchemaVersion: 1,
		SessionID:     e.w.id,
		TraceID:       "trace-" + occ,
		Name:          events.MessageCommitted,
		Data:          data,
	})
}

// TestCommitNamedPageWinsOverReplacedOCRRead (ADR-0184): the read is OCR
// noise that contains no page's words at all, so choosePage's containment
// and word-share re-derivation both fail — without the commit's page the line
// would have spoken the whole message. The commit's page is the matcher's
// own comparison, and it wins.
func TestCommitNamedPageWinsOverUnplaceableOCRRead(t *testing.T) {
	e := newFenceEnv(t, 0.1, true, nil)
	e.w.initPrewarm(nil, map[string]string{"m-multi": multiPageText}, map[string][]string{
		"m-multi": {pageOneText, pageTwoText, pageThreeText},
	}, nil)

	// raw_text is junk that contains no page's words; page 2 is the box the
	// matcher actually matched.
	e.commitPageWithPage(t, "occ-p2-named", "m-multi", "region-box", 7, pageTwoText, "ZZZ QQQ XXX", 2, true)
	evs := e.waitReady(t, 7, 10*time.Second)
	var requested map[string]any
	for _, ev := range evs {
		if ev.Name != events.SpeechRequested || genOfEv(t, ev) != 7 {
			continue
		}
		requested = evPayload(t, ev)
	}
	if requested == nil {
		t.Fatal("no speech.requested was emitted for generation 7")
	}
	if got, _ := requested["message_id"].(string); got != "m-multi#p2" {
		t.Fatalf("speech.requested message_id = %q, want the page-scoped id \"m-multi#p2\"", got)
	}
	if got, _ := requested["text"].(string); got != pageTwoText {
		t.Fatalf("speech.requested text = %q, want the named page's text %q (the junk read must not fall back to the whole message)", got, pageTwoText)
	}
}
