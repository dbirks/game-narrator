/**
 * The agent dock's contract (plan.md Phase 1 item 3, UX-01/UX-04): the
 * context derivation (the payload the dock sends with every message), the
 * chip seam (the pill and the docked header read the same words), and the
 * docked panel itself — send with context, the streamed tool cards and
 * narrative, the NEEDS-YOU block with its "or keep going" escape, and the
 * problem states (busy vs dead thread).
 *
 * The model boundary here is the fake fetch and the scripted event source:
 * the wire shapes are the ADR-0099 contract (the same JSON the server's
 * assistant tests assert), the component's behaviour is real.
 */

import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { assistantEventStreamUrl, type AssistantEventFrame } from "../../api/assistant";
import { AgentDock } from "./AgentDock";
import {
  agentChipText,
  deriveAgentContext,
  routeContextChip,
  selectionFromLocation,
} from "./agentContext";

afterEach(() => vi.unstubAllGlobals());

/** 26-char Crockford ids (games.ValidID: first char 0-7, no I/L/O/U). */
const GAME_ID = "0123456789ABCDEFGHJKMNPQRS";
const PROFILE_ID = "2123456789ABCDEFGHJKMNPQRS";
const CHARACTER_ID = "4123456789ABCDEFGHJKMNPQRS";
const OTHER_CHARACTER_ID = "6123456789ABCDEFGHJKMNPQRS";
const THREAD_ID = "0123456789ABCDEFGHJKMNPQRT";
const RUN_ID = "1123456789ABCDEFGHJKMNPQRS";
const QUESTION_ID = "2123456789ABCDEFGHJKMNPQRS";
const TOOL_CALL_ID = "3123456789ABCDEFGHJKMNPQRS";

const VOICES_LOCATION = {
  pathname: `/games/${GAME_ID}/voices`,
  search: `?profile=${PROFILE_ID}&character=${CHARACTER_ID}`,
};

// ── the context derivation (pure) ──────────────────────────────────────────

describe("deriveAgentContext", () => {
  it("derives the bare screen payload for a screen with no selection", () => {
    expect(deriveAgentContext({ pathname: "/library", search: "" })).toEqual({
      route: "/library",
    });
  });

  it("carries game, profile, the selection and the visible ids on a cast screen", () => {
    const context = deriveAgentContext(
      { pathname: `/games/${GAME_ID}/voices`, search: VOICES_LOCATION.search },
      { visibleIds: [CHARACTER_ID, OTHER_CHARACTER_ID] },
    );
    expect(context).toEqual({
      route: `/games/${GAME_ID}/voices${VOICES_LOCATION.search}`,
      game_id: GAME_ID,
      profile_id: PROFILE_ID,
      selection: { type: "character", id: CHARACTER_ID },
      visible_ids: [CHARACTER_ID, OTHER_CHARACTER_ID],
    });
  });

  it("drops a selection whose id the server would refuse (not an id)", () => {
    const context = deriveAgentContext({
      pathname: `/games/${GAME_ID}/voices`,
      search: `?character=${"not-an-id".padEnd(26, "-")}`,
    });
    expect(context.selection).toBeUndefined();
  });

  it("derives a line selection from ?line=", () => {
    const context = deriveAgentContext({
      pathname: `/traces/${RUN_ID}`,
      search: `?line=${CHARACTER_ID}`,
    });
    expect(context.selection).toEqual({ type: "line", id: CHARACTER_ID });
  });

  it("bounds the route and the visible ids to the server's rules", () => {
    const params = new URLSearchParams({ pad: "x".repeat(400) });
    const context = deriveAgentContext(
      { pathname: "/library", search: `?${params.toString()}` },
      { visibleIds: Array.from({ length: 80 }, (_, i) => `id-${i}`.padEnd(10, "0")) },
    );
    expect(context.route).toHaveLength(300);
    expect(context.visible_ids).toHaveLength(64);
  });

  it("omits visible ids when nothing was registered or they are empty", () => {
    expect(deriveAgentContext(VOICES_LOCATION, {}).visible_ids).toBeUndefined();
    expect(deriveAgentContext(VOICES_LOCATION, { visibleIds: [] }).visible_ids).toBeUndefined();
  });
});

