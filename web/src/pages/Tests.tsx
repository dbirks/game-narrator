import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import type { ReactNode } from "react";

import { apiStatus } from "../api/client";
import {
  fetchDevTests,
  runScenario,
  scenarioReportUrl,
  scenarioRunStatus,
  type DevTests,
  type GateView,
  type ScenarioRunStatus,
  type ScenarioView,
} from "../api/settings";
import { ErrorState } from "../components/states";

/**
 * `/tests` — docs/spec/09: the scenario catalog with run controls, gate
 * results, Arch Dolphin readiness, live-provider/hardware status, and the
 * HTML report viewer/download. A run is a durable job (ADR-0047): the button
 * enqueues, the row polls the job's state, and the report opens in a sandboxed
 * iframe (`sandbox=""` — no scripts, no same-origin access; the report is a
 * static verdict page by construction, and so is its download).
 *
 * A BLOCKED scenario is a row that runs and says BLOCKED (ADR-0037: blocked is
 * not pass — but the page that shows the reason is the point of the page).
 */

type Run = { jobId: string; scenarioId: string; state?: string };

export function TestsPage() {
  const tests = useQuery({
    queryKey: ["dev", "tests"],
    queryFn: ({ signal }) => fetchDevTests(signal),
    retry: false,
  });
  const [run, setRun] = useState<Run | null>(null);
  const [runError, setRunError] = useState<string | null>(null);
  const [viewing, setViewing] = useState<{
    scenarioId: string;
    report?: ScenarioRunStatus["report"];
  } | null>(null);

  const status = useQuery({
    queryKey: ["dev", "tests", "run", run?.jobId ?? ""],
    queryFn: ({ signal }) => scenarioRunStatus(run!.jobId, signal),
    enabled: run !== null,
    refetchInterval: (query) =>
      query.state.data && isTerminal(query.state.data.state) ? false : 1000,
  });

  let body: ReactNode;
  if (tests.error) {
    const status_ = apiStatus(tests.error);
    body =
      status_ === 401 || status_ === 403 ? (
        <p className="muted">
          The test harness is a developer view: sign in with a developer or administrator account to
          read the catalog and run scenarios.
        </p>
      ) : (
        <ErrorState
          error={tests.error}
          action="Test harness unavailable"
          onRetry={() => void tests.refetch()}
        />
      );
  } else if (!tests.data) {
    body = <p className="muted">Loading the test harness…</p>;
  } else {
    body = (
      <div>
        {!tests.data.repo_root_known && (
          <p role="status" className="muted" data-testid="harness-not-wired">
            No scenario catalog is wired into this process (the repository root is not known): set
            GAME_NARRATOR_REPO_ROOT to the checkout to list and run scenarios.
          </p>
        )}

        <section aria-labelledby="tests-scenarios">
          <h2 id="tests-scenarios">Scenarios</h2>
          {tests.data.scenarios.length === 0 ? (
            <p className="muted">
              No scenarios found under testdata/scenarios (the catalog directory is{" "}
              {tests.data.scenario_dir || "not resolvable"}).
            </p>
          ) : (
            <table className="table" data-testid="scenario-catalog">
              <thead>
                <tr>
                  <th>Scenario</th>
                  <th>Catalog</th>
                  <th>Mode</th>
                  <th>Origin</th>
                  <th>State</th>
                  <th>Run</th>
                </tr>
              </thead>
              <tbody>
                {tests.data.scenarios.map((s) => (
                  <ScenarioRow key={s.id} scenario={s} onRun={() => void onRun(s)} />
                ))}
              </tbody>
            </table>
          )}
        </section>

        {run && (
          <section aria-labelledby="tests-run" data-testid="run-status">
            <h2 id="tests-run">Run</h2>
            <RunStatus
              scenarioId={run.scenarioId}
              error={status.error}
              status={status.data}
              onRetry={() => void status.refetch()}
              onDone={() => setRun(null)}
              onViewReport={(report) => setViewing({ scenarioId: run.scenarioId, report })}
            />
          </section>
        )}

        {viewing && (
          <section aria-labelledby="tests-report" data-testid="report-viewer">
            <h2 id="tests-report">Report — {viewing.scenarioId}</h2>
            {viewing.report === undefined ? (
              <p className="muted">Waiting for the report link…</p>
            ) : (
              <>
                <iframe
                  title={`Report for ${viewing.scenarioId}`}
                  src={scenarioReportUrl(viewing.report!.html)}
                  sandbox=""
                  data-testid="report-iframe"
                />
                <p>
                  <a
                    href={scenarioReportUrl(viewing.report!.html)}
                    download
                    data-testid="report-download"
                  >
                    Download the report
                  </a>
                </p>
              </>
            )}
          </section>
        )}

        <section aria-labelledby="tests-gates">
          <h2 id="tests-gates">Gates</h2>
          <GateTable gates={tests.data.gates} />
        </section>

        <section aria-labelledby="tests-dolphin">
          <h2 id="tests-dolphin">Arch Dolphin readiness</h2>
          <DolphinView data={tests.data.dolphin} />
        </section>

        <section aria-labelledby="tests-providers">
          <h2 id="tests-providers">Live providers and hardware</h2>
          <ProviderView data={tests.data.providers} />
        </section>

        {runError && (
          <p role="alert" className="error" data-testid="run-error">
            {runError}
          </p>
        )}
      </div>
    );
  }

  return (
    <main className="page">
      <h1>Tests</h1>
      {body}
    </main>
  );

  async function onRun(scenario: ScenarioView) {
    setRunError(null);
    setViewing(null);
    try {
      const accepted = await runScenario(scenario.id);
      setRun({ jobId: accepted.job_id, scenarioId: scenario.id });
    } catch (error) {
      setRunError(error instanceof Error ? error.message : "the run request failed");
    }
  }
}

