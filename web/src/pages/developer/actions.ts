/**
 * `src/pages/developer/actions.ts` — the inspector's per-event actions as pure
 * functions (ADR-0083): the six correction labels and what each one records,
 * the trace's stored observation and recorded expectation extracted from its
 * events, the utterance's audio source from its speech.ready, and the WAV
 * waveform peaks the inspector draws on its canvas.
 *
 * Pure on purpose: the component layer (ActionPanel.tsx) wires these to the
 * API and the DOM, and this file is unit-tested without a browser, the same
 * way decision.ts and timeline.ts are.
 */

import type { EventEnvelope } from "../../api/sessions";

/** One of the inspector's six correction labels (docs/spec/09 "corrections"). */
export interface CorrectionLabel {
  /** Stable key, used for the button's testid. */
  key:
    | "speaker-incorrect"
    | "text-incorrect"
    | "wrong-region"
    | "not-narratable"
    | "duplicate"
    | "prediction-misfire";
  /** The button's visible text. */
  label: string;
  /** The correction's kind (docs/spec/08's CorrectionKind vocabulary). */
  kind: "speaker" | "text" | "region" | "classification" | "other";
  /** The correction's target table. A message target needs a committed row. */
  target: "message" | "other";
  /** The reason the label carries: the bench reads it back verbatim. */
  reason: string;
}

/** The six labels, in the order docs/spec/09 lists them. */
export const CORRECTION_LABELS: readonly CorrectionLabel[] = [
  {
    key: "speaker-incorrect",
    label: "Speaker incorrect",
    kind: "speaker",
    target: "message",
    reason: "inspector: the committed line was attributed to the wrong speaker",
  },
  {
    key: "text-incorrect",
    label: "Text incorrect",
    kind: "text",
    target: "message",
    reason: "inspector: the committed text is not what the textbox shows",
  },
  {
    key: "wrong-region",
    label: "Wrong region",
    kind: "region",
    target: "other",
    reason: "inspector: the observation read the wrong region of the screen",
  },
  {
    key: "not-narratable",
    label: "Not narratable",
    kind: "classification",
    target: "message",
    reason: "inspector: this text is not meant to be narrated",
  },
  {
    key: "duplicate",
    label: "Duplicate",
    kind: "other",
    target: "message",
    reason: "inspector: this line repeats one already narrated",
  },
  {
    key: "prediction-misfire",
    label: "Prediction misfire",
    kind: "other",
    target: "message",
    reason: "inspector: a predicted line committed on the wrong frame",
  },
];

/**
 * The correction body one label records for one occurrence. `messageId` is the
 * committed row, when the occurrence committed; a label before the commit has
 * no message row yet and targets the occurrence itself (`other`), which is
 * the pre-commit reading of the same label (ADR-0083). The correlation ids
 * ride the `evidence` member as typed string entries (the column's shape,
 * ADR-0052): `trace:<id>` is the member the server's fixture and the panel's
 * recorded-labels list filter on.
 */
export function correctionForLabel(
  label: CorrectionLabel,
  args: {
    profileId: string;
    traceId: string;
    /** The trace's committed row; null before the commit. */
    messageId: string | null;
    /** The trace's dialogue occurrence id (present on the observation's events). */
    occurrenceId: string | null;
    /** What the label was about: the observed or committed text. */
    text: string;
    /** The commit's speaker id, when the payload carries one. */
    speakerId: string | null;
  },
): {
  profile_id: string;
  kind: CorrectionLabel["kind"];
  target_type: CorrectionLabel["target"];
  target_id: string;
  before_value: string;
  reason: string;
  evidence: string[];
} {
  const target: CorrectionLabel["target"] =
    label.target === "message" && args.messageId ? "message" : "other";
  const targetId = target === "message" ? (args.messageId as string) : (args.occurrenceId ?? "");
  // The speaker label's before_value is the committed speaker id when the
  // commit carries one (ADR-0083 decision 1); the other labels name the
  // observation's reading.
  const beforeValue =
    label.key === "speaker-incorrect" && args.speakerId ? args.speakerId : args.text;
  const body = {
    profile_id: args.profileId,
    kind: label.kind,
    target_type: target,
    target_id: targetId,
    before_value: beforeValue,
    reason: label.reason,
    evidence: [
      `trace:${args.traceId}`,
      args.occurrenceId ? `occurrence:${args.occurrenceId}` : null,
      target === "message" ? `message:${args.messageId}` : null,
    ].filter((entry): entry is string => entry !== null),
  };
  return body;
}

/** The observation the re-run replays: the trace's last stored text.observed. */
export interface TraceObservation {
  traceId: string;
  eventSeq: number;
  rawText: string;
  normalizedKey: string;
  textState: string;
  confidence: number | null;
  regionId: string | null;
  sceneId: string | null;
  generation: number | null;
  /** The trace's committed row's store id; null before the commit. */
  messageId: string | null;
  /** The commit's speaker id, when the payload carries one. */
  speakerId: string | null;
  occurrenceId: string | null;
  /** The utterance's speech cache key (speech.ready); null when it never spoke. */
  cacheKey: string | null;
  /** The recorded ranking's top candidate, when the trace ranked. */
  topCandidate: { messageId: string; probability: number | null } | null;
}

function payloadOf(event: EventEnvelope): Record<string, unknown> {
  const data = event.data;
  return data && typeof data === "object" ? (data as Record<string, unknown>) : {};
}

