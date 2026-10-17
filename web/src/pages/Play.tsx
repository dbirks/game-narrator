import { useMutation, useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useReducer, useState } from "react";
import { Link, useSearchParams } from "react-router";

import {
  api,
  catalogApi,
  type Edition,
  type Game,
  type Profile,
  type Character,
} from "../api/client";
import { fetchReadiness } from "../api/system";
import { fetchAllMessages } from "../api/profiles";
import { openEventStream, sessionsApi } from "../api/sessions";
import { useAuth } from "../auth/auth-context";
import { EmptyState, ErrorState, Loading } from "../components/states";
import {
  currentLine,
  displaySpeaker,
  initialPlayState,
  reducePlayEvent,
  type CommittedLine,
} from "../lib/playReducer";

/**
 * `/play` — docs/spec/09: the active session view with its live state, the
 * committed lines and the session controls. The session id lives in the URL
 * (`?session=`), so a refresh reopens the same watch: the snapshot refetches
 * and the event stream resumes from the session's own `event_cursor`, which is
 * the docs/spec/09 "session survives a refresh" contract with the hub's
 * Last-Event-ID replay doing the history half.
 *
 * The audio controls are rendered disabled on purpose: this build has no
 * `/audio` API (the scope table reserves it, the routes do not exist), so a
 * control that would do nothing is not a control.
 */

const DEFAULT_REPLAY_DIR = "testdata/synthetic/replay/dialogue-advance-001";

type SessionMode = "conservative" | "predictive";
type SourceKind = "replay" | "test_pattern";

export function PlayPage() {
  const [params] = useSearchParams();
  const sessionId = params.get("session");
  return (
    <main>
      <h1 data-testid="play-title">Play</h1>
      {sessionId ? <LiveSession sessionId={sessionId} /> : <SetupForm />}
    </main>
  );
}

/** One runnable profile: a game, its edition, the revision it published. */
interface RunOption {
  game: Game;
  edition: Edition;
  profile: Profile;
}

function runLabel(option: RunOption): string {
  const { game, edition, profile } = option;
  return `${game.title} · ${edition.region}/${edition.language} · r${profile.revision_num}`;
}

/** Every published revision the appliance could run, for the start form. */
async function fetchRunOptions(signal?: AbortSignal): Promise<RunOption[]> {
  const gamesPage = (await catalogApi.listGames({ limit: 200 }, signal)) as unknown as {
    games: Game[];
  };
  const options: RunOption[] = [];
  for (const game of gamesPage.games ?? []) {
    const editionsPage = (await catalogApi.listEditions(game.id, signal)) as unknown as {
      editions: Edition[];
    };
    for (const edition of editionsPage.editions ?? []) {
      let profile: Profile | null = null;
      try {
        profile = (await api.get("/api/v1/editions/{id}/published", {
          params: { id: edition.id },
          signal,
        })) as unknown as Profile;
      } catch {
        continue; // a 404 here means "nothing published", not a fault
      }
      if (profile) options.push({ game, edition, profile });
    }
  }
  return options;
}

function SetupForm() {
  const { atLeast } = useAuth();
  const [, setParams] = useSearchParams();
  const runs = useQuery({
    queryKey: ["play", "run-options"],
    queryFn: ({ signal }) => fetchRunOptions(signal),
  });
  const [profileId, setProfileId] = useState("");
  const [sourceKind, setSourceKind] = useState<SourceKind>("replay");
  const [replayDir, setReplayDir] = useState(DEFAULT_REPLAY_DIR);
  const [mode, setMode] = useState<SessionMode>("conservative");
  const [startError, setStartError] = useState<unknown>(null);

  const operator = atLeast("operator");

  const start = useMutation({
    mutationFn: () =>
      sessionsApi.start({
        profile_id: profileId,
        frame_source:
          sourceKind === "replay" ? { kind: "replay", dir: replayDir } : { kind: "test_pattern" },
        mode,
        dev_trace: false,
      }),
    onSuccess: (session) => setParams({ session: session.session_id }),
    onError: (error) => setStartError(error),
  });

  return (
    <section className="panel" data-testid="play-setup">
      <p className="muted">
        Pick the published revision to run and where the frames come from. One session at a time is
        a runtime invariant, not a UI limit.
      </p>
      {runs.isPending ? <Loading label="Finding published profiles…" /> : null}
      {runs.isError ? (
        <ErrorState
          error={runs.error}
          action="Cannot list published profiles"
          onRetry={() => void runs.refetch()}
        />
      ) : null}
      {runs.data && runs.data.length === 0 ? (
        <EmptyState title="Nothing is published">
          <p>
            A session runs an edition&apos;s published revision. Curate a profile and publish it
            from <Link to="/library">the library</Link> first.
          </p>
        </EmptyState>
      ) : null}
      {runs.data && runs.data.length > 0 ? (
        <form
          className="stack"
          onSubmit={(event) => {
            event.preventDefault();
            start.mutate();
          }}
          data-testid="play-start-form"
        >
          <label>
            Published profile
            <select
              value={profileId}
              onChange={(event) => setProfileId(event.target.value)}
              data-testid="play-profile"
            >
              <option value="">choose a published revision…</option>
              {runs.data.map((option) => (
                <option key={option.profile.id} value={option.profile.id}>
                  {runLabel(option)}
                </option>
              ))}
            </select>
          </label>
          <label>
            Frame source
            <select
              value={sourceKind}
              onChange={(event) => setSourceKind(event.target.value as SourceKind)}
              data-testid="play-source-kind"
            >
              <option value="replay">replay clip</option>
              <option value="test_pattern">test pattern</option>
            </select>
          </label>
          {sourceKind === "replay" ? (
            <label>
              Replay directory
              <input
                type="text"
                value={replayDir}
                onChange={(event) => setReplayDir(event.target.value)}
                data-testid="play-replay-dir"
              />
              <span className="muted">
                Must sit under the configured replay allowlist; the dev checkout admits its
                testdata.
              </span>
            </label>
          ) : null}
          <label>
            Mode
            <select
              value={mode}
              onChange={(event) => setMode(event.target.value as SessionMode)}
              data-testid="play-mode"
            >
              <option value="conservative">conservative</option>
              <option value="predictive">predictive</option>
            </select>
          </label>
          {startError ? <ErrorState error={startError} action="Cannot start the session" /> : null}
          <p>
            <button
              type="submit"
              disabled={!operator || start.isPending || !profileId}
              data-testid="play-start"
            >
              {start.isPending ? "Starting…" : "Start session"}
            </button>
            {!operator ? (
              <span className="muted" data-testid="play-role-note">
                Starting or stopping a session needs the operator role (the server enforces it).
              </span>
            ) : null}
          </p>
        </form>
      ) : null}
    </section>
  );
}

