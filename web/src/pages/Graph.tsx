import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { useParams, useSearchParams } from "react-router";

import { api, catalogApi, type Character } from "../api/client";
import { EmptyState, ErrorState, Loading } from "../components/states";
import { visibleRange } from "../lib/regionGeometry";

/**
 * `/games/:id/graph` — the virtualized scene/message graph of docs/spec/09.
 *
 * The rule it exists to obey is the same as the dialogue explorer's: never
 * mount thousands of rows. The server pages the graph (limit ≤ 2000) and
 * answers with edges only when both endpoints landed on the page, so the
 * browser holds one page's worth and paints a window of it — `visibleRange`
 * decides which rows live in the DOM. Filters (speaker, kind, scene) are
 * exact and server-side; the edge legend colours the five edge kinds, and
 * every node carries the attribution the verified-vs-inferred label renders.
 * Arrow keys walk the list; the side panel shows the selected node's text,
 * provenance and the edges touching it.
 */

const ROW_HEIGHT = 56;
/**
 * The list's height, in px. A constant by contract, not a convenience:
 * a full-page screenshot resizes the layout viewport to the document's
 * size, so a box whose height is derived from the window re-lays out inside
 * the capture; if the re-render it causes changes the document size the
 * capture is chasing, the screenshot never returns (the m5 walk's
 * intermittent 10-minute timeout, pinned by `Graph.test.tsx`). The box is
 * therefore a fixed, self-measured scroll container — never a viewport-unit
 * or window-driven height.
 */
const LIST_HEIGHT = 480;
const PAGE_LIMIT = 2000;

const EDGE_KINDS = ["next", "branch", "repeat", "contextual", "inferred"] as const;
const TEXT_KINDS = ["dialogue", "sign", "item", "menu", "tutorial", "system", "other"] as const;

const EDGE_COLORS: Record<(typeof EDGE_KINDS)[number], string> = {
  next: "#2563eb",
  branch: "#7c3aed",
  repeat: "#059669",
  contextual: "#b45309",
  inferred: "#6b7280",
};