describe("routeContextChip", () => {
  it("names the screen by its route (the chip's bare-screen case)", () => {
    expect(routeContextChip("/library")).toBe("library");
    expect(routeContextChip("/play")).toBe("play");
    expect(routeContextChip("/traces")).toBe("traces");
    expect(routeContextChip("/developer")).toBe("traces");
    expect(routeContextChip("/health")).toBe("health");
    expect(routeContextChip("/diagnostics")).toBe("health");
    expect(routeContextChip("/settings")).toBe("settings");
    expect(routeContextChip("/games/new")).toBe("setup");
  });

  it("names the game pages: cast for the merged cast family, game otherwise", () => {
    expect(routeContextChip(`/games/${GAME_ID}/cast`)).toBe("cast");
    expect(routeContextChip(`/games/${GAME_ID}/characters`)).toBe("cast");
    expect(routeContextChip(`/games/${GAME_ID}/voices`)).toBe("cast");
    expect(routeContextChip(`/games/${GAME_ID}/overview`)).toBe("game");
    expect(routeContextChip(`/games/${GAME_ID}/dialogue`)).toBe("game");
  });
});

describe("agentChipText", () => {
  it("shows the bare screen name without a resolved selection", () => {
    expect(agentChipText("cast", null)).toBe("cast");
  });

  it("shows `screen › name` once the selection has resolved", () => {
    expect(agentChipText("cast", "Marla")).toBe("cast › Marla");
  });
});

describe("selectionFromLocation", () => {
  it("reads the character selection from the URL", () => {
    expect(selectionFromLocation(VOICES_LOCATION)).toEqual({
      type: "character",
      id: CHARACTER_ID,
    });
  });

  it("returns null for a screen with no selection", () => {
    expect(selectionFromLocation({ pathname: "/library", search: "" })).toBeNull();
  });
});

// ── the docked panel ────────────────────────────────────────────────────────

/** The scripted event source behind the dock's seam: it records the named
 *  listeners and the test emits frames through them, the way the hub does. */
class DockFakeEventSource {
  static instances: DockFakeEventSource[] = [];
  #listeners = new Map<string, Set<(event: MessageEvent) => void>>();
  closed = false;

  constructor(public readonly url: string) {
    expect(url).toBe(assistantEventStreamUrl());
    DockFakeEventSource.instances.push(this);
  }

  addEventListener(name: string, listener: (event: MessageEvent) => void): void {
    this.#listeners.set(name, new Set([...(this.#listeners.get(name) ?? []), listener]));
  }

  removeEventListener(name: string, listener: (event: MessageEvent) => void): void {
    this.#listeners.get(name)?.delete(listener);
  }

  close(): void {
    this.closed = true;
  }

  get listenerNames(): string[] {
    return [...this.#listeners.keys()];
  }

  /** Emit one named frame the way the SSE hub writes it (a JSON `data:`). */
  emit(name: string, data: Record<string, unknown>, traceId: string = RUN_ID): void {
    const frame = { name, trace_id: traceId, data } as AssistantEventFrame;
    for (const listener of this.#listeners.get(name) ?? []) {
      listener({ data: JSON.stringify(frame) } as MessageEvent);
    }
  }
}

interface RecordedCall {
  method: string;
  url: string;
  body: Record<string, unknown> | null;
}

function jsonResponse(status: number, body: unknown): Response {
  const text = JSON.stringify(body);
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => body,
    text: async () => text,
  } as unknown as Response;
}

