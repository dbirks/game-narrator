import { useId } from "react";

export interface SparklineProps {
  /** Twelve values (the health tile's window); anything else is padded or
   *  truncated to twelve — the design's bar count is binding. */
  values: readonly number[];
  label: string;
  testid?: string;
  /** A fixed scale top: the bars scale to this value instead of the
   *  window's own max, so each value's own meaning survives a flat window
   *  (the health tile's level encoding: ok 100 / slow 55 / fail 25 /
   *  unknown 0). Omitted: the window's max, the general shape. */
  max?: number;
}

const BAR_COUNT = 12;

/** The health tile's 12-bar dithered sparkline (docs/design/plum-peach 2i).
 *  Bars scale to the window's max (a flat-zero window renders the floor
 *  line, not nothing) — or to the fixed `max` when the values carry their
 *  own scale; the bar geometry is SVG attributes and the 50% checker fill
 *  is a stylesheet pattern, so no inline styles are needed. */
export function Sparkline({ values, label, testid = "kit-sparkline", max }: SparklineProps) {
  const id = useId();
  const bars: number[] = [];
  for (let i = 0; i < BAR_COUNT; i++) bars.push(i < values.length ? values[i] : 0);
  const top = max ?? Math.max(...bars, 0);
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
        const height = top > 0 ? Math.max(2, Math.round((value / top) * 20)) : 2;
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
