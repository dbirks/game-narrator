import { useQuery } from "@tanstack/react-query";

import type { LogRecord } from "../../api/diagnostics";
import { gatedJson } from "../../api/diagnostics";
import type { Readiness } from "../../api/client";
import { ErrorState } from "../../components/states";
import { useGated } from "./gated";

/**
 * `src/pages/health/logs.tsx` — the preserved system-log tail (the
 * **logs** tab) and the readiness checks, moved verbatim from the
 * pre-redesign /diagnostics page (ADR-0111 §1/§4); the job queue is
 * jobs.tsx (the file-size policy).
 */

const loadLogs = (signal: AbortSignal) =>
  gatedJson<{ limit: number; count: number; records: LogRecord[]; note: string }>(
    "/api/v1/diagnostics/logs?limit=200",
    undefined,
    signal,
  );

export function SystemLogsSection() {
  const logs = useGated(loadLogs, true);

  return (
    <section className="panel" data-testid="system-logs">
      <h2>System logs</h2>
      <p className="muted">
        The newest records of the process log, every value passed the support-bundle redactor (URLs,
        tokens and private IPs replaced) before it left the process. The tail is process-local: a
        restart clears it, and the durable redacted path is the support bundle.
      </p>
      {logs.state.kind === "loading" ? (
        <p className="muted">Reading the log tail…</p>
      ) : logs.state.kind === "unauthenticated" ? (
        <p className="muted" data-testid="system-logs-sign-in">
          The log tail needs a signed-in session. <a href="/login">Sign in</a> to read it.
        </p>
      ) : logs.state.kind === "forbidden" ? (
        <p className="muted" data-testid="system-logs-developer">
          The log tail is developer tier: system logs can carry provider request detail, so the
          scope table grants it to the developer role only.
        </p>
      ) : logs.state.kind === "error" ? (
        <div role="alert" className="error" data-testid="system-logs-error">
          <p>
            The log read answered HTTP {logs.state.status}: {logs.state.detail}
          </p>
          <button type="button" onClick={logs.refetch}>
            Try again
          </button>
        </div>
      ) : (
        <LogsTable logs={logs.state.body} />
      )}
    </section>
  );
}

function LogsTable({
  logs,
}: {
  logs: { limit: number; count: number; records: LogRecord[]; note: string };
}) {
  if (logs.count === 0) {
    return (
      <p className="muted" data-testid="system-logs-empty">
        No records in the ring yet — the process has not logged anything since it booted (or the
        ring rolled over).
      </p>
    );
  }
  const newest = logs.records.slice(-20).reverse();
  return (
    <div className="stack">
      <p className="muted">
        {logs.count} record{logs.count === 1 ? "" : "s"} in the ring; showing the newest{" "}
        {newest.length}. {logs.note}
      </p>
      <table>
        <thead>
          <tr>
            <th>Time</th>
            <th>Level</th>
            <th>Message</th>
          </tr>
        </thead>
        <tbody>
          {newest.map((record, i) => (
            <tr key={`${record.time}-${i}`} data-testid="log-row">
              <td>{new Date(record.time).toLocaleTimeString()}</td>
              <td>{record.level}</td>
              <td>
                {record.msg}
                {Object.entries(record.attrs ?? {})
                  .map(([key, value]) => `${key}=${value}`)
                  .join(" ")}
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}

export function ReadinessSection() {
  const readiness = useQuery({
    queryKey: ["health", "ready"],
    queryFn: async ({ signal }) => {
      const response = await fetch("/api/v1/health/ready", {
        credentials: "same-origin",
        signal,
      });
      if (!response.ok) throw new Error(`the readiness probe answered ${response.status}`);
      return (await response.json()) as Readiness;
    },
    refetchInterval: 10_000,
  });

  if (readiness.isError) {
    return (
      <section className="panel" data-testid="readiness-checks">
        <h2>Readiness checks</h2>
        <ErrorState error={readiness.error} action="Cannot read readiness" />
      </section>
    );
  }
  if (!readiness.data) {
    return (
      <section className="panel" data-testid="readiness-checks">
        <h2>Readiness checks</h2>
        <p className="muted">Loading…</p>
      </section>
    );
  }
  const ready = readiness.data;
  const checks = Object.entries(ready.checks ?? {});
  return (
    <section className="panel" data-testid="readiness-checks">
      <h2>
        Readiness checks{" "}
        <span className="muted" data-testid="readiness">
          {ready.status}
        </span>
      </h2>
      {checks.length === 0 ? (
        <p className="muted">
          No dependency has probed yet: the check map is empty, which the API reports as
          &ldquo;nothing is wired&rdquo; rather than &ldquo;everything works&rdquo;.
        </p>
      ) : (
        <table className="checks">
          <thead>
            <tr>
              <th>Dependency</th>
              <th>Status</th>
              <th>Detail</th>
            </tr>
          </thead>
          <tbody>
            {checks.map(([name, detail]) => (
              <CheckRow key={name} name={name} detail={detail} />
            ))}
          </tbody>
        </table>
      )}
    </section>
  );
}

function CheckRow({ name, detail }: { name: string; detail: Record<string, unknown> }) {
  const status = typeof detail.status === "string" ? detail.status : "unprobed";
  const extras = Object.entries(detail).filter(([key]) => key !== "status");
  return (
    <tr className={`check-row check-${status}`} data-testid={`readiness-check-${name}`}>
      <td>{name}</td>
      <td>
        <span className={`badge ${status === "ok" ? "badge-succeeded" : "badge-failed"}`}>
          {status}
        </span>
      </td>
      <td>
        {extras.length === 0
          ? "—"
          : extras
              .map(
                ([key, value]) =>
                  `${key}: ${typeof value === "string" ? value : JSON.stringify(value)}`,
              )
              .join(" · ")}
      </td>
    </tr>
  );
}
