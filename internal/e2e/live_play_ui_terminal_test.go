// Package e2e — the step-9 poll's fail-fast decision as a pure unit test
// (issue #39).
//
// TestLivePlayUI's step-9 poll waits up to 10 minutes for the committed-and-
// spoken chain. On a run where no trace ends with the full chain, the 2026-
// 10-20 liveui run kept polling a session the Play page had already stopped
// for the whole 10 minutes — the terminal state and the last stats were
// knowable at the first tick, but only after the full budget the test said so
// (and the Dolphin + Xvfb idled under the bridge the whole time).
//
// sessionTerminal is the decision extracted from that poll loop: given the
// decoded dev-events page, is the session already terminal, and if so which
// terminal state, with what reason and last stats? This test pins it without
// an emulator, server or provider — just the envelopes the poll reads.

package e2e

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/dbirks/game-narrator/internal/events"
	"github.com/dbirks/game-narrator/internal/pipeline"
	"github.com/dbirks/game-narrator/internal/runtime"
)

// TestSessionTerminal pins the fail-fast decision: a terminal event on the
// page names the state and the last stats and ends the wait; without it the
// 10-minute budget still covers the running session.
func TestSessionTerminal(t *testing.T) {
	const thisSession = "sess-liveui-test"
	lastStats := pipeline.Stats{
		FramesDelivered: 1204, FramesDropped: 3, FramesProcessed: 1201,
		OCRCalls: 95, Rankings: 40, Commits: 3,
	}
	marshal := func(v any) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal fixture: %v", err)
		}
		return b
	}
	running := []devEnvelope{
		{SessionID: thisSession, Name: "session.started"},
		{SessionID: thisSession, Name: "text.observed"},
		{SessionID: thisSession, Name: "message.candidates"},
		{SessionID: thisSession, Name: "decision.resolved"},
		{SessionID: thisSession, Name: "message.committed"},
		{SessionID: thisSession, Name: "speech.requested"},
		{SessionID: thisSession, Name: "speech.started"},
	}

	t.Run("a running session's page is not terminal", func(t *testing.T) {
		rep, terminal := sessionTerminal(running, thisSession)
		if terminal {
			t.Fatalf("terminal = true on a running session's page (report %+v): the 10-minute budget must still cover it", rep)
		}
		if rep.state != "" || rep.reason != "" {
			t.Fatalf("report carries state=%q reason=%q on a non-terminal page; want empty", rep.state, rep.reason)
		}
	})

	t.Run("session.stopped names the state, the reason and the last stats", func(t *testing.T) {
		evs := append(append([]devEnvelope{}, running...),
			devEnvelope{SessionID: thisSession, Name: events.SessionStopped, Data: marshal(map[string]any{
				"reason": "requested", "stats": lastStats, "trace": []any{},
			})},
		)
		rep, terminal := sessionTerminal(evs, thisSession)
		if !terminal {
			t.Fatal("terminal = false after session.stopped landed on the page")
		}
		if rep.state != runtime.StateStopped {
			t.Fatalf("state = %q, want %q", rep.state, runtime.StateStopped)
		}
		if rep.reason != "requested" {
			t.Fatalf("reason = %q, want %q", rep.reason, "requested")
		}
		if !reflect.DeepEqual(rep.stats, lastStats) {
			t.Fatalf("last stats = %+v, want the terminal event's stats %+v", rep.stats, lastStats)
		}
	})

	t.Run("session.failed before session.stopped is the failed state", func(t *testing.T) {
		evs := append(append([]devEnvelope{}, running...),
			devEnvelope{SessionID: thisSession, Name: runtime.EventSessionFailed, Data: marshal(map[string]any{
				"error": "the source's read failed", "stats": lastStats, "trace": []any{},
			})},
			devEnvelope{SessionID: thisSession, Name: events.SessionStopped, Data: marshal(map[string]any{
				"reason": "error", "stats": lastStats, "trace": []any{},
			})},
		)
		rep, terminal := sessionTerminal(evs, thisSession)
		if !terminal {
			t.Fatal("terminal = false after session.failed + session.stopped")
		}
		if rep.state != runtime.StateFailed {
			t.Fatalf("state = %q, want %q (the session.failed event marks the failed state)", rep.state, runtime.StateFailed)
		}
		if rep.reason != "error" {
			t.Fatalf("reason = %q, want the session.stopped event's reason %q", rep.reason, "error")
		}
		if !reflect.DeepEqual(rep.stats, lastStats) {
			t.Fatalf("last stats = %+v, want %+v", rep.stats, lastStats)
		}
	})

	t.Run("session.failed alone is not terminal: session.stopped is the authoritative mark", func(t *testing.T) {
		evs := append(append([]devEnvelope{}, running...),
			devEnvelope{SessionID: thisSession, Name: runtime.EventSessionFailed, Data: marshal(map[string]any{
				"error": "the source's read failed", "stats": lastStats, "trace": []any{},
			})},
		)
		rep, terminal := sessionTerminal(evs, thisSession)
		if terminal {
			t.Fatalf("terminal = true on session.failed alone (report %+v): the manager publishes session.stopped after every failed run — the poll waits for it", rep)
		}
		if rep.state != runtime.StateFailed {
			t.Fatalf("state = %q, want %q (the failed state is noted even before the stop lands)", rep.state, runtime.StateFailed)
		}
	})

	t.Run("a malformed session.stopped still marks the session terminal", func(t *testing.T) {
		evs := append(append([]devEnvelope{}, running...),
			devEnvelope{SessionID: thisSession, Name: events.SessionStopped, Data: json.RawMessage(`{"reason":`)},
		)
		rep, terminal := sessionTerminal(evs, thisSession)
		if !terminal {
			t.Fatal("terminal = false on a malformed session.stopped: the event's presence is the signal (the runtime never publishes a partial stop)")
		}
		if rep.state != runtime.StateStopped || rep.reason != "" {
			t.Fatalf("state = %q reason = %q, want the stopped state with empty detail on an undecodable payload", rep.state, rep.reason)
		}
	})

	t.Run("a foreign session's terminal event does not count", func(t *testing.T) {
		evs := append(append([]devEnvelope{}, running...),
			devEnvelope{SessionID: "foreign", Name: events.SessionStopped, Data: marshal(map[string]any{
				"reason": "requested", "stats": lastStats, "trace": []any{},
			})},
		)
		if rep, terminal := sessionTerminal(evs, thisSession); terminal {
			t.Fatalf("terminal = true on another session's session.stopped (report %+v)", rep)
		}
	})
}
