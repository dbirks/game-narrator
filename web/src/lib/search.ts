import { useEffect, useRef, useState } from "react";

import { api, ApiError, type Schemas } from "../api/client";

/**
 * `src/lib/search.ts` — the ⌘K command palette's search client and the
 * Tab-handoff contract (docs/design/plum-peach UX-02; ADR-0098).
 *
 * One debounced call per keystroke: `GET /api/v1/search?q=&kinds=&limit=`.
 * The server owns the grouping, the role filtering and the scoring
 * (ADR-0098 "Decision"); this module only adds the palette's client
 * concerns — the debounce, the abort of superseded queries, and the
 * normalised-query handoff the Tab key (and the "Ask the agent: …" row)
 * hands to the agent dock.
 *
 * The handoff is a window CustomEvent, not an import of the dock: the
 * palette (plan item 2) and the dock (plan item 3) are built in parallel,
 * and the event is the seam both sides compile against. `agent_prompt` in
 * the response is the one source of truth for the string handed over —
 * the UI never re-derives it (ADR-0098 §7).
 */

/** The palette's result kinds, in the server's canonical display order. */
export const SEARCH_KINDS = ["game", "character", "line", "trace", "setting", "action"] as const;

export type SearchKind = (typeof SEARCH_KINDS)[number];
export type SearchResponse = Schemas["SearchResponse"];
export type SearchGroup = Schemas["SearchGroup"];
export type SearchResult = Schemas["SearchResult"];
export type SearchAction = Schemas["SearchAction"];

/** The group headers the palette paints (mono, uppercase, per design). */
export const KIND_LABELS: Record<SearchKind, string> = {
  game: "Games",
  character: "Characters",
  line: "Lines",
  trace: "Traces",
  setting: "Settings",
  action: "Actions",
};

export interface SearchCatalogOptions {
  /** The palette query; empty (or whitespace) is the recents mode. */
  query?: string;
  /** A subset of the six kinds; absent means all of them. */
  kinds?: SearchKind[];
  /** The per-kind ceiling (server default 5, ceiling 20). */
  limit?: number;
  signal?: AbortSignal;
}

/** One debounced palette read (ADR-0098). */
export function searchCatalog(options: SearchCatalogOptions = {}): Promise<SearchResponse> {
  const { query = "", kinds, limit, signal } = options;
  return api.get("/api/v1/search", {
    query: { q: query, kinds: kinds?.join(","), limit },
    signal,
  });
}

/**
 * The normalisation the Tab handoff applies: trimmed, internal whitespace
 * collapsed — the same rule the server applies to `q` (so the echoed
 * `agent_prompt` and the handed-over string share one spelling).
 */
export function normalizeAgentQuery(query: string): string {
  return query.replace(/\s+/g, " ").trim();
}

/** The window event name the palette's Tab key (and "Ask the agent: …" row) dispatches. */
export const AGENT_QUERY_EVENT = "gn:agent-query";

/** The event's detail: the normalised query, pre-filled into the dock input. */
export interface AgentQueryDetail {
  query: string;
}

/** Hand the normalised query to the agent dock (UX-02's Tab). */
export function emitAgentQuery(query: string): void {
  window.dispatchEvent(new CustomEvent<AgentQueryDetail>(AGENT_QUERY_EVENT, { detail: { query } }));
}

/** Subscribe to dock handoffs; returns the unsubscribe. */
export function subscribeAgentQuery(handler: (detail: AgentQueryDetail) => void): () => void {
  const listener = (event: Event) => {
    const detail = (event as CustomEvent<AgentQueryDetail>).detail;
    if (detail && typeof detail.query === "string") handler(detail);
  };
  window.addEventListener(AGENT_QUERY_EVENT, listener);
  return () => window.removeEventListener(AGENT_QUERY_EVENT, listener);
}

export interface CatalogSearchState {
  data: SearchResponse | null;
  error: ApiError | null;
  pending: boolean;
}

/** The debounce: a keystroke that is followed within 150 ms does not fire. */
const SEARCH_DEBOUNCE_MS = 150;

/**
 * The palette's search state: one debounced, abortable read per query
 * change. `retryKey` re-fires the same query (the error state's retry).
 * The previous answer stays on screen while a newer read is in flight
 * (stale-while-revalidate: a keystroke must not flash the results away),
 * and only the newest read may settle the state — a slow response for an
 * older keystroke, or its abort, lands as a quiet no-op.
 */
export function useCatalogSearch(query: string, retryKey = 0): CatalogSearchState {
  const [state, setState] = useState<CatalogSearchState>({
    data: null,
    error: null,
    pending: true,
  });
  // One token per query change: only the newest read may settle the state, so
  // a slow response for an older keystroke never overwrites a newer answer.
  const runIdRef = useRef(0);

  useEffect(() => {
    const runId = ++runIdRef.current;
    const controller = new AbortController();
    const trimmed = query.trim();
    // Keep the previous answer (stale) while the new read is in flight; the
    // first read has nothing to keep, so it reads as a plain loading state.
    setState((prev) => ({ data: prev.data, error: null, pending: true }));
    const timer = window.setTimeout(() => {
      if (runId !== runIdRef.current) return;
      searchCatalog({ query: trimmed, signal: controller.signal })
        .then((data) => {
          if (runId !== runIdRef.current) return; // a newer keystroke owns the UI now
          setState({ data, error: null, pending: false });
        })
        .catch((error: unknown) => {
          if (runId !== runIdRef.current || controller.signal.aborted) return;
          setState({
            data: null,
            error: error instanceof ApiError ? error : null,
            pending: false,
          });
        });
    }, SEARCH_DEBOUNCE_MS);
    return () => {
      window.clearTimeout(timer);
      controller.abort();
    };
  }, [query, retryKey]);

  return state;
}

/** The row shape executeSearchAction needs (a server row or the palette's
 *  client-side "Ask the agent" row both satisfy it). */
export interface SearchActionRow {
  id: string;
  title: string;
  action?: SearchAction;
}

/**
 * Execute one action row's API target (ADR-0098: the verb's route or API
 * call; the registry ships exactly one API target — the voice-regenerate
 * audition — so the palette's execution surface is this one shape).
 * Returns a human-readable line for the palette's result note.
 */
export async function executeSearchAction(
  row: SearchActionRow,
  groups: SearchGroup[],
  signal?: AbortSignal,
): Promise<string> {
  const action = row.action;
  if (action === undefined) {
    return "This row navigates; ↵ opened it instead.";
  }
  if (action.method === "POST" && action.path === "/api/v1/voices/auditions") {
    // The row's id is `regenerate-voice:<characterId>` (ADR-0098 §6); the
    // character's name is the roster row the same query already fetched.
    const characterId = row.id.slice(`regenerate-voice:`.length);
    const name =
      groups
        .find((group) => group.kind === "character")
        ?.items.find((item) => item.id === characterId)?.title ?? row.title;
    const body = {
      character: name,
      brief: `Regenerate ${name}'s voice from the ⌘K palette.`,
    };
    const response = await api.post("/api/v1/voices/auditions", { body, signal });
    const accepted = response as { job_id?: string };
    return `Queued · job ${accepted.job_id ?? "…"}`;
  }
  return `No palette execution for ${action.method} ${action.path} yet.`;
}
