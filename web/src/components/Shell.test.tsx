import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { afterEach, describe, expect, it, vi } from "vitest";

import { AuthProvider } from "../auth/auth-context";
import { LiveStatusProvider, STATUS_PLACEHOLDER, STATUS_STREAM_PATH } from "../lib/liveStatus";
import { buildLine, routeContextChip, Shell } from "./Shell";

// The fetch stub in renderShell must not leak into the next test's global.
afterEach(() => vi.unstubAllGlobals());

/**
 * The shell's contract (plan.md Phase 0 item 5): the wordmark, the four
 * nav items, the live string from the status feed (not a second copy of
 * the server's state), the agent pill's context chip following the route,
 * the ⌘K palette's keyboard contract, and the footer build line.
 */

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

  /** Emit a named status.changed frame (the wire's event name). */
  emit(data: string): void {
    if (this.onstatus !== null) this.onstatus({ data } as MessageEvent);
  }
}

function renderShell(
  pathname = "/library",
  principal: { username: string; role: string } | null = null,
) {
  const stream = new FakeEventSource(STATUS_STREAM_PATH);
  const body = {
    version: "v0.9.2",
    commit: "abcdef1",
    go_version: "go1.27.2",
    os: "linux",
    arch: "arm64",
    num_cpu: 4,
    started_at: "2026-10-16T12:00:00Z",
  };
  const fetchCalls: string[] = [];
  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === "string" ? input : input.toString();
      fetchCalls.push(url);
      if (url.includes("/api/v1/system/info")) {
        return {
          ok: true,
          status: 200,
          headers: new Headers({ "content-type": "application/json" }),
          json: async () => body,
          text: async () => JSON.stringify(body),
        } as Response;
      }
      if (url.includes("/api/v1/auth/me")) {
        // A 401 is the server's "anonymous" answer (auth-context models it
        // that way); a principal is the signed-in state.
        if (principal !== null) {
          const me = { user: principal, csrf_token: "csrf", auth_method: "cookie" };
          return {
            ok: true,
            status: 200,
            headers: new Headers({ "content-type": "application/json" }),
            json: async () => me,
            text: async () => JSON.stringify(me),
          } as Response;
        }
        return {
          ok: false,
          status: 401,
          statusText: "Unauthorized",
          headers: new Headers(),
          json: async () => ({}),
          text: async () => "unauthorized",
        } as Response;
      }
      if (url.includes("/api/v1/auth/status")) {
        const status = { setup_required: false, loopback_setup: true };
        return {
          ok: true,
          status: 200,
          headers: new Headers({ "content-type": "application/json" }),
          json: async () => status,
          text: async () => JSON.stringify(status),
        } as Response;
      }
      if (url.includes("/api/v1/search")) {
        // The ⌘K palette's debounced read (ADR-0098): the empty-query
        // recents answer with no groups — the unit tests only need the
        // dialog's chrome and keyboard contract, not the results.
        const search = { query: "", agent_prompt: "", groups: [] };
        return {
          ok: true,
          status: 200,
          headers: new Headers({ "content-type": "application/json" }),
          json: async () => search,
          text: async () => JSON.stringify(search),
        } as Response;
      }
      if (url.includes("/api/v1/auth/logout")) {
        return {
          ok: true,
          status: 204,
          statusText: "No Content",
          headers: new Headers(),
          json: async () => ({}),
          text: async () => "",
        } as Response;
      }
      throw new Error(`unexpected fetch: ${url}`);
    }),
  );
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const utils = render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[pathname]}>
        <AuthProvider>
          <LiveStatusProvider feed={{ eventSourceFactory: () => stream, pollIntervalMs: 60_000 }}>
            <Shell>
              <main data-testid="shell-child">page body</main>
            </Shell>
          </LiveStatusProvider>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  return { stream, queryClient, fetchCalls, ...utils };
}

describe("routeContextChip", () => {
  it("names the screen for the agent payload seam", () => {
    expect(routeContextChip("/library")).toBe("library");
    expect(routeContextChip("/play")).toBe("play");
    expect(routeContextChip("/games/new")).toBe("setup");
    expect(routeContextChip("/games/1/cast")).toBe("cast");
    expect(routeContextChip("/games/1/publish")).toBe("game");
    expect(routeContextChip("/dev/kit")).toBe("dev kit");
    expect(routeContextChip("/")).toBe("home");
  });
});

describe("buildLine", () => {
  it("renders version, commit, build time and Go version", () => {
    expect(
      buildLine({
        version: "v0.9.2",
        commit: "abcdef1",
        started_at: "2026-10-16T12:00:00Z",
        go_version: "go1.27.2",
      }),
    ).toBe("Build: v0.9.2 · abcdef1 · 2026-10-16 12:00 UTC · Go go1.27.2");
  });
});

