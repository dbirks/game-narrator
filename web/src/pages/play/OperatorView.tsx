/**
 * `src/pages/play/OperatorView.tsx` — the Play page's operator view
 * (docs/design/plum-peach 2f, ADR-0093): the 4:3 live frame with its ROI
 * overlays (OperatorFrame), the committed line beneath it, the event stream
 * table (time · kind · detail, the newest row peach), the match candidates
 * with their probability bars, the session's own facts (the stats block —
 * mode, frame source, counters, end reason — and the audio output card,
 * both moved here from the default view by the 2026-10-20 review against
 * mocks 2e/2f) and the correction buttons (Wrong speaker · Wrong text ·
 * Stop) that record the ADR-0052 label and stop the session.
 *
 * The view is presentational over the session state LiveSession owns: the
 * event table and the candidates fold the same stream the now-speaking view
 * renders (playReducer + playEvents over one event feed), so the
 * "show workings" toggle swaps views without restarting or duplicating
 * speech (plan.md Phase 1 item 6, acceptance).
 */
import { Link } from "react-router";

import { Card } from "../../components/kit/Card";
import { Chip } from "../../components/kit/Chip";
import { NameTab } from "../../components/kit/NameTab";
import { PillButton } from "../../components/kit/PillButton";
import { ProgressBar } from "../../components/kit/ProgressBar";
import type { Readiness } from "../../api/system";
import type { Session } from "../../api/sessions";
import { AudioCard, SessionStatus } from "./SessionFacts";
import { OperatorFrame } from "./OperatorFrame";
import { clockTime, lineSnippet, probabilityText } from "./shared";
import type { SessionControlsValue } from "./useSessionControls";
import type { PlayEventStream } from "../../lib/playEvents";
import type { CommittedLine } from "../../lib/playReducer";

/** How many of the recent events the table renders (the durable log holds
 *  the rest; the Traces page pages it). */
const EVENT_ROWS = 15;

interface Props {
  line: CommittedLine | null;
  speakerLabel: string;
  stream: PlayEventStream;
  controls: SessionControlsValue;
  operator: boolean;
  terminal: boolean;
  canCorrect: boolean;
  /** The session snapshot (the facts block and the audio card read it). */
  session: Session;
  /** The appliance's readiness (the facts block's last row). */
  readiness: Readiness | undefined;
  /** The SSE subscription could not be kept open (the stream's note). */
  streamFailed: boolean;
  /** The profile's message texts (the candidates' snippets, by message id). */
  messageText: Map<string, string>;
}

