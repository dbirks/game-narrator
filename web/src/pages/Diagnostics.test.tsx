import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Readiness, SystemInfo } from "../api/system";
import { DiagnosticsPage } from "./Diagnostics";

const systemInfo: SystemInfo = {
  version: "v0.0.0-test",
  commit: "abcdef1",
  go_version: "go1.27.2",
  os: "linux",
  arch: "arm64",
  num_cpu: 4,
  started_at: "2026-10-16T00:00:00Z",
};

const readiness: Readiness = { status: "ok", checks: {} };

/** Minimal Response stand-in; jsdom provides no fetch implementation. */
function fakeResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    json: async () => body,
  } as unknown as Response;
}

function installFetch(responder: (path: string) => Response) {
  const mock = vi.fn((input: RequestInfo | URL): Promise<Response> => {
    const url = typeof input === "string" ? input : input.toString();
    return Promise.resolve(responder(new URL(url, "http://localhost").pathname));
  });
  globalThis.fetch = mock as unknown as typeof fetch;
  return mock;
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <DiagnosticsPage />
    </QueryClientProvider>,
  );
}

let originalFetch: typeof fetch;

beforeEach(() => {
  originalFetch = globalThis.fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  cleanup();
});

// Synthetic stub, docs/spec/12 "Unit" mode: no Go server is involved.
describe("DiagnosticsPage (unit test with a stubbed fetch fake)", () => {
  it("shows a loading state, then version, commit, platform and readiness", async () => {
    installFetch((path) =>
      path === "/api/v1/system/info"
        ? fakeResponse(200, systemInfo)
        : path === "/api/v1/health/ready"
          ? fakeResponse(200, readiness)
          : fakeResponse(404, { title: "Not Found" }),
    );

    renderPage();
    expect(screen.getByText("Loading…")).toBeInTheDocument();

    expect(await screen.findByTestId("version")).toHaveTextContent("v0.0.0-test");
    expect(screen.getByTestId("commit")).toHaveTextContent("abcdef1");
    expect(screen.getByTestId("platform")).toHaveTextContent("linux/arm64");
    expect(screen.getByTestId("readiness")).toHaveTextContent("ok");
    expect(screen.queryByText("Loading…")).not.toBeInTheDocument();
  });

  it("renders an error alert with the HTTP status when the API returns 503", async () => {
    installFetch(() => fakeResponse(503, { title: "Service Unavailable" }));

    renderPage();

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("HTTP 503");
    expect(screen.queryByTestId("version")).not.toBeInTheDocument();
  });
});
