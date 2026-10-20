import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";

import { ApiError } from "../api/client";
import { ErrorCard, KitEmptyState } from "../components/kit";
import {
  emitAgentQuery,
  executeSearchAction,
  KIND_LABELS,
  normalizeAgentQuery,
  useCatalogSearch,
  type SearchGroup,
  type SearchKind,
  type SearchResult,
} from "../lib/search";

/**
 * `src/pages/KSearch.tsx` — the ⌘K general search overlay (docs/design
 * plum-peach UX-02, ADR-0098; plan.md Phase 1 item 2).
 *
 * The Shell mounts this from any route (the ⌘K / ⌘J keyboard contract lives
 * in the Shell's chrome): it is a global overlay, never a navigation target.
 * One debounced call per keystroke answers `GET /api/v1/search`; the server
 * owns grouping, role filtering and scoring. The keyboard contract:
 *
 *   ↑↓ move the active row · ↵ opens the active row (or runs its action) ·
 *   Tab hands the query to the agent dock · Esc closes.
 *
 * The active row is a real, focused <button> — the design's --peach fill and
 * 2px coral focus ring come from the stylesheet, so the keyboard focus IS the
 * active row (no aria-activedescendant bookkeeping, and screen readers read
 * real buttons). Line rows carry their line id in mono (the design's
 * "line rows show the line id"); the footnote bar spells the contract out.
 */

/** One flattened palette row: a server row, or the client-side "Ask the
 *  agent: …" row the Tab key makes reachable by mouse (UX-02). */
interface PaletteRow {
  kind: SearchKind;
  id: string;
  title: string;
  subtitle?: string;
  route?: string;
  action?: SearchResult["action"];
  askAgent?: boolean;
}

/** The "Ask the agent: …" row's id prefix (one per open query). */
const ASK_AGENT_ID = "ask-agent";

/** The result note's tone: a queued action, a failure, or nothing. */
interface PaletteResult {
  ok: boolean;
  text: string;
  code?: string;
  requestId?: string;
}

export interface KSearchPaletteProps {
  /** Close the overlay (Esc, backdrop, or a finished row action). */
  onClose: () => void;
}

