package runtime

// ADR-0186 (issue #41): the cross-region duplicate guard's 3 s window missed
// the A/V bundles' slow cross-region commits. The box sits where the
// profile's middle and bottom regions overlap; both regions commit the same
// message+page, 5.5-8.2 s apart (the box still on screen, no other line in
// between), and today both commits are routed — the line is spoken twice.
// The guard must skip the second region's commit with duplicate_region, while
// a legitimately repeated line (a different line in between, or the box
// closed and reopened) still speaks.

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
)

// TestCrossRegionDuplicateSlowCommitGap reproduces the bundle timing: the
// middle region routes the page, the bottom region commits the same
// message+page 5.5 s later (the slow-drive gap) while the box is still on
// screen and no other line routed in between. Today: routed — spoken twice.
func TestCrossRegionDuplicateSlowCommitGap(t *testing.T) {
	w, mem := newDedupeWiring(t)
	mid := &utteranceInfo{occurrence: "occ-mid", messageID: "m-slow", regionID: "r-mid", generation: 10}
	if w.crossRegionDuplicate("m-slow#p1", mid) {
		t.Fatal("the first copy was reported as a duplicate")
	}
	// The bottom region's commit of the same page arrives 5.5 s later — the
	// bundle's slow-drive gap — with no intervening line (the mark is still
	// the freshest routing).
	w.recentRouted["m-slow#p1"] = routedMark{region: "r-mid", at: time.Now().Add(-5500 * time.Millisecond)}
	bot := &utteranceInfo{occurrence: "occ-bot", messageID: "m-slow", regionID: "r-bot", generation: 11}
	if !w.crossRegionDuplicate("m-slow#p1", bot) {
		t.Fatal("the second region's late commit of the same page was routed (the line is spoken twice)")
	}

	// The duplicate's one terminal: speech.ended skipped/duplicate_region on
	// its own occurrence, and its generation settled.
	evs, err := mem.After(t.Context(), 0, events.Filter{SessionID: "sess-dedupe"}, 100)
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
		if m["dialogue_occurrence_id"] != "occ-bot" || m["result"] != "skipped" || m["reason"] != "duplicate_region" {
			t.Fatalf("unexpected terminal %s", ev.Data)
		}
		n++
	}
	if n != 1 {
		t.Fatalf("%d duplicate terminals, want exactly 1", n)
	}
	if !w.settled[11] {
		t.Fatal("the duplicate's generation was not settled")
	}
}

// TestCrossRegionDuplicateLegitRepeatsStillSpeak: a different line routed in
// between makes the same page from the other region a repeat line, not the
// same box read through two regions — it must speak. Same for the box
// closed and reopened from the region that committed it.
func TestCrossRegionDuplicateLegitRepeatsStillSpeak(t *testing.T) {
	w, _ := newDedupeWiring(t)

	// A different line in between: the box advanced, so the other region's
	// commit of the first page again is a real repeat line.
	a := &utteranceInfo{occurrence: "occ-a", messageID: "m-a", regionID: "r-mid", generation: 20}
	if w.crossRegionDuplicate("m-a#p1", a) {
		t.Fatal("the first line was reported as a duplicate")
	}
	b := &utteranceInfo{occurrence: "occ-b", messageID: "m-b", regionID: "r-mid", generation: 21}
	if w.crossRegionDuplicate("m-b#p1", b) {
		t.Fatal("the intervening line was reported as a duplicate")
	}
	again := &utteranceInfo{occurrence: "occ-c", messageID: "m-a", regionID: "r-bot", generation: 22}
	if w.crossRegionDuplicate("m-a#p1", again) {
		t.Fatal("a repeated line after a different line in between was suppressed (it must re-speak)")
	}

	// The box closed and reopened: the region that committed it re-shows the
	// same page as a new occurrence — a same-region re-commit speaks.
	ka := &utteranceInfo{occurrence: "occ-k1", messageID: "m-k", regionID: "r-mid", generation: 30}
	if w.crossRegionDuplicate("m-k#p1", ka) {
		t.Fatal("the first showing was reported as a duplicate")
	}
	// The overlapping other region's copy of the still-open box is the
	// duplicate: skipped.
	kb := &utteranceInfo{occurrence: "occ-k2", messageID: "m-k", regionID: "r-bot", generation: 31}
	if !w.crossRegionDuplicate("m-k#p1", kb) {
		t.Fatal("the overlapping region's copy of the open box was routed (spoken twice)")
	}
	// The box closed and reopened: the same region re-commits the same page —
	// a new occurrence, must speak.
	kc := &utteranceInfo{occurrence: "occ-k3", messageID: "m-k", regionID: "r-mid", generation: 32}
	if w.crossRegionDuplicate("m-k#p1", kc) {
		t.Fatal("the reopened box was suppressed (a re-shown line must re-speak)")
	}
}
