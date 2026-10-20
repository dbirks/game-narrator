package session

// The mid-session operator policy (docs/spec/09 "/play"): the narrated
// classes and the decision mode, applied by one settings control on the
// session's single-writer chain. The synthetic input is the harness's (this
// package's unit-test seam, AGENTS.md rule 3 class (a)); the reducer's logic
// is the real one.

import (
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/games"
)

// TestSettingsControlMovesEligibility proves the narrated-class veto end to
// end at the layer that decides it: a committed line whose class is not
// narrated still commits (the box advances, the text machine knows) but
// carries speech_eligible false — the speech layer's no-audio answer — and
// the same class commits eligible once the operator adds it. The dialogue
// class is the product and stays eligible throughout: the policy never
// silences a character's line.
func TestSettingsControlMovesEligibility(t *testing.T) {
	h := newHarness(t, DefaultConfig(), WithMessageKind(func(id string) (games.TextKind, bool) {
		if id == "msg-sign" || id == "msg-sign-2" {
			return games.TextSign, true
		}
		return games.TextDialogue, true
	}))

	// The default policy narrates dialogue alone: a sign commits, silently.
	h.settle("A SIGN ON THE BOAT", 0.9)
	res := h.rank("msg-sign", 0.5, false)
	if !res.Applied || !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("the sign did not commit: %s", res.Reason)
	}
	committed := h.pub.byName(events.MessageCommitted)[0]
	if got := fieldStr(t, committed, "text_kind"); got != "sign" {
		t.Fatalf("committed text_kind = %q, want sign", got)
	}
	if fieldBool(t, committed, "speech_eligible") {
		t.Fatal("speech_eligible = true for a non-narrated class; the veto is the operator's, not the policy's default")
	}
	// The speech layer takes the (absent) audio exactly as far as it gets: the
	// line's utterance is ready, and the fence it sets is what the next advance
	// cancels.
	occ := fieldStr(t, committed, "dialogue_occurrence_id")
	h.gen = h.r.Generation()
	h.speech(occ, SpeechReady, "bottom")

	// The operator adds signs. One settings control, one event, the applied
	// state normalized (dialogue first, always present).
	h.pub.reset()
	res = h.controlSettings(false, games.TextDialogue, games.TextSign)
	if !res.Applied {
		t.Fatalf("settings control refused: %s", res.Reason)
	}
	changed := h.pub.byName(events.SessionSettings)
	if len(changed) != 1 {
		t.Fatalf("settings applied %d events, want 1 (%v)", len(changed), namesOf(h.pub.published))
	}
	if got := fieldStr(t, changed[0], "mode"); got != "conservative" {
		t.Fatalf("event mode = %q, want conservative", got)
	}
	classes, ok := dataOf(t, changed[0])["narrated_classes"].([]any)
	if !ok || len(classes) != 2 || classes[0].(string) != "dialogue" || classes[1].(string) != "sign" {
		t.Fatalf("event narrated_classes = %v, want the normalized [dialogue sign]", dataOf(t, changed[0])["narrated_classes"])
	}
	if st := h.r.State(); st.PredictiveMode || len(st.NarratedKinds) != 2 {
		t.Fatalf("state after settings = predictive %v kinds %v, want conservative with the two classes", st.PredictiveMode, st.NarratedKinds)
	}

	// The next sign is narrated: the same evidence, the eligibility moved.
	h.settle("A SECOND SIGN", 0.9)
	res = h.rank("msg-sign-2", 0.5, false)
	if !res.Applied || !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("the second sign did not commit: %s", res.Reason)
	}
	// (pub was reset before the control, so this commit is the first in the
	// publisher's memory.)
	second := h.pub.byName(events.MessageCommitted)[0]
	if !fieldBool(t, second, "speech_eligible") {
		t.Fatal("speech_eligible = false after the operator added the class: the toggle changed nothing")
	}

	// A request that names the state the session already has is a duplicate,
	// not a change: no second event, because a settings event that changed
	// nothing would be a lie in the log.
	h.pub.reset()
	res = h.controlSettings(false, games.TextSign, games.TextDialogue)
	if res.Reason != "duplicate_control" {
		t.Fatalf("duplicate settings reason = %q, want duplicate_control", res.Reason)
	}
	if got := len(h.pub.byName(events.SessionSettings)); got != 0 {
		t.Fatalf("a no-op settings sent %d events, want 0", got)
	}
}

// TestSettingsControlMovesTheMode proves the conservative/predictive half:
// the one control carries the mode to the text machines (the state the
// pipeline's ranking gate reads) and the event names the applied mode, so
// the developer UI and the event log cannot disagree with the snapshot.
func TestSettingsControlMovesTheMode(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	if h.r.State().PredictiveMode {
		t.Fatal("a default session is conservative; the fixture drifted")
	}

	h.settle("A LINE", 0.9)
	h.pub.reset()
	res := h.controlSettings(true, games.TextDialogue)
	if !res.Applied {
		t.Fatalf("mode switch refused: %s", res.Reason)
	}
	changed := h.pub.byName(events.SessionSettings)
	if len(changed) != 1 || fieldStr(t, changed[0], "mode") != "predictive" {
		t.Fatalf("the mode switch sent %v, want one event with mode predictive", namesOf(h.pub.published))
	}
	if !h.r.State().PredictiveMode {
		t.Fatal("the state still reads conservative after the switch: the text machines did not move")
	}

	// And back: the machines follow the control in both directions.
	h.pub.reset()
	if res := h.controlSettings(false, games.TextDialogue); !res.Applied {
		t.Fatalf("mode switch back refused: %s", res.Reason)
	}
	if h.r.State().PredictiveMode {
		t.Fatal("the state still reads predictive after the switch back")
	}
	if got := len(h.pub.byName(events.SessionSettings)); got != 1 {
		t.Fatalf("the switch back sent %d events, want 1", got)
	}
}

// TestPausedSnapshotCarriesThePolicy pins the snapshot contract the UI's
// controls read from: a paused session that has taken a settings control
// reports both halves of the operator's policy, so a refresh reopens the
// watch on the server's state, not the page's last click.
func TestPausedSnapshotCarriesThePolicy(t *testing.T) {
	h := newHarness(t, DefaultConfig(), WithMessageKind(func(string) (games.TextKind, bool) {
		return games.TextItem, true
	}))
	h.settle("AN ITEM NOTICE", 0.9)
	h.rank("msg-item", 0.5, false)

	// A resume before the pause is a recorded duplicate, not a fault.
	if res := h.control(ControlResume, ""); res.Applied || res.Reason != "duplicate_control" {
		t.Fatalf("resume on a running session = applied %v reason %q, want the duplicate refusal", res.Applied, res.Reason)
	}
	h.control(ControlPause, "")
	if !h.r.State().Paused {
		t.Fatal("the session is not paused")
	}
	h.controlSettings(true, games.TextDialogue, games.TextItem)
	st := h.r.State()
	if !st.Paused {
		t.Fatal("a settings control moved the pause: the operator's policy and the pause are separate halves")
	}
	if !st.PredictiveMode || len(st.NarratedKinds) != 2 {
		t.Fatalf("snapshot after the control = %v, want predictive with dialogue and item", st)
	}
}
