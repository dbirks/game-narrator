/**
 * `decision.test.ts` — the decision row's contracts (docs/spec/13 "did it
 * speak, and why not"): the four answers a decision.resolved can carry
 * (accepted, abstain, late-discarded, degraded local commit), the request id
 * origin the correlation proof needs, and the newest-wins trace join.
 */

import { describe, expect, it } from "vitest";

import type { EventEnvelope } from "../../api/sessions";
import {
  decisionFacts,
  decisionOutcome,
  lastDecision,
  requestSourceLabel,
  type DecisionFacts,
} from "./decision";

function resolved(data: Record<string, unknown>, seq = 10): EventEnvelope {
  return {
    schema_version: 1,
    event_id: `evt-${seq}`,
    seq,
    name: "decision.resolved",
    occurred_at: "2026-01-01T00:00:00.000Z",
    data,
  } as unknown as EventEnvelope;
}

const BASE = {
  provider: "systemone",
  model: "clef-omni",
  region_id: "bottom",
  observation_seq: 42,
  generation: 3,
  text: "the harbour bell rang",
  latency_ms: 612,
  request_id: "01J9CLIENTISSUEDID0123456789",
  request_id_source: "client",
};

describe("decisionOutcome", () => {
  it("reports an applied answer as accepted", () => {
    const got = decisionOutcome({
      ...BASE,
      accepted: true,
      applied: true,
      reason: "answered",
      choice: "c1",
    });
    expect(got).toEqual({ outcome: "accepted", degraded: false, reason: "answered" });
  });

  it("reports an applied refusal as abstain", () => {
    const got = decisionOutcome({
      ...BASE,
      accepted: true,
      applied: true,
      reason: "answered",
      choice: "none",
    });
    expect(got.outcome).toMatch(/^abstain/);
    expect(got.degraded).toBe(false);
  });

  it("names the fence on a late-discarded answer", () => {
    const got = decisionOutcome({
      ...BASE,
      accepted: true,
      applied: false,
      reason: "stale_generation",
      choice: "c1",
    });
    expect(got).toEqual({
      outcome: "late-discarded (stale_generation)",
      degraded: false,
      reason: "stale_generation",
    });
  });

  it("names the reason on a declined answer as the degraded local commit", () => {
    const got = decisionOutcome({
      ...BASE,
      accepted: false,
      applied: false,
      reason: "provider_error:unreachable",
      request_id: "",
      request_id_source: undefined,
    });
    expect(got.degraded).toBe(true);
    expect(got.outcome).toBe("local commit (decision unavailable: provider_error:unreachable)");
  });

  it("degrades with an unknown reason when the payload carries none", () => {
    const got = decisionOutcome({ accepted: false });
    expect(got.outcome).toBe("local commit (decision unavailable: unknown)");
  });
});

describe("decisionFacts", () => {
  it("reads the row's fields from the event", () => {
    const facts = decisionFacts(
      resolved({ ...BASE, accepted: true, applied: true, reason: "answered", choice: "c2" }),
    );
    expect(facts).toEqual({
      provider: "systemone",
      model: "clef-omni",
      requestId: "01J9CLIENTISSUEDID0123456789",
      requestIdSource: "client",
      latencyMs: 612,
      generation: 3,
      observationSeq: 42,
      choice: "c2",
      outcome: "accepted",
      degraded: false,
      reason: "answered",
    } satisfies DecisionFacts);
  });

  it("keeps the provider-issued origin distinct from the client's", () => {
    const facts = decisionFacts(
      resolved({
        ...BASE,
        request_id_source: "provider",
        accepted: true,
        applied: true,
        reason: "answered",
        choice: "c1",
      }),
    );
    expect(facts?.requestIdSource).toBe("provider");
    expect(requestSourceLabel(facts?.requestIdSource ?? null)).toBe("provider-issued");
  });

  it("leaves the source unknown when the payload does not carry it", () => {
    const facts = decisionFacts(
      resolved({
        ...BASE,
        request_id_source: undefined,
        accepted: true,
        applied: true,
        reason: "answered",
        choice: "c1",
      }),
    );
    expect(facts?.requestIdSource).toBeNull();
    expect(requestSourceLabel(facts?.requestIdSource ?? null)).toBe("origin unknown");
  });

  it("refuses envelopes that are not decision events", () => {
    const other = { ...resolved({}), name: "message.committed" } as EventEnvelope;
    expect(decisionFacts(other)).toBeNull();
  });
});

describe("lastDecision", () => {
  it("returns null when the trace holds no decision", () => {
    const events = [
      {
        schema_version: 1,
        event_id: "a",
        seq: 1,
        name: "text.observed",
        occurred_at: "",
        data: {},
      } as EventEnvelope,
    ];
    expect(lastDecision(events)).toBeNull();
  });

  it("picks the newest decision in the trace", () => {
    const first = resolved(
      { ...BASE, accepted: true, applied: true, reason: "answered", choice: "none" },
      10,
    );
    const second = resolved(
      { ...BASE, accepted: true, applied: true, reason: "answered", choice: "c1" },
      11,
    );
    const events: EventEnvelope[] = [
      first,
      { ...second, name: "message.committed" } as EventEnvelope,
      second,
    ];
    expect(lastDecision(events)?.event_id).toBe(second.event_id);
  });
});
