import { useQuery, useQueryClient } from "@tanstack/react-query";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  type ReactNode,
  useState,
} from "react";
import { Navigate, useLocation } from "react-router";

import { ApiError, api, authApi, type AuthMe, type Role, type User } from "../api/client";

/**
 * `src/auth/auth-context.tsx` — the signed-in principal for the SPA.
 *
 * The credential is the `HttpOnly` session cookie, so the browser, not React,
 * holds the secret. What the app needs is (a) who the caller is, (b) the CSRF
 * token the client must echo (ADR-0016 §4 re-echoes it on `GET /auth/me` precisely
 * so a reload is not indistinguishable from a logout), and (c) a single answer to
 * "is this route mine to show".
 *
 * A 401 is modelled as "anonymous", not as an error: an expired session and a
 * visitor who has never signed in are the same UI state (the sign-in form), and
 * the difference matters only in the audit log. Any other failure stays an error
 * so a dead appliance never looks like a logged-out user.
 */

export const authQueryKey = ["auth", "me"] as const;
export const statusQueryKey = ["auth", "status"] as const;

const ROLE_ORDER: readonly Role[] = ["viewer", "curator", "operator", "developer", "admin"];

export function roleAtLeast(held: Role | null, min: Role): boolean {
  if (!held) return false;
  const index = ROLE_ORDER.indexOf(held);
  return index >= 0 && index >= ROLE_ORDER.indexOf(min);
}

export interface AuthValue {
  /** `checking` until /auth/me answers; `error` only for a real fault. */
  state: "checking" | "setup" | "anonymous" | "authenticated" | "error";
  user: User | null;
  role: Role | null;
  atLeast: (min: Role) => boolean;
  error: ApiError | null;
  login: (username: string, password: string) => Promise<void>;
  logout: () => Promise<void>;
  reload: () => Promise<void>;
  /**
   * The anonymous state came from this tab's own sign-out, not an expired
   * session: the guard then lands on plain /login instead of remembering the
   * page as ?next= (the a11y walk caught the two navigations racing).
   */
  signedOut: boolean;
}

const AuthContext = createContext<AuthValue | null>(null);

export function AuthProvider({ children }: { children: ReactNode }) {
  const queryClient = useQueryClient();

  const me = useQuery({
    queryKey: authQueryKey,
    queryFn: async ({ signal }): Promise<AuthMe | null> => {
      try {
        return await authApi.me(signal);
      } catch (error) {
        if (error instanceof ApiError && error.status === 401) return null;
        throw error;
      }
    },
    retry: false,
    staleTime: 30_000,
  });

  const status = useQuery({
    queryKey: statusQueryKey,
    queryFn: ({ signal }) => authApi.status(signal),
    retry: false,
    staleTime: 60_000,
  });

  // The token is session-scoped and the client is module-level, so principal and
  // token are set together — a principal without a token would make every write
  // a 403 (ADR-0016 §5).
  useEffect(() => {
    api.setCsrfToken(me.data?.csrf_token ?? null);
  }, [me.data?.csrf_token]);

  useEffect(() => {
    // One retry with a re-read token is what a stale SPA build needs; the
    // handler is how any other request's 401 becomes "show the sign-in form"
    // instead of an empty page (ADR-0016 follow-ups).
    api.setCsrfRefresh(async () => (await authApi.me()).csrf_token ?? null);
    api.setUnauthenticatedHandler(() => {
      queryClient.setQueryData<AuthMe | null>(authQueryKey, null);
    });
    return () => api.setUnauthenticatedHandler(undefined);
  }, [queryClient]);

  const state: AuthValue["state"] = useMemo(() => {
    if (status.isPending || me.isPending) return "checking";
    if (status.isError || me.isError) return "error";
    if (status.data?.setup_required) return "setup";
    if (!me.data) return "anonymous";
    return "authenticated";
  }, [status.isPending, status.isError, status.data, me.isPending, me.isError, me.data]);

  const login = useCallback(
    async (username: string, password: string) => {
      const result = await authApi.login(username, password);
      // The login response *is* the new principal; re-reading /auth/me would ask
      // the server to tell us what we just heard.
      const principal: AuthMe = {
        user: result.user,
        csrf_token: result.csrf_token,
        auth_method: "cookie",
        expires_at: result.expires_at,
      };
      queryClient.setQueryData<AuthMe | null>(authQueryKey, principal);
      setSignedOut(false);
      // Whatever /auth/status said a minute ago, the appliance is claimed now.
      queryClient.setQueryData(statusQueryKey, { setup_required: false, loopback_setup: true });
    },
    [queryClient],
  );

  const [signedOut, setSignedOut] = useState(false);

  const logout = useCallback(async () => {
    await authApi.logout();
    setSignedOut(true);
    queryClient.setQueryData<AuthMe | null>(authQueryKey, null);
  }, [queryClient]);

  const reload = useCallback(async () => {
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: authQueryKey }),
      queryClient.invalidateQueries({ queryKey: statusQueryKey }),
    ]);
  }, [queryClient]);

  const value = useMemo<AuthValue>(
    () => ({
      state,
      user: me.data?.user ?? null,
      role: me.data?.user.role ?? null,
      atLeast: (min: Role) => roleAtLeast(me.data?.user.role ?? null, min),
      error: me.error instanceof ApiError ? me.error : null,
      login,
      logout,
      reload,
      signedOut,
    }),
    [state, me.data, me.error, login, logout, reload, signedOut],
  );

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}

export function useAuth(): AuthValue {
  const value = useContext(AuthContext);
  if (!value) throw new Error("useAuth must be used inside <AuthProvider>");
  return value;
}

/**
 * The guard for protected routes. Each non-authenticated state says what it is:
 * docs/spec/09 "UX robustness" asks for correct loading/empty/error states, and a
 * spinner that never resolves is exactly how an unguarded route looks when the
 * session died in another tab.
 */
export function RequireAuth({ children }: { children: ReactNode }) {
  const { state, error, reload, signedOut } = useAuth();
  const location = useLocation();

  switch (state) {
    case "checking":
      return (
        <p className="muted" data-testid="auth-checking">
          Checking this appliance…
        </p>
      );
    case "setup":
      return <Navigate to="/setup" replace />;
    case "error":
      return (
        <p role="alert" className="error" data-testid="auth-error">
          Cannot check your session: {error ? error.detail : "network error"}.{" "}
          <button
            type="button"
            onClick={() => {
              void reload();
            }}
          >
            Try again
          </button>
        </p>
      );
    case "anonymous": {
      // A deliberate sign-out lands on the plain form; only a session that
      // lapsed under a page remembers where it was.
      if (signedOut) return <Navigate to="/login" replace />;
      const next = `${location.pathname}${location.search}`;
      return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />;
    }
    case "authenticated":
      return children;
  }
}
