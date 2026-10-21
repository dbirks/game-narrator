/**
 * `src/pages/play/ControlBar.tsx` — the Play page's bottom control bar
 * (docs/design/plum-peach 2e/2f, ADR-0093): pause · Mute · volume (the
 * dithered bar and its percentage) · speech delay · the careful/eager
 * segmented toggle (the friendly names for the session's conservative /
 * predictive decision mode) · Wrong speaker? · Stop.
 *
 * One bar serves both views — now-speaking and "show workings" — and the kid
 * mode's pause/volume go through the same hook, so the design's "one session
 * state across the toggle" holds: the bar renders from the server's snapshot
 * through useSessionControls and never owns a second copy of the session.
 * The operator view's own correction trio already carries Wrong speaker and
 * Stop, so over that view the bar yields those two controls (the testids
 * stay single in the DOM); the correction's outcome line is the bar's in
 * both views.
 * The pre-redesign control testids (play-pause, play-mute, play-volume,
 * play-speech-delay, play-stop, play-narrated) stay put (ADR-0093 keeps the
 * e2e selector contract); the mode select becomes the design's segmented
 * toggle (play-mode-segmented) — the 2e/2f merge's control-bar change.
 * The now-speaking bar's Stop is the mock's static control (the 2026-10-20
 * review: it unmounted on a settled session, so only the operator view's
 * correction trio showed one) — it stays in the bar in every state and
 * disables where there is nothing to stop.
 */
import { Segmented } from "../../components/kit/Segmented";
import { PillButton } from "../../components/kit/PillButton";
import type { SessionControlsValue, DecisionMode } from "./useSessionControls";

const MODE_OPTIONS = [
  { value: "conservative", label: "careful" },
  { value: "predictive", label: "eager" },
] as const;

interface Props {
  controls: SessionControlsValue;
  /** The operator role gates the session controls (the server enforces it). */
  operator: boolean;
  terminal: boolean;
  /** The curator role gates the correction label (the server enforces it). */
  canCorrect: boolean;
  /** A committed line exists for a correction to name. */
  hasLine: boolean;
  /** The line's speaker is the store's "unknown" sentinel (nothing to correct). */
  speakerUnknown: boolean;
  /**
   * Which view the bar serves ("now" = the caption view, "operator" = "show
   * workings"). The bar renders over both views (one session, one bar), but
   * the operator view's own correction trio (Wrong speaker · Wrong text ·
   * Stop) already carries the correction labels and the stop — so in that
   * view the bar yields those two controls, keeping the testids single in
   * the DOM (the e2e contract: one element per selector, ADR-0093).
   */
  view: "now" | "operator";
}

export function ControlBar({
  controls,
  operator,
  terminal,
  canCorrect,
  hasLine,
  speakerUnknown,
  view,
}: Props) {
  // The lifecycle controls need a running session (pausing, stopping or
  // re-deciding a settled run has nothing to act on). The audio settings
  // are different: the API applies them to the session's row whatever the
  // run's state (the 200 body is the fresh snapshot, and the next session
  // reads the persisted settings), so the operator calibrating while
  // reviewing a settled run is a legitimate walk, not a fault.
  const controlsDisabled = !operator || terminal;
  const audioDisabled = !operator;
  const correctable = canCorrect && hasLine && !speakerUnknown;

  return (
    <section
      className="pp-controls"
      aria-label="Session controls"
      data-testid="play-session-controls"
    >
      <div className="pp-controls-row">
        <PillButton
          aria-pressed={controls.paused}
          disabled={controlsDisabled || controls.pausePending}
          onClick={controls.togglePause}
          data-testid="play-pause"
        >
          {controls.pausePending ? "Applying…" : controls.paused ? "Resume" : "Pause"}
        </PillButton>
        <PillButton
          role="switch"
          aria-checked={controls.muted}
          disabled={audioDisabled}
          onClick={() => controls.setMuted(!controls.muted)}
          data-testid="play-mute"
        >
          {controls.muted ? "Unmute" : "Mute"}
        </PillButton>
        <span className="pp-slice">
          <label className="pp-controls-note" htmlFor="play-volume">
            Volume
          </label>
          <span className="pp-volume">
            <span className="pp-volume-bar" aria-hidden="true" />
            <input
              id="play-volume"
              type="range"
              min={0}
              max={100}
              step={1}
              value={controls.volumePct}
              disabled={audioDisabled}
              onChange={(event) => controls.pressVolume(Number(event.target.value))}
              data-testid="play-volume"
            />
          </span>
          <span className="pp-pct" data-testid="play-volume-pct">
            {controls.volumePct}%
          </span>
        </span>
        <span className="pp-slice">
          <label className="pp-controls-note" htmlFor="play-speech-delay">
            Delay
          </label>
          <span className="pp-delay">
            <input
              id="play-speech-delay"
              aria-label="Speech delay in milliseconds"
              type="number"
              min={0}
              max={2000}
              value={controls.delayMs}
              disabled={audioDisabled}
              onChange={(event) => controls.setDelayMs(Number(event.target.value))}
              data-testid="play-speech-delay"
            />
          </span>
        </span>
        <Segmented
          options={MODE_OPTIONS}
          value={controls.mode}
          onValueChange={(mode: DecisionMode) => controls.setMode(mode)}
          label="Decision mode (careful keeps the matcher's call; eager also speaks predictive lines)"
          testid="play-mode-segmented"
        />
        {canCorrect && view === "now" ? (
          <PillButton
            disabled={!correctable || controls.correctionPending}
            onClick={() => controls.correct("speaker")}
            data-testid="play-correct-speaker"
          >
            Wrong speaker?
          </PillButton>
        ) : null}
        {view === "now" ? (
          <PillButton
            variant="coral"
            disabled={!operator || terminal}
            onClick={controls.stop}
            data-testid="play-stop"
          >
            Stop
          </PillButton>
        ) : null}
      </div>
      <div className="pp-controls-row pp-classes" aria-label="Narrated classes">
        <label className="pp-class" data-testid="play-class-dialogue-row">
          <input type="checkbox" checked disabled data-testid="play-class-dialogue" />
          <span>dialogue</span>
        </label>
        <label className="pp-class">
          <input
            type="checkbox"
            checked={controls.classes.includes("sign")}
            disabled={controlsDisabled || controls.settingsPending}
            onChange={() => controls.toggleClass("sign")}
            data-testid="play-class-signs"
          />
          <span>+signs</span>
        </label>
        <label className="pp-class">
          <input
            type="checkbox"
            checked={controls.classes.includes("item")}
            disabled={controlsDisabled || controls.settingsPending}
            onChange={() => controls.toggleClass("item")}
            data-testid="play-class-items"
          />
          <span>+items</span>
        </label>
      </div>
      <p className="pp-controls-note" data-testid="play-controls-note">
        Narrating:{" "}
        <span data-testid="play-narrated">{controls.classes.join(", ") || "dialogue"}</span> in{" "}
        {controls.mode === "predictive" ? "eager" : "careful"} mode
        {" · "}
        shortcuts <kbd>p</kbd> pause · <kbd>s</kbd> signs · <kbd>i</kbd> items · <kbd>m</kbd> mode
      </p>
      {controls.correction ? (
        <p className="pp-correction-result" data-testid="play-correction-result" aria-live="polite">
          Correction recorded as proposal <code>{controls.correction.id}</code> — it labels the line
          for the re-run; the curation side applies the fix.
        </p>
      ) : null}
      {controls.correctionError ? (
        <p className="error" data-testid="play-correction-error" role="alert">
          The correction could not be recorded: {controls.correctionError}
        </p>
      ) : null}
    </section>
  );
}