function ScenarioRow({ scenario, onRun }: { scenario: ScenarioView; onRun: () => void }) {
  return (
    <tr data-testid={`scenario-${scenario.id}`}>
      <td>{scenario.id}</td>
      <td>{scenario.catalog ?? "—"}</td>
      <td>{scenario.mode ?? "—"}</td>
      <td>{scenario.origin ?? "—"}</td>
      <td>
        <span className={`pill ${scenario.status === "runnable" ? "pill-ok" : "pill-warn"}`}>
          {scenario.status}
        </span>
        {scenario.reason ? <span className="muted"> — {scenario.reason}</span> : null}
      </td>
      <td>
        <button type="button" onClick={onRun} disabled={scenario.status !== "runnable"}>
          Run
        </button>
      </td>
    </tr>
  );
}

function isTerminal(state?: string): boolean {
  return state === "succeeded" || state === "failed" || state === "cancelled";
}

function RunStatus({
  error,
  status,
  onRetry,
  onDone,
  onViewReport,
}: {
  scenarioId: string;
  error: unknown;
  status?: ScenarioRunStatus;
  onRetry: () => void;
  onDone: () => void;
  onViewReport: (report: ScenarioRunStatus["report"]) => void;
}) {
  if (error) {
    return <ErrorState error={error} action="The run status is unavailable" onRetry={onRetry} />;
  }
  if (!status) {
    return <p className="muted">Queued…</p>;
  }
  const terminal = isTerminal(status.state);
  // The harness verdict vocabulary (internal/scenario.RunResult) is
  // "passed" | "failed" | "blocked" — the report page's badge CSS and the
  // job-failure path ("failed") use the same words, so the pills key on
  // exactly those values.
  const verdict =
    status.status === "passed" ? (
      <span className="pill pill-ok" data-testid="run-verdict">
        PASS
      </span>
    ) : status.status === "blocked" ? (
      <span className="pill pill-warn" data-testid="run-verdict">
        BLOCKED
      </span>
    ) : status.status === "failed" ? (
      <span className="pill pill-error" data-testid="run-verdict">
        FAIL
      </span>
    ) : null;

  return (
    <p data-testid="run-state">
      {status.state}
      {status.summary ? ` — ${status.summary}` : ""}
      {verdict}
      {status.report ? (
        <button type="button" onClick={() => onViewReport(status.report!)}>
          Open the report
        </button>
      ) : null}
      {terminal ? (
        <button type="button" onClick={onDone}>
          Close
        </button>
      ) : null}
    </p>
  );
}

