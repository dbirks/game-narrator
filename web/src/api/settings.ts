// Typed REST access for the /settings and /tests pages (docs/spec/09), on the
// generated contract types. The two things that do not fit the JSON client
// live here instead of in client.ts:
//
// - the **backup download** (POST /api/v1/admin/backup) — the client decodes
//   JSON, but this one returns a database file;
// - the **scenario run path** — a scenario id carries its directory
//   (`synthetic/dialogue-advance-001`), which is why the contract's template is
//   `{rest...}` and the id is percent-encoded per segment, not as a whole.

import { api, ApiError } from "./client";
import type { components, paths } from "./schema";

export type Settings = components["schemas"]["Settings"];
export type DevTests = components["schemas"]["DevTests"];
export type ScenarioView = components["schemas"]["ScenarioView"];
export type GateView = components["schemas"]["GateView"];

const settingsPath = "/api/v1/settings" as const;
const devTestsPath = "/api/v1/dev/tests" as const;
const runPath = "/api/v1/dev/tests/scenarios/{rest...}" as const;
const runStatusPath = "/api/v1/dev/tests/runs/{job_id}" as const;
const reportPath = "/api/v1/dev/tests/reports/{sha256}" as const;

export function fetchSettings(signal?: AbortSignal): Promise<Settings> {
  return api.get(settingsPath, { signal }) as Promise<Settings>;
}

export function fetchDevTests(signal?: AbortSignal): Promise<DevTests> {
  return api.get(devTestsPath, { signal }) as Promise<DevTests>;
}

/** The percent-encoded scenario id for the contract's `{rest...}` template. */
export function scenarioRunRest(id: string): string {
  return id.split("/").map(encodeURIComponent).join("/");
}

/** Enqueue the scenario run; the answer is the durable job id. */
export function runScenario(id: string, signal?: AbortSignal): Promise<{ job_id: string }> {
  const op = api.post(runPath, {
    params: { "rest...": `${scenarioRunRest(id)}/run` },
    signal,
  });
  return op as Promise<{ job_id: string }>;
}

export type ScenarioRunStatus = {
  job_id: string;
  kind: string;
  state: "queued" | "running" | "recoverable" | "succeeded" | "failed" | "cancelled";
  scenario_id?: string;
  attempts: number;
  started_at?: string;
  finished_at?: string;
  error?: string;
  summary?: string;
  // The harness verdict, verbatim from internal/scenario.RunResult:
  // "passed" | "failed" | "blocked" (and "failed" for a run job that died
  // before the harness answered).
  status?: "passed" | "failed" | "blocked";
  report?: { json: string; html: string };
};

export function scenarioRunStatus(jobId: string, signal?: AbortSignal): Promise<ScenarioRunStatus> {
  const op = api.get(runStatusPath, {
    params: { job_id: jobId },
    signal,
  });
  return op as Promise<ScenarioRunStatus>;
}

/** The report URL the sandboxed iframe and the download link both use. */
export function scenarioReportUrl(sha256: string): string {
  return api.buildUrl(reportPath, { params: { sha256 } });
}

const CSRF_HEADER = "X-CSRF-Token";

function csrfTokenFromCookie(): string | null {
  const jar = globalThis.document?.cookie ?? "";
  for (const part of jar.split(";")) {
    const index = part.indexOf("=");
    if (index < 0) continue;
    const name = part.slice(0, index).trim();
    if (name === "gn_csrf") return decodeURIComponent(part.slice(index + 1));
  }
  return null;
}

/**
 * Download the SQLite backup. A direct POST cannot ride browser navigation, so
 * this one goes through fetch with the CSRF twin cookie's token, then hands the
 * bytes to an anchor click — the only code path in the app that does that.
 */
export async function downloadBackup(): Promise<void> {
  const token = api.csrfTokenValue() ?? csrfTokenFromCookie();
  const response = await fetch("/api/v1/admin/backup", {
    method: "POST",
    credentials: "same-origin",
    headers: token ? { [CSRF_HEADER]: token } : {},
  });
  if (!response.ok) {
    throw new ApiError({
      status: response.status,
      code: "http-" + response.status,
      title: "Backup failed",
      detail: `the backup download answered HTTP ${response.status}`,
    });
  }
  const blob = await response.blob();
  const url = URL.createObjectURL(blob);
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = "game-narrator-backup.sqlite";
  document.body.appendChild(anchor);
  anchor.click();
  anchor.remove();
  URL.revokeObjectURL(url);
}

// The re-exports keep the pages importing everything from one module.
export type { paths };
