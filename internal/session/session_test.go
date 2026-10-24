package session

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/matcher"
	"github.com/dbirks/game-narrator/internal/providers/systemone"
	"github.com/dbirks/game-narrator/internal/recognition"
)

// TestCommitOnceAcrossFiftyRepeatedFrames is docs/spec/16 M2's gate line — "text
// state machine … repeat observation doesn't create duplicate speech" — and the
// requirement's own first scripted timeline: a box appears, types out, stabilizes,
// reaches READY, and commits exactly once; fifty further frames of the same OCR
// change nothing.
//
// The tracker is the reducer's, driven by the same Observation values the ROI
// worker will produce, so the frame stream that commits here is the frame stream
// that commits in a live session. The ranking is the only thing faked, and it is
// faked as a *message* (what the ranking worker really sends), not as a stubbed
// interface inside the commit path.
func TestCommitOnceAcrossFiftyRepeatedFrames(t *testing.T) {
	h := newHarness(t, DefaultConfig())

	// Typewriter: six frames of a growing prefix must never look like a new
	// utterance. The last two frames are the stability window closing, and a
	// seventh is the frame whose evidence is allowed to claim READY (a
	// stabilizing claim and a ready claim may not be the same observation twice —
	// see recognition.TestReadyClaimCostsAStabilizingFrame).
	typewriter := []string{"THE SHIP", "THE SHIP S", "THE SHIP SA", "THE SHIP SAILS", "THE SHIP SAILS", "THE SHIP SAILS"}
	var states []recognition.State
	for _, txt := range typewriter {
		h.frame(txt, 0.9)
		states = append(states, h.regionState("bottom"))
	}
	for i := 0; i < 4 && h.regionState("bottom") != recognition.StateReady; i++ {
		h.frame("THE SHIP SAILS", 0.9)
		states = append(states, h.regionState("bottom"))
	}
	if got := h.regionState("bottom"); got != recognition.StateReady {
		t.Fatalf("state after the reveal = %s, want ready (states seen %v)", got, states)
	}
	if !sawState(states, recognition.StateRevealing) || !sawState(states, recognition.StateStabilizing) {
		t.Fatalf("the reveal never passed through revealing and stabilizing: %v", states)
	}

	res := h.rank("demo-001", 0.5, false)
	if !res.Applied {
		t.Fatalf("ranking not applied: %s", res.Reason)
	}
	if !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("no message.committed in %s", joinNames(res.Events))
	}
	committed := res.Events[len(res.Events)-1]
	if got := fieldStr(t, committed, "message_id"); got != "demo-001" {
		t.Fatalf("message_id = %q, want demo-001", got)
	}
	if got := fieldFloat(t, committed, "generation"); got != 1 {
		t.Fatalf("generation = %v, want 1 (the first commit is the first generation)", got)
	}
	if !fieldBool(t, committed, "speech_eligible") {
		t.Fatal("speech_eligible = false for a matched commit")
	}
	if id := fieldStr(t, committed, "dialogue_occurrence_id"); id == "" {
		t.Fatal("message.committed carries no dialogue_occurrence_id")
	}

	for i := 0; i < 50; i++ {
		h.frame("THE SHIP SAILS", 0.9)
	}
	st := h.r.Stats()
	if st.Commits != 1 {
		t.Fatalf("commits after 50 repeated frames = %d, want 1", st.Commits)
	}
	if got := len(h.pub.byName(events.MessageCommitted)); got != 1 {
		t.Fatalf("%d message.committed events, want 1 — a duplicate here is duplicate speech (docs/spec/12)", got)
	}
	if got := h.r.Generation(); got != 1 {
		t.Fatalf("generation = %d, want 1", got)
	}
	if got := len(h.pub.byName(events.TextObserved)); got < 3 {
		t.Fatalf("text.observed events = %d, want the reveal, the stabilization and the ready step", got)
	}
}

// TestTextObservedIsNotEmittedOncePerFrame pins the perception-side backpressure
// rule. At 2–5 Hz OCR (docs/spec/05) an unfiltered text.observed is the loudest
// thing on the SSE stream while repeating what the last one said, and docs/spec/01
// exists to stop exactly that traffic from stalling a slow browser.
func TestTextObservedIsNotEmittedOncePerFrame(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("A QUIET VILLAGE", 0.9)
	h.rank("demo-002", 0.5, false)
	before := len(h.pub.byName(events.TextObserved))
	h.hold("A QUIET VILLAGE", 0.9, 30)
	if after := len(h.pub.byName(events.TextObserved)); after != before {
		t.Fatalf("30 repeated frames emitted %d more text.observed events, want 0", after-before)
	}
}

// twentyPages is one line per page of a walk through a temple, each materially
// different from the one before it.
var twentyPages = []string{
	"THE DOOR OPENS",
	"A GUARD BARS IT",
	"SWORDS CROSS ABOVE",
	"THE STAIRS DESCEND",
	"WATER RUNS BELOW",
	"A LIGHT FLICKERS",
	"SOMETHING BREATHES",
	"THE WALLS ARE WARM",
	"A VOICE SAYS TURN BACK",
	"NO DOOR BEHIND YOU",
	"THE FLOOR IS GLASS",
	"UNDERNEATH THE SEA",
	"A CITY OF BONES",
	"THE GATE RECOGNIZES YOU",
	"IT ASKS YOUR NAME",
	"YOU ANSWER WITH SILENCE",
	"THE GATE OPENS WIDE",
	"A LONG HALL OF MIRRORS",
	"NONE OF THEM REFLECT YOU",
	"ONLY THE DARK DOES",
}

