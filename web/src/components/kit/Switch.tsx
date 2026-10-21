/**
 * `src/components/kit/Switch.tsx` — the kit's ink pill toggle
 * (docs/design/plum-peach): a 1.5px ink-bordered pill track, the knob an
 * ink dot at rest; on, the track fills ink and the paper knob sits right.
 * A real checkbox with `role="switch"` (the Cast auto-cast dial, ADR-0109,
 * and the Play operator toggles use it), so `checked` state, keyboard and
 * screen-reader switch semantics all come from the platform.
 */
import type { ChangeEvent } from "react";

export interface SwitchProps {
  /** The label text beside the pill (the accessible name). */
  label: string;
  checked: boolean;
  disabled?: boolean;
  /** The input's testid (callers keep stable ids, e.g. auto-cast-switch). */
  testid?: string;
  /** Extra class for the labelled group. */
  className?: string;
  onChange: (next: boolean) => void;
}

export function Switch({
  label,
  checked,
  disabled,
  testid = "kit-switch",
  className = "",
  onChange,
}: SwitchProps) {
  return (
    <label className={`kit-switch ${className}`.trim()}>
      <span>{label}</span>
      <input
        type="checkbox"
        role="switch"
        aria-checked={checked}
        data-testid={testid}
        checked={checked}
        disabled={disabled}
        onChange={(event: ChangeEvent<HTMLInputElement>) => onChange(event.target.checked)}
      />
    </label>
  );
}
