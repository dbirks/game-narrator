import type { ReactNode } from "react";

import { ApiError } from "../api/client";

/**
 * `src/components/states.tsx` — the three states docs/spec/09 "UX robustness"
 * requires for every data-backed view, plus the error shape it asks for.
 *
 * The error block always carries the `code` and `request_id` of the problem
 * document (docs/spec/08, docs/spec/13): "Diagnostics unavailable" is not
 * actionable, "VALIDATION_FAILED / 4e7f97…" is, and the id is what a developer
 * pastes next to the server log line.
 */

export function Loading({ label = "Loading…" }: { label?: string }) {
  return (
    <p className="muted" data-testid="loading">
      {label}
    </p>
  );
}

export function EmptyState({ title, children }: { title: string; children?: ReactNode }) {
  return (
    <div className="empty" data-testid="empty-state">
      <h2>{title}</h2>
      {children}
    </div>
  );
}

export interface ErrorStateProps {
  error: unknown;
  /** What was being done, e.g. "loading the library". */
  action?: string;
  onRetry?: () => void;
}

/** A problem+json failure rendered as text. Never `dangerouslySetInnerHTML`:
 *  server and source text passes through this component uninterpreted. */
export function ErrorState({ error, action, onRetry }: ErrorStateProps) {
  const detail =
    error instanceof ApiError
      ? `${error.title} — ${error.detail}`
      : error instanceof Error
        ? error.message
        : "network error";
  const code = error instanceof ApiError ? error.code : null;
  const requestId = error instanceof ApiError ? error.requestId : "";

  return (
    <div role="alert" className="error" data-testid="error-state">
      <p>
        {action ? `${action}: ` : ""}
        {detail}
      </p>
      {code || requestId ? (
        <p className="muted error-meta">
          {code ? <code>{code}</code> : null} {requestId ? <code>request {requestId}</code> : null}
        </p>
      ) : null}
      {onRetry ? (
        <button type="button" onClick={onRetry}>
          Try again
        </button>
      ) : null}
    </div>
  );
}

/** Every finding of a refused publish, listed rather than truncated to one. */
export function ProblemList({
  problems,
  heading = "Publication refused",
}: {
  problems: string[];
  heading?: string;
}) {
  if (problems.length === 0) return null;
  return (
    <div role="alert" className="error" data-testid="problem-list">
      <p>
        <strong>{heading}</strong> ({problems.length} finding{problems.length === 1 ? "" : "s"})
      </p>
      <ol className="problems">
        {problems.map((problem) => (
          <li key={problem}>{problem}</li>
        ))}
      </ol>
    </div>
  );
}
