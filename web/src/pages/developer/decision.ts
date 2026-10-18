/**
 * `src/pages/developer/decision.ts` — the pure reading of one
 * `decision.resolved` event (docs/spec/13: the developer view must show "did
 * it speak, and why not", and docs/spec/17 pins the decision.resolved payload:
 * provider, model, request id, request id source, latency, accepted,
 * generation).
 *
 * The pipeline emits one decision.resolved per adjudication the policy asked —
 * an applied answer, a fenced one, and a provider that could not be asked —
 * so the event is always the run's record of why a line was committed the way
 * it was. The mapping here turns that payload into the inspector's decision
 * row: the answer (accepted / abstain / late-discarded) and the degraded mode
 * the commit ran on the local evidence ("decision unavailable: <reason>").
 */

import type { EventEnvelope } from "../../api/sessions";

/** The two origins a decision request id can have (internal/providers/systemone). */
export type RequestIDSource = "client" | "provider";

export interface DecisionFacts {
  provider: string | null;
  model: string | null;
  /** The correlation id the provider logs can be searched by ("" when no call). */
  requestId: string | null;
  /** Which side issued the id; null when the payload does not say. */
  requestIdSource: RequestIDSource | null;
  /** The call's duration in milliseconds, as the event reports it. */
  latencyMs: number | null;
  /** The generation the answer was asked (and, when applied, committed) under. */
  generation: number | null;
  /** The observation seq the answer was stamped under (the join key). */
  observationSeq: number | null;
  /** The candidate option the model picked ("none" when it matched nothing). */
  choice: string | null;
  /** The one-phrase answer: accepted / abstain / late-discarded (<reason>). */
  outcome: string;
  /** True when the commit ran on the local evidence without the model's answer. */
  degraded: boolean;
  /** The reason phrase behind the outcome (the event's own wording). */
  reason: string | null;
}

function payload(envelope: EventEnvelope): Record<string, unknown> {
  const data = envelope.data;
  return data && typeof data === "object" ? (data as Record<string, unknown>) : {};
}

function str(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function num(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

/** The one-phrase answer for one decision.resolved payload.
 *
 * The event's three ways out (docs/spec/05, ADR-0050):
 * - an accepted answer the fences applied → "accepted" — the commit carries
 *   the model's candidate (commit reason CLEF_DISAMBIGUATED);
 * - an accepted answer the model used to refuse → "abstain": the model saw
 *   none of the candidates matched, and the commit stands on the local
 *   evidence (message_id unknown, speech_eligible false);
 * - an accepted answer the fences discarded late (the page turned while the
 *   call was out) → "late-discarded (<reason>)": the event names the fence;
 * - a provider that could not be asked (not configured, breaker open, or the
 *   endpoint failed) → degraded: the commit ran on the local evidence and the
 *   row says so with the reason the wiring recorded.
 */
export function decisionOutcome(data: Record<string, unknown>): {
  outcome: string;
  degraded: boolean;
  reason: string | null;
} {
  const reason = str(data.reason);
  const accepted = data.accepted === true;
  if (accepted) {
    if (data.applied === true) {
      if (str(data.choice) === "none") {
        return {
          outcome: "abstain (no candidate matched the visible text)",
          degraded: false,
          reason,
        };
      }
      return { outcome: "accepted", degraded: false, reason };
    }
    return { outcome: `late-discarded (${reason ?? "fenced"})`, degraded: false, reason };
  }
  // Declined: the model's answer never reached the fences — the role was not
  // configured, the breaker refused the call, or the endpoint failed — and
  // the commit proceeded on the local evidence alone.
  return {
    outcome: `local commit (decision unavailable: ${reason ?? "unknown"})`,
    degraded: true,
    reason,
  };
}

/** Read one decision.resolved envelope into the row's facts; null when the
 *  envelope is not a decision event at all. */
export function decisionFacts(envelope: EventEnvelope): DecisionFacts | null {
  if (envelope.name !== "decision.resolved") return null;
  const data = payload(envelope);
  const source = str(data.request_id_source);
  const { outcome, degraded, reason } = decisionOutcome(data);
  return {
    provider: str(data.provider),
    model: str(data.model),
    requestId: str(data.request_id),
    requestIdSource: source === "client" || source === "provider" ? source : null,
    latencyMs: num(data.latency_ms),
    generation: num(data.generation),
    observationSeq: num(data.observation_seq),
    choice: str(data.choice),
    outcome,
    degraded,
    reason,
  };
}

/** The newest decision in one trace page (a trace is per occurrence, so the
 *  last decision is the occurrence's — the same newest-wins rule the
 *  candidates and commit rows use). */
export function lastDecision(events: EventEnvelope[]): EventEnvelope | null {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event && event.name === "decision.resolved") return event;
  }
  return null;
}

/** The pipeline stage a trace event belongs to: the frame → OCR → candidates →
 *  Clef decision → commit → speech chain docs/spec/17's per-dialogue trace
 *  carries. The trace view labels each row with its stage so the chain reads
 *  left to right the way the spec's directed graph does. */
export const CHAIN_STAGES: Record<string, string> = {
  "frame.observed": "frame",
  "text.observed": "OCR",
  "message.candidates": "candidates",
  "decision.requested": "Clef request",
  "decision.resolved": "Clef decision",
  "message.committed": "commit",
  "speech.requested": "speech plan",
  "speech.ready": "speech ready",
  "speech.started": "speech playback",
  "speech.ended": "speech end",
  "speech.cancelled": "speech cancelled",
};

export function chainStage(name: string): string {
  return CHAIN_STAGES[name] ?? "event";
}

/** The human-facing origin label for a request id (the correlation proof). */
export function requestSourceLabel(source: RequestIDSource | null): string {
  if (source === "client") return "client-issued";
  if (source === "provider") return "provider-issued";
  return "origin unknown";
}
