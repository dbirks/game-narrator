/**
 * `src/pages/play/KidMode.tsx` — the kid/tablet mode (docs/design/plum-peach
 * 2g, plan.md Phase 1 item 10's stub, UX-10): three huge buttons (− / pause
 * / +) and nothing else — the 260px circular portrait, the character name
 * at 48px and the current line at 24px. Entry is `?mode=kid` on the session
 * URL; the view renders from the same session state its parent owns, so
 * pause and volume act on the live session exactly like the operator's
 * controls (session state changes are asserted via the API in the spec).
 *
 * The acceptance's "no other interactive controls by role" is scoped to
 * this view: the global chrome (status bar nav, the ⌘J pill) is the shell's
 * on every route, not part of the mode.
 */
import { DitherCover } from "../../components/kit/DitherCover";
import type { SessionControlsValue } from "./useSessionControls";
import type { CommittedLine } from "../../lib/playReducer";

const VOLUME_STEP = 10;

interface Props {
  speakerLabel: string;
  line: CommittedLine | null;
  controls: SessionControlsValue;
}

export function KidMode({ speakerLabel, line, controls }: Props) {
  const volume = controls.volumePct;
  const paused = controls.paused;

  return (
    <section className="pp-kid" aria-label="Kid mode" data-testid="play-kid-mode">
      <div className="pp-kid-portrait" data-testid="play-kid-portrait">
        <DitherCover label={`Portrait placeholder for ${speakerLabel}`} testid="play-kid-cover" />
      </div>
      <h2 className="pp-kid-name" data-testid="play-kid-name">
        {speakerLabel}
      </h2>
      <p className="pp-kid-line" data-testid="play-kid-line" aria-live="polite">
        {line ? line.text : "Waiting for the first line…"}
      </p>
      <div className="pp-kid-controls" data-testid="play-kid-controls">
        <button
          type="button"
          className="pp-kid-btn"
          aria-label={`Volume down (now ${volume}%)`}
          onClick={() => controls.pressVolume(Math.max(0, volume - VOLUME_STEP))}
          data-testid="play-kid-minus"
        >
          −
        </button>
        <button
          type="button"
          className="pp-kid-btn pp-kid-btn--pause"
          aria-label={paused ? "Resume" : "Pause"}
          aria-pressed={paused}
          disabled={controls.pausePending}
          onClick={controls.togglePause}
          data-testid="play-kid-pause"
        >
          {paused ? "▶" : "❚❚"}
        </button>
        <button
          type="button"
          className="pp-kid-btn"
          aria-label={`Volume up (now ${volume}%)`}
          onClick={() => controls.pressVolume(Math.min(100, volume + VOLUME_STEP))}
          data-testid="play-kid-plus"
        >
          +
        </button>
      </div>
    </section>
  );
}
