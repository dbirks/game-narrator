/**
 * `src/lib/playReducer.test.ts` — synthetic-stub unit tests for the play
 * event fold (docs/spec/12 "No simulation theater": this is the (a) case —
 * envelopes are hand-built fixtures, no server, no fetch).
 */
import { describe, expect, it } from "vitest";

import type { EventEnvelope } from "../api/sessions";
import {
  MAX_RECENT_LINES,
  UNKNOWN_MESSAGE_ID,
  currentLine,
  displaySpeaker,
  initialPlayState,
  reducePlayEvent,
  reducePlayEvents,
} from "./playReducer";

let counter = 0;
function envelope(name: string, data: Record<string, unknown>): EventEnvelope {
  counter += 1;
  return {
    schema_version: 1,
    event_id: `evt-${counter}`,
    session_id: "01J9SESSION000000000000000",
    seq: counter,
    name,
    occurred_at: `2026-01-01T00:00:0${counter}Z`,
    data: data as Record<string, never>,
  };
}

function commit(overrides: Record<string, unknown> = {}): EventEnvelope {
  return envelope("message.committed", {
    dialogue_occurrence_id: `occ-${counter + 1000}`,
    message_id: "msg-row-1",
    speech_eligible: true,
    reason: "committed",
    text: "The harbour bell rang twice.",
    speaker_known: false,
    ...overrides,
  });
}

describe("reducePlayEvent (synthetic stubs)", () => {
  it("keeps the fold empty on non-commit events but still advances the seq cursor", () => {
    const state = reducePlayEvent(
      initialPlayState(),
      envelope("text.observed", { observation_seq: 1 }),
    );
    expect(state.lines).toEqual([]);
    expect(state.maxSeq).toBe(1);
  });

  it("folds a commit into the newest-first list with its evidence", () => {
    const frame = commit();
    const state = reducePlayEvent(initialPlayState(), frame);
    expect(state.lines).toHaveLength(1);
    const line = state.lines[0];
    expect(line.messageId).toBe("msg-row-1");
    expect(line.text).toBe("The harbour bell rang twice.");
    expect(line.speechEligible).toBe(true);
    expect(line.predictive).toBe(false);
    expect(state.maxSeq).toBe(frame.seq);
    expect(currentLine(state)?.eventId).toBe(frame.event_id);
  });

  it("treats a replayed occurrence as the same line, not a duplicate", () => {
    const first = commit({ dialogue_occurrence_id: "occ-dup" });
    let state = reducePlayEvent(initialPlayState(), first);
    // The reconnect resumed inside the durable window and the same commit
    // frame arrived again (same occurrence id, later seq, same event id).
    const replay = { ...first, seq: first.seq + 5 };
    state = reducePlayEvent(state, replay);
    expect(state.lines).toHaveLength(1);
    expect(state.maxSeq).toBe(first.seq + 5);
  });

  it("keeps the list newest-first and capped at MAX_RECENT_LINES", () => {
    let state = initialPlayState();
    for (let i = 0; i < MAX_RECENT_LINES + 10; i += 1) {
      state = reducePlayEvent(state, commit({ dialogue_occurrence_id: `occ-${i}` }));
    }
    expect(state.lines).toHaveLength(MAX_RECENT_LINES);
    // The newest commit is at the head: its seq is the highest one folded.
    expect(state.lines[0].seq).toBe(state.maxSeq);
  });

  it("flags a resync notice and survives an undecodable commit payload", () => {
    let state = reducePlayEvent(
      initialPlayState(),
      envelope("events.resync_required", { reason: "cursor predates the durable window" }),
    );
    expect(state.resyncRequired).toBe(true);

    const malformed = envelope("message.committed", { garbage: true });
    const before = state;
    state = reducePlayEvent(state, malformed);
    expect(state.lines).toEqual(before.lines);
    expect(state.maxSeq).toBeGreaterThanOrEqual(before.maxSeq);
  });

  it("folds a page of envelopes in order (the polling fallback shape)", () => {
    const frames = [commit(), commit(), envelope("session.stopped", { reason: "source_end" })];
    const state = reducePlayEvents(initialPlayState(), frames);
    expect(state.lines).toHaveLength(2);
    expect(state.lines[0].seq).toBeGreaterThan(state.lines[1].seq);
  });
});

describe("displaySpeaker (synthetic stubs)", () => {
  it("prefers the live decision's speaker when it is known", () => {
    const line = {
      messageId: "m1",
      speakerId: "Marina",
      speakerKnown: true,
    };
    expect(displaySpeaker(line as never, new Map([["m1", "Keeper"]]))).toBe("Marina");
  });

  it("falls back to the roster assignment, then to unknown", () => {
    const speakers = new Map([["m1", "Keeper"]]);
    const line = { messageId: "m1", speakerId: "", speakerKnown: false };
    expect(displaySpeaker(line as never, speakers)).toBe("Keeper");
    expect(
      displaySpeaker(
        { messageId: UNKNOWN_MESSAGE_ID, speakerId: "", speakerKnown: false } as never,
        speakers,
      ),
    ).toBe("unknown");
  });
});
