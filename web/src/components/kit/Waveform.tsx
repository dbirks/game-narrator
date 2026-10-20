import { useId } from "react";

export interface WaveformProps {
  /** Per-bar amplitudes 0–1 (the clip's envelope). */
  values: readonly number[];
  label: string;
  testid?: string;
}

/** The voice clip's waveform (the text twin's visual half — every clip
 *  keeps its text twin, docs/design/plum-peach "Accessibility"). Bars sit
 *  over the baseline the design's now-speaking view draws; geometry is SVG
 *  attributes, colour is the stylesheet's. */
export function Waveform({ values, label, testid = "kit-waveform" }: WaveformProps) {
  const id = useId();
  const patternId = `kit-wave-pattern-${id}`;
  const count = Math.max(values.length, 1);
  return (
    <svg
      className="kit-wave"
      role="img"
      aria-label={label}
      viewBox={`0 0 ${count * 5} 30`}
      preserveAspectRatio="none"
      data-testid={testid}
    >
      <defs>
        <pattern id={patternId} width={2} height={2} patternUnits="userSpaceOnUse">
          <rect x={0} y={0} width={1} height={1} className="kit-wave-bar" />
          <rect x={1} y={1} width={1} height={1} className="kit-wave-bar" />
        </pattern>
      </defs>
      {values.map((value, index) => {
        const clamped = Math.max(0.08, Math.min(1, value));
        const height = Math.round(clamped * 24);
        return (
          <rect
            key={index}
            x={index * 5 + 1}
            y={30 - height}
            width={3}
            height={height}
            fill={`url(#${patternId})`}
          />
        );
      })}
      <line x1={0} y1={30} x2={count * 5} y2={30} className="kit-wave-baseline" />
    </svg>
  );
}
