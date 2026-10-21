/**
 * `src/pages/play/SessionFacts.tsx` — the session's facts on the Play page:
 * the status table (mode, frame source + its state/error, frames, OCR calls,
 * commits, appliance readiness) and the audio output card (the sink note and
 * the speech counters).
 *
 * Both are the pre-redesign blocks, testid-for-testid (play-status,
 * play-source, play-source-state, play-source-error, play-capture-device-info,
 * play-frames, play-ocr, play-commits, play-health, play-audio-note,
 * play-audio-sink, play-audio-counters, play-speech-*, play-utterance-state):
 * the m5-part1 / capture-device / sse-reconnect specs read them, and ADR-0093
 * keeps the e2e selector contract while the view around them changes.
 *
 * Since the 2026-10-20 review (against mocks 2e/2f) both blocks render in
 * the operator ("show workings") view only — the caption view is the clean
 * 2e surface (portrait, line, waveform, last three lines, the bar) — so
 * every reader of these testids reads them from the operator view.
 */
import type { Readiness } from "../../api/system";
import type { Session, SessionAudio, UtteranceState } from "../../api/sessions";

export function SessionStatus({
  session,
  readiness,
}: {
  session: Session;
  readiness: Readiness | undefined;
}) {
  const stats = session.stats;
  return (
    <dl className="facts pp-status" data-testid="play-status">
      <div>
        <dt>Mode</dt>
        <dd>{session.mode}</dd>
      </div>
      <div>
        <dt>Frame source</dt>
        <dd data-testid="play-source">
          {session.frame_source}
          {session.capture_device ? (
            <span data-testid="play-capture-device-info">
              {" "}
              · device {session.capture_device}
              {session.capture_mode
                ? ` (${session.capture_mode} — ${session.capture_mode_source})`
                : ""}
            </span>
          ) : null}
          {session.source ? (
            <span data-testid="play-source-state"> · {session.source.state}</span>
          ) : null}
          {session.source?.last_error ? (
            <span className="error" data-testid="play-source-error" role="alert">
              {" · "}
              {session.source.last_error}
            </span>
          ) : null}
        </dd>
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
          {readiness ? `${readiness.status}` : "checking…"}
          {readiness?.checks?.database
            ? ` (database ${readiness.checks.database.status ?? "unknown"})`
            : ""}
          <span className="muted"> · model-role health is not exposed by this build</span>
        </dd>
      </div>
    </dl>
  );
}

/**
 * The audio output card: what the sink is, the note when nothing is wired,
 * and the speech counters (the "did we speak, and why not" evidence the
 * specs assert). The controls themselves live in the 2e control bar — this
 * card reports, it does not steer.
 */
export function AudioCard({
  audio,
  pending,
}: {
  audio: SessionAudio | undefined;
  pending: boolean;
}) {
  const current = audio?.current_utterance;
  return (
    <section
      className="kit-card pp-audio-card"
      aria-label="Audio output"
      data-testid="play-audio-card"
    >
      <h2 className="kit-card-title">Audio output</h2>
      {audio === undefined ? (
        <p className="muted" data-testid="play-audio-note">
          This process has no voice workflow wired (the audio API answers 503 audio_unavailable):
          the session runs silent, and the controls stay inert until a voice is configured.
        </p>
      ) : (
        <p className="muted" data-testid="play-audio-note">
          Sink <code data-testid="play-audio-sink">{audio.sink}</code> — the controls apply live.
          Muting still speaks the events: the utterance ends with result&nbsp;“muted” instead of
          starting.
        </p>
      )}
      {pending ? (
        <p className="muted" aria-live="polite" data-testid="play-audio-pending">
          Applying the audio controls…
        </p>
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
              {current ? describeUtterance(current) : "none yet"}
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
