import type { AudioDevice } from "../../api/audio";

/**
 * The audio device picker (docs/design/plum-peach UX-12, ADR-0142 §4): the
 * selector's shape is the appliance's real sink categories — one `optgroup`
 * each for HDMI (the hub's port), USB and Bluetooth, with everything else
 * under "Other" so the enumeration is never dropped from the picker. An
 * unpaired device from the Bluetooth inventory (ADR-0091) appears in the
 * Bluetooth group as a "pair first" option; the parent renders the pairing
 * prompt when it is chosen instead of persisting an id that has no sink.
 */

/** The appliance's sink categories (UX-12), in the design's order. */
const CATEGORIES = [
  { kind: "hdmi", label: "HDMI — the hub's port" },
  { kind: "usb", label: "USB" },
  { kind: "bluetooth", label: "Bluetooth" },
] as const;

export const PAIR_FIRST_PREFIX = "pair-first:";

export interface PairFirst {
  address: string;
  name: string;
}

function categoryOf(kind: string): string {
  if (kind === "hdmi" || kind === "usb" || kind === "bluetooth") return kind;
  return "other";
}

export function AudioDeviceSelect({
  devices,
  pairFirst,
  value,
  disabled,
  onChange,
}: {
  devices: AudioDevice[];
  pairFirst: PairFirst[];
  value: string;
  disabled: boolean;
  onChange: (value: string) => void;
}) {
  const optionsOf = (kind: string) =>
    devices
      .filter((device) => device.id !== "default")
      .filter((device) => categoryOf(device.kind) === kind)
      .map((device) => (
        <option key={device.id} value={device.id}>
          {device.name} ({device.kind})
        </option>
      ));

  return (
    <select
      id="audio-device-select"
      data-testid="audio-device-select"
      className="kit-select"
      value={value}
      disabled={disabled}
      onChange={(event) => onChange(event.target.value)}
    >
      <option value="default">default (the platform's own routing)</option>
      {CATEGORIES.map((category) => (
        <optgroup
          key={category.kind}
          label={category.label}
          data-testid={`audio-device-category-${category.kind}`}
        >
          {optionsOf(category.kind)}
          {category.kind === "bluetooth"
            ? pairFirst.map((device) => (
                <option
                  key={device.address}
                  value={`${PAIR_FIRST_PREFIX}${device.address}`}
                  data-testid={`audio-pair-first-${device.address}`}
                >
                  {device.name} — pair first
                </option>
              ))
            : null}
        </optgroup>
      ))}
      <optgroup label="Other" data-testid="audio-device-category-other">
        {optionsOf("other")}
      </optgroup>
    </select>
  );
}