// TestTwentyRapidAdvances is docs/spec/12's "20 rapid textbox page advances":
// twenty commits, nineteen cancellations of the speech the player walked away
// from, a generation counter that moved exactly twenty, and — the part that makes
// the others more than a count — no speech stage from any older generation is
// accepted afterwards.
func TestTwentyRapidAdvances(t *testing.T) {
	// The cancellation-on-advance contract this test grades is the
	// latest_only shape (ADR-0122 decision 2): under the catch_up default the
	// live line is left to run out and the speech side batches the new one, so
	// the session does not cancel here. Pin the policy the test asserts.
	cfg := DefaultConfig()
	cfg.CatchUp = CatchUpPolicyLatestOnly
	h := newHarness(t, cfg)
	occurrences := map[string]bool{}

	// Twenty *materially* different pages. The wording matters: a ChangeThreshold
	// of 0.34 exists so that one misread glyph in a 25-character line is jitter
	// rather than a page turn (recognition.classify), so pages named "PAGE 1 …" and
	// "PAGE 2 …" differ by one character and would correctly not advance. Real pages
	// are different sentences, so they are written as different sentences.
	for page, text := range twentyPages {
		_ = page
		h.settle(text, 0.9)
		res := h.rank(fmt.Sprintf("msg-%02d", page+1), 0.5, false)
		if !hasName(res.Events, events.MessageCommitted) {
			t.Fatalf("page %d (%q) did not commit: %s", page+1, text, joinNames(res.Events))
		}
		occ := fieldStr(t, res.Events[len(res.Events)-1], "dialogue_occurrence_id")
		if occurrences[occ] {
			t.Fatalf("page %d reused occurrence %s: two pages are two occurrences", page+1, occ)
		}
		occurrences[occ] = true
		// The speech layer keeps up exactly as far as it can: the clip for page N
		// is ready before the player flips to page N+1, which is the situation the
		// cancellation exists for.
		h.gen = h.r.Generation()
		h.speech(occ, SpeechReady, "bottom")
	}

	st := h.r.Stats()
	if st.Commits != 20 {
		t.Fatalf("commits = %d, want 20", st.Commits)
	}
	if got := h.r.Generation(); got != 20 {
		t.Fatalf("generation = %d, want 20 (one increment per committed page)", got)
	}
	cancels := h.pub.byName(events.SpeechCancelled)
	if len(cancels) != 19 {
		t.Fatalf("speech.cancelled events = %d, want 19", len(cancels))
	}
	for _, e := range cancels {
		if got := fieldStr(t, e, "reason"); got != CancelDialogueAdvanced {
			t.Fatalf("cancel reason = %q, want %q", got, CancelDialogueAdvanced)
		}
		if fieldFloat(t, e, "generation") < fieldFloat(t, e, "cancelled_generation") {
			t.Fatalf("cancel event names a generation below the one it cancels: %s", string(e.Data))
		}
	}
	// The last page's utterance must not have been cancelled by anything.
	if got := h.regionSpeechPhase("bottom"); got == PhaseCancelled {
		t.Fatal("the newest page's utterance was cancelled by its own commit")
	}

	// Property: no speech stage from an older generation is accepted. Twenty
	// generations of late clips arrive now — from the reducer's point of view they
	// are all stale, and none of them may move state or reach the event log.
	h.pub.reset()
	before := h.r.Stats()
	for gen := int64(1); gen <= 19; gen++ {
		res := h.speechAt(gen, fmt.Sprintf("occ-%06d", gen*3), "bottom", SpeechStarted)
		if res.Applied {
			t.Fatalf("speech from generation %d was applied", gen)
		}
		if res.Reason != "stale_generation" && res.Reason != "unknown_occurrence" {
			t.Fatalf("speech from generation %d rejected for %q, want a fence reason", gen, res.Reason)
		}
	}
	after := h.r.Stats()
	if after.Stale-before.Stale != 19 {
		t.Fatalf("stale counter moved by %d over 19 late clips, want 19 (record(stale), discard)", after.Stale-before.Stale)
	}
	if len(h.pub.published) != 0 {
		t.Fatalf("late speech emitted events: %v", h.pub.names())
	}
}

// TestOutOfOrderRankingIsDiscardedAndRecorded is docs/spec/12's "two OCR futures
// finish out of order" — ranking for observation 2 arriving before ranking for
// observation 1. The first is the answer to the current screen; the second is a
// provider replying about a box that has already moved on, and applying it would
// commit a line the player passed.
func TestOutOfOrderRankingIsDiscardedAndRecorded(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	first := h.frame("OLD PAGE WORDS", 0.9)
	_ = first
	oldSeq := h.lastObs
	h.frame("NEW PAGE ENTIRELY DIFFERENT", 0.9)
	newSeq := h.lastObs

	resNew := h.rankAt(newSeq, "bottom", "NEW PAGE ENTIRELY DIFFERENT", "msg-new", 0.5, false, "")
	if !resNew.Applied {
		t.Fatalf("the current ranking was rejected: %s", resNew.Reason)
	}
	resOld := h.rankAt(oldSeq, "bottom", "OLD PAGE WORDS", "msg-old", 0.5, false, "")
	if resOld.Applied {
		t.Fatal("the superseded ranking was applied")
	}
	if resOld.Reason != "stale_observation" {
		t.Fatalf("reason = %q, want stale_observation", resOld.Reason)
	}
	if n := len(h.pub.byName(events.MessageCandidates)); n != 1 {
		t.Fatalf("message.candidates events = %d, want 1", n)
	}
	st := h.r.State()
	if len(st.Stale) == 0 {
		t.Fatal("nothing recorded about the discarded ranking (docs/spec/17: record(stale), discard)")
	}
	last := st.Stale[len(st.Stale)-1]
	if last.Generation != st.Generation {
		t.Fatalf("recorded notice names generation %d while %d is active", last.Generation, st.Generation)
	}
	if last.Why == "" {
		t.Fatal("recorded notice carries no explanation")
	}
	if got := h.r.Stats().Stale; got != 1 {
		t.Fatalf("stale counter = %d, want 1", got)
	}
}

// TestLateDecisionAfterNewerCommitIsStale: a Clef answer that took longer than the
// player's next page must not be able to change what was said. This is
// docs/spec/12's "request late after session reset" in fast-forward.
func TestLateDecisionAfterNewerCommitIsStale(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("FIRST PAGE TEXT", 0.9)
	firstObs := h.lastObs
	h.rank("msg-1", 0.5, false)
	h.gen = h.r.Generation()
	h.speech(h.occurrence("bottom"), SpeechReady, "bottom")

	h.settle("SECOND PAGE TEXT", 0.9)
	h.rank("msg-2", 0.5, false)
	if h.r.Generation() != 2 {
		t.Fatalf("generation = %d, want 2", h.r.Generation())
	}

	res := h.decide(firstObs, 1, "msg-1", "narrator", true, true)
	if res.Applied {
		t.Fatal("a decision stamped with the superseded generation was applied")
	}
	if res.Reason != "stale_generation" {
		t.Fatalf("reason = %q, want stale_generation", res.Reason)
	}
	if got := h.r.Stats().Stale; got != 1 {
		t.Fatalf("stale = %d, want 1", got)
	}
}

// TestDismissThenSameTextIsANewOccurrence is docs/spec/12's "repeat NPC after
// menu": the identical words in a box that vanished and came back are a different
// dialogue occurrence, and the developer UI needs the ids to prove it. Within one
// box lifetime the same words are one occurrence — the fifty-frame test above
// covers that half, and this one asserts both halves in one timeline.
func TestDismissThenSameTextIsANewOccurrence(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("HELDO THE BRAVE", 0.9)
	first := h.rank("msg-heldo", 0.5, false)
	firstOcc := fieldStr(t, first.Events[len(first.Events)-1], "dialogue_occurrence_id")
	firstTrace := first.Events[len(first.Events)-1].TraceID

	h.hold("HELDO THE BRAVE", 0.9, 6)
	if got := len(h.pub.byName(events.MessageCommitted)); got != 1 {
		t.Fatalf("commits within one box lifetime = %d, want 1", got)
	}

	h.dismiss()
	h.settle("HELDO THE BRAVE", 0.9)
	second := h.rank("msg-heldo", 0.5, false)
	if !hasName(second.Events, events.MessageCommitted) {
		t.Fatalf("the reappearing box never committed: %s", joinNames(second.Events))
	}
	secondOcc := fieldStr(t, second.Events[len(second.Events)-1], "dialogue_occurrence_id")
	if secondOcc == firstOcc {
		t.Fatalf("a reappearing box reused occurrence id %s; it is a new occurrence", firstOcc)
	}
	if second.Events[len(second.Events)-1].TraceID == firstTrace {
		t.Fatal("the new occurrence kept the old trace_id (docs/spec/01: one trace per occurrence)")
	}
	if got := h.r.Stats().DuplicateContent; got != 0 {
		t.Fatalf("duplicate_content_suppressed = %d across a dismissal: the reappearing box is a new occurrence, not a duplicate", got)
	}
}

