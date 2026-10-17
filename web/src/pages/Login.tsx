import { useState, type FormEvent } from "react";
import { Navigate, useLocation, useNavigate, useSearchParams } from "react-router";

import { ApiError } from "../api/client";
import { useAuth } from "../auth/auth-context";

/**
 * `/login` — the form ADR-0016 §7 exists for. Two things the page must say
 * correctly rather than guess: the appliance is unclaimed (go to `/setup`, where
 * the 401 would otherwise look like a wrong password), and a 429 is a rate limit,
 * not a rejection — `Retry-After` is shown instead of a spinner so an operator
 * waiting on a keyboard-mashing script can see what the server is doing.
 */
export function LoginPage() {
  const { state, login } = useAuth();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();
  const location = useLocation();
  const [params] = useSearchParams();

  if (state === "authenticated") return <Navigate to={nextTarget(params, location)} replace />;
  if (state === "setup") return <Navigate to="/setup" replace />;

  const onSubmit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await login(username, password);
      navigate(nextTarget(params, location), { replace: true });
    } catch (caught) {
      setError(
        caught instanceof ApiError
          ? caught
          : new ApiError({
              status: 0,
              code: "network",
              title: "Sign-in failed",
              detail: String(caught),
            }),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <main>
      <h1>Sign in</h1>
      {error ? (
        <div role="alert" className="error" data-testid="login-error">
          <p>
            {error.status === 401
              ? "Username or password is not right."
              : error.status === 429
                ? `Too many attempts${error.retryAfter ? `; try again in ${error.retryAfter} seconds` : "."}`
                : `${error.title} — ${error.detail}`}
          </p>
          {error.status === 401 ? (
            <p className="muted error-meta">
              The server does not say which half was wrong, and the attempt is in its audit log.
            </p>
          ) : null}
          {error.retryAfter && error.status === 429 ? (
            <p className="muted error-meta">
              <code>Retry-After: {error.retryAfter}</code>
            </p>
          ) : null}
        </div>
      ) : null}
      <form onSubmit={(event) => void onSubmit(event)} className="stack" data-testid="login-form">
        <label>
          Username
          <input
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            autoComplete="username"
            required
            data-testid="login-username"
          />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            autoComplete="current-password"
            required
            data-testid="login-password"
          />
        </label>
        <button type="submit" disabled={busy} data-testid="login-submit">
          {busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </main>
  );
}

/**
 * Where a completed sign-in goes: the route the guard bounced the caller from,
 * when it is a same-origin path, and the library otherwise. An absolute URL in
 * `?next=` is ignored rather than followed — an open redirect is the classic way
 * a login page hands a session cookie to somebody else's host.
 */
function nextTarget(params: URLSearchParams, location: { search: string }): string {
  const raw = params.get("next") ?? (location.search.includes("next=") ? "" : "/library");
  if (!raw.startsWith("/") || raw.startsWith("//")) return "/library";
  return raw;
}
