/**
 * `src/api/diagnostics.ts` — typed access for the /diagnostics surface
 * (ADR-0087, docs/spec/09): the public host vitals, the model-role probe
 * runs that execute the doctor's own checks server-side, and the redacted
 * tail of the process log (developer tier).
 *
 * The page's honesty rules live in the contract and this client passes them
 * through: `host.ram` is null when the kernel exposed nothing, a role row's
 * `status` is the doctor's own vocabulary (a NOT CONFIGURED row is an answer,
 * not an error), and a run that fell out of the bounded store is a 404
 * (problem), which the page renders as "expired", never a retry loop.
 */

import { api } from "./client";
import type { components } from "./schema";

export type HostReport = components["schemas"]["HostReport"];
export type DiagnosticsRunRequest = components["schemas"]["DiagnosticsRunRequest"];
export type DiagnosticsRunRef = components["schemas"]["DiagnosticsRunRef"];
export type DiagnosticsRun = components["schemas"]["DiagnosticsRun"];
export type DiagnosticsRole = components["schemas"]["DiagnosticsRole"];
export type LogRecord = components["schemas"]["LogRecord"];
export type DiagnosticsLogs = components["schemas"]["DiagnosticsLogs"];

/** The seven model roles of docs/spec/06, in declaration order: the
 *  diagnostics page and the setup wizard enumerate them from here so the UI
 *  cannot invent an eighth role the doctor would refuse with a 400. */
export const MODEL_ROLES = [
  "agent_reasoning",
  "fast_reasoning",
  "vision_ocr_fallback",
  "decision",
  "voice_design",
  "voice_clone",
  "speech_asr",
] as const;

export type ModelRole = (typeof MODEL_ROLES)[number];

const hostPath = "/api/v1/diagnostics/host" as const;
const runPath = "/api/v1/diagnostics/run" as const;
const runIdPath = "/api/v1/diagnostics/{id}" as const;
const logsPath = "/api/v1/diagnostics/logs" as const;

/** The public host section: CPU, RAM, storage, thermal, the derived warnings. */
export function fetchDiagnosticsHost(signal?: AbortSignal): Promise<HostReport> {
  return api.get(hostPath, { signal }) as Promise<HostReport>;
}

/**
 * Start a diagnostics run (operator): the host snapshot plus the doctor's
 * probe of the named roles. Answers 202 with the run id; the result arrives
 * on GET — a role probe's budget is minutes, and the 202 is the only body
 * this route carries.
 */
export function startDiagnosticsRun(
  roles: readonly string[] = [],
  signal?: AbortSignal,
): Promise<DiagnosticsRunRef> {
  return api.post(runPath, { body: { roles: [...roles] }, signal }) as Promise<DiagnosticsRunRef>;
}

/** One run's current shape; a 404 means unknown or already evicted. */
export function fetchDiagnosticsRun(id: string, signal?: AbortSignal): Promise<DiagnosticsRun> {
  return api.get(runIdPath, { params: { id }, signal }) as Promise<DiagnosticsRun>;
}

/**
 * The recent system logs, redacted (developer tier like the dev-trace
 * evidence route). `limit` defaults to 200 server-side, capped 1000.
 */
export function fetchDiagnosticsLogs(limit = 200, signal?: AbortSignal): Promise<DiagnosticsLogs> {
  return api.get(logsPath, { query: { limit }, signal }) as Promise<DiagnosticsLogs>;
}

/** Human size for the byte fields; -1 (the unmeasured sentinel) reads as "—". */
export function formatBytes(bytes: number | null | undefined): string {
  if (bytes == null || bytes < 0) return "—";
  const units = ["B", "KiB", "MiB", "GiB", "TiB"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value >= 100 || unit === 0 ? Math.round(value) : value.toFixed(1)} ${units[unit]}`;
}

/**
 * A failure of one of the diagnostics surface's gated reads: the status is
 * the whole story (401 → sign in, 403 → role, 404 → expired, 503 → the
 * subsystem's own reason), and the detail is the server's problem text,
 * which the pages render as text — never interpreted.
 *
 * The reads are raw fetches on purpose: the /diagnostics page is public and
 * must not mutate the global auth state from a 401 it expects (an unwired
 * developer grant must not log the operator out of a working session), and
 * a host read must work with no account at all. Auth rides the session
 * cookie (credentials: same-origin), the same way the shared client's
 * requests do; unsafe methods carry the CSRF token the auth context
 * re-echoes from /auth/me (ADR-0016 §4).
 */
export class GatedError extends Error {
  constructor(
    readonly status: number,
    readonly detail: string,
  ) {
    super(`http ${status}: ${detail}`);
    this.name = "GatedError";
  }
}

/** The pages' per-endpoint state machine: the honest states, never a fake OK. */
export type GatedState<T> =
  | { kind: "loading" }
  | { kind: "unauthenticated" }
  | { kind: "forbidden" }
  | { kind: "error"; status: number; detail: string }
  | { kind: "ok"; body: T };

/**
 * Fetch one endpoint with the session's credentials (and the CSRF token for
 * unsafe methods) and decode problems into GatedError. An empty 2xx body
 * comes back as undefined, like a 204.
 */
export async function gatedJson<T>(
  path: string,
  init?: { method?: string; body?: unknown },
  signal?: AbortSignal,
): Promise<T> {
  const headers: Record<string, string> = { accept: "application/json" };
  const unsafe = (init?.method ?? "GET").toUpperCase() !== "GET";
  if (unsafe) {
    const token = api.csrfTokenValue();
    if (token) headers["x-csrf-token"] = token;
    if (init?.body !== undefined) headers["content-type"] = "application/json";
  }
  const response = await fetch(path, {
    credentials: "same-origin",
    headers,
    method: init?.method ?? "GET",
    body: init?.body === undefined ? undefined : JSON.stringify(init.body),
    signal,
  });
  const text = await response.text().catch(() => "");
  if (response.status === 401) throw new GatedError(401, "sign in required");
  if (response.status === 403) {
    throw new GatedError(403, "this section needs a higher role than the session holds");
  }
  if (!response.ok) {
    let detail = `the request answered HTTP ${response.status}`;
    try {
      const problem = JSON.parse(text) as { detail?: string; title?: string };
      if (problem.detail) detail = problem.detail;
      else if (problem.title) detail = `${problem.title} (HTTP ${response.status})`;
    } catch {
      // the body was not a problem document; keep the status text
    }
    throw new GatedError(response.status, detail);
  }
  if (text === "") return undefined as T;
  return JSON.parse(text) as T;
}
