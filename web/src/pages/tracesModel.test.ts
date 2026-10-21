/**
 * `src/pages/tracesModel.test.ts` — the Traces screen's reading rules
 * (ADR-0141): one line per committed occurrence, the 5-step strip with +ms
 * offsets, the spoken/skipped/slow filters, and the matcher's reason codes
 * in words. Pure: fake envelopes in, assertions out.
 */
import { describe, expect, it } from "vitest";

import type { EventEnvelope } from "../api/sessions";
import {
  SLOW_THRESHOLD_MS,
  matchReasonSentence,
  speakerName,
  traceLine,
  traceLines,
} from "./tracesModel";

let seq = 0;
let atMsValue = Date.UTC(2026, 9, 21, 12, 0, 0);
function envelope(
  name: string,
  traceId: string,
  data: Record<string, unknown>,
  overrides: Partial<EventEnvelope> = {},
): EventEnvelope {
  seq += 1;
  const at = new Date(atMsValue + seq * 1000).toISOString();
  return {
    schema_version: 1,
    event_id: `evt-${seq}`,
    session_id: "session-1",
    seq,
    trace_id: traceId,
    name,
    occurred_at: at,
    data: data as Record<string, never>,
    ...overrides,
  };
}

/** A full, speaking trace: saw → read → matched → who → spoke (cache hit). */
function speakingTrace(): EventEnvelope[] {
  atMsValue = Date.UTC(2026, 9, 21, 12, 0, 0);
  seq = 0;
  const capture = (atMsValue + 2600) * 1_000_000; // ns: 400ms before the commit
  return [
    envelope("text.observed", "trace-a", {
      frame_seq: 12,
      raw_text: "The gate creaks open.",
      normalized_text: "The gate creaks open.",
      confidence: 0.981,
      capture_timestamp_ns: capture,
      crop_asset: "dev-trace/trace-a/crop.png",
      frame_asset: "dev-trace/trace-a/frame.png",
    }),
    envelope("message.candidates", "trace-a", {
      reason: "MATCH_EXACT",
      margin: 0.02,
      candidates: [
        { message_id: "m-1", probability: 0.9734, components: { text: 0.97, visibility: 0.9 } },
        { message_id: "m-2", probability: 0.7211, components: { text: 0.7 } },
      ],
    }),
    envelope("message.committed", "trace-a", {
      text: "The gate creaks open.",
      message_id: "m-1",
      speaker_id: "char-1",
      speaker_known: true,
      margin: 0.25,
    }),
    envelope("speech.started", "trace-a", {}),
    envelope("speech.ready", "trace-a", {
      voice_id: "v-1",
      cache_key: "c-1",
      from_cache: true,
    }),
    envelope("speech.ended", "trace-a", { result: "ended" }),
  ];
}

/** A committed line the run held back: no voice, skipped. */
function skippedTrace(): EventEnvelope[] {
  atMsValue = Date.UTC(2026, 9, 21, 12, 5, 0);
  seq = 0;
  return [
    envelope("text.observed", "trace-b", {
      frame_seq: 13,
      raw_text: "A cold draft slipped past.",
      normalized_text: "A cold draft slipped past.",
      confidence: 0.9,
    }),
    envelope("message.candidates", "trace-b", {
      reason: "MATCH_EXACT",
      candidates: [{ message_id: "m-3", probability: 0.99 }],
    }),
    envelope("message.committed", "trace-b", {
      text: "A cold draft slipped past.",
      message_id: "m-3",
      speaker_id: null,
      speaker_known: false,
    }),
    envelope("speech.ended", "trace-b", { result: "skipped", reason: "no_approved_voice" }),
  ];
}

describe("traceLine", () => {
  it("carries the speaker (resolved), the time and the line", () => {
    const line = traceLine("trace-a", speakingTrace(), { "char-1": "Wren" });
    expect(line.traceId).toBe("trace-a");
    expect(line.text).toBe("The gate creaks open.");
    expect(line.messageId).toBe("m-1");
    expect(line.speakerId).toBe("char-1");
    expect(line.steps.find((s) => s.step === "who")?.value).toBe("Wren");
    expect(line.status).toBe("spoken");
    expect(line.slow).toBe(false);
  });

  it("names an unattributed commit's speaker", () => {
    const line = traceLine("trace-b", skippedTrace());
    expect(line.steps.find((s) => s.step === "who")?.value).toBe("Narrator");
    expect(speakerName(null, {})).toBe("Narrator");
  });

  it("classifies the held-back line as skipped with the server's reason", () => {
    const line = traceLine("trace-b", skippedTrace());
    expect(line.status).toBe("skipped");
    expect(line.speechResult).toBe("skipped");
    expect(line.speechReason).toBe("no_approved_voice");
    expect(line.steps.find((s) => s.step === "spoke")?.value).toBe("skipped · no_approved_voice");
  });

  it("classifies slow by the capture clock, above the documented threshold", () => {
    atMsValue = Date.UTC(2026, 9, 21, 12, 0, 0);
    seq = 0;
    const slow = [
      envelope("text.observed", "trace-c", {
        raw_text: "x",
        capture_timestamp_ns: (atMsValue + 1000) * 1_000_000, // 2s before the commit
      }),
      // The commit lands 2.5s later (the envelopes tick 1s per seq).
      envelope("message.committed", "trace-c", { text: "x", message_id: "m-9" }),
      envelope("message.committed", "trace-c", { text: "x", message_id: "m-9" }),
      envelope("message.committed", "trace-c", { text: "x", message_id: "m-9" }),
    ];
    const line = traceLine("trace-c", slow);
    expect(line.latencyMs).toBeGreaterThanOrEqual(SLOW_THRESHOLD_MS + 500);
    expect(line.slow).toBe(true);
  });

  it("falls back to the envelopes' clocks when the source clock is absent", () => {
    // Drop the source clock and pull the commit's envelope time close to the
    // observation's, so the fallback (commit.at - observed.at) is a small,
    // non-slow latency.
    const events = speakingTrace().map((event) => {
      const data = { ...(event.data as Record<string, unknown>) };
      delete data.capture_timestamp_ns;
      return { ...event, data: data as Record<string, never> };
    });
    const observed = events.find((event) => event.name === "text.observed") as EventEnvelope;
    const commit = events.find((event) => event.name === "message.committed") as EventEnvelope;
    commit.occurred_at = new Date(Date.parse(observed.occurred_at) + 200).toISOString();
    const line = traceLine("trace-a", events);
    expect(line.latencyMs).toBe(200);
    expect(line.slow).toBe(false);
  });
});

