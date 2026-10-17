// Typed REST access to the introspection endpoints of docs/spec/08, on the
// generated contract types (src/api/client.ts). Kept as its own module because
// /diagnostics is the one page that must work before anybody has an account
// (docs/spec/09: the SPA serves its own login form, ADR-0016 §"Enforcement point").

import { api } from "./client";
import type { Readiness, SystemInfo } from "./client";

export type { Readiness, SystemInfo };
export { ApiError, apiCode, apiStatus } from "./client";

export function fetchSystemInfo(signal?: AbortSignal): Promise<SystemInfo> {
  return api.get("/api/v1/system/info", { signal }) as Promise<SystemInfo>;
}

export function fetchReadiness(signal?: AbortSignal): Promise<Readiness> {
  // The contract types `checks` as an open object map; the shape the page reads
  // is documented in internal/api/api.go's readiness handler.
  return api.get("/api/v1/health/ready", { signal }) as Promise<Readiness>;
}
