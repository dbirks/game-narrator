/**
 * `src/pages/play/NowSpeaking.tsx` — the Play page's default view
 * (docs/design/plum-peach 2e, ADR-0093): the portrait arch with the
 * speaker's name tab, the current line at 52px (the caption for the audio)
 * and the big waveform, and the last three lines at 75% opacity.
 *
 * The view is purely presentational over the session state its parent
 * (LiveSession in Play.tsx) owns — the event stream, the snapshot and the
 * controls all live above it, so "show workings" can swap this view out for
 * the operator view without restarting or duplicating anything (plan.md
 * Phase 1 item 6: one session state across the toggle). The committed line
 * is the caption: it is the aria-live region the pre-redesign suite pins.
 *
 * The default view is exactly this surface (the 2026-10-20 review, against
 * mocks 2e/2f): portrait, name tab, line, waveform, last three lines — and
 * the control bar, which the parent renders beneath both views so one bar
 * serves now-speaking and operator alike. The session's stats (mode, frame
 * source, counters, end reason) and the audio output card belong to the
 * operator view (SessionFacts renders them there), keeping the caption view
 * clean.
 */
import { DitherCover } from "../../components/kit/DitherCover";
import { NameTab } from "../../components/kit/NameTab";
import { Waveform } from "../../components/kit/Waveform";
import { clockTime, waveformValues } from "./shared";
import type { CommittedLine } from "../../lib/playReducer";

interface Props {
  /** The newest committed line (the current one), or null before the first. */
  line: CommittedLine | null;
  /** The line's displayed speaker name (the roster's, the page's rule). */
  speakerLabel: string;
  /** Committed lines, newest first (the view shows the last three). */
  lines: CommittedLine[];
  speakerOf: (line: CommittedLine) => string;
}

export function NowSpeaking({ line, speakerLabel, lines, speakerOf }: Props) {
  return (
    <div className="pp-now" data-testid="play-now-speaking">
      <div className="pp-portrait">
        <div className="pp-portrait-frame">
          <DitherCover
            className="pp-portrait-cover"
            label={`Portrait placeholder for ${speakerLabel}`}
            testid="play-portrait"
          />
        </div>
        <span className="pp-name-tab-row">
          <NameTab testid="play-speaker-tab">{speakerLabel}</NameTab>
        </span>
      </div>

      <div className="pp-now-main">
        <section className="play-line" aria-label="Current committed line" aria-live="polite">
          {line ? (
            <div data-testid="current-line">
              <p className="pp-line" data-testid="current-line-text">
                {line.text}
              </p>
              <p className="pp-controls-note">
                speaker: <span data-testid="current-line-speaker">{speakerLabel}</span>
                {" · "}
                message: <code data-testid="current-line-message-id">{line.messageId}</code>
                {line.speechEligible ? "" : " · not speech-eligible"}
              </p>
              <div className="pp-line-wave">
                <Waveform
                  values={waveformValues(line.text)}
                  label={`Speech waveform for the current line: ${line.text}`}
                  testid="play-waveform"
                />
              </div>
            </div>
          ) : (
            <p className="pp-line-empty" data-testid="current-line-empty">
              Waiting for the first line…
            </p>
          )}
        </section>

        <ol
          className="pp-last-lines"
          aria-label="Last three committed lines"
          aria-live="polite"
          aria-relevant="additions"
          data-testid="recent-lines"
        >
          {lines.slice(0, 3).map((committed, index) => (
            <li
              key={committed.occurrenceId}
              className="pp-last-line"
              data-testid={`recent-line-${index}`}
            >
              <span className="pp-last-line-text">{committed.text}</span>
              <span className="pp-last-line-meta">
                <NameTab testid={`recent-line-speaker-${index}`}>
                  {speakerOf(committed) === "unknown" ? "?" : speakerOf(committed)}
                </NameTab>
                {" · "}
                <code>{committed.messageId}</code>
                {" · "}
                {clockTime(committed.occurredAt)}
              </span>
            </li>
          ))}
        </ol>
        {lines.length === 0 ? (
          <p className="pp-controls-note" data-testid="play-no-lines">
            No commits yet — the lines committed by the session land here, newest first.
          </p>
        ) : null}
      </div>
    </div>
  );
}
