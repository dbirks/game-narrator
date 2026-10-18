import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { DevTests } from "../api/settings";
import { TestsPage } from "./Tests";

const RUNNABLE_ID = "synthetic/dialogue-advance-001";
const BLOCKED_ID = "synthetic/blocked-001";
const JOB_ID = "01JOBTEST0000000000000001";

const devTests: DevTests = {
  repo_root_known: true,
  repo_root_env: "GAME_NARRATOR_REPO_ROOT",
  scenario_dir: "testdata/scenarios",
  scenarios: [
    {
      id: RUNNABLE_ID,
      catalog: "GN-T001",
      file: "testdata/scenarios/synthetic/dialogue-advance-001.yaml",
      origin: "generated_synthetic",
      mode: "frame_replay",
      decision: "recorded",
      tts: "recorded",
      faults: 0,
      status: "runnable",
    },
    {
      id: BLOCKED_ID,
      catalog: "GN-T047",
      file: "testdata/scenarios/synthetic/blocked-001.yaml",
      origin: "generated_synthetic",
      mode: "frame_replay",
      decision: "none",
      tts: "none",
      faults: 0,
      status: "blocked",
      reason: "the TTS endpoint has no streamed/cancellable interface in this build",
    },
  ],
  gates: [
    {
      name: "test-unit",
      result: "PASS",
      exit_code: 0,
      tests: 42,
      failures: 0,
      errors: 0,
      skipped: 1,
      instruction: "make test-unit",
    },
    {
      name: "test-dolphin",
      result: "NOT RUN",
      instruction: "make test-dolphin (needs dolphin-emu and a game image on this host)",
    },
  ],
  dolphin: {
    dev_mode_enabled: false,
    dolphin_binary_present: false,
    xvfb_present: false,
    game_image_env_set: false,
    env: ["GAME_NARRATOR_GAME_IMAGE_PATH"],
  },
  providers: { configured: [], missing: ["decision", "fast_reasoning", "tts"] },
};

const running = {
  job_id: JOB_ID,
  kind: "scenario.run",
  state: "running",
  scenario_id: RUNNABLE_ID,
  attempts: 1,
};

const succeeded = {
  job_id: JOB_ID,
  kind: "scenario.run",
  state: "succeeded",
  scenario_id: RUNNABLE_ID,
  attempts: 1,
  finished_at: "2026-10-17T00:00:10Z",
  summary: "PASS: 2/2 assertions held",
  status: "passed",
  report: {
    json: "deadbeef",
    html: "cafebabe",
  },
};

let polls = 0;

function fakeResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => body,
    text: async () => JSON.stringify(body),
  } as unknown as Response;
}

function installFetch() {
  polls = 0;
  const mock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = new URL(typeof input === "string" ? input : input.toString(), "http://localhost");
    const method = (init?.method ?? "GET").toUpperCase();
    // The client percent-encodes the whole {rest...} value, so the scenario
    // id's separator arrives as %2F inside one segment: compare decoded.
    const path = decodeURIComponent(url.pathname);
    if (method === "GET" && path === "/api/v1/dev/tests") {
      return Promise.resolve(fakeResponse(200, devTests));
    }
    if (method === "POST" && path === `/api/v1/dev/tests/scenarios/${RUNNABLE_ID}/run`) {
      return Promise.resolve(fakeResponse(202, { job_id: JOB_ID, status: "queued" }));
    }
    if (method === "GET" && path === `/api/v1/dev/tests/runs/${JOB_ID}`) {
      polls += 1;
      return Promise.resolve(fakeResponse(200, polls < 2 ? running : succeeded));
    }
    return Promise.resolve(
      fakeResponse(404, {
        title: "Not Found",
        detail: `no stub for ${method} ${path}`,
      }),
    );
  });
  globalThis.fetch = mock as unknown as typeof fetch;
  return mock;
}

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <TestsPage />
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
describe("TestsPage (unit test with a stubbed fetch fake)", () => {
  it("lists the catalog with the blocked scenario's reason and disables its run", async () => {
    installFetch();
    renderPage();

    const catalog = await screen.findByTestId("scenario-catalog");
    const runnable = within(catalog).getByTestId(`scenario-${RUNNABLE_ID}`);
    expect(runnable).toHaveTextContent("GN-T001");
    expect(runnable).toHaveTextContent("runnable");
    const blocked = within(catalog).getByTestId(`scenario-${BLOCKED_ID}`);
    expect(blocked).toHaveTextContent("blocked");
    expect(blocked).toHaveTextContent("the TTS endpoint has no streamed/cancellable interface");
    expect(within(blocked).getByRole("button", { name: "Run" })).toBeDisabled();
    expect(within(runnable).getByRole("button", { name: "Run" })).toBeEnabled();

    const gates = screen.getByTestId("gate-results");
    expect(within(gates).getByTestId("gate-test-unit")).toHaveTextContent("PASS");
    expect(within(gates).getByTestId("gate-test-dolphin")).toHaveTextContent("NOT RUN");
    expect(screen.getByTestId("dolphin-readiness")).toHaveTextContent(
      "GAME_NARRATOR_GAME_IMAGE_PATH",
    );
    expect(screen.getByTestId("provider-readiness")).toHaveTextContent("decision");
  });

  it("runs a scenario, polls the job, and opens the script-free report in a sandboxed iframe", async () => {
    installFetch();
    renderPage();

    const runnable = await screen.findByTestId(`scenario-${RUNNABLE_ID}`);
    fireEvent.click(within(runnable).getByRole("button", { name: "Run" }));

    // The run section appears, polls the job (the stub answers running, then
    // succeeded on the second poll), and lands on the PASS verdict.
    const runState = await screen.findByTestId("run-state");
    await screen.findByTestId("run-verdict", {}, { timeout: 15_000 });
    expect(runState).toHaveTextContent("succeeded");
    expect(within(runState).getByTestId("run-verdict")).toHaveTextContent("PASS");
    expect(runState).toHaveTextContent("PASS: 2/2 assertions held");

    fireEvent.click(screen.getByRole("button", { name: "Open the report" }));

    const iframe = await screen.findByTestId("report-iframe");
    expect(iframe).toHaveAttribute("sandbox", "");
    expect(iframe).toHaveAttribute(
      "src",
      "http://localhost:3000/api/v1/dev/tests/reports/cafebabe",
    );
    const download = screen.getByTestId("report-download");
    expect(download).toHaveAttribute(
      "href",
      "http://localhost:3000/api/v1/dev/tests/reports/cafebabe",
    );
  });

  it("shows the harness-not-wired state when the root is unknown", async () => {
    installFetch();
    // A second stub: the same responder, minus the catalog — modelled by a
    // responder that answers the catalog route with the not-wired body.
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      const url = new URL(typeof input === "string" ? input : input.toString(), "http://localhost");
      const path = decodeURIComponent(url.pathname);
      if (path === "/api/v1/dev/tests") {
        return fakeResponse(200, {
          ...devTests,
          repo_root_known: false,
          scenario_dir: "",
          scenarios: [],
        });
      }
      return fakeResponse(404, { title: "Not Found" });
    }) as unknown as typeof fetch;

    renderPage();
    expect(await screen.findByTestId("harness-not-wired")).toHaveTextContent(
      "GAME_NARRATOR_REPO_ROOT",
    );
  });
});
