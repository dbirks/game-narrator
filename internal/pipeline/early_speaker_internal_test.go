package pipeline

// The ADR-0160 early-speaker ask, from the internal side: the fire-and-forget
// rank worker. A confident local top candidate on a shared-slot line owes the
// adjudicator's speaker attribution; the ask runs in the rank worker's own
// goroutine and delivers the ranking reply itself, so a burst of reads of one
// typing box costs exactly one ask and never serializes behind the decider.

import (
	"context"
	"image"
	"sync"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/games"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/session"
	"github.com/dbirks/game-narrator/internal/vision/ocr"
)

// burstOCR is the least machinery a Runner needs to construct: the rank
// worker never reads it — only the Config validation does.
type burstOCR struct{}

func (burstOCR) Recognize(ctx context.Context, img image.Image) (ocr.Result, error) {
	return ocr.Result{Text: "the boy shouts back"}, nil
}

// burstEarlySpeakerDecider answers the speaker-only early ask after a fixed
// delay and counts the asks it served.
type burstEarlySpeakerDecider struct {
	mu    sync.Mutex
	asked int64
	sleep time.Duration
}

func (d *burstEarlySpeakerDecider) Decide(ctx context.Context, req DecisionRequest) DecisionReply {
	_ = ctx
	d.mu.Lock()
	d.asked++
	d.mu.Unlock()
	time.Sleep(d.sleep)
	top := ""
	if len(req.Ranking.Candidates) > 0 {
		top = req.Ranking.Candidates[0].MessageID
	}
	return DecisionReply{
		Msg: &session.DecisionMsg{
			RegionID:              req.RegionID,
			ObservationSeq:        req.ObservationSeq,
			MessageID:             top,
			SpeakerID:             "char-boy",
			SpeakerConfidence:     0.9,
			SpeakerKnown:          true,
			SpeakerKind:           "boy",
			SpeakerKindConfidence: 0.9,
			SpeechEligible:        true,
			Accepted:              true,
			AttributionOnly:       true,
			Provider:              "scripted",
			Model:                 "scripted",
			Reason:                "answered",
		},
		Provider:              "scripted",
		RequestID:             "scripted-req",
		Model:                 "scripted",
		Accepted:              true,
		Reason:                "answered",
		ChosenID:              top,
		Speaker:               "char-boy",
		SpeakerConfidence:     0.9,
		SpeakerKnown:          true,
		SpeakerKind:           "boy",
		SpeakerKindConfidence: 0.9,
	}
}

func (d *burstEarlySpeakerDecider) askCount() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.asked
}

// TestEarlySpeakerBurstReadsOfOneBoxStayFastWithSleepingDecider is the
// ADR-0160 throughput half of the review-redesign: a Decider that answers late
// (2 s here) must not serialize the rank worker behind the burst of reads of
// one typing box. The first read's early ask carries the sleeping decider and
// answers through it; the reads after it are deduped per occurrence and must
// be replied to without waiting on the answer — their rank→reply latency
// stays flat instead of queueing behind the sleep.
func TestEarlySpeakerBurstReadsOfOneBoxStayFastWithSleepingDecider(t *testing.T) {
	dec := &burstEarlySpeakerDecider{sleep: 2 * time.Second}
	idx, err := matcher.Build([]games.Message{{
		ID:             "synthmsg-harbour",
		OriginalText:   "the boy shouts back",
		NormalizedText: "the boy shouts back",
		SpeechText:     "the boy shouts back",
	}}, nil, matcher.IndexOptions{})
	if err != nil {
		t.Fatalf("build index: %v", err)
	}
	r, err := New(Config{
		SessionID:             "burstdemo",
		ProfileRevision:       "1",
		Regions:               []games.Region{{ID: "bottom", RegionType: games.RegionTextbox, RectX: 0, RectY: 0, RectW: 1, RectH: 1}},
		Index:                 idx,
		OCR:                   burstOCR{},
		Decider:               dec,
		Reducer:               session.Config{Adjudicate: false},
		SharedSpeakerMessages: map[string]bool{"synthmsg-harbour": true},
		Speakers:              []SpeakerOption{{ID: "char-boy", Description: "a boy"}},
	})
	if err != nil {
		t.Fatalf("new runner: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ranks := make(chan rankJob, 4)
	replies := make(chan rankIn, 4)
	go r.rankWorker(ctx, ranks, replies)

	// The burst: the first read's ask carries the sleeping decider; the
	// following reads of the same box drop through the in-flight dedupe and
	// must not queue behind the sleep — their rank→reply latency stays flat.
	var first time.Duration
	for i := 0; i < 3; i++ {
		ranks <- rankJob{req: rankRequest{
			region:      "bottom",
			obsSeq:      int64(i),
			text:        "the boy shouts back",
			scene:       "outset",
			gen:         1,
			entered:     time.Now(),
			ocrAt:       time.Now(),
			frameSeq:    int64(i),
			captureTSNS: int64(i) * 1e9,
			kindHint:    games.TextDialogue,
		}, raw: "the boy shouts back", conf: 0.9}
		start := time.Now()
		select {
		case <-replies:
			elapsed := time.Since(start)
			if i == 0 {
				first = elapsed
			} else if elapsed > time.Second {
				t.Fatalf("rank→reply latency of burst read %d was %s — it queued behind the sleeping decider", i, elapsed)
			}
		case <-ctx.Done():
			t.Fatalf("reply %d never arrived", i)
		}
	}
	if dec.askCount() != 1 {
		t.Fatalf("early speaker asks = %d, want exactly 1 per box occurrence across the burst", dec.askCount())
	}
	if first < 1500*time.Millisecond {
		t.Fatalf("the first read's reply came back in %s — behind the sleeping decider's 2 s answer?", first)
	}
}
