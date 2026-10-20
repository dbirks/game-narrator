/**
 * `actions.test.ts` — the inspector's per-event actions (ADR-0083) as pure
 * logic: the six correction labels and the correction body each one records
 * (committed and pre-commit), the trace's stored observation read back from
 * its events, and the WAV peaks the waveform draws.
 */

import { describe, expect, it } from "vitest";

import type { EventEnvelope } from "../../api/sessions";
import { CORRECTION_LABELS, correctionForLabel, traceObservation, wavPeaks } from "./actions";

function envelope(
  name: string,
  seq: number,
  data: Record<string, unknown>,
  traceId?: string,
): EventEnvelope {
  return {
    schema_version: 1,
    event_id: `evt-${seq}`,
    seq,
    name,
    trace_id: traceId,
    occurred_at: "2026-01-01T00:00:00.000Z",
    data,
  } as unknown as EventEnvelope;
}

/** One 16-bit-PCM mono WAV: RIFF + fmt + data, the exact bytes the API serves. */
function makeWav(samples: number[], sampleRate = 8000): Uint8Array {
  const dataLen = samples.length * 2;
  const riffSize = 4 + (8 + 16) + (8 + dataLen); // WAVE + fmt chunk + data chunk
  const bytes = new Uint8Array(8 + riffSize);
  const view = new DataView(bytes.buffer);
  view.setUint32(0, 0x52494646, false); // "RIFF" (ASCII: big-endian of the value)
  view.setUint32(4, riffSize, true);
  view.setUint32(8, 0x57415645, false); // "WAVE"
  // fmt chunk
  view.setUint32(12, 0x666d7420, false); // "fmt "
  view.setUint32(16, 16, true);
  view.setUint16(20, 1, true); // PCM
  view.setUint16(22, 1, true); // mono
  view.setUint32(24, sampleRate, true);
  view.setUint32(28, sampleRate * 2, true); // byte rate
  view.setUint16(32, 2, true); // block align
  view.setUint16(34, 16, true); // bits
  // data chunk
  view.setUint32(36, 0x64617461, false); // "data"
  view.setUint32(40, dataLen, true);
  samples.forEach((sample, i) => view.setInt16(44 + i * 2, sample, true));
  return bytes;
}

describe("correctionForLabel", () => {
  const base = {
    profileId: "profile-1",
    traceId: "trace-1",
    occurrenceId: "occ-1",
    text: "The harbour bell rang twice.",
    speakerId: null,
  };

  it("records each label's kind and reason, targeting the committed row", () => {
    for (const label of CORRECTION_LABELS) {
      const body = correctionForLabel(label, {
        ...base,
        messageId: "msg-1",
      });
      expect(body.kind, label.key).toBe(label.kind);
      expect(body.reason, label.key).toBe(label.reason);
      expect(body.profile_id).toBe("profile-1");
      expect(body.before_value).toBe(base.text);
      expect(body.evidence).toEqual(expect.arrayContaining(["occurrence:occ-1"]));
    }
  });

  it("message-kind labels target the committed row with its id", () => {
    const body = correctionForLabel(CORRECTION_LABELS[0], {
      ...base,
      messageId: "msg-1",
    });
    expect(body.target_type).toBe("message");
    expect(body.target_id).toBe("msg-1");
    expect(body.evidence).toEqual(["trace:trace-1", "occurrence:occ-1", "message:msg-1"]);
  });

  it("the speaker label's before_value is the committed speaker id when the commit carries one", () => {
    const body = correctionForLabel(CORRECTION_LABELS[0], {
      ...base,
      messageId: "msg-1",
      speakerId: "speaker-row-9",
    });
    expect(body.before_value).toBe("speaker-row-9");
    const textBody = correctionForLabel(CORRECTION_LABELS[1], {
      ...base,
      messageId: "msg-1",
      speakerId: "speaker-row-9",
    });
    expect(textBody.before_value).toBe(base.text);
  });

  it("before the commit there is no message row: the label targets the occurrence", () => {
    const body = correctionForLabel(CORRECTION_LABELS[0], {
      ...base,
      messageId: null,
    });
    expect(body.target_type).toBe("other");
    expect(body.target_id).toBe("occ-1");
    expect(body.evidence).toEqual(["trace:trace-1", "occurrence:occ-1"]);
  });

  it("region-kind labels target the occurrence even after the commit", () => {
    const body = correctionForLabel(CORRECTION_LABELS[2], {
      ...base,
      messageId: "msg-1",
    });
    expect(body.target_type).toBe("other");
    expect(body.target_id).toBe("occ-1");
  });
});