// TestPageAdvanceInOneBoxGivesANewTrace is docs/spec/01 "High-level events" for
// the case the box-lifetime rule cannot see: a page turn inside one box lifetime
// is a new dialogue occurrence, so the new page's text.observed, message.candidates
// and message.committed must carry a trace_id the previous page never used. One
// trace per occurrence is what the inspector relies on to group one page's events
// together; a trace holding both pages' message.committed would look like one
// occurrence that committed twice.
func TestPageAdvanceInOneBoxGivesANewTrace(t *testing.T) {
	// The advance cancels the advanced-away page's audio in the latest_only
	// shape (ADR-0122 decision 2); under the catch_up default the live line is
	// left to run out and the session does not cancel on the turn. Pin the
	// policy the test asserts (one cancel for the advanced-away page).
	cfg := DefaultConfig()
	cfg.CatchUp = CatchUpPolicyLatestOnly
	h := newHarness(t, cfg)
	h.settle("THE DOOR OPENS", 0.9)
	first := h.rank("msg-01", 0.5, false)
	if !hasName(first.Events, events.MessageCommitted) {
		t.Fatalf("page one did not commit: %s", joinNames(first.Events))
	}
	h.gen = h.r.Generation()
	h.speech(h.occurrence("bottom"), SpeechReady, "bottom")

	h.settle("A GUARD BARS IT", 0.9)
	second := h.rank("msg-02", 0.5, false)
	if !hasName(second.Events, events.MessageCommitted) {
		t.Fatalf("page two did not commit: %s", joinNames(second.Events))
	}

	firstCommit := first.Events[len(first.Events)-1]
	secondCommit := second.Events[len(second.Events)-1]
	if firstCommit.TraceID == "" || secondCommit.TraceID == "" {
		t.Fatal("a message.committed carries no trace_id")
	}
	if firstCommit.TraceID == secondCommit.TraceID {
		t.Fatalf("both pages committed under trace %s: a page advance in one box is a new occurrence with its own trace", firstCommit.TraceID)
	}
	// The advance's own events belong to the new page: the new text.observed and
	// the candidate list that answered it carry the new trace, and the
	// cancellation of page one's audio — the last event of the old occurrence —
	// still carries the old one, with causation crossing the boundary.
	var newPageObs, cands []events.Envelope
	for _, e := range h.pub.published {
		switch e.Name {
		case events.TextObserved:
			if e.TraceID == secondCommit.TraceID {
				newPageObs = append(newPageObs, e)
			}
		case events.MessageCandidates:
			if e.TraceID == secondCommit.TraceID {
				cands = append(cands, e)
			}
		}
	}
	if len(newPageObs) == 0 {
		t.Fatal("page two's text.observed events carry no trace of the commitment: the advance did not start a new trace")
	}
	if len(cands) == 0 {
		t.Fatal("page two's message.candidates carry no trace of the commitment")
	}
	cancels := h.pub.byName(events.SpeechCancelled)
	if len(cancels) != 1 {
		t.Fatalf("speech.cancelled events = %d, want 1 (one cancel for the advanced-away page)", len(cancels))
	}
	if cancels[0].TraceID != firstCommit.TraceID {
		t.Fatalf("the cancellation of page one's audio rides trace %q, want page one's %q", cancels[0].TraceID, firstCommit.TraceID)
	}
	if got := cands[0].CausationID; got == "" {
		t.Fatal("page two's message.candidates carries no causation_id: the chain across the advance is broken")
	}
	// One trace per occurrence, stated over the whole log: every trace holds at
	// most one message.committed.
	commitsByTrace := map[string]int{}
	for _, e := range h.pub.published {
		if e.Name == events.MessageCommitted {
			commitsByTrace[e.TraceID]++
		}
	}
	for trace, n := range commitsByTrace {
		if n != 1 {
			t.Fatalf("trace %s holds %d message.committed events, want 1", trace, n)
		}
	}
	if h.trace("bottom") != secondCommit.TraceID {
		t.Fatalf("region state trace %q is not page two's %q", h.trace("bottom"), secondCommit.TraceID)
	}
}

// TestAbstainingRankingCommitsUnknownAndStaysSilent is docs/spec/05's "never invent
// dialogue to fill uncertainty" made observable: the box commits, the message id
// says unknown, and nothing about it may reach a speaker.
func TestAbstainingRankingCommitsUnknownAndStaysSilent(t *testing.T) {
	// Confidence is legible; it is the *matching* that abstains. Low OCR confidence
	// would never reach READY at all, and this test is about the honest answer to
	// an unresolvable line, not about an unreadable screen.
	h := newHarness(t, DefaultConfig())
	h.settle("YES", 0.9)
	res := h.rank("", 0.0, true)
	if !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("an abstaining ranking produced no commitment: %s", joinNames(res.Events))
	}
	ev := res.Events[len(res.Events)-1]
	if got := fieldStr(t, ev, "message_id"); got != UnknownMessage {
		t.Fatalf("message_id = %q, want %q", got, UnknownMessage)
	}
	if fieldBool(t, ev, "speech_eligible") {
		t.Fatal("an abstained occurrence is speech eligible — that is speaking a guess")
	}
	if got := fieldStr(t, ev, "reason"); got != ReasonCommittedAbstain {
		t.Fatalf("reason = %q, want %q", got, ReasonCommittedAbstain)
	}
	if got := h.r.Generation(); got != 1 {
		t.Fatalf("generation = %d, want 1: the occurrence is real even when unnamed", got)
	}
	// And because nothing was eligible, turning the page must not fabricate a
	// cancellation of audio that was never scheduled.
	h.pub.reset()
	h.settle("SOMETHING ELSE ENTIRELY", 0.9)
	h.rank("msg-next", 0.5, false)
	if n := len(h.pub.byName(events.SpeechCancelled)); n != 0 {
		t.Fatalf("%d speech.cancelled events after an ineligible commit, want 0", n)
	}
}

// TestPredictiveCommitIsOptIn covers both halves of docs/spec/05 "Predictive
// playback policy": the mode is off by default, and even when it is on the only
// thing that can commit a revealing box is the matcher's PREDICTIVE_PREFIX reason
// with a margin over the runner-up.
func TestPredictiveCommitIsOptIn(t *testing.T) {
	t.Run("off_blocks_the_prefix", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		if got := h.regionState("bottom"); got != recognition.StateRevealing {
			t.Fatalf("state = %s, want revealing (a predictive commit must not need a stable line)", got)
		}
		res := h.predictiveRank(h.lastObs, "bottom", h.text("bottom"), "demo-long", 0.6)
		if hasName(res.Events, events.MessageCommitted) {
			t.Fatal("a prefix committed with PredictiveMode off")
		}
		if h.r.Generation() != 0 {
			t.Fatalf("generation moved to %d without a commit", h.r.Generation())
		}
		if got := h.r.Stats().CommitBlocked; got == 0 {
			t.Fatal("the blocked commit was not counted")
		}
	})

	t.Run("on_requires_the_prefix_reason", func(t *testing.T) {
		h := newHarness(t, predictiveConfig(), predictiveTrackerOption())
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		res := h.rank("demo-long", 0.6, false) // MATCH_EXACT, not PREDICTIVE_PREFIX
		if hasName(res.Events, events.MessageCommitted) {
			t.Fatal("a revealing box committed on a non-prefix reason")
		}
		if got := h.r.Stats().CommitBlocked; got == 0 {
			t.Fatal("the blocked commit was not counted")
		}
	})

	t.Run("on_requires_the_margin", func(t *testing.T) {
		h := newHarness(t, predictiveConfig(), predictiveTrackerOption())
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		res := h.predictiveRank(h.lastObs, "bottom", h.text("bottom"), "demo-long", 0.05)
		if hasName(res.Events, events.MessageCommitted) {
			t.Fatal("a prefix with no margin over the runner-up committed")
		}
	})

	t.Run("on_and_strong_commits_once", func(t *testing.T) {
		h := newHarness(t, predictiveConfig(), predictiveTrackerOption())
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		h.frame("THE SHIP SAILS SOUTH ACROSS THE BAY", 0.95)
		res := h.predictiveRank(h.lastObs, "bottom", h.text("bottom"), "demo-long", 0.6)
		if !hasName(res.Events, events.MessageCommitted) {
			t.Fatalf("a strong unique prefix did not commit: %s", joinNames(res.Events))
		}
		ev := res.Events[len(res.Events)-1]
		if !fieldBool(t, ev, "predictive") {
			t.Fatal("the commit does not say it was predictive; a misfire cannot be tracked without that")
		}
		if got := fieldStr(t, ev, "reason"); got != ReasonPredictivePrefix {
			t.Fatalf("reason = %q, want %q", got, ReasonPredictivePrefix)
		}
		if got := h.regionState("bottom"); got != recognition.StateCommitted {
			t.Fatalf("text state after the predictive commit = %s, want committed", got)
		}
		if got := h.r.Stats().Commits; got != 1 {
			t.Fatalf("commits = %d, want 1", got)
		}
	})
}

