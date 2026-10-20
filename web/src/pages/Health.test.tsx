import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AuthProvider } from "../auth/auth-context";
import {
  LiveStatusProvider,
  STATUS_STREAM_PATH,
  useLiveStatus,
  type StatusSnapshot,
} from "../lib/liveStatus";
import { HealthPage, healthSummaryText } from "./Health";
import { HealthTiles } from "./health/tiles";

/**
 * The Health screen's contract (plan.md Phase 1 item 8; ADR-0111), as a
 * synthetic stub (AGENTS.md rule 3 class (a): no Go server — the fetch
 * seam and the scripted status feed stand in, labelled as such): the 3×2
 * grid renders the fixture rollup's six tiles with the server's levels and
 * details, the non-ok tile wears the coral tag, the capture tile carries
 * the UX-11 signal/picture words from the scripted signal answer, the bar
 * string (the one stream) names the non-ok tile, and the four tabs switch
 * the page's content.
 */

interface Principal {
  username: string;
  role: string;
}

/** The fixture health report: one non-ok tile (tts fail) plus a slow one. */
function fixtureSnapshot(overrides: Partial<StatusSnapshot> = {}): StatusSnapshot {
  return {
    updated_at: "2026-10-21T00:00:00Z",
    status: "4 ok · 1 slow · 1 fail · decision slow · tts fail",
    health: {
      components: [
        { id: "capture", level: "ok", detail: "29 fps" },
        { id: "ocr", level: "ok", detail: "512 reads" },
        { id: "decision", level: "slow", detail: "p95 3.1 s over budget" },
        { id: "tts", level: "fail", detail: "the provider answered 500" },
        { id: "agent", level: "ok", detail: "answered" },
        { id: "storage", level: "ok", detail: "database ready" },
      ],
      ok: 4,
      slow: 1,
      fail: 1,
      unknown: 0,
      detail: "4 ok · 1 slow · 1 fail",
    },
    device: { version: "v0.9.2", uptime_ms: 0 },
    ...overrides,
  };
}

/** The signal answer the tile's block reads (ADR-0110 §1's shape). */
function signalAnswer(overrides: Record<string, unknown> = {}) {
  return {
    device: "usb-synthetic-0001",
    mode: "640x480 MJPEG @ 10 fps",
    source: "oneshot",
    signal: true,
    fps_actual: null,
    fps_target: 10,
    picture: { x: 0, y: 0, w: 640, h: 480, settled: true, active: true },
    age_ms: 120,
    synthetic: true,
    ...overrides,
  };
}

class FakeEventSource {
  readyState = 0;
  onmessage: ((event: MessageEvent) => void) | null = null;
  onstatus: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: unknown) => void) | null = null;

  constructor(public readonly url: string) {
    expect(url).toBe(STATUS_STREAM_PATH);
  }

  close(): void {
    this.readyState = 2;
  }

  emit(snapshot: StatusSnapshot): void {
    if (this.onstatus !== null) {
      this.onstatus({ data: JSON.stringify(snapshot) } as MessageEvent);
    }
  }
}

/** The bar's contract probe: the status bar renders the feed's text. */
function BarProbe() {
  const { text } = useLiveStatus();
  return <span data-testid="bar-probe">{text}</span>;
}

function fakeResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: "",
    headers: new Headers({ "content-type": "application/json", ...headers }),
    json: async () => body,
    text: async () => (body instanceof Blob ? "" : JSON.stringify(body)),
    blob: async () => body as Blob,
  } as unknown as Response;
}

interface RenderOptions {
  snapshot?: StatusSnapshot;
  principal?: Principal | null;
  signal?: unknown;
}