function LiveSession({ sessionId }: { sessionId: string }) {
  const { atLeast } = useAuth();
  const operator = atLeast("operator");
  const sessionQuery = useQuery({
    queryKey: ["session", sessionId],
    queryFn: ({ signal }) => sessionsApi.get(sessionId, signal),
    refetchInterval: (query) => {
      const state = query.state.data?.state;
      return state === "starting" || state === "running" ? 1000 : false;
    },
  });
  const session = sessionQuery.data;
  const terminal = session?.state === "stopped" || session?.state === "failed";

  const [events, dispatch] = useReducer(reducePlayEvent, undefined, initialPlayState);
  const [streamFailed, setStreamFailed] = useState(false);

  // The event stream resumes from the session's own cursor, so a refresh sees
  // its own history — including a session that already settled: the hub replays
  // the run's events from the cursor, and a finished session simply stops
  // producing new ones. (A terminal state must not gate the open, or the
  // refresh case of an ended run would show no committed lines at all.)
  const cursor = session?.event_cursor;
  useEffect(() => {
    if (cursor === undefined) return;
    const controller = new AbortController();
    const close = openEventStream(
      `/api/v1/events?session_id=${encodeURIComponent(sessionId)}`,
      {
        onEvent: (envelope) => dispatch(envelope),
        onGiveUp: () => setStreamFailed(true),
      },
      controller.signal,
      String(cursor),
    );
    return () => {
      close();
      controller.abort();
    };
  }, [sessionId, cursor]);

  // A resync notice means the replay could not cover our cursor: the snapshot
  // is the reconciliation truth, so refetch it.
  useEffect(() => {
    if (!events.resyncRequired) return;
    void sessionQuery.refetch();
  }, [events.resyncRequired, sessionQuery]);

  // The committed line's assigned speaker: message → speaker_id → character name.
  const profileId = session?.profile_id;
  const messagesQuery = useQuery({
    queryKey: ["play", "messages", profileId],
    queryFn: ({ signal }) => fetchAllMessages(profileId as string, signal),
    enabled: Boolean(profileId),
  });
  const charactersQuery = useQuery({
    queryKey: ["characters", profileId],
    queryFn: ({ signal }) =>
      api.get("/api/v1/profiles/{id}/characters", {
        params: { id: profileId as string },
        signal,
      }) as Promise<{ characters: Character[] }>,
    enabled: Boolean(profileId),
  });
  const messageSpeaker = useMemo(() => {
    const names = new Map((charactersQuery.data?.characters ?? []).map((c) => [c.id, c.name]));
    const byMessage = new Map<string, string>();
    for (const message of messagesQuery.data ?? []) {
      if (message.speaker_id) byMessage.set(message.id, names.get(message.speaker_id) ?? "");
    }
    return byMessage;
  }, [messagesQuery.data, charactersQuery.data]);

  const readiness = useQuery({
    queryKey: ["health", "ready"],
    queryFn: ({ signal }) => fetchReadiness(signal),
    refetchInterval: 10_000,
  });

  const stop = useMutation({
    mutationFn: () => sessionsApi.stop(sessionId),
    onSuccess: () => void sessionQuery.refetch(),
  });

  if (sessionQuery.isPending) return <Loading label="Opening the session…" />;
  if (sessionQuery.isError) {
    return (
      <ErrorState
        error={sessionQuery.error}
        action="Cannot open this session"
        onRetry={() => void sessionQuery.refetch()}
      />
    );
  }
  if (!session) return <Loading label="Opening the session…" />;

  const line = currentLine(events);
  const stats = session.stats;
  return (
    <section className="panel" data-testid="play-live">
      <header className="play-head">
        <span className={`badge badge-${session.state}`} data-testid="play-state">
          {session.state}
        </span>
        <span className="muted">
          session <code data-testid="play-session-id">{session.session_id}</code>
        </span>
        {terminal ? (
          <span className="muted">
            {session.reason ? `reason: ${session.reason}` : null}
            {session.ended_at ? ` · ended ${session.ended_at}` : null}
          </span>
        ) : (
          <button
            type="button"
            onClick={() => stop.mutate()}
            disabled={!operator || stop.isPending}
            data-testid="play-stop"
          >
            {stop.isPending ? "Stopping…" : "Stop"}
          </button>
        )}
        <Link to="/play" className="muted">
          Close this watch
        </Link>
      </header>

      <dl className="facts" data-testid="play-status">
        <div>
          <dt>Mode</dt>
          <dd>{session.mode}</dd>
        </div>
        <div>
          <dt>Frame source</dt>
          <dd>{session.frame_source}</dd>
        </div>
        <div>
          <dt>Frames</dt>
          <dd data-testid="play-frames">
            {stats.frames_delivered} delivered · {stats.frames_processed} processed ·{" "}
            {stats.frames_dropped} dropped
          </dd>
        </div>
        <div>
          <dt>OCR calls</dt>
          <dd data-testid="play-ocr">{stats.ocr_calls}</dd>
        </div>
        <div>
          <dt>Commits</dt>
          <dd data-testid="play-commits">{stats.commits}</dd>
        </div>
        <div>
          <dt>Appliance readiness</dt>
          <dd data-testid="play-health">
            {readiness.data ? `${readiness.data.status}` : "checking…"}
            {readiness.data?.checks?.database
              ? ` (database ${readiness.data.checks.database.status ?? "unknown"})`
              : ""}
            <span className="muted"> · model-role health is not exposed by this build</span>
          </dd>
        </div>
      </dl>
      {streamFailed ? (
        <p className="error" data-testid="play-stream-failed" role="alert">
          The live event stream is down; the counters above keep polling and the committed lines
          resume when the stream does.
        </p>
      ) : null}
      {stop.isError ? <ErrorState error={stop.error} action="Cannot stop the session" /> : null}

      <section className="play-line" aria-label="Current committed line">
        <h2>Current line</h2>
        {line ? (
          <div data-testid="current-line">
            <p data-testid="current-line-text">{line.text}</p>
            <p className="muted">
              speaker:{" "}
              <span data-testid="current-line-speaker">{displaySpeaker(line, messageSpeaker)}</span>
              {" · "}message: <code data-testid="current-line-message-id">{line.messageId}</code>
              {line.speechEligible ? "" : " · not speech-eligible"}
            </p>
          </div>
        ) : (
          <p className="muted">Nothing committed yet.</p>
        )}
      </section>

      <section className="play-lines" aria-label="Recently committed lines">
        <h2>Recent lines</h2>
        {events.lines.length === 0 ? (
          <p className="muted">No commits yet.</p>
        ) : (
          <ol data-testid="recent-lines" className="recent-lines">
            {events.lines.map((committed, index) => (
              <LineRow
                key={committed.occurrenceId}
                line={committed}
                index={index}
                speaker={displaySpeaker(committed, messageSpeaker)}
              />
            ))}
          </ol>
        )}
      </section>

      <section className="play-audio" aria-label="Audio output">
        <h2>Audio output</h2>
        <p className="muted" data-testid="play-audio-note">
          Audio output wiring lands next: this build has no audio API, so playback runs on the
          appliance&apos;s own audio path and these controls are placeholders that do nothing.
        </p>
        <div className="audio-controls" data-testid="play-audio-controls">
          <button type="button" disabled data-testid="play-mute">
            Mute
          </button>
          <label className="muted">
            Volume
            <input
              type="range"
              min={0}
              max={100}
              defaultValue={100}
              disabled
              data-testid="play-volume"
            />
          </label>
          <label className="muted">
            Speech delay
            <input type="number" value={0} disabled data-testid="play-speech-delay" />
          </label>
        </div>
      </section>
    </section>
  );
}

function LineRow({
  line,
  index,
  speaker,
}: {
  line: CommittedLine;
  index: number;
  speaker: string;
}) {
  return (
    <li className="recent-line" data-testid={`recent-line-${index}`}>
      <span className="recent-line-text">{line.text}</span>
      <span className="muted">
        {speaker} · <code>{line.messageId}</code> · {line.occurredAt}
      </span>
    </li>
  );
}
