import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useReducer, useRef, useState } from "react";
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
import {
  openEventStream,
  sessionsApi,
  type Session,
  type SessionAudioSettings,
  type UtteranceState,
} from "../api/sessions";
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
 * committed lines, the session controls and the live audio controls (mute,
 * volume, speech delay) over `POST /api/v1/sessions/{id}/audio` (docs/spec/07,
 * ADR-0039). The session id lives in the URL (`?session=`), so a refresh
 * reopens the same watch: the snapshot refetches and the event stream resumes
 * from the session's own `event_cursor`, which is the docs/spec/09 "session
 * survives a refresh" contract with the hub's Last-Event-ID replay doing the
 * history half.
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
  const queryClient = useQueryClient();
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

  // The applied audio snapshot: the /audio 200 body is the fresh session
  // snapshot, and installing it keeps the polling query from racing the
  // control that just landed (docs/spec/09: the snapshot is the truth).
  const setSessionSnapshot = (id: string, next: Session) => {
    queryClient.setQueryData(["session", id], next);
  };

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

      <AudioPanel
        sessionId={sessionId}
        audio={session.audio}
        onApplied={(next) => setSessionSnapshot(sessionId, next)}
      />
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

/**
 * The live audio controls (docs/spec/07, ADR-0039): mute, volume and the
 * speech delay over `POST /api/v1/sessions/{id}/audio`. The body is partial,
 * so each control posts only its own field; the 200 body is the fresh session
 * snapshot, which the parent installs as the query data — the snapshot is the
 * source of truth, and the local state re-syncs from it (a refresh or a
 * second operator may have moved the controls). A session the process cannot
 * give a voice pipeline renders the honest degraded note and inert controls:
 * the API answers 503 and a control that would do nothing is not a control.
 */
