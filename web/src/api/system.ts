// Typed REST access to the introspection endpoints of docs/spec/08.
// Field names mirror the Go JSON payloads in internal/api/api.go.

/** Response of GET /api/v1/system/info. */
export interface SystemInfo {
  version: string;
  commit: string;
  go_version: string;
  os: string;
  arch: string;
  num_cpu: number;
  started_at: string;
}

/** Response of GET /api/v1/health/ready. */
export interface Readiness {
  status: string;
  checks: Record<string, string>;
}

/** HTTP failure that carries the status code so pages can display it. */
export class ApiError extends Error {
  readonly status: number;

  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

/** The HTTP status behind an error, or null when it is not an HTTP failure. */
export function apiStatus(error: unknown): number | null {
  return error instanceof ApiError ? error.status : null;
}

async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await fetch(path, {
    headers: { accept: "application/json" },
    signal,
  });
  if (!response.ok) {
    throw new ApiError(response.status, `GET ${path} failed with HTTP ${response.status}`);
  }
  return (await response.json()) as T;
}

export function fetchSystemInfo(signal?: AbortSignal): Promise<SystemInfo> {
  return getJson<SystemInfo>("/api/v1/system/info", signal);
}

export function fetchReadiness(signal?: AbortSignal): Promise<Readiness> {
  return getJson<Readiness>("/api/v1/health/ready", signal);
}
