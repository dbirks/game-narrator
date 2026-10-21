/**
 * `src/pages/Traces.tsx` — the Traces screen (docs/design/plum-peach 2h,
 * ADR-0109): the line trace the operator reviews after a run.
 *
 * Left: a filterable list of the run's committed lines (spoken / skipped /
 * slow), each showing speaker + time + line; the selected one is `--peach`.
 * Main: the line as a 30px title, the 5-step strip (Saw frame → Read OCR →
 * Matched → Who → Spoke, each with its +ms offset; the Spoke step is
 * `--peach`), "What it read" (the textbox crop + raw vs normalised OCR),
 * "Why this line" (candidate probability bars + a one-sentence reason), and
 * the actions (Replay audio · Wrong speaker · Save as test · Download
 * evidence). The existing developer-dashboard inspection (candidates, Clef
 * payloads, the model inspector) stays reachable below as the full trace
 * detail (the session inspector's DetailPanel, unmodified).
 *
 * The "Wrong speaker" action is the UX-05 feedback loop: it records the
 * speaker-incorrect correction on the line immediately and hands the line to
 * the in-app agent (the ADR-0099 assistant), which checks the similar lines
 * and stages the bulk fix behind the confirmation gate. The correction is
 * user-confirmed before any bulk speaker change lands.
 *
 * The reading rules (grouping, the strip, the filters) live in
 * `tracesModel.ts` (pure, unit-tested); this file owns the data wiring and
 * the DOM.
 */

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router";

import { api, ApiError, apiStatus, type Schemas } from "../api/client";
import { fetchAllMessages } from "../api/profiles";
import {
  devApi,
  sessionsApi,
  openEventStream,
  type EventEnvelope,
  type Session,
} from "../api/sessions";
import { useAuth } from "../auth/auth-context";
import { ErrorState, Loading } from "../components/states";
import { Chip, KitEmptyState, PillButton } from "../components/kit";
import { DeveloperPage } from "./developer/DeveloperPage";
import { DetailPanel } from "./developer/DetailPanel";
import { correctionForLabel, CORRECTION_LABELS, wavPeaks } from "./developer/actions";
import { lineSnippet } from "./play/shared";
import {
  matchReasonSentence,
  traceLine,
  traceLines,
  type StripStep,
  type TraceLine,
} from "./tracesModel";

const SPEAKER_LABEL = CORRECTION_LABELS.find((label) => label.key === "speaker-incorrect");

/** The agent's dock events (ADR-0099 decision 8). */
const ASSISTANT_EVENT_NAMES = "assistant.message,assistant.question";
/** How long the wrong-speaker panel keeps following its thread. */
const THREAD_DEADLINE_MS = 120_000;
const THREAD_POLL_MS = 700;
/** The appliance runs a handful of sessions at a time; cap the catalog read. */
const MAX_SESSIONS_CATALOGUED = 20;

export function TracesPage() {
  const { atLeast, state } = useAuth();
  const [searchParams] = useSearchParams();

  if (state === "checking") {
    return <Loading label="Checking your session…" />;
  }
  if (!atLeast("developer")) {
    return (
      <div className="panel" data-testid="traces-role-required">
        <h1>Traces</h1>
        <p className="muted">
          Traces show the run's committed lines and the pipeline's reading of each one. It requires
          the developer role; your role does not grant access, and the API answers it with 403.
        </p>
      </div>
    );
  }
  // The inspector view: ?view=inspector or a ?session= deep link (the old
  // /developer surface, preserved unmodified; ADR-0109).
  const view = searchParams.get("view");
  const sessionParam = searchParams.get("session");
  if (view === "inspector" || sessionParam !== null) {
    return <DeveloperPage />;
  }
  return <TracesView />;
}

type FilterKey = "all" | "spoken" | "skipped" | "slow";

interface AgentThread {
  threadId: string;
  runId: string;
  /** The agent's narrative (assistant.message), newest last. */
  messages: string[];
  /** The open NEEDS-YOU / confirmation card (assistant.question). */
  question: { id: string; kind: string; prompt: string; options: string[] } | null;
  deadline: number;
}

