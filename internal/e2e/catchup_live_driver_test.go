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

// TestCuDecidePressSilentStaticBoxOnScreenStillPresses: a fully typed box
// that sits unchanged produces no text.observed at all; when the driver's own
// screen check sees the box, the silence is a static box and the lost-A retry
// fires (the 2026-10-23 sweep run stopped pressing on such a box and ended
// the conversation with the box still on screen).
func TestCuDecidePressSilentStaticBoxOnScreenStillPresses(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 4, 46, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.commits = append(drv.commits, cuCommitInfo{})
	drv.instPresses = 1 // the rapid press landed while the typewriter ran
	at := t0.Add(cuLostA + time.Second)
	if !drv.needsScreen(at) {
		t.Fatal("needsScreen = false after a silent stretch; want a screen check")
	}
	drv.noteScreen(at, true)
	press, why := drv.decidePress(nil, at)
	if !press || !strings.Contains(why, "lost-A") {
		t.Fatalf("decidePress on a silent box the screen still shows = %v (%q); want the lost-A retry", press, why)
	}
	// Past cuConvGone of silence the box on screen still keeps the drive on.
	late := t0.Add(cuConvGone + time.Second)
	drv.noteScreen(late, true)
	if _, why := drv.decidePress(nil, late); drv.done {
		t.Fatalf("the drive ended (%q) with the box still on screen", why)
	}
}