export function KSearchPalette({ onClose }: KSearchPaletteProps) {
  const navigate = useNavigate();
  const [query, setQuery] = useState("");
  const [retryKey, setRetryKey] = useState(0);
  const [result, setResult] = useState<PaletteResult | null>(null);
  const [running, setRunning] = useState(false);
  const inputRef = useRef<HTMLInputElement>(null);
  const rowRefs = useRef<Array<HTMLButtonElement | null>>([]);
  const previouslyFocused = useRef<HTMLElement | null>(null);

  const trimmed = query.trim();
  const { data, error, pending } = useCatalogSearch(query, retryKey);

  // Focus contract: the input takes focus the moment the overlay mounts
  // (the Shell renders it only while open); the element the palette stole
  // focus from gets it back on unmount, so Esc never strands the keyboard.
  useEffect(() => {
    previouslyFocused.current =
      document.activeElement instanceof HTMLElement ? document.activeElement : null;
    inputRef.current?.focus();
    return () => {
      previouslyFocused.current?.focus();
    };
  }, []);

  // The rows, in the server's canonical group order. The "Ask the agent: …"
  // row leads the action group while a query is live (the design's action
  // group offers the app's actions and that row); it is the mouse path to
  // exactly what Tab does.
  const sections = useMemo<Array<{ kind: SearchKind; rows: PaletteRow[] }>>(() => {
    const out = (data?.groups ?? []).map((group: SearchGroup) => ({
      kind: group.kind,
      rows: group.items.map((item) => rowFromItem(item)),
    }));
    if (trimmed.length > 0) {
      const ask: PaletteRow = {
        kind: "action",
        id: ASK_AGENT_ID,
        title: `Ask the agent: ${trimmed}`,
        subtitle: "hands the query to the dock · tab",
        askAgent: true,
      };
      const actionIndex = out.findIndex((section) => section.kind === "action");
      if (actionIndex >= 0) out[actionIndex].rows.unshift(ask);
      else out.push({ kind: "action", rows: [ask] });
    }
    return out;
  }, [data, trimmed]);

  const rows = useMemo(() => sections.flatMap((section) => section.rows), [sections]);

  const agentPrompt = data?.agent_prompt ?? normalizeAgentQuery(query);

  const focusRow = useCallback(
    (index: number) => {
      const count = rows.length;
      if (count === 0) return;
      const wrapped = ((index % count) + count) % count;
      const element = rowRefs.current[wrapped];
      element?.focus();
      element?.scrollIntoView({ block: "nearest" });
    },
    [rows.length],
  );

  /** Tab's job (UX-02): the normalised query goes to the agent dock; the
   *  overlay closes. An empty query has nothing to hand over — it just
   *  closes, the way a bare Tab would move on. */
  const handOff = useCallback(() => {
    const text = normalizeAgentQuery(agentPrompt);
    if (text.length > 0) emitAgentQuery(text);
    onClose();
  }, [agentPrompt, onClose]);

  const runRow = useCallback(
    (row: PaletteRow) => {
      if (row.askAgent) {
        handOff();
        return;
      }
      if (row.route !== undefined) {
        onClose();
        navigate(row.route);
        return;
      }
      if (row.action === undefined) return;
      void (async () => {
        setRunning(true);
        setResult(null);
        try {
          const text = await executeSearchAction(row, data?.groups ?? []);
          setResult({ ok: true, text });
        } catch (error) {
          const apiError = error instanceof ApiError ? error : null;
          setResult({
            ok: false,
            text: apiError?.detail ?? "The action did not run.",
            code: apiError?.code,
            requestId: apiError?.requestId || undefined,
          });
        } finally {
          setRunning(false);
        }
      })();
    },
    [data, handOff, navigate, onClose],
  );

  const onInputKeyDown = (event: React.KeyboardEvent<HTMLInputElement>) => {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focusRow(0);
        break;
      case "ArrowUp":
        event.preventDefault();
        focusRow(rows.length - 1);
        break;
      case "Enter":
        event.preventDefault();
        if (rows.length > 0) runRow(rows[0]);
        break;
      case "Tab":
        event.preventDefault();
        handOff();
        break;
      case "Escape":
        event.preventDefault();
        onClose();
        break;
      default:
        break;
    }
  };

  const onRowKeyDown = (event: React.KeyboardEvent<HTMLButtonElement>, index: number) => {
    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        focusRow(index + 1);
        break;
      case "ArrowUp":
        event.preventDefault();
        focusRow(index - 1);
        break;
      case "Home":
        event.preventDefault();
        focusRow(0);
        break;
      case "End":
        event.preventDefault();
        focusRow(rows.length - 1);
        break;
      case "Tab":
        event.preventDefault();
        handOff();
        break;
      case "Escape":
        event.preventDefault();
        onClose();
        break;
      default:
        break;
    }
  };

  // Flat row index across the rendered sections (the refs array is indexed
  // by this number; the group headers in between are not focusable).
  let flatIndex = -1;

  return (
    <>
      <div className="palette-backdrop" data-testid="palette-backdrop" onClick={onClose} />
      <div
        className="palette"
        role="dialog"
        aria-modal="true"
        aria-label="Search"
        data-testid="palette"
      >
        <h2>
          Search{" "}
          <span className="palette-kbd" aria-hidden="true">
            ⌘K
          </span>
        </h2>
        <input
          ref={inputRef}
          type="text"
          value={query}
          placeholder="Search games, characters, lines, traces, settings and actions"
          aria-label="Search"
          autoComplete="off"
          spellCheck={false}
          data-testid="palette-input"
          onChange={(event) => {
            setQuery(event.target.value);
            setResult(null);
          }}
          onKeyDown={onInputKeyDown}
        />
        {result !== null ? (
          <p
            className={`palette-result${result.ok ? " palette-result-ok" : " palette-result-err"}`}
            data-testid="palette-result"
          >
            {result.text}
            {result.code !== undefined ? (
              <span className="palette-result-meta">
                {" "}
                · {result.code}
                {result.requestId !== undefined ? ` · ${result.requestId}` : ""}
              </span>
            ) : null}
          </p>
        ) : null}
        {error !== null ? (
          <ErrorCard
            title={error.detail || "Search is unavailable."}
            code={error.code}
            requestId={error.requestId || undefined}
            onRetry={() => setRetryKey((key) => key + 1)}
            testid="palette-error"
          />
        ) : null}
        <ul className="palette-results" data-testid="palette-results">
          {pending && data === null ? (
            <li className="palette-note" data-testid="palette-loading">
              {trimmed.length > 0 ? "Searching…" : "Loading recent items…"}
            </li>
          ) : null}
          {pending && data !== null ? (
            <li className="palette-note" data-testid="palette-updating" aria-hidden="true">
              Updating…
            </li>
          ) : null}
          {!pending && rows.length === 0 && error === null ? (
            <li>
              <KitEmptyState
                title={trimmed.length > 0 ? "No matches" : "Nothing here yet"}
                testid="palette-empty"
              >
                {trimmed.length > 0 ? (
                  <p className="palette-note">
                    No games, characters, lines, traces, settings or actions match “{trimmed}”.
                  </p>
                ) : (
                  <p className="palette-note">
                    Add a game in the library and the palette's recent items land here.
                  </p>
                )}
              </KitEmptyState>
            </li>
          ) : null}
          {sections.map((section) => (
            <li key={section.kind}>
              {section.rows.length > 0 ? (
                <>
                  <h3
                    className="palette-group-header"
                    data-testid={`palette-group-${section.kind}`}
                  >
                    {KIND_LABELS[section.kind]}
                  </h3>
                  <div>
                    {section.rows.map((row) => {
                      flatIndex += 1;
                      const index = flatIndex;
                      return (
                        <button
                          key={row.id}
                          type="button"
                          className="palette-row"
                          ref={(element) => {
                            rowRefs.current[index] = element;
                          }}
                          data-testid="palette-row"
                          data-kind={row.kind}
                          data-id={row.id}
                          data-route={row.route}
                          data-action={
                            row.action !== undefined
                              ? `${row.action.method} ${row.action.path}`
                              : undefined
                          }
                          onClick={() => runRow(row)}
                          onKeyDown={(event) => onRowKeyDown(event, index)}
                        >
                          <span className="palette-row-title">{row.title}</span>
                          {row.subtitle !== undefined ? (
                            <span className="palette-row-subtitle">{row.subtitle}</span>
                          ) : null}
                          {row.kind === "line" ? (
                            <span className="palette-row-id" data-testid="palette-row-id">
                              {row.id}
                            </span>
                          ) : null}
                        </button>
                      );
                    })}
                  </div>
                </>
              ) : null}
            </li>
          ))}
        </ul>
        <p className="palette-footnote" data-testid="palette-footnote">
          <span>
            <kbd>↑↓</kbd> move · <kbd>↵</kbd> open · <kbd>tab</kbd> ask agent
          </span>
          <span className="palette-footnote-meta">
            {running ? "Running…" : `Esc close${trimmed.length > 0 ? ` · “${trimmed}”` : ""}`}
          </span>
        </p>
      </div>
    </>
  );
}

function rowFromItem(item: SearchResult): PaletteRow {
  return {
    kind: item.kind,
    id: item.id,
    title: item.title,
    subtitle: item.subtitle,
    route: item.route,
    action: item.action ?? undefined,
  };
}