function TracesView() {
  const [searchParams, setSearchParams] = useSearchParams();
  const [events, setEvents] = useState<EventEnvelope[] | null>(null);
  const [eventsError, setEventsError] = useState<unknown>(null);
  const [sessionMap, setSessionMap] = useState<Record<string, Session>>({});
  const [characters, setCharacters] = useState<Record<string, string>>({});
  // The "why this line" rows name the candidate line, not its store ULID
  // (the 2026-10-20 review — the same fix as Play's candidate rows): the
  // profile's own message list, keyed by message id, is the snippet source.
  const [messageText, setMessageText] = useState<Record<string, string>>({});
  const [thread, setThread] = useState<AgentThread | null>(null);

  const deepTrace = searchParams.get("trace");
  const lines = useMemo(() => (events ? traceLines(events, characters) : []), [events, characters]);
  const selected: TraceLine | null = useMemo(() => {
    if (lines.length === 0) return null;
    return lines.find((line) => line.traceId === deepTrace) ?? lines[0];
  }, [lines, deepTrace]);

  // The durable log: one bounded page covers the appliance's recent runs.
  useEffect(() => {
    let cancelled = false;
    setEvents(null);
    setEventsError(null);
    void devApi
      .events({ limit: 1000 })
      .then((page) => {
        if (!cancelled) setEvents(page.events);
      })
      .catch((error) => {
        if (!cancelled) setEventsError(error);
      });
    return () => {
      cancelled = true;
    };
  }, []);

  // The sessions the lines belong to, then each profile's character roster
  // (the speaker names the list and the "Who" step resolve to). The set of
  // session ids comes from the raw events, so it is independent of the
  // character names (no circular dependency on `lines`).
  const sessionIds = useMemo(() => {
    const ids = new Set<string>();
    if (events) for (const event of events) if (event.session_id) ids.add(event.session_id);
    return [...ids].slice(0, MAX_SESSIONS_CATALOGUED);
  }, [events]);

  useEffect(() => {
    let cancelled = false;
    setSessionMap({});
    setCharacters({});
    setMessageText({});
    if (sessionIds.length === 0) return;
    void (async () => {
      try {
        const sessions: Record<string, Session> = {};
        const profileIds = new Set<string>();
        for (const id of sessionIds) {
          try {
            const session = await sessionsApi.get(id);
            sessions[id] = session;
            profileIds.add(session.profile_id);
          } catch {
            // A session that has gone missing is not fatal to the catalog.
          }
        }
        if (cancelled) return;
        setSessionMap(sessions);
        const names: Record<string, string> = {};
        for (const profileId of profileIds) {
          try {
            const body = (await api.get("/api/v1/profiles/{id}/characters", {
              params: { id: profileId },
            })) as unknown as { characters: { id: string; name: string }[] };
            for (const row of body.characters ?? []) names[row.id] = row.name;
          } catch {
            // A profile with no roster yet still renders (ids as names).
          }
        }
        if (!cancelled) setCharacters(names);
        // The candidates' line text (one read per profile, the same cursor
        // walk the Play page's speaker chips use): the "why this line" row
        // shows the line's own words, its id kept in the row's title.
        const texts: Record<string, string> = {};
        for (const profileId of profileIds) {
          try {
            for (const message of await fetchAllMessages(profileId)) {
              texts[message.id] =
                message.speech_text || message.normalized_text || message.original_text;
            }
          } catch {
            // A profile whose list fails still renders (ids as names).
          }
        }
        if (!cancelled) setMessageText(texts);
      } catch {
        if (!cancelled) setCharacters({});
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [sessionIds]);

  // The selected line's full event chain (the per-trace record the API
  // serves; the strip and the detail re-read it so the page cannot drift).
  const selectedTraceId = selected?.traceId ?? null;
  const [traceEvents, setTraceEvents] = useState<EventEnvelope[] | null>(null);
  const [traceError, setTraceError] = useState<unknown>(null);
  useEffect(() => {
    let cancelled = false;
    setTraceEvents(null);
    setTraceError(null);
    if (!selectedTraceId) return;
    void devApi
      .trace(selectedTraceId)
      .then((page) => {
        if (!cancelled) setTraceEvents(page.events);
      })
      .catch((error) => {
        if (!cancelled) setTraceError(error);
      });
    return () => {
      cancelled = true;
    };
  }, [selectedTraceId]);

  const selectLine = useCallback(
    (traceId: string) => {
      const next = new URLSearchParams(searchParams);
      next.set("trace", traceId);
      setSearchParams(next, { replace: true });
    },
    [searchParams, setSearchParams],
  );

  if (eventsError !== null) {
    return (
      <div className="panel">
        <h1>Traces</h1>
        <ErrorState error={eventsError} action="Loading the run's lines" />
      </div>
    );
  }
  if (events === null) {
    return (
      <div className="panel">
        <h1>Traces</h1>
        <Loading label="Loading the run's lines…" />
      </div>
    );
  }

  const selectedSession =
    selected && selected.sessionId ? (sessionMap[selected.sessionId] ?? null) : null;

  return (
    <div>
      <h1>Traces</h1>
      <div className="traces-layout">
        <div className="traces-side">
          <LineList
            lines={lines}
            characters={characters}
            selectedTraceId={selectedTraceId}
            onSelect={selectLine}
          />
        </div>
        <div className="traces-main">
          {selected ? (
            <LineDetail
              line={selected}
              traceEvents={traceEvents}
              traceError={traceError}
              characters={characters}
              session={selectedSession}
              thread={thread}
              onThreadChange={setThread}
              profileId={selectedSession?.profile_id ?? null}
              messageText={messageText}
            />
          ) : (
            <KitEmptyState title="No lines yet" testid="traces-empty">
              <p className="muted">
                Run a session first — its committed lines land here as they speak.
              </p>
            </KitEmptyState>
          )}
        </div>
      </div>
    </div>
  );
}

/** The left column: the filter pills and the line list. */
function LineList({
  lines,
  characters,
  selectedTraceId,
  onSelect,
}: {
  lines: TraceLine[];
  characters: Record<string, string>;
  selectedTraceId: string | null;
  onSelect: (traceId: string) => void;
}) {
  const [filter, setFilter] = useState<FilterKey>("all");

  const filtered = useMemo(() => {
    if (filter === "all") return lines;
    if (filter === "spoken") return lines.filter((line) => line.status === "spoken");
    if (filter === "skipped") return lines.filter((line) => line.status === "skipped");
    return lines.filter((line) => line.slow);
  }, [lines, filter]);

  const counts = useMemo(
    () => ({
      all: lines.length,
      spoken: lines.filter((line) => line.status === "spoken").length,
      skipped: lines.filter((line) => line.status === "skipped").length,
      slow: lines.filter((line) => line.slow).length,
    }),
    [lines],
  );

  const pills: { key: FilterKey; label: string; count: number }[] = [
    { key: "all", label: "All", count: counts.all },
    { key: "spoken", label: "Spoken", count: counts.spoken },
    { key: "skipped", label: "Skipped", count: counts.skipped },
    { key: "slow", label: "Slow", count: counts.slow },
  ];

  return (
    <div className="panel" data-testid="traces-list">
      <div className="traces-pills" role="group" aria-label="Filter lines">
        {pills.map((pill) => (
          <button
            key={pill.key}
            type="button"
            className={filter === pill.key ? "traces-pill active" : "traces-pill"}
            aria-pressed={filter === pill.key}
            data-testid={`traces-filter-${pill.key}`}
            onClick={() => setFilter(pill.key)}
          >
            {pill.label} <span className="muted">({pill.count})</span>
          </button>
        ))}
      </div>
      {filtered.length === 0 ? (
        <p className="muted" data-testid="traces-list-empty">
          {filter === "all" ? "No lines to show." : `No ${filter} lines.`}
        </p>
      ) : (
        <ul className="traces-lines" data-testid="traces-line-rows">
          {filtered.map((line) => {
            const current = line.traceId === selectedTraceId;
            const speaker = line.speakerId
              ? (characters[line.speakerId] ?? line.speakerId)
              : "Narrator";
            return (
              <li key={line.traceId}>
                <button
                  type="button"
                  className={current ? "traces-line current" : "traces-line"}
                  data-testid={`traces-line-${line.traceId}`}
                  aria-current={current ? "true" : undefined}
                  onClick={() => onSelect(line.traceId)}
                >
                  <span className="traces-line-meta">
                    <Chip tone={current ? "peach" : "neutral"} testid="traces-line-speaker">
                      {speaker}
                    </Chip>
                    <span className="muted traces-line-time">{formatTime(line.occurredAt)}</span>
                  </span>
                  <span className="traces-line-text" data-testid="traces-line-text">
                    {line.text}
                  </span>
                </button>
              </li>
            );
          })}
        </ul>
      )}
    </div>
  );
}

/** The main column for one selected line. */
function LineDetail({
  line,
  traceEvents,
  traceError,
  characters,
  session,
  thread,
  onThreadChange,
  profileId,
  messageText,
}: {
  line: TraceLine;
  traceEvents: EventEnvelope[] | null;
  traceError: unknown;
  characters: Record<string, string>;
  session: Session | null;
  thread: AgentThread | null;
  onThreadChange: (thread: AgentThread | null) => void;
  profileId: string | null;
  messageText: Record<string, string>;
}) {
  const detail = useMemo(
    () => (traceEvents ? traceLine(line.traceId, traceEvents, characters) : null),
    [traceEvents, line.traceId, characters],
  );

  return (
    <div data-testid="traces-detail">
      <h2 className="traces-title" data-testid="traces-title">
        {detail?.text ?? line.text}
      </h2>

      {session ? (
        <p className="muted">
          <a
            data-testid="traces-open-inspector"
            className="mono"
            href={`/traces?view=inspector&session=${encodeURIComponent(session.session_id)}`}
          >
            Open in session inspector →
          </a>
        </p>
      ) : null}

      {traceError !== null ? (
        <p role="alert" className="muted" data-testid="traces-detail-error">
          Loading the trace failed:{" "}
          {traceError instanceof Error ? traceError.message : String(traceError)}
        </p>
      ) : null}

      <StripSteps steps={detail?.steps ?? []} testidPrefix="traces-step" />

      <div className="traces-read" data-testid="traces-what-it-read">
        <h3>What it read</h3>
        <div className="traces-read-body">
          {detail?.cropSha ? (
            <img
              data-testid="traces-crop"
              src={devApi.assetUrl(detail.cropSha)}
              alt="The textbox crop the OCR engine read"
              width={280}
            />
          ) : (
            <p className="muted" data-testid="traces-crop-missing">
              No stored crop for this observation (dev trace off, or the sample was evicted).
            </p>
          )}
          <dl className="stats">
            <dt>Raw OCR text</dt>
            <dd className="mono" data-testid="traces-ocr-raw">
              {detail?.rawText || "—"}
            </dd>
            <dt>Normalised text</dt>
            <dd className="mono" data-testid="traces-ocr-normalized">
              {detail?.normalizedText || "—"}
            </dd>
            <dt>OCR confidence</dt>
            <dd data-testid="traces-ocr-confidence">
              {detail?.confidence === null || detail?.confidence === undefined
                ? "—"
                : detail.confidence.toFixed(3)}
            </dd>
          </dl>
        </div>
      </div>

      <div className="traces-why" data-testid="traces-why-this-line">
        <h3>Why this line</h3>
        <p className="muted" data-testid="traces-why-reason">
          {detail
            ? matchReasonSentence(
                detail.reason,
                detail.candidates[0]?.probability ?? null,
                detail.margin,
              )
            : "Loading the ranking…"}
        </p>
        <ul className="traces-candidates" data-testid="traces-candidate-rows">
          {(detail?.candidates ?? []).map((candidate, index) => (
            <li
              key={`${candidate.messageId}-${index}`}
              data-testid={`traces-candidate-${index}`}
              title={candidate.messageId}
            >
              <span className="bar">
                <span
                  className="bar-fill"
                  style={{ width: `${barWidth(candidate.probability ?? 0)}%` }}
                />
              </span>
              {/* The row names the candidate's line text (the 2026-10-20
                  review's raw-ULID finding), the message id kept in the
                  row's title — the same fix as Play's candidate rows. */}
              <span className="muted traces-candidate-text">
                {messageText[candidate.messageId]
                  ? lineSnippet(messageText[candidate.messageId])
                  : `message ${candidate.messageId}`}
              </span>
              <span data-testid={`traces-candidate-prob-${index}`}>
                {candidate.probability === null ? "—" : candidate.probability.toFixed(4)}
              </span>
            </li>
          ))}
        </ul>
      </div>

      <LineActions
        line={detail ?? line}
        profileId={profileId}
        characters={characters}
        thread={thread}
        onThreadChange={onThreadChange}
      />
      {thread !== null ? (
        <AgentFeedback thread={thread} onDismissed={() => onThreadChange(null)} />
      ) : null}

      {session && traceEvents ? (
        <div className="traces-inspection" data-testid="traces-full-inspection">
          <h3>Full inspection</h3>
          <p className="muted">
            The session inspector's read of this trace — the stored frame and ROI, the candidates
            with their components, the Clef decision, the commitment, and the per-event actions.
          </p>
          <DetailPanel pinned={pinnedEvent(traceEvents, line)} session={session} />
        </div>
      ) : null}
    </div>
  );
}

/**
 * The event DetailPanel pins for the selected line: the commit when the line
 * committed (it carries the trace id and renders the full detail branch),
 * else the newest observation.
 */
function pinnedEvent(events: EventEnvelope[], _line: TraceLine): EventEnvelope {
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event && event.name === "message.committed") return event;
  }
  for (let i = events.length - 1; i >= 0; i -= 1) {
    const event = events[i];
    if (event && event.name === "text.observed") return event;
  }
  return events[0] as EventEnvelope;
}

/** The 5-step strip: Saw → Read → Matched → Who → Spoke, each +ms. */
function StripSteps({ steps, testidPrefix }: { steps: StripStep[]; testidPrefix: string }) {
  if (steps.length === 0) return null;
  return (
    <ol className="traces-strip" data-testid={`${testidPrefix}-strip`}>
      {steps.map((step, index) => {
        const isLast = index === steps.length - 1;
        return (
          <li
            key={step.step}
            className={isLast ? "traces-strip-step final" : "traces-strip-step"}
            data-testid={`${testidPrefix}-${step.step}`}
          >
            <span className="traces-strip-label">
              {index + 1}. {stepLabel(step.step)}
            </span>
            <span className="traces-strip-value" data-testid={`${testidPrefix}-${step.step}-value`}>
              {step.value}
            </span>
            <span className="muted traces-strip-offset">+{step.offsetMs} ms</span>
          </li>
        );
      })}
    </ol>
  );
}

const STEP_LABELS: Record<StripStep["step"], string> = {
  saw: "Saw frame",
  read: "Read OCR",
  matched: "Matched",
  who: "Who",
  spoke: "Spoke",
};

function stepLabel(step: StripStep["step"]): string {
  return STEP_LABELS[step];
}

/** The action row: Replay audio · Wrong speaker · Save as test · Download evidence. */
function LineActions({
  line,
  profileId,
  characters,
  thread,
  onThreadChange,
}: {
  line: TraceLine;
  profileId: string | null;
  characters: Record<string, string>;
  thread: AgentThread | null;
  onThreadChange: (thread: AgentThread | null) => void;
}) {
  const [fixture, setFixture] = useState<Schemas["DevFixture"] | null>(null);
  const [fixtureBusy, setFixtureBusy] = useState(false);
  const [fixtureError, setFixtureError] = useState<string | null>(null);

  const [evidenceBusy, setEvidenceBusy] = useState(false);
  const [evidenceError, setEvidenceError] = useState<string | null>(null);

  const [targetSpeaker, setTargetSpeaker] = useState("");
  const [wrongSpeakerBusy, setWrongSpeakerBusy] = useState(false);
  const [wrongSpeakerError, setWrongSpeakerError] = useState<string | null>(null);

  const roster = useMemo(
    () => Object.entries(characters).map(([id, name]) => ({ id, name })),
    [characters],
  );

  const saveFixture = useCallback(async () => {
    setFixtureBusy(true);
    setFixtureError(null);
    try {
      const body = await devApi.recordFixture(line.traceId);
      setFixture(body.fixture);
    } catch (caught) {
      setFixtureError(explain(caught));
    } finally {
      setFixtureBusy(false);
    }
  }, [line.traceId]);

  const downloadEvidence = useCallback(async () => {
    setEvidenceBusy(true);
    setEvidenceError(null);
    try {
      const response = await fetch(devApi.evidenceUrl(line.traceId), {
        credentials: "same-origin",
      });
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
      anchor.download = `trace-evidence-${line.traceId}.tar.gz`;
      document.body.append(anchor);
      anchor.click();
      anchor.remove();
      URL.revokeObjectURL(url);
    } catch (caught) {
      setEvidenceError(caught instanceof Error ? caught.message : String(caught));
    } finally {
      setEvidenceBusy(false);
    }
  }, [line.traceId]);

  const markWrongSpeaker = useCallback(async () => {
    if (!profileId || !line.messageId || targetSpeaker === "" || SPEAKER_LABEL === undefined)
      return;
    setWrongSpeakerBusy(true);
    setWrongSpeakerError(null);
    try {
      // 1. The correction on this line lands immediately (UX-05: "the
      //     corrected line is changed immediately").
      await api.post("/api/v1/corrections", {
        body: correctionForLabel(SPEAKER_LABEL, {
          profileId,
          traceId: line.traceId,
          messageId: line.messageId,
          speakerId: line.speakerId,
          occurrenceId: null,
          text: line.text,
        }),
      });
      // 2. Hand the line to the in-app agent (ADR-0099): the screen context
      //    names the selection and the target; the agent checks the similar
      //    lines and stages the bulk fix behind the confirmation gate.
      const accepted = (await api.post("/api/v1/assistant/messages", {
        body: {
          text:
            `Wrong speaker: this line was attributed to the current speaker, but it was ` +
            `spoken by ${characters[targetSpeaker] ?? targetSpeaker}. ` +
            `target_character_id=${targetSpeaker}. ` +
            `Find the other lines that share this correction's pattern and re-point ` +
            `them to the correct speaker.`,
          context: {
            route: "/traces",
            profile_id: profileId,
            selection: { type: "line", id: line.messageId },
          },
        },
      })) as unknown as { thread_id: string; run_id: string };
      onThreadChange({
        threadId: accepted.thread_id,
        runId: accepted.run_id,
        messages: [],
        question: null,
        deadline: Date.now() + THREAD_DEADLINE_MS,
      });
    } catch (caught) {
      setWrongSpeakerError(explain(caught));
    } finally {
      setWrongSpeakerBusy(false);
    }
  }, [profileId, line, targetSpeaker, characters, onThreadChange]);

  return (
    <div className="traces-actions" data-testid="traces-actions">
      <div className="row">
        <ReplayAudio cacheKey={line.cacheKey} />
        <select
          data-testid="traces-wrong-speaker-select"
          value={targetSpeaker}
          onChange={(event) => setTargetSpeaker(event.target.value)}
          disabled={wrongSpeakerBusy || thread !== null}
          aria-label="The correct speaker"
        >
          <option value="">Correct speaker…</option>
          {roster.map((character) => (
            <option key={character.id} value={character.id}>
              {character.name}
            </option>
          ))}
        </select>
        <PillButton
          testid="traces-action-wrong-speaker"
          variant="coral"
          disabled={wrongSpeakerBusy || thread !== null || targetSpeaker === ""}
          onClick={() => void markWrongSpeaker()}
        >
          {wrongSpeakerBusy ? "Sending to the agent…" : "Wrong speaker"}
        </PillButton>
        <PillButton
          testid="traces-action-save-test"
          variant="outline"
          disabled={fixtureBusy}
          onClick={() => void saveFixture()}
        >
          {fixtureBusy ? "Saving…" : "Save as test"}
        </PillButton>
        <PillButton
          testid="traces-action-evidence"
          variant="outline"
          disabled={evidenceBusy}
          onClick={() => void downloadEvidence()}
        >
          {evidenceBusy ? "Downloading…" : "Download evidence"}
        </PillButton>
      </div>

      {fixture ? (
        <dl className="stats" data-testid="traces-fixture-result">
          <dt>Fixture id</dt>
          <dd>
            <code>{fixture.id}</code>
          </dd>
          <dt>Location</dt>
          <dd data-testid="traces-fixture-dir">{fixture.dir}</dd>
          <dt>Entries</dt>
          <dd data-testid="traces-fixture-entries">{fixture.entries.length}</dd>
        </dl>
      ) : null}
      {fixtureError ? (
        <p role="alert" className="muted" data-testid="traces-fixture-error">
          {fixtureError}
        </p>
      ) : null}
      {evidenceError ? (
        <p role="alert" className="muted" data-testid="traces-evidence-error">
          {evidenceError}
        </p>
      ) : null}
      {wrongSpeakerError ? (
        <p role="alert" className="muted" data-testid="traces-wrong-speaker-error">
          {wrongSpeakerError}
        </p>
      ) : null}
    </div>
  );
}

/** The utterance's stored audio (the speech cache key the trace carried). */
function ReplayAudio({ cacheKey }: { cacheKey: string | null }) {
  const [peaks, setPeaks] = useState<number[] | null>(null);
  const [error, setError] = useState<string | null>(null);
  const canvasRef = useRef<HTMLCanvasElement | null>(null);

  const url = cacheKey ? devApi.speechAudioUrl(cacheKey) : null;

  useEffect(() => {
    let cancelled = false;
    setPeaks(null);
    setError(null);
    if (!url) return;
    void (async () => {
      try {
        const response = await fetch(url, { credentials: "same-origin" });
        if (!response.ok) throw new Error(`the audio answered HTTP ${response.status}`);
        const bytes = new Uint8Array(await response.arrayBuffer());
        const drawn = wavPeaks(bytes, 96);
        if (cancelled) return;
        if (drawn === null) {
          setError("the stored audio is not a PCM WAV the inspector can draw");
          return;
        }
        setPeaks(drawn);
      } catch (caught) {
        if (!cancelled) setError(caught instanceof Error ? caught.message : String(caught));
      }
    })();
    return () => {
      cancelled = true;
    };
  }, [url]);

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

  if (!cacheKey) {
    return (
      <span className="muted" data-testid="traces-replay-unavailable">
        Replay audio — no stored utterance
      </span>
    );
  }
  return (
    <span data-testid="traces-replay-audio">
      <audio data-testid="traces-replay-player" controls src={url as string} />
      {peaks !== null ? (
        <canvas
          data-testid="traces-replay-waveform"
          ref={canvasRef}
          width={192}
          height={32}
          aria-label="The utterance's waveform"
        />
      ) : null}
      {error ? (
        <span role="alert" className="muted" data-testid="traces-replay-error">
          {error}
        </span>
      ) : null}
    </span>
  );
}

/**
 * The UX-05 agent feedback: the agent's suggestion (the similar lines it
 * found) and the NEEDS-YOU confirmation card. The bulk fix lands only on an
 * explicit confirm.
 */
function AgentFeedback({ thread, onDismissed }: { thread: AgentThread; onDismissed: () => void }) {
  // The rebuild (and the path that catches up for a page that opened mid-
  // run or whose stream dropped): poll the thread snapshot. The SSE cards
  // are the live path and land a touch faster; both update the same fields.
  const [messages, setMessages] = useState<string[]>([]);
  const [question, setQuestion] = useState<AgentThread["question"]>(null);

  // The live cards (ADR-0099): assistant.message (narrative) and
  // assistant.question (the NEEDS-YOU / confirmation card), filtered to this
  // run.
  useEffect(() => {
    const url = `/api/v1/events?names=${encodeURIComponent(ASSISTANT_EVENT_NAMES)}`;
    const close = openEventStream(url, {
      onEvent: (envelope: EventEnvelope) => {
        const data =
          envelope.data && typeof envelope.data === "object"
            ? (envelope.data as Record<string, unknown>)
            : {};
        if (data.run_id !== thread.runId && data.thread_id !== thread.threadId) return;
        if (envelope.name === "assistant.message") {
          const text = typeof data.text === "string" ? data.text : "";
          if (text !== "") setMessages((prev) => [...prev, text]);
        } else if (envelope.name === "assistant.question") {
          setQuestion({
            id: typeof data.id === "string" ? data.id : "",
            kind: typeof data.kind === "string" ? data.kind : "question",
            prompt: typeof data.prompt === "string" ? data.prompt : "",
            options: Array.isArray(data.options) ? (data.options as string[]) : [],
          });
        }
      },
      onGiveUp: () => {
        // The snapshot below keeps the panel correct if the stream ends.
      },
    });
    return close;
  }, [thread.runId, thread.threadId]);

  // The rebuild: the thread snapshot is the durable record (turns + the open
  // question), so it is the authoritative state the panel renders. An empty
  // assistant turn is the loop's tool-call-only step (persisted before its
  // calls execute), not narrative — the panel renders the non-empty answers
  // only, the same rule the live path applies (assistant.message fires only
  // for a non-empty answer).
  useEffect(() => {
    let cancelled = false;
    let timer: ReturnType<typeof setTimeout> | undefined;
    const tick = async () => {
      try {
        const body = (await api.get("/api/v1/assistant/threads/{id}", {
          params: { id: thread.threadId },
        })) as unknown as {
          turns: { role: string; content: string }[];
          questions: {
            id: string;
            kind: string;
            prompt: string;
            options?: string[];
            answer?: string;
          }[];
        };
        if (cancelled) return;
        const assistantTurns = (body.turns ?? [])
          .filter((turn) => turn.role === "assistant")
          .map((turn) => turn.content)
          .filter((content) => content !== "");
        setMessages(assistantTurns);
        const open = (body.questions ?? []).find((row) => row.answer === undefined);
        setQuestion(
          open
            ? {
                id: open.id,
                kind: open.kind,
                prompt: open.prompt,
                options: open.options ?? [],
              }
            : null,
        );
      } catch {
        // The snapshot has not landed yet: the next tick retries.
      } finally {
        if (!cancelled && Date.now() < thread.deadline) timer = setTimeout(tick, THREAD_POLL_MS);
      }
    };
    void tick();
    return () => {
      cancelled = true;
      if (timer) clearTimeout(timer);
    };
  }, [thread.threadId, thread.deadline]);

  const answer = useCallback(
    async (choice: string) => {
      if (!question) return;
      try {
        await api.post("/api/v1/assistant/answers", {
          body: {
            thread_id: thread.threadId,
            question_id: question.id,
            answer: choice,
          },
        });
      } catch {
        // The snapshot re-renders the card's state; the answer is durable
        // server-side either way.
      }
    },
    [question, thread.threadId],
  );

  return (
    <div className="traces-agent" data-testid="traces-agent-panel">
      <h3>Agent</h3>
      {messages.length === 0 ? (
        <p className="muted" data-testid="traces-agent-working">
          The agent is checking the similar lines…
        </p>
      ) : (
        <ul className="traces-agent-messages" data-testid="traces-agent-messages">
          {messages.map((message, index) => (
            <li key={index} data-testid="traces-agent-message">
              {message}
            </li>
          ))}
        </ul>
      )}
      {question ? (
        <div className="traces-agent-question" data-testid="traces-agent-question">
          <p data-testid="traces-agent-question-prompt">{question.prompt}</p>
          {question.options.length > 0 ? (
            <p className="muted" data-testid="traces-agent-question-options">
              {question.options.join(" · ")}
            </p>
          ) : null}
          <div className="row">
            <PillButton
              testid="traces-agent-confirm"
              variant="ink"
              onClick={() => void answer("confirm")}
            >
              Confirm
            </PillButton>
            <PillButton
              testid="traces-agent-cancel"
              variant="outline"
              onClick={() => {
                void answer("cancel");
                onDismissed();
              }}
            >
              Cancel
            </PillButton>
          </div>
        </div>
      ) : null}
    </div>
  );
}

// ── small helpers ───────────────────────────────────────────────────────────

/** Bar width for a value in [0,1]; clamped so an out-of-band score cannot
 *  render a 400%-wide bar. */
function barWidth(value: number): number {
  return Math.max(0, Math.min(100, value * 100));
}

function formatTime(iso: string): string {
  if (!iso) return "—";
  const date = new Date(iso);
  if (Number.isNaN(date.getTime())) return iso;
  return date.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" });
}

/** One API failure as the page reports it: the status and the detail. */
function explain(caught: unknown): string {
  if (caught instanceof ApiError) {
    return `${apiStatus(caught)} ${caught.title}: ${caught.detail}`;
  }
  return String(caught);
}
