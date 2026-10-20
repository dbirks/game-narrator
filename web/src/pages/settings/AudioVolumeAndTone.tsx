/**
 * The master volume and the test tone of the "Audio output" card (ADR-0074):
 * the volume persists for an administrator on release (mouse) or keyup, and
 * the test tone plays only on the button's click — consent is the gate
 * (docs/spec/13), and the filesink leaves the tone's recorded WAV named in
 * the answer, so a machine with no audible output still gets proof.
 */
export function AudioVolumeAndTone({
  canEdit,
  canPlay,
  saving,
  toneBusy,
  value,
  source,
  onVolumeChange,
  onPersistVolume,
  onTone,
}: {
  canEdit: boolean;
  canPlay: boolean;
  saving: boolean;
  toneBusy: boolean;
  value: number;
  source: string;
  onVolumeChange: (value: number) => void;
  onPersistVolume: (value: number) => void;
  onTone: () => void;
}) {
  return (
    <>
      <label htmlFor="audio-master-volume">Master volume</label>
      <input
        id="audio-master-volume"
        data-testid="audio-master-volume"
        className="kit-range"
        /* The fill fraction as an attribute (ADR-0011: no inline styles):
           the kit-range rules in tokens.css paint the coral fill over the
           ink track from it. */
        data-volume={value.toFixed(2)}
        type="range"
        min={0}
        max={1}
        step={0.05}
        value={value}
        disabled={!canEdit || saving}
        onChange={(event) => onVolumeChange(Number(event.target.value))}
        onMouseUp={() => onPersistVolume(value)}
        onKeyUp={() => onPersistVolume(value)}
      />
      <span className="muted" data-testid="audio-master-volume-value">
        {Math.round(value * 100)}% ({source})
      </span>
      <button
        type="button"
        onClick={onTone}
        disabled={!canPlay || toneBusy}
        data-testid="audio-test-tone"
      >
        {toneBusy ? "Playing…" : "Play test tone (440 Hz, 1 s)"}
      </button>
    </>
  );
}
