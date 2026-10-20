import { useEffect, useRef, useState, type ReactNode } from "react";

import { Sparkline } from "../../components/kit";
import { useLiveStatus, type StatusSnapshot } from "../../lib/liveStatus";

/**
 * `src/pages/health/tiles.tsx` — the Health screen's 3×2 tile grid
 * (docs/design/plum-peach README 2i; ADR-0111 §2).
 *
 * The grid reads the one status stream the app root already opens for the
 * status bar (`useLiveStatus`, UX-07's one-subscription-per-page rule), so
 * the tiles and the bar render the same rollup and cannot disagree: the
 * canonical six components (capture, ocr, decision, tts, agent, storage),
 * the levels the server chose (ok / slow / fail / unknown) and the
 * server's one-line detail. The client never re-derives a level from the
 * data — a component nobody probed is "unknown", never a restyled OK.
 *
 * The 12-bar sparkline is the page's own bounded window of the tile's last
 * twelve buckets (ADR-0111 §2, the run-3715836 review fix 3): a fixed one-
 * second sampler records the tile's current level from the one stream the
 * bar reads — not per push, because the feed pushes on change (ADR-0100) and
 * an idle box's frames never arrive, which left every window at the floor
 * (the "empty dashes" the review named). The bar heights encode the levels
 * (ok 100 / slow 55 / fail 25 / unknown 0 — the floor). Per-visit memory
 * only; a flat floor is still the honest rendering of "not probed yet",
 * and only of that.
 */

export type HealthLevel = "ok" | "slow" | "fail" | "unknown";

/** The canonical six, in the order the rollup and the grid share (ADR-0100). */
export const HEALTH_COMPONENTS = ["capture", "ocr", "decision", "tts", "agent", "storage"] as const;

export type HealthComponentId = (typeof HEALTH_COMPONENTS)[number];

/** The grid's display name for a component id (the tile's mono label).
 *  The design's full names (the run-3715836 review fix 2): the label wraps,
 *  never truncates, so the word the status bar uses is the word the tile
 *  shows. */
export const TILE_LABELS: Record<HealthComponentId, string> = {
  capture: "capture card",
  ocr: "OCR",
  decision: "decision model",
  tts: "voice (TTS)",
  agent: "agent model",
  storage: "storage",
};

/** The sparkline's level encoding (ADR-0111 §2). */
const LEVEL_HEIGHT: Record<HealthLevel, number> = {
  ok: 100,
  slow: 55,
  fail: 25,
  unknown: 0,
};

export const SPARK_WINDOW = 12;

/** The sampler's cadence: one bucket per second (the design's twelve bars
 *  are the tile's last twelve seconds). */
export const SPARK_TICK_MS = 1_000;

function isLevel(value: string): value is HealthLevel {
  return value === "ok" || value === "slow" || value === "fail" || value === "unknown";
}

/** One component's row of the snapshot's rollup, or absent (never rendered). */
function componentOf(
  snapshot: StatusSnapshot | null,
  id: HealthComponentId,
): { level: HealthLevel; detail: string } | null {
  const component = snapshot?.health?.components?.find((candidate) => candidate.id === id);
  if (component === undefined || component === null || !isLevel(component.level)) {
    return null;
  }
  return { level: component.level, detail: component.detail ?? "" };
}

/**
 * The page's bounded per-tile history (ADR-0111 §2): a fixed-cadence sampler
 * appends the tile's current level once per tick, the window stays at twelve
 * buckets, in-memory only. The sampler reads the feed's newest frame — the
 * one subscription the bar already opened (UX-07) — so a box whose rollup
 * never changes still fills the window with the level it keeps showing, and
 * only a tile nobody probed (unknown) renders the floor. A frame that names
 * a tile the vocabulary dropped is ignored, never invented; a component the
 * frame does not name keeps its last level.
 */
