import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { Profiler } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { AuthProvider } from "../auth/auth-context";
import { GraphPage } from "./Graph";

/**
 * The graph list's height contract, in the unit lane (docs/spec/12 "Unit"
 * mode — synthetic fetch, jsdom).
 *
 * The e2e walk takes full-page screenshots; a full-page capture resizes the
 * layout viewport to the document's size, so a list whose height is derived
 * from the window re-lays out inside the capture, and if the re-render it
 * causes changes the document size the capture is chasing, the screenshot
 * never returns and the walk hangs (the m5 walk's intermittent 10-minute
 * timeout). The list therefore sizes itself from its own fixed box, and
 * this test is the pin for that invariant: no matter what sizes the window
 * (or the capture's viewport resize) reports, the container's measured
 * height stays its one constant — the visible row window stays bounded and
 * the page settles, the bounded-render assertion Regions.test.tsx makes for
 * the region editor's draw.
 */

const ROWS = 200;
const ROW_HEIGHT = 56;
// The page's contract (Graph.tsx LIST_HEIGHT): a fixed, self-measured box.
const LIST_HEIGHT = 480;
// What the fixed box shows: the rows inside it plus the library's overscan.
const EXPECTED_WINDOW = Math.min(ROWS, Math.ceil(LIST_HEIGHT / ROW_HEIGHT) + 6);

const admin = {
  id: "01ADMIN",
  username: "admin",
  role: "admin" as const,
  disabled: false,
  created_at: "2026-11-25T00:00:00Z",
  password_changed_at: "2026-11-25T00:00:00Z",
};

/** jsdom has no ResizeObserver; this one is controllable, so the test can
 *  fire the callbacks the browser would fire after a re-layout. */
class ResizeObserverMock {
  static instances: ResizeObserverMock[] = [];
  // Public for the test's assertion that the page observed the list's own
  // box.
  readonly entries: { target: Element; cb: ResizeObserverCallback }[] = [];

  constructor(private readonly cb: ResizeObserverCallback) {
    ResizeObserverMock.instances.push(this);
  }

  observe(target: Element) {
    this.entries.push({ target, cb: this.cb });
  }

  unobserve() {}

  disconnect() {}

  /** Deliver the notification a re-layout would produce: one entry per
   *  observed target, with the reported (content) size. */
  fire(height: number) {
    for (const { target, cb } of this.entries) {
      cb(
        [
          {
            target,
            contentRect: {
              width: 640,
              height,
              top: 0,
              left: 0,
              bottom: height,
              right: 640,
              x: 0,
              y: 0,
              toJSON: () => ({}),
            },
          } as unknown as ResizeObserverEntry,
        ],
        this as unknown as ResizeObserver,
      );
    }
  }
}

function installFetch(): void {
  const nodes = Array.from({ length: ROWS }, (_, i) => ({
    id: `n${i}`,
    kind: "dialogue",
    text: `line ${i + 1}`,
    attribution_status: i % 2 === 0 ? "confirmed" : "unknown",
    attribution_confidence: i % 2 === 0 ? 0.95 : 0,
    speaker: { id: "marina", name: "Marina" },
    scene: { id: "s1", name: "Harbour" },
  }));
  const graphPage = { nodes, edges: [], total: ROWS };
  installGraphFetch(graphPage);
}

/** The table view's fetch: a small page with two edges, so the successors
 *  cell has something to render (one node's two transitions, one node with
 *  none on the page). */
function installTableFetch(): void {
  installGraphFetch({
    nodes: [
      {
        id: "a",
        kind: "dialogue",
        text: "line a",
        attribution_status: "confirmed",
        attribution_confidence: 0.95,
        speaker: { id: "marina", name: "Marina" },
        scene: null,
      },
      {
        id: "b",
        kind: "sign",
        text: "line b",
        attribution_status: "unknown",
        attribution_confidence: 0,
        speaker: { id: "marina", name: "Marina" },
        scene: null,
      },
      {
        id: "c",
        kind: "dialogue",
        text: "line c",
        attribution_status: "unknown",
        attribution_confidence: 0,
        speaker: null,
        scene: null,
      },
    ],
    edges: [
      { from: "a", to: "b", kind: "next", confidence: 0.9, source: null, evidence: 3 },
      { from: "a", to: "c", kind: "branch", confidence: 0.4, source: null, evidence: 1 },
    ],
    total: 3,
  });
}

function installGraphFetch(graphPage: unknown): void {
  const bodyFor = (path: string): unknown => {
    if (path === "/api/v1/auth/me")
      return {
        user: admin,
        csrf_token: "csrf-1",
        auth_method: "cookie",
        expires_at: "2026-11-26T00:00:00Z",
      };
    if (path === "/api/v1/auth/status") return { setup_required: false, loopback_setup: true };
    if (path === "/api/v1/profiles/p1/graph") return graphPage;
    if (path === "/api/v1/profiles/p1/characters")
      return {
        characters: [
          {
            id: "marina",
            profile_id: "p1",
            name: "Marina",
            aliases: [],
            visual_descriptors: [],
            personality_summary: "",
            role: "protagonist",
            verified_source_refs: [],
            status: "confirmed",
            created_at: "2026-11-25T00:00:00Z",
          },
        ],
      };
    return {};
  };
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(String(input), "http://localhost").pathname;
    return new Response(JSON.stringify(bodyFor(path)), {
      status: 200,
      headers: { "content-type": "application/json" },
    });
  }) as unknown as typeof fetch;
}

/** A resize, the way a full-page capture performs it: the window reports a
 *  larger viewport and the re-layout delivers it to the observers. */