describe("traceLines", () => {
  it("lists one entry per committed occurrence, newest first, and skips pre-commit traces", () => {
    const events = [
      ...skippedTrace(), // committed at 12:05
      ...speakingTrace(), // committed at 12:00
      envelope("text.observed", "trace-none", { raw_text: "never committed" }),
    ];
    const lines = traceLines(events, { "char-1": "Wren" });
    expect(lines.map((l) => l.traceId)).toEqual(["trace-b", "trace-a"]);
    expect(lines[0]?.steps.find((s) => s.step === "who")?.value).toBe("Narrator");
    expect(lines[1]?.steps.find((s) => s.step === "who")?.value).toBe("Wren");
  });
});

describe("the 5-step strip", () => {
  it("maps the steps onto the trace's events with +ms offsets", () => {
    const detail = traceLine("trace-a", speakingTrace(), { "char-1": "Wren" });
    expect(detail.steps.map((s) => s.step)).toEqual(["saw", "read", "matched", "who", "spoke"]);
    // Each step's offset is measured from the trace's first event.
    expect(detail.steps[0]?.offsetMs).toBe(0);
    expect(detail.steps[2]?.offsetMs).toBe(1000);
    // The values: frame number, OCR confidence, probability with the
    // matcher's own reason token, the character's name, cache hit.
    expect(detail.steps[0]?.value).toBe("frame #12");
    expect(detail.steps[1]?.value).toBe("conf 0.981");
    expect(detail.steps[2]?.value).toContain("0.9734");
    expect(detail.steps[2]?.value).toContain("MATCH_EXACT");
    expect(detail.steps[3]?.value).toBe("Wren");
    expect(detail.steps[4]?.value).toBe("cache hit");
  });

  it("carries what it read (crop, raw and normalised) and why this line", () => {
    const detail = traceLine("trace-a", speakingTrace());
    expect(detail.rawText).toBe("The gate creaks open.");
    expect(detail.normalizedText).toBe("The gate creaks open.");
    expect(detail.confidence).toBeCloseTo(0.981);
    expect(detail.cropSha).toBe("dev-trace/trace-a/crop.png");
    expect(detail.frameSha).toBe("dev-trace/trace-a/frame.png");
    expect(detail.candidates).toHaveLength(2);
    expect(detail.reason).toBe("MATCH_EXACT");
    expect(detail.cacheKey).toBe("c-1");
    expect(detail.fromCache).toBe(true);
  });

  it("answers missing steps honestly (an observation that never ranked)", () => {
    const events = [
      envelope("text.observed", "trace-d", { raw_text: "A fragment." }),
      envelope("message.committed", "trace-d", { text: "A fragment.", message_id: "m-4" }),
    ];
    const detail = traceLine("trace-d", events);
    expect(detail.steps.find((s) => s.step === "matched")?.value).toBe("—");
    expect(detail.steps.find((s) => s.step === "spoke")?.value).toBe("not spoken");
  });
});

describe("matchReasonSentence", () => {
  it("words the matcher's own tokens, with the probability and margin", () => {
    expect(matchReasonSentence("MATCH_EXACT", 0.9734, 0.25)).toBe(
      "matched as an exact normalised match at 97.3% (margin 0.2500)",
    );
    expect(matchReasonSentence("PREDICTIVE_PREFIX", 0.4, null)).toBe(
      "matched as a prediction from the corpus order at 40.0%",
    );
    expect(matchReasonSentence(null, null, null)).toBe("matched as no ranking recorded");
    // A token this map does not know prints as-is.
    expect(matchReasonSentence("SOMETHING_NEW", 0.5, null)).toContain(
      "matcher reason SOMETHING_NEW",
    );
  });
});
