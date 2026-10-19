import { useState } from "react";
import { Link, useNavigate } from "react-router";

import { useAuth } from "../auth/auth-context";

/**
 * `src/components/AppNav.tsx` — primary navigation plus the sign-in state.
 *
 * Showing the role is not decoration: the catalogue is read-only for a viewer and
 * writable from curator up (ADR-0016 §3), so "who am I, what can I do" has to be
 * answerable from the header before somebody wonders why a button is missing.
 */
export function AppNav() {
  const { state, user, role, logout, atLeast } = useAuth();
  const [signingOut, setSigningOut] = useState(false);
  const navigate = useNavigate();

  const onLogout = async () => {
    setSigningOut(true);
    try {
      await logout();
      navigate("/login");
    } finally {
      setSigningOut(false);
    }
  };

  return (
    <nav aria-label="Primary">
      <Link to="/library">Library</Link>
      {atLeast("curator") ? (
        <Link to="/games/new" data-testid="nav-new-game">
          New game
        </Link>
      ) : null}
      {/* /play starts and stops sessions, which the scope table reserves for
          operator and up (docs/spec/08); the page itself degrades to a read-only
          watch for lower roles, but the nav hides the link rather than offering
          a form whose button would 403. */}
      {atLeast("operator") ? (
        <Link to="/play" data-testid="nav-play">
          Play
        </Link>
      ) : null}
      <Link to="/diagnostics">Diagnostics</Link>
      {/* The two operator-facing pages: settings is admin-only (its API routes
          say so with a 403), the test harness is developer-and-up (ADR-0047).
          Hiding a link the role cannot use is the nav's job, not the page's. */}
      {atLeast("admin") ? (
        <Link to="/settings" data-testid="nav-settings">
          Settings
        </Link>
      ) : null}
      {atLeast("developer") ? (
        <Link to="/tests" data-testid="nav-tests">
          Tests
        </Link>
      ) : null}
      {/* The clip store is developer-scoped (ADR-0073): a clip is a recording
          of gameplay frames, the same evidence class as the dev-trace store. */}
      {atLeast("developer") ? (
        <Link to="/replay-clips" data-testid="nav-replay-clips">
          Replay clips
        </Link>
      ) : null}
      <span className="nav-spacer" />
      {state === "authenticated" && user ? (
        <span className="nav-user" data-testid="nav-user">
          <span className="nav-username">{user.username}</span>
          <span className="badge" data-testid="nav-role">
            {role}
          </span>
          <button
            type="button"
            onClick={() => void onLogout()}
            disabled={signingOut}
            data-testid="nav-logout"
          >
            {signingOut ? "Signing out…" : "Sign out"}
          </button>
        </span>
      ) : state === "setup" ? (
        <Link to="/setup" data-testid="nav-setup">
          Claim this appliance
        </Link>
      ) : (
        <Link to="/login" data-testid="nav-login">
          Sign in
        </Link>
      )}
    </nav>
  );
}