function simulateCaptureResize(windowHeight: number, reportedHeight: number): void {
  Object.defineProperty(window, "innerHeight", { configurable: true, value: windowHeight });
  window.dispatchEvent(new Event("resize"));
  for (const instance of ResizeObserverMock.instances) instance.fire(reportedHeight);
}

beforeEach(() => {
  ResizeObserverMock.instances = [];
  // jsdom has no ResizeObserver; install the controllable stand-in so the
  // page's observer attaches the way it would in a browser.
  globalThis.ResizeObserver = ResizeObserverMock as unknown as typeof ResizeObserver;
});

it("a resize leaves the list's measured height at its own fixed box", async () => {
  installFetch();
  const onRender = vi.fn();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/games/g1/graph?profile=p1"]}>
        <AuthProvider>
          <Profiler id="graph" onRender={onRender}>
            <Routes>
              <Route path="/games/:id/graph" element={<GraphPage />} />
            </Routes>
          </Profiler>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );

  // The page settles on the loaded list.
  const list = await screen.findByTestId("graph-node-list", undefined, { timeout: 10_000 });
  await screen.findByText(`${ROWS} of ${ROWS} lines`, { exact: false });

  // The observer is attached to the list's own box (it mounts with the
  // list — before the data lands the page renders its loading state).
  expect(ResizeObserverMock.instances).toHaveLength(1);
  expect(ResizeObserverMock.instances[0].entries[0]?.target).toBe(list);

  // jsdom reports no layout: give the box the size a browser would lay it
  // out at, then let the observer's measure of that box settle.
  Object.defineProperty(list, "clientHeight", { configurable: true, value: LIST_HEIGHT });
  await act(async () => {
    for (const instance of ResizeObserverMock.instances) instance.fire(LIST_HEIGHT);
  });
  // The window of rows the box renders. Counted from the box itself: its
  // own test id shares the row prefix, so the rows are its option roles.
  const rowWindow = () => list.querySelectorAll('[role="option"]').length;
  expect(rowWindow(), "the fixed box shows its rows plus the overscan").toBe(EXPECTED_WINDOW);

  // The capture's viewport resize: the window reports ever larger sizes and
  // the re-layout delivers them. The box is its own constant — a
  // window-derived measure would grow, widen the row window and stretch the
  // document the capture is chasing.
  const before = onRender.mock.calls.length;
  for (const size of [1200, 2400, 6000]) simulateCaptureResize(size, size);
  await act(async () => {});

  expect(rowWindow(), "the measured height stayed the box's own constant").toBe(EXPECTED_WINDOW);
  expect(list.style.height).toBe(`${LIST_HEIGHT}px`);
  expect(list.style.maxHeight).toBe(`${LIST_HEIGHT}px`);
  expect(onRender.mock.calls.length - before, "the resizes settle, they do not loop").toBeLessThan(
    50,
  );
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(
    onRender.mock.calls.length,
    "no observer or effect keeps scheduling re-renders after the resize",
  ).toBeLessThanOrEqual(before + 50);
});

it("the table view carries the page's messages with kind, speaker and successors", async () => {
  installTableFetch();
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/games/g1/graph?profile=p1"]}>
        <AuthProvider>
          <Routes>
            <Route path="/games/:id/graph" element={<GraphPage />} />
          </Routes>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );

  // The page settles on its default (canvas) view; the toggle is the design's
  // segmented switch (issue #34's non-canvas alternative).
  await screen.findByTestId("graph-node-list", undefined, { timeout: 10_000 });
  await screen.findByText("3 of 3 lines", { exact: false });
  const toggle = screen.getByTestId("graph-view-toggle");
  expect(toggle, "the view toggle is on the page").toBeTruthy();

  // The toggle is keyboard-operable (Radix tabs: the trigger is a real
  // button in the tab list, Enter-activatable — the activation the design's
  // keyboard contract names): focus it and press Enter.
  const tableTab = screen.getByRole("tab", { name: "table" });
  tableTab.focus();
  fireEvent.keyDown(tableTab, { key: "Enter" });
  const table = await screen.findByTestId("graph-table", undefined, { timeout: 10_000 });
  expect(table).toBeTruthy();
  // The choice persists per user (UX-08), the way the library/cast toggles.
  expect(window.localStorage.getItem("gn-graph-view")).toBe("table");
  // The canvas list is gone: one view, no duplicated data in the DOM.
  expect(screen.queryByTestId("graph-node-list")).toBeNull();

  // Every message of the page, with its text kind and speaker.
  const rowA = screen.getByTestId("graph-table-row-a");
  expect(rowA.textContent ?? "").toContain("line a");
  expect(rowA).toHaveTextContent("dialogue");
  expect(rowA).toHaveTextContent("Marina");
  const rowB = screen.getByTestId("graph-table-row-b");
  expect(rowB).toHaveTextContent("sign");
  const rowC = screen.getByTestId("graph-table-row-c");
  expect(rowC.textContent ?? "").toContain("line c");

  // The successors: node a's two transitions, each naming the successor's
  // kind and speaker and the edge's kind — the same edges the canvas colours.
  expect(rowA).toHaveTextContent("next → line b");
  // Successor b is a sign: its kind is named, not the source line's.
  expect(rowA).toHaveTextContent("(sign, Marina)");
  expect(rowA).toHaveTextContent("90%");
  expect(rowA).toHaveTextContent("branch → line c");
  expect(rowA).toHaveTextContent("unattributed");
  expect(rowA).toHaveTextContent("40%");
  // Node b has no outgoing edge on the page: the honest empty answer.
  expect(rowB).toHaveTextContent("none on this page");
});

afterEach(cleanup);
