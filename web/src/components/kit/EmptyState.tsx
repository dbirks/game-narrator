import type { ReactNode } from "react";

export interface KitEmptyStateProps {
  title: string;
  children?: ReactNode;
  testid?: string;
}

/** The restyled empty state (docs/design/plum-peach "Interactions and
 *  state"): the dashed 1.5px ink card. The legacy `.empty` class keeps
 *  working for existing pages; this is the kit's explicit version. */
export function KitEmptyState({ title, children, testid = "kit-empty" }: KitEmptyStateProps) {
  return (
    <div className="kit-empty" data-testid={testid}>
      <h2>{title}</h2>
      {children}
    </div>
  );
}