// sawState reports whether a state appeared in a sequence of observations.
func sawState(states []recognition.State, want recognition.State) bool {
	for _, s := range states {
		if s == want {
			return true
		}
	}
	return false
}

// TestReturningToTheSamePageInOneLifetimeDoesNotRespeak is the dedupe rule at its
// least obvious edge: a loop or a menu revisit puts the *same* text back in the
// *same* box, with a fresh ranking behind it. It has already been spoken once in
// this box lifetime, and saying it twice is docs/spec/12's "duplicate speech"
// failure — while the very same content in a box that vanished and returned is a
// new occurrence (the test above).
func TestReturningToTheSamePageInOneLifetimeDoesNotRespeak(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("PRESS A TO BEGIN", 0.9)
	h.rank("msg-press-a", 0.5, false)
	h.gen = h.r.Generation()
	h.speech(h.occurrence("bottom"), SpeechReady, "bottom")

	// The player advances to a different page, which commits normally, and then a
	// branch brings the original line back into the same box.
	h.settle("THE GUARD NODS", 0.9)
	mid := h.rank("msg-guard", 0.5, false)
	if !hasName(mid.Events, events.MessageCommitted) {
		t.Fatalf("the intermediate page did not commit: %s", joinNames(mid.Events))
	}
	h.gen = h.r.Generation()
	h.speech(h.occurrence("bottom"), SpeechReady, "bottom")

	h.settle("PRESS A TO BEGIN", 0.9)
	h.pub.reset()
	res := h.rank("msg-press-a", 0.5, false)
	if hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("the revisited line was committed again: %s", joinNames(res.Events))
	}
	if got := h.r.Stats().DuplicateContent; got != 1 {
		t.Fatalf("duplicate_content_suppressed = %d, want 1", got)
	}
	// The generation still moved with the intermediate page, and the text machine
	// recorded the revisit as a commit (so it will not retry every frame) without
	// the session ever announcing it.
	if got := h.r.Generation(); got != 2 {
		t.Fatalf("generation = %d, want 2: only real commitments move the fence", got)
	}
	if got := h.regionState("bottom"); got != recognition.StateCommitted {
		t.Fatalf("text state = %s, want committed", got)
	}
}

// TestEventLogSeesTheSameEnvelopesAsThePublisher covers the injection point a
// developer inspector or a fixture recorder uses: the reducer hands it exactly the
// numbered envelopes it published, in order, with nothing extra of its own.
func TestEventLogSeesTheSameEnvelopesAsThePublisher(t *testing.T) {
	rec := &recorder{}
	h := newHarness(t, DefaultConfig(), WithLog(rec))
	h.settle("A LINE WORTH RECORDING", 0.9)
	h.rank("demo-recorded", 0.5, false)
	if len(rec.evs) != len(h.pub.published) || len(rec.evs) == 0 {
		t.Fatalf("log received %d events, publisher accepted %d", len(rec.evs), len(h.pub.published))
	}
	for i := range rec.evs {
		if rec.evs[i].EventID != h.pub.published[i].EventID || rec.evs[i].Seq != h.pub.published[i].Seq {
			t.Fatalf("event %d differs between the log (%s/%d) and the publisher (%s/%d)",
				i, rec.evs[i].Name, rec.evs[i].Seq, h.pub.published[i].Name, h.pub.published[i].Seq)
		}
	}
}

type recorder struct{ evs []events.Envelope }

func (r *recorder) Append(e events.Envelope) { r.evs = append(r.evs, e) }

// TestCommitWhilePausedIsAnExplicitPolicyChoice pins the other side of the pause
// rule. The default is the conservative reading of docs/spec/05 ("belief decays or
// resets on … pause"); a caller that wants a paused session to keep recognizing
// text has to say so, and the switch has to actually work when it does.
func TestCommitWhilePausedIsAnExplicitPolicyChoice(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AllowCommitWhilePaused = true
	h := newHarness(t, cfg)
	h.settle("A LINE WHILE PAUSED", 0.9)
	res := h.rank("demo-first", 0.5, false)
	if !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("first commit missing: %s", joinNames(res.Events))
	}
	h.control(ControlPause, "")
	h.settle("A SECOND LINE DURING THE PAUSE", 0.9)
	h.pub.reset()
	h.rank("demo-second", 0.5, false)
	if n := len(h.pub.byName(events.MessageCommitted)); n != 1 {
		t.Fatalf("commits while paused = %d, want 1 under AllowCommitWhilePaused", n)
	}
}

// TestWakeWakesTheRunLoop: Send already notifies, but the batched producer that
// calls Wake itself must not have to poll.
func TestWakeWakesTheRunLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.r.Run(ctx) }()
		h.seq++
		h.ts += int64(200 * time.Millisecond)
		h.r.Send(ObservationMsg{
			Header:   Header{SessionID: h.r.st.SessionID, ProfileRevision: h.profile},
			RegionID: "bottom",
			Observation: recognition.Observation{
				FrameSeq: 1, CaptureTimestampNS: h.ts, SourceClock: "monotonic",
				RegionID: "bottom", RegionPresent: true, RegionHash: "w", OCR: ocrResult("WOKEN TEXT", 0.9),
			},
		})
		h.r.Wake()
		deadline := time.Now().Add(5 * time.Second)
		for h.r.Stats().Applied == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if h.r.Stats().Applied == 0 {
			t.Fatal("the run loop never applied the queued observation")
		}
	})
}
func predictiveConfig() Config {
	c := DefaultConfig()
	c.PredictiveMode = true
	c.MinPredictiveMargin = 0.2
	return c
}

func predictiveTrackerOption() Option {
	cfg := recognition.DefaultConfig()
	cfg.PredictiveMode = true
	cfg.MinPredictLen = 10
	return WithTrackerConfig(cfg)
}

