import { describe, expect, it } from "vitest";

import type { EventEnvelope } from "../api/sessions";
import { initialPlayEventStream, MAX_RECENT_EVENTS, reducePlayEventStream } from "./playEvents";

/** A synthetic envelope for the fold's tests (no network involved). */
function envelope(name: string, seq: number, data: Record<string, unknown> = {}): EventEnvelope {
  return {
    schema_version: 1,
    event_id: `evt-${seq}`,
    seq,
    name,
    occurred_at: `2026-10-20T00:00:${String(seq % 60).padStart(2, "0")}Z`,
    data: data as EventEnvelope["data"],
  };
}

describe("reducePlayEventStream", () => {
  it("keeps recent events newest-first and bounded", () => {
    let state = initialPlayEventStream();
    for (let i = 1; i <= MAX_RECENT_EVENTS + 10; i++) {
      state = reducePlayEventStream(state, envelope("text.observed", i, { text: `t${i}` }));
    }
    expect(state.events).toHaveLength(MAX_RECENT_EVENTS);
    // Newest first: the last folded event is on top.
    expect(state.events[0].seq).toBe(MAX_RECENT_EVENTS + 10);
    expect(state.events.at(-1)?.seq).toBe(11);
    expect(state.maxSeq).toBe(MAX_RECENT_EVENTS + 10);
  });

  it("does not duplicate a replayed event", () => {
    let state = initialPlayEventStream();
    state = reducePlayEventStream(state, envelope("message.committed", 5, { message_id: "m" }));
    const before = state.events.length;
    // The reconnect replay re-folds the same event id.
    state = reducePlayEventStream(state, envelope("message.committed", 5, { message_id: "m" }));
    expect(state.events.length).toBe(before);
    expect(state.events.filter((row) => row.eventId === "evt-5")).toHaveLength(1);
  });

  it("folds the newest message.candidates ranking (and keeps it on a gap)", () => {
    let state = initialPlayEventStream();
    state = reducePlayEventStream(
      state,
      envelope("message.candidates", 1, {
        text: "the harbour bell",
        reason: "exact",
        abstain: false,
        candidate_total: 2,
        candidates: [
          { message_id: "a", score: 0.9, probability: 0.88 },
          { message_id: "b", score: 0.4, probability: 0.05 },
        ],
      }),
    );
    expect(state.candidates?.occurredAt).toBe(state.events[0].occurredAt);
    expect(state.candidates?.candidates).toHaveLength(2);
    expect(state.candidates?.candidates[0]?.probability).toBe(0.88);

    // A later non-candidates event leaves the ranking in place.
    state = reducePlayEventStream(state, envelope("speech.started", 2, {}));
    expect(state.candidates?.eventId).toBe("evt-1");
    // A newer ranking replaces it.
    state = reducePlayEventStream(
      state,
      envelope("message.candidates", 3, {
        text: "nobody in the village",
        abstain: true,
        candidate_total: 1,
        candidates: [{ message_id: "c", score: 0.1, probability: 0.1 }],
      }),
    );
    expect(state.candidates?.eventId).toBe("evt-3");
    expect(state.candidates?.abstain).toBe(true);
    expect(state.candidates?.candidateTotal).toBe(1);
  });

  it("skips a candidates frame whose data does not carry the shape (degraded, not fatal)", () => {
    let state = initialPlayEventStream();
    state = reducePlayEventStream(state, envelope("message.candidates", 1, { candidates: "nope" }));
    expect(state.candidates).toBeNull();
    // The event itself is still a table row.
    expect(state.events[0]?.name).toBe("message.candidates");
  });

  it("writes one short detail line per event kind", () => {
    let state = initialPlayEventStream();
    state = reducePlayEventStream(
      state,
      envelope("message.committed", 1, { text: "x".repeat(80) }),
    );
    expect(state.events[0]?.detail.length).toBeLessThanOrEqual(64);
    state = reducePlayEventStream(state, envelope("message.candidates", 2, { candidate_total: 3 }));
    expect(state.events[0]?.detail).toBe("3 candidates");
    state = reducePlayEventStream(state, envelope("dialogue.advanced", 3, {}));
    expect(state.events[0]?.detail).toBe("player advanced the text box");
    state = reducePlayEventStream(state, envelope("speech.cancelled", 4, {}));
    expect(state.events[0]?.detail).toBe("cancelled");
  });
});
