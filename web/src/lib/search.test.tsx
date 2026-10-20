import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "../api/client";
import {
  AGENT_QUERY_EVENT,
  emitAgentQuery,
  normalizeAgentQuery,
  searchCatalog,
  subscribeAgentQuery,
  useCatalogSearch,
  type SearchResponse,
} from "./search";

/**
 * The ⌘K search client's contract (ADR-0098; UX-02's Tab handoff): the
 * normalisation rule, the agent-query event both sides compile against,
 * and the debounced, abortable, newest-read-wins search state.
 */

function searchResponse(body: Partial<SearchResponse> = {}): Response {
  const json: SearchResponse = { query: "", agent_prompt: "", groups: [], ...body };
  return {
    ok: true,
    status: 200,
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => json,
    text: async () => JSON.stringify(json),
  } as Response;
}

describe("normalizeAgentQuery", () => {
  it("trims and collapses internal whitespace (the server's q rule)", () => {
    expect(normalizeAgentQuery("  boy   in   an oilskin coat  ")).toBe("boy in an oilskin coat");
    expect(normalizeAgentQuery("\t\n")).toBe("");
    expect(normalizeAgentQuery("")).toBe("");
  });
});

describe("the agent handoff event (UX-02's Tab)", () => {
  it("dispatches the query and the subscriber receives it; unsubscribe stops it", () => {
    const seen: string[] = [];
    const unsubscribe = subscribeAgentQuery((detail) => seen.push(detail.query));
    emitAgentQuery("boy");
    expect(seen).toEqual(["boy"]);
    unsubscribe();
    emitAgentQuery("nobody");
    expect(seen).toEqual(["boy"]);
  });

  it("ignores events with a malformed detail", () => {
    const seen: string[] = [];
    const unsubscribe = subscribeAgentQuery((detail) => seen.push(detail.query));
    window.dispatchEvent(new CustomEvent(AGENT_QUERY_EVENT, { detail: { query: 42 } }));
    window.dispatchEvent(new CustomEvent(AGENT_QUERY_EVENT));
    expect(seen).toEqual([]);
    unsubscribe();
  });
});

describe("searchCatalog", () => {
  it("sends q, kinds and limit to /api/v1/search", async () => {
    const calls: Array<{ url: string; signal?: AbortSignal | null }> = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        calls.push({ url: String(input), signal: init?.signal });
        return searchResponse({ query: "boy", agent_prompt: "boy" });
      }),
    );
    try {
      const controller = new AbortController();
      const body = await searchCatalog({
        query: "boy",
        kinds: ["game", "character", "line"],
        limit: 5,
        signal: controller.signal,
      });
      expect(body.query).toBe("boy");
      expect(body.agent_prompt).toBe("boy");
      expect(calls).toHaveLength(1);
      const url = new URL(calls[0].url ?? "");
      expect(url.pathname).toBe("/api/v1/search");
      expect(url.searchParams.get("q")).toBe("boy");
      expect(url.searchParams.get("kinds")).toBe("game,character,line");
      expect(url.searchParams.get("limit")).toBe("5");
      expect(calls[0].signal).toBe(controller.signal);
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("omits the kinds and limit parameters when the caller does not set them", async () => {
    const calls: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        calls.push(String(input));
        return searchResponse();
      }),
    );
    try {
      await searchCatalog();
      const url = new URL(calls[0] ?? "");
      // An empty query is the recents mode and the client omits "" members, so
      // a bare recents read sends no query string at all.
      expect(url.searchParams.get("q")).toBeNull();
      expect(url.searchParams.get("kinds")).toBeNull();
      expect(url.searchParams.get("limit")).toBeNull();
    } finally {
      vi.unstubAllGlobals();
    }
  });
});

/** A palette stand-in: the hook's query is the input's value. */
function HookHarness({ initial = "" }: { initial?: string }) {
  const [query, setQuery] = useState(initial);
  const state = useCatalogSearch(query);
  const label =
    state.error !== null
      ? `error ${state.error.detail}`
      : `${state.pending ? "pending" : "done"} ${state.data?.groups.length ?? 0} groups ` +
        `${state.data?.agent_prompt ?? ""}`;
  return (
    <div>
      <input
        data-testid="hook-input"
        value={query}
        onChange={(event) => setQuery(event.target.value)}
      />
      <p data-testid="hook-state">{label}</p>
    </div>
  );
}

