import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, useLocation, useNavigate } from "react-router";

import { fetchSystemInfo } from "../api/system";
import { useAuth } from "../auth/auth-context";
import { subscribeAgentQuery } from "../lib/search";
import { agentChipText, routeContextChip, useSelectionLabel } from "./agent/agentContext";
import { AgentDock } from "./agent/AgentDock";
import { useLiveStatus } from "../lib/liveStatus";
import { KSearchPalette } from "../pages/KSearch";

/**
 * `src/components/Shell.tsx` — the global chrome of the Plum & Peach
 * redesign (docs/design/plum-peach "Global chrome"; ADR-0093): the 28px
 * status bar — the one nav — with the wordmark, the primary nav, the
 * developer entry, the signed-in principal and the server-rendered live
 * string (ADR-0100), the 3-row dither fade, the page container, the
 * ask-the-agent pill (⌘J) with its docked panel (the agent dock, Phase 1
 * item 3, over the ADR-0099 wire), and the ⌘K command palette.
 *
 * The legacy second nav row is gone: the design has exactly one nav (the
 * status bar). The principal (name, role chip, sign-out) moved to the bar's
 * right side; "New game" becomes a Library action ("Add a game" tile,
 * Phase 1); the harness and the clip store sit under the developer entry,
 * visible to developer-and-up. The moved elements keep their pre-redesign
 * data-testids, which the e2e suite addresses.
 *
 * The palette is the real ⌘K overlay (plan item 2; ADR-0098): one debounced
 * `GET /api/v1/search` per keystroke, grouped results, ↑↓/↵/Tab/Esc. The
 * dock is the real agent dock (plan item 3; ADR-0099): the pill and the
 * dock read one chip seam (agentContext) — the bare screen name, or
 * `screen › selection` once the URL's selection has resolved (UX-01 — the
 * chip follows the selection, and the outgoing message carries the same
 * context payload the chip shows). The ⌘K Tab key dispatches
 * `gn:agent-query` (src/lib/search.ts) and the dock listens — opening and
 * pre-filling its input with the normalised query (UX-02).
 */

// Re-exported for the pre-redesign imports (the seam now lives with the
// dock: web/src/components/agent/agentContext.ts).
export { routeContextChip };

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
  // The ⌘K Tab handoff (UX-02): the query the palette handed to the dock.
  // The dock mounts fresh each time it opens and seeds its input from this
  // value, so the pre-fill is the event (gn:agent-query) plus this state.
  const [agentDraft, setAgentDraft] = useState("");
  const [footerLine, setFooterLine] = useState<string | null>(null);
  const [signingOut, setSigningOut] = useState(false);

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
  // The pill's chip: the dock's seam (agentContext), so the pill and the
  // docked header — and the outgoing message's context payload — read the
  // same words (UX-01).
  const pillProfileId = new URLSearchParams(location.search).get("profile");
  const pillSelectionName = useSelectionLabel(location, pillProfileId);
  const chip = agentChipText(routeContextChip(location.pathname), pillSelectionName);

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

  // The ⌘K palette's Tab key (and its "Ask the agent: …" row) dispatches
  // the handoff event; the dock reacts by opening and pre-filling its input
  // (UX-02). The real dock (plan item 3) subscribes to the same event.
  useEffect(() => {
    return subscribeAgentQuery((detail) => {
      setAgentDraft(detail.query);
      setAgentOpen(true);
    });
  }, []);

  // The footer's build line: one public read, no polling (it never changes
  // without a restart, and a restart reloads the page).
  useEffect(() => {
    const controller = new AbortController();
    fetchSystemInfo(controller.signal)
      .then((info) => setFooterLine(buildLine(info)))
      .catch(() => undefined); // offline dev shell: the footer line just waits
    return () => controller.abort();
  }, []);

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

      {/* The pill is the dock's launcher; while the panel is open the pill is
          its docked form — the panel's header carries the same chip and its
          own close control, so the launcher drops out of the layer (it would
          otherwise sit over the panel's input row, bottom-right). ⌘J still
          toggles from the window handler, so hiding never strands the close. */}
      <button
        type="button"
        className="agent-pill offset-shadow"
        data-testid="agent-pill"
        aria-haspopup="dialog"
        aria-expanded={agentOpen}
        aria-controls="agent-dock"
        hidden={agentOpen}
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
          <AgentDock onClose={() => setAgentOpen(false)} initialQuery={agentDraft} />
        </aside>
      ) : null}

      {paletteOpen ? <KSearchPalette onClose={() => setPaletteOpen(false)} /> : null}
    </>
  );
}
