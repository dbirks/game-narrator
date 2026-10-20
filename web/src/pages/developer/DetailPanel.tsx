/**
 * `src/pages/developer/DetailPanel.tsx` — what docs/spec/09's dashboard shows
 * when an event row is selected: the stored frame with the ROI rectangle drawn
 * over it, the ROI crop, the raw and normalised OCR text with its confidence,
 * the ranked candidates of the same trace with their component scores, and the
 * `message.committed` outcome.
 *
 * Two deliberate choices:
 *
 * - The ROI overlay is a CSS-positioned box over the `<img>` (a style
 *  attribute), not canvas drawing: the served bundle's CSP allows
 *  `style-src-attr 'unsafe-inline'` and nothing else, so there is no script
 *  injection path in a canvas either.
 * - Every string from the pipeline (game text, ids, reasons) renders as React
 *  text children; images only from `/dev/assets/{sha256}`, which the server
 *  answers `no-store` (docs/spec/09 "Privacy controls").
 */

import { useEffect, useMemo, useState } from "react";

import { api } from "../../api/client";
import { devApi, type EventEnvelope, type Session } from "../../api/sessions";
import { EmptyState, Loading } from "../../components/states";
import { TraceActions } from "./ActionPanel";
import { captureMsAt } from "./timeline";
import {
  activeWindowFromPayload,
  cropBasisLabel,
  regionThroughWindow,
  windowOutline,
} from "./window";
import {
  chainStage,
  decisionFacts,
  lastDecision,
  requestSourceLabel,
  type DecisionFacts,
} from "./decision";

/** The region DTO as `/profiles/{id}/regions` renders it (rect is normalised). */
interface RegionRow {
  id: string;
  label: string;
  region_type: string;
  rect_x: number;
  rect_y: number;
  rect_w: number;
  rect_h: number;
}

/** A message row as `/profiles/{id}/messages` renders it. */
interface MessageRow {
  id: string;
  source_message_id?: string;
}

/** Resolve a profile's row ids to their friendly source ids (the seed's names
 *  travel as source_message_id, not as row ids). The event payloads carry the
 *  ULID row ids; the inspector shows the human-readable name when it knows it,
 *  and falls back to the raw id otherwise. `null` while loading / on error. */
