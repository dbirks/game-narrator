import {
  useCallback,
  useEffect,
  useRef,
  useState,
  type KeyboardEvent,
  type ReactNode,
} from "react";
import { Link, useLocation, useNavigate } from "react-router";

import { fetchSystemInfo } from "../api/system";
import { useAuth } from "../auth/auth-context";
import { useLiveStatus } from "../lib/liveStatus";

/**
 * `src/components/Shell.tsx` — the global chrome of the Plum & Peach
 * redesign (docs/design/plum-peach "Global chrome"; ADR-0093): the 28px
 * status bar — the one nav — with the wordmark, the primary nav, the
 * developer entry, the signed-in principal and the server-rendered live
 * string (ADR-0100), the 3-row dither fade, the page container, the
 * ask-the-agent pill (⌘J) with its docked stub, and the ⌘K command
 * palette stub.
 *
 * The legacy second nav row is gone: the design has exactly one nav (the
 * status bar). The principal (name, role chip, sign-out) moved to the bar's
 * right side; "New game" becomes a Library action ("Add a game" tile,
 * Phase 1); the harness and the clip store sit under the developer entry,
 * visible to developer-and-up. The moved elements keep their pre-redesign
 * data-testids, which the e2e suite addresses.
 *
 * The pill and palette are clean seams: their chrome, keyboard contract
 * (⌘J / ⌘K from anywhere, Esc to close, focus into the palette input) and
 * context chip are real; the agent backend (ADR-0099) and the search
 * backend (ADR-0098) arrive in Phase 1, and the stubs say so.
 */

/** The nav of the design's status bar. Phase 0 seam: "traces" and the old
 *  inspector paths coexist — /traces redirects to /developer until the
 *  Phase 1 rename (ADR-0093 §4), so the link never 404s. */
const NAV_ITEMS = [
  { id: "library", label: "library", to: "/library" },
  { id: "play", label: "play", to: "/play" },
  { id: "traces", label: "traces", to: "/traces" },
  { id: "settings", label: "settings", to: "/settings" },
] as const;

/** Which nav item a route belongs to (the active item is --peach 700). */
function activeNavId(pathname: string): (typeof NAV_ITEMS)[number]["id"] | null {
  if (pathname.startsWith("/library")) return "library";
  if (pathname.startsWith("/play")) return "play";
  if (pathname.startsWith("/traces") || pathname.startsWith("/developer")) return "traces";
  if (pathname.startsWith("/settings")) return "settings";
  return null;
}

/** The agent pill's context chip: one word for the current screen
 *  (docs/design/plum-peach "Ask the agent"). Phase 1 deepens it with the
 *  selection (`cast › Grandma`); the seam is the chip's text, not its place. */
export function routeContextChip(pathname: string): string {
  const segments = pathname.split("/").filter(Boolean);
  const head = segments[0] ?? "";
  if (head === "games") {
    if (segments[1] === "new") return "setup";
    const kind = segments[2];
    if (kind === "cast" || kind === "characters" || kind === "voices") return "cast";
    if (
      kind === "overview" ||
      kind === "dialogue" ||
      kind === "graph" ||
      kind === "regions" ||
      kind === "publish"
    ) {
      return "game";
    }
    return "game";
  }
  const names: Record<string, string> = {
    library: "library",
    play: "play",
    traces: "traces",
    developer: "traces",
    diagnostics: "health",
    health: "health",
    settings: "settings",
    tests: "tests",
    "replay-clips": "clips",
    login: "sign in",
    setup: "setup",
  };
  if (head === "dev" && segments[1] === "kit") return "dev kit";
  return names[head] ?? (head === "" ? "home" : head);
}

interface ShellProps {
  children: ReactNode;
}

/** The footer's build line (docs/design/plum-peach "Global chrome" footer):
 *  version, commit and build time from the public system/info endpoint —
 *  the three values `doctor` prints, in one line. */
export function buildLine(info: {
  version: string;
  commit: string;
  started_at: string;
  go_version: string;
}): string {
  const built = new Date(info.started_at).toISOString().replace("T", " ").slice(0, 16);
  return `Build: ${info.version} · ${info.commit} · ${built} UTC · Go ${info.go_version}`;
}

