import { useQuery } from "@tanstack/react-query";

/**
 * `src/pages/health/jobs.tsx` — the preserved job-queue section (the
 * diagnostics run's queued/running/succeeded/failed/cancelled counts and
 * rows), moved verbatim from the pre-redesign /diagnostics page (ADR-0111
 * §1); split out of logs.tsx to keep each file under the size policy.
 */

const JOB_STATES = ["queued", "running", "succeeded", "failed", "cancelled"] as const;

interface JobSummaryRow {
  id: string;
  kind: string;
  state: string;
  created_at?: string;
}
export function JobQueueSection() {
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

  const rows = "jobs" in (jobs.data ?? {}) ? (jobs.data as { jobs: JobSummaryRow[] }).jobs : [];
  const counts = new Map<string, number>();
  for (const row of rows) counts.set(row.state, (counts.get(row.state) ?? 0) + 1);
  return (
    <section className="panel" data-testid="job-queue">
      <h2>Job queue</h2>
      {jobs.data && "unauthenticated" in jobs.data ? (
        <p className="muted" data-testid="jobs-unauthenticated">
          The queue needs a signed-in session; sign in to see its state.
        </p>
      ) : jobs.isError ? (
        <p className="error" role="alert">
          Cannot read the job queue:{" "}
          {jobs.error instanceof Error ? jobs.error.message : "network error"}
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
