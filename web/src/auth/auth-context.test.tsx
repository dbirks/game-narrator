import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen } from "@testing-library/react";
import { MemoryRouter, Route, Routes, useLocation } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { AuthProvider, RequireAuth } from "./auth-context";

/**
 * The auth guard against a fake fetch (docs/spec/12 "Unit" mode — synthetic
 * stubs, not a live Go server; the real first-run cycle is
 * web/e2e/catalog.spec.ts). What is being pinned is the *routing* consequence of
 * each answer from GET /auth/{me,status}: an unguarded page, or a page that shows
 * a spinner while the session is actually dead, is the failure users report as
 * "the app lost my data".
 */

function response(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "content-type": status >= 400 ? "application/problem+json" : "application/json" },
  });
}

const admin = {
  id: "01ADMIN",
  username: "admin",
  role: "admin" as const,
  disabled: false,
  created_at: "2026-10-16T00:00:00Z",
  password_changed_at: "2026-10-16T00:00:00Z",
};

function installFetch(handler: (path: string) => Response) {
  const mock = vi.fn((input: RequestInfo | URL): Promise<Response> => {
    const url = typeof input === "string" ? input : input.toString();
    return Promise.resolve(handler(new URL(url, "http://localhost").pathname));
  });
  globalThis.fetch = mock as unknown as typeof fetch;
  return mock;
}

function LocationProbe() {
  const location = useLocation();
  return <div data-testid="location">{`${location.pathname}${location.search}`}</div>;
}

function renderAt(path: string) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <MemoryRouter initialEntries={[path]}>
        <AuthProvider>
          <LocationProbe />
          <Routes>
            <Route
              path="/library"
              element={
                <RequireAuth>
                  <p>the library</p>
                </RequireAuth>
              }
            />
            <Route
              path="/games/:id/dialogue"
              element={
                <RequireAuth>
                  <p>the dialogue</p>
                </RequireAuth>
              }
            />
            <Route path="/login" element={<p>the sign-in form</p>} />
            <Route path="/setup" element={<p>the first-run wizard</p>} />
          </Routes>
        </AuthProvider>
      </MemoryRouter>
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

describe("RequireAuth", () => {
  it("bounces an anonymous caller to the sign-in form and keeps the requested route", async () => {
    installFetch((path) =>
      path === "/api/v1/auth/status"
        ? response(200, { setup_required: false, loopback_setup: true })
        : path === "/api/v1/auth/me"
          ? response(401, {
              type: "urn:game-narrator:error:unauthenticated",
              title: "Unauthorized",
              status: 401,
              detail: "no credential",
              instance: path,
              request_id: "r1",
            })
          : response(404, {}),
    );

    renderAt("/games/01GAME/dialogue?profile=01PROFILE");
    // A loading state first, not a flash of the protected page.
    expect(screen.getByTestId("auth-checking")).toBeInTheDocument();
    expect(await screen.findByText("the sign-in form")).toBeInTheDocument();
    expect(screen.queryByText("the dialogue")).not.toBeInTheDocument();
    expect(screen.getByTestId("location")).toHaveTextContent(
      "/login?next=%2Fgames%2F01GAME%2Fdialogue%3Fprofile%3D01PROFILE",
    );
  });

  it("sends an unclaimed appliance to /setup rather than a form that cannot work", async () => {
    installFetch((path) =>
      path === "/api/v1/auth/status"
        ? response(200, { setup_required: true, loopback_setup: true })
        : response(401, {
            type: "urn:game-narrator:error:unauthenticated",
            title: "Unauthorized",
            status: 401,
            detail: "no credential",
            instance: path,
            request_id: "r1",
          }),
    );

    renderAt("/library");
    expect(await screen.findByText("the first-run wizard")).toBeInTheDocument();
  });

  it("renders the protected route for a signed-in principal", async () => {
    installFetch((path) =>
      path === "/api/v1/auth/status"
        ? response(200, { setup_required: false, loopback_setup: true })
        : path === "/api/v1/auth/me"
          ? response(200, { user: admin, csrf_token: "tok", auth_method: "cookie" })
          : response(404, {}),
    );

    renderAt("/library");
    expect(await screen.findByText("the library")).toBeInTheDocument();
    expect(screen.queryByText("the sign-in form")).not.toBeInTheDocument();
  });

  it("says so when the credential store is broken, rather than showing the sign-in form", async () => {
    installFetch((path) =>
      path === "/api/v1/auth/status"
        ? response(200, { setup_required: false, loopback_setup: true })
        : response(500, {
            type: "urn:game-narrator:error:auth-fault",
            title: "Internal Server Error",
            status: 500,
            detail: "the credential store could not answer",
            instance: path,
            request_id: "r2",
          }),
    );

    renderAt("/library");
    const alert = await screen.findByTestId("auth-error");
    expect(alert).toHaveTextContent("credential store could not answer");
    expect(screen.queryByText("the sign-in form")).not.toBeInTheDocument();
  });
});
