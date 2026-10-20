import type { ReactNode } from "react";

export interface ErrorCardProps {
  /** The problem, human-readable (the server's detail, not its code). */
  title: string;
  children?: ReactNode;
  /** The problem+json error code (VALIDATION_FAILED, …). */
  code?: string;
  /** The request_id that matches the server log line — always shown when
   *  present (docs/design/plum-peach "Interactions and state"). */
  requestId?: string;
  onRetry?: () => void;
  retryLabel?: string;
  testid?: string;
}

/** The restyled error state: the coral border, the problem and the
 *  `request_id` kept in view (the id a developer pastes next to the server
 *  log line). */
export function ErrorCard({
  title,
  children,
  code,
  requestId,
  onRetry,
  retryLabel = "Try again",
  testid = "kit-error",
}: ErrorCardProps) {
  return (
    <div className="kit-error" role="alert" data-testid={testid}>
      <p>{title}</p>
      {children}
      {code !== undefined || requestId !== undefined ? (
        <span className="kit-error-meta" data-testid="kit-error-meta">
          {code !== undefined ? code : ""}
          {code !== undefined && requestId !== undefined ? " · " : ""}
          {requestId !== undefined ? requestId : ""}
        </span>
      ) : null}
      {onRetry !== undefined ? (
        <button type="button" onClick={onRetry} data-testid="kit-error-retry">
          {retryLabel}
        </button>
      ) : null}
    </div>
  );
}
