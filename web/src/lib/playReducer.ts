/**
 * `src/lib/playReducer.ts` — the pure fold of the session event stream into
 * what the `/play` page renders (docs/spec/09 "UX robustness": SSE reconnect,
 * event replay and state reconciliation).
 *
 * The stream is the live path and the session snapshot is the degraded-mode
 * truth; this reducer only owns the event half. Every frame of
 * `GET /api/v1/events?session_id=…` that names a committed line folds into an
 * immutable list, newest first, bounded, deduplicated on the dialogue
 * occurrence so a reconnect replay cannot produce a second copy of a line the
 * user has already heard.
 *
 * Nothing here talks to the network; the payload cast is the boundary where
 * the generated contract's free-form `data` member becomes the shape the
 * runtime actually sends (internal/session/apply.go `committedData`).
 */
import type { EventEnvelope } from "../api/sessions";

/** How many committed lines the page keeps; older ones are still in the
 *  durable log and the developer inspector can page it. */
export const MAX_RECENT_LINES = 50;

/** The server's sentinel for a committed box the retrieval could not name
 *  (internal/session `UnknownMessage`): it is an answer, not an error. */
export const UNKNOWN_MESSAGE_ID = "unknown";

/** The committed-line payload as the runtime sends it (internal/session/apply.go). */
export interface CommittedPayload {
  dialogue_occurrence_id?: string;
  message_id?: string;
  speech_eligible?: boolean;
  reason?: string;
  /** The OCR engine's confidence in the read (internal/session committedData). */
  confidence?: number;
  text?: string;
  normalized_text?: string;
  speaker_id?: string;
  speaker_known?: boolean;
  predictive?: boolean;
}

/** One committed line as the page shows it. */
export interface CommittedLine {
  /** The global seq of the commit frame; the key of the dedupe table. */
  seq: number;
  eventId: string;
  occurredAt: string;
  /** The store's row id, or `unknown` for an abstained box. */
  messageId: string;
  /** What the OCR read, not the profile text: this is the evidence. */
  text: string;
  /** The OCR engine's confidence in the read (0–1), the operator's ROI label. */
  confidence: number | null;
  /** The decision's speaker ("" when none), separate from the assigned one. */
  speakerId: string;
  speakerKnown: boolean;
  speechEligible: boolean;
  reason: string;
  predictive: boolean;
  occurrenceId: string;
}

/** The fold's output: an immutable view over the session's committed lines. */
export interface PlayEventState {
  /** Committed lines, newest first, capped at MAX_RECENT_LINES. */
  lines: CommittedLine[];
  /** The highest seq folded so far; the cursor a polling fallback resumes at. */
  maxSeq: number;
  /** The hub said our cursor predates its durable window: reconcile from the
   *  snapshot and expect the replay to re-fold history. */
  resyncRequired: boolean;
}

export function initialPlayState(): PlayEventState {
  return { lines: [], maxSeq: 0, resyncRequired: false };
}

/**
 * Fold one event into the state. Events that are not commits only advance the
 * seq cursor — the page's live "something is moving" signal — and the resync
 * notice flips its flag. A frame whose data does not carry the commit shape is
 * skipped, not fatal: a gap is a degraded state, not a crash.
 */
export function reducePlayEvent(state: PlayEventState, envelope: EventEnvelope): PlayEventState {
  const maxSeq = Math.max(state.maxSeq, envelope.seq);
  if (envelope.name === "events.resync_required") {
    return { ...state, maxSeq, resyncRequired: true };
  }
  if (envelope.name !== "message.committed") {
    return state.maxSeq === maxSeq ? state : { ...state, maxSeq };
  }
  const data = envelope.data as unknown as CommittedPayload;
  // The runtime always sends message_id, including the `unknown` sentinel for
  // an abstained box (internal/session `decide`); a frame without one is not a
  // commit the page can place, so it is skipped rather than folded as a line.
  if (!data || typeof data !== "object" || typeof data.message_id !== "string") {
    return { ...state, maxSeq };
  }

  const occurrenceId =
    typeof data.dialogue_occurrence_id === "string"
      ? data.dialogue_occurrence_id
      : envelope.event_id;
  // A replayed commit (the reconnect resumed inside the durable window) must
  // not duplicate a line the user already saw.
  if (state.lines.some((line) => line.occurrenceId === occurrenceId)) {
    return { ...state, maxSeq };
  }

  const line: CommittedLine = {
    seq: envelope.seq,
    eventId: envelope.event_id,
    occurredAt: envelope.occurred_at,
    messageId: data.message_id,
    text: typeof data.text === "string" ? data.text : (data.normalized_text ?? ""),
    confidence: typeof data.confidence === "number" ? data.confidence : null,
    speakerId: typeof data.speaker_id === "string" ? data.speaker_id : "",
    speakerKnown: Boolean(data.speaker_known),
    speechEligible: Boolean(data.speech_eligible),
    reason: typeof data.reason === "string" ? data.reason : "",
    predictive: Boolean(data.predictive),
    occurrenceId,
  };
  const lines = [line, ...state.lines].slice(0, MAX_RECENT_LINES);
  return { ...state, maxSeq, lines };
}

/** Fold a page (the polling fallback hands over arrays). Order-preserving:
 *  the durable log is in seq order, so folding each frame in turn keeps the
 *  list newest-first without re-sorting. */
export function reducePlayEvents(
  state: PlayEventState,
  envelopes: EventEnvelope[],
): PlayEventState {
  let out = state;
  for (const envelope of envelopes) out = reducePlayEvent(out, envelope);
  return out;
}

/** The one line currently on screen: the newest commit. */
export function currentLine(state: PlayEventState): CommittedLine | null {
  return state.lines[0] ?? null;
}

/**
 * Resolve the committed line's *assigned* speaker from the profile's own data.
 * The live decision's speaker is model evidence (absent without a decision
 * provider); the assignment is the curator's own roster, and the page shows it
 * as such. `messageById` is message_id → speaker character name.
 */
export function displaySpeaker(line: CommittedLine, messageSpeaker: Map<string, string>): string {
  if (line.speakerKnown && line.speakerId !== "") return line.speakerId;
  const assigned = messageSpeaker.get(line.messageId);
  return assigned ?? "unknown";
}
