import type { ReactNode } from "react";

import { ProgressBar } from "./ProgressBar";

export interface StatTileProps {
  label: string;
  value: string | number;
  /** A dithered progress foot (0–100), e.g. "voiced". */
  progress?: number;
  /** Optional detail line under the value (mono, muted). */
  detail?: ReactNode;
  testid?: string;
}

/** The setup page's stat tile (docs/design/plum-peach 2c): mono label,
 *  big number, an optional dithered progress foot. */
export function StatTile({
  label,
  value,
  progress,
  detail,
  testid = "kit-stat-tile",
}: StatTileProps) {
  return (
    <div className="kit-stat" data-testid={testid}>
      <span className="kit-stat-label">{label}</span>
      <span className="kit-stat-value">{value}</span>
      {detail !== undefined ? <span className="kit-stat-detail muted">{detail}</span> : null}
      {progress !== undefined ? (
        <ProgressBar value={progress} label={`${label} progress`} testid="kit-stat-progress" />
      ) : null}
    </div>
  );
}
