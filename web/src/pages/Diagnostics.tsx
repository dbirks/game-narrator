import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";

import { fetchReadiness, fetchSystemInfo, type Readiness } from "../api/system";
import { ErrorState } from "../components/states";

/**
 * `/diagnostics` — docs/spec/09: "video input modes, power/environment
 * warnings, CPU/RAM/storage, network and each model role, voice registry
 * replication, speaker test, system logs (redacted)". This page renders what
 * the API can answer in this build: build/machine facts, the readiness checks
 * with their detail, and the durable job queue's shape. What it cannot answer
 * is named as such — model-role health and the voice registry are not exposed
 * by any endpoint in this build (the readiness probe reports the database
 * only), and a diagnostics page that invents them is worse than one that
 * points at the gap.
 *
 * The page is public (ADR-0016 §3); the queue section is the one part that
 * needs a credential, and it says so instead of failing.
 */

const JOB_STATES = ["queued", "running", "succeeded", "failed", "cancelled"] as const;

interface JobSummaryRow {
  id: string;
  kind: string;
  state: string;
  created_at?: string;
}

export function DiagnosticsPage() {
  const system = useQuery({
    queryKey: ["system", "info"],
    queryFn: ({ signal }) => fetchSystemInfo(signal),
  });
  const readiness = useQuery({
    queryKey: ["health", "ready"],
    queryFn: ({ signal }) => fetchReadiness(signal),
    refetchInterval: 10_000,
  });

  // The queue needs a credential and this page is public: a 401 here is the
  // "sign in to see this" state, not a fault the page should shout about. A raw
  // fetch on purpose — the shared client would treat the 401 as a dead session
  // and mutate the auth state from a page that is meant to work without one.
  const jobs = useQuery({
    queryKey: ["diagnostics", "jobs"],
    queryFn: async ({ signal }) => {
      const page = (await fetch("/api/v1/jobs?limit=200", {
        credentials: "same-origin",
        signal,
      })) as Response;
      if (page.status === 401) return { unauthenticated: true } as const;
      if (!page.ok) throw new Error(`the job queue answered ${page.status}`);
      const body = (await page.json()) as { jobs: JobSummaryRow[] };
      return { jobs: body.jobs ?? [] } as const;
    },
    retry: false,
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
      <>
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
        <ReadinessChecks readiness={ready} />
        <JobQueue jobs={jobs.data} jobsError={jobs.error} />
        <ModelRoleHealth readiness={ready} />
      </>
    );
  }

  return (
    <main>
      <h1>Diagnostics</h1>
      {body}
    </main>
  );
}

/** The per-dependency detail behind the readiness line: what probed, what it said. */
function ReadinessChecks({ readiness }: { readiness: Readiness }) {
  const checks = Object.entries(readiness.checks ?? {});
  return (
    <section className="panel" data-testid="readiness-checks">
      <h2>Readiness checks</h2>
      {checks.length === 0 ? (
        <p className="muted">
          No dependency has probed yet: the check map is empty, which the API reports as
          &ldquo;nothing is wired&rdquo; rather than &ldquo;everything works&rdquo;.
        </p>
      ) : (
        <table className="checks" data-testid="readiness-check-table">
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

function JobQueue({
  jobs,
  jobsError,
}: {
  jobs: { unauthenticated: true } | { jobs: JobSummaryRow[] } | undefined;
  jobsError: unknown;
}) {
  const rows = "jobs" in (jobs ?? {}) ? (jobs as { jobs: JobSummaryRow[] }).jobs : [];
  const counts = new Map<string, number>();
  for (const row of rows) counts.set(row.state, (counts.get(row.state) ?? 0) + 1);
  return (
    <section className="panel" data-testid="job-queue">
      <h2>Job queue</h2>
      {jobs && "unauthenticated" in jobs ? (
        <p className="muted" data-testid="jobs-unauthenticated">
          The queue needs a signed-in session; sign in to see its state.
        </p>
      ) : jobsError ? (
        <p className="error" role="alert">
          Cannot read the job queue:{" "}
          {jobsError instanceof Error ? jobsError.message : "network error"}
        </p>
      ) : rows.length === 0 ? (
        <p className="muted" data-testid="jobs-empty">
          No jobs recorded.
        </p>
      ) : (
        <>
          <p data-testid="jobs-summary">
            {JOB_STATES.filter((state) => counts.has(state)).map((state) => (
              <span key={state} className="badge badge-plain" data-testid={`jobs-count-${state}`}>
                {state}: {counts.get(state)}
              </span>
            ))}
          </p>
          <table className="jobs">
            <thead>
              <tr>
                <th>Job</th>
                <th>Kind</th>
                <th>State</th>
                <th>Created</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.id} data-testid="job-row">
                  <td>
                    <code>{row.id}</code>
                  </td>
                  <td>{row.kind}</td>
                  <td>{row.state}</td>
                  <td>{row.created_at ?? "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      )}
    </section>
  );
}

/**
 * The model-role health that docs/spec/09 asks for, answered honestly: the
 * readiness probe in this build reports the database only, so the model roles
 * have no exposed health and the page says that instead of drawing an empty
 * table that reads as "all models fine".
 */
function ModelRoleHealth({ readiness }: { readiness: Readiness }) {
  const modelChecks = Object.entries(readiness.checks ?? {}).filter(([name]) =>
    name.startsWith("model"),
  );
  return (
    <section className="panel" data-testid="model-health">
      <h2>Model roles</h2>
      {modelChecks.length > 0 ? (
        <p>
          {modelChecks
            .map(
              ([name, detail]) =>
                `${name}: ${String((detail as { status?: string }).status ?? "unprobed")}`,
            )
            .join(" · ")}
        </p>
      ) : (
        <p className="muted" data-testid="model-health-unavailable">
          Per-model-role health is not available in this build: the readiness probe reports the
          database only, and no endpoint exposes a model-role probe. Circuit-breaker state lives in
          the server log until a probe lands.
        </p>
      )}
    </section>
  );
}
