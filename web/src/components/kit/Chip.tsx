import type { ReactNode } from "react";

export interface ChipProps {
  children: ReactNode;
  /** "peach" = selected/current (default); "neutral" = plain metadata. */
  tone?: "peach" | "neutral";
  testid?: string;
}

/** The 11px mono chip: --peach fill for "selected / current", neutral for
 *  plain metadata (docs/design/plum-peach "Typography"). */
export function Chip({ children, tone = "peach", testid = "kit-chip" }: ChipProps) {
  return (
    <span
      className={tone === "neutral" ? "kit-chip kit-chip-neutral" : "kit-chip"}
      data-testid={testid}
    >
      {children}
    </span>
  );
}
