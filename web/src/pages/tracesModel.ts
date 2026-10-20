/**
 * `src/pages/tracesModel.ts` — the Traces screen's pure reading rules
 * (docs/design/plum-peach 2h, ADR-0109).
 *
 * The screen reads the durable developer event log (the same surface the
 * session inspector reads) and renders it as a line list plus the 5-step
 * strip. This file owns the reading rules so they are unit-tested without a
 * browser, the way `pages/developer/actions.ts` and `timeline.ts` are:
 * nothing here touches the API or the DOM. The e2e re-derives the same
 * values from the API and asserts the strip matches (plan.md item 7
 * acceptance: "for a seeded trace_id, the 5-step strip values match the API
 * record").
 *
 * The rules (ADR-0109 §3–§5):
 * - one line per committed occurrence, grouped by trace id;
 * - the 5-step strip maps onto the trace's own events (Saw frame → Read OCR
 *   → Matched → Who → Spoke), each step with a +ms offset from the trace's
 *   first event;
 * - spoken/skipped classify the utterance's terminal result, slow is the
 *   capture→commit latency above SLOW_THRESHOLD_MS.
 */

import type { EventEnvelope } from "../api/sessions";

/** The matcher's capture→commit budget: above it a line is "slow". */
export const SLOW_THRESHOLD_MS = 1000;

/** One strip step: the stage, its value, and the +ms offset. */
export interface StripStep {
  step: "saw" | "read" | "matched" | "who" | "spoke";
  value: string;
  /** Offset in ms from the trace's first event (0 for the first). */
  offsetMs: number;
}

/** One ranked candidate for the "Why this line" bars. */
export interface TraceCandidate {
  messageId: string;
  probability: number | null;
  components: Record<string, number>;
}

/** One entry of the Traces line list (2h left column) plus its strip. */
export interface TraceLine {
  /** The trace id — the deep-link target (?trace=). */
  traceId: string;
  /** The session the line committed in (the inspector deep link). */
  sessionId: string | null;
  /** The committed line's text as the textbox held it. */
  text: string;
  /** What it read: raw and normalised OCR, with the engine's confidence. */
  rawText: string;
  normalizedText: string;
  confidence: number | null;
  /** The stored crop (dev-trace sha); null when the sample was never kept. */
  cropSha: string | null;
  /** The stored frame (dev-trace sha); null when the sample was never kept. */
  frameSha: string | null;
  /** The frame number the observation came from. */
  frameSeq: number | null;
  /** The committed row's store id (corrections and the agent name it). */
  messageId: string | null;
  /** The commit's speaker id (null before the roster can name one). */
  speakerId: string | null;
  /** The commit's speaker_known flag (the roster could name the speaker). */
  speakerKnown: boolean;
  /** The utterance's speech cache key (Replay audio); null when it never
   *  resolved a voice. */
  cacheKey: string | null;
  /** The utterance resolved from the speech cache (cache hit). */
  fromCache: boolean | null;
  /** The utterance's terminal result (ended/skipped/muted/…). */
  speechResult: string | null;
  /** The utterance's terminal reason (the skip's why). */
  speechReason: string | null;
  /** Why this line: the ranked candidates, the matcher's reason code and the
   *  margin over the runner-up. */
  candidates: TraceCandidate[];
  reason: string | null;
  margin: number | null;
  /** The 5-step strip, in pipeline order. */
  steps: StripStep[];
  /** spoken / skipped / unresolved — the filter pills' vocabulary. */
  status: "spoken" | "skipped" | "unresolved";
  /** The slow filter: capture→commit latency above SLOW_THRESHOLD_MS. */
  slow: boolean;
  /** Capture→commit latency in ms; null when neither clock is present. */
  latencyMs: number | null;
  /** The line's timestamp (the commit's time). */
  occurredAt: string;
}

// ── payload reading (the inspector's own conventions) ───────────────────────

function payload(event: EventEnvelope): Record<string, unknown> {
  const data = event.data;
  return data && typeof data === "object" ? (data as Record<string, unknown>) : {};
}

function str(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function num(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function bool(value: unknown): boolean | null {
  return typeof value === "boolean" ? value : null;
}

/** The envelope's time as epoch ms (the durable log is UTC date-times). */
function atMs(event: EventEnvelope): number {
  const ms = Date.parse(event.occurred_at);
  return Number.isFinite(ms) ? ms : 0;
}

/** The newest event in the list matching the predicate (null if none). */
function lastWhere(
  events: EventEnvelope[],
  keep: (event: EventEnvelope) => boolean,
): EventEnvelope | null {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event && keep(event)) return event;
  }
  return null;
}

