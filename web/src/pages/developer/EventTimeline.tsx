/**
 * `src/pages/developer/EventTimeline.tsx` — the time-sequenced event table of
 * the developer dashboard (docs/spec/09 "Developer dashboard layout").
 *
 * Rows are keyboard-selectable (tab to a row, Enter or Space pins it); the
 * pinned row keeps `aria-selected` while the stream advances, and "Load older"
 * pages the durable log through `/dev/events` from seq 0 so history older than
 * the 500-row window is reachable without virtualization (docs/spec/09: it is
 * not needed yet at this event rate).
 */

import { useState } from "react";

import { devApi, type EventEnvelope } from "../../api/sessions";
import { Loading } from "../../components/states";
import { renderedRows, type TimelineState } from "./timeline";

interface EventTimelineProps {
  state: TimelineState;
  /** The session whose durable log "Load older" pages, if one is tracked. */
  sessionId: string | null;
  onPin: (eventId: string | null) => void;
  /** Hand a paged-in older page to the reducer (the pin survives the merge). */
  onOlder: (events: EventEnvelope[], atStart: boolean) => void;
}

export function EventTimeline({ state, sessionId, onPin, onOlder }: EventTimelineProps) {
  const [loadingOlder, setLoadingOlder] = useState(false);
  const [olderError, setOlderError] = useState<unknown>(null);
  const rows = renderedRows(state);

  const loadOlder = async () => {
    if (!sessionId || state.pagedToStart) return;
    setLoadingOlder(true);
    setOlderError(null);
    try {
      // One page of 1000 from the log's start: the window keeps its newest 500
      // and whatever fell out of it becomes the paged-in older history.
      const page = await devApi.events({ sessionId, sinceSeq: 0, limit: 1000 });
      // atStart: the page's first row is the log's first seq.
      onOlder(page.events, page.events.length > 0 && page.events[0].seq === 1);
    } catch (error) {
      setOlderError(error);
    } finally {
      setLoadingOlder(false);
    }
  };

  return (
    <div className="panel" data-testid="event-timeline">
      <div className="row spread">
        <h2>
          Events <span className="muted">({rows.length})</span>
        </h2>
        <button
          type="button"
          data-testid="load-older"
          onClick={() => void loadOlder()}
          disabled={!sessionId || state.pagedToStart || loadingOlder}
        >
          {loadingOlder ? "Loading…" : "Load older"}
        </button>
      </div>
      {olderError ? (
        <p className="muted" role="alert" data-testid="older-error">
          Loading older events failed:{" "}
          {olderError instanceof Error ? olderError.message : String(olderError)}
        </p>
      ) : null}
      {rows.length === 0 ? (
        <Loading label={sessionId ? "Waiting for the session's events…" : "No events yet"} />
      ) : (
        <div className="table-scroll">
          <table data-testid="event-table">
            <thead>
              <tr>
                <th scope="col">Seq</th>
                <th scope="col">Capture</th>
                <th scope="col">Event</th>
                <th scope="col">Occurrence</th>
                <th scope="col">Trace</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((event) => (
                <TimelineRow
                  key={event.event_id}
                  event={event}
                  pinned={state.pinnedId === event.event_id}
                  onPin={() => onPin(state.pinnedId === event.event_id ? null : event.event_id)}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

function TimelineRow({
  event,
  pinned,
  onPin,
}: {
  event: EventEnvelope;
  pinned: boolean;
  onPin: () => void;
}) {
  const data = (event.data ?? {}) as Record<string, unknown>;
  const occurrence =
    typeof data.dialogue_occurrence_id === "string" ? data.dialogue_occurrence_id : "—";
  const select = () => onPin();
  return (
    <tr
      data-testid={`event-row-${event.seq}`}
      tabIndex={0}
      aria-selected={pinned}
      className={pinned ? "pinned" : undefined}
      onClick={select}
      onKeyDown={(key) => {
        if (key.key === "Enter" || key.key === " ") {
          key.preventDefault();
          select();
        }
      }}
    >
      <td>{event.seq}</td>
      <td>{formatCapture(event)}</td>
      <td data-testid={`event-name-${event.seq}`}>{event.name}</td>
      <td title={occurrence}>{truncate(occurrence, 18)}</td>
      <td title={event.trace_id ?? undefined}>
        {event.trace_id ? truncate(event.trace_id, 14) : "—"}
      </td>
    </tr>
  );
}

/** The capture time: the observation's source-clock timestamp when the payload
 *  carries one, else the envelope's occurred_at. */
function formatCapture(event: EventEnvelope): string {
  const data = (event.data ?? {}) as Record<string, unknown>;
  const ns = data.capture_timestamp_ns;
  if (typeof ns === "number" && Number.isFinite(ns)) {
    return `${(ns / 1e6).toFixed(0)} ms (source clock)`;
  }
  const date = new Date(event.occurred_at);
  if (Number.isNaN(date.getTime())) return "—";
  const hh = String(date.getUTCHours()).padStart(2, "0");
  const mm = String(date.getUTCMinutes()).padStart(2, "0");
  const ss = String(date.getUTCSeconds()).padStart(2, "0");
  return `${hh}:${mm}:${ss}.${String(date.getUTCMilliseconds()).padStart(3, "0")}`;
}

function truncate(text: string, width: number): string {
  return text.length > width ? `${text.slice(0, width - 1)}…` : text;
}
