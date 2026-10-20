import { useEffect, useState } from "react";

import {
  GatedError,
  MODEL_ROLES,
  type DiagnosticsRun,
  type GatedState,
  gatedJson,
} from "../../api/diagnostics";
import { GatedHint, mapGated } from "./gated";

/**
 * `src/pages/health/model-roles.tsx` — the model-role probe section, moved
 * verbatim from the pre-redesign /diagnostics page (ADR-0111 §1): each row
 * is the doctor's own `providers.<role>` check run on the server, with the
 * page's run-poll and its honest signed-out / expired-run states.
 */

const PROBE_POLL_MS = 1_000;
const PROBE_MAX_POLLS = 300;

const startProbeRun = (signal?: AbortSignal) =>
  gatedJson<{ run_id: string }>(
    "/api/v1/diagnostics/run",
    {
      method: "POST",
      body: { roles: [...MODEL_ROLES] },
    },
    signal,
  );

const loadRun = (id: string, signal?: AbortSignal) =>
  gatedJson<DiagnosticsRun>(`/api/v1/diagnostics/${encodeURIComponent(id)}`, undefined, signal);

export function ModelRolesSection() {
  const [runId, setRunId] = useState<string | null>(null);
  const [run, setRun] = useState<GatedState<DiagnosticsRun>>({ kind: "loading" });
  const [startState, setStartState] = useState<GatedState<{ run_id: string }>>({
    kind: "loading",
  });

  // Poll the run while it is running; stop on done, 404 (evicted) or an error.
  useEffect(() => {
    if (!runId) return;
    const controller = new AbortController();
    let timer: number | undefined;
    let cancelled = false;
    let attempt = 0;
    const tick = () => {
      loadRun(runId, controller.signal)
        .then((body) => {
          if (cancelled) return;
          setRun({ kind: "ok", body });
          if (body.status === "running") {
            attempt += 1;
            if (attempt < PROBE_MAX_POLLS) {
              timer = window.setTimeout(tick, PROBE_POLL_MS);
            } else {
              setRun({
                kind: "error",
                status: 0,
                detail: "the run is still running after ten minutes of polling",
              });
            }
          }
        })
        .catch((error) => {
          if (cancelled || controller.signal.aborted) return;
          if (error instanceof GatedError && error.status === 404) {
            setRun({
              kind: "error",
              status: 404,
              detail: "the run expired from the bounded store (32 newest, one-hour TTL)",
            });
            return;
          }
          setRun(mapGated(error));
        });
    };
    tick();
    return () => {
      cancelled = true;
      controller.abort();
      if (timer !== undefined) window.clearTimeout(timer);
    };
  }, [runId]);

  const probeAll = () => {
    setStartState({ kind: "loading" });
    startProbeRun(undefined)
      .then((ref) => {
        setStartState({ kind: "ok", body: ref });
        setRun({ kind: "loading" });
        setRunId(ref.run_id);
      })
      .catch((error) => {
        setStartState(mapGated(error));
      });
  };

  const roles = (run.kind === "ok" ? run.body.roles : []) ?? [];
  const requested = (run.kind === "ok" ? run.body.requested : []) ?? [];

  return (
    <section className="panel" data-testid="model-roles">
      <h2>Model roles</h2>
      <p className="muted">
        Each row is the doctor&apos;s own <code>providers.&lt;role&gt;</code> check run on the
        server (the same probe <code>game-narrator doctor</code> runs): reachability, the measured
        answer latency, and the status. A role with no model answers NOT CONFIGURED — an answer, not
        an error.
      </p>
      <button type="button" onClick={probeAll} data-testid="model-roles-probe-all">
        Probe all roles
      </button>
      {startState.kind === "unauthenticated" ? (
        <p className="muted" data-testid="model-roles-sign-in">
          Probing the roles needs a signed-in session (operator role or higher).{" "}
          <a href="/login">Sign in</a> to run the probes.
        </p>
      ) : null}
      {startState.kind === "forbidden" ? (
        <p className="muted" data-testid="model-roles-role">
          Probing the roles needs the operator role or higher; this session does not hold it.
        </p>
      ) : null}
      {startState.kind === "error" ? (
        <div role="alert" className="error" data-testid="model-roles-error">
          <p>
            The probe did not start (HTTP {startState.status}): {startState.detail}
          </p>
        </div>
      ) : null}
      {runId ? (
        <>
          {run.kind === "ok" ? (
            <>
              <p className="muted" data-testid="model-roles-run-id">
                Run {run.body.id} —{" "}
                {run.body.status === "done"
                  ? `finished ${run.body.finished_at ? new Date(run.body.finished_at).toLocaleTimeString() : ""}`
                  : "running"}
                .
              </p>
              <table>
                <thead>
                  <tr>
                    <th>Role</th>
                    <th>Configured</th>
                    <th>Status</th>
                    <th>Latency</th>
                    <th>Detail</th>
                  </tr>
                </thead>
                <tbody>
                  {MODEL_ROLES.map((role) => {
                    const row = roles.find((candidate) => candidate.role === role);
                    return (
                      <tr key={role} data-testid={`model-role-row-${role}`}>
                        <td>
                          <code>{role}</code>
                        </td>
                        <td>
                          {row
                            ? row.configured
                              ? "yes"
                              : "no"
                            : requested.includes(role)
                              ? "…"
                              : "—"}
                        </td>
                        <td>
                          {row ? (
                            <span
                              className={`badge ${row.status === "OK" ? "badge-succeeded" : "badge-failed"}`}
                            >
                              {row.status}
                            </span>
                          ) : requested.includes(role) ? (
                            "…"
                          ) : (
                            "—"
                          )}
                        </td>
                        <td>{row ? `${row.duration_ms} ms` : ""}</td>
                        <td className="muted">{row?.summary ?? ""}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </>
          ) : run.kind === "loading" ? (
            <p className="muted" data-testid="model-roles-loading">
              Probing — the doctor's checks run server-side…
            </p>
          ) : run.kind === "error" ? (
            <div role="alert" className="error" data-testid="model-roles-run-error">
              <p>
                The run could not be read (HTTP {run.status}): {run.detail}
              </p>
            </div>
          ) : (
            <GatedHint state={run} action="Reading the probe run" refetch={() => undefined} />
          )}
        </>
      ) : (
        <p className="muted" data-testid="model-roles-idle">
          No probe has run on this page visit yet.
        </p>
      )}
    </section>
  );
}
