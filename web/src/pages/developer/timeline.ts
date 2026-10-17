/**
 * `src/pages/developer/timeline.ts` — the pure reducer behind the developer
 * event timeline (docs/spec/09 "Developer dashboard layout").
 *
 * The rules it owns:
 *
 * - **Ordering.** Rows keep seq order; seq is the monotonic event sequence
 *   (docs/spec/01), so the timeline is ordered by it, not by arrival time.
 * - **Deduplication.** SSE reconnects replay from `Last-Event-ID`, and the
 *   polling fallback pages the same durable log; the same event_id arriving
 *   twice must not double a row.
 * - **The cap.** The window holds the last `WINDOW` (500) rows; older history
 *   is paged in on demand through `/dev/events`, and merging a page re-caps the
 *   window to the newest 500.
 * - **Pinning.** Selecting a row pins it: new events streaming in keep the
 *   selection selected (docs/spec/09 "Pin a particular event/frame despite
 *   stream advancement"), and a pin that the 500-row cap would evict is kept
 *   in `pinnedEvent` so the detail panel never points at a row the window no
 *   longer renders.
 */

import type { EventEnvelope } from "../../api/sessions";

/** The last 500 rows are the window (docs/spec/09: virtualization later). */
export const WINDOW = 500;

export interface TimelineState {
  /** The visible window: newest `WINDOW` rows, ascending seq. */
  rows: EventEnvelope[];
  /** Highest seq seen, the since-cursor for the next durable page. */
  maxSeq: number;
  /** Paged-in rows older than the window's oldest seq, ascending seq. */
  older: EventEnvelope[];
  /** The pinned row's event id, or null when nothing is selected. */
  pinnedId: string | null;
  /**
   * The pinned event itself: the window's copy while it is in the window, a
   * retained copy when the cap evicted it (the detail panel must keep working
   * while the timeline scrolls past the pin).
   */
  pinnedEvent: EventEnvelope | null;
  /** True once older history was requested; the pager disables when the log
   *  started earlier than the page returned (truncated === false at seq 0). */
  pagedToStart: boolean;
}

export type TimelineAction =
  | { type: "events"; events: EventEnvelope[] }
  | { type: "older"; events: EventEnvelope[]; atStart: boolean }
  | { type: "pin"; eventId: string | null }
  | { type: "reset" };

export function initialTimeline(): TimelineState {
  return {
    rows: [],
    maxSeq: 0,
    older: [],
    pinnedId: null,
    pinnedEvent: null,
    pagedToStart: false,
  };
}

/** One merge of durable events into the window: dedupe by event_id, order by
 *  seq, cap to the newest WINDOW rows, and keep the pin alive across both the
 *  merge and the cap. */
export function timelineReducer(state: TimelineState, action: TimelineAction): TimelineState {
  switch (action.type) {
    case "reset":
      return initialTimeline();

    case "events":
      return merge(state, action.events);

    case "older": {
      const merged = merge(state, action.events);
      return { ...merged, pagedToStart: merged.pagedToStart || action.atStart };
    }

    case "pin": {
      if (action.eventId === null) return { ...state, pinnedId: null, pinnedEvent: null };
      const found =
        state.rows.find((row) => row.event_id === action.eventId) ??
        state.older.find((row) => row.event_id === action.eventId) ??
        (state.pinnedEvent?.event_id === action.eventId ? state.pinnedEvent : null);
      if (!found) return state;
      return { ...state, pinnedId: found.event_id, pinnedEvent: found };
    }
  }
}

/** Merge `incoming` into the state's existing rows (dedupe, order, cap) and
 *  keep the pin alive across both the merge and the cap. */
function merge(state: TimelineState, incoming: EventEnvelope[]): TimelineState {
  const byId = new Map<string, EventEnvelope>();
  for (const row of state.older) byId.set(row.event_id, row);
  for (const row of state.rows) byId.set(row.event_id, row);
  for (const row of incoming) {
    // A replayed envelope carries the same event_id with the same payload; the
    // first copy seen is the one the window rendered, so it wins.
    if (!byId.has(row.event_id)) byId.set(row.event_id, row);
  }
  const all = [...byId.values()].sort((a, b) => a.seq - b.seq);
  const rows = all.slice(-WINDOW);
  const olderRows = all.slice(0, Math.max(0, all.length - WINDOW));
  const maxSeq = all.length > 0 ? all[all.length - 1].seq : state.maxSeq;
  const pinnedInWindow =
    state.pinnedId !== null && rows.some((row) => row.event_id === state.pinnedId);
  const pinnedInOlder =
    state.pinnedId !== null && olderRows.some((row) => row.event_id === state.pinnedId);
  // A pin survives stream advancement: the window copy when present, the
  // retained copy otherwise (the cap evicting the pinned row is the case
  // "pinning" exists for).
  const pinnedEvent =
    state.pinnedId === null
      ? null
      : pinnedInWindow
        ? (rows.find((row) => row.event_id === state.pinnedId) ?? state.pinnedEvent)
        : pinnedInOlder
          ? (olderRows.find((row) => row.event_id === state.pinnedId) ?? state.pinnedEvent)
          : state.pinnedEvent;

  return {
    rows,
    older: olderRows,
    maxSeq,
    pagedToStart: state.pagedToStart,
    pinnedId: state.pinnedId,
    pinnedEvent,
  };
}

/** The oldest seq the window still holds: the `since_seq` a "load older" page
 *  must not re-cover is everything at or above it. */
export function oldestWindowSeq(state: TimelineState): number {
  return state.rows.length > 0 ? state.rows[0].seq : state.maxSeq;
}

/** Rows the timeline renders: the paged-in older history plus the window. */
export function renderedRows(state: TimelineState): EventEnvelope[] {
  return [...state.older, ...state.rows];
}

/** The pinned row's capture time as milliseconds, for the frame's alt text. */
export function captureMsAt(event: EventEnvelope | null | undefined): number | null {
  if (!event) return null;
  const ns = (event.data as Record<string, unknown> | undefined)?.capture_timestamp_ns;
  if (typeof ns === "number" && Number.isFinite(ns)) return Math.round(ns / 1e6);
  const at = Date.parse(event.occurred_at);
  return Number.isNaN(at) ? null : at;
}
