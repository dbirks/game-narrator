import type { ReactNode } from "react";

export interface ChipProps {
  children: ReactNode;
  /** "peach" = selected/current (default); "neutral" = plain metadata;
   *  "bad" = the design's slow/fail status tag (coral fill). */
  tone?: "peach" | "neutral" | "bad";
  testid?: string;
}

/** The 11px mono chip: --peach fill for "selected / current", neutral for
 *  plain metadata, coral for the slow/fail tag (docs/design/plum-peach
 *  "2i": status tags, --peach = ok, --coral = slow/fail). */
export function Chip({ children, tone = "peach", testid = "kit-chip" }: ChipProps) {
  return (
    <span
      className={
        tone === "neutral"
          ? "kit-chip kit-chip-neutral"
          : tone === "bad"
            ? "kit-chip kit-chip-bad"
            : "kit-chip"
      }
      data-testid={testid}
    >
      {children}
    </span>
  );
}
