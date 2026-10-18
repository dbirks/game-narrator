import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { Settings } from "../api/settings";
import { SettingsPage } from "./Settings";

const settings: Settings = {
  models: {
    gateway: {
      url_env: "GAME_NARRATOR_GATEWAY_URL",
      token_env_name: "GAME_NARRATOR_GATEWAY_TOKEN",
      url_configured: false,
      token_configured: false,
    },
    roles: [
      {
        role: "decision",
        state: "NOT CONFIGURED",
        configured: false,
        model: "",
        protocol: "openai_chat",
        base_url: "",
        reasoning_effort: "",
        timeout: "",
        token_configured: false,
        capabilities: null,
        env: ["GAME_NARRATOR_ROLE_DECISION_MODEL"],
      },
      {
        role: "fast_reasoning",
        state: "CONFIGURED",
        configured: true,
        model: "test-model-x",
        protocol: "openai_chat",
        base_url: "<configured>",
        reasoning_effort: "",
        timeout: "60s",
        token_configured: true,
        capabilities: null,
        env: ["GAME_NARRATOR_ROLE_FAST_REASONING_MODEL"],
      },
    ],
  },
  storage: {
    data_dir: "gn-test",
    data_dir_known: true,
    free_space_bytes: 42 * 1024 * 1024,
    free_space_known: true,
    asset_store_bytes: 2048,
    speech_cache: {
      bytes: null,
      note: "the speech cache stores its WAVs as content-addressed blobs inside the asset store, so its share of the total is not distinguishable by size",
    },
    env: ["GAME_NARRATOR_DATA_DIR"],
  },
  audio: {
    sink: "filesink",
    device: "",
    env: ["GAME_NARRATOR_AUDIO_SINK"],
  },
  security: {
    listen: "127.0.0.1:18090",
    listen_env: "GAME_NARRATOR_LISTEN",
    auth: { mode: "local", admin_configured: true },
  },
  imports: {
    web_search: {
      enabled: true,
      provider: "exa-mcp",
      note: "web pages are untrusted data for the agent",
      env_private_allow: "GAME_NARRATOR_WEB_PRIVATE_ALLOW",
      private_hosts_allowed: 0,
    },
    agent: {
      max_steps: 24,
      max_tool_calls: 48,
      source: "build constants of cmd/game-narrator's agent wiring",
    },
  },
  version: {
    version: "v0.0.0-test",
    commit: "abcdef1",
    go_version: "go1.27.2",
    os: "linux",
    arch: "arm64",
  },
  update: {
    auto_update: false,
    docs: "docs/release.md",
    note: "updates are manual",
  },
  mutable: {
    count: 0,
    note: "",
    env: ["GAME_NARRATOR_GATEWAY_URL", "GAME_NARRATOR_LISTEN", "GAME_NARRATOR_AUDIO_SINK"],
  },
};

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

function installFetch(responder: (path: string) => Response) {
  const mock = vi.fn((input: RequestInfo | URL): Promise<Response> => {
    const url = typeof input === "string" ? input : input.toString();
    return Promise.resolve(responder(new URL(url, "http://localhost").pathname));
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
      <SettingsPage />
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
describe("SettingsPage (unit test with a stubbed fetch fake)", () => {
  it("renders the effective configuration, naming the variables and the redaction", async () => {
    installFetch((path) =>
      path === "/api/v1/settings"
        ? fakeResponse(200, settings)
        : fakeResponse(404, { title: "Not Found" }),
    );

    renderPage();
    expect(screen.getByTestId("settings-loading")).toBeInTheDocument();

    const roles = await screen.findByTestId("settings-roles");
    expect(roles).toHaveTextContent("decision");
    expect(roles).toHaveTextContent("NOT CONFIGURED");
    expect(roles).toHaveTextContent("test-model-x");
    // Redaction the page must show: the endpoint renders as the marker, never
    // a host, and the model id is the one displayable part.
    expect(roles).toHaveTextContent("<configured>");

    expect(screen.getByTestId("settings-mutable-note")).toHaveTextContent(
      "Nothing is mutable at runtime",
    );
    expect(screen.getByTestId("settings-gateway")).toHaveTextContent("GAME_NARRATOR_GATEWAY_URL");
    expect(screen.getByTestId("settings-gateway")).toHaveTextContent("presence only");
    expect(screen.getByTestId("settings-storage")).toHaveTextContent("gn-test");
    expect(screen.getByTestId("settings-audio")).toHaveTextContent("filesink");
    expect(screen.getByTestId("settings-security")).toHaveTextContent("127.0.0.1:18090");
    expect(screen.getByTestId("settings-imports")).toHaveTextContent("24 steps");
    expect(screen.getByTestId("settings-version")).toHaveTextContent("abcdef1");
    expect(screen.getByTestId("settings-version")).toHaveTextContent("no auto-update");
    expect(screen.getByTestId("backup-download")).toBeInTheDocument();
  });

  it("shows the administrator notice on a 403 instead of an error", async () => {
    installFetch(() =>
      fakeResponse(403, {
        type: "urn:game-narrator:error:forbidden",
        title: "Forbidden",
        status: 403,
        detail: "this route requires the admin role",
      }),
    );

    renderPage();
    expect(await screen.findByTestId("settings-needs-admin")).toHaveTextContent(
      "administrator view",
    );
  });

  it("renders the error state with a retry on a 500", async () => {
    installFetch(() =>
      fakeResponse(500, {
        type: "urn:game-narrator:error:internal",
        title: "Internal",
        status: 500,
        detail: "the settings snapshot failed",
        request_id: "req-500",
      }),
    );

    renderPage();
    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("the settings snapshot failed");
    expect(alert).toHaveTextContent("req-500");
  });
});