function GateTable({ gates }: { gates: GateView[] }) {
  return (
    <table className="table" data-testid="gate-results">
      <thead>
        <tr>
          <th>Gate</th>
          <th>Result</th>
          <th>Exit code</th>
          <th>Tests</th>
          <th>Detail</th>
        </tr>
      </thead>
      <tbody>
        {gates.map((gate) => (
          <tr key={gate.name}>
            <td>{gate.name}</td>
            <td>
              <span
                className={`pill ${
                  gate.result === "PASS"
                    ? "pill-ok"
                    : gate.result === "FAIL"
                      ? "pill-error"
                      : "pill-warn"
                }`}
                data-testid={`gate-${gate.name}`}
              >
                {gate.result}
              </span>
            </td>
            <td>{gate.exit_code ?? "—"}</td>
            <td>{gate.tests ?? "—"}</td>
            <td>
              {(gate.failures ?? 0) > 0 || (gate.errors ?? 0) > 0 ? (
                <span className="muted">
                  {gate.failures ?? 0} failures, {gate.errors ?? 0} errors, {gate.skipped ?? 0}{" "}
                  skipped.{" "}
                </span>
              ) : null}
              {gate.instruction}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Flag({ label, ok }: { label: string; ok: boolean }) {
  return (
    <span className={`pill ${ok ? "pill-ok" : "pill-warn"}`}>
      {label}: {ok ? "ready" : "missing"}
    </span>
  );
}

function DolphinView({ data }: { data: DevTests["dolphin"] }) {
  return (
    <dl className="grid-defs" data-testid="dolphin-readiness">
      <dt>Dev flag</dt>
      <dd>
        <Flag label="dev mode" ok={data.dev_mode_enabled} />
      </dd>
      <dt>Binary</dt>
      <dd>
        <Flag label="dolphin-emu on PATH" ok={data.dolphin_binary_present} />
      </dd>
      <dt>Display server</dt>
      <dd>
        <Flag label="Xvfb on PATH" ok={data.xvfb_present} />
      </dd>
      <dt>Game image</dt>
      <dd>
        <Flag label={data.env[0] ?? "GAME_NARRATOR_GAME_IMAGE_PATH"} ok={data.game_image_env_set} />
        <span className="muted">
          {" "}
          (presence only: the path itself is an operational detail and never leaves the process)
        </span>
      </dd>
    </dl>
  );
}

function ProviderView({ data }: { data: DevTests["providers"] }) {
  const missing = data.missing.length === 0;
  return (
    <dl className="grid-defs" data-testid="provider-readiness">
      <dt>Configured roles</dt>
      <dd>
        {data.configured.length === 0 ? (
          <span className="muted">none</span>
        ) : (
          data.configured.map((role) => <span key={role}>{role} </span>)
        )}
      </dd>
      <dt>Missing roles</dt>
      <dd>
        {missing ? (
          <span className="muted">none</span>
        ) : (
          data.missing.map((role) => <span key={role}>{role} </span>)
        )}
      </dd>
      <dt>Live tests</dt>
      <dd>
        {missing
          ? "Live provider tests (make test-models) can run against the configured endpoints."
          : "Live provider tests will report NOT CONFIGURED for the missing roles above."}
      </dd>
    </dl>
  );
}