// TestCuDecidePressSilentNoBoxOnScreenEndsTheDrive: silence with no box on
// the screen check is the closed box: no fallback press, and the drive ends
// after cuConvGone.
func TestCuDecidePressSilentNoBoxOnScreenEndsTheDrive(t *testing.T) {
	t0 := time.Date(2026, 10, 23, 4, 46, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.commits = append(drv.commits, cuCommitInfo{})
	at := t0.Add(cuStallGap + time.Second)
	drv.noteScreen(at, false)
	if press, why := drv.decidePress(nil, at); press {
		t.Fatalf("decidePress pressed (%q) with no box on screen", why)
	}
	late := t0.Add(cuConvGone + time.Second)
	drv.noteScreen(late, false)
	if _, why := drv.decidePress(nil, late); !drv.done {
		t.Fatalf("the drive did not end (%q) after %s of silence with no box on screen", why, cuConvGone)
	}
}

// cuStubCommit appends one settled (matched) line to the driver's view —
// the box's progress the mash gate waits on before the first cadence
// press (the commit is the evidence the session recognises boxes).
func cuStubCommit(drv *cuDriver, msg string) {
	drv.matched = append(drv.matched, cuCommitInfo{Occurrence: "occ-" + msg, MessageID: msg, Generation: 1, CommittedAt: time.Now()})
	drv.commits = append(drv.commits, cuCommitInfo{Occurrence: "occ-" + msg, MessageID: msg, Generation: 1})
}

// TestCuMashNoPressBeforeFirstCommit pins the mash gate: with no commit
// yet the cadence does not start (the commit is the evidence the session
// recognises boxes); the box just sits, no press.
func TestCuMashNoPressBeforeFirstCommit(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.mash = true
	// Well past the settle gate, still no commit: no press.
	if press, why := drv.decidePress(nil, t0.Add(cuMashSettle+time.Second)); press {
		t.Fatalf("mash pressed (%q) before the first line committed; the cadence starts on the first commit", why)
	}
}

// TestCuMashGrowthHoldsTheSettleGate pins the growth read: while the box
// text is still growing (a new reading keeps refreshing textChanged), no
// mash press lands — the gate waits for the growth to STOP, not for the
// instance's appearance.
func TestCuMashGrowthHoldsTheSettleGate(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.mash = true
	cuStubCommit(drv, "m-1")
	// The typewriter's next reading lands 0.6 s in: the settle gate now
	// runs from that instant, not from the box's appearance.
	growth := `{"dialogue_occurrence_id":"occ-1","region_id":"box","generation":1,"text":"Greetings, brave sailor, the sea calls out to you!","region_present":true}`
	drv.observe([]devEnvelope{{Name: "text.observed", Data: json.RawMessage(growth)}})
	drv.textChanged = t0.Add(600 * time.Millisecond) // pin the stub obs's stamp (observe stamps the wall clock)
	drv.lastObs = t0.Add(600 * time.Millisecond)
	// t0+1.2 s is settled from the APPEARANCE but not from the last growth.
	if press, why := drv.decidePress(nil, t0.Add(cuMashSettle)); press {
		t.Fatalf("mash pressed (%q) while the growth's settle gate is still open", why)
	}
	// Past the growth's gate the first press lands.
	if press, why := drv.decidePress(nil, t0.Add(600*time.Millisecond+cuMashSettle)); !press {
		t.Fatalf("mash did not press (%q) once the text stopped growing for %s", why, cuMashSettle)
	}
}

// TestCuMashPressesOnFastCadenceAfterSettle pins the cadence: a settled
// (silent, growth unobservable) box gets the first mash press at the
// fixed 1.2 s from the instance's appearance, then presses at most every
// 350 ms on the same box instance — independent of any speech state (the
// driver has no speech input at all).
func TestCuMashPressesOnFastCadenceAfterSettle(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.mash = true
	cuStubCommit(drv, "m-1")
	// Before the settle gate: no press.
	if press, why := drv.decidePress(nil, t0.Add(cuMashSettle-time.Second)); press {
		t.Fatalf("mash pressed (%q) before the %s text-settle gate", why, cuMashSettle)
	}
	// The gate opens: press 1. The cadence arms from this instant.
	want, why := drv.decidePress(nil, t0.Add(cuMashSettle))
	if !want {
		t.Fatalf("mash did not press (%q) at the settle gate on a settled box", why)
	}
	if !strings.Contains(why, "mash 1/"+itoa(int64(cuMashMaxPresses))) {
		t.Fatalf("first mash press reason = %q, want the 1/bound mash shape", why)
	}
	// Mid-cadence: held (the next press is allowed at +cuMashGap).
	if press, why := drv.decidePress(nil, t0.Add(cuMashSettle+200*time.Millisecond)); press {
		t.Fatalf("mash pressed (%q) inside the %s cadence", why, cuMashGap)
	}
	// On the cadence: press 2, then press 3 on the next tick.
	for i, at := range []time.Duration{cuMashSettle + cuMashGap, cuMashSettle + 2*cuMashGap} {
		if press, why := drv.decidePress(nil, t0.Add(at)); !press {
			t.Fatalf("mash did not press (%q) on the %s cadence tick %d", why, cuMashGap, i+2)
		}
	}
	if drv.instPresses != 3 {
		t.Fatalf("instPresses = %d, want 3 (one per cadence tick landed)", drv.instPresses)
	}
}

// TestCuMashBoundedPerBox pins the bound: after cuMashMaxPresses cadence
// presses on one box instance the cadence stops (the stall press covers
// the never-committing box, separately — not now).
func TestCuMashBoundedPerBox(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.mash = true
	cuStubCommit(drv, "m-1")
	at := t0.Add(cuMashSettle)
	for i := 0; i < cuMashMaxPresses; i++ {
		if press, why := drv.decidePress(nil, at); !press {
			t.Fatalf("mash press %d did not land (%q)", i+1, why)
		}
		at = at.Add(cuMashGap)
	}
	press, why := drv.decidePress(nil, at.Add(cuMashGap))
	if press {
		t.Fatalf("mash pressed a %dth time on the same box instance (%q); the per-box bound is %d", cuMashMaxPresses+1, why, cuMashMaxPresses)
	}
	if !strings.Contains(why, "bound is spent") {
		t.Fatalf("no-press reason = %q, want the spent-bound shape", why)
	}
	// A NEW box resets the instance: the next settled box gets the cadence
	// again (the drive walks the whole conversation, box by box).
	next := `{"dialogue_occurrence_id":"occ-9","region_id":"box","generation":9,"text":"The tide is turning, sailor; trust your course.","region_present":true}`
	drv.observe([]devEnvelope{{Name: "text.observed", Data: json.RawMessage(next)}})
	drv.textChanged = at.Add(cuMashGap)
	drv.lastObs = at.Add(cuMashGap)
	cuStubCommit(drv, "m-9")
	if drv.instPresses != 0 {
		t.Fatalf("instPresses = %d on the new box, want 0 (the instance resets on the new text)", drv.instPresses)
	}
	if press, why := drv.decidePress(nil, at.Add(cuMashGap+cuMashSettle)); !press || !strings.Contains(why, "mash 1/") {
		t.Fatalf("mash on the new box = %v (%q), want the cadence running again from press 1", press, why)
	}
}

// TestCuMashHoldsDuringTransition pins the transition hold: the box region
// reads empty (the between-box gap) — no mash press lands on it (a press
// into a closed box would skip the next box's line), and the settle gate
// of the NEXT box restarts from that box's own appearance.
func TestCuMashHoldsDuringTransition(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.mash = true
	cuStubCommit(drv, "m-1")
	drv.observe([]devEnvelope{{Name: "text.observed", Data: json.RawMessage(cuStubBoxAbsent)}})
	at := t0.Add(cuMashSettle + cuMashGap)
	if press, why := drv.decidePress(nil, at); press {
		t.Fatalf("mash pressed (%q) while the box region reads empty (the transition); a press there skips the next box's line", why)
	}
}

// TestCuMashEndsWhenBoxGone pins the stop rule: after the conversation
// ends (the box gone — the ADR-0163 box-gone + screen-check rules), the
// mash cadence does not press into gameplay; the drive ends.
func TestCuMashEndsWhenBoxGone(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	drv := cuStubObs(t, t0, cuStubBoxText)
	drv.boxRegion = "box"
	drv.mash = true
	cuStubCommit(drv, "m-1")
	at := t0.Add(cuConvGone + time.Second) // past cuConvGone with no fresh observation
	if press, why := drv.decidePress(nil, at); press || !drv.done {
		t.Fatalf("decidePress = (%v, %q), done = %v on a box gone for %s; want no press and the drive ended (the box-gone rules govern the mash drive)", press, why, drv.done, cuConvGone)
	}
}