describe("traceObservation", () => {
  const TRACE = "trace-1";

  it("reads the last text.observed and joins the commit, speech and ranking", () => {
    const events = [
      envelope(
        "text.observed",
        1,
        {
          raw_text: "The harbour bell",
          normalized_key: "harbour bell",
          text_state: "steady",
          confidence: 0.97,
          region_id: "region-a",
          scene_id: "scene-1",
          generation: 3,
          dialogue_occurrence_id: "occ-1",
        },
        TRACE,
      ),
      envelope(
        "message.candidates",
        2,
        {
          candidates: [
            { message_id: "msg-top", probability: 0.9 },
            { message_id: "msg-second", probability: 0.1 },
          ],
        },
        TRACE,
      ),
      envelope(
        "text.observed",
        3,
        {
          raw_text: "The harbour bell rang twice",
          normalized_key: "harbour bell rang twice",
          text_state: "steady",
          confidence: 0.99,
          region_id: "region-a",
          scene_id: "scene-1",
          generation: 3,
        },
        TRACE,
      ),
      envelope(
        "message.committed",
        4,
        { message_id: "msg-1", dialogue_occurrence_id: "occ-1" },
        TRACE,
      ),
      envelope("speech.ready", 5, { cache_key: "key-1", trace_id: TRACE }, TRACE),
    ];
    const got = traceObservation(TRACE, events);
    expect(got).not.toBeNull();
    expect(got?.traceId).toBe(TRACE);
    expect(got?.eventSeq).toBe(3); // the LAST observation, not the first
    expect(got?.rawText).toBe("The harbour bell rang twice");
    expect(got?.normalizedKey).toBe("harbour bell rang twice");
    expect(got?.messageId).toBe("msg-1");
    expect(got?.occurrenceId).toBe("occ-1");
    expect(got?.cacheKey).toBe("key-1");
    expect(got?.topCandidate).toEqual({ messageId: "msg-top", probability: 0.9 });
  });

  it("is null when the trace holds no observation", () => {
    const events = [envelope("message.committed", 1, { message_id: "msg-1" }, TRACE)];
    expect(traceObservation(TRACE, events)).toBeNull();
  });

  it("carries nulls for the parts the trace never produced", () => {
    const events = [
      envelope(
        "text.observed",
        1,
        { raw_text: "x", normalized_key: "x", text_state: "steady", confidence: 0.5 },
        TRACE,
      ),
    ];
    const got = traceObservation(TRACE, events);
    expect(got?.messageId).toBeNull();
    expect(got?.occurrenceId).toBeNull();
    expect(got?.cacheKey).toBeNull();
    expect(got?.topCandidate).toBeNull();
  });
});

describe("wavPeaks", () => {
  it("reads the RIFF header the format actually writes (ASCII tags)", () => {
    // The tags are the ASCII strings "RIFF"/"WAVE" on the wire: pin the helper
    // to the format's own byte order, so the parser's acceptance of makeWav's
    // bytes is an acceptance of real WAV bytes (the ETag'd fake-tts WAV).
    const wav = makeWav([1, 2, 3]);
    const ascii = (offset: number) =>
      Array.from(wav.subarray(offset, offset + 4))
        .map((code) => String.fromCharCode(code))
        .join("");
    expect(ascii(0)).toBe("RIFF");
    expect(ascii(8)).toBe("WAVE");
    expect(ascii(12)).toBe("fmt ");
    expect(ascii(36)).toBe("data");
  });

  it("returns one peak in [0,1] per bucket, tracking the sample magnitudes", () => {
    // 320 samples: 160 quiet, then 160 loud — a rising edge the peaks must show.
    const samples: number[] = [];
    for (let i = 0; i < 160; i += 1) samples.push(100);
    for (let i = 0; i < 160; i += 1) samples.push(30000);
    const drawn = wavPeaks(makeWav(samples), 10);
    expect(drawn).not.toBeNull();
    const peaks = drawn as number[];
    expect(peaks.length).toBe(10);
    for (const peak of peaks) {
      expect(peak).toBeGreaterThanOrEqual(0);
      expect(peak).toBeLessThanOrEqual(1);
    }
    // The loud half must be far louder than the quiet half.
    expect(peaks[9]).toBeGreaterThan(0.5);
    expect(peaks[0]).toBeLessThan(0.01);
    // Peaks are non-decreasing across this step input.
    for (let i = 1; i < peaks.length; i += 1) {
      expect(peaks[i]).toBeGreaterThanOrEqual(peaks[i - 1]);
    }
  });

  it("handles absolute values (a negative sample peaks the same as its mirror)", () => {
    const quiet = wavPeaks(makeWav([0, 0, 0, 0]), 2)?.[0] ?? 1;
    const pos = (wavPeaks(makeWav([1000, 1000, 1000, 1000]), 2) ?? [0, 0])[1];
    const neg = (wavPeaks(makeWav([-1000, -1000, -1000, -1000]), 2) ?? [0, 0])[1];
    expect(quiet).toBe(0);
    expect(pos).toBeCloseTo(1000 / 32768);
    expect(neg).toBeCloseTo(1000 / 32768);
  });

  it("returns null for bytes that are not a PCM WAV", () => {
    expect(wavPeaks(new Uint8Array(64), 10)).toBeNull();
    expect(wavPeaks(new Uint8Array([0x50, 0x45, 0x45, 0x4b]), 10)).toBeNull(); // "PEEK"
    // RIFF/WAVE but an 8-bit fmt: the inspector draws 16-bit PCM only.
    const bytes = makeWav([1, 2, 3]);
    const view = new DataView(bytes.buffer);
    view.setUint16(20, 6, true); // 8-bit PCM — not the inspector's format
    expect(wavPeaks(bytes, 4)).toBeNull();
  });
});
