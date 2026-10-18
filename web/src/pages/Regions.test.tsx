import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes } from "react-router";
import { Profiler } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { AuthProvider } from "../auth/auth-context";
import { RegionsPage } from "./Regions";

/**
 * The region editor's pointer walk, in the unit lane (docs/spec/12 "Unit"
 * mode — synthetic fetch, jsdom): draw a box with pointerdown/move/up exactly
 * the way the e2e walk drags it, and hold the component to a bounded number
 * of renders.
 *
 * This pins the 2026-11-25 hang: after a drawn box landed on the canvas the
 * renderer never produced another frame — the component was stuck in an
 * effect → setState → render cycle, so Playwright's screenshot (which waits
 * for a frame) never returned. A page that cannot stop re-rendering after a
 * plain drag is a page that cannot be screenshotted, tested, or used, so the
 * bound on renders is the assertion, and the draw is the input.
 */

const admin = {
  id: "01ADMIN",
  username: "admin",
  role: "admin" as const,
  disabled: false,
  created_at: "2026-11-25T00:00:00Z",
  password_changed_at: "2026-11-25T00:00:00Z",
};

/** jsdom has no layout: the walk's geometry only means something if the
 *  canvas reports the frame it is laid out on. */
const CANVAS_RECT = {
  left: 0,
  top: 0,
  width: 1000,
  height: 750,
  right: 1000,
  bottom: 750,
  x: 0,
  y: 0,
  toJSON: () => ({}),
};

function installFetch(): ReturnType<typeof vi.fn> {
  const mock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
    const url = new URL(typeof input === "string" ? input : input.toString(), "http://localhost");
    const path = url.pathname;
    const method = init?.method ?? "GET";
    let body: unknown;
    if (path === "/api/v1/auth/me")
      body = {
        user: admin,
        csrf_token: "csrf-1",
        auth_method: "cookie",
        expires_at: "2026-11-26T00:00:00Z",
      };
    else if (path === "/api/v1/auth/status") body = { setup_required: false, loopback_setup: true };
    else if (path === "/api/v1/profiles/p1/regions" && method === "PUT")
      body = { regions: [], revision: 2 };
    else if (path === "/api/v1/profiles/p1/regions")
      body = {
        profile_id: "p1",
        regions: [
          {
            id: "bottom",
            region_type: "textbox",
            rect_x: 0.096875,
            rect_y: 0.75625,
            rect_w: 0.80625,
            rect_h: 0.158333,
          },
        ],
      };
    else body = {};
    return Promise.resolve(
      new Response(JSON.stringify(body), {
        status: 200,
        headers: {
          "content-type": "application/json",
          etag: `W/"v${url.pathname.length}"`,
        },
      }),
    );
  });
  globalThis.fetch = mock as unknown as typeof fetch;
  return mock;
}

/** The walk: the e2e drag, 5%×65% → 95%×97% of the frame, 12 pointer moves. */
async function drawBox() {
  const canvas = await screen.findByTestId("regions-canvas");
  canvas.getBoundingClientRect = () => CANVAS_RECT as DOMRect;
  // jsdom has no pointer capture; the page calls setPointerCapture on the
  // canvas, so the element needs the method to exist.
  canvas.setPointerCapture = vi.fn();
  canvas.releasePointerCapture = vi.fn();
  const pt = (fx: number, fy: number) => ({
    clientX: Math.round(fx * CANVAS_RECT.width),
    clientY: Math.round(fy * CANVAS_RECT.height),
  });
  fireEvent.pointerDown(canvas, pt(0.05, 0.65));
  for (let i = 1; i <= 12; i += 1) {
    fireEvent.pointerMove(canvas, pt(0.05 + (0.9 * i) / 12, 0.65 + (0.32 * i) / 12));
  }
  fireEvent.pointerUp(canvas);
}

it("a drawn box leaves the editor settled: the render count is bounded", async () => {
  installFetch();
  const onRender = vi.fn();
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={["/games/g1/regions?profile=p1"]}>
        <AuthProvider>
          <Profiler id="regions" onRender={onRender}>
            <Routes>
              <Route path="/games/:id/regions" element={<RegionsPage />} />
            </Routes>
          </Profiler>
        </AuthProvider>
      </MemoryRouter>
    </QueryClientProvider>,
  );

  // The stored box loads (the page is settled before the draw).
  const seeded = await screen.findByTestId("region-box-bottom");
  expect(seeded).toBeVisible();
  const before = onRender.mock.calls.length;

  await drawBox();

  // The drag landed: the seeded box and the drawn one.
  await screen.findByTestId("region-box-1");

  // The page settles: whatever the draw costs in renders, it finishes. If a
  // handler or effect re-dispatches itself the count keeps climbing and the
  // page never paints — the screenshot the e2e walk takes would hang.
  const rendersDuringSettle = onRender.mock.calls.length - before;
  expect(
    rendersDuringSettle,
    "the editor re-rendered a bounded number of times after the draw",
  ).toBeLessThanOrEqual(100);

  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(
    onRender.mock.calls.length,
    "no effect or handler keeps scheduling re-renders after the draw",
  ).toBeLessThanOrEqual(before + 100);
});

afterEach(cleanup);