export function useTileHistory(
  snapshot: StatusSnapshot | null,
  tickMs: number = SPARK_TICK_MS,
): Map<HealthComponentId, HealthLevel[]> {
  const [history, setHistory] = useState<Map<HealthComponentId, HealthLevel[]>>(
    () => new Map(HEALTH_COMPONENTS.map((id) => [id, []])),
  );
  // The sampler reads the newest frame through a ref: the interval is the
  // page's own clock (independent of the feed's push cadence), and a
  // per-frame effect would restart it on every frame the feed delivers.
  const latest = useRef<StatusSnapshot | null>(snapshot);

  useEffect(() => {
    latest.current = snapshot;
  }, [snapshot]);

  useEffect(() => {
    const sample = () => {
      const snap = latest.current;
      if (snap === null) return; // no frame yet: the window stays honestly empty
      setHistory((previous) => {
        const next = new Map(previous);
        for (const id of HEALTH_COMPONENTS) {
          const row = componentOf(snap, id);
          if (row === null) continue;
          const windowLevels = [...(next.get(id) ?? []), row.level].slice(-SPARK_WINDOW);
          next.set(id, windowLevels);
        }
        return next;
      });
    };
    const id = setInterval(sample, tickMs);
    return () => clearInterval(id);
  }, [tickMs]);

  return history;
}

/**
 * One health tile (README 2i): the mono label, the status tag (peach ok,
 * the error language for slow/fail — coral border, ink text, so the small
 * mono word holds WCAG 1.4.3 while the border carries the hot accent — and
 * the neutral chip for unknown), the one-line detail, and the 12-bar
 * dithered sparkline. `extra` is the capture tile's signal/preview block
 * (ADR-0111 §3); the other tiles carry none.
 */
export function HealthTile({
  id,
  label,
  level,
  detail,
  values,
  extra,
}: {
  id: HealthComponentId;
  label: string;
  level: HealthLevel;
  detail: string;
  values: readonly number[];
  extra?: ReactNode;
}) {
  const warn = level === "slow" || level === "fail";
  return (
    <div
      className={warn ? "health-tile health-tile-warn" : "health-tile"}
      data-testid={`health-tile-${id}`}
      data-level={level}
    >
      <div className="health-tile-head">
        <span className="health-tile-label">{label}</span>
        <span
          className={
            level === "ok"
              ? "health-tile-tag health-tile-tag-ok"
              : warn
                ? "health-tile-tag health-tile-tag-warn"
                : "health-tile-tag health-tile-tag-unknown"
          }
          data-testid={`health-tile-${id}-tag`}
        >
          {level}
        </span>
      </div>
      <p className="health-tile-detail" data-testid={`health-tile-${id}-detail`}>
        {detail}
      </p>
      <Sparkline
        values={values}
        label={`${label}: last ${SPARK_WINDOW} seconds`}
        max={100}
        testid={`health-tile-${id}-spark`}
      />
      {extra}
    </div>
  );
}

/**
 * The 3×2 grid over the live rollup (README 2i). Renders only once the feed
 * has delivered a frame with the component rows (the feed's placeholder state
 * renders nothing: an anonymous box gets the page's sign-in affordance, not
 * a grid of invented tiles); `signalExtra` plugs the UX-11 block into the
 * capture tile.
 */
export function HealthTiles({
  signalExtra,
  sparkTickMs = SPARK_TICK_MS,
}: {
  signalExtra?: ReactNode;
  /** The sparkline sampler's cadence (the production page's is the
   *  one-second bucket; tests pass a fast tick to fill the window). */
  sparkTickMs?: number;
}) {
  const { snapshot } = useLiveStatus();
  const history = useTileHistory(snapshot, sparkTickMs);
  const rows =
    snapshot === null
      ? []
      : HEALTH_COMPONENTS.map((id) => ({ id, row: componentOf(snapshot, id) })).filter(
          (
            entry,
          ): entry is { id: HealthComponentId; row: { level: HealthLevel; detail: string } } =>
            entry.row !== null,
        );

  if (rows.length === 0) {
    return (
      <p className="muted" data-testid="health-tiles-waiting">
        Reading the health rollup…
      </p>
    );
  }

  return (
    <div className="health-grid" data-testid="health-grid">
      {rows.map(({ id, row }) => (
        <HealthTile
          key={id}
          id={id}
          label={TILE_LABELS[id]}
          level={row.level}
          detail={row.detail}
          values={(history.get(id) ?? []).map((level) => LEVEL_HEIGHT[level])}
          extra={id === "capture" ? signalExtra : undefined}
        />
      ))}
    </div>
  );
}
