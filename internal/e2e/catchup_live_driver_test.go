// Package e2e — the cuDriver's decidePress unit tests: the rapid-A advance
// driver's box-gone / stall / lost-A press rules over synthetic event pages
// (the developer API's dev-events shape). No emulator, no gateway, no bridge:
// the driver is the headless session's stand-in and these tests pin its
// decision table.

package e2e

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// The synthetic text.observed event pages the press tests fold through
// observe (the developer API's dev-events route shape): the box region's
// reading and the presence flag the payload carries.
const (
	cuStubBoxText   = `{"dialogue_occurrence_id":"occ-1","region_id":"box","generation":1,"text":"Greetings, brave sailor, the sea calls!","region_present":true}`
	cuStubBoxAbsent = `{"dialogue_occurrence_id":"occ-1","region_id":"box","generation":1,"text":"","region_present":false}`
)

// cuStubObs arms the driver with one box reading at the synthetic t0 (the
// observation stamps the wall clock, so the derived stability clocks are
// pinned onto t0 afterwards).
func cuStubObs(t *testing.T, t0 time.Time, data string) *cuDriver {
	t.Helper()
	drv := cuNewDriver(t0)
	drv.observe([]devEnvelope{{Name: "text.observed", Data: json.RawMessage(data)}})
	drv.textChanged = t0
	drv.instAppeared = t0
	drv.lastObs = t0
	return drv
}

// TestCuDecidePressStaleTextNoPressAfterQuiet pins the box-gone fix: when the
// conversation ends and the box closes, the observations stop, and the stale
// lastText must read as gone — no stall/lost-A press may fire off it (the
// 2026-10-22 run pressed A into gameplay on that stale text).
func TestCuDecidePressStaleTextNoPressAfterQuiet(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	// The conversation ended ~5 s after the last observation (the box went
	// quiet past the press-stability window): the stale text must not read as
	// a still-stable box and press A into gameplay.
	want, why := drv.decidePress(nil, t0.Add(cuStallGap+5*time.Second))
	if want {
		t.Fatalf("decidePress pressed (%q) on stale box text with no fresh observation; want no press (the box is gone)", why)
	}
}

// TestCuDecidePressStallOnStablePresentText pins the stall fallback's
// unchanged behaviour: a box that is still present, whose text has been stable
// with no commit for cuStallGap, still gets the stall press.
func TestCuDecidePressStallOnStablePresentText(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	// One later reading of the same (still stable) text refreshes the
	// freshness clock; the decision lands cuStallGap after the instance
	// appeared, with no commit landed.
	drv.lastObs = t0.Add(cuStallGap - time.Second)
	want, why := drv.decidePress(nil, t0.Add(cuStallGap))
	if !want {
		t.Fatalf("decidePress = no press (%q) on a still-present stable box with no commit for %s; want the stall press", why, cuStallGap)
	}
	if !strings.Contains(why, "stall") {
		t.Fatalf("decidePress stall reason = %q, want the stall press shape", why)
	}
}

// TestCuDecidePressBoxAbsentRegionNoPress pins the region-presence signal:
// a newest observation that reports the box region absent is a gone box, and
// no stall/lost-A press fires off it.
func TestCuDecidePressBoxAbsentRegionNoPress(t *testing.T) {
	t0 := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	drv := cuNewDriver(t0)
	drv.lastText = cuStubBoxText
	drv.textChanged = t0
	drv.instAppeared = t0
	drv.lastObs = t0
	drv.lastRegionPresent = false // the newest observation reported the box absent

	want, why := drv.decidePress(nil, t0.Add(5*time.Second))
	if want {
		t.Fatalf("decidePress = press (%q) on a box the last observation reported absent; want no press (the box is gone)", why)
	}
}

// TestCuDecidePressOtherRegionAbsentKeepsTheBox: the profile has several
// textbox regions; a different region reporting absent while the box region
// holds the text must not read as the box gone.
func TestCuDecidePressOtherRegionAbsentKeepsTheBox(t *testing.T) {
	t0 := time.Now()
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	other := `{"dialogue_occurrence_id":"occ-1","region_id":"other","generation":1,"text":"","region_present":false}`
	drv.observe([]devEnvelope{{Name: "text.observed", Data: json.RawMessage(other)}})
	if !drv.lastRegionPresent {
		t.Fatal("another region's absent reading cleared the box region's presence")
	}
}

// TestCuDecidePressBetweenBoxGapDoesNotEndTheConversation: the box region
// reads absent for a few seconds between boxes; that must not end the drive
// (a run on 2026-10-23 stopped after 2 presses on it).
func TestCuDecidePressBetweenBoxGapDoesNotEndTheConversation(t *testing.T) {
	t0 := time.Now()
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.commits = append(drv.commits, cuCommitInfo{})
	drv.observe([]devEnvelope{{Name: "text.observed", Data: json.RawMessage(cuStubBoxAbsent)}})
	drv.lastObs = t0
	if press, why := drv.decidePress(nil, t0.Add(5*time.Second)); press || drv.done {
		t.Fatalf("a 5 s between-box gap: press %v (%s), done %v; want no press and the conversation still on", press, why, drv.done)
	}
}