function useMessageNames(profileId: string | undefined): Record<string, string> | null {
  const [names, setNames] = useState<Record<string, string> | null>(null);
  useEffect(() => {
    if (!profileId) {
      setNames(null);
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const body = (await api.get("/api/v1/profiles/{id}/messages", {
          params: { id: profileId },
          query: { limit: 200 },
        })) as unknown as { messages: MessageRow[] };
        const map: Record<string, string> = {};
        for (const row of body.messages ?? []) {
          if (row.source_message_id) map[row.id] = row.source_message_id;
        }
        if (!cancelled) setNames(map);
      } catch {
        if (!cancelled) setNames({});
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [profileId]);
  return names;
}

/** The friendly name for a row id, or the raw id when it is unknown. */
function displayName(names: Record<string, string> | null, id: string): string {
  return names?.[id] ?? id;
}

export function DetailPanel({
  pinned,
  session,
}: {
  pinned: EventEnvelope | null;
  session: Session | null;
}) {
  const isDetailEvent =
    pinned !== null && (pinned.name === "text.observed" || pinned.name.startsWith("message."));

  if (!pinned) {
    return (
      <div className="panel" data-testid="detail-panel">
        <h2>Event detail</h2>
        <EmptyState title="Nothing selected">
          <p className="muted">
            Select a row in the event table to pin its frame, OCR and decision.
          </p>
        </EmptyState>
      </div>
    );
  }
  if (!isDetailEvent) {
    return (
      <div className="panel" data-testid="detail-panel">
        <h2>
          Event {pinned.name} (seq {pinned.seq})
        </h2>
        <p className="muted">
          Frame, OCR and decision detail is shown for text.observed and message.* events.
        </p>
        <Pre data={pinned.data} testId="detail-raw" />
        <TraceView traceId={pinned.trace_id} />
      </div>
    );
  }

  return (
    <div className="panel" data-testid="detail-panel">
      <h2>
        {pinned.name} <span className="muted">(seq {pinned.seq})</span>
      </h2>
      <FrameAndCrop pinned={pinned} session={session} />
      <OcrTexts pinned={pinned} />
      {pinned.trace_id ? (
        <TraceDetail pinned={pinned} session={session} />
      ) : (
        <p className="muted">This event carries no trace id.</p>
      )}
    </div>
  );
}

/** The stored frame with its ROI box and the crop beside it. */
function FrameAndCrop({ pinned, session }: { pinned: EventEnvelope; session: Session | null }) {
  const data = payload(pinned);
  const frameSha = str(data.frame_asset);
  const cropSha = str(data.crop_asset);
  const regionId = str(data.region_id);
  const captureMs = captureMsAt(pinned);
  const [regions, setRegions] = useState<RegionRow[] | null>(null);

  // The region rect comes from the running profile's regions table (normalised
  // coordinates), so the overlay tracks the same box the pipeline cropped.
  useEffect(() => {
    if (!session) {
      setRegions([]);
      return;
    }
    let cancelled = false;
    void (async () => {
      try {
        const body = (await api.get("/api/v1/profiles/{id}/regions", {
          params: { id: session.profile_id },
        })) as unknown as { regions: RegionRow[] };
        if (!cancelled) setRegions(body.regions ?? []);
      } catch {
        if (!cancelled) setRegions([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [session?.profile_id]);

  const region =
    regions === null
      ? null
      : regionId
        ? (regions.find((row) => row.id === regionId) ?? regions[0])
        : regions[0];

  // The crop basis the session's crop mapped the region through when the
  // sample was taken (ADR-0082 amendment 1): the tracked active window, in
  // fractions of the frame, or the whole frame until it settled. The overlay
  // is the region rect mapped through that window — the box the pipeline
  // actually cut — and the window itself gets its own outline when it is not
  // the whole frame, so a mis-settled window is visible, not guessed at.
  const win = activeWindowFromPayload(data);
  const roiBox = regionThroughWindow(region, win);
  const winBox = windowOutline(win);

  const alt =
    captureMs === null
      ? "Captured frame with dialogue region outlined"
      : `Captured frame at ${captureMs} ms with dialogue region outlined`;

  return (
    <div className="frame-crop" data-testid="detail-frame">
      {frameSha ? (
        <div className="frame-wrap">
          <img data-testid="frame-image" src={devApi.assetUrl(frameSha)} alt={alt} width={320} />
          {winBox ? (
            <div
              className="roi-window"
              data-testid="roi-window-box"
              style={{
                left: `${winBox.left}%`,
                top: `${winBox.top}%`,
                width: `${winBox.width}%`,
                height: `${winBox.height}%`,
              }}
            />
          ) : null}
          {roiBox ? (
            <div
              className="roi"
              data-testid="roi-box"
              style={{
                left: `${roiBox.left}%`,
                top: `${roiBox.top}%`,
                width: `${roiBox.width}%`,
                height: `${roiBox.height}%`,
              }}
            />
          ) : null}
        </div>
      ) : (
        <p className="muted" data-testid="frame-missing">
          No stored frame for this observation (dev trace off, or the sample was evicted).
        </p>
      )}
      <p className="muted" data-testid="crop-basis">
        {cropBasisLabel(win)}
      </p>
      {cropSha ? (
        <img
          data-testid="crop-image"
          src={devApi.assetUrl(cropSha)}
          alt={
            captureMs === null
              ? "ROI crop sent to the OCR engine"
              : `ROI crop at ${captureMs} ms sent to the OCR engine`
          }
          width={320}
        />
      ) : (
        <p className="muted" data-testid="crop-missing">
          No stored crop for this observation.
        </p>
      )}
    </div>
  );
}

/** Raw text, normalised text, and the OCR confidence the engine reported. */
function OcrTexts({ pinned }: { pinned: EventEnvelope }) {
  const data = payload(pinned);
  const raw = str(data.raw_text);
  const normalized = str(data.normalized_text) || str(data.normalized_key);
  const confidence = num(data.confidence);
  return (
    <dl className="stats" data-testid="detail-ocr">
      <dt>Raw OCR text</dt>
      <dd data-testid="ocr-raw">{raw || "—"}</dd>
      <dt>Normalised text</dt>
      <dd data-testid="ocr-normalized">{normalized || "—"}</dd>
      <dt>OCR confidence</dt>
      <dd data-testid="ocr-confidence">{confidence === null ? "—" : confidence.toFixed(3)}</dd>
    </dl>
  );
}

/** The decision half of a pinned event: the trace's candidates and commit. */
function TraceDetail({ pinned, session }: { pinned: EventEnvelope; session: Session | null }) {
  const names = useMessageNames(session?.profile_id);
  const [page, setPage] = useState<DevTracePage | null>(null);
  const [error, setError] = useState<unknown>(null);

  useEffect(() => {
    let cancelled = false;
    setPage(null);
    setError(null);
    void devApi
      .trace(pinned.trace_id as string)
      .then((result) => {
        if (!cancelled) setPage(result);
      })
      .catch((caught) => {
        if (!cancelled) setError(caught);
      });
    return () => {
      cancelled = true;
    };
  }, [pinned.trace_id, pinned.event_id]);

  if (error) {
    return (
      <p role="alert" className="muted" data-testid="trace-error">
        Loading the trace failed: {error instanceof Error ? error.message : String(error)}
      </p>
    );
  }
  if (!page) return <Loading label="Loading the trace…" />;

  // Correlate the ranking and the commitment by trace: page.events is exactly
  // the pinned event's trace, and traces are per dialogue occurrence (a page
  // advance mints a new trace), so this trace's candidates and its commitment
  // are this occurrence's — no text-matching across occurrences, which was how
  // the panel could show one page's commit under another page's observation.
  // The pinned event's own candidates/commit win when it is one; within the
  // trace the newest wins, and a trace may have no commit yet (the pinned
  // observation can be any frame of a reveal that has not committed), in which
  // case the commitment section says so rather than borrowing one from
  // elsewhere.
  const candidatesEvent =
    (pinned.name === "message.candidates" && hasCandidates(pinned) ? pinned : null) ??
    lastEvent(page.events, (event) => hasCandidates(event));
  const decisionEvent =
    (pinned.name === "decision.resolved" ? pinned : null) ?? lastDecision(page.events);
  const committed =
    (pinned.name === "message.committed" ? pinned : null) ??
    lastEvent(page.events, (event) => event.name === "message.committed");

  return (
    <div>
      {candidatesEvent ? (
        <CandidatesTable event={candidatesEvent} names={names} />
      ) : (
        <p className="muted">No ranking in this trace.</p>
      )}
      {decisionEvent ? (
        <DecisionBlock event={decisionEvent} />
      ) : (
        <p className="muted" data-testid="decision-pending">
          No Clef decision in this trace (the ranking was unambiguous, or the decision role did not
          engage).
        </p>
      )}
      {committed ? (
        <CommitOutcome event={committed} names={names} />
      ) : (
        <p className="muted" data-testid="commit-pending">
          No commit in this trace yet.
        </p>
      )}
      <TraceActions traceId={pinned.trace_id as string} session={session} events={page.events} />
      <TraceView traceId={pinned.trace_id as string} page={page} />
    </div>
  );
}

/** The Clef decision row (docs/spec/13 "did it speak, and why not"): the
 *  provider and model that answered, the request id the provider logs can be
 *  searched by and which side issued it, the call's latency, the answer the
 *  fences produced (accepted / abstain / late-discarded), and the generation
 *  it was committed under — or, when the model could not be asked, the
 *  degraded local commit with the reason the wiring recorded. */
function DecisionBlock({ event }: { event: EventEnvelope }) {
  const facts = decisionFacts(event);
  if (!facts) return null;
  return <DecisionFactsView facts={facts} event={event} />;
}

function DecisionFactsView({ facts, event }: { facts: DecisionFacts; event: EventEnvelope }) {
  return (
    <div data-testid="decision-block" className={facts.degraded ? "degraded" : undefined}>
      <h3>
        Clef decision{" "}
        <span className={facts.degraded ? "muted" : undefined} data-testid="decision-outcome">
          {facts.outcome}
        </span>
      </h3>
      <dl className="stats" data-testid="decision-stats">
        <dt>Provider / model</dt>
        <dd data-testid="decision-provider-model">
          {facts.provider ?? "—"}
          {facts.model ? ` · ${facts.model}` : ""}
        </dd>
        <dt>Request id</dt>
        <dd data-testid="decision-request-id">
          {facts.requestId ? (
            <span>
              <code>{facts.requestId}</code>{" "}
              <span className="muted" data-testid="decision-request-source">
                ({requestSourceLabel(facts.requestIdSource)})
              </span>
            </span>
          ) : (
            "— (no provider call)"
          )}
        </dd>
        <dt>Latency</dt>
        <dd data-testid="decision-latency">
          {facts.latencyMs === null ? "—" : `${facts.latencyMs} ms`}
        </dd>
        <dt>Answer</dt>
        <dd data-testid="decision-answer">
          {facts.outcome}
          {facts.choice && facts.choice !== "none" ? ` (candidate ${facts.choice})` : ""}
        </dd>
        <dt>Generation applied</dt>
        <dd data-testid="decision-generation">
          {facts.generation === null ? "—" : `#${facts.generation}`}
          {facts.observationSeq !== null ? ` · observation seq ${facts.observationSeq}` : ""}
        </dd>
      </dl>
      <p className="muted">
        Event seq {event.seq} · causation {event.causation_id ?? "(origin)"}
      </p>
    </div>
  );
}

/** The ranked candidates: each component score as a small bar, plus the
 *  combined probability, the margin and the matcher's reason code. */
function CandidatesTable({
  event,
  names,
}: {
  event: EventEnvelope;
  names: Record<string, string> | null;
}) {
  const data = payload(event);
  const candidates = arr(data.candidates);
  const margin = num(data.margin);
  const reason = str(data.reason);
  const abstain = data.abstain === true;
  return (
    <div data-testid="candidates">
      <h3>
        Candidates{" "}
        <span className="muted">
          {reason ? `reason ${reason}` : ""}
          {reason && margin !== null ? ` · ` : ""}
          {margin !== null ? `margin ${margin.toFixed(4)}` : ""}
          {abstain ? " · abstained" : ""}
        </span>
      </h3>
      <table data-testid="candidates-table">
        <thead>
          <tr>
            <th scope="col">#</th>
            <th scope="col">Message</th>
            <th scope="col">Probability</th>
            <th scope="col">Components</th>
          </tr>
        </thead>
        <tbody>
          {candidates.map((candidate, index) => {
            const record = (candidate ?? {}) as Record<string, unknown>;
            const rawMessage = str(record.message_id) ?? "";
            const message = displayName(names, rawMessage);
            const probability = num(record.probability) ?? num(record.score);
            const components =
              record.components && typeof record.components === "object"
                ? Object.entries(record.components as Record<string, unknown>)
                : [];
            return (
              <tr key={`${rawMessage}-${index}`} data-testid={`candidate-row-${index}`}>
                <td>{index + 1}</td>
                <td data-testid={`candidate-id-${index}`}>{message || "unknown"}</td>
                <td>
                  <span className="bar">
                    <span
                      className="bar-fill"
                      style={{ width: `${barWidth(probability ?? 0)}%` }}
                    />
                  </span>{" "}
                  {probability === null ? "—" : probability.toFixed(4)}
                </td>
                <td>
                  {components.map(([name, value]) => {
                    const componentValue = num(value);
                    return (
                      <span className="component" key={name} data-testid={`component-${name}`}>
                        <span className="component-name">{name}</span>
                        <span className="bar small">
                          <span
                            className="bar-fill"
                            style={{ width: `${barWidth(componentValue ?? 0)}%` }}
                          />
                        </span>{" "}
                        {componentValue === null ? "—" : componentValue.toFixed(3)}
                      </span>
                    );
                  })}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** The commitment's outcome: which message, whether speech may announce it. */
function CommitOutcome({
  event,
  names,
}: {
  event: EventEnvelope;
  names: Record<string, string> | null;
}) {
  const data = payload(event);
  const rawMessage = str(data.message_id) ?? "";
  const message = rawMessage === "" ? "unknown" : displayName(names, rawMessage);
  const eligible = data.speech_eligible === true;
  const reason = str(data.reason);
  // The corpus's content class for the committed line (ADR-0083): the session
  // reads it off the candidate index at commit time; "" for an abstain
  // commit, whose message the corpus could not name.
  const textKind = str(data.text_kind);
  return (
    <div data-testid="commit-outcome">
      <h3>
        Committed <span className="muted">{message}</span>
      </h3>
      <dl className="stats">
        <dt>Message id</dt>
        <dd data-testid="commit-message">{message}</dd>
        <dt>Text kind</dt>
        <dd data-testid="commit-text-kind">{textKind || "—"}</dd>
        <dt>Speech eligible</dt>
        <dd data-testid="commit-speech-eligible">{eligible ? "yes" : "no"}</dd>
        <dt>Reason</dt>
        <dd data-testid="commit-reason">{reason || "—"}</dd>
      </dl>
    </div>
  );
}

/** "Open trace": the trace's events in seq order with their causation links. */
function TraceView({ traceId, page }: { traceId?: string; page?: DevTracePage }) {
  const [fetched, setFetched] = useState<DevTracePage | null>(null);
  useEffect(() => {
    if (page || !traceId) return;
    let cancelled = false;
    void devApi.trace(traceId).then((result) => {
      if (!cancelled) setFetched(result);
    });
    return () => {
      cancelled = true;
    };
  }, [traceId, page]);

  const rows = page?.events ?? fetched?.events;
  if (!traceId) return null;
  if (!rows) return <Loading label="Loading the trace…" />;
  return (
    <div data-testid="trace-view">
      <h3>
        Trace <code>{traceId}</code> <span className="muted">({rows.length} events)</span>
      </h3>
      <ol className="trace-list" data-testid="trace-events">
        {rows.map((event) => {
          const causation = event.causation_id;
          const decision = event.name === "decision.resolved" ? decisionFacts(event) : null;
          return (
            <li key={event.event_id} data-testid={`trace-event-${event.seq}`}>
              <code>{event.seq}</code> <span className="muted">[{chainStage(event.name)}]</span>{" "}
              {event.name}
              {decision ? (
                <span className="muted">
                  {" · req "}
                  {decision.requestId ? (
                    <code data-testid="trace-decision-request">{decision.requestId}</code>
                  ) : (
                    "(no provider call)"
                  )}
                  {" · "}
                  {decision.outcome}
                  {decision.latencyMs !== null ? ` · ${decision.latencyMs} ms` : ""}
                </span>
              ) : null}
              {causation ? (
                <span className="muted"> caused by {causation}</span>
              ) : (
                <span className="muted"> (origin)</span>
              )}
            </li>
          );
        })}
      </ol>
    </div>
  );
}

type DevTracePage = { events: EventEnvelope[]; next_seq: number; truncated: boolean };

function payload(event: EventEnvelope): Record<string, unknown> {
  const data = event.data;
  return data && typeof data === "object" ? (data as Record<string, unknown>) : {};
}

function str(value: unknown): string | null {
  return typeof value === "string" && value !== "" ? value : null;
}

function num(value: unknown): number | null {
  return typeof value === "number" && Number.isFinite(value) ? value : null;
}

function arr(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

/** Bar width for a value in [0,1]; clamped so an out-of-band score cannot
 *  render a 400%-wide bar. */
function barWidth(value: number): number {
  return Math.max(0, Math.min(100, value * 100));
}

function lastEvent(
  events: EventEnvelope[],
  keep: (event: EventEnvelope) => boolean,
): EventEnvelope | null {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event && keep(event)) return event;
  }
  return null;
}

function hasCandidates(event: EventEnvelope): boolean {
  return event.name === "message.candidates" && arr(payload(event).candidates).length > 0;
}

/** The envelope's data as preformatted text for events without a dedicated view. */
function Pre({ data, testId }: { data: unknown; testId: string }) {
  const text = useMemo(() => JSON.stringify(data, null, 2) ?? "null", [data]);
  return (
    <pre data-testid={testId} className="muted">
      {text}
    </pre>
  );
}
