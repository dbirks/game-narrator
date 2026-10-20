import { useId } from "react";

export interface SparklineProps {
  /** Twelve values (the health tile's window); anything else is padded or
   *  truncated to twelve — the design's bar count is binding. */
  values: readonly number[];
  label: string;
  testid?: string;
}

const BAR_COUNT = 12;

/** The health tile's 12-bar dithered sparkline (docs/design/plum-peach 2i).
 *  Bars scale to the window's max (a flat-zero window renders the floor
 *  line, not nothing); the bar geometry is SVG attributes and the 50%
 *  checker fill is a stylesheet pattern, so no inline styles are needed. */
export function Sparkline({ values, label, testid = "kit-sparkline" }: SparklineProps) {
  const id = useId();
  const bars: number[] = [];
  for (let i = 0; i < BAR_COUNT; i++) bars.push(i < values.length ? values[i] : 0);
  const max = Math.max(...bars, 0);
  const patternId = `kit-spark-pattern-${id}`;
  return (
    <svg
      className="kit-spark"
      role="img"
      aria-label={label}
      viewBox="0 0 108 24"
      preserveAspectRatio="none"
      data-testid={testid}
    >
      <defs>
        <pattern id={patternId} width={2} height={2} patternUnits="userSpaceOnUse">
          <rect x={0} y={0} width={1} height={1} className="kit-spark-bar" />
          <rect x={1} y={1} width={1} height={1} className="kit-spark-bar" />
        </pattern>
      </defs>
      {bars.map((value, index) => {
        const height = max > 0 ? Math.max(2, Math.round((value / max) * 20)) : 2;
        return (
          <rect
            key={index}
            x={index * 9}
            y={24 - height}
            width={7}
            height={height}
            fill={`url(#${patternId})`}
          />
        );
      })}
    </svg>
  );
}