function strOf(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function numOf(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/**
 * The trace's stored observation and recorded expectation, read from its
 * events the way the re-run and the fixture read them server-side: the last
 * text.observed is what the matcher re-ranks, the candidates/commit/decision
 * rows are what the run recorded, and speech.ready's cache_key is where the
 * utterance's audio lives in the speech cache.
 */
export function traceObservation(
  traceId: string,
  events: EventEnvelope[],
): TraceObservation | null {
  let observation: TraceObservation | null = null;
  let messageId: string | null = null;
  let speakerId: string | null = null;
  let occurrenceId: string | null = null;
  let cacheKey: string | null = null;
  let topCandidate: TraceObservation["topCandidate"] = null;
  for (const event of events) {
    const data = payloadOf(event);
    switch (event.name) {
      case "text.observed":
        occurrenceId = strOf(data.dialogue_occurrence_id) ?? occurrenceId;
        observation = {
          traceId,
          eventSeq: event.seq,
          rawText: strOf(data.raw_text) ?? "",
          normalizedKey: strOf(data.normalized_key) ?? strOf(data.normalized_text) ?? "",
          textState: strOf(data.text_state) ?? "",
          confidence: numOf(data.confidence),
          regionId: strOf(data.region_id),
          sceneId: strOf(data.scene_id),
          generation: numOf(data.generation),
          messageId: null,
          speakerId: null,
          occurrenceId: null,
          cacheKey: null,
          topCandidate: null,
        };
        break;
      case "message.candidates": {
        const candidates = Array.isArray(data.candidates) ? data.candidates : [];
        if (candidates.length > 0 && observation) {
          const top = (candidates[0] ?? {}) as Record<string, unknown>;
          const id = strOf(top.message_id);
          if (id) topCandidate = { messageId: id, probability: numOf(top.probability) };
        }
        break;
      }
      case "message.committed":
        messageId = strOf(data.message_id) ?? messageId;
        speakerId = strOf(data.speaker_id) ?? speakerId;
        break;
      case "speech.ready":
        cacheKey = strOf(data.cache_key) ?? cacheKey;
        break;
      default:
        break;
    }
  }
  if (!observation) return null;
  observation.messageId = messageId;
  observation.speakerId = speakerId;
  observation.occurrenceId = occurrenceId;
  observation.cacheKey = cacheKey;
  observation.topCandidate = topCandidate;
  return observation;
}

/**
 * The waveform peaks the inspector draws under the utterance's player: the
 * WAV's 16-bit PCM samples bucketed to one peak per canvas column. A pure
 * parser (no DOM): it reads the RIFF chunks itself, so a test can feed it the
 * exact bytes the API serves. Returns one value in [-1, 1] per bucket, or
 * null when the bytes are not a PCM WAV the inspector can draw.
 */
export function wavPeaks(bytes: Uint8Array, buckets: number): number[] | null {
  if (bytes.length < 44) return null;
  const view = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  // The RIFF/WAVE tags and the chunk ids are ASCII, so they read big-endian;
  // the sizes, rates and the PCM samples are the format's little-endian fields.
  if (view.getUint32(0, false) !== 0x52494646 /* RIFF */) return null;
  if (view.getUint32(8, false) !== 0x57415645 /* WAVE */) return null;
  let offset = 12;
  let sampleRate = 0;
  let bits = 0;
  let dataStart = -1;
  let dataLength = 0;
  while (offset + 8 <= bytes.length) {
    const id = view.getUint32(offset, false);
    const size = view.getUint32(offset + 4, true);
    if (id === 0x666d7420 /* fmt */) {
      const audioFormat = view.getUint16(offset + 8, true);
      const channelsIn = view.getUint16(offset + 10, true);
      sampleRate = view.getUint32(offset + 12, true);
      bits = view.getUint16(offset + 22, true);
      if (audioFormat !== 1 || bits !== 16 || channelsIn === 0) return null;
    } else if (id === 0x64617461 /* data */) {
      dataStart = offset + 8;
      dataLength = Math.min(size, bytes.length - dataStart);
      break; // the data chunk is the one the peaks are read from
    }
    if (size > bytes.length - offset) return null;
    offset += 8 + size + (size % 2); // chunks are word-aligned
  }
  if (dataStart < 0 || sampleRate === 0) return null;
  const samples = Math.floor(dataLength / 2);
  if (samples === 0) return null;
  if (buckets <= 0) return null;

  // Downsample without resampling: every bucket covers an equal slice of the
  // samples, and the peak is the slice's maximum absolute sample — the shape
  // the eye reads, not a spectrogram the inspector does not promise.
  const peaks = Array.from({ length: buckets }, () => 0);
  const per = samples / buckets;
  for (let bucket = 0; bucket < buckets; bucket += 1) {
    const from = Math.floor(bucket * per);
    const to = Math.min(samples, Math.floor((bucket + 1) * per));
    let peak = 0;
    for (let i = from; i < to; i += 1) {
      const at = dataStart + i * 2;
      const sample = Math.abs(view.getInt16(at, true)) / 32768;
      if (sample > peak) peak = sample;
    }
    peaks[bucket] = peak;
  }
  return peaks;
}

/** The trace id of a pinned event, or null when the event carries none. */
export function traceIdOf(event: EventEnvelope): string | null {
  return typeof event.trace_id === "string" && event.trace_id !== "" ? event.trace_id : null;
}
