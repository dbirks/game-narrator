package runtime

// The cross-region duplicate guard (#41): a profile that declares the box at
// two overlapping on-screen positions sees one box from both regions, and
// both commit the same line ~100-200 ms apart. The A/V harness run
// av-20261021T190256Z transcribed the result — every such line spoken twice
// in one merged catch-up run. The guard routes the first copy and gives the
// second its one terminal (skipped, duplicate_region) without routing it.

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

func newDedupeWiring(t *testing.T) (*speechWiring, *events.MemStore) {
	t.Helper()
	mem := events.NewMemStore()
	hub := events.New(events.WithStore(mem))
	t.Cleanup(func() { _ = hub.Close() })
	return &speechWiring{
		id:           "sess-dedupe",
		log:          func(string, ...any) {},
		hub:          hub,
		endCtx:       context.Background(),
		recentRouted: map[string]routedMark{},
		settled:      map[int64]bool{},
		byGeneration: map[int64]string{},
	}, mem
}

func TestCrossRegionDuplicateSuppressesSecondRegionOnly(t *testing.T) {
	w, mem := newDedupeWiring(t)
	first := &utteranceInfo{occurrence: "occ-a", messageID: "m-1", regionID: "r-low", generation: 1}
	if w.crossRegionDuplicate("m-1", first) {
		t.Fatal("the first copy was reported as a duplicate")
	}
	// Same line, other region, inside the window: the duplicate.
	dup := &utteranceInfo{occurrence: "occ-b", messageID: "m-1", regionID: "r-high", generation: 2}
	if !w.crossRegionDuplicate("m-1", dup) {
		t.Fatal("the second region's copy was routed (the line would be spoken twice)")
	}
	// Same line, SAME region (a repeat line after an advance): not a duplicate.
	again := &utteranceInfo{occurrence: "occ-c", messageID: "m-1", regionID: "r-low", generation: 3}
	if w.crossRegionDuplicate("m-1", again) {
		t.Fatal("a same-region repeat was suppressed (repeat lines must re-speak)")
	}
	// Other region but outside the window: a real repeat, routed.
	w.recentRouted["m-1"] = routedMark{region: "r-low", at: time.Now().Add(-crossRegionDupWindow - time.Second)}
	late := &utteranceInfo{occurrence: "occ-d", messageID: "m-1", regionID: "r-high", generation: 4}
	if w.crossRegionDuplicate("m-1", late) {
		t.Fatal("a cross-region repeat outside the window was suppressed")
	}

	// The duplicate's one terminal: speech.ended skipped/duplicate_region on
	// its own occurrence, and its generation settled.
	evs, err := mem.After(context.Background(), 0, events.Filter{SessionID: "sess-dedupe"}, 100)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	n := 0
	for _, ev := range evs {
		if ev.Name != events.SpeechEnded {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(ev.Data, &m)
		if m["dialogue_occurrence_id"] != "occ-b" || m["result"] != "skipped" || m["reason"] != "duplicate_region" {
			t.Fatalf("unexpected terminal %s", ev.Data)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("%d duplicate terminals, want exactly 1", n)
	}
	if !w.settled[2] {
		t.Fatal("the duplicate's generation was not settled")
	}
}
