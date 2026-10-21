/**
 * The unpaired-Bluetooth pairing prompt (docs/design/plum-peach UX-12,
 * ADR-0142 §4): the audio device selector lists an unpaired device from the
 * Bluetooth inventory as a "pair first" option; selecting it renders this
 * prompt — the device's name, why it cannot be the output yet (no pairing,
 * no A2DP sink), and the Pair action that drives the existing pair action
 * (ADR-0091) — instead of persisting an id that has no sink. That
 * silent-failing persist is the failure the acceptance rule rules out.
 */
export function PairingPrompt({
  address,
  name,
  canAct,
  busy,
  note,
  error,
  onPair,
}: {
  address: string;
  name: string;
  canAct: boolean;
  busy: boolean;
  note: string | null;
  error: string | null;
  onPair: () => void;
}) {
  return (
    <div
      className="settings-bt-prompt"
      role="group"
      aria-label={`Pair ${name} first`}
      data-testid="pairing-prompt"
    >
      <p>
        <strong data-testid="pairing-prompt-name">{name}</strong> ({address}) is not paired yet, so
        BlueZ has no A2DP sink for it — there is no output to select. Pairing creates the sink
        (bluez_output.{address}.1), which then appears in the device list.
      </p>
      <button
        type="button"
        data-testid="pairing-prompt-pair"
        disabled={!canAct || busy}
        onClick={onPair}
      >
        {busy ? "Pairing…" : "Pair"}
      </button>
      {note !== null && (
        <p className="muted" role="status" data-testid="pairing-prompt-note">
          {note}
        </p>
      )}
      {error !== null && (
        <p role="alert" className="error" data-testid="pairing-prompt-error">
          {error}
        </p>
      )}
    </div>
  );
}