function problemResponse(status: number, code: string, title: string, detail: string): Response {
  return {
    ok: false,
    status,
    statusText: String(status),
    headers: new Headers({ "content-type": "application/problem+json" }),
    json: async () => ({
      type: `urn:game-narrator:error:slug-${code}`,
      title,
      status,
      detail,
      request_id: "req-dock-test",
      code,
    }),
    text: async () =>
      JSON.stringify({
        type: `urn:game-narrator:error:slug-${code}`,
        title,
        status,
        detail,
        request_id: "req-dock-test",
        code,
      }),
  } as unknown as Response;
}

interface RenderOptions {
  location?: string;
  /** The POST /assistant/messages answers, in order (the last one repeats;
   *  default: one 202 opening the thread). */
  messageResponses?: Response[];
  /** The GET /assistant/threads/{id} answer (default: the finished snapshot). */
  threadSnapshot?: (id: string) => Response;
}

function renderDock(options: RenderOptions = {}) {
  const location = options.location ?? `${VOICES_LOCATION.pathname}${VOICES_LOCATION.search}`;
  const calls: RecordedCall[] = [];
  DockFakeEventSource.instances = [];
  const messageResponses = options.messageResponses ?? [
    jsonResponse(202, { thread_id: THREAD_ID, run_id: RUN_ID, job_id: "job-dock-test", seq: 1 }),
  ];
  let messageCallCount = 0;

  // The finished-conversation snapshot: the state the server's truth would
  // hold once the scripted attempt completes (both turns, the tool card with
  // its duration, the succeeded run). The dock converges on it.
  const snapshotBody = (id: string) => ({
    id,
    run_id: RUN_ID,
    title: "Placeholder thread title",
    state: "succeeded",
    turns: [
      { seq: 1, role: "user", content: "Placeholder line for the operator to the agent." },
      { seq: 2, role: "assistant", content: "Placeholder reply from the scripted agent." },
    ],
    tool_cards: [{ id: TOOL_CALL_ID, name: "search", state: "succeeded", duration_ms: 42 }],
    questions: [],
  });

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = typeof input === "string" ? input : input.toString();
      const method = init?.method ?? "GET";
      let body: Record<string, unknown> | null = null;
      if (init?.body !== undefined && init?.body !== null) {
        body = JSON.parse(String(init.body)) as Record<string, unknown>;
      }
      calls.push({ method, url, body });

      if (method === "POST" && url.includes("/api/v1/assistant/messages")) {
        const response = messageResponses[Math.min(messageCallCount, messageResponses.length - 1)];
        messageCallCount += 1;
        return response;
      }
      if (method === "POST" && url.includes("/api/v1/assistant/answers")) {
        return jsonResponse(202, {
          thread_id: THREAD_ID,
          run_id: RUN_ID,
          question_id: QUESTION_ID,
          seq: 3,
        });
      }
      if (method === "GET" && url.includes("/api/v1/profiles/")) {
        return jsonResponse(200, {
          characters: [
            { id: CHARACTER_ID, name: "Marla" },
            { id: OTHER_CHARACTER_ID, name: "Corvin" },
          ],
        });
      }
      if (method === "GET" && url.includes("/api/v1/assistant/threads/")) {
        const id = url.split("/").pop() ?? "";
        return options.threadSnapshot?.(id) ?? jsonResponse(200, snapshotBody(id));
      }
      throw new Error(`unexpected fetch in the dock test: ${method} ${url}`);
    }),
  );

  const utils = render(
    <MemoryRouter initialEntries={[location]}>
      <AgentDock
        onClose={() => undefined}
        eventSourceFactory={(url) => new DockFakeEventSource(url)}
      />
    </MemoryRouter>,
  );
  return {
    ...utils,
    calls,
    source: () => DockFakeEventSource.instances.at(-1) as DockFakeEventSource | undefined,
  };
}