/** A message.candidates that actually carries candidates. */
function hasCandidates(event: EventEnvelope): boolean {
  const candidates = payload(event).candidates;
  return Array.isArray(candidates) && candidates.length > 0;
}

/** The trace's first event (the frame's first sighting) — the offset base. */
function firstEvent(events: EventEnvelope[]): EventEnvelope | null {
  let first: EventEnvelope | null = null;
  for (const event of events) {
    if (!first || event.seq < first.seq) first = event;
  }
  return first;
}

/**
 * The speaker's name for the Who step and the list. `characters` maps store
 * character ids to names (the profile's roster); a speaker the roster cannot
 * name reads as "Narrator" when the commit is unattributed, or the raw id
 * when the roster has not loaded.
 */
export function speakerName(speakerId: string | null, characters: Record<string, string>): string {
  if (speakerId === null) return "Narrator";
  return characters[speakerId] ?? speakerId;
}

/**
 * One line of the list view, from the trace's events (ADR-0109 §3–§4). The
 * trace is one dialogue occurrence: the commit is the line, the observation
 * is what it read, the candidates are why, and the speech events are how it
 * ended.
 */
export function traceLine(
  traceId: string,
  events: EventEnvelope[],
  characters: Record<string, string> = {},
): TraceLine {
  const base = firstEvent(events);
  const baseAt = base ? atMs(base) : 0;
  const offsetOf = (event: EventEnvelope): number => Math.max(0, atMs(event) - baseAt);

  const observed = lastWhere(events, (event) => event.name === "text.observed");
  const obsData = observed ? payload(observed) : {};
  const rawText = str(obsData.raw_text) ?? "";
  const normalizedText = str(obsData.normalized_text) ?? str(obsData.normalized_key) ?? "";
  const confidence = num(obsData.confidence);
  const cropSha = str(obsData.crop_asset);
  const frameSha = str(obsData.frame_asset);
  const frameSeq = num(obsData.frame_seq);

  const candidatesEvent = lastWhere(
    events,
    (event) => event.name === "message.candidates" && hasCandidates(event),
  );
  const candData = candidatesEvent ? payload(candidatesEvent) : {};
  const rawCandidates = Array.isArray(candData.candidates) ? candData.candidates : [];
  const candidates: TraceCandidate[] = rawCandidates.map((entry) => {
    const record = (entry ?? {}) as Record<string, unknown>;
    const components: Record<string, number> = {};
    const rawComponents = record.components;
    if (rawComponents && typeof rawComponents === "object") {
      for (const [name, value] of Object.entries(rawComponents as Record<string, unknown>)) {
        const v = num(value);
        if (v !== null) components[name] = v;
      }
    }
    return {
      messageId: str(record.message_id) ?? "unknown",
      probability: num(record.probability) ?? num(record.score),
      components,
    };
  });
  const reason = str(candData.reason);
  const margin = num(candData.margin);

  const committed = lastWhere(events, (event) => event.name === "message.committed");
  const commitData = committed ? payload(committed) : {};
  const text = str(commitData.text) ?? normalizedText ?? rawText;
  const messageId = str(commitData.message_id);
  const speakerId = str(commitData.speaker_id);
  const speakerKnown = bool(commitData.speaker_known) ?? false;

  const speechReady = lastWhere(events, (event) => event.name === "speech.ready");
  const readyData = speechReady ? payload(speechReady) : {};
  const cacheKey = str(readyData.cache_key);
  const fromCache = bool(readyData.from_cache);
  const speechEnd = lastWhere(events, (event) => event.name === "speech.ended");
  const endData = speechEnd ? payload(speechEnd) : {};
  const speechResult = str(endData.result);
  const speechReason = str(endData.reason);

  // The utterance's terminal: the start fired and it ran to the end is
  // "spoken"; a skip/mute/failure is "skipped"; a line that committed but
  // never resolved a voice ends skipped (no_approved_voice and friends).
  const started = events.some((event) => event.name === "speech.started");
  let status: TraceLine["status"] = "unresolved";
  if (started || speechResult === "ended") status = "spoken";
  else if (speechResult !== null && speechResult !== "ended") status = "skipped";

  // Capture→commit latency: the commit's time minus the observation's
  // capture clock (the source clock both carry). When the source clock is
  // absent the envelopes' own timestamps stand in; when neither is readable
  // the answer is null, not a guess.
  let latencyMs: number | null = null;
  if (committed && observed) {
    const commitAt = atMs(committed);
    const captureNs = num(obsData.capture_timestamp_ns);
    if (captureNs !== null) latencyMs = Math.max(0, commitAt - captureNs / 1e6);
    else latencyMs = Math.max(0, commitAt - atMs(observed));
  }
  const slow = latencyMs !== null && latencyMs > SLOW_THRESHOLD_MS;

  // ── the 5-step strip (ADR-0109 §4) ──────────────────────────────────────
  const steps: StripStep[] = [];
  steps.push(
    observed
      ? {
          step: "saw",
          value: frameSeq === null ? "frame —" : `frame #${frameSeq}`,
          offsetMs: offsetOf(observed),
        }
      : { step: "saw", value: "—", offsetMs: 0 },
  );
  steps.push(
    observed
      ? {
          step: "read",
          value: confidence === null ? "OCR conf —" : `conf ${confidence.toFixed(3)}`,
          offsetMs: offsetOf(observed),
        }
      : { step: "read", value: "—", offsetMs: 0 },
  );
  const top = candidates[0] ?? null;
  steps.push(
    candidatesEvent && top
      ? {
          step: "matched",
          value:
            top.probability === null
              ? "ranked —"
              : `${top.probability.toFixed(4)}${reason ? ` · ${reason}` : ""}`,
          offsetMs: offsetOf(candidatesEvent),
        }
      : { step: "matched", value: "—", offsetMs: 0 },
  );
  steps.push(
    committed
      ? {
          step: "who",
          value: speakerName(speakerId, characters),
          offsetMs: offsetOf(committed),
        }
      : { step: "who", value: "—", offsetMs: 0 },
  );
  if (speechReady) {
    steps.push({
      step: "spoke",
      value: fromCache === null ? "spoke" : fromCache ? "cache hit" : "cache miss",
      offsetMs: offsetOf(speechReady),
    });
  } else {
    steps.push({
      step: "spoke",
      value:
        speechResult === null
          ? "not spoken"
          : `${speechResult}${speechReason ? ` · ${speechReason}` : ""}`,
      offsetMs: speechEnd ? offsetOf(speechEnd) : 0,
    });
  }

  return {
    traceId,
    sessionId: committed?.session_id ?? events[0]?.session_id ?? null,
    text,
    rawText,
    normalizedText,
    confidence,
    cropSha,
    frameSha,
    frameSeq,
    messageId,
    speakerId,
    speakerKnown,
    cacheKey,
    fromCache,
    speechResult,
    speechReason,
    candidates,
    reason,
    margin,
    steps,
    status,
    slow,
    latencyMs,
    occurredAt: committed?.occurred_at ?? events[0]?.occurred_at ?? "",
  };
}