function AudioPanel({
  sessionId,
  audio,
  onApplied,
}: {
  sessionId: string;
  audio: Session["audio"];
  onApplied: (next: Session) => void;
}) {
  const { atLeast } = useAuth();
  const operator = atLeast("operator");
  const [muted, setMuted] = useState(audio?.muted ?? false);
  const [volumePct, setVolumePct] = useState(Math.round((audio?.volume ?? 1) * 100));
  const [delayMS, setDelayMS] = useState(audio?.speech_delay_ms ?? 0);

  // The apply sequence: each POST is tagged with the sequence it was issued
  // under, and only the most recently issued apply may install its snapshot.
  // A late acknowledgement of an earlier apply is by definition older than
  // the state the operator set in the meantime, and installing it would
  // show the control a value the server no longer holds (read-your-writes).
  const applySeq = useRef(0);

  const apply = useMutation({
    mutationFn: (body: SessionAudioSettings) => {
      applySeq.current += 1;
      const seq = applySeq.current;
      return sessionsApi.audio(sessionId, body).then((next) => ({ seq, next }));
    },
    onSuccess: ({ seq, next }) => {
      if (seq === applySeq.current) onApplied(next);
    },
  });

  // Re-sync from the snapshot — except while an apply is in flight. The 1s
  // snapshot poll can still be carrying a response the server built before
  // the apply landed; during that window the local value is the honest one,
  // and the apply's own fresh snapshot is what the controls re-sync from
  // when it is acknowledged. A refresh or a second operator is a different
  // story: their change is the server's truth, whatever the local state did.
  useEffect(() => {
    if (!audio || apply.isPending) return;
    setMuted(audio.muted);
    setVolumePct(Math.round(audio.volume * 100));
    setDelayMS(audio.speech_delay_ms);
  }, [audio, apply.isPending]);

  // The sliders fire on every detent; the server sees one apply per rest,
  // not one per pixel (the debounce is the operator's "short fade" half).
  // Changed fields accumulate into the window's body, so a rest between two
  // different controls posts both — the last field alone would swallow the
  // earlier one.
  const pending = useRef<SessionAudioSettings>({});
  const commitTimer = useRef<number | undefined>(undefined);
  const commit = (body: SessionAudioSettings) => {
    Object.assign(pending.current, body);
    window.clearTimeout(commitTimer.current);
    commitTimer.current = window.setTimeout(() => {
      const settled = pending.current;
      pending.current = {};
      apply.mutate(settled);
    }, 250);
  };
  useEffect(() => () => window.clearTimeout(commitTimer.current), []);

  // The controls stay operable while an apply is in flight — disabling the
  // slider mid-gesture would drop the detents the operator just pressed. The
  // in-flight window is shown instead ("Applying…") and the debounce carries
  // the pressed value to the next apply, so nothing is lost while it runs.
  const disabled = !operator || audio === undefined;
  const current = audio?.current_utterance;

  return (
    <section className="play-audio" aria-label="Audio output">
      <h2>Audio output</h2>
      {audio === undefined ? (
        <p className="muted" data-testid="play-audio-note">
          This process has no voice workflow wired (the audio API answers 503 audio_unavailable):
          the session runs silent, and the controls below stay inert until a voice is configured.
        </p>
      ) : (
        <p className="muted" data-testid="play-audio-note">
          Sink <code data-testid="play-audio-sink">{audio.sink}</code> — the controls apply live.
          Muting still speaks the events: the utterance ends with result&nbsp;“muted” instead of
          starting.
        </p>
      )}
      {apply.isPending ? (
        <p className="muted" aria-live="polite" data-testid="play-audio-pending">
          Applying the audio controls…
        </p>
      ) : null}
      <div className="audio-controls" data-testid="play-audio-controls">
        <button
          type="button"
          role="switch"
          aria-checked={muted}
          disabled={disabled}
          onClick={() => {
            const next = !muted;
            setMuted(next);
            apply.mutate({ muted: next });
          }}
          data-testid="play-mute"
        >
          {muted ? "Unmute" : "Mute"}
        </button>
        <label className="muted">
          Volume {volumePct}%
          <input
            type="range"
            min={0}
            max={100}
            value={volumePct}
            disabled={disabled}
            onChange={(event) => {
              const pct = Number(event.target.value);
              setVolumePct(pct);
              commit({ volume: pct / 100 });
            }}
            data-testid="play-volume"
          />
        </label>
        <label className="muted">
          Speech delay (ms)
          <input
            type="number"
            min={0}
            max={2000}
            value={delayMS}
            disabled={disabled}
            onChange={(event) => {
              const ms = Number(event.target.value);
              setDelayMS(ms);
              if (Number.isFinite(ms) && ms >= 0 && ms <= 2000) {
                commit({ speech_delay_ms: ms });
              }
            }}
            data-testid="play-speech-delay"
          />
        </label>
      </div>
      {apply.isError ? (
        <ErrorState error={apply.error} action="Cannot apply the audio controls" />
      ) : null}
      {audio ? (
        <dl className="facts" data-testid="play-audio-counters">
          <div>
            <dt>Requested</dt>
            <dd data-testid="play-speech-requested">{audio.speech.requested}</dd>
          </div>
          <div>
            <dt>Ready</dt>
            <dd data-testid="play-speech-ready">{audio.speech.ready}</dd>
          </div>
          <div>
            <dt>Started</dt>
            <dd data-testid="play-speech-started">{audio.speech.started}</dd>
          </div>
          <div>
            <dt>Cancelled</dt>
            <dd data-testid="play-speech-cancelled">{audio.speech.cancelled}</dd>
          </div>
          <div>
            <dt>Ended</dt>
            <dd data-testid="play-speech-ended">{audio.speech.ended}</dd>
          </div>
          <div>
            <dt>Cache hits</dt>
            <dd data-testid="play-speech-cache-hits">{audio.speech.cache_hits}</dd>
          </div>
          <div>
            <dt>Current utterance</dt>
            <dd data-testid="play-utterance-state">
              {current ? `${describeUtterance(current)}` : "none yet"}
            </dd>
          </div>
        </dl>
      ) : null}
    </section>
  );
}

/** The current-utterance state in one human line (the snapshot's shape). */
function describeUtterance(state: UtteranceState): string {
  const result = state.result ? ` (${state.result})` : "";
  return `${state.phase}${result} · generation ${state.generation}`;
}
