/**
 * `src/api/sessions.ts` — the session runtime and developer-inspector half of
 * the API (ADR-0024): start/stop a session, read its state and pipeline
 * counters, page the durable developer event log, walk one trace, and serve the
 * stored frame/crop samples.
 *
 * Two things live here that do not fit the generic `api` client:
 *
 * - **SSE.** The event stream is `GET /api/v1/events?session_id=…` and every
 *   frame names its event (`event: <name>`), so the browser's `EventSource`
 *   default `message` channel never fires and a hand-rolled fetch stream is the
 *   honest way to read it. The reader tracks the last `id:` so a reconnect
 *   sends `Last-Event-ID` and the hub resumes the durable half rather than
 *   replaying from the start (docs/spec/08 "Event stream reliability").
 * - **The polling fallback.** SSE is the live path; the /dev/events since-cursor
 *   page and the session snapshot are the degraded path, and the page chooses
 *   which is driving the timeline.
 *
 * Types come from the generated contract (`schema.d.ts`); nothing here
 * re-interprets event payloads — the developer page renders them as text
 * (docs/spec/14), and images only from `/dev/assets/{sha256}`.
 */

import { api } from "./client";
import type { Schemas } from "./client";

export type Session = Schemas["Session"];
export type SessionState = Schemas["SessionState"];
export type PipelineStats = Schemas["PipelineStats"];
export type EventEnvelope = Schemas["EventEnvelope"];
export type DevEventsPage = Schemas["DevEventsPage"];
export type FrameSource = Schemas["FrameSource"];

export interface StartSessionBody {
  profile_id: string;
  frame_source: FrameSource;
  mode?: Schemas["SessionMode"];
  /** The contract types the toggle as required (default false server-side). */
  dev_trace: boolean;
}

export const sessionsApi = {
  start(body: StartSessionBody): Promise<Session> {
    return api.post("/api/v1/sessions", { body }) as Promise<Session>;
  },
  get(id: string, signal?: AbortSignal): Promise<Session> {
    return api.get("/api/v1/sessions/{id}", { params: { id }, signal }) as Promise<Session>;
  },
  /** Idempotent: answering the terminal snapshot for a finished run is a 200. */
  stop(id: string): Promise<Session> {
    return api.post("/api/v1/sessions/{id}/stop", { params: { id }, body: {} }) as Promise<Session>;
  },
};

/** The developer evidence surfaces (developer role; ADR-0024). */
export const devApi = {
  /** Page the durable event log. `sinceSeq` is the first seq to include. */
  events(
    query: { sessionId?: string; sinceSeq?: number; limit?: number },
    signal?: AbortSignal,
  ): Promise<DevEventsPage> {
    return api.get("/api/v1/dev/events", {
      query: {
        session_id: query.sessionId,
        since_seq: query.sinceSeq,
        limit: query.limit,
      },
      signal,
    }) as Promise<DevEventsPage>;
  },
  /** Every persisted event carrying one trace id, in seq order. */
  trace(traceId: string, signal?: AbortSignal): Promise<DevEventsPage> {
    return api.get("/api/v1/dev/traces/{trace_id}", {
      params: { trace_id: traceId },
      signal,
    }) as Promise<DevEventsPage>;
  },
  /** The only read path into a session's private dev-trace store (no-store). */
  assetUrl(sha256: string): string {
    return `/api/v1/dev/assets/${encodeURIComponent(sha256)}`;
  },
};

// ── SSE ──────────────────────────────────────────────────────────────────────

export interface EventStreamHandlers {
  onEvent: (envelope: EventEnvelope, seq: number) => void;
  /** The transport failed after every retry: the caller must switch to polling. */
  onGiveUp: (error: Error) => void;
}

/** How long before a broken stream is declared dead and the fallback takes over. */
const MAX_RETRIES = 4;
const RETRY_DELAY_MS = 500;

/**
 * Read the SSE event stream with fetch. `lastEventId` is tracked across
 * (re)connects and sent as `Last-Event-ID`, so a dropped wire resumes the
 * durable stream instead of replaying it; the hub answers a gap with
 * `events.resync_required` rather than silently skipping history.
 */
export function openEventStream(
  url: string,
  handlers: EventStreamHandlers,
  signal?: AbortSignal,
): () => void {
  let closed = false;
  let lastEventId = "0";
  let retries = 0;

  const connect = async () => {
    while (!closed && retries < MAX_RETRIES) {
      if (signal?.aborted) return;
      try {
        const headers: Record<string, string> = { accept: "text/event-stream" };
        if (Number(lastEventId) > 0) headers["last-event-id"] = lastEventId;
        const response = await fetch(url, {
          headers,
          credentials: "same-origin",
          signal,
        });
        if (!response.ok || !response.body) {
          throw new Error(`the event stream answered ${response.status}`);
        }
        retries = 0;
        await readFrames(response.body, (id, _name, data) => {
          if (id) lastEventId = id;
          if (!data) return;
          try {
            handlers.onEvent(JSON.parse(data) as EventEnvelope, Number(id) || 0);
          } catch {
            // A frame the client cannot decode is a gap, not a crash: the
            // hub's resync notice and the polling fallback both recover.
          }
        });
        // Clean end (hub closed). Retry if we were following a live session.
        if (!closed && !signal?.aborted && Number(lastEventId) > 0) continue;
        return;
      } catch (error) {
        if (closed || signal?.aborted) return;
        retries += 1;
        if (retries >= MAX_RETRIES) {
          handlers.onGiveUp(error instanceof Error ? error : new Error(String(error)));
          return;
        }
        await delay(RETRY_DELAY_MS * retries);
      }
    }
  };

  void connect();

  return () => {
    closed = true;
    signal?.removeEventListener("abort", noop);
  };
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

const noop = () => {};

/**
 * Parse a `text/event-stream` body into (id, name, data) frames. Data is the
 * joined `data:` lines of one frame; comments (`: …`) are the heartbeats the
 * server sends to keep proxies from closing idle streams.
 */
async function readFrames(
  body: ReadableStream<Uint8Array>,
  onFrame: (id: string, name: string, data: string | null) => void,
): Promise<void> {
  const reader = body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    let boundary = buffer.indexOf("\n\n");
    while (boundary >= 0) {
      const frame = buffer.slice(0, boundary);
      buffer = buffer.slice(boundary + 2);
      const [id, name, data] = parseSseFrame(frame);
      onFrame(id, name, data);
      boundary = buffer.indexOf("\n\n");
    }
  }
  if (buffer.trim()) {
    const [id, name, data] = parseSseFrame(buffer);
    onFrame(id, name, data);
  }
}

/** One blank-line-delimited SSE frame into its three fields. */
export function parseSseFrame(frame: string): [string, string, string | null] {
  let id = "";
  let name = "message";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith(":")) continue; // comment (heartbeat)
    const colon = line.indexOf(":");
    const field = colon < 0 ? line : line.slice(0, colon);
    let value = colon < 0 ? "" : line.slice(colon + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    switch (field) {
      case "id":
        id = value;
        break;
      case "event":
        name = value;
        break;
      case "data":
        dataLines.push(value);
        break;
    }
  }
  return [id, name, dataLines.length ? dataLines.join("\n") : null];
}
