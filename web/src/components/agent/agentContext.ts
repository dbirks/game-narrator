/**
 * `src/components/agent/agentContext.ts` — the dock's screen context (UX-01).
 *
 * Every agent message carries the context payload
 * `{route, gameId, profileId, selection: {type, id}, visibleIds[]}` so an
 * instruction resolves against the screen it was written on ("make her older"
 * means the selected character). The dock derives that payload from the URL
 * it renders on:
 *
 * - `route` is the pathname plus query (the query is where the selection
 *   lives: `?character=`, `?line=`), bounded by the server's 300-char rule;
 * - `gameId` is the `/games/:id/…` segment when the screen names a game;
 * - `profileId` is the `?profile=` revision picker the pages already use;
 * - `selection` is the `?character=` / `?line=` id, kept only when it is an
 *   id the server will accept (a malformed id is dropped rather than 422'd);
 * - `visibleIds` is what a page registers into the enrichment store below —
 *   roster order on a list page, timeline order on a trace. Pages opt in;
 *   the dock works without them (the payload's "selection is null/absent"
 *   case is still honest).
 *
 * The chip text is the same seam the pill and the docked header read: the
 * bare screen name, or `screen › selection` once the selection's name has
 * resolved (the dock fetches it from the page's own data, e.g. the profile's
 * character roster — the chip must say `cast › Marla`, not a ULID).
 *
 * `routeContextChip` (the route→screen map) lives here — not in Shell —
 * because the dock's chip and the shell's pill read the same seam; Shell
 * re-exports it for the pre-existing imports.
 */

import { useEffect, useState, useSyncExternalStore } from "react";

import { api } from "../../api/client";
import type { AssistantScreenContext } from "../../api/assistant";

/** The server's context bounds (internal/assistant/context.go): the route is
 *  300 chars, visible_ids 64 entries of 1..64 chars. Truncated to the same
 *  bounds client-side so a long query string cannot 422 the whole message. */
const MAX_ROUTE = 300;
const MAX_VISIBLE_IDS = 64;
const MAX_VISIBLE_ID_LEN = 64;

/** A ULID-shaped identifier (games.ValidID): 26 Crockford chars, the first
 *  0-7. The selection's id must pass this or the server refuses the message;
 *  a query param that does not name an id is not a selection. */
const ID_RE = /^[0-7][0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{25}$/;

/** The open selection vocabulary (docs/design/plum-peach UX-01): the URL
 *  param that names it and the selection type it carries. New kinds join
 *  here as pages get selections; unknown params are plain context, not
 *  selections. */
const SELECTION_PARAMS: Record<string, string> = {
  character: "character",
  line: "line",
};

/** The enrichment a page registers for the screen it renders: the ids
 *  visible in its main list. One active registration at a time — a page sets
 *  it in an effect and clears it on unmount, so a route change cannot carry
 *  the previous screen's ids along. */
export interface AgentEnrichment {
  visibleIds?: string[];
}

let enrichment: AgentEnrichment = {};
const listeners = new Set<() => void>();

/** Pages register (and clear) their visible ids through this seam. */
export function setAgentEnrichment(next: AgentEnrichment): void {
  enrichment = next;
  for (const listener of listeners) listener();
}

function subscribeEnrichment(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** The registered enrichment, reactive for the component that reads it. */
export function useAgentEnrichment(): AgentEnrichment {
  return useSyncExternalStore(
    subscribeEnrichment,
    () => enrichment,
    () => enrichment,
  );
}

export interface AgentLocation {
  pathname: string;
  search: string;
}

/** The screen's context payload, derived from the URL (and the page's
 *  enrichment, when one is registered). Pure: the unit test and the dock
 *  both run this, so the chip and the outgoing body cannot disagree. */
export function deriveAgentContext(
  location: AgentLocation,
  registered: AgentEnrichment = {},
): AssistantScreenContext {
  const route = (location.pathname + location.search).slice(0, MAX_ROUTE);
  const context: AssistantScreenContext = { route };

  const segments = location.pathname.split("/").filter(Boolean);
  if (segments[0] === "games" && segments[1] !== undefined) {
    context.game_id = segments[1];
  }

  const params = new URLSearchParams(location.search);
  const profileId = params.get("profile");
  if (profileId !== null && profileId !== "") context.profile_id = profileId;

  const selection = selectionFromLocation(location);
  if (selection !== null) context.selection = selection;

  if (registered.visibleIds !== undefined) {
    const ids = registered.visibleIds
      .filter((id) => id.length >= 1 && id.length <= MAX_VISIBLE_ID_LEN)
      .slice(0, MAX_VISIBLE_IDS);
    if (ids.length > 0) context.visible_ids = ids;
  }

  return context;
}

/** The agent pill's context chip map: one word for the current screen
 *  (docs/design/plum-peach "Ask the agent"). Game pages name the screen by
 *  its second segment; the rest by the first. `/games/:id/new` (setup) is
 *  "setup", the cast family is "cast" (ADR-0093 §4 merged them). */
export function routeContextChip(pathname: string): string {
  const segments = pathname.split("/").filter(Boolean);
  const head = segments[0] ?? "";
  if (head === "games") {
    if (segments[1] === "new") return "setup";
    const kind = segments[2];
    if (kind === "cast" || kind === "characters" || kind === "voices") return "cast";
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

/** The chip's full text: the screen name, or `screen › name` once the
 *  selection's name has resolved. A selection whose name has not resolved
 *  (roster still loading, or an id the roster does not know) shows the bare
 *  screen name — the id is still in the outgoing payload either way. */
export function agentChipText(screen: string, selectionName: string | null): string {
  return selectionName === null || selectionName === "" ? screen : `${screen} › ${selectionName}`;
}

/** The selection the URL names, or null. Shared by the context derivation
 *  (its payload half) and the label lookup (its display half). */
export function selectionFromLocation(location: AgentLocation): {
  type: string;
  id: string;
} | null {
  const params = new URLSearchParams(location.search);
  for (const [param, type] of Object.entries(SELECTION_PARAMS)) {
    const id = params.get(param);
    if (id !== null && ID_RE.test(id)) return { type, id };
  }
  return null;
}

interface CharacterRoster {
  characters: { id: string; name: string }[];
}

/** The name a `?character=` selection points at: the profile's roster
 *  (the same read the cast/voices pages render), or null while it has not
 *  resolved. No character selection, no profile, or a failed read all
 *  settle on null — the chip then shows the bare screen name. */
export function useSelectionLabel(
  selection: { type: string; id: string } | null,
  profileId: string | null,
): string | null {
  const [label, setLabel] = useState<string | null>(null);
  const selectionKey = selection === null ? "" : `${selection.type}:${selection.id}`;
  useEffect(() => {
    setLabel(null);
    if (selectionKey === "" || selectionKey.startsWith("line:") || profileId === null) return;
    const [type, id] = selectionKey.split(":");
    if (type !== "character") return;
    let cancelled = false;
    const controller = new AbortController();
    api
      .get("/api/v1/profiles/{id}/characters", {
        params: { id: profileId },
        signal: controller.signal,
      })
      .then((roster) => {
        if (cancelled) return;
        const found = (roster as CharacterRoster | undefined)?.characters?.find((c) => c.id === id);
        setLabel(found?.name ?? null);
      })
      .catch(() => {
        if (!cancelled) setLabel(null);
      });
    return () => {
      cancelled = true;
      controller.abort();
    };
  }, [selectionKey, profileId]);
  return label;
}
