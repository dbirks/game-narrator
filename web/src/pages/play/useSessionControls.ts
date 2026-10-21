/**
 * `src/pages/play/useSessionControls.ts` — the one owner of the live
 * session's operator controls (docs/spec/08 "…/pause, …/resume, …/settings",
 * docs/spec/07 audio, ADR-0039/0075): pause/resume, the decision mode
 * (careful/eager), the narrated classes, the unmatched read-through policy
 * (ADR-0136), the live audio controls and the operator's corrections.
 *
 * Both Play views (now-speaking and "show workings") and the kid mode render
 * from this single hook, so switching views cannot restart or duplicate
 * speech (ADR-0093 open question (b)): the session is started and stopped
 * exactly once, and every control lands on the session's single writer. The
 * 200 body of each apply is the fresh session snapshot, which the hook
 * installs through `onApplied` — the snapshot is the source of truth, so the
 * controls show what the server holds, not the click the operator made.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { useMutation } from "@tanstack/react-query";

import { ApiError } from "../../api/client";
import { recordCorrection, type Correction } from "../../api/corrections";
import {
  sessionsApi,
  type Session,
  type SessionAudioSettings,
  type SessionSettings,
} from "../../api/sessions";
import type { CommittedLine } from "../../lib/playReducer";

export type DecisionMode = "conservative" | "predictive";

export interface SessionControlsValue {
  paused: boolean;
  pausePending: boolean;
  togglePause: () => void;
  mode: DecisionMode;
  settingsPending: boolean;
  setMode: (mode: DecisionMode) => void;
  classes: string[];
  toggleClass: (cls: "sign" | "item") => void;
  /** The session's unmatched read-through policy (ADR-0136): the snapshot's
   *  applied state, default ON (a session before its first snapshot reads
   *  the server's documented default). */
  readThrough: boolean;
  toggleReadThrough: () => void;
  muted: boolean;
  volumePct: number;
  delayMs: number;
  audioPending: boolean;
  setMuted: (next: boolean) => void;
  pressVolume: (pct: number) => void;
  setDelayMs: (ms: number) => void;
  /** The last recorded correction label (its row id) for the result line. */
  correction: Correction | null;
  correctionPending: boolean;
  correctionError: string | null;
  /** Record Wrong speaker / Wrong text for the committed line (curator). */
  correct: (kind: "speaker" | "text") => void;
  /** The session's one Stop (idempotent; the 200 body is the fresh snapshot). */
  stop: () => void;
  stopPending: boolean;
  stopError: unknown;
}

interface Options {
  sessionId: string;
  /** The live snapshot the controls render from (the first poll's answer
   *  arrives after mount; until then the controls render at their defaults). */
  session: Session | undefined;
  /** The committed line a correction is about (null: the buttons stay inert). */
  line: CommittedLine | null;
  /** The line's displayed speaker name (the label's before_value). */
  speakerLabel: string;
  /** The line's profile, named by the correction label's profile_id. */
  profileId: string | undefined;
  onApplied: (next: Session) => void;
}