// TestUnchangedRegionHashCountsOCRSkipped is docs/spec/05's duplicate-call
// suppression seen from the session's side: the counter a benchmark reads
// (docs/spec/13) is on the reducer, not only inside a tracker nobody queries.
func TestUnchangedRegionHashCountsOCRSkipped(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.frame("A STATIC LINE", 0.9)
	h.frame("A STATIC LINE", 0.9)
	// The first frame at a new hash establishes it; every later one that arrives
	// with no OCR at that same hash is a call the capture worker proved it did not
	// need to make.
	h.reuse("hash-static")
	for i := 0; i < 6; i++ {
		h.reuse("hash-static")
	}
	if got := h.r.Stats().OCRSkippedSameHash; got != 6 {
		t.Fatalf("ocr_skipped_same_hash = %d, want 6", got)
	}
	if got := h.text("bottom"); got != "a static line" {
		t.Fatalf("accepted text after reuse = %q, want the previous read to survive", got)
	}
}

// TestInboxOverflowDropsObservationsOnly is requirement 5's contract: Send never
// blocks, and when the queue is full it is the oldest observation that leaves —
// never a control, because a control is the player's intention, and never a
// ranking, because dropping it silently loses a commitment the next frame may not
// be able to re-earn.
func TestInboxOverflowDropsObservationsOnly(t *testing.T) {
	h := newHarness(t, Config{InboxSize: 4})
	var sent []Msg
	for i := 0; i < 6; i++ {
		h.ts += int64(200 * 1000 * 1000)
		m := ObservationMsg{
			Header:   Header{SessionID: h.r.st.SessionID, Seq: int64(i + 1), Generation: 0, ProfileRevision: h.profile},
			RegionID: "bottom",
			Observation: recognition.Observation{
				FrameSeq: int64(i + 1), CaptureTimestampNS: h.ts, SourceClock: "monotonic",
				RegionID: "bottom", RegionPresent: true, RegionHash: fmt.Sprintf("h%d", i), OCR: ocrResult("SOME TEXT", 0.9),
			},
		}
		sent = append(sent, m)
		h.r.Send(m)
	}
	if got := h.r.InboxDepth(); got != 4 {
		t.Fatalf("inbox depth = %d, want the configured cap 4", got)
	}
	if got := h.r.Stats().DroppedInboxObservations; got != 2 {
		t.Fatalf("dropped observations = %d, want 2", got)
	}

	// A ranking and a control now arrive at a full queue: they must get in, and the
	// two oldest observations must be what paid for it.
	h.seq = 10
	rk := RankingMsg{
		Header:         Header{SessionID: h.r.st.SessionID, Seq: 11, Generation: 0, ProfileRevision: h.profile},
		RegionID:       "bottom",
		ObservationSeq: 6,
		Text:           "some text",
		Ranking:        matcherRank("msg-x"),
	}
	if !h.r.Send(rk) {
		t.Fatal("the ranking was dropped from a full inbox")
	}
	ctl := ControlMsg{
		Header: Header{SessionID: h.r.st.SessionID, Seq: 12, Generation: 0, ProfileRevision: h.profile},
		Kind:   ControlStop,
	}
	if !h.r.Send(ctl) {
		t.Fatal("the control was dropped from a full inbox")
	}
	st := h.r.Stats()
	if st.DroppedInboxObservations != 4 || st.DroppedInboxRankings != 0 || st.DroppedInboxControl != 0 {
		t.Fatalf("drops = observations %d, rankings %d, control %d; want 4, 0, 0",
			st.DroppedInboxObservations, st.DroppedInboxRankings, st.DroppedInboxControl)
	}
	if got := h.r.InboxDepth(); got != 4 {
		t.Fatalf("inbox depth after evictions = %d, want 4", got)
	}

	// Drain through the same Apply path Run uses, and prove the survivors are the
	// newest observations plus the ranking and the control.
	var kinds []Kind
	seen := map[Kind]int{}
	for {
		m, ok := h.r.inbox.pop()
		if !ok {
			break
		}
		res, err := h.r.Apply(h.ctx, m)
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		kinds = append(kinds, res.Kind)
		seen[res.Kind]++
		if res.Kind == KindControl && !res.Applied {
			t.Fatalf("the surviving control was not applied: %s", res.Reason)
		}
		if res.Kind == KindControl && seen[res.Kind] == 1 && !h.r.Stopped() {
			t.Fatal("the Stop did not take effect")
		}
	}
	if seen[KindObservation] != 2 {
		t.Fatalf("%d observations survived, want 2: %v", seen[KindObservation], kinds)
	}
	if seen[KindRanking] != 1 {
		t.Fatalf("the ranking did not survive the overflow: %v", kinds)
	}
}

// TestInboxOverflowNeverLosesAControlToALowerPriorityPeer documents the one case
// the eviction rule cannot serve: a queue that is nothing but controls. Losing a
// Stop would be worse than losing a duplicate of one, and the counter says which
// happened rather than deciding silently.
func TestInboxOverflowNeverLosesAControlToALowerPriorityPeer(t *testing.T) {
	h := newHarness(t, Config{InboxSize: 3})
	for i := 0; i < 3; i++ {
		if !h.r.Send(ControlMsg{Header: Header{SessionID: h.r.st.SessionID, Generation: 0, ProfileRevision: h.profile}, Kind: ControlPause}) {
			t.Fatalf("control %d was refused", i)
		}
	}
	if h.r.Send(ControlMsg{Header: Header{SessionID: h.r.st.SessionID, Generation: 0, ProfileRevision: h.profile}, Kind: ControlStop}) {
		t.Fatal("a control was accepted into a queue full of controls; the queue must be bounded")
	}
	if got := h.r.Stats().DroppedInboxControl; got != 1 {
		t.Fatalf("dropped control counter = %d, want 1", got)
	}
	if got := h.r.InboxDepth(); got != 3 {
		t.Fatalf("depth = %d, want 3", got)
	}
}

