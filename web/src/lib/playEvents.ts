/**
 * `src/lib/playEvents.ts` — the session event stream folded into what the
 * Play operator view ("show workings", docs/design/plum-peach 2f) renders:
 * a bounded recent-event table (time · kind · detail) and the newest
 * match-candidate ranking with its probability bars.
 *
 * This is the presentational half of the stream; `playReducer.ts` owns the
 * committed-line fold (the now-speaking view). Both fold the same envelope,
 * so the one event stream feeds both views and switching between them can
 * never restart or duplicate a fold (ADR-0093 open question (b)).
 *
 * Nothing here talks to the network. Candidate payloads are read as the
 * runtime sends them (internal/session/apply.go `candidatesData`); a frame
 * whose data does not carry the shape is skipped, not fatal — a gap is a
 * degraded state, not a crash.
 */
import type { EventEnvelope } from "../api/sessions";

/** How many events the operator table keeps; older ones stay in the durable
 *  log the developer inspector pages. */
export const MAX_RECENT_EVENTS = 40;

/** The candidate payload as the runtime sends it (internal/session/apply.go). */
export interface CandidatePayload {
  message_id: string;
  score: number;
  probability: number;
  components?: Record<string, unknown>;
}

/** The newest ranking the operator's candidate bars show. */
export interface CandidateSet {
  /** The seq of the event the ranking came from. */
  seq: number;
  eventId: string;
  occurredAt: string;
  /** The OCR text the ranking ranked. */
  text: string;
  /** The matcher's own verdict (or the abstention flag). */
  reason: string;
  abstain: boolean;
  candidates: CandidatePayload[];
  candidateTotal: number;
}

/** One row of the operator's event table. */
export interface RecentEvent {
  seq: number;
  eventId: string;
  occurredAt: string;
  name: string;
  /** One short, honest detail line (never the whole payload). */
  detail: string;
}

/** The fold's output. */
export interface PlayEventStream {
  /** Recent events, newest first, capped at MAX_RECENT_EVENTS. */
  events: RecentEvent[];
  /** The newest message.candidates ranking, or null before one arrives. */
  candidates: CandidateSet | null;
  /** The highest seq folded so far. */
  maxSeq: number;
}

export function initialPlayEventStream(): PlayEventStream {
  return { events: [], candidates: null, maxSeq: 0 };
}

const EVENT_DETAIL_LIMIT = 64;

/** One human line out of an event's data (bounded, no payload dumps). */
function detailFor(name: string, data: unknown): string {
  if (typeof data !== "object" || data === null) return "";
  const record = data as Record<string, unknown>;
  const str = (key: string): string => {
    const value = record[key];
    return typeof value === "string" ? value : "";
  };
  const num = (key: string): string => {
    const value = record[key];
    return typeof value === "number" ? String(value) : "";
  };
  const clip = (text: string): string =>
    text.length > EVENT_DETAIL_LIMIT ? `${text.slice(0, EVENT_DETAIL_LIMIT - 1)}…` : text;

  switch (name) {
    case "message.committed":
      return clip(str("text") || str("normalized_text"));
    case "message.candidates": {
      const total = num("candidate_total");
      return total === "" ? "candidates" : `${total} candidate${total === "1" ? "" : "s"}`;
    }
    case "text.observed":
      return clip(str("text"));
    case "speech.ready":
      return `result ${str("result") || "ready"}`;
    case "speech.started":
      return str("result") === "" ? "speaking" : `result ${str("result")}`;
    case "speech.ended":
      return str("result") === "" ? "ended" : `result ${str("result")}`;
    case "speech.cancelled":
      return "cancelled";
    case "dialogue.advanced":
      return "player advanced the text box";
    default:
      return "";
  }
}

/** Fold one event into the stream view. Non-stream events still advance the
 *  seq cursor (the "something is moving" signal), like the line reducer. */
export function reducePlayEventStream(
  state: PlayEventStream,
  envelope: EventEnvelope,
): PlayEventStream {
  const maxSeq = Math.max(state.maxSeq, envelope.seq);
  if (envelope.name === "events.resync_required") {
    return { ...state, maxSeq };
  }
  const candidates =
    envelope.name === "message.candidates"
      ? (candidatesFrom(envelope) ?? state.candidates)
      : state.candidates;

  const event: RecentEvent = {
    seq: envelope.seq,
    eventId: envelope.event_id,
    occurredAt: envelope.occurred_at,
    name: envelope.name,
    detail: detailFor(envelope.name, envelope.data),
  };
  // A replayed frame (reconnect inside the durable window) must not
  // duplicate a row the operator already saw.
  const events = state.events.some((row) => row.eventId === event.eventId)
    ? state.events
    : [event, ...state.events].slice(0, MAX_RECENT_EVENTS);
  return { events, candidates, maxSeq };
}

/** Parse the message.candidates payload (the runtime's `candidatesData`). */
function candidatesFrom(envelope: EventEnvelope): CandidateSet | null {
  const data = envelope.data as unknown;
  if (typeof data !== "object" || data === null) return null;
  const record = data as Record<string, unknown>;
  if (!Array.isArray(record.candidates)) return null;
  const candidates: CandidatePayload[] = [];
  for (const raw of record.candidates as unknown[]) {
    if (typeof raw !== "object" || raw === null) continue;
    const c = raw as Record<string, unknown>;
    if (typeof c.message_id !== "string") continue;
    candidates.push({
      message_id: c.message_id,
      score: typeof c.score === "number" ? c.score : 0,
      probability: typeof c.probability === "number" ? c.probability : 0,
      components:
        typeof c.components === "object" && c.components !== null
          ? (c.components as Record<string, unknown>)
          : undefined,
    });
  }
  return {
    seq: envelope.seq,
    eventId: envelope.event_id,
    occurredAt: envelope.occurred_at,
    text: typeof record.text === "string" ? record.text : "",
    reason: typeof record.reason === "string" ? record.reason : "",
    abstain: Boolean(record.abstain),
    candidates,
    candidateTotal: typeof record.candidate_total === "number" ? record.candidate_total : 0,
  };
}
