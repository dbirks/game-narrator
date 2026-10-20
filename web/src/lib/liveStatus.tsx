import { createContext, useContext, useEffect, useMemo, useReducer, type ReactNode } from "react";

import { api, ApiError, type Schemas } from "../api/client";

/**
 * `src/lib/liveStatus.ts` — the global status bar's feed
 * (docs/design/plum-peach "Global chrome"; ADR-0100).
 *
 * The server renders the canonical bar string (`RenderStatus` in
 * internal/api/status.go) so every client shows the same text; this module
 * only carries it. One process-wide feed per page: the provider opens the
 * SSE stream (`GET /api/v1/status/stream`, viewer tier) and falls back to a
 * read of `GET /api/v1/status` when the stream cannot open (anonymous 401,
 * hub-less 503, a dead network). The bar renders the feed's `text`; it never
 * assembles its own variant (UX-07).
 *
 * The anonymous and unreachable states show the neutral placeholder below —
 * they are not a status rollup and must never read like one.
 */

export type StatusSnapshot = Schemas["Status"];

/** The snapshot read (docs/spec/08 "APIs"): a fresh assembly per request. */
export const STATUS_SNAPSHOT_PATH = "/api/v1/status";
/** The on-change push: SSE of status.changed frames with Last-Event-ID resume. */
export const STATUS_STREAM_PATH = "/api/v1/status/stream";

/** The poll cadence for the stream-less fallback (a viewer polling the bar
 *  must not trigger probes; the read is the cheap half by construction). */
export const STATUS_POLL_INTERVAL_MS = 5_000;

// TODO(rd-phase1): neutral placeholder for the anonymous/unreachable states.
// A signed-in viewer always gets the server-rendered string (ADR-0100); the
// placeholder stands in until then and must stay visibly neutral — never a
// fake "5 ok" (the rollup's "unknown" is the honesty the levels exist for).
export const STATUS_PLACEHOLDER = "· status ·";

/** The status frame's SSE event name (internal/api/status.go): the hub sends
 *  named events, so the stream must listen for this name — the browser's
 *  default onmessage only receives unnamed events.
 */
export const STATUS_EVENT_NAME = "status.changed";

/** The EventSource surface the feed needs; the seam lets tests script frames.
 *  `onstatus` receives the named `status.changed` frames; `onmessage` is kept
 *  for unnamed frames only.
 */
export interface EventSourceLike {
  readonly readyState: number;
  onmessage: ((event: MessageEvent) => void) | null;
  onstatus: ((event: MessageEvent) => void) | null;
  onerror: ((event: unknown) => void) | null;
  close(): void;
}

/** The real browser EventSource behind the seam: it forwards the named
 *  status.changed frames to onstatus and mirrors readyState/close.
 */
class BrowserStatusStream implements EventSourceLike {
  onmessage: ((event: MessageEvent) => void) | null = null;
  onstatus: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;
  #es: EventSource;

  constructor(url: string) {
    this.#es = new EventSource(url);
    this.#es.addEventListener(STATUS_EVENT_NAME, (event) => {
      if (this.onstatus !== null) this.onstatus(event as MessageEvent);
    });
    this.#es.onerror = (event) => {
      if (this.onerror !== null) this.onerror(event);
    };
  }

  get readyState(): number {
    return this.#es.readyState;
  }

  close(): void {
    this.#es.close();
  }
}

/** The feed's observable state. */
export interface LiveStatusState {
  /** The newest snapshot (from either source), or null. */
  snapshot: StatusSnapshot | null;
  /** The bar's text: the snapshot's canonical string, else the placeholder. */
  text: string;
  /** Where the newest frame came from; "none" before any frame arrived. */
  source: "sse" | "poll" | "none";
  /** A human-readable fault (kept for tests and devtooling; the bar keeps
   *  its last frame rather than rendering an error card mid-screen). */
  error: string | null;
}

const initialState: LiveStatusState = {
  snapshot: null,
  text: STATUS_PLACEHOLDER,
  source: "none",
  error: null,
};

type Action =
  | { type: "frame"; snapshot: StatusSnapshot; source: "sse" | "poll" }
  | { type: "fault"; message: string }
  | { type: "anonymous" };

function reducer(state: LiveStatusState, action: Action): LiveStatusState {
  switch (action.type) {
    case "frame":
      return {
        snapshot: action.snapshot,
        text: action.snapshot.status,
        source: action.source,
        error: null,
      };
    case "fault":
      // A fault never blanks a bar that already has a frame: the last frame
      // stays on screen and the fault is recorded, not displayed.
      return { ...state, error: action.message };
    case "anonymous":
      // A 401 on the fallback read is the anonymous state, not an error:
      // the bar shows the placeholder (the page shows the sign-in affordance).
      return { ...initialState, source: "none" };
  }
}

/** The wire shape of every frame (SSE data and GET body share it, ADR-0100):
 *  the stream carries event envelopes whose `data` holds the snapshot, and
 *  the snapshot read carries the bare snapshot — both must parse.
 */
export function parseStatusFrame(data: string): StatusSnapshot | null {
  let value: unknown;
  try {
    value = JSON.parse(data);
  } catch {
    return null;
  }
  if (typeof value !== "object" || value === null) return null;
  const asRecord = value as Record<string, unknown>;
  const candidate = (
    typeof asRecord.data === "object" && asRecord.data !== null ? asRecord.data : asRecord
  ) as Partial<StatusSnapshot> | null;
  return typeof candidate?.status === "string" ? (candidate as StatusSnapshot) : null;
}

