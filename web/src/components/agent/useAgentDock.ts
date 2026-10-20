/**
 * `src/components/agent/useAgentDock.ts` — the docked panel's state (UX-01/04).
 *
 * One active thread per page load (the walk's model: the conversation is
 * about the screen you are on, not a thread browser). The state machine is
 * small and every transition is a server fact:
 *
 * - send → POST /assistant/messages (202: accepted, reply streams; the new
 *   thread's ids arrive in the response body);
 * - the event hub streams the attempt: tool.started / tool.completed (the
 *   tool cards), agent.step (the "working" line), assistant.message (the
 *   narrative bubble) and assistant.question (NEEDS-YOU cards);
 * - the thread snapshot (GET /assistant/threads/{id}) is the rebuild the
 *   panel runs when it opens on a known thread and after each finished
 *   attempt — it carries the tool cards' durations, which the events do
 *   not, and is the source of truth the stream keeps converging on.
 *
 * Conflicts are the server's contract, not the dock's invention: 409 while
 * an attempt is queued or running disables the input until the stream
 * settles; a failed run is the "start a new conversation" state. The dock
 * never blocks the run for a question (UX-04): answering is a durable user
 * message, and the run keeps moving while the question is open.
 */

import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation } from "react-router";

import {
  assistantApi,
  assistantEventStreamUrl,
  ASSISTANT_EVENT_NAMES,
  type AssistantEventFrame,
  type AssistantQuestionView,
  type AssistantToolCardView,
  type AssistantTurnView,
  type AssistantScreenContext,
} from "../../api/assistant";
import { ApiError } from "../../api/client";
import { deriveAgentContext, useAgentEnrichment } from "./agentContext";

export interface DockToolCard {
  id: string;
  name: string;
  /** "running" until tool.completed; then the result status. */
  state: string;
  durationMs?: number;
  error?: string;
  resultPreview?: string;
}

export interface DockQuestion {
  id: string;
  kind: "question" | "confirmation";
  prompt: string;
  options?: string[];
  /** Present once answered (the card then renders its answer, not its pills). */
  answer?: string;
}

export interface DockTurn {
  seq: number;
  role: "user" | "assistant";
  content: string;
}

/** A problem the dock renders in its own error card (the design's
 *  "coral border + request_id" state). */
export interface DockProblem {
  message: string;
  requestId: string;
  /** A failed run: the thread cannot be messaged again, a new conversation
   *  is the only forward direction (the server's 409 says exactly this). */
  threadDead: boolean;
}

const isRunning = (state: string) => state === "queued" || state === "running";

/** The event stream behind the dock's seam: the unit test substitutes one
 *  that drives the named listeners by hand. */
export interface DockEventSource {
  addEventListener(name: string, listener: (event: MessageEvent) => void): void;
  removeEventListener(name: string, listener: (event: MessageEvent) => void): void;
  close(): void;
}

interface Options {
  /** The SSE opener (test seam; default: the browser EventSource). */
  eventSourceFactory?: (url: string) => DockEventSource;
}