describe("AgentDock", () => {
  it("shows the bare screen name and the empty conversation on a screen without a selection", async () => {
    renderDock({ location: "/library" });
    await expect(screen.getByTestId("agent-dock-context")).toBeVisible();
    expect(screen.getByTestId("agent-dock-context")).toHaveTextContent("library");
    expect(screen.getByTestId("dock-chat-empty")).toHaveTextContent(
      "Ask the agent about this screen",
    );
    expect(screen.getByTestId("agent-dock-send")).toBeDisabled();
  });

  it("resolves the chip to `screen › selection` and sends the context payload with the message", async () => {
    const view = renderDock();

    // The chip follows the selection: the roster read resolves `?character=`
    // to the character's name.
    await waitFor(() => {
      expect(screen.getByTestId("agent-dock-context")).toHaveTextContent("cast › Marla");
    });

    fireEvent.change(screen.getByTestId("agent-dock-input"), {
      target: { value: "Placeholder line for the operator to the agent." },
    });
    fireEvent.click(screen.getByTestId("agent-dock-send"));

    // The outgoing message carries the full context payload (UX-01).
    const messageCall = await waitFor(() => {
      const call = view.calls.find(
        (c) => c.method === "POST" && c.url.includes("/api/v1/assistant/messages"),
      );
      if (call === undefined) throw new Error("the dock has not sent the message yet");
      return call;
    });
    expect(messageCall.body).toEqual({
      text: "Placeholder line for the operator to the agent.",
      context: {
        route: `${VOICES_LOCATION.pathname}${VOICES_LOCATION.search}`,
        game_id: GAME_ID,
        profile_id: PROFILE_ID,
        selection: { type: "character", id: CHARACTER_ID },
      },
    });

    // The user's turn is optimistic (their own words) — it shows at once.
    expect(screen.getByTestId("dock-turn-user-1")).toHaveTextContent(
      "Placeholder line for the operator to the agent.",
    );

    // The stream: a tool card appears running, settles succeeded, and the
    // narrative answer lands as the agent's bubble.
    const source = view.source();
    expect(source, "the dock opened exactly one stream").toBeDefined();
    expect(source?.listenerNames).toEqual(
      expect.arrayContaining(["assistant.message", "assistant.question", "tool.started"]),
    );
    act(() => {
      source?.emit("tool.started", { run_id: RUN_ID, tool_call_id: TOOL_CALL_ID, name: "search" });
      source?.emit("tool.completed", {
        run_id: RUN_ID,
        tool_call_id: TOOL_CALL_ID,
        name: "search",
        status: "succeeded",
      });
      source?.emit("assistant.message", {
        thread_id: THREAD_ID,
        run_id: RUN_ID,
        text: "Placeholder reply from the scripted agent.",
      });
    });

    expect(screen.getByTestId(`dock-tool-${TOOL_CALL_ID}`)).toHaveTextContent("search");
    expect(screen.getByTestId(`dock-tool-${TOOL_CALL_ID}`)).toHaveTextContent("succeeded");
    expect(await screen.findByTestId(/dock-turn-agent-/)).toHaveTextContent(
      "Placeholder reply from the scripted agent.",
    );

    // The finished attempt's snapshot refetch ran (the stream carries no
    // durations; the snapshot is the convergence point).
    await waitFor(() => {
      const snapshotCalls = view.calls.filter(
        (c) => c.method === "GET" && c.url.includes(`/api/v1/assistant/threads/${THREAD_ID}`),
      );
      expect(snapshotCalls.length, "the dock re-read the thread snapshot").toBeGreaterThanOrEqual(
        1,
      );
    });
  });

  it("renders NEEDS YOU with its answer pills and the keep-going escape (UX-04)", async () => {
    const view = renderDock();

    // The stream carries the events of the current run, so the run is
    // adopted first (one message), and the open-thread snapshot settles
    // before the question streams in.
    fireEvent.change(screen.getByTestId("agent-dock-input"), {
      target: { value: "Placeholder line for the operator to the agent." },
    });
    fireEvent.click(screen.getByTestId("agent-dock-send"));
    await waitFor(() => {
      expect(
        view.calls.filter((c) => c.method === "POST" && c.url.includes("/assistant/messages"))
          .length,
      ).toBe(1);
    });
    // The open-thread snapshot settles (its turns are what the panel shows)
    // before the question streams in, so the stream cannot be raced by the
    // snapshot refetch.
    await screen.findByTestId(/dock-turn-agent-/);

    const source = view.source();
    act(() => {
      source?.emit("assistant.question", {
        thread_id: THREAD_ID,
        run_id: RUN_ID,
        id: QUESTION_ID,
        kind: "question",
        prompt: "Which placeholder character is this line for?",
        options: ["Marla", "Corvin"],
      });
    });

    // The coral block with the question, its option pills and the escape.
    const block = screen.getByTestId("dock-needs-you");
    expect(block).toHaveTextContent("Which placeholder character is this line for?");
    expect(screen.getByTestId(`dock-question-option-${QUESTION_ID}-Marla`)).toBeEnabled();
    expect(screen.getByTestId(`dock-question-option-${QUESTION_ID}-Corvin`)).toBeEnabled();
    expect(screen.getByTestId(`dock-keep-going-${QUESTION_ID}`)).toHaveTextContent("or keep going");

    // "Or keep going" defers: the answer is durable and empty, the run keeps
    // moving — the card records the deferral, it does not vanish.
    fireEvent.click(screen.getByTestId(`dock-keep-going-${QUESTION_ID}`));
    const answerCall = await waitFor(() => {
      const call = view.calls.find(
        (c) => c.method === "POST" && c.url.includes("/api/v1/assistant/answers"),
      );
      if (call === undefined) throw new Error("the deferral was not sent");
      return call;
    });
    expect(answerCall.body).toEqual({
      thread_id: THREAD_ID,
      question_id: QUESTION_ID,
      answer: "",
    });
    await waitFor(() => {
      expect(screen.getByTestId(`dock-question-answer-${QUESTION_ID}`)).toHaveTextContent(
        "kept going",
      );
    });
  });

  it("shows the problem card on a conflict and offers a new conversation when the thread's own state is failed", async () => {
    // The first message opens the thread (202); the second hits the 409 the
    // server answers when the run has stopped on a fault. The dead-thread
    // decision reads the thread's own state from the snapshot — the server's
    // fact, not prose.
    const view = renderDock({
      messageResponses: [
        jsonResponse(202, {
          thread_id: THREAD_ID,
          run_id: RUN_ID,
          job_id: "job-dock-test",
          seq: 1,
        }),
        problemResponse(
          409,
          "RESOURCE_CONFLICT",
          "Conflict",
          "the thread's run has stopped on a fault",
        ),
      ],
      threadSnapshot: (id) =>
        jsonResponse(200, {
          id,
          run_id: RUN_ID,
          title: "Placeholder thread title",
          state: "failed",
          turns: [],
          tool_cards: [],
          questions: [],
        }),
    });

    const sendMessage = () => {
      fireEvent.change(screen.getByTestId("agent-dock-input"), {
        target: { value: "Placeholder line for the operator to the agent." },
      });
      fireEvent.click(screen.getByTestId("agent-dock-send"));
    };

    sendMessage(); // opens the thread
    await waitFor(() => {
      expect(
        view.calls.filter((c) => c.method === "POST" && c.url.includes("/assistant/messages"))
          .length,
      ).toBe(1);
    });
    sendMessage(); // the conflict

    // The problem card: the server's words, the request_id (the design's
    // error state), and — the thread's own state says failed — the new
    // conversation is offered.
    const problem = await screen.findByTestId("dock-problem");
    expect(problem).toHaveTextContent("Conflict");
    expect(screen.getByTestId("dock-problem-request-id")).toHaveTextContent("req-dock-test");
    const fresh = screen.getByTestId("dock-new-conversation");
    expect(fresh).toBeVisible();

    // Starting fresh clears the state: the input is usable again.
    fireEvent.click(fresh);
    await waitFor(() => {
      expect(screen.getByTestId("agent-dock-input")).toBeEnabled();
    });
    expect(screen.queryByTestId("dock-problem")).toBeNull();
  });
});