function renderPage({
  snapshot = fixtureSnapshot(),
  principal = { username: "ops", role: "admin" },
  signal = signalAnswer(),
}: RenderOptions = {}) {
  const stream = new FakeEventSource(STATUS_STREAM_PATH);
  const systemInfo = {
    version: "v0.9.2-test",
    commit: "abcdef1",
    go_version: "go1.27.2",
    os: "linux",
    arch: "arm64",
    num_cpu: 4,
    started_at: "2026-10-21T00:00:00Z",
    listen_addresses: ["127.0.0.1:18090"],
    tailnet: { present: false },
  };
  const host = {
    cpu: { cores: 4, load_1m: 0.4, load_5m: 0.3, load_15m: 0.2, throttle_count: 0 },
    ram: { available_bytes: 1073741824, total_bytes: 4294967296 },
    storage: {
      known: true,
      free_bytes: 10737418240,
      total_bytes: 53687091200,
      asset_store_bytes: 1048576,
      data_dir: "run",
    },
    thermal: { available: false, zones: [] },
    warnings: [],
  };
  const captureDevices = {
    synthetic: true,
    note: "labelled synthetic (test seam)",
    devices: [
      { id: "usb-synthetic-0001", name: "Synthetic HDMI", path: "/dev/video0", state: "ready" },
    ],
  };
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      if (url.includes("/api/v1/system/info")) return fakeResponse(200, systemInfo);
      if (url.includes("/api/v1/auth/status")) {
        return fakeResponse(200, { setup_required: false, loopback_setup: true });
      }
      if (url.includes("/api/v1/auth/me")) {
        if (principal === null) return fakeResponse(401, {});
        return fakeResponse(200, { user: principal, csrf_token: "csrf", auth_method: "cookie" });
      }
      if (url.includes("/api/v1/health/ready")) {
        return fakeResponse(200, { status: "ok", checks: { database: { status: "ok" } } });
      }
      if (url.includes("/api/v1/diagnostics/host")) return fakeResponse(200, host);
      if (url.includes("/api/v1/capture/devices")) return fakeResponse(200, captureDevices);
      if (url.includes("/api/v1/capture/signal")) return fakeResponse(200, signal);
      if (url.includes("/api/v1/capture/preview")) {
        return fakeResponse(200, new Blob([new Uint8Array(64)], { type: "image/jpeg" }));
      }
      if (url.includes("/api/v1/voices")) return fakeResponse(200, { voices: [] });
      if (url.includes("/api/v1/audio/devices")) {
        return fakeResponse(200, { devices: [{ id: "out-0", name: "Hub HDMI", state: "ready" }] });
      }
      if (url.includes("/api/v1/jobs")) return fakeResponse(200, { jobs: [] });
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
  // jsdom provides no object URLs; the tile's contract is the src it sets.
  (URL as { createObjectURL: unknown }).createObjectURL = vi.fn(() => "blob:fixture");
  (URL as { revokeObjectURL: unknown }).revokeObjectURL = vi.fn();

  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/health"]}>
        <AuthProvider>
          <LiveStatusProvider feed={{ eventSourceFactory: () => stream, pollIntervalMs: 60_000 }}>
            <HealthPage />
            <BarProbe />
          </LiveStatusProvider>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  act(() => {
    stream.emit(snapshot);
  });
  return { ...utils, stream };
}

let originalCreateObjectURL: typeof URL.createObjectURL;
let originalRevokeObjectURL: typeof URL.revokeObjectURL;

beforeEach(() => {
  originalCreateObjectURL = URL.createObjectURL;
  originalRevokeObjectURL = URL.revokeObjectURL;
});

afterEach(() => {
  vi.unstubAllGlobals();
  URL.createObjectURL = originalCreateObjectURL;
  URL.revokeObjectURL = originalRevokeObjectURL;
});

describe("healthSummaryText", () => {
  it("summarises the rollup's own counts (the page's 36 px title word)", () => {
    // No rollup yet (the one stream has not delivered a frame): the page's
    // own name, never a guess.
    expect(healthSummaryText(undefined)).toBe("Health");
    // The mock's all-healthy headline word, to the letter.
    expect(healthSummaryText({ slow: 0, fail: 0, unknown: 0 })).toBe("Everything's mostly fine");
    // The rollup's non-ok tail, in the mock's shape (1 slow + 1 fail).
    expect(healthSummaryText({ slow: 1, fail: 0, unknown: 0 })).toBe("1 needs attention");
    expect(healthSummaryText({ slow: 1, fail: 1, unknown: 0 })).toBe("2 need attention");
    // An unprobed tile keeps its own word, never a restyled "all fine".
    expect(healthSummaryText({ slow: 0, fail: 0, unknown: 5 })).toBe("5 unprobed");
    expect(healthSummaryText({ slow: 1, fail: 1, unknown: 3 })).toBe(
      "2 need attention · 3 unprobed",
    );
  });
});