export function useSessionControls(options: Options): SessionControlsValue {
  const { sessionId, session, line, speakerLabel, profileId, onApplied } = options;
  const applySeq = useRef(0);
  const [pausedLocal, setPausedLocal] = useState(session?.paused ?? false);

  // The applied snapshot re-syncs the local control state (a refresh or a
  // second operator is the server's truth, whatever the local state did).
  useEffect(() => {
    setPausedLocal(session?.paused ?? false);
  }, [session]);

  const pauseResume = useMutation({
    mutationFn: () =>
      (session?.paused ?? false) ? sessionsApi.resume(sessionId) : sessionsApi.pause(sessionId),
    onSuccess: (next) => {
      setPausedLocal(next.paused ?? false);
      onApplied(next);
    },
  });

  const settings = useMutation({
    mutationFn: (body: SessionSettings) => {
      applySeq.current += 1;
      const seq = applySeq.current;
      return sessionsApi.settings(sessionId, body).then((next) => ({ seq, next }));
    },
    onSuccess: ({ seq, next }) => {
      if (seq === applySeq.current) onApplied(next);
    },
  });

  const classes = session?.narrated_classes ?? [];
  const toggleClass = useCallback(
    (cls: "sign" | "item") => {
      const signs = classes.includes("sign");
      const items = classes.includes("item");
      const set: SessionSettings["narrated_classes"] = ["dialogue"];
      if (cls === "sign" ? !signs : signs) set.push("sign");
      if (cls === "item" ? !items : items) set.push("item");
      settings.mutate({ narrated_classes: set });
    },
    [classes, settings],
  );

  // The unmatched read-through (ADR-0136): a session policy like the
  // narrated classes — the snapshot carries the applied state (the server's
  // default is ON), and the switch moves it on the settings route. The
  // switch decides what happens with lines committed next; the applied
  // snapshot re-syncs the checkbox from the 200 body the way the class
  // toggles re-sync from theirs.
  const readThrough = session?.unmatched_read_through ?? true;
  const toggleReadThrough = useCallback(() => {
    settings.mutate({ unmatched_read_through: !readThrough });
  }, [readThrough, settings]);

  // ── the live audio controls (docs/spec/07, ADR-0039) ─────────────────────
  const [muted, setMutedState] = useState(session?.audio?.muted ?? false);
  const [volumePct, setVolumePctState] = useState(Math.round((session?.audio?.volume ?? 1) * 100));
  const [delayMS, setDelayMSState] = useState(session?.audio?.speech_delay_ms ?? 0);

  const audio = useMutation({
    mutationFn: (body: SessionAudioSettings) => {
      applySeq.current += 1;
      const seq = applySeq.current;
      return sessionsApi.audio(sessionId, body).then((next) => ({ seq, next }));
    },
    onSuccess: ({ seq, next }) => {
      if (seq !== applySeq.current) return;
      setMutedState(next.audio?.muted ?? false);
      setVolumePctState(Math.round((next.audio?.volume ?? 1) * 100));
      setDelayMSState(next.audio?.speech_delay_ms ?? 0);
      onApplied(next);
    },
  });

  // Re-sync from the snapshot — except while an apply is in flight (the 1s
  // poll can still carry a response built before the apply landed; the
  // apply's own fresh snapshot is what the controls re-sync from).
  useEffect(() => {
    const audioState = session?.audio;
    if (!audioState || audio.isPending) return;
    setMutedState(audioState.muted);
    setVolumePctState(Math.round(audioState.volume * 100));
    setDelayMSState(audioState.speech_delay_ms);
  }, [session?.audio, audio.isPending]);

  // The sliders fire on every detent; the server sees one apply per rest.
  // Changed fields accumulate into the window's body, so a rest between two
  // different controls posts both — the last field alone would swallow the
  // earlier one.
  const pending = useRef<SessionAudioSettings>({});
  const commitTimer = useRef<number | undefined>(undefined);
  const commit = useCallback(
    (body: SessionAudioSettings) => {
      Object.assign(pending.current, body);
      window.clearTimeout(commitTimer.current);
      commitTimer.current = window.setTimeout(() => {
        const settled = pending.current;
        pending.current = {};
        audio.mutate(settled);
      }, 250);
    },
    [audio],
  );
  useEffect(() => () => window.clearTimeout(commitTimer.current), []);

  // ── the operator's corrections (ADR-0052: a label, never a fix) ──────────
  const [correction, setCorrection] = useState<Correction | null>(null);
  const [correctionError, setCorrectionError] = useState<string | null>(null);
  const correct = useMutation({
    mutationFn: (kind: "speaker" | "text") => {
      if (!line || !profileId) throw new Error("no committed line to correct");
      return recordCorrection({
        profile_id: profileId,
        kind,
        target_type: "message",
        target_id: line.messageId,
        message_id: line.messageId,
        before_value: kind === "speaker" ? speakerLabel : line.text,
        reason:
          kind === "speaker"
            ? "operator: wrong speaker (Play, ADR-0093 2e/2f)"
            : "operator: wrong text (Play, ADR-0093 2e/2f)",
        evidence: [
          `session:${sessionId}`,
          `occurrence:${line.occurrenceId}`,
          `event:${line.eventId}`,
        ],
      });
    },
    onSuccess: (result) => {
      setCorrection(result.correction);
      setCorrectionError(null);
    },
    onError: (error: unknown) => {
      setCorrection(null);
      setCorrectionError(
        error instanceof ApiError
          ? `${error.status} ${error.detail}${error.requestId ? ` · ${error.requestId}` : ""}`
          : String(error),
      );
    },
  });

  // ── the session's one Stop (idempotent; 200 = the terminal snapshot) ────
  const stopSession = useMutation({
    mutationFn: () => sessionsApi.stop(sessionId),
    onSuccess: (next) => onApplied(next),
  });

  return {
    paused: pausedLocal,
    pausePending: pauseResume.isPending,
    togglePause: () => pauseResume.mutate(),
    mode: session?.mode ?? "conservative",
    settingsPending: settings.isPending,
    setMode: (mode) => settings.mutate({ mode }),
    classes,
    toggleClass,
    readThrough,
    toggleReadThrough,
    muted,
    volumePct,
    delayMs: delayMS,
    audioPending: audio.isPending,
    setMuted: (next) => {
      setMutedState(next);
      audio.mutate({ muted: next });
    },
    pressVolume: (pct) => {
      setVolumePctState(pct);
      commit({ volume: pct / 100 });
    },
    setDelayMs: (ms) => {
      if (!Number.isFinite(ms) || ms < 0 || ms > 2000) return;
      setDelayMSState(ms);
      commit({ speech_delay_ms: ms });
    },
    correction,
    correctionPending: correct.isPending,
    correctionError,
    correct: (kind) => correct.mutate(kind),
    stop: () => stopSession.mutate(),
    stopPending: stopSession.isPending,
    stopError: stopSession.error,
  };
}
