/**
 * `src/api/assistant.ts` — the context-aware assistant's wire surface
 * (ADR-0099; docs/design/plum-peach UX-01/UX-04/UX-05).
 *
 * The dock (⌘J) talks to the assistant through three operations, all over the
 * shared `api` client (CSRF retry, problem decoding, generated types):
 *
 * - one message (new thread or a turn in the caller's thread) — 202, the
 *   reply is not in the response, it streams over the event hub;
 * - one NEEDS-YOU answer (a durable user message; the run never blocks on
 *   it, UX-04);
 * - the thread snapshot (conversation tail, tool cards, questions) a page
 *   that missed the stream rebuilds from.
 *
 * The event stream itself is the generic `GET /api/v1/events` hub: the dock
 * subscribes with the `names=` filter and keeps only the frames whose
 * `trace_id` is the thread's run id (the assistant.run job carries it).
 * `EVENTS` below is the dock's subscription contract — one constant, so the
 * unit test and the component cannot disagree about the names.
 */

import { api, type Schemas } from "./client";

export type AssistantScreenContext = Schemas["AssistantScreenContext"];
export type AssistantMessageAccepted = Schemas["AssistantMessageAccepted"];
export type AssistantAnswerAccepted = Schemas["AssistantAnswerAccepted"];
export type AssistantThreadView = Schemas["AssistantThreadView"];
export type AssistantThreadList = Schemas["AssistantThreadList"];
export type AssistantTurnView = Schemas["AssistantTurnView"];
export type AssistantToolCardView = Schemas["AssistantToolCardView"];
export type AssistantQuestionView = Schemas["AssistantQuestionView"];
export type AssistantThreadSnapshot = Schemas["AssistantThreadSnapshot"];

/** The dock's SSE subscription: the narrative + question cards and the loop's
 *  step/tool events that drive the tool cards' tail. */
export const ASSISTANT_EVENT_NAMES =
  "assistant.message,assistant.question,agent.step,tool.started,tool.completed";

/** One SSE frame for the dock: the envelope members the dock reads. `data` is
 *  the event-specific payload (assistant.message: {thread_id, run_id, text};
 *  assistant.question: {thread_id, run_id, id, kind, prompt, options}). */
export interface AssistantEventFrame {
  event_id?: string;
  trace_id?: string;
  name: string;
  seq?: number;
  data?: Record<string, unknown> & {
    run_id?: string;
    text?: string;
    id?: string;
    kind?: string;
    prompt?: string;
    options?: string[];
    tool?: string;
    status?: string;
    error?: string;
    duration_ms?: number;
  };
}

export const assistantApi = {
  /** One dock message. With `threadId` it appends a turn to that thread;
   *  without one it opens the thread and returns its ids (the 202 half: the
   *  turn is accepted, the reply streams). `context` is the screen payload
   *  (UX-01), rendered into the model-facing user turn server-side; absent
   *  members are omitted from the body (JSON drops undefined), which is the
   *  wire's "no context". */
  send(
    threadId: string | undefined,
    text: string,
    context: AssistantScreenContext | undefined,
    signal?: AbortSignal,
  ): Promise<AssistantMessageAccepted> {
    const body: Schemas["AssistantMessageRequest"] = {
      text,
      context: context ?? undefined,
    };
    if (threadId !== undefined && threadId !== "") body.thread_id = threadId;
    return api.post("/api/v1/assistant/messages", {
      body,
      signal,
    }) as Promise<AssistantMessageAccepted>;
  },

  /** One NEEDS-YOU answer. For a confirmation card `answer` is "confirm" or
   *  "cancel"; an empty answer defers a question (UX-04's "or keep going"). */
  answer(
    threadId: string,
    questionId: string,
    answer: string,
    signal?: AbortSignal,
  ): Promise<AssistantAnswerAccepted> {
    return api.post("/api/v1/assistant/answers", {
      body: { thread_id: threadId, question_id: questionId, answer },
      signal,
    }) as Promise<AssistantAnswerAccepted>;
  },

  /** The caller's threads, newest first (the dock's "other conversations"
   *  list; the walk keeps one active thread in memory for now). */
  threads(signal?: AbortSignal): Promise<AssistantThreadList> {
    return api.get("/api/v1/assistant/threads", {
      signal,
    }) as Promise<AssistantThreadList>;
  },

  /** The snapshot the dock rebuilds from when it reopens on a thread it
   *  already owns: conversation tail, tool cards, open and recent questions. */
  thread(threadId: string, signal?: AbortSignal): Promise<AssistantThreadSnapshot> {
    return api.get("/api/v1/assistant/threads/{id}", {
      params: { id: threadId },
      signal,
    }) as Promise<AssistantThreadSnapshot>;
  },
};

/** The dock's SSE URL (the generic events hub, name-filtered). The browser
 *  EventSource resumes with Last-Event-ID natively, so a reconnect replays
 *  the frames the hub's bounded log still holds. */
export function assistantEventStreamUrl(): string {
  return `/api/v1/events?names=${encodeURIComponent(ASSISTANT_EVENT_NAMES)}`;
}