// TestFencesRejectOtherSessionProfileRevisionAndDuplicateSeq exercises the first
// four lines of docs/spec/17's pseudo-code one at a time, so a regression in any of
// them fails a named test rather than showing up as a wrong commit later.
func TestFencesRejectOtherSessionProfileRevisionAndDuplicateSeq(t *testing.T) {
	h := newHarness(t, DefaultConfig())

	other := ObservationMsg{
		Header:   Header{SessionID: "someone-elses-session", Seq: 1, Generation: 0, ProfileRevision: h.profile},
		RegionID: "bottom",
		Observation: recognition.Observation{
			FrameSeq: 1, CaptureTimestampNS: h.ts + 1, SourceClock: "monotonic",
			RegionID: "bottom", RegionPresent: true, RegionHash: "x", OCR: ocrResult("NOT MINE", 0.9),
		},
	}
	if res, _ := h.r.Apply(h.ctx, other); res.Applied || res.Reason != "other_session" {
		t.Fatalf("a message for another session was applied: %+v", res)
	}

	bad := ObservationMsg{
		Header:   Header{SessionID: h.r.st.SessionID, Seq: 2, Generation: 0, ProfileRevision: "draft-rev"},
		RegionID: "bottom",
		Observation: recognition.Observation{
			FrameSeq: 1, CaptureTimestampNS: h.ts + 2, SourceClock: "monotonic",
			RegionID: "bottom", RegionPresent: true, RegionHash: "x", OCR: ocrResult("STALE REVISION", 0.9),
		},
	}
	if res, _ := h.r.Apply(h.ctx, bad); res.Applied || res.Reason != "profile_revision_mismatch" {
		t.Fatalf("a message from another profile revision was applied: %+v", res)
	}

	// A duplicate seq is re-delivery of something already applied.
	good := ObservationMsg{
		Header:   Header{SessionID: h.r.st.SessionID, Seq: 3, Generation: 0, ProfileRevision: h.profile},
		RegionID: "bottom",
		Observation: recognition.Observation{
			FrameSeq: 1, CaptureTimestampNS: h.ts + 3, SourceClock: "monotonic",
			RegionID: "bottom", RegionPresent: true, RegionHash: "x", OCR: ocrResult("APPLIED ONCE", 0.9),
		},
	}
	if res, _ := h.r.Apply(h.ctx, good); !res.Applied {
		t.Fatalf("the first delivery was refused: %+v", res)
	}
	if res, _ := h.r.Apply(h.ctx, good); res.Applied || res.Reason != "duplicate_seq" {
		t.Fatalf("the second delivery was applied: %+v", res)
	}
	st := h.r.Stats()
	if st.OtherSession != 1 || st.ProfileMismatch != 1 || st.Duplicated != 1 {
		t.Fatalf("fence counters = other session %d, revision %d, duplicate %d; want 1,1,1",
			st.OtherSession, st.ProfileMismatch, st.Duplicated)
	}
	if st.Applied != 1 {
		t.Fatalf("applied = %d, want 1", st.Applied)
	}

	// A malformed message never reaches the fences: it is refused as invalid so the
	// counters above keep meaning "a real message was declined for a real reason".
	if res, _ := h.r.Apply(h.ctx, ObservationMsg{Header: Header{SessionID: h.r.st.SessionID}}); res.Applied || res.Reason[:8] != "invalid:" {
		t.Fatalf("a malformed observation: %+v", res)
	}
}

// TestTraceAndCausationChainIsWalkable is the event-envelope requirement stated as
// a test: from a frame observation to the candidates it produced to the commitment
// they produced, one trace_id and a causation_id that names the event before it.
// docs/spec/11 asserts the same chain through a browser, and it cannot be asserted
// at all unless the ids are wired here.
func TestTraceAndCausationChainIsWalkable(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("THE LANTERN FLICKERS", 0.9)
	res := h.rank("demo-lantern", 0.5, false)
	if !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("no commitment to walk the chain of: %s", joinNames(res.Events))
	}
	evs := h.pub.published

	var text, cand, comm events.Envelope
	for _, e := range evs {
		switch e.Name {
		case events.TextObserved:
			text = e
		case events.MessageCandidates:
			cand = e
		case events.MessageCommitted:
			comm = e
		}
	}
	if text.EventID == "" || cand.EventID == "" || comm.EventID == "" {
		t.Fatalf("missing event in the chain: %v", namesOf(evs))
	}
	if comm.TraceID == "" {
		t.Fatal("message.committed carries no trace_id")
	}
	if text.TraceID != comm.TraceID {
		t.Fatalf("the frame's trace %q is not the commitment's %q", text.TraceID, comm.TraceID)
	}
	if cand.TraceID != comm.TraceID {
		t.Fatalf("the candidate list's trace %q is not the commitment's %q", cand.TraceID, comm.TraceID)
	}
	if comm.CausationID != cand.EventID {
		t.Fatalf("message.committed causation %q, want the candidate list %q", comm.CausationID, cand.EventID)
	}
	if cand.CausationID != text.EventID {
		t.Fatalf("message.candidates causation %q, want the observation %q", cand.CausationID, text.EventID)
	}
	if comm.SessionID != h.r.st.SessionID {
		t.Fatalf("event session %q, want %q", comm.SessionID, h.r.st.SessionID)
	}
	if !comm.OccurredAt.After(text.OccurredAt) {
		t.Fatalf("occurred_at did not advance across the chain: %v then %v", text.OccurredAt, comm.OccurredAt)
	}
}

// TestSpeechLifecycleAndStaleStartRejected covers docs/spec/12's "no stale audio
// starts after a new committed dialogue event" from the audio side: a clip that a
// TTS worker started after the page had turned arrives with the old generation, and
// even one that arrives with the right generation but names an occurrence the
// session already cancelled is refused by the phase table, not filtered in a UI.
func TestSpeechLifecycleAndStaleStartRejected(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("SPEAK THIS LINE", 0.9)
	res := h.rank("demo-speak", 0.5, false)
	occ := fieldStr(t, res.Events[len(res.Events)-1], "dialogue_occurrence_id")
	h.gen = h.r.Generation()

	for _, k := range []SpeechKind{SpeechReady, SpeechStarted, SpeechEnded} {
		r := h.speech(occ, k, "bottom")
		if !r.Applied {
			t.Fatalf("%s refused: %s", k, r.Reason)
		}
	}
	if got := h.regionSpeechPhase("bottom"); got != PhaseEnded {
		t.Fatalf("phase = %q, want ended", got)
	}

	// A second "started" for an ended utterance is a worker that lost track.
	if r := h.speech(occ, SpeechStarted, "bottom"); r.Applied || r.Reason != "invalid_speech_phase" {
		t.Fatalf("a started after an ended was applied: %+v", r)
	}
	// An occurrence the session never committed cannot put an event on the wire.
	if r := h.speech("occ-999999", SpeechStarted, "bottom"); r.Applied || r.Reason != "unknown_occurrence" {
		t.Fatalf("an unknown occurrence was accepted: %+v", r)
	}
	if got := h.r.Stats().RejectedState; got != 1 {
		t.Fatalf("rejected_transition = %d, want 1", got)
	}

	// Now the real case: the page turns while the clip is still pending, and the
	// clip starts anyway. The cancellation is already on the log; the late start is
	// rejected twice over — by generation and by phase.
	h.settle("NEXT PAGE ALTOGETHER", 0.9)
	if n := len(h.pub.byName(events.SpeechCancelled)); n != 0 {
		t.Fatalf("cancelling an already-ended utterance produced %d events, want 0", n)
	}
	h.rank("demo-next", 0.5, false)
	if r := h.speechAt(1, occ, "bottom", SpeechStarted); r.Applied || r.Reason != "stale_generation" {
		t.Fatalf("a start from the superseded generation was applied: %+v", r)
	}
}

