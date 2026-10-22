package pipeline_test

// OCRLagging (2026-10-22): with an OCR slower than the frame interval — the
// UNO Q's Tesseract takes 1–2 s — every frame of a typewriter reveal rides
// the read already in flight for an older crop. Counting those frames toward
// the stability window committed a stale prefix ("the", "the harbour bell")
// as an unmatched line, and the real line was then lost or doubled.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
)

//
// The delays stay well inside the 2.4 s clip: at 400 ms under -race and
// parallel package load the clip can end before a fresh read confirms the
// last box (a real box stays up until the player advances; the clip does
// not) — without the fix, 200 ms already committed "the harbour bell".
func TestSlowOCRCommitsTheFullLinesNotAPrefix(t *testing.T) {
	for _, d := range []time.Duration{150 * time.Millisecond, 250 * time.Millisecond} {
		eng := &fakeOCR{script: ocrScript(t), delay: func(string) time.Duration { return d }, respectCtx: true}
		h := newHarness(t, eng, func(c *pipeline.Config) { c.Mode = pipeline.ModeLive; c.Queue = pipeline.QueueLossless })
		_, evs, err := h.run(t, h.source(1, false))
		if err != nil {
			t.Fatalf("delay %v: run: %v", d, err)
		}
		var got []string
		for _, e := range evs {
			if e.Name != events.MessageCommitted {
				continue
			}
			var m struct {
				MessageID string `json:"message_id"`
				Reason    string `json:"reason"`
				Text      string `json:"text"`
			}
			_ = json.Unmarshal(e.Data, &m)
			if m.Reason == "COMMITTED_ABSTAINED" {
				t.Errorf("delay %v: a stale prefix committed unmatched: %q", d, m.Text)
			}
			got = append(got, m.MessageID)
		}
		if len(got) != 2 || got[0] != msgIDLineA || got[1] != msgIDLineB {
			t.Errorf("delay %v: committed %v, want [%s %s]", d, got, msgIDLineA, msgIDLineB)
		}
	}
}
