package runtime

// Profile pre-synthesis (ADR 0153): a pre-synthesized line is a cache hit
// for the live commit that speaks it (the shared key identity, lineCacheKey),
// a second pass costs no provider call, and a voiceless line is skipped.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

func TestPresynthOneThenCommitHitsTheCache(t *testing.T) {
	e := newCatchUpEnv(t, 0.2, "catch_up", 0, 0, nil)
	ctx := context.Background()
	text := "A line voiced before anyone plays."

	hit, n, skipped, err := e.w.presynthOne(ctx, "m-pre", text)
	if err != nil || skipped || hit || n == 0 {
		t.Fatalf("first presynth = hit %v, %d bytes, skipped %v, err %v; want a fresh synthesis", hit, n, skipped, err)
	}
	if got, _ := e.tts.requests(); len(got) != 1 {
		t.Fatalf("provider requests after the first pass = %d, want 1", len(got))
	}
	// A second pass is a cache lookup, not a provider call (resumability).
	hit, _, _, err = e.w.presynthOne(ctx, "m-pre", text)
	if err != nil || !hit {
		t.Fatalf("second presynth = hit %v, err %v; want a cache hit", hit, err)
	}
	if got, _ := e.tts.requests(); len(got) != 1 {
		t.Fatalf("provider requests after the second pass = %d, want still 1", len(got))
	}

	// The live commit of the same line under the same canonical text speaks
	// from the cache: the key the job booked is the key the commit computes.
	e.commit(t, "occ-pre", "m-pre", "bottom", 1, text)
	e.waitEvents(t, "the committed line was ready from the cache", 5*time.Second, func(evs []events.Envelope) bool {
		for _, ev := range evs {
			if ev.Name != events.SpeechReady {
				continue
			}
			var d map[string]any
			_ = json.Unmarshal(ev.Data, &d)
			if d["dialogue_occurrence_id"] == "occ-pre" {
				return d["from_cache"] == true
			}
		}
		return false
	})
	if got, _ := e.tts.requests(); len(got) != 1 {
		t.Fatalf("provider requests after the commit = %d, want still 1 (the commit must hit the presynthesized audio)", len(got))
	}
}

func TestPresynthOneSkipsAVoicelessSpeaker(t *testing.T) {
	e := newCatchUpEnv(t, 0.2, "catch_up", 0, 0, map[string]string{"m-x": "ch-without-voice"})
	e.w.narratorFallback = false
	_, _, skipped, err := e.w.presynthOne(context.Background(), "m-x", "Nobody voices this speaker.")
	if err != nil || !skipped {
		t.Fatalf("presynth of a voiceless speaker = skipped %v, err %v; want skipped", skipped, err)
	}
	if got, _ := e.tts.requests(); len(got) != 0 {
		t.Fatalf("provider requests = %d, want 0", len(got))
	}
}
