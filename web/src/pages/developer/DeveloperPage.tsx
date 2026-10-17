/**
 * `src/pages/developer/DeveloperPage.tsx` — the /developer inspector
 * (docs/spec/09 "Developer dashboard layout", docs/spec/16 milestone 2:
 * "Developer inspector displays frame, crop, normalized OCR and candidates").
 *
 * What this file owns: the developer-role gate, the session control panel
 * (pick a published revision, name the replay clip, dev-trace toggle,
 * Start/Stop), and the live wiring — the SSE event stream with its durable
 * polling fallback (docs/spec/08 "Event stream reliability") and the session
 * snapshot poll that keeps the state and pipeline counters current.
 *
 * What it does not own: the timeline's merge rules (timeline.ts, tested in
 * timeline.test.ts) and the detail panel's rendering (DetailPanel.tsx).
 */

import { useCallback, useEffect, useReducer, useRef, useState } from "react";

import { ApiError, catalogApi } from "../../api/client";
import {
  devApi,
  openEventStream,
  sessionsApi,
  type EventEnvelope,
  type Session,
} from "../../api/sessions";
import { useAuth } from "../../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../../components/states";
import { EventTimeline } from "./EventTimeline";
import { DetailPanel } from "./DetailPanel";
import { initialTimeline, timelineReducer } from "./timeline";

const DEFAULT_REPLAY_DIR = "testdata/synthetic/replay/dialogue-advance-001";
const SNAPSHOT_POLL_MS = 1500;
const POLL_FALLBACK_MS = 1500;

export function DeveloperPage() {
  const { atLeast, state: authState } = useAuth();

  if (authState === "checking") {
    return <Loading label="Checking your session…" />;
  }
  if (!atLeast("developer")) {
    return (
      <div className="panel" data-testid="dev-role-required">
        <h1>Developer inspector</h1>
        <p className="muted">
          This page shows raw recognised gameplay text, provider traces and stored frames. It
          requires the developer role; your role does not grant access, and the API answers it with
          403.
        </p>
      </div>
    );
  }
  return <DeveloperInspector />;
}

interface PublishedRevisionOption {
  id: string;
  label: string;
}

/** One session under the microscope, or null before the first Start. */
interface TrackedSession {
  session: Session;
  error: ApiError | null;
  streaming: boolean;
  streamFailed: boolean;
}