// TestPauseCancelsAndBlocksCommits, TestStopEndsTheSession and
// TestProfileChangeResetsBelief cover ControlMsg: the only inputs that change what
// the session is for rather than what it believes.
func TestPauseCancelsAndBlocksCommits(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("LINES WHILE PAUSED", 0.9)
	res := h.rank("demo-pause", 0.5, false)
	occ := fieldStr(t, res.Events[len(res.Events)-1], "dialogue_occurrence_id")
	h.gen = h.r.Generation()
	h.speech(occ, SpeechReady, "bottom")

	h.pub.reset()
	h.control(ControlPause, "")
	cancels := h.pub.byName(events.SpeechCancelled)
	if len(cancels) != 1 || fieldStr(t, cancels[0], "reason") != CancelPaused {
		t.Fatalf("pause produced %v, want one speech.cancelled{paused}", namesOf(h.pub.published))
	}
	if !h.r.State().Paused {
		t.Fatal("state does not record the pause")
	}

	// A new line while paused must not commit: docs/spec/05 resets belief on a
	// pause, and speaking during one is not what pausing means to a player.
	h.pub.reset()
	h.settle("A NEW LINE DURING THE PAUSE", 0.9)
	h.rank("demo-paused-2", 0.5, false)
	if n := len(h.pub.byName(events.MessageCommitted)); n != 0 {
		t.Fatalf("%d commits while paused, want 0", n)
	}
	if got := h.r.Stats().CommitBlocked; got == 0 {
		t.Fatal("the blocked commits were not counted")
	}
	before := h.r.Generation()

	h.control(ControlResume, "")
	if h.r.State().Paused {
		t.Fatal("resume did not clear the pause")
	}
	// The line is still on screen and the ranking that was refused while paused is
	// still held: docs/spec/12's "new profile publication while session active" and
	// the pause case both depend on evidence surviving a block rather than being
	// spent by it, so the very next observation commits it.
	beforeCommits := len(h.pub.byName(events.MessageCommitted))
	h.settle("A NEW LINE DURING THE PAUSE", 0.9)
	h.rank("demo-paused-2", 0.5, false)
	if got := len(h.pub.byName(events.MessageCommitted)); got <= beforeCommits {
		t.Fatalf("after resume the waiting line never committed (%d commitments total)", got)
	}
	if h.r.Generation() <= before {
		t.Fatalf("generation stayed at %d across a post-resume commit", h.r.Generation())
	}
}

func TestStopEndsTheSession(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("A LINE BEFORE THE STOP", 0.9)
	res := h.rank("demo-stop", 0.5, false)
	occ := fieldStr(t, res.Events[len(res.Events)-1], "dialogue_occurrence_id")
	h.gen = h.r.Generation()
	h.speech(occ, SpeechReady, "bottom")
	h.speech(occ, SpeechStarted, "bottom")

	h.pub.reset()
	h.control(ControlStop, "")
	if !h.r.Stopped() {
		t.Fatal("the session did not record the stop")
	}
	names := h.pub.names()
	if !hasName(h.pub.published, events.SpeechCancelled) || !hasName(h.pub.published, events.SessionStopped) {
		t.Fatalf("stop emitted %v, want a cancellation and session.stopped", names)
	}
	// Anything else now is refused with a reason, not absorbed: a caller that keeps
	// feeding a dead session has a lifecycle bug (AGENTS.md: graceful degraded
	// modes, not silent ones).
	if r := h.frame("TEXT AFTER THE STOP", 0.9); r.Applied || r.Reason != "stopped" {
		t.Fatalf("an observation after stop was applied: %+v", r)
	}
	if got := h.r.Stats().DroppedAfterStop; got != 1 {
		t.Fatalf("dropped_after_stop = %d, want 1", got)
	}
}

func TestProfileChangeResetsBelief(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("SAME WORDS BOTH REVISIONS", 0.9)
	first := h.rank("msg-rev1", 0.5, false)
	occ := fieldStr(t, first.Events[len(first.Events)-1], "dialogue_occurrence_id")
	h.gen = h.r.Generation()
	h.speech(occ, SpeechReady, "bottom")

	h.pub.reset()
	h.control(ControlProfileChanged, "profile-rev-2")
	if got := h.r.State().ProfileRevision; got != "profile-rev-2" {
		t.Fatalf("revision = %q, want profile-rev-2", got)
	}
	if !hasName(h.pub.published, events.SpeechCancelled) || fieldStr(t, h.pub.published[0], "reason") != CancelProfileChanged {
		t.Fatalf("profile change emitted %v, want a cancellation first", h.pub.names())
	}
	if got := h.regionState("bottom"); got != recognition.StateAbsent {
		t.Fatalf("text state after a publish = %s, want absent: the vote window held text whose meaning was defined by the old revision", got)
	}
	// The same words must be committable again, because in the new revision they may
	// be a different message. dedupe is per content-and-lifetime, and the
	// publication ended both.
	h.pub.reset()
	h.settle("SAME WORDS BOTH REVISIONS", 0.9)
	res := h.rank("msg-rev2", 0.5, false)
	if !hasName(res.Events, events.MessageCommitted) {
		t.Fatalf("the same line could not be committed under the new revision: %s", joinNames(res.Events))
	}
	if got := fieldStr(t, res.Events[len(res.Events)-1], "dialogue_occurrence_id"); got == occ {
		t.Fatalf("the new revision reused occurrence %s", occ)
	}
}

// TestSceneChangeIsAnnouncedOnlyOnAChange covers session.scene_changed: the
// matcher's scene is belief, the session's scene is a fact the UI and the graph
// both need, and the first one seen is a beginning rather than a change.
func TestSceneChangeIsAnnouncedOnlyOnAChange(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("THE VILLAGE GATE", 0.9)
	h.rankAt(h.lastObs, "bottom", h.text("bottom"), "msg-gate", 0.5, false, "village")
	if n := len(h.pub.byName(events.SessionSceneChanged)); n != 0 {
		t.Fatalf("the first scene produced %d scene_changed events, want 0 (nothing changed from)", n)
	}
	if got := h.r.State().SceneID; got != "village" {
		t.Fatalf("scene = %q, want village", got)
	}

	h.settle("THE SMITH FORGES", 0.9)
	h.rankAt(h.lastObs, "bottom", h.text("bottom"), "msg-smith", 0.5, false, "forge")
	evs := h.pub.byName(events.SessionSceneChanged)
	if len(evs) != 1 {
		t.Fatalf("scene_changed events = %d, want 1", len(evs))
	}
	if got := fieldStr(t, evs[0], "from_scene"); got != "village" {
		t.Fatalf("from_scene = %q, want village", got)
	}
	if got := fieldStr(t, evs[0], "to_scene"); got != "forge" {
		t.Fatalf("to_scene = %q, want forge", got)
	}
	if got := fieldFloat(t, evs[0], "generation"); got != 2 {
		t.Fatalf("generation = %v, want the commit's own generation", got)
	}
	// Same scene again: no event.
	h.settle("THE SMITH SPEAKS AGAIN", 0.9)
	h.rankAt(h.lastObs, "bottom", h.text("bottom"), "msg-smith-2", 0.5, false, "forge")
	if got := len(h.pub.byName(events.SessionSceneChanged)); got != 1 {
		t.Fatalf("scene_changed events = %d, want 1 total", got)
	}
}

