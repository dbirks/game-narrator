import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { useParams, useSearchParams } from "react-router";

import { api, catalogApi, type Character } from "../api/client";
import { EmptyState, ErrorState, Loading } from "../components/states";
import { Segmented, Table, readSegmented } from "../components/kit";
import type { components } from "../api/schema";
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
 *
 * The table view (docs/spec/09 "keyboard accessible … text equivalents") is
 * the non-canvas alternative the canvas rendering is the picture of: the
 * same page's nodes and edges as a real <table> — every message with its
 * text kind and speaker, then its successors (the transitions out of it)
 * with each successor's kind and speaker. The page's bounded ceiling (the
 * server's limit ≤ 2000) bounds the table the same way it bounds the list,
 * so the view mounts at most one page's worth of rows; the choice persists
 * per user (UX-08), the way the library and cast view toggles do.
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

const GRAPH_VIEW_OPTIONS = [
  { value: "canvas", label: "canvas" },
  { value: "table", label: "table" },
] as const;
type GraphView = (typeof GRAPH_VIEW_OPTIONS)[number]["value"];
/** UX-08: the view choice persists per user, the way the library/cast toggles. */
const GRAPH_VIEW_STORAGE_KEY = "gn-graph-view";

type GraphNode = components["schemas"]["GraphNode"];
type GraphEdge = components["schemas"]["GraphEdge"];

/** One row of the table view: a message, then its successors. */
interface GraphTableRow {
  node: GraphNode;
  successors: { edge: GraphEdge; target: GraphNode | null }[];
}

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

  // The table view (issue #34, docs/spec/09's keyboard-accessibility rule):
  // the same page's data the canvas paints — every message, its text kind
  // and speaker, then its successors with each successor's kind and speaker.
  const [view, setView] = useState<GraphView>(() =>
    readSegmented([...GRAPH_VIEW_OPTIONS], GRAPH_VIEW_STORAGE_KEY, "canvas"),
  );
  const tableRows: GraphTableRow[] = useMemo(() => {
    const byId = new Map(nodes.map((node) => [node.id, node]));
    return nodes.map((node) => ({
      node,
      successors: edges
        .filter((edge) => edge.from === node.id)
        .map((edge) => ({ edge, target: byId.get(edge.to) ?? null }))
        .sort((a, b) => b.edge.confidence - a.edge.confidence),
    }));
  }, [nodes, edges]);

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
          {/* The non-canvas view (issue #34, docs/spec/09's keyboard
              accessibility rule): the same page's data as a real table. */}
          <Segmented
            options={[...GRAPH_VIEW_OPTIONS]}
            value={view}
            onValueChange={setView}
            label="Graph view (the canvas list or the keyboard table)"
            storageKey={GRAPH_VIEW_STORAGE_KEY}
            testid="graph-view-toggle"
          />
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

      {view === "canvas" ? (
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
                      {Math.round(edge.confidence * 100)}%
                      {edge.source ? " · sourced" : " · inferred"} · evidence {edge.evidence}
                    </li>
                  ))}
                </ul>
              </>
            ) : (
              <p className="muted">Select a line to read its transitions.</p>
            )}
          </aside>
        </div>
      ) : (
        /* The table view (issue #34): the canvas's own page — the same
           nodes and edges the list virtualises — as a real, keyboard-
           navigable table. Every message row carries its text kind and
           speaker, then its successors (the transitions out of it) with
           each successor's kind and speaker. The server's page ceiling
           (limit ≤ 2000) bounds the rows, the way it bounds the list. */
        <div className="graph-table-wrap" data-testid="graph-table-wrap">
          <p className="muted">
            {nodes.length} of {graph.data.total} lines
            {graph.data.next_cursor ? " · next page available" : ""} — the canvas's own page, as a
            table
          </p>
          <Table
            caption="The dialogue graph as a table: every message on this page with its text kind and speaker, then its successors — the transitions out of it — with each successor's kind and speaker."
            columns={[
              {
                key: "message",
                header: "Message",
                cell: (row: GraphTableRow) => (
                  <span data-testid={`graph-table-text-${row.node.id}`}>{row.node.text}</span>
                ),
              },
              {
                key: "kind",
                header: "Text kind",
                cell: (row: GraphTableRow) => (
                  <span className={`kind-badge kind-${row.node.kind}`}>{row.node.kind}</span>
                ),
              },
              {
                key: "speaker",
                header: "Speaker",
                cell: (row: GraphTableRow) => row.node.speaker?.name ?? "—",
              },
              {
                key: "successors",
                header: "Successors",
                cell: (row: GraphTableRow) =>
                  row.successors.length === 0 ? (
                    <span className="muted">none on this page</span>
                  ) : (
                    <ul className="graph-table-successors">
                      {row.successors.map(({ edge, target }, i) => (
                        <li
                          key={`${edge.from}-${edge.to}-${edge.kind}-${i}`}
                          data-testid={`graph-table-successor-${row.node.id}-${i}`}
                        >
                          <span
                            className="graph-legend-swatch"
                            style={{ background: EDGE_COLORS[edge.kind] }}
                          />
                          {edge.kind} → {target ? target.text : "…"}
                          <span className="muted">
                            ({target ? target.kind : "?"}, {target?.speaker?.name ?? "unattributed"}
                            ) · {Math.round(edge.confidence * 100)}%
                          </span>
                        </li>
                      ))}
                    </ul>
                  ),
              },
            ]}
            rows={tableRows}
            rowKey={(row) => row.node.id}
            testid="graph-table"
            rowTestid={(row) => `graph-table-row-${row.node.id}`}
          />
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
      )}
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