function DeveloperInspector() {
  const [revisions, setRevisions] = useState<PublishedRevisionOption[] | null>(null);
  const [revisionsError, setRevisionsError] = useState<unknown>(null);
  const [profileId, setProfileId] = useState("");
  const [replayDir, setReplayDir] = useState(DEFAULT_REPLAY_DIR);
  const [devTrace, setDevTrace] = useState(true);
  const [tracked, setTracked] = useState<TrackedSession | null>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const [acting, setActing] = useState(false);

  const [timeline, dispatch] = useReducer(timelineReducer, undefined, initialTimeline);
  const streamRef = useRef<() => void>(() => {});

  // The published revisions are the only ones a session may run
  // (docs/spec/01 "only the published revision runs"), so the selector lists
  // exactly those: every game, every edition, the revisions with status
  // "published".
  useEffect(() => {
    let cancelled = false;
    (async () => {
      try {
        const games = await catalogApi.listGames({ limit: 100 });
        const options: PublishedRevisionOption[] = [];
        for (const game of games.games ?? []) {
          const editions = await catalogApi.listEditions(game.id);
          for (const edition of editions.editions ?? []) {
            const profiles = await catalogApi.listProfiles(edition.id);
            for (const profile of profiles.profiles ?? []) {
              if (profile.status !== "published") continue;
              options.push({
                id: profile.id,
                label: `${game.title} / ${edition.region} ${edition.language} ${edition.revision} / rev ${profile.revision_num}`,
              });
            }
          }
        }
        if (!cancelled) {
          setRevisions(options);
          if (options.length > 0) setProfileId((current) => current || options[0].id);
        }
      } catch (error) {
        if (!cancelled) setRevisionsError(error);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const start = useCallback(async () => {
    setActing(true);
    setActionError(null);
    dispatch({ type: "reset" });
    try {
      const session = await sessionsApi.start({
        profile_id: profileId,
        frame_source: { kind: "replay", dir: replayDir },
        mode: "conservative",
        dev_trace: devTrace,
      });
      setTracked({ session, error: null, streaming: true, streamFailed: false });
    } catch (error) {
      setActionError(error);
      setTracked(null);
    } finally {
      setActing(false);
    }
  }, [profileId, replayDir, devTrace]);

  const stop = useCallback(async () => {
    if (!tracked) return;
    setActing(true);
    setActionError(null);
    try {
      const session = await sessionsApi.stop(tracked.session.session_id);
      setTracked({ ...tracked, session });
    } catch (error) {
      setActionError(error);
    } finally {
      setActing(false);
    }
  }, [tracked]);

  // Live state and pipeline counters: the snapshot poll is the degraded-mode
  // truth (docs/spec/09 "state reconciliation"), running for as long as the
  // session has not settled.
  const active =
    tracked && (tracked.session.state === "starting" || tracked.session.state === "running");
  useEffect(() => {
    if (!tracked || !active) return;
    const id = tracked.session.session_id;
    const timer = setInterval(() => {
      void sessionsApi.get(id).then((session) => {
        setTracked((current) => (current ? { ...current, session } : current));
      });
    }, SNAPSHOT_POLL_MS);
    return () => clearInterval(timer);
  }, [tracked, active]);

  // The SSE half: one stream per session; events feed the timeline reducer.
  // When the transport dies the durable /dev/events page takes over, because
  // the hub persists every event it streams.
  const streamUrl = tracked
    ? `/api/v1/events?session_id=${encodeURIComponent(tracked.session.session_id)}`
    : null;
  const lastSeq = timeline.maxSeq;
  useEffect(() => {
    if (!tracked || !tracked.streaming || !streamUrl) return;
    const close = openEventStream(streamUrl, {
      onEvent: (envelope: EventEnvelope) => {
        dispatch({ type: "events", events: [envelope] });
        setTracked((current) =>
          current?.streamFailed ? { ...current, streamFailed: false } : current,
        );
      },
      onGiveUp: () =>
        setTracked((current) => (current ? { ...current, streamFailed: true } : current)),
    });
    streamRef.current = close;
    return () => {
      close();
      streamRef.current = () => {};
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [streamUrl, tracked?.session.session_id, tracked?.streamFailed]);

  // The polling fallback: while the stream is dead, page the durable log from
  // the highest seq we have. The reducer dedupes the replayed boundary seq.
  useEffect(() => {
    if (!tracked || !tracked.streamFailed) return;
    const id = tracked.session.session_id;
    const timer = setInterval(() => {
      void devApi.events({ sessionId: id, sinceSeq: lastSeq, limit: 200 }).then((page) => {
        if (page.events.length > 0) dispatch({ type: "events", events: page.events });
      });
    }, POLL_FALLBACK_MS);
    return () => clearInterval(timer);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [tracked?.streamFailed, lastSeq]);

  return (
    <div>
      <h1>Developer inspector</h1>
      <div className="dev-layout">
        <div className="dev-side">
          <SessionPanel
            revisions={revisions}
            revisionsError={revisionsError}
            profileId={profileId}
            setProfileId={setProfileId}
            replayDir={replayDir}
            setReplayDir={setReplayDir}
            devTrace={devTrace}
            setDevTrace={setDevTrace}
            tracked={tracked}
            acting={acting}
            actionError={actionError}
            onStart={() => void start()}
            onStop={() => void stop()}
          />
          {tracked ? (
            <SessionStats
              session={tracked.session}
              streaming={tracked.streaming}
              failed={tracked.streamFailed}
            />
          ) : (
            <EmptyState title="No session">
              <p className="muted">Start a replay session to watch its events stream in.</p>
            </EmptyState>
          )}
        </div>
        <div className="dev-main">
          <EventTimeline
            state={timeline}
            sessionId={tracked?.session.session_id ?? null}
            onPin={(eventId) => dispatch({ type: "pin", eventId })}
            onOlder={(events, atStart) => dispatch({ type: "older", events, atStart })}
          />
          <DetailPanel pinned={timeline.pinnedEvent} session={tracked ? tracked.session : null} />
        </div>
      </div>
    </div>
  );
}

interface SessionPanelProps {
  revisions: PublishedRevisionOption[] | null;
  revisionsError: unknown;
  profileId: string;
  setProfileId: (id: string) => void;
  replayDir: string;
  setReplayDir: (dir: string) => void;
  devTrace: boolean;
  setDevTrace: (on: boolean) => void;
  tracked: TrackedSession | null;
  acting: boolean;
  actionError: unknown;
  onStart: () => void;
  onStop: () => void;
}

function SessionPanel(props: SessionPanelProps) {
  const {
    revisions,
    revisionsError,
    profileId,
    setProfileId,
    replayDir,
    setReplayDir,
    devTrace,
    setDevTrace,
    tracked,
    acting,
    actionError,
    onStart,
    onStop,
  } = props;
  const busy =
    tracked !== null &&
    (tracked.session.state === "starting" || tracked.session.state === "running");
  return (
    <div className="panel" data-testid="session-panel">
      <h2>Session</h2>
      {revisionsError ? (
        <ErrorState error={revisionsError} action="Loading the published revisions" />
      ) : revisions === null ? (
        <Loading label="Loading the published revisions…" />
      ) : revisions.length === 0 ? (
        <p className="muted">
          No published revision exists yet; publish one from the catalogue first.
        </p>
      ) : (
        <label>
          <span className="muted">Published profile revision</span>
          <select
            data-testid="session-profile"
            value={profileId}
            onChange={(event) => setProfileId(event.target.value)}
            disabled={busy}
          >
            {revisions.map((option) => (
              <option key={option.id} value={option.id}>
                {option.label}
              </option>
            ))}
          </select>
        </label>
      )}
      <label>
        <span className="muted">Replay clip</span>
        <input
          data-testid="session-replay-dir"
          value={replayDir}
          onChange={(event) => setReplayDir(event.target.value)}
          disabled={busy}
        />
      </label>
      <label className="row">
        <input
          type="checkbox"
          data-testid="session-dev-trace"
          checked={devTrace}
          onChange={(event) => setDevTrace(event.target.checked)}
          disabled={busy}
        />
        <span>Developer trace (store sampled frames and ROI crops)</span>
      </label>
      <div className="row">
        <button
          type="button"
          data-testid="session-start"
          onClick={onStart}
          disabled={busy || acting || !profileId}
        >
          Start
        </button>
        <button
          type="button"
          data-testid="session-stop"
          onClick={onStop}
          disabled={!tracked || acting}
        >
          Stop
        </button>
      </div>
      {actionError ? (
        <ErrorState error={actionError} action={tracked ? "Stopping" : "Starting"} />
      ) : null}
    </div>
  );
}

/** The live state and the pipeline's own counters (docs/spec/16 session gate). */
function SessionStats({
  session,
  streaming,
  failed,
}: {
  session: Session;
  streaming: boolean;
  failed: boolean;
}) {
  const stats = session.stats;
  return (
    <div className="panel" data-testid="session-stats">
      <h2>
        State: <span data-testid="session-state">{session.state}</span>
        {session.reason ? <span className="muted"> ({session.reason})</span> : null}
      </h2>
      <p className="muted">
        Session <code data-testid="session-id">{session.session_id}</code>
      </p>
      <p className="muted" data-testid="session-stream">
        {streaming
          ? failed
            ? "Event stream lost; polling the durable log"
            : "Event stream connected"
          : "No event stream"}
      </p>
      <dl className="stats" data-testid="session-counters">
        <dt>Frames delivered</dt>
        <dd>{stats.frames_delivered}</dd>
        <dt>Frames processed</dt>
        <dd>{stats.frames_processed}</dd>
        <dt>Frames dropped</dt>
        <dd>{stats.frames_dropped}</dd>
        <dt>OCR calls</dt>
        <dd>{stats.ocr_calls}</dd>
        <dt>OCR skipped (hash)</dt>
        <dd>{stats.ocr_skipped_by_hash}</dd>
        <dt>Rankings</dt>
        <dd>{stats.rankings}</dd>
        <dt>Commits</dt>
        <dd>{stats.commits}</dd>
        <dt>Stale generation applied</dt>
        <dd>{stats.stale_generation_applied}</dd>
      </dl>
    </div>
  );
}