describe("useCatalogSearch", () => {
  it("debounces, and the newest read wins", async () => {
    const answers: string[] = [];
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const q = new URL(String(input)).searchParams.get("q") ?? "";
        answers.push(q);
        return searchResponse({ query: q, agent_prompt: q, groups: [] });
      }),
    );
    try {
      render(<HookHarness initial="b" />);
      const input = screen.getByTestId("hook-input");

      // The initial read fires once the debounce elapses.
      await waitFor(() => expect(answers).toEqual(["b"]));
      // Two keystrokes inside one debounce window coalesce: one read for the
      // settled spelling, not one per keystroke.
      fireEvent.change(input, { target: { value: "bo" } });
      fireEvent.change(input, { target: { value: "boy" } });
      await waitFor(() => expect(answers).toEqual(["b", "boy"]));

      // A later keystroke fires its own read (debounced, not immediate).
      fireEvent.change(input, { target: { value: "boy!" } });
      expect(answers).toEqual(["b", "boy"]);
      await waitFor(() => expect(answers).toEqual(["b", "boy", "boy!"]));
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("keeps the previous answer on screen while a read is pending, and a superseded read's late answer never settles the state", async () => {
    let abResolve: (() => void) | undefined;
    const abGate = new Promise<void>((resolve) => {
      abResolve = resolve;
    });
    vi.stubGlobal(
      "fetch",
      vi.fn(async (input: RequestInfo | URL) => {
        const q = new URL(String(input)).searchParams.get("q") ?? "";
        if (q === "ab") await abGate; // the read that will be superseded
        return searchResponse({ query: q, agent_prompt: q });
      }),
    );
    try {
      render(<HookHarness initial="a" />);
      const input = screen.getByTestId("hook-input");
      const state = () => screen.getByTestId("hook-state").textContent ?? "";
      await waitFor(() => expect(state()).toContain("done 0 groups a"));
      // Keystroke "ab": its read is in flight, so the answer for "a" stays on
      // screen (stale-while-revalidate — no flash to empty). Let the debounce
      // elapse so the "ab" read really starts (and hangs on the gate).
      fireEvent.change(input, { target: { value: "ab" } });
      await waitFor(() => expect(state()).toContain("pending 0 groups a"));
      await new Promise((resolve) => setTimeout(resolve, 300));
      expect(state()).toContain("pending 0 groups a");
      // Keystroke "abc": the "ab" read is superseded (its controller aborts,
      // its run token goes stale) and the read for "abc" settles the state...
      fireEvent.change(input, { target: { value: "abc" } });
      await waitFor(() => expect(state()).toContain("done 0 groups abc"));
      // ...and the superseded "ab" read's late answer must be a quiet no-op.
      act(() => abResolve?.());
      await new Promise((resolve) => setTimeout(resolve, 10));
      expect(state()).toContain("done 0 groups abc");
    } finally {
      vi.unstubAllGlobals();
    }
  });

  it("surfaces an ApiError as the error state (not a throw)", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        const problem = {
          type: "urn:game-narrator:error:queue-unavailable",
          title: "Service Unavailable",
          status: 503,
          detail: "the job queue is not configured",
          code: "SUBSYSTEM_UNAVAILABLE",
          request_id: "req-1",
        };
        return {
          ok: false,
          status: 503,
          statusText: "Service Unavailable",
          headers: new Headers(),
          json: async () => problem,
          text: async () => JSON.stringify(problem),
        } as Response;
      }),
    );
    try {
      render(<HookHarness initial="x" />);
      await waitFor(() => expect(screen.getByTestId("hook-state").textContent).toContain("error"));
      expect(screen.getByTestId("hook-state").textContent).toContain("the job queue is not");

      // The same boundary the hook uses yields the ApiError the palette's
      // error card reads (code and request id for the server log line).
      let caught: unknown = null;
      try {
        await searchCatalog({ query: "x" });
      } catch (error) {
        caught = error;
      }
      expect(caught instanceof ApiError).toBe(true);
      if (caught instanceof ApiError) {
        expect(caught.code).toBe("SUBSYSTEM_UNAVAILABLE");
        expect(caught.requestId).toBe("req-1");
      }
    } finally {
      vi.unstubAllGlobals();
    }
  });
});
