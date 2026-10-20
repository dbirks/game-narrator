/**
 * `src/pages/developer/ActionPanel.tsx` — the inspector's per-event actions
 * (ADR-0083, docs/spec/09): the six correction labels, the trace's fixture
 * recording, the stored observation's re-run with its diff, the redacted
 * evidence download, and the utterance's waveform and player.
 *
 * The logic lives in actions.ts (pure, unit-tested); this component wires it
 * to the API and the DOM. Every result state renders: a pending action says
 * so, a failed one carries the error's detail, and the saved-fixture and
 * re-run results show the server's own answer (the directory the fixture
 * landed in; the re-run's winner, the diff and its honesty note).
 */

import { useCallback, useEffect, useRef, useState } from "react";

import { api, apiStatus, ApiError, type Schemas } from "../../api/client";
import { devApi, type EventEnvelope, type Session } from "../../api/sessions";
import {
  CORRECTION_LABELS,
  correctionForLabel,
  traceObservation,
  wavPeaks,
  type CorrectionLabel,
} from "./actions";

type CorrectionRow = Schemas["Correction"];

/** The correction labels for one occurrence: the six buttons, the labels the
 *  run has already recorded for this trace, and the result of the last write. */
function CorrectionLabels({
  profileId,
  observation,
  traceId,
}: {
  profileId: string;
  observation: ReturnType<typeof traceObservation>;
  traceId: string;
}) {
  const [labels, setLabels] = useState<CorrectionRow[] | null>(null);
  const [busyKey, setBusyKey] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [lastSaved, setLastSaved] = useState<string | null>(null);

  // The labels linked to this trace: the profile's corrections whose evidence
  // names it (the server's fixture applies the same rule).
  useEffect(() => {
    let cancelled = false;
    setLabels(null);
    void (async () => {
      try {
        // The list member is `items` (openapi.json CorrectionList, the same
        // shape GET /api/v1/dev/fixtures and the corrections-fixtures spec
        // assert), not `corrections`.
        const body = (await api.get("/api/v1/corrections", {
          query: { profile_id: profileId },
        })) as unknown as { items: CorrectionRow[] };
        const linked = (body.items ?? []).filter((row) =>
          (row.evidence ?? []).some((entry) => entry.includes(`trace:${traceId}`)),
        );
        if (!cancelled) setLabels(linked);
      } catch {
        if (!cancelled) setLabels([]);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [profileId, traceId]);

  const record = useCallback(
    async (label: CorrectionLabel) => {
      if (!observation || busyKey) return;
      setBusyKey(label.key);
      setError(null);
      try {
        const body = correctionForLabel(label, {
          profileId,
          traceId,
          messageId: observation.messageId,
          speakerId: observation.speakerId,
          occurrenceId: observation.occurrenceId,
          text: observation.rawText,
        });
        await api.post("/api/v1/corrections", { body });
        const refreshed = (await api.get("/api/v1/corrections", {
          query: { profile_id: profileId },
        })) as unknown as { items: CorrectionRow[] };
        setLabels(
          (refreshed.items ?? []).filter((row) =>
            (row.evidence ?? []).some((entry) => entry.includes(`trace:${traceId}`)),
          ),
        );
        setLastSaved(label.label);
      } catch (caught) {
        setError(caught instanceof ApiError ? `${caught.title}: ${caught.detail}` : String(caught));
      } finally {
        setBusyKey(null);
      }
    },
    [observation, profileId, traceId, busyKey],
  );

  return (
    <div data-testid="action-labels">
      <h3>
        Corrections <span className="muted">(labels only — the bench reads them back)</span>
      </h3>
      <p className="actions" data-testid="action-label-buttons">
        {CORRECTION_LABELS.map((label) => (
          <button
            key={label.key}
            data-testid={`action-label-${label.key}`}
            disabled={busyKey !== null || observation === null}
            onClick={() => void record(label)}
          >
            {label.label}
            {busyKey === label.key ? "…" : ""}
          </button>
        ))}
      </p>
      {error ? (
        <p role="alert" className="muted" data-testid="action-label-error">
          {error}
        </p>
      ) : null}
      {lastSaved && !error ? (
        <p className="muted" data-testid="action-label-saved">
          Recorded “{lastSaved}” on this occurrence.
        </p>
      ) : null}
      {labels !== null ? (
        <ul className="stack" data-testid="action-labels-recorded">
          {labels.length === 0 ? (
            <li className="muted" data-testid="action-labels-empty">
              No labels recorded on this occurrence yet.
            </li>
          ) : (
            labels.map((row) => (
              <li key={row.id} data-testid="action-label-recorded">
                <code>{row.kind}</code>{" "}
                <span className="muted">
                  ({row.target_type} {row.target_id}) · {row.status}
                </span>
                <br />
                <span className="muted">{row.reason || row.before_value}</span>
              </li>
            ))
          )}
        </ul>
      ) : (
        <p className="muted">Loading the recorded labels…</p>
      )}
    </div>
  );
}

/** The fixture, re-run and evidence actions for one trace. */
function RegressionActions({ traceId }: { traceId: string }) {
  const [fixture, setFixture] = useState<Schemas["DevFixture"] | null>(null);
  const [fixtureBusy, setFixtureBusy] = useState(false);
  const [fixtureError, setFixtureError] = useState<string | null>(null);

  const [rerun, setRerun] = useState<Schemas["DevRerunResponse"] | null>(null);
  const [rerunBusy, setRerunBusy] = useState(false);
  const [rerunError, setRerunError] = useState<string | null>(null);

  const [evidenceBusy, setEvidenceBusy] = useState(false);
  const [evidenceError, setEvidenceError] = useState<string | null>(null);

  const saveFixture = useCallback(async () => {
    setFixtureBusy(true);
    setFixtureError(null);
    try {
      const body = await devApi.recordFixture(traceId);
      setFixture(body.fixture);
    } catch (caught) {
      setFixtureError(explain(caught));
    } finally {
      setFixtureBusy(false);
    }
  }, [traceId]);

  const reRun = useCallback(async () => {
    setRerunBusy(true);
    setRerunError(null);
    try {
      setRerun(await devApi.rerunTrace(traceId));
    } catch (caught) {
      setRerunError(explain(caught));
    } finally {
      setRerunBusy(false);
    }
  }, [traceId]);

  const downloadEvidence = useCallback(async () => {
    setEvidenceBusy(true);
    setEvidenceError(null);
    try {
      // GET with the session cookie; the page drives the download from the
      // blob so the browser's download manager names the file.
      const response = await fetch(devApi.evidenceUrl(traceId), { credentials: "same-origin" });
      if (!response.ok) {
        let detail = `HTTP ${response.status}`;
        try {
          const problem = (await response.json()) as { detail?: string };
          if (problem.detail) detail = problem.detail;
        } catch {
          // a non-problem body: the status is the report.
        }
        throw new Error(detail);
      }
      const blob = await response.blob();
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = `trace-evidence-${traceId}.tar.gz`;
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
    } catch (caught) {
      setEvidenceError(caught instanceof Error ? caught.message : String(caught));
    } finally {
      setEvidenceBusy(false);
    }
  }, [traceId]);

  return (
    <div data-testid="action-regression">
      <h3>Regression</h3>
      <p className="actions">
        <button
          data-testid="action-fixture"
          disabled={fixtureBusy}
          onClick={() => void saveFixture()}
        >
          {fixtureBusy ? "Saving fixture…" : "Save regression fixture"}
        </button>
        <button data-testid="action-rerun" disabled={rerunBusy} onClick={() => void reRun()}>
          {rerunBusy ? "Re-running…" : "Re-run against current model"}
        </button>
        <button
          data-testid="action-evidence"
          disabled={evidenceBusy}
          onClick={() => void downloadEvidence()}
        >
          {evidenceBusy ? "Downloading…" : "Download evidence"}
        </button>
      </p>

      {fixture ? (
        <dl className="stats" data-testid="action-fixture-result">
          <dt>Fixture id</dt>
          <dd>
            <code>{fixture.id}</code>
          </dd>
          <dt>Location</dt>
          <dd data-testid="action-fixture-dir">{fixture.dir}</dd>
          <dt>Entries</dt>
          <dd data-testid="action-fixture-entries">{fixture.entries.length}</dd>
        </dl>
      ) : null}
      {fixtureError ? (
        <p role="alert" className="muted" data-testid="action-fixture-error">
          {fixtureError}
        </p>
      ) : null}

      {rerun ? (
        <div data-testid="action-rerun-result">
          <dl className="stats">
            <dt>Changed</dt>
            <dd data-testid="action-rerun-changed">{rerun.changed ? "yes" : "no"}</dd>
            <dt>Re-run winner</dt>
            <dd data-testid="action-rerun-winner">
              {rerun.current.candidates[0]?.message_id ?? "(none)"}
            </dd>
            <dt>Decision</dt>
            <dd data-testid="action-rerun-decision">{rerun.decision.note}</dd>
          </dl>
          {rerun.diff.length > 0 ? (
            <table data-testid="action-rerun-diff">
              <thead>
                <tr>
                  <th scope="col">Message</th>
                  <th scope="col">Before</th>
                  <th scope="col">After</th>
                  <th scope="col">Δ</th>
                </tr>
              </thead>
              <tbody>
                {rerun.diff.map((row) => (
                  <tr key={row.message_id} data-testid={`action-rerun-row-${row.message_id}`}>
                    <td>{row.message_id}</td>
                    <td>{rankCell(row.previous_rank, row.previous_probability)}</td>
                    <td>{rankCell(row.current_rank, row.current_probability)}</td>
                    <td>
                      {row.delta === null || row.delta === undefined ? "—" : row.delta.toFixed(4)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          ) : null}
        </div>
      ) : null}
      {rerunError ? (
        <p role="alert" className="muted" data-testid="action-rerun-error">
          {rerunError}
        </p>
      ) : null}
      {evidenceError ? (
        <p role="alert" className="muted" data-testid="action-evidence-error">
          {evidenceError}
        </p>
      ) : null}
    </div>
  );
}

/** One diff cell: the rank with its probability, or the absence marker. */
function rankCell(rank: number | null | undefined, probability: number | null | undefined): string {
  if (rank === null || rank === undefined) return "(absent)";
  if (probability === null || probability === undefined) return `#${rank}`;
  return `#${rank} · ${probability.toFixed(4)}`;
}

/** One utterance's stored audio: the waveform the canvas draws from the
 *  served WAV, and the player that plays the same bytes. */
export function UtteranceAudio({ cacheKey }: { cacheKey: string }) {
  const url = devApi.speechAudioUrl(cacheKey);
  const [peaks, setPeaks] = useState<number[] | null>(null);
  const [state, setState] = useState<"loading" | "ready" | "error">("loading");
  const [error, setError] = useState<string | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

  useEffect(() => {
    let cancelled = false;
    setState("loading");
    void (async () => {
      try {
        const response = await fetch(url, { credentials: "same-origin" });
        if (!response.ok) throw new Error(`the audio answered HTTP ${response.status}`);
        const bytes = new Uint8Array(await response.arrayBuffer());
        const drawn = wavPeaks(bytes, 96);
        if (cancelled) return;
        if (drawn === null) {
          setState("error");
          setError("the stored audio is not a PCM WAV the inspector can draw");
          return;
        }
        setPeaks(drawn);
        setState("ready");
      } catch (caught) {
        if (cancelled) return;
        setState("error");
        setError(explain(caught));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [url]);

  // The peaks draw once the canvas has mounted with them (the element is
  // conditionally rendered, so the draw is its own effect after the fetch).
  useEffect(() => {
    const canvas = canvasRef.current;
    if (!canvas || peaks === null) return;
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const { width, height } = canvas;
    ctx.clearRect(0, 0, width, height);
    ctx.fillStyle = "#7d8590";
    const slot = width / peaks.length;
    for (let i = 0; i < peaks.length; i += 1) {
      const bar = Math.max(1, peaks[i] * height);
      ctx.fillRect(Math.round(i * slot), (height - bar) / 2, Math.max(1, slot - 1), bar);
    }
  }, [peaks]);

  return (
    <div data-testid="action-audio">
      <h3>Utterance audio</h3>
      {state === "loading" ? (
        <p className="muted">Loading the utterance's audio…</p>
      ) : state === "error" ? (
        <p role="alert" className="muted" data-testid="action-audio-error">
          {error}
        </p>
      ) : (
        <>
          <canvas
            data-testid="action-audio-waveform"
            ref={canvasRef}
            width={384}
            height={64}
            style={{ width: "100%", maxWidth: 384, height: 64 }}
            aria-label="The utterance's waveform"
          >
            The utterance's waveform (canvas unavailable).
          </canvas>
          <audio
            data-testid="action-audio-player"
            controls
            src={url}
            style={{ width: "100%", maxWidth: 480 }}
          />
        </>
      )}
    </div>
  );
}

/** The whole panel: audio, labels, regression — for one pinned trace. */
export function TraceActions({
  traceId,
  session,
  events,
}: {
  traceId: string;
  session: Session | null;
  events: EventEnvelope[];
}) {
  const observation = traceObservation(traceId, events);
  if (!session || observation === null) {
    return null;
  }
  return (
    <div data-testid="trace-actions">
      {observation.cacheKey ? <UtteranceAudio cacheKey={observation.cacheKey} /> : null}
      <CorrectionLabels
        profileId={session.profile_id}
        observation={observation}
        traceId={traceId}
      />
      <RegressionActions traceId={traceId} />
    </div>
  );
}

/** One API failure as the page reports it: the status and the detail. */
function explain(caught: unknown): string {
  if (caught instanceof ApiError) {
    return `${apiStatus(caught)} ${caught.title}: ${caught.detail}`;
  }
  return String(caught);
}