/**
 * The line list, from a page of the durable dev log (ADR-0109 §3): one entry
 * per committed occurrence, newest commit first. Traces that never committed
 * are not lines the list shows — the list is the lines the run spoke (or held
 * back), and a pre-commit trace has no line yet.
 */
export function traceLines(
  events: EventEnvelope[],
  characters: Record<string, string> = {},
): TraceLine[] {
  const byTrace = new Map<string, EventEnvelope[]>();
  for (const event of events) {
    const traceId = event.trace_id;
    if (!traceId) continue;
    const list = byTrace.get(traceId) ?? [];
    list.push(event);
    byTrace.set(traceId, list);
  }
  const lines: TraceLine[] = [];
  for (const [traceId, traceEvents] of byTrace) {
    const committed = lastWhere(traceEvents, (event) => event.name === "message.committed");
    if (!committed) continue; // the list is committed lines (ADR-0109 §3)
    lines.push(traceLine(traceId, traceEvents, characters));
  }
  return lines.sort((a, b) => b.occurredAt.localeCompare(a.occurredAt));
}

/**
 * The one-sentence reason for "why this line" (ADR-0109): the matcher's
 * reason code in words, with the probability and margin when it ranked. The
 * codes are the matcher's own tokens (internal/matcher/reasons.go, plus the
 * decision layer's additions); a code this map does not know prints as-is.
 */
export function matchReasonSentence(
  reason: string | null,
  topProbability: number | null,
  margin: number | null,
): string {
  const words: Record<string, string> = {
    MATCH_EXACT: "an exact normalised match",
    GRAPH_NEIGHBOR: "a graph neighbour of a recent line",
    PREDICTIVE_PREFIX: "a prediction from the corpus order",
    MATCH_FUZZY: "a close (fuzzy) match",
    ABSTAINED: "no confident match (abstained)",
    CLEF_DISAMBIGUATED: "adjudicated by the model",
    UNKNOWN_SPEAKER: "no speaker could be named",
    NO_NARRATABLE_TEXT: "no narratable text",
  };
  const base = reason ? (words[reason] ?? `matcher reason ${reason}`) : "no ranking recorded";
  if (topProbability === null) return `matched as ${base}`;
  const prob = `at ${(topProbability * 100).toFixed(1)}%`;
  const marginPart = margin !== null ? ` (margin ${margin.toFixed(4)})` : "";
  return `matched as ${base} ${prob}${marginPart}`;
}