describe("the tile sparkline's window (the one stream, sampled on a fixed cadence)", () => {
  /** The grid alone over a scripted feed: no fetch seam is needed — the
   *  tiles read the one status stream only (the capture tile's signal
   *  block is the page's `signalExtra`, not the grid's). */
  function renderTiles(snapshot: StatusSnapshot) {
    const stream = new FakeEventSource(STATUS_STREAM_PATH);
    const utils = render(
      <MemoryRouter initialEntries={["/health"]}>
        <LiveStatusProvider feed={{ eventSourceFactory: () => stream, pollIntervalMs: 60_000 }}>
          {/* A fast tick fills the twelve-bucket window in the test. */}
          <HealthTiles sparkTickMs={40} />
        </LiveStatusProvider>
      </MemoryRouter>,
    );
    act(() => {
      stream.emit(snapshot);
    });
    return utils;
  }

  function sparkHeights(testid: string): number[] {
    const svg = document.querySelector(`[data-testid="${testid}"]`);
    return Array.from(svg?.querySelectorAll("rect[fill]") ?? []).map((rect) =>
      Number(rect.getAttribute("height") ?? "0"),
    );
  }

  it("fills the twelve buckets from the stream's levels; an unprobed tile stays at the floor", async () => {
    // The fixture: storage ok, tts fail (the 25 level), the rest ok/slow.
    const { unmount } = renderTiles(fixtureSnapshot());
    // ok = 100 → a full bar (20 of 24) once the window has sampled it.
    await waitFor(
      () => {
        expect(sparkHeights("health-tile-storage-spark")).toEqual(Array(12).fill(20));
      },
      { timeout: 5_000 },
    );
    // fail = 25 → the quarter-height bar (5), the encoding the ADR pins.
    expect(sparkHeights("health-tile-tts-spark")).toEqual(Array(12).fill(5));
    unmount();
  });

  it("renders the floor — and only the floor — for a component nobody probed", async () => {
    // storage flipped to unknown ("no readiness probe wired"): the window
    // fills with the floor, which is the honest rendering of not-probed.
    const snapshot = fixtureSnapshot();
    const storage = snapshot.health.components.find((component) => component.id === "storage");
    expect(storage).toBeDefined();
    const { unmount } = renderTiles({
      ...snapshot,
      health: {
        ...snapshot.health,
        ok: snapshot.health.ok - 1,
        unknown: 1,
        components: snapshot.health.components.map((component) =>
          component.id === "storage"
            ? { id: "storage", level: "unknown", detail: "no readiness probe wired" }
            : component,
        ),
      },
    });
    // Wait on a sibling tile (capture, ok) that the window has filled,
    // then the unprobed tile's bars must still be the floor (2).
    await waitFor(
      () => {
        expect(sparkHeights("health-tile-capture-spark")).toEqual(Array(12).fill(20));
      },
      { timeout: 5_000 },
    );
    expect(sparkHeights("health-tile-storage-spark")).toEqual(Array(12).fill(2));
    unmount();
  });
});

