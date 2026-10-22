/**
 * `src/api/onboarding.ts` — the onboarding agent surface (ADR-0028).
 *
 * A run executes as a durable `agent.run` job; this client is the snapshot
 * half of the UI (the SSE stream is the live half, see NewGame.tsx). The two
 * rules the page must not break:
 *
 * - nothing here publishes. `approve` is the only path that can lead to
 *   publication, and it only issues the human approval token;
 * - every body this client returns is rendered as text (docs/spec/14): the
 *   run's conversation and tool output are model- and source-shaped, and the
 *   page never interprets them as HTML or code.
 */

import { api } from "./client";
import type { components } from "./schema";

export type OnboardingRunRequest = components["schemas"]["OnboardingRunRequest"];
export type OnboardingRunCreated = components["schemas"]["OnboardingRunCreated"];
export type OnboardingRunState = components["schemas"]["OnboardingRunState"];
export type OnboardingToolCallView = components["schemas"]["OnboardingToolCallView"];
export type OnboardingSourceView = components["schemas"]["OnboardingSourceView"];
export type OnboardingReviewItem = components["schemas"]["OnboardingReviewItem"];
export type OnboardingMessageView = components["schemas"]["OnboardingMessageView"];
export type OnboardingDraftView = components["schemas"]["OnboardingDraftView"];
export type OnboardingAttemptResult = components["schemas"]["OnboardingAttemptResult"];
export type OnboardingUploadAccepted = components["schemas"]["OnboardingUploadAccepted"];

/** The setup board's snapshot half (ADR-0108, UX-06). */
export type SetupBoard = components["schemas"]["SetupBoard"];
export type SetupBoardCharacter = components["schemas"]["SetupBoardCharacter"];
export type SetupRailCell = components["schemas"]["SetupRailCell"];
export type SetupProgress = components["schemas"]["SetupProgress"];

/** The board's four columns, in board order (UX-06). */
export const SETUP_STAGES = ["finding", "brief", "voicing", "done"] as const;
export type SetupStage = (typeof SETUP_STAGES)[number];

export const onboardingApi = {
  /** Start a run (202: accepted, executing as an agent.run job). */
  create(body: OnboardingRunRequest, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs", {
      body,
      signal,
    }) as Promise<OnboardingRunCreated>;
  },

  /** The REST snapshot: state, steps, tool timeline, sources, quality, draft,
   *  review items and the redacted conversation. */
  state(runId: string, signal?: AbortSignal) {
    return api.get("/api/v1/onboarding/runs/{id}", {
      params: { id: runId },
      signal,
    }) as Promise<OnboardingRunState>;
  },

  /** Append a user turn; the run resumes to read it (409 while an attempt is
   *  in flight). */
  message(runId: string, text: string, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs/{id}/messages", {
      params: { id: runId },
      body: { text },
      signal,
    }) as Promise<OnboardingAttemptResult>;
  },

  pause(runId: string, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs/{id}/pause", {
      params: { id: runId },
      signal,
    }) as Promise<OnboardingAttemptResult>;
  },

  resume(runId: string, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs/{id}/resume", {
      params: { id: runId },
      signal,
    }) as Promise<OnboardingAttemptResult>;
  },

  cancel(runId: string, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs/{id}/cancel", {
      params: { id: runId },
      signal,
    }) as Promise<OnboardingAttemptResult>;
  },

  /** Abandon a blocked or paused run (issue #45, ADR-0028): the operator's
   *  stop. A run with no attempt in flight moves to the terminal abandoned
   *  state; an attempt in flight answers 409 (cancel the attempt first). */
  abandon(runId: string, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs/{id}/abandon", {
      params: { id: runId },
      signal,
    }) as Promise<OnboardingAttemptResult>;
  },

  /** Approve a review item. `review_item_id "publish"` is the publish
   *  boundary: the token is issued server-side and never crosses the wire. */
  approve(runId: string, reviewItemId: string, signal?: AbortSignal) {
    return api.post("/api/v1/onboarding/runs/{id}/approve", {
      params: { id: runId },
      body: { review_item_id: reviewItemId },
      signal,
    }) as Promise<OnboardingAttemptResult>;
  },

  /** Stage a script for a run. Multipart (the browser's shape); the response
   *  carries the sha256 the run request names. */
  upload(file: File, signal?: AbortSignal) {
    const form = new FormData();
    form.set("file", file);
    return api.post("/api/v1/onboarding/uploads", {
      form,
      signal,
    }) as Promise<OnboardingUploadAccepted>;
  },

  /** The setup board snapshot (ADR-0108): the page load that cannot trust
   *  the stream rebuilds the board from this body. It equals the board a
   *  client assembles by replaying the setup.* SSE events. */
  board(runId: string, signal?: AbortSignal) {
    return api.get("/api/v1/onboarding/runs/{id}/board", {
      params: { id: runId },
      signal,
    }) as Promise<SetupBoard>;
  },
};

/** The SSE frame the event stream sends: the envelope under `data`. */
export interface EventFrame {
  event_id: string;
  name: string;
  seq: number;
  data: Record<string, unknown>;
}

/** The typed payload of a setup.character_stage SSE event (ADR-0108):
 *  one card's move, with everything the board needs to place it. */
export interface CharacterStagePayload {
  run_id: string;
  character_id: string;
  name: string;
  stage: SetupStage;
  lines: number;
  at: string;
}

/** The typed payload of a setup.progress SSE event (ADR-0108). */
export interface ProgressPayload {
  run_id: string;
  lines_read: number;
  characters: number;
  voiced: number;
  total: number;
  elapsed: number;
  stage_rail: SetupRailCell[];
}
