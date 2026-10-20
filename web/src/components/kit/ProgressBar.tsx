import { useId } from "react";

export interface ProgressBarProps {
  /** 0–100; out-of-range values clamp (the bar must never render >100%). */
  value: number;
  /** The accessible name (e.g. "Skies of Arcadia voiced"). */
  label: string;
  testid?: string;
}

/** The dithered progress bar (docs/design/plum-peach 2c): ink + coral
 *  checker over the --sand track. The fill is an SVG whose width attribute
 *  carries the only dynamic value — presentation stays in the stylesheet
 *  (no inline styles, no injected CSS; ADR-0011), and `role=progressbar`
 *  with aria-valuenow is the a11y contract the e2e suite asserts. */
export function ProgressBar({ value, label, testid = "kit-progress" }: ProgressBarProps) {
  const id = useId();
  const clamped = Math.max(0, Math.min(100, value));
  const patternId = `kit-progress-pattern-${id}`;
  const clipId = `kit-progress-clip-${id}`;
  return (
    <svg
      className="kit-progress"
      role="progressbar"
      aria-valuemin={0}
      aria-valuemax={100}
      aria-valuenow={Math.round(clamped)}
      aria-label={label}
      viewBox="0 0 100 10"
      preserveAspectRatio="none"
      data-testid={testid}
    >
      <defs>
        {/* The ink + coral checker (2x2 cells on the 100x10 grid). */}
        <pattern id={patternId} width={4} height={4} patternUnits="userSpaceOnUse">
          <rect x={0} y={0} width={2} height={2} className="kit-progress-fill-a" />
          <rect x={2} y={0} width={2} height={2} className="kit-progress-fill-b" />
          <rect x={0} y={2} width={2} height={2} className="kit-progress-fill-b" />
          <rect x={2} y={2} width={2} height={2} className="kit-progress-fill-a" />
        </pattern>
        <clipPath id={clipId}>
          <rect x={0} y={0} width={clamped} height={10} />
        </clipPath>
      </defs>
      <rect x={0} y={0} width={100} height={10} className="kit-progress-track" />
      <rect
        x={0}
        y={0}
        width={100}
        height={10}
        fill={`url(#${patternId})`}
        clipPath={`url(#${clipId})`}
      />
    </svg>
  );
}