export function useAgentDock(options: Options = {}) {
  const location = useLocation();
  const enrichment = useAgentEnrichment();

  const [threadId, setThreadId] = useState<string | null>(null);
  const [runId, setRunId] = useState<string | null>(null);
  const [turns, setTurns] = useState<DockTurn[]>([]);
  const [tools, setTools] = useState<DockToolCard[]>([]);
  const [questions, setQuestions] = useState<DockQuestion[]>([]);
  const [runState, setRunState] = useState<string | null>(null);
  const [sending, setSending] = useState(false);
  const [answering, setAnswering] = useState(false);
  const [problem, setProblem] = useState<DockProblem | null>(null);

  // The stream's live view of the attempt: while an attempt is queued or
  // running the "working" line shows; the finished attempt's snapshot
  // refetch clears it.
  const attemptActive = sending || isRunning(runState ?? "");

  // The stream reads the current run through refs: the subscription is one
  // per open dock (opened on mount, closed on unmount), and a runId change
  // must not tear it down — the EventSource's Last-Event-ID resume and the
  // hub's replay are what keep a reconnect gap-free.
  const runIdRef = useRef(runId);
  runIdRef.current = runId;
  // The thread id the snapshot reads target: the finished-attempt refetch
  // refreshes the THREAD (its row), never the run (the run id 404s on the
  // thread read and must not be mistaken for a missing thread).
  const threadIdRef = useRef(threadId);
  threadIdRef.current = threadId;

  const applySnapshot = useCallback(
    (snapshot: {
      turns: AssistantTurnView[];
      tool_cards: AssistantToolCardView[];
      questions: AssistantQuestionView[];
      state: string;
    }) => {
      setTurns(snapshot.turns.map((t) => ({ seq: t.seq, role: t.role, content: t.content })));
      setTools(
        snapshot.tool_cards.map((c) => ({
          id: c.id,
          name: c.name,
          state: c.state,
          durationMs: c.duration_ms,
          error: c.error,
          resultPreview: c.result_preview,
        })),
      );
      setQuestions(
        snapshot.questions.map((q) => ({
          id: q.id,
          kind: q.kind,
          prompt: q.prompt,
          options: q.options,
          answer: q.answer,
        })),
      );
      setRunState(snapshot.state);
    },
    [],
  );

  const refreshRef = useRef<(id: string) => Promise<void> | void>(() => undefined);

  const onEvent = useCallback((event: MessageEvent) => {
    let frame: AssistantEventFrame;
    try {
      frame = JSON.parse(String(event.data)) as AssistantEventFrame;
    } catch {
      return; // a frame we cannot parse is skipped, not a fault
    }
    const current = runIdRef.current;
    if (current === null) return;
    const dataRunId = frame.data?.run_id;
    if (frame.trace_id !== current && dataRunId !== current) return;

    switch (frame.name) {
      case "assistant.message": {
        const text = frame.data?.text;
        if (text !== undefined && text !== "") {
          setTurns((prev) => [
            ...prev,
            { seq: frame.seq ?? prev.length, role: "assistant", content: text },
          ]);
        }
        setRunState("succeeded");
        // The finished attempt's snapshot carries the tool cards' durations
        // (the events do not) — the panel converges on it. The refetch is the
        // THREAD's read (the row the 202 named): the frame carries it, and
        // the run id it rides with is not a thread id.
        const snapshotId =
          typeof frame.data?.thread_id === "string" && frame.data.thread_id !== ""
            ? frame.data.thread_id
            : threadIdRef.current;
        if (snapshotId !== null) void refreshRef.current(snapshotId);
        break;
      }
      case "assistant.question": {
        const data = frame.data;
        if (data?.id !== undefined) {
          setQuestions((prev) => [
            ...prev,
            {
              id: String(data.id),
              kind: (data.kind as "question" | "confirmation") ?? "question",
              prompt: String(data.prompt ?? ""),
              options: data.options,
            },
          ]);
        }
        break;
      }
      case "tool.started": {
        const data = frame.data;
        const id = String(data?.tool_call_id ?? data?.id ?? "");
        if (id !== "") {
          setTools((prev) => [
            ...prev,
            { id, name: String(data?.name ?? "tool"), state: "running" },
          ]);
        }
        break;
      }
      case "tool.completed": {
        const data = frame.data;
        const id = String(data?.tool_call_id ?? data?.id ?? "");
        const status = String(data?.status ?? "succeeded");
        setTools((prev) =>
          prev.map((card) => (card.id === id ? { ...card, state: status } : card)),
        );
        break;
      }
      case "agent.step":
        // The loop's step tick: the "working" line is derived from the
        // attempt's state, not from each step (a step per tick would make
        // the dock narrate the machine to itself).
        setRunState((state) => (state === null ? "running" : state));
        break;
      default:
        break;
    }
  }, []);

  // One stream per open dock (the Phase 0 "one subscription per page"
  // discipline, mirrored): the panel mounts only while open, so the stream
  // opens on mount and closes on unmount. Named listeners per event name —
  // the hub names every frame; the default onmessage would only see unnamed
  // ones. The test seam is read through a ref so a new factory cannot
  // re-subscribe mid-conversation. An environment without EventSource
  // (jsdom) degrades to snapshot-only: the panel still rebuilds from the
  // thread read, the stream just has nothing to open.
  const factoryRef = useRef(options.eventSourceFactory);
  factoryRef.current = options.eventSourceFactory;
  useEffect(() => {
    let source: DockEventSource;
    const factory = factoryRef.current;
    if (factory !== undefined) {
      source = factory(assistantEventStreamUrl());
    } else if (typeof EventSource !== "undefined") {
      source = new EventSource(assistantEventStreamUrl());
    } else {
      return;
    }
    const names = ASSISTANT_EVENT_NAMES.split(",");
    for (const name of names) source.addEventListener(name, onEvent);
    return () => {
      for (const name of names) source.removeEventListener(name, onEvent);
      source.close();
    };
  }, [onEvent]);

  /** Rebuild the panel from the snapshot (opening on a known thread, or a
   *  finished attempt — the snapshot carries the durations the stream does
   *  not). A 404 has two honest causes: the commit race (the 202 returns
   *  before the job's first commit writes the thread row — bounded by one
   *  retry, which is what settles the race) and a stale id (the server no
   *  longer has the thread — settle on the empty state, never a crash, and
   *  never a wipe of a live conversation on the first transient read). */
  const snapshot404Retried = useRef(false);
  const refresh = useCallback(
    (id: string) => {
      return assistantApi
        .thread(id)
        .then((snapshot) => {
          snapshot404Retried.current = false;
          applySnapshot(snapshot);
        })
        .catch((error: unknown) => {
          if (!(error instanceof ApiError) || error.status !== 404) return;
          if (threadIdRef.current !== id) return; // the panel moved on; a stale read
          if (snapshot404Retried.current) {
            // The retry did not find the row either: this thread is gone
            // (another user's browser, a stale id) — the empty state is the
            // honest answer.
            snapshot404Retried.current = false;
            setThreadId(null);
            setRunId(null);
            setTurns([]);
            setTools([]);
            setQuestions([]);
            setRunState(null);
            return;
          }
          // Commit race: the job writes the row milliseconds after the 202.
          // One bounded retry distinguishes the race from a missing thread
          // without ever wiping the in-flight conversation.
          snapshot404Retried.current = true;
          setTimeout(() => {
            snapshot404Retried.current = false;
            if (threadIdRef.current === id) void refresh(id);
          }, 400);
        });
    },
    [applySnapshot],
  );

  // The finished-attempt refetch and the open-on-thread rebuild both run
  // the same snapshot read; the stream reaches it through the ref.
  useEffect(() => {
    refreshRef.current = refresh;
  }, [refresh]);

  // Opening the dock on a thread we already own rebuilds from the snapshot
  // (the frames that arrived while the panel was closed are gone; the
  // snapshot is what the panel shows when it comes back).
  useEffect(() => {
    if (threadId !== null) void refresh(threadId);
  }, [threadId, refresh]);

  const fail = useCallback((error: unknown, dead: boolean) => {
    if (error instanceof ApiError) {
      setProblem({
        message: `${error.title ?? "The request failed"} — ${error.detail ?? "no detail"}`,
        requestId: error.requestId ?? "",
        threadDead: dead,
      });
    } else {
      setProblem({
        message: "The agent is unreachable right now; the message was not sent.",
        requestId: "",
        threadDead: dead,
      });
    }
  }, []);

  /** One dock message with the screen context (UX-01). The user turn is
   *  optimistic — it is the operator's own words — and the server's seq (or
   *  the snapshot) replaces the local one; a conflict settles on the
   *  server's state, which is why the turn's seq is cosmetic. */
  const send = useCallback(
    (text: string) => {
      const trimmed = text.trim();
      if (trimmed === "" || sending || answering) return;
      const context: AssistantScreenContext = deriveAgentContext(
        { pathname: location.pathname, search: location.search },
        enrichment,
      );
      setSending(true);
      setProblem(null);
      const localSeq = turns.length + 1;
      setTurns((prev) => [...prev, { seq: localSeq, role: "user", content: trimmed }]);
      assistantApi
        .send(threadId ?? undefined, trimmed, context)
        .then((accepted) => {
          if (threadId === null) {
            setThreadId(accepted.thread_id);
            setRunId(accepted.run_id);
          }
          // The optimistic turn's seq is local; the server's is the truth
          // (an existing thread appends; a new thread starts the log).
          const seq = accepted.seq;
          if (seq !== undefined && threadId !== null) {
            setTurns((prev) => [...prev.slice(0, -1), { seq, role: "user", content: trimmed }]);
          }
        })
        .catch((error: unknown) => {
          // A 409 is the server's conflict fact (an attempt already owns the
          // run's worklist, or the run stopped on a fault). Whether the thread
          // is dead or merely busy is the thread's own state — read from the
          // snapshot, not prose-matched from the problem document.
          if (error instanceof ApiError && error.status === 409 && threadId !== null) {
            assistantApi
              .thread(threadId)
              .then((snapshot) => {
                const dead = snapshot.state === "failed";
                if (dead) setRunState("failed");
                fail(error, dead);
              })
              .catch(() => fail(error, false));
          } else {
            fail(error, false);
          }
        })
        .finally(() => setSending(false));
    },
    [
      answering,
      enrichment,
      fail,
      location.pathname,
      location.search,
      sending,
      threadId,
      turns.length,
    ],
  );

  /** One NEEDS-YOU answer (UX-04). An empty answer defers ("or keep going"):
   *  the question stays visible with its deferred state, the run keeps
   *  moving, and the attempt owes the follow-up. */
  const answer = useCallback(
    (questionId: string, text: string) => {
      if (threadId === null || answering) return;
      setAnswering(true);
      setProblem(null);
      assistantApi
        .answer(threadId, questionId, text)
        .then(() => {
          setQuestions((prev) =>
            prev.map((q) => (q.id === questionId ? { ...q, answer: text } : q)),
          );
        })
        .catch((error: unknown) => fail(error, false))
        .finally(() => setAnswering(false));
    },
    [answering, fail, threadId],
  );

  /** The failed-run state's only forward direction: forget the thread (the
   *  server keeps it; the next message opens a new one). */
  const newConversation = useCallback(() => {
    setThreadId(null);
    setRunId(null);
    setTurns([]);
    setTools([]);
    setQuestions([]);
    setRunState(null);
    setProblem(null);
  }, []);

  return {
    threadId,
    runId,
    turns,
    tools,
    questions,
    runState,
    attemptActive,
    sending,
    answering,
    problem,
    send,
    answer,
    newConversation,
    refresh,
  };
}