/** The default snapshot read through the shared API client. */
export async function fetchStatusSnapshot(signal?: AbortSignal): Promise<StatusSnapshot> {
  return api
    .get(STATUS_SNAPSHOT_PATH, { signal })
    .then((body) => body as unknown as StatusSnapshot);
}

export interface StatusFeedOptions {
  /** The stream opener (default: `new EventSource(STATUS_STREAM_PATH)`). */
  eventSourceFactory?: (url: string) => EventSourceLike;
  /** The fallback snapshot read (default: the shared API client). */
  fetchSnapshot?: (signal?: AbortSignal) => Promise<StatusSnapshot>;
  /** The fallback poll cadence (default: STATUS_POLL_INTERVAL_MS). */
  pollIntervalMs?: number;
}

/**
 * Opens the feed and wires its updates into `dispatch`: the SSE stream first,
 * the polling read when the stream never delivers a frame or is refused.
 * Returns the teardown (closes the stream, stops the poll).
 *
 * Exactly one subscription per page: the provider mounts this once at the
 * app root, and the bar is the only consumer (plan.md Phase 0 item 5).
 */
export function openStatusFeed(
  dispatch: (action: Action) => void,
  options: StatusFeedOptions = {},
): () => void {
  const {
    eventSourceFactory = (url) => new BrowserStatusStream(url),
    fetchSnapshot = fetchStatusSnapshot,
    pollIntervalMs = STATUS_POLL_INTERVAL_MS,
  } = options;

  let source: EventSourceLike | null = null;
  let pollTimer: ReturnType<typeof setTimeout> | null = null;
  let snapshotAbort: AbortController | null = null;
  let gotFrame = false;
  let closing = false;

  const startPolling = () => {
    if (closing) return;
    const tick = async () => {
      if (closing) return;
      snapshotAbort?.abort();
      snapshotAbort = new AbortController();
      try {
        const snapshot = await fetchSnapshot(snapshotAbort.signal);
        if (!closing) {
          gotFrame = true;
          dispatch({ type: "frame", snapshot, source: "poll" });
        }
      } catch (error) {
        if (!closing) {
          if (error instanceof ApiError && error.status === 401) {
            dispatch({ type: "anonymous" });
          } else if (error instanceof ApiError) {
            dispatch({ type: "fault", message: `${error.status} ${error.detail}` });
          }
          // A network fault keeps the last frame (or placeholder) on the bar.
        }
      } finally {
        if (!closing) pollTimer = setTimeout(() => void tick(), pollIntervalMs);
      }
    };
    void tick();
  };

  const abortStream = () => {
    if (source === null) return;
    source.onmessage = null;
    source.onstatus = null;
    source.onerror = null;
    source.close();
    source = null;
  };

  try {
    source = eventSourceFactory(STATUS_STREAM_PATH);
    source.onstatus = (event: MessageEvent) => {
      const snapshot = parseStatusFrame(String((event as MessageEvent).data ?? ""));
      if (snapshot === null) return; // a malformed frame is dropped, not rendered
      gotFrame = true;
      dispatch({ type: "frame", snapshot, source: "sse" });
    };
    source.onerror = () => {
      // CLOSED: the browser refused the stream (401/403/404 — it will not
      // retry), or it died and gave up. A never-delivered stream (503
      // hub-less, network) also falls back: the GET read is the bar's honest
      // state rather than a bar that opens and never moves.
      if (source === null) return;
      if (source.readyState === 2 /* EventSource.CLOSED */ || !gotFrame) {
        abortStream();
        startPolling();
      }
      // A stream that already delivered frames keeps the browser's own
      // reconnect (Last-Event-ID replay makes a resumed stream whole); the
      // bar holds its last frame meanwhile.
    };
  } catch {
    // EventSource is unavailable (no network object at all): the poll is the
    // only feed, and its errors say so.
    startPolling();
  }

  return () => {
    closing = true;
    abortStream();
    if (pollTimer !== null) clearTimeout(pollTimer);
    snapshotAbort?.abort();
  };
}

interface LiveStatusContextValue extends LiveStatusState {
  dispatch: (action: Action) => void;
}

const LiveStatusContext = createContext<LiveStatusContextValue | null>(null);

export interface LiveStatusProviderProps {
  children: ReactNode;
  /** Seams for tests (openStatusFeed's options, applied once at mount). */
  feed?: StatusFeedOptions;
}

/** The app-root feed. Mount once (main.tsx): the one status subscription
 *  per page the redesign's acceptance pins. */
export function LiveStatusProvider({ children, feed }: LiveStatusProviderProps) {
  const [state, dispatch] = useReducer(reducer, initialState);

  useEffect(() => {
    return openStatusFeed(dispatch, feed);
    // The feed opens once per mount; the seams are test-only and stable.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  const value = useMemo(() => ({ ...state, dispatch }), [state]);
  return <LiveStatusContext.Provider value={value}>{children}</LiveStatusContext.Provider>;
}

export function useLiveStatus(): LiveStatusState {
  const value = useContext(LiveStatusContext);
  if (value === null) throw new Error("useLiveStatus must be used inside LiveStatusProvider");
  return {
    snapshot: value.snapshot,
    text: value.text,
    source: value.source,
    error: value.error,
  };
}
