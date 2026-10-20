import type { ReactNode } from "react";

export interface CardProps {
  title?: ReactNode;
  children: ReactNode;
  /** Stable testid (the default names the component; pages override). */
  testid?: string;
}

/** The design's card: 1.5px ink line, 14px radius, --card surface. */
export function Card({ title, children, testid = "kit-card" }: CardProps) {
  return (
    <section className="kit-card" data-testid={testid}>
      {title !== undefined ? <h2 className="kit-card-title">{title}</h2> : null}
      {children}
    </section>
  );
}
