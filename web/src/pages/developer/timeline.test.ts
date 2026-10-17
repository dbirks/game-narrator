/**
 * `timeline.test.ts` — the timeline reducer's contracts: seq ordering, dedupe
 * by event_id on SSE reconnect, the 500-row cap, and pinning across stream
 * advancement (docs/spec/09 "Pin a particular event/frame despite stream
 * advancement").
 */

import { describe, expect, it } from "vitest";

import type { EventEnvelope } from "../../api/sessions";
import { initialTimeline, renderedRows, timelineReducer, WINDOW } from "./timeline";

function envelope(id: string, seq: number, name = "text.observed"): EventEnvelope {
  return {
    schema_version: 1,
    event_id: id,
    seq,
    name,
    occurred_at: "2026-01-01T00:00:00.000Z",
    data: {},
  } as EventEnvelope;
}

/** `n` events with ids e1..en and ascending seq. */
function burst(n: number, fromSeq = 1, prefix = "e"): EventEnvelope[] {
  return Array.from({ length: n }, (_, i) => envelope(`${prefix}${fromSeq + i}`, fromSeq + i));
}

describe("timeline reducer", () => {
  it("keeps rows in seq order, not arrival order", () => {
    const state = timelineReducer(
      timelineReducer(initialTimeline(), {
        type: "events",
        events: [envelope("b", 2), envelope("c", 3)],
      }),
      { type: "events", events: [envelope("a", 1)] },
    );
    expect(state.rows.map((row) => row.event_id)).toEqual(["a", "b", "c"]);
  });

  it("dedupes by event id when SSE reconnect replays the tail", () => {
    const events = burst(5);
    let state = timelineReducer(initialTimeline(), { type: "events", events });
    // The reconnect replays seq 4 and 5 (Last-Event-ID lands one event short).
    state = timelineReducer(state, {
      type: "events",
      events: [events[3], events[4]],
    });
    expect(state.rows).toHaveLength(5);
    expect(state.rows.map((row) => row.seq)).toEqual([1, 2, 3, 4, 5]);
    // A replayed duplicate with the same id but an earlier copy already seen
    // cannot replace the seen row.
    state = timelineReducer(state, {
      type: "events",
      events: [envelope("e5", 5, "frame.observed")],
    });
    expect(state.rows.find((row) => row.seq === 5)?.name).toBe("text.observed");
  });

  it("caps the window at the newest 500 rows and keeps the pin when evicted", () => {
    let state = initialTimeline();
    // 501 events: the window must hold the newest 500.
    state = timelineReducer(state, { type: "events", events: burst(WINDOW + 1) });
    expect(state.rows).toHaveLength(WINDOW);
    expect(state.rows[0].seq).toBe(2);
    expect(state.rows[WINDOW - 1].seq).toBe(WINDOW + 1);

    // Pin the oldest evicted row (seq 1) through its retained copy, then keep
    // streaming: the pin must stay selected while new events arrive.
    state = timelineReducer(state, { type: "pin", eventId: "e1" });
    for (let i = WINDOW + 2; i <= WINDOW + 40; i += 1) {
      state = timelineReducer(state, { type: "events", events: [envelope(`x${i}`, i)] });
    }
    expect(state.pinnedId).toBe("e1");
    expect(state.pinnedEvent?.event_id).toBe("e1");
    expect(state.rows.every((row) => row.event_id !== "e1")).toBe(true);
    expect(state.rows).toHaveLength(WINDOW);
  });

  it("keeps a pinned row selected while new events stream in", () => {
    let state = timelineReducer(initialTimeline(), { type: "events", events: burst(10) });
    state = timelineReducer(state, { type: "pin", eventId: "e5" });
    state = timelineReducer(state, { type: "events", events: burst(5, 11) });
    expect(state.pinnedId).toBe("e5");
    expect(state.pinnedEvent?.event_id).toBe("e5");
    expect(state.rows.map((row) => row.seq)).toEqual([...Array(15)].map((_, i) => i + 1));
  });

  it("unpins on an explicit null pin and ignores pins of unknown events", () => {
    let state = timelineReducer(initialTimeline(), { type: "events", events: burst(3) });
    state = timelineReducer(state, { type: "pin", eventId: "never-seen" });
    expect(state.pinnedId).toBeNull();
    state = timelineReducer(state, { type: "pin", eventId: "e1" });
    expect(state.pinnedId).toBe("e1");
    state = timelineReducer(state, { type: "pin", eventId: null });
    expect(state.pinnedId).toBeNull();
    expect(state.pinnedEvent).toBeNull();
  });

  it("pages older history in and re-caps the window to the newest 500", () => {
    // The window currently holds seq 501..1000 (the live tail).
    let state = timelineReducer(initialTimeline(), { type: "events", events: burst(WINDOW, 501) });
    expect(state.rows[0].seq).toBe(501);
    expect(state.maxSeq).toBe(1000);
    // A "load older" page returns seq 1..500 in seq order.
    state = timelineReducer(state, { type: "older", events: burst(500, 1), atStart: true });
    expect(renderedRows(state)).toHaveLength(WINDOW + 500);
    expect(state.rows).toHaveLength(WINDOW);
    expect(state.rows[0].seq).toBe(501);
    expect(state.older).toHaveLength(500);
    expect(state.pagedToStart).toBe(true);
    expect(state.maxSeq).toBe(1000);
    // Receiving the same page again (a retry) must not duplicate a row.
    state = timelineReducer(state, { type: "older", events: burst(500, 1), atStart: true });
    expect(renderedRows(state)).toHaveLength(WINDOW + 500);
  });

  it("tracks the highest seq as the durable cursor", () => {
    let state = timelineReducer(initialTimeline(), { type: "events", events: burst(3) });
    state = timelineReducer(state, { type: "events", events: [envelope("z", 9)] });
    expect(state.maxSeq).toBe(9);
  });

  it("resets to a fresh timeline", () => {
    let state = timelineReducer(initialTimeline(), { type: "events", events: burst(4) });
    state = timelineReducer(state, { type: "pin", eventId: "e2" });
    state = timelineReducer(state, { type: "reset" });
    expect(state).toEqual(initialTimeline());
  });
});