export function Shell({ children }: ShellProps) {
  const location = useLocation();
  const navigate = useNavigate();
  const { state: authState, user, role, atLeast, logout } = useAuth();
  const { text: statusText } = useLiveStatus();
  const [agentOpen, setAgentOpen] = useState(false);
  const [paletteOpen, setPaletteOpen] = useState(false);
  const [footerLine, setFooterLine] = useState<string | null>(null);
  const [signingOut, setSigningOut] = useState(false);
  const paletteInputRef = useRef<HTMLInputElement>(null);

  // Sign-out ends the session on the server, then lands on the sign-in form
  // (the remembered-target behaviour of /login?next= is the page's, not the
  // bar's).
  const onLogout = useCallback(async () => {
    setSigningOut(true);
    try {
      await logout();
      navigate("/login");
    } finally {
      setSigningOut(false);
    }
  }, [logout, navigate]);

  const active = activeNavId(location.pathname);
  const chip = routeContextChip(location.pathname);

  const togglePalette = useCallback(() => setPaletteOpen((open) => !open), []);
  const toggleAgent = useCallback(() => setAgentOpen((open) => !open), []);

  // ⌘K / ⌘J from anywhere (Ctrl on keyboards without a Meta key).
  useEffect(() => {
    const onKey = (event: globalThis.KeyboardEvent) => {
      if (!(event.metaKey || event.ctrlKey)) return;
      const key = event.key.toLowerCase();
      if (key === "k") {
        event.preventDefault();
        togglePalette();
      } else if (key === "j") {
        event.preventDefault();
        toggleAgent();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [togglePalette, toggleAgent]);

  // The palette takes focus into its input the moment it opens (keyboard
  // contract: ↑↓/↵/Esc; Phase 0 the input is the only control).
  useEffect(() => {
    if (paletteOpen) paletteInputRef.current?.focus();
  }, [paletteOpen]);

  // The footer's build line: one public read, no polling (it never changes
  // without a restart, and a restart reloads the page).
  useEffect(() => {
    const controller = new AbortController();
    fetchSystemInfo(controller.signal)
      .then((info) => setFooterLine(buildLine(info)))
      .catch(() => undefined); // offline dev shell: the footer line just waits
    return () => controller.abort();
  }, []);

  const onPaletteKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      setPaletteOpen(false);
    }
  };

  return (
    <>
      <header className="status-bar" data-testid="status-bar">
        <span className="status-bar-wordmark" data-testid="status-wordmark">
          GAME·NARRATOR
        </span>
        <nav aria-label="Status" className="status-nav" data-testid="status-nav">
          {NAV_ITEMS.map((item) => (
            <Link
              key={item.id}
              to={item.to}
              data-testid={`status-nav-${item.id}`}
              aria-current={active === item.id ? "page" : undefined}
            >
              {item.label}
            </Link>
          ))}
        </nav>
        {/* The developer entry (ADR-0093 §4 IA): the two developer-scoped
            destinations — the harness and the clip store — grouped under one
            label, visible to developer-and-up only (the API 403s the rest;
            the bar hides what the role cannot use). The links keep the
            pre-redesign nav testids. */}
        {atLeast("developer") ? (
          <span className="status-dev" data-testid="status-dev">
            <span className="status-dev-label">developer</span>
            <Link to="/tests" data-testid="nav-tests">
              tests
            </Link>
            <Link to="/replay-clips" data-testid="nav-replay-clips">
              replay clips
            </Link>
          </span>
        ) : null}
        {/* The principal, moved from the legacy second nav row into the bar's
            right side (the design has one nav): name, role chip, sign-out.
            Unclaimed appliance offers the claim link; anonymous, sign-in —
            the legacy row's three states, in the bar. */}
        {authState === "authenticated" && user ? (
          <span className="nav-user" data-testid="nav-user">
            <span className="nav-username">{user.username}</span>
            <span className="badge" data-testid="nav-role">
              {role}
            </span>
            <button
              type="button"
              className="status-logout"
              onClick={() => void onLogout()}
              disabled={signingOut}
              data-testid="nav-logout"
            >
              {signingOut ? "signing out…" : "sign out"}
            </button>
          </span>
        ) : authState === "setup" ? (
          <Link to="/setup" data-testid="nav-setup">
            claim this appliance
          </Link>
        ) : (
          <Link to="/login" data-testid="nav-login">
            sign in
          </Link>
        )}
        {/* The live status string: the server's canonical text (ADR-0100).
            Not an aria-live region: it ticks every second and would make
            screen readers narrate the machine to itself. */}
        <span className="status-live" data-testid="status-live">
          {statusText}
        </span>
      </header>
      <div className="status-fade" data-testid="status-fade" aria-hidden="true" />

      <div className="page-container">{children}</div>

      <footer className="status-footer" data-testid="status-footer" aria-label="Build info">
        <span>{footerLine ?? "Build: · · ·"}</span>
      </footer>

      <button
        type="button"
        className="agent-pill offset-shadow"
        data-testid="agent-pill"
        aria-haspopup="dialog"
        aria-expanded={agentOpen}
        aria-controls="agent-dock"
        onClick={() => toggleAgent()}
      >
        <span className="agent-pill-context" data-testid="agent-pill-context">
          {chip}
        </span>
        <span>ask the agent</span>
        <kbd>⌘J</kbd>
      </button>

      {agentOpen ? (
        <aside
          className="agent-dock"
          id="agent-dock"
          data-testid="agent-dock"
          aria-label="Ask the agent"
        >
          <h2>
            Ask the agent
            <span className="agent-pill-context" data-testid="agent-dock-context">
              {chip}
            </span>
          </h2>
          <p className="muted" data-testid="agent-dock-stub">
            The agent dock arrives in Phase 1 (ADR-0099): NEEDS YOU, tool cards and chat with the
            {` `}
            <code>{"{route, gameId, profileId, selection, visibleIds}"}</code> context payload on
            every message.
          </p>
          <button
            type="button"
            className="link-button"
            data-testid="agent-dock-close"
            onClick={() => setAgentOpen(false)}
          >
            Close (Esc)
          </button>
        </aside>
      ) : null}

      {paletteOpen ? (
        <>
          <div
            className="palette-backdrop"
            data-testid="palette-backdrop"
            onClick={() => setPaletteOpen(false)}
          />
          <div
            className="palette"
            role="dialog"
            aria-modal="true"
            aria-label="Search"
            data-testid="palette"
          >
            <h2>Search</h2>
            <input
              ref={paletteInputRef}
              type="search"
              placeholder="Search games, characters, lines, traces, settings and actions"
              data-testid="palette-input"
              onKeyDown={onPaletteKeyDown}
            />
            <p className="palette-note" data-testid="palette-note">
              The result groups land with the search backend (ADR-0098): games · characters · lines
              · traces · settings · actions — ↑↓ move, ↵ open, Tab hands the query to the agent.
            </p>
            <button
              type="button"
              className="link-button"
              data-testid="palette-close"
              onClick={() => setPaletteOpen(false)}
            >
              Close (Esc)
            </button>
          </div>
        </>
      ) : null}
    </>
  );
}