describe("Shell", () => {
  it("renders the wordmark, the four nav items and the live string", async () => {
    renderShell();
    expect(screen.getByTestId("status-wordmark").textContent).toBe("GAME·NARRATOR");
    expect(screen.getByTestId("status-nav-library")).toHaveAttribute("aria-current", "page");
    for (const id of ["play", "traces", "settings"]) {
      expect(screen.getByTestId(`status-nav-${id}`)).not.toHaveAttribute("aria-current");
    }
    expect(screen.getByTestId("status-fade")).toBeInTheDocument();
    expect(screen.getByTestId("status-live").textContent).toBe(STATUS_PLACEHOLDER);
  });

  it("follows the stream's frames into the bar", async () => {
    const { stream } = renderShell();
    act(() => {
      stream.emit(JSON.stringify({ status: "● live · capture 30fps" }));
    });
    await waitFor(() =>
      expect(screen.getByTestId("status-live").textContent).toBe("● live · capture 30fps"),
    );
  });

  it("renders the footer build line from system/info", async () => {
    renderShell();
    await waitFor(() =>
      expect(screen.getByTestId("status-footer").textContent).toContain(
        "Build: v0.9.2 · abcdef1 · 2026-10-16 12:00 UTC · Go go1.27.2",
      ),
    );
  });

  it("opens the ⌘K palette and closes it on Esc", async () => {
    renderShell();
    expect(screen.queryByTestId("palette")).toBeNull();

    fireEvent.keyDown(window, { key: "k", metaKey: true });
    expect(await screen.findByTestId("palette")).toBeInTheDocument();
    expect(screen.getByTestId("palette-input")).toHaveFocus();

    fireEvent.keyDown(screen.getByTestId("palette-input"), { key: "Escape" });
    expect(screen.queryByTestId("palette")).toBeNull();
  });

  it("hands the ⌘K query to the agent dock on Tab (UX-02)", async () => {
    renderShell("/library");
    fireEvent.keyDown(window, { key: "k", metaKey: true });
    const input = await screen.findByTestId("palette-input");
    expect(input).toHaveFocus();

    fireEvent.change(input, { target: { value: "boy" } });
    fireEvent.keyDown(input, { key: "Tab" });

    // The overlay closes and the dock opens with the query pre-filled.
    expect(screen.queryByTestId("palette")).toBeNull();
    const dock = await screen.findByTestId("agent-dock");
    expect(dock).toBeInTheDocument();
    expect(screen.getByTestId("agent-dock-input")).toHaveValue("boy");
  });

  it("opens the ⌘J agent dock with the context chip", async () => {
    renderShell("/play");
    expect(screen.getByTestId("agent-pill").getAttribute("aria-expanded")).toBe("false");
    fireEvent.keyDown(window, { key: "j", metaKey: true });
    const dock = await screen.findByTestId("agent-dock");
    expect(dock).toBeInTheDocument();
    expect(screen.getByTestId("agent-dock-context").textContent).toBe("play");
    fireEvent.click(screen.getByTestId("agent-dock-close"));
    expect(screen.queryByTestId("agent-dock")).toBeNull();
  });

  it("shows the principal and the developer entry in the bar for a signed-in developer", async () => {
    renderShell("/library", { username: "ops", role: "developer" });
    // The principal moved from the legacy second nav row into the bar's
    // right side, keeping the e2e testids.
    const principal = await screen.findByTestId("nav-user");
    expect(principal).toHaveTextContent("ops");
    expect(screen.getByTestId("nav-role").textContent).toBe("developer");
    expect(screen.getByTestId("nav-logout")).toBeEnabled();
    // The developer entry groups the two developer-scoped destinations.
    expect(screen.getByTestId("status-dev")).toHaveTextContent("developer");
    expect(screen.getByTestId("nav-tests")).toHaveAttribute("href", "/tests");
    expect(screen.getByTestId("nav-replay-clips")).toHaveAttribute("href", "/replay-clips");
    expect(screen.queryByTestId("nav-login")).toBeNull();
  });

  it("hides the developer entry below the developer role", async () => {
    renderShell("/library", { username: "family", role: "operator" });
    await screen.findByTestId("nav-user");
    expect(screen.queryByTestId("status-dev")).toBeNull();
    expect(screen.queryByTestId("nav-tests")).toBeNull();
    expect(screen.queryByTestId("nav-replay-clips")).toBeNull();
  });

  it("offers sign-in when anonymous and ends the session on sign-out", async () => {
    // Anonymous: the bar's right side is the sign-in link (the legacy
    // row's anonymous state, in the bar).
    const { unmount } = renderShell("/library");
    expect(await screen.findByTestId("nav-login")).toHaveAttribute("href", "/login");
    expect(screen.queryByTestId("nav-user")).toBeNull();
    unmount();

    const { fetchCalls } = renderShell("/library", { username: "ops", role: "admin" });
    await screen.findByTestId("nav-user");
    fireEvent.click(screen.getByTestId("nav-logout"));
    await waitFor(() =>
      expect(fetchCalls.some((url) => url.includes("/api/v1/auth/logout"))).toBe(true),
    );
  });
});