export function OperatorView({
  line,
  speakerLabel,
  stream,
  controls,
  operator,
  terminal,
  canCorrect,
  session,
  readiness,
  streamFailed,
  messageText,
}: Props) {
  const usable = operator && !terminal;
  const correctable = canCorrect && line !== null && line.messageId !== "unknown";
  // The correction buttons label the line as it is on screen — a curation
  // act the API owns (POST /corrections is session-agnostic, and UX-05
  // reaches labels "from Play or a trace": a settled run is a legitimate
  // target). Stop is the one control that needs a live session.
  const correctUsable = operator && correctable && !controls.correctionPending;
  const speakerUnknown = line !== null && !line.speakerKnown && speakerLabel === "unknown";
  const events = stream.events.slice(0, EVENT_ROWS);
  const candidates = stream.candidates;

  return (
    <div className="pp-operator" data-testid="play-operator">
      <div className="pp-operator-frame-col">
        <OperatorFrame line={line} />

        {/* The committed line beneath the frame is the operator's twin of
            the caption view's aria-live region: it updates as the session
            commits, and the text equivalent is announced like the caption
            (docs/design/plum-peach "Accessibility": the line is the
            caption for the audio; issue #34 pins it in the a11y walk). */}
        <div
          className="pp-frame-completed"
          data-testid="play-operator-line"
          aria-live="polite"
          aria-atomic="true"
        >
          {line ? (
            <span className="pp-name-tab-row">
              <NameTab testid="play-operator-line-speaker">{speakerLabel}</NameTab>
              <span className="pp-last-line-text">“{line.text}”</span>
              <code className="pp-last-line-meta">
                {line.messageId} · {clockTime(line.occurredAt)}
              </code>
            </span>
          ) : (
            <p className="pp-controls-note" data-testid="play-operator-line-empty">
              No committed line yet — the frame's textbox commits through the pipeline and appears
              here with its speaker.
            </p>
          )}
        </div>

        <div className="pp-corrections" data-testid="play-operator-corrections">
          <PillButton
            disabled={!correctUsable || speakerUnknown}
            onClick={() => controls.correct("speaker")}
            data-testid="play-correct-speaker"
          >
            Wrong speaker
          </PillButton>
          <PillButton
            disabled={!correctUsable}
            onClick={() => controls.correct("text")}
            data-testid="play-correct-text"
          >
            Wrong text
          </PillButton>
          <PillButton
            variant="coral"
            disabled={!usable}
            onClick={controls.stop}
            data-testid="play-stop"
          >
            Stop
          </PillButton>
          <Chip tone="neutral" testid="play-corrections-hint">
            corrections record a label, never a fix (ADR-0052)
          </Chip>
        </div>
      </div>

      <div className="pp-operator-side">
        <Card title="Session" testid="play-session-facts">
          <SessionStatus session={session} readiness={readiness} />
          {terminal ? (
            <p className="muted pp-terminal-note" data-testid="play-terminal">
              {session.reason ? `reason: ${session.reason} · ` : ""}
              {session.ended_at ? `ended ${session.ended_at}` : "session ended"} — the transcript
              and audio persist. <Link to="/play">Start another</Link>.
            </p>
          ) : null}
          {streamFailed ? (
            <p className="muted pp-terminal-note" data-testid="play-stream-failed">
              The live event stream could not be kept open; the snapshot polling still updates the
              state above.
            </p>
          ) : null}
        </Card>

        <Card title="Event stream" testid="play-event-stream">
          {events.length === 0 ? (
            <p className="pp-controls-note" data-testid="play-event-stream-empty">
              Waiting for events — the session's frame, OCR, ranking and speech events land here,
              newest first.
            </p>
          ) : (
            <table className="pp-events" data-testid="play-events">
              <thead>
                <tr>
                  <th scope="col">Time</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Detail</th>
                </tr>
              </thead>
              <tbody>
                {events.map((event, index) => (
                  <tr
                    key={event.eventId}
                    className={index === 0 ? "is-latest" : undefined}
                    data-testid={`play-event-row-${event.eventId}`}
                  >
                    <td>{clockTime(event.occurredAt)}</td>
                    <td className="pp-events-name">{event.name}</td>
                    <td>{event.detail || "—"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          {stream.events.length > EVENT_ROWS ? (
            <p className="pp-controls-note">
              … {stream.events.length - EVENT_ROWS} more in the durable log (the Traces page pages
              it).
            </p>
          ) : null}
        </Card>

        <Card title="Match candidates" testid="play-candidates">
          {candidates === null ? (
            <p className="pp-controls-note" data-testid="play-candidates-empty">
              No ranking yet — the matcher's candidates for the frame's textbox land here with their
              probabilities.
            </p>
          ) : (
            <>
              <p className="pp-controls-note" data-testid="play-candidates-summary">
                ranking for “{candidates.text}”{candidates.reason ? ` · ${candidates.reason}` : ""}
                {candidates.abstain ? " · abstained" : ""}
                {" · "}
                {candidates.candidateTotal} candidate{candidates.candidateTotal === 1 ? "" : "s"}
              </p>
              <div className="pp-candidates">
                {candidates.candidates.map((candidate) => {
                  // The row names the line, not its store id (the review's
                  // fix for the raw-ULID rows): the text snippet ellipsises
                  // in CSS, and the id retreats to the row's title/tooltip
                  // (and the bar's accessible label).
                  const text = messageText.get(candidate.message_id) ?? "";
                  return (
                    <div
                      className="pp-candidate"
                      key={candidate.message_id}
                      title={candidate.message_id}
                      data-testid={`play-candidate-${candidate.message_id}`}
                    >
                      <span
                        className="pp-candidate-snippet"
                        data-testid={`play-candidate-text-${candidate.message_id}`}
                      >
                        {text ? lineSnippet(text) : `message ${candidate.message_id}`}
                      </span>
                      <ProgressBar
                        value={candidate.probability * 100}
                        label={`Candidate ${text || candidate.message_id} (message ${candidate.message_id}) probability ${probabilityText(candidate.probability)}`}
                        testid={`play-candidate-bar-${candidate.message_id}`}
                      />
                      <span className="pp-candidate-prob">
                        {probabilityText(candidate.probability)}
                      </span>
                    </div>
                  );
                })}
                {candidates.candidates.length === 0 ? (
                  <p className="pp-controls-note">The ranking carried no candidates.</p>
                ) : null}
              </div>
            </>
          )}
        </Card>

        <AudioCard audio={session.audio} pending={controls.audioPending} />
      </div>
    </div>
  );
}