// TestDecisionCanRefuseButNeverInvent is the advisory half of docs/spec/05's commit
// policy: a model may say "not this" (safe_to_speak), may name a speaker, and may
// choose among the candidates retrieval produced — but a hallucinated message id
// that retrieval never offered cannot become a commitment, because that is the
// difference between a second opinion and a guess with better formatting.
func TestDecisionCanRefuseButNeverInvent(t *testing.T) {
	t.Run("veto", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("A LINE THE MODEL WOULD NOT SAY", 0.9)
		obs := h.lastObs
		h.decide(obs, 0, "", "narrator", false, true)
		res := h.rank("demo-vetoed", 0.5, false)
		ev := res.Events[len(res.Events)-1]
		if fieldBool(t, ev, "speech_eligible") {
			t.Fatal("safe_to_speak = no did not veto the utterance")
		}
		if got := fieldStr(t, ev, "message_id"); got != "demo-vetoed" {
			t.Fatalf("message_id = %q: a veto should silence the utterance, not erase the match", got)
		}
	})

	t.Run("invention", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("A LINE WITH A KNOWN MATCH", 0.9)
		obs := h.lastObs
		h.decide(obs, 0, "msg-hallucinated", "hero", true, true)
		res := h.rank("demo-known", 0.5, false)
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != "demo-known" {
			t.Fatalf("message_id = %q, want the retrieval answer: the model's choice was not a candidate", got)
		}
		if got := fieldStr(t, ev, "speaker_id"); got != "hero" {
			t.Fatalf("speaker_id = %q, want the decision's speaker", got)
		}
	})

	t.Run("narrowing", func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		h.settle("TWO ADJACENT GRAPH CANDIDATES", 0.9)
		obs := h.lastObs
		h.gen = 0
		h.seq++
		if res, err := h.r.Apply(h.ctx, DecisionMsg{
			Header:   Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: 0, ProfileRevision: h.profile},
			RegionID: "bottom", ObservationSeq: obs, MessageID: "demo-second", SpeakerID: "narrator",
			SpeakerKnown: true, SpeechEligible: true, Accepted: true, Provider: "systemone",
		}); err != nil || res.Reason != "applied" {
			t.Fatalf("decision delivery: %+v %v", res, err)
		}
		h.seq++
		rk := RankingMsg{
			Header:         Header{SessionID: h.r.st.SessionID, Seq: h.seq, Generation: 0, ProfileRevision: h.profile},
			RegionID:       "bottom",
			ObservationSeq: obs,
			Text:           h.text("bottom"),
			Ranking: matcher.Ranking{
				Candidates: []matcher.Candidate{{MessageID: "demo-first", Probability: 0.5}, {MessageID: "demo-second", Probability: 0.4}},
				Margin:     0.1,
				Reason:     matcher.ReasonFuzzy,
			},
		}
		res, err := h.r.Apply(h.ctx, rk)
		if err != nil {
			t.Fatalf("ranking: %v", err)
		}
		ev := res.Events[len(res.Events)-1]
		if got := fieldStr(t, ev, "message_id"); got != "demo-second" {
			t.Fatalf("message_id = %q, want the candidate the model chose", got)
		}
		if got := fieldStr(t, ev, "reason"); got != ReasonClefDisambiguated {
			t.Fatalf("reason = %q, want %q so the calibration data can tell which evidence decided", got, ReasonClefDisambiguated)
		}
	})
}

// TestCommitPayloadCarriesSpeakerKind is the ADR-0158 plumbing half: the
// kind answer rides the same path as the speaker confidence — DecisionMsg →
// the commit payload's speaker_kind / speaker_kind_confidence — so the
// speech side can gate the kind and, when the speaker gate falls back, voice
// the line through the generic kind character.
func TestCommitPayloadCarriesSpeakerKind(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("A LINE THE MODEL WOULD NOT SAY", 0.9)
	obs := h.lastObs
	h.decideKind(obs, 0, "", "narrator", systemone.SpeakerKindAdultMale, 0.9, true, true)
	res := h.rank("demo-kind", 0.5, false)
	ev := res.Events[len(res.Events)-1]
	if got := fieldStr(t, ev, "speaker_kind"); got != systemone.SpeakerKindAdultMale {
		t.Fatalf("speaker_kind = %q, want the decision's kind %q", got, systemone.SpeakerKindAdultMale)
	}
	if got := fieldFloat(t, ev, "speaker_kind_confidence"); got != 0.9 {
		t.Fatalf("speaker_kind_confidence = %v, want the decision's kind confidence 0.9", got)
	}
}

// TestPublishFailureIsReportedAndCounted: a session that cannot write its events is
// degraded, and must say so rather than continue as though the log were healthy
// (docs/spec/13 "explain degraded status").
func TestPublishFailureIsReportedAndCounted(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.pub.failFor[events.TextObserved] = 1
	res, err := h.r.Apply(h.ctx, ObservationMsg{
		Header:   Header{SessionID: h.r.st.SessionID, Seq: h.nextSeq(), Generation: 0, ProfileRevision: h.profile},
		RegionID: "bottom",
		Observation: recognition.Observation{
			FrameSeq: 1, CaptureTimestampNS: h.ts + 1, SourceClock: "monotonic",
			RegionID: "bottom", RegionPresent: true, RegionHash: "x", OCR: ocrResult("A FRAME", 0.9),
		},
	})
	if err == nil {
		t.Fatal("a publish failure was swallowed")
	}
	if res.Applied {
		t.Fatal("the message reported itself applied while its event failed to publish")
	}
	if got := h.r.Stats().PublishErrors; got != 1 {
		t.Fatalf("publish_errors = %d, want 1", got)
	}
	if h.r.Stats().PublishErrorsLast == "" {
		t.Fatal("the failure was counted but not described")
	}
}

// TestFlickerInsideACommittedBoxDoesNotReachTheWire is the session-side half of the
// one-noisy-frame test: the vote keeps the text, so the reducer emits neither a new
// commitment nor a cancellation.
func TestFlickerInsideACommittedBoxDoesNotReachTheWire(t *testing.T) {
	h := newHarness(t, DefaultConfig())
	h.settle("THE SHIP SAILS SOUTH", 0.9)
	res := h.rank("demo-flicker", 0.5, false)
	occ := fieldStr(t, res.Events[len(res.Events)-1], "dialogue_occurrence_id")
	h.gen = h.r.Generation()
	h.speech(occ, SpeechStarted, "bottom")

	h.pub.reset()
	h.frame("A WHOLE DIFFERENT LINE", 0.9)
	h.hold("THE SHIP SAILS SOUTH", 0.9, 3)
	if n := len(h.pub.byName(events.MessageCommitted)); n != 0 {
		t.Fatalf("%d commits from a flicker, want 0", n)
	}
	if n := len(h.pub.byName(events.SpeechCancelled)); n != 0 {
		t.Fatalf("%d cancellations from a flicker, want 0 — that is the player's speech being cancelled for one bad frame", n)
	}
}

// TestRunDrainsTheInboxOnOneGoroutine exercises the concurrency contract end to
// end: producers Send, Run applies, and a Stop ends the loop cleanly (docs/spec/01
// "One session event reducer").
func TestRunDrainsTheInboxOnOneGoroutine(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, DefaultConfig())
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- h.r.Run(ctx) }()

		for i := 0; i < 5; i++ {
			h.ts += int64(200 * 1000 * 1000)
			h.r.Send(ObservationMsg{
				Header:   Header{SessionID: h.r.st.SessionID, Generation: 0, ProfileRevision: h.profile},
				RegionID: "bottom",
				Observation: recognition.Observation{
					FrameSeq: int64(i + 1), CaptureTimestampNS: h.ts, SourceClock: "monotonic",
					RegionID: "bottom", RegionPresent: true, RegionHash: fmt.Sprintf("run-%d", i),
					OCR: ocrResult("RUN LOOP TEXT", 0.9),
				},
			})
		}
		h.r.Send(ControlMsg{Header: Header{SessionID: h.r.st.SessionID, ProfileRevision: h.profile}, Kind: ControlStop})

		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Run returned %v, want a clean stop", err)
			}
		case <-timeoutAfter():
			t.Fatal("Run did not return after the Stop")
		}
		if got := h.r.InboxDepth(); got != 0 {
			t.Fatalf("inbox depth after stop = %d, want 0", got)
		}
		if got := h.r.Stats().Applied; got < 2 {
			t.Fatalf("applied = %d, want the observation and the control", got)
		}
	})
}