export function GraphPage() {
  const { id: gameId = "" } = useParams();
  const [params, setParams] = useSearchParams();

  const profileId = params.get("profile") ?? "";
  const speaker = params.get("speaker") ?? "";
  const kind = params.get("kind") ?? "";
  const scene = params.get("scene") ?? "";

  // The route names a game; the graph belongs to one revision. Without
  // ?profile= the page picks a revision and puts it in the URL so a reload
  // lands on the same graph.
  const fallback = useDefaultProfile(gameId, profileId === "");
  useEffect(() => {
    if (profileId === "" && fallback) {
      const next = new URLSearchParams(params);
      next.set("profile", fallback);
      setParams(next, { replace: true });
    }
  }, [profileId, fallback, params, setParams]);

  const setFilter = (name: string, value: string) => {
    const next = new URLSearchParams(params);
    if (value === "") next.delete(name);
    else next.set(name, value);
    setParams(next);
  };

  // Scene options come from the unfiltered first page: scenes have no roster
  // endpoint, and the nodes themselves name them.
  const scenes = useQuery({
    queryKey: ["graph-scenes", profileId],
    queryFn: ({ signal }) =>
      api
        .get("/api/v1/profiles/{id}/graph", {
          params: { id: profileId },
          query: { limit: PAGE_LIMIT },
          signal,
        })
        .then((doc) => {
          const byId = new Map<string, string>();
          for (const node of doc.nodes) if (node.scene) byId.set(node.scene.id, node.scene.name);
          return [...byId.entries()];
        }),
    enabled: profileId !== "",
  });
  const characters = useQuery({
    queryKey: ["characters", profileId],
    queryFn: ({ signal }) => catalogApi.getCharacters(profileId, signal),
    enabled: profileId !== "",
  });

  const [cursor, setCursor] = useState("");
  useEffect(() => setCursor(""), [profileId, speaker, kind, scene]);

  const graph = useQuery({
    queryKey: ["graph", profileId, { speaker, kind, scene, cursor }],
    queryFn: ({ signal }) =>
      api.get("/api/v1/profiles/{id}/graph", {
        params: { id: profileId },
        query: {
          speaker: speaker || undefined,
          kind: (kind || undefined) as (typeof TEXT_KINDS)[number] | undefined,
          scene: scene || undefined,
          limit: PAGE_LIMIT,
          cursor: cursor || undefined,
        },
        signal,
      }),
    enabled: profileId !== "",
  });

  const nodes = graph.data?.nodes ?? [];
  const edges = graph.data?.edges ?? [];

  // The virtual window: the viewport is a fixed-height scroll container and
  // only the rows inside it (plus overscan) are rendered.
  const viewportRef = useRef<HTMLDivElement>(null);
  const [scrollTop, setScrollTop] = useState(0);
  const [viewportHeight, setViewportHeight] = useState(LIST_HEIGHT);
  // The container only exists once nodes have loaded (Loading/Error/Empty
  // render before it), so the observer attaches when the list mounts and
  // detaches with it; until it attaches the initial constant is the measure.
  const listMounted = graph.isSuccess && nodes.length > 0;
  useEffect(() => {
    if (!listMounted) return;
    const el = viewportRef.current;
    if (!el) return;
    // Measure the container's own box, never the window (see LIST_HEIGHT):
    // a window-derived measure would feed a full-page capture's re-layout
    // back into the document size the capture is taking.
    const measure = () => setViewportHeight(el.clientHeight);
    measure();
    const observer = new ResizeObserver(measure);
    observer.observe(el);
    return () => observer.disconnect();
  }, [listMounted]);

  const [sel, setSel] = useState(0);
  useEffect(() => setSel(0), [profileId, speaker, kind, scene, cursor]);
  const selected = nodes[Math.min(sel, nodes.length - 1)] ?? null;
  const [start, end] = visibleRange(scrollTop, ROW_HEIGHT, viewportHeight, nodes.length);

  const onKey = (event: KeyboardEvent) => {
    if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
    event.preventDefault();
    const next = Math.max(
      0,
      Math.min(nodes.length - 1, sel + (event.key === "ArrowDown" ? 1 : -1)),
    );
    setSel(next);
    const el = viewportRef.current;
    if (el) {
      const top = next * ROW_HEIGHT;
      if (top < el.scrollTop || top + ROW_HEIGHT > el.scrollTop + el.clientHeight) {
        el.scrollTo({ top: top - el.clientHeight / 2 });
      }
    }
  };

  const nodeEdges = useMemo(
    () =>
      edges.filter((edge) => selected && (edge.from === selected.id || edge.to === selected.id)),
    [edges, selected],
  );

  if (graph.isPending) return <Loading label="Loading the dialogue graph…" />;
  if (graph.isError) {
    return (
      <ErrorState
        error={graph.error}
        action="Loading the dialogue graph"
        onRetry={() => graph.refetch()}
      />
    );
  }
  if (nodes.length === 0) {
    return (
      <EmptyState title="No lines in this revision">
        <p>
          {speaker || kind || scene
            ? "The current filters match nothing — clear one and the list comes back."
            : "Import a script or add lines and the graph fills in."}
        </p>
      </EmptyState>
    );
  }

  return (
    <div className="graph-page" data-testid="graph-page">
      <header className="graph-header">
        <h1>Dialogue graph</h1>
        <div className="graph-filters" data-testid="graph-filters">
          <label>
            Speaker
            <select value={speaker} onChange={(e) => setFilter("speaker", e.target.value)}>
              <option value="">All</option>
              {(characters.data?.characters ?? []).map((c: Character) => (
                <option key={c.id} value={c.id}>
                  {c.name}
                </option>
              ))}
            </select>
          </label>
          <label>
            Kind
            <select value={kind} onChange={(e) => setFilter("kind", e.target.value)}>
              <option value="">All</option>
              {TEXT_KINDS.map((k) => (
                <option key={k} value={k}>
                  {k}
                </option>
              ))}
            </select>
          </label>
          <label>
            Scene
            <select value={scene} onChange={(e) => setFilter("scene", e.target.value)}>
              <option value="">All</option>
              {(scenes.data ?? []).map(([id, name]) => (
                <option key={id} value={id}>
                  {name}
                </option>
              ))}
            </select>
          </label>
        </div>
        <ul className="graph-legend" data-testid="graph-legend" aria-label="Edge kinds">
          {EDGE_KINDS.map((k) => (
            <li key={k}>
              <span className="graph-legend-swatch" style={{ background: EDGE_COLORS[k] }} />
              {k}
            </li>
          ))}
        </ul>
      </header>

      <div className="graph-columns">
        <div className="graph-list-wrap">
          <p className="muted">
            {nodes.length} of {graph.data.total} lines
            {graph.data.next_cursor ? " · next page available" : ""}
          </p>
          <div
            ref={viewportRef}
            role="listbox"
            aria-label="Graph nodes"
            data-testid="graph-node-list"
            tabIndex={0}
            className="graph-list"
            style={{
              // The fixed, self-measured box LIST_HEIGHT names; the
              // max-height keeps the clamp if a stylesheet ever wins the
              // height rule, so the document height the list contributes
              // cannot feed back into a full-page capture.
              height: LIST_HEIGHT,
              maxHeight: LIST_HEIGHT,
              overflowY: "auto",
              outline: "1px solid #ddd",
            }}
            onScroll={(e) => setScrollTop(e.currentTarget.scrollTop)}
            onKeyDown={onKey}
          >
            <div style={{ height: nodes.length * ROW_HEIGHT, position: "relative" }}>
              {nodes.slice(start, end).map((node, i) => {
                const index = start + i;
                return (
                  <div
                    key={node.id}
                    role="option"
                    aria-selected={index === sel}
                    data-testid={`graph-node-${node.id}`}
                    className={index === sel ? "graph-row graph-row-selected" : "graph-row"}
                    style={{ position: "absolute", top: index * ROW_HEIGHT, height: ROW_HEIGHT }}
                    onClick={() => setSel(index)}
                  >
                    <span className={`kind-badge kind-${node.kind}`}>{node.kind}</span>
                    <span className="graph-row-text">{node.text}</span>
                    {node.speaker ? (
                      <span className="graph-row-speaker">{node.speaker.name}</span>
                    ) : null}
                    <span
                      className={
                        node.attribution_status === "confirmed"
                          ? "attr-badge verified"
                          : "attr-badge inferred"
                      }
                      data-testid={`graph-node-attr-${node.id}`}
                      title={`attribution: ${node.attribution_status} (${Math.round(node.attribution_confidence * 100)}%)`}
                    >
                      {node.attribution_status === "confirmed" ? "verified" : "inferred"}
                    </span>
                  </div>
                );
              })}
            </div>
          </div>
          {graph.data.next_cursor ? (
            <button
              type="button"
              data-testid="graph-next-page"
              onClick={() => setCursor(graph.data!.next_cursor!)}
            >
              Next page
            </button>
          ) : null}
        </div>

        <aside className="graph-detail" data-testid="graph-detail">
          {selected ? (
            <>
              <h2>{selected.speaker ? selected.speaker.name : "Unattributed"}</h2>
              <p data-testid="graph-detail-text">{selected.text}</p>
              <dl className="muted">
                <div>
                  <dt>Scene</dt>
                  <dd>{selected.scene ? selected.scene.name : "—"}</dd>
                </div>
                <div>
                  <dt>Attribution</dt>
                  <dd>
                    {selected.attribution_status} ·{" "}
                    {Math.round(selected.attribution_confidence * 100)}%
                  </dd>
                </div>
              </dl>
              <h3>
                {nodeEdges.length} transition{nodeEdges.length === 1 ? "" : "s"}
              </h3>
              <ul data-testid="graph-detail-edges">
                {nodeEdges.map((edge, i) => (
                  <li key={`${edge.from}-${edge.to}-${edge.kind}-${i}`}>
                    <span
                      className="graph-legend-swatch"
                      style={{ background: EDGE_COLORS[edge.kind] }}
                    />
                    {edge.from === selected.id ? "→" : "←"} {edge.kind} ·{" "}
                    {Math.round(edge.confidence * 100)}%{edge.source ? " · sourced" : " · inferred"}{" "}
                    · evidence {edge.evidence}
                  </li>
                ))}
              </ul>
            </>
          ) : (
            <p className="muted">Select a line to read its transitions.</p>
          )}
        </aside>
      </div>
    </div>
  );
}

// The same default-revision rule the dialogue page spells: published first,
// then the newest. A page-local copy keeps the two pages independent.
function useDefaultProfile(gameId: string, enabled: boolean): string | null {
  const editions = useQuery({
    queryKey: ["editions", gameId],
    queryFn: ({ signal }) => catalogApi.listEditions(gameId, signal),
    enabled,
  });
  const editionId = (editions.data as { editions: { id: string }[] } | undefined)?.editions?.[0]
    ?.id;
  const profiles = useQuery({
    queryKey: ["profiles", editionId],
    queryFn: ({ signal }) => catalogApi.listProfiles(editionId as string, signal),
    enabled: editionId !== undefined,
  });
  return useMemo(() => {
    const list =
      (
        profiles.data as
          | { profiles: { id: string; revision_num: number; status?: string }[] }
          | undefined
      )?.profiles ?? [];
    if (list.length === 0) return null;
    const sorted = [...list].sort((a, b) => {
      const ap = a.status === "published" ? 0 : 1;
      const bp = b.status === "published" ? 0 : 1;
      return ap - bp || b.revision_num - a.revision_num;
    });
    return sorted[0]?.id ?? null;
  }, [profiles.data]);
}
