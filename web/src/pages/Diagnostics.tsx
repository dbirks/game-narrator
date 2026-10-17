import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { fetchReadiness, fetchSystemInfo } from "../api/system";
import { ErrorState } from "../components/states";

/**
 * `/diagnostics` — Milestone 0 slice of docs/spec/09 "Routes and page
 * requirements": build/version/os/arch plus API readiness. Capture, model-role
 * and voice-registry sections arrive with their owning milestones.
 */
export function DiagnosticsPage() {
  const system = useQuery({
    queryKey: ["system", "info"],
    queryFn: ({ signal }) => fetchSystemInfo(signal),
  });
  const readiness = useQuery({
    queryKey: ["health", "ready"],
    queryFn: ({ signal }) => fetchReadiness(signal),
  });

  const error = system.error ?? readiness.error;

  let body: ReactNode;
  if (error) {
    body = (
      <ErrorState
        error={error}
        action="Diagnostics unavailable"
        onRetry={() => {
          void system.refetch();
          void readiness.refetch();
        }}
      />
    );
  } else if (!system.data || !readiness.data) {
    body = <p className="muted">Loading…</p>;
  } else {
    const info = system.data;
    const ready = readiness.data;
    body = (
      <dl className="facts">
        <div>
          <dt>Version</dt>
          <dd data-testid="version">{info.version}</dd>
        </div>
        <div>
          <dt>Commit</dt>
          <dd data-testid="commit">{info.commit}</dd>
        </div>
        <div>
          <dt>Go version</dt>
          <dd>{info.go_version}</dd>
        </div>
        <div>
          <dt>Platform</dt>
          <dd data-testid="platform">
            {info.os}/{info.arch}
          </dd>
        </div>
        <div>
          <dt>CPU cores</dt>
          <dd>{info.num_cpu}</dd>
        </div>
        <div>
          <dt>Started at</dt>
          <dd>{info.started_at}</dd>
        </div>
        <div>
          <dt>Readiness</dt>
          <dd data-testid="readiness">{ready.status}</dd>
        </div>
      </dl>
    );
  }

  return (
    <main>
      <h1>Diagnostics</h1>
      {body}
    </main>
  );
}
