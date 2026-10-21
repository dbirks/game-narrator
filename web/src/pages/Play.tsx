/**
 * `src/pages/Play.tsx` — the operator's Play page (ADR-0093 / IA): the
 * session-start form, then one session with three views over it —
 * now-speaking (2e, the default), the operator "show workings" view (2f,
 * frame + event stream + candidates + corrections) and the kid/tablet mode
 * (2g, `?mode=kid`: −/pause/+ and nothing else).
 *
 * ADR-0093's single-session rework lives here, not in a new page: the
 * session's state, its event fold (playReducer for the lines, playEvents
 * for the stream) and its controls (useSessionControls) are owned once,
 * so toggling the "show workings" view swaps what is rendered without
 * restarting or duplicating speech (plan.md Phase 1 item 6, acceptance).
 * The event stream is the session's SSE with the hub's Last-Event-ID replay
 * (docs/spec/09: the session survives a refresh), and the old page's
 * keyboard contract survives: p = pause, s = signs, i = items, m = mode.
 */
import { useEffect, useMemo, useReducer, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router";

import { api, type Character } from "../api/client";
import { fetchReadiness } from "../api/system";
import { fetchAllMessages } from "../api/profiles";
import { openEventStream, sessionsApi } from "../api/sessions";
import { useAuth } from "../auth/auth-context";
import { ErrorState, Loading } from "../components/states";
import { PillButton } from "../components/kit/PillButton";
import { currentLine, displaySpeaker, initialPlayState, reducePlayEvent } from "../lib/playReducer";
import { initialPlayEventStream, reducePlayEventStream } from "../lib/playEvents";
import { KidMode } from "./play/KidMode";
import { NowSpeaking } from "./play/NowSpeaking";
import { OperatorView } from "./play/OperatorView";
import { ControlBar } from "./play/ControlBar";
import { SetupForm } from "./play/SetupForm";
import { useSessionControls } from "./play/useSessionControls";

export function PlayPage() {
  const [params] = useSearchParams();
  const sessionId = params.get("session");
  const kid = params.get("mode") === "kid";
  if (!sessionId) return <SetupForm />;
  return <LiveSession sessionId={sessionId} kid={kid} />;
}

function LiveSession({ sessionId, kid }: { sessionId: string; kid: boolean }) {
  const { atLeast } = useAuth();
  const operator = atLeast("operator");
  const queryClient = useQueryClient();

  const sessionQuery = useQuery({
    queryKey: ["session", sessionId],
    queryFn: ({ signal }) => sessionsApi.get(sessionId, signal),
    // Starting sessions move fast; poll the snapshot while one is in flight.
    refetchInterval: (query) => {
      const state = query.state.data?.state;
      return state === "starting" || state === "running" ? 1000 : false;
    },
    refetchOnWindowFocus: true,
  });
  const session = sessionQuery.data;
  const terminal = session?.state === "stopped" || session?.state === "failed";

  // Two folds over one event feed: the lines (the now-speaking view) and
  // the recent-event stream (the operator view). One SSE subscription feeds
  // both; the view swap therefore cannot duplicate speech.
  const [lines, dispatch] = useReducer(reducePlayEvent, undefined, initialPlayState);
  const [stream, streamDispatch] = useReducer(
    reducePlayEventStream,
    undefined,
    initialPlayEventStream,
  );
  const [streamFailed, setStreamFailed] = useState(false);
  const [workings, setWorkings] = useState(false);

  // The event stream resumes from the session's own cursor, so a refresh sees
  // its own history — including a session that already settled: the hub
  // replays the run's events from the cursor, and a finished session simply
  // stops producing new ones. (A terminal state must not gate the open, or
  // the refresh case of an ended run would show no committed lines at all.)
  const cursor = session?.event_cursor;
  useEffect(() => {
    if (cursor === undefined) return;
    const controller = new AbortController();
    const close = openEventStream(
      `/api/v1/events?session_id=${encodeURIComponent(sessionId)}`,
      {
        onEvent: (envelope) => {
          dispatch(envelope);
          streamDispatch(envelope);
          // A session-level event (paused / resumed / stopped) refreshes the
          // session snapshot straight away: the header's state chip and the
          // view's terminal block read the snapshot's state, and the 1s poll
          // alone can lag the run's tail. The high-rate frame and commit
          // events are not invalidated for — they change the line folds, not
          // the snapshot — so the bar does not re-render on every event.
          if (envelope.name.startsWith("session.")) {
            void queryClient.invalidateQueries({ queryKey: ["session", sessionId] });
          }
        },
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
  // is the reconciliation truth, so refetch it (a fresh event_cursor restarts
  // the stream at the hub's head).
  useEffect(() => {
    if (!lines.resyncRequired) return;
    void sessionQuery.refetch();
  }, [lines.resyncRequired, sessionQuery]);

  // The applied snapshot re-sync: the 200 bodies are the fresh session, and
  // installing them keeps the polling query from racing the control.
  const onApplied = (snapshot: NonNullable<typeof session>) => {
    queryClient.setQueryData(["session", sessionId], snapshot);
  };
  // The revision's speakers: committed lines address the profile's message
  // ids, and the speaker chip shows the roster's name, not the id.
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

  const line = currentLine(lines);
  const speakerLabel = line ? displaySpeaker(line, messageSpeaker) : "…";
  // The operator view's candidate rows name the line, not its store ULID
  // (the 2026-10-20 review): the snippet map is the profile's own messages
  // (the same read the speaker chips use), keyed by the store's message id.
  const messageText = useMemo(() => {
    const byId = new Map<string, string>();
    for (const message of messagesQuery.data ?? []) {
      byId.set(message.id, message.speech_text || message.normalized_text || message.original_text);
    }
    return byId;
  }, [messagesQuery.data]);
  // The correction labels the line as it is on screen — a curation act the
  // API owns (POST /corrections is session-agnostic: the label targets the
  // line's message row, and UX-05 reaches labels "from Play or a trace",
  // settled runs included). The views gate the button on the line itself
  // (nothing to label, nothing offered); the API answers otherwise.
  const canCorrect = operator;

  const controls = useSessionControls({
    sessionId,
    session,
    line,
    speakerLabel,
    profileId: profileId as string | undefined,
    onApplied,
  });

  // Keyboard: p = pause, s = signs, i = items, m = decision mode. Guarded
  // against typing in a control and against the terminal (the old contract).
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      const target = event.target;
      if (target instanceof HTMLElement && ["INPUT", "SELECT", "TEXTAREA"].includes(target.tagName))
        return;
      if (terminal) return;
      if (event.key === "p") controls.togglePause();
      else if (event.key === "s") controls.toggleClass("sign");
      else if (event.key === "i") controls.toggleClass("item");
      else if (event.key === "m")
        controls.setMode(controls.mode === "predictive" ? "conservative" : "predictive");
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [controls, terminal]);

  if (sessionQuery.isPending || !session)
    return (
      <main data-testid="play-page">
        <Loading label="Opening the session…" />
      </main>
    );

  return (
    <main data-testid="play-page">
      {sessionQuery.isError ? (
        <ErrorState
          error={sessionQuery.error}
          action="Cannot open this session"
          onRetry={() => void sessionQuery.refetch()}
        />
      ) : null}

      <header className="pp-session-head" data-testid="play-session-head">
        <span className={`badge badge-${session.state}`} data-testid="play-state">
          {session.state}
        </span>
        <span className="muted pp-head-meta">
          session <code data-testid="play-session-id">{session.session_id}</code>
        </span>
        {/* The view toggle and the watch's exit sit as one spaced action
            group (the review's fix for the run-together links): the toggle
            is a pill, the exit stays a quiet link, and the group's gap and
            right alignment keep them apart at every width. */}
        <span className="pp-head-actions">
          {!kid ? (
            <PillButton
              variant="outline"
              aria-pressed={workings}
              onClick={() => setWorkings((value) => !value)}
              data-testid="play-show-workings"
            >
              {workings ? "hide workings" : "show workings"}
            </PillButton>
          ) : null}
          <Link to="/play" className="muted" data-testid="play-close-watch">
            Close this watch
          </Link>
        </span>
      </header>

      <section className="panel pp-live" data-testid="play-live">
        {kid ? (
          <KidMode speakerLabel={speakerLabel} line={line} controls={controls} />
        ) : (
          <>
            {workings ? (
              <OperatorView
                line={line}
                speakerLabel={speakerLabel}
                stream={stream}
                controls={controls}
                operator={operator}
                terminal={terminal}
                canCorrect={canCorrect}
                session={session}
                readiness={readiness.data}
                streamFailed={streamFailed}
                messageText={messageText}
              />
            ) : (
              <NowSpeaking
                line={line}
                speakerLabel={speakerLabel}
                lines={lines.lines}
                speakerOf={(committed) => displaySpeaker(committed, messageSpeaker)}
              />
            )}
            {/* The bottom control bar: one bar along the bottom of the page
                over both views (the design's 2e/2f merge — pause · mute ·
                volume · delay · careful/eager · wrong speaker? · stop),
                so the operator calibrating in "show workings" reaches the
                same session's controls as the caption view. */}
            <ControlBar
              controls={controls}
              operator={operator}
              terminal={terminal}
              canCorrect={canCorrect}
              hasLine={line !== null && line.messageId !== "unknown"}
              speakerUnknown={line !== null && !line.speakerKnown && speakerLabel === "unknown"}
              view={workings ? "operator" : "now"}
            />
          </>
        )}
      </section>

      {controls.stopError ? (
        <ErrorState error={controls.stopError} action="Stop did not complete" />
      ) : null}
    </main>
  );
}
