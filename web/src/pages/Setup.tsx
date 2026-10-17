import { useQuery } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { Navigate, useNavigate } from "react-router";

import { ApiError, authApi } from "../api/client";
import { ErrorState } from "../components/states";

/**
 * `/setup` — the first-run wizard slice of docs/spec/09 ("first-run wizard:
 * detect device, configure auth/storage/gateway roles"). This milestone ships the
 * step the appliance cannot be used without: claiming it by creating the first
 * admin. The device/storage/gateway steps arrive with their owning milestones.
 *
 * Three facts the form has to respect, all from ADR-0016 §7:
 * setup is loopback-only (a remote 403 is a real answer, not a retry), it creates
 * an account but mints **no session** (so the wizard must log in afterwards, which
 * proves the password was known), and once an admin exists it is 409 forever.
 */
export function SetupPage() {
  const status = useQuery({
    queryKey: ["auth", "status"],
    queryFn: ({ signal }) => authApi.status(signal),
    retry: false,
  });
  const [username, setUsername] = useState("admin");
  const [password, setPassword] = useState("");
  const [error, setError] = useState<ApiError | null>(null);
  const [busy, setBusy] = useState(false);
  const navigate = useNavigate();

  if (status.isPending) return <p className="muted">Checking this appliance…</p>;
  if (status.isError) return <ErrorState error={status.error} action="Cannot read setup state" />;
  if (!status.data.setup_required) return <Navigate to="/login" replace />;

  const onSubmit = async (event: FormEvent) => {
    event.preventDefault();
    setBusy(true);
    setError(null);
    try {
      await authApi.setup(username, password);
      // Deliberately no session: sign in like anybody else (ADR-0016 §7).
      navigate("/login", { replace: true });
    } catch (caught) {
      setError(
        caught instanceof ApiError
          ? caught
          : new ApiError({
              status: 0,
              code: "network",
              title: "Setup failed",
              detail: String(caught),
            }),
      );
    } finally {
      setBusy(false);
    }
  };

  return (
    <main>
      <h1>Claim this appliance</h1>
      <p className="muted">
        No administrator exists yet, so every privileged route refuses. The first account created
        here becomes the appliance&apos;s admin — this must be done on the machine itself, and it
        can only be done once.
      </p>
      {!status.data.loopback_setup ? (
        <p role="alert" className="error" data-testid="setup-not-local">
          This request did not come from the appliance itself. Open{" "}
          <code>http://127.0.0.1:8080/setup</code> on the machine, or run{" "}
          <code>game-narrator admin create</code> there.
        </p>
      ) : null}
      {error ? (
        <ErrorState
          error={error}
          action="Setup refused"
          onRetry={() => {
            void status.refetch();
          }}
        />
      ) : null}
      <form onSubmit={(event) => void onSubmit(event)} className="stack" data-testid="setup-form">
        <label>
          Username
          <input
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            autoComplete="username"
            required
            minLength={3}
            data-testid="setup-username"
          />
        </label>
        <label>
          Password
          <input
            type="password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            autoComplete="new-password"
            required
            minLength={12}
            data-testid="setup-password"
          />
          <span className="muted">
            At least 12 characters. Passwords are stored as PBKDF2 digests.
          </span>
        </label>
        <button
          type="submit"
          disabled={busy || !status.data.loopback_setup}
          data-testid="setup-submit"
        >
          {busy ? "Creating…" : "Create admin and continue"}
        </button>
      </form>
    </main>
  );
}