describe("HealthPage (unit test with a stubbed feed and fetch fake)", () => {
  it("renders the 3×2 grid over the fixture rollup; the non-ok tile carries the coral tag and the detail", async () => {
    renderPage();
    // The page's title is the rollup's own summary (the fixture's one slow
    // and one fail tile — the mock's "2 need attention"), not a guess.
    expect(screen.getByRole("heading", { name: "2 need attention" })).toBeInTheDocument();

    // The grid arrives with the feed's frame (the one stream, UX-07).
    await screen.findByTestId("health-grid");

    // The six tiles, canonical order, each with tag, detail and the 12-bar spark.
    for (const id of ["capture", "ocr", "decision", "tts", "agent", "storage"]) {
      expect(screen.getByTestId(`health-tile-${id}`)).toBeInTheDocument();
      expect(screen.getByTestId(`health-tile-${id}-tag`)).toBeInTheDocument();
      expect(screen.getByTestId(`health-tile-${id}-detail`)).toBeInTheDocument();
      const spark = screen.getByTestId(`health-tile-${id}-spark`);
      // The kit sparkline's twelve value bars (pattern-filled rects).
      expect(spark.querySelectorAll("rect[fill]").length).toBe(12);
    }

    // The server's levels, verbatim (the client never re-derives them).
    expect(screen.getByTestId("health-tile-capture-tag")).toHaveTextContent("ok");
    expect(screen.getByTestId("health-tile-decision-tag")).toHaveTextContent("slow");
    expect(screen.getByTestId("health-tile-tts-tag")).toHaveTextContent("fail");
    expect(screen.getByTestId("health-tile-tts-detail")).toHaveTextContent(
      "the provider answered 500",
    );
    // The non-ok tile wears the error language (the coral border class).
    expect(screen.getByTestId("health-tile-tts").classList).toContain("health-tile-warn");
    expect(screen.getByTestId("health-tile-decision").classList).toContain("health-tile-warn");
    expect(screen.getByTestId("health-tile-capture").classList).not.toContain("health-tile-warn");

    // The default tab is health (the grid's home).
    expect(await screen.findByTestId("host-vitals")).toBeInTheDocument();
    expect(screen.getByRole("tab", { name: "health" })).toHaveAttribute("aria-selected", "true");
  });

  it("the one stream's bar string names the non-ok tile (UX-07)", async () => {
    renderPage();
    const probe = await screen.findByTestId("bar-probe");
    expect(probe).toHaveTextContent("tts fail");
    expect(probe).toHaveTextContent("decision slow");
  });

  it("the capture tile says signal present · no active picture for a black feed (UX-11)", async () => {
    renderPage({
      signal: signalAnswer({
        picture: { x: 0, y: 0, w: 640, h: 480, settled: true, active: false },
      }),
    });
    expect(await screen.findByTestId("health-tile-capture-signal")).toHaveTextContent(
      "signal present",
    );
    expect(screen.getByTestId("health-tile-capture-picture")).toHaveTextContent(
      "no active picture",
    );
  });

  it("the capture tile says signal present · active picture and shows the preview for a changing feed (UX-11)", async () => {
    renderPage();
    expect(await screen.findByTestId("health-tile-capture-signal")).toHaveTextContent(
      "signal present",
    );
    expect(screen.getByTestId("health-tile-capture-picture")).toHaveTextContent("active picture");
    // The preview's first tick is half a period behind the signal's (the
    // shared throttle's offset), so wait for the img to replace the waiting
    // placeholder (they share the testid).
    const preview = await waitFor(
      () => {
        const element = screen.queryByTestId("health-tile-capture-preview");
        if (element !== null && element.tagName === "IMG") {
          return element as HTMLElement;
        }
        throw new Error("the waiting placeholder still stands");
      },
      { timeout: 5_000 },
    );
    expect(preview).toHaveAttribute("src", "blob:fixture");
  });

  it("the tabs switch to logs, setup runs and line traces", async () => {
    renderPage();
    await screen.findByTestId("health-grid");
    // logs: the preserved tail (the signed-in principal reads the ring).
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "logs" }), { button: 0 });
    expect(await screen.findByTestId("system-logs")).toBeInTheDocument();
    expect(screen.queryByTestId("health-grid")).not.toBeInTheDocument();

    // setup runs: the fixture has no active run — the empty state, not a
    // fabricated row.
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "setup runs" }), { button: 0 });
    expect(await screen.findByTestId("health-setup-runs-empty")).toBeInTheDocument();

    // line traces: no session in the fixture — the honest pointer.
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "line traces" }), { button: 0 });
    expect(await screen.findByTestId("health-line-traces-empty")).toBeInTheDocument();
  });

  it("the capture tile's signal block shows the sign-in hint when anonymous", async () => {
    renderPage({ principal: null });
    expect(await screen.findByTestId("health-tile-capture-signal-sign-in")).toBeInTheDocument();
    expect(screen.queryByTestId("health-tile-capture-signal")).not.toBeInTheDocument();
    // The tiles themselves keep rendering: the rollup is public.
    expect(screen.getByTestId("health-grid")).toBeInTheDocument();
  });

  it("a session in the fixture shows the line-traces tab's line activity", async () => {
    renderPage({
      snapshot: fixtureSnapshot({
        session: {
          session_id: "s1",
          game: "Harbour Light",
          mode: "conservative",
          state: "running",
          paused: false,
          source: "v4l2",
          capture_fps: 29,
          frames_dropped: 0,
          lines_spoken: 12,
          frame_to_voice_p50_ms: 412,
          frame_to_voice_samples: 12,
          ocr_calls: 20,
          ocr_errors: 0,
          elapsed_ms: 60_000,
          created_at: "2026-10-21T00:00:00Z",
        },
        setup_runs: [
          {
            run_id: "r1",
            game: "Harbour Light",
            state: "running",
            stage: "voicing",
            voiced: 3,
            characters: 12,
            elapsed_ms: 60_000,
          },
        ],
      }),
    });
    fireEvent.mouseDown(await screen.findByRole("tab", { name: "line traces" }), { button: 0 });
    expect(await screen.findByTestId("health-line-traces-session")).toBeInTheDocument();
    expect(screen.getByTestId("health-line-traces-spoken")).toHaveTextContent("12");

    fireEvent.mouseDown(await screen.findByRole("tab", { name: "setup runs" }), { button: 0 });
    expect(await screen.findByTestId("health-setup-run-r1")).toBeInTheDocument();
  });
});
