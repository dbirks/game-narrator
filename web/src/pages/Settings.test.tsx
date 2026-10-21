import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { MemoryRouter } from "react-router";

import type { Settings } from "../api/settings";
import { AuthProvider } from "../auth/auth-context";
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
    tailnet: { present: false },
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
  network: {
    tailnet_url: "http://uno-q.stubbed-tailnet.ts.net:18090",
    source: "env",
    note: "Pinned by GAME_NARRATOR_TAILNET_URL — the operator's stable tailnet name (the environment override beats the live probe).",
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
    current: "v0.0.0-test",
    available: false,
    checking: false,
    from_cache: false,
    no_releases: false,
    disabled: false,
    env: ["GAME_NARRATOR_UPDATE_CHECK", "GAME_NARRATOR_UPDATE_URL"],
  },
  mutable: {
    count: 1,
    note: "One setting is mutable at runtime: the audio output (ADR-0074).",
    env: ["GAME_NARRATOR_GATEWAY_URL", "GAME_NARRATOR_LISTEN", "GAME_NARRATOR_AUDIO_SINK"],
  },
};

/** The audio device list the page's "Audio output" section renders (ADR-0074). */
const audioDevices = {
  backend: "pipewire",
  synthetic: false,
  devices: [
    {
      id: "auto_null",
      name: "Monitor of Autopipe Sink",
      kind: "null",
      sample_rate: "48000",
      channels: 2,
      selected: true,
      platform_default: true,
    },
    {
      id: "usb_jbl",
      name: "JBL USB DAC",
      kind: "usb",
      sample_rate: "48000",
      channels: 2,
      selected: false,
      platform_default: false,
    },
  ],
  selected: {
    device: "auto_null",
    device_source: "store",
    sink_kind: "pipewire",
    sink_kind_source: "store",
    master_volume: 0.8,
    master_volume_source: "default",
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
    // MemoryRouter: the page's Theme card carries a Link to /settings/theme
    // (ADR-0142), the same seam the other page tests wrap.
    <MemoryRouter>
      <QueryClientProvider client={client}>
        <AuthProvider>
          <SettingsPage />
        </AuthProvider>
      </QueryClientProvider>
    </MemoryRouter>,
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
        : path === "/api/v1/auth/status"
          ? fakeResponse(200, { setup_required: false, loopback_setup: false })
          : path === "/api/v1/auth/me"
            ? fakeResponse(401, { title: "Not Signed In" })
            : path === "/api/v1/audio/devices"
              ? fakeResponse(200, audioDevices)
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
      "1 setting(s) are mutable at runtime",
    );
    expect(screen.getByTestId("settings-gateway")).toHaveTextContent("GAME_NARRATOR_GATEWAY_URL");
    expect(screen.getByTestId("settings-gateway")).toHaveTextContent("presence only");
    expect(screen.getByTestId("settings-storage")).toHaveTextContent("gn-test");
    expect(screen.getByTestId("settings-audio")).toHaveTextContent("filesink");
    // The one runtime-mutable setting (ADR-0074): the enumerated devices with
    // their kinds and marks, the selection's source, and the master volume.
    const audioOutput = await screen.findByTestId("settings-audio-output");
    expect(audioOutput).toHaveTextContent("JBL USB DAC");
    expect(audioOutput).toHaveTextContent("null");
    expect(screen.getByTestId("audio-device-select")).toHaveValue("auto_null");
    expect(screen.getByTestId("audio-master-volume-value")).toHaveTextContent("80%");
    // No synthetic flag: the backend answered, so the list reads as hardware.
    expect(screen.queryByTestId("audio-output-synthetic")).not.toBeInTheDocument();
    expect(screen.getByTestId("audio-test-tone")).toBeInTheDocument();
    expect(screen.getByTestId("settings-security")).toHaveTextContent("127.0.0.1:18090");
    expect(screen.getByTestId("settings-imports")).toHaveTextContent("24 steps");
    expect(screen.getByTestId("settings-version")).toHaveTextContent("abcdef1");
    expect(screen.getByTestId("settings-version")).toHaveTextContent("no auto-update");
    expect(screen.getByTestId("backup-download")).toBeInTheDocument();
    // The new ADR-0142 cards: the theme link (the theme page route) and the
    // tailnet URL, rendered verbatim from the API's network section.
    expect(screen.getByTestId("theme-page-link")).toHaveAttribute("href", "/settings/theme");
    expect(screen.getByTestId("settings-theme-current")).toHaveTextContent("Plum & Peach");
    expect(screen.getByTestId("tailnet-url")).toHaveTextContent(
      "http://uno-q.stubbed-tailnet.ts.net:18090",
    );
    expect(screen.getByTestId("tailnet-note")).toHaveTextContent("GAME_NARRATOR_TAILNET_URL");
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
