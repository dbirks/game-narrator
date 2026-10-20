/**
 * `src/pages/developer/window.ts` — the crop-basis math of the developer
 * inspector (ADR-0082 amendment 1).
 *
 * The session's crops map the profile's regions through the tracked
 * active-picture window once it has settled (the whole frame until then), and
 * the dev-trace recorder stamps the window it sampled on each text.observed
 * payload (`active_window`, fractions of the frame). These functions turn
 * that payload into the boxes the DetailPanel draws over the stored frame and
 * the one-line basis label it shows: a mis-settled window is visible, not
 * guessed at.
 */

/** The `active_window` payload field: the sample's crop basis, in fractions
 *  of the frame. */
export interface ActiveWindow {
  x: number;
  y: number;
  w: number;
  h: number;
  settled: boolean;
}

/** One box in percent of the frame, the units the CSS-positioned overlay
 *  uses. */
export interface FrameBox {
  left: number;
  top: number;
  width: number;
  height: number;
}

/** A stored profile region row (rect in fractions of the active picture). */
export interface RegionRect {
  rect_x: number;
  rect_y: number;
  rect_w: number;
  rect_h: number;
}

const inUnit = (v: unknown): v is number =>
  typeof v === "number" && Number.isFinite(v) && v >= 0 && v <= 1;

/** Read `active_window` off an event payload. Null when the event predates
 *  the field (dev trace off, or an older session) or carries a malformed
 *  window: the panel degrades to the whole-frame overlay rather than drawing
 *  a box it cannot vouch for. */
export function activeWindowFromPayload(data: Record<string, unknown>): ActiveWindow | null {
  const v = data.active_window;
  if (typeof v !== "object" || v === null) return null;
  const m = v as Record<string, unknown>;
  if (!inUnit(m.x) || !inUnit(m.y) || !inUnit(m.w) || !inUnit(m.h)) return null;
  if (m.w <= 0 || m.h <= 0) return null;
  if (m.x + m.w > 1 + 1e-9 || m.y + m.h > 1 + 1e-9) return null;
  return { x: m.x, y: m.y, w: m.w, h: m.h, settled: m.settled === true };
}

const isWholeFrame = (win: ActiveWindow): boolean =>
  win.x === 0 && win.y === 0 && win.w === 1 && win.h === 1;

/** The region rect (fractions of the active picture) mapped through the
 *  window into frame percents — the box the pipeline's crop actually cut.
 *  With no window (or a whole-frame basis) the region rect is the box itself,
 *  which is the historic whole-frame overlay. Null when the panel has no
 *  region to draw. */
export function regionThroughWindow(
  region: RegionRect | null,
  win: ActiveWindow | null,
): FrameBox | null {
  if (region === null) return null;
  const x = win === null || isWholeFrame(win) ? 0 : win.x;
  const y = win === null || isWholeFrame(win) ? 0 : win.y;
  const w = win === null || isWholeFrame(win) ? 1 : win.w;
  const h = win === null || isWholeFrame(win) ? 1 : win.h;
  return {
    left: (x + region.rect_x * w) * 100,
    top: (y + region.rect_y * h) * 100,
    width: region.rect_w * w * 100,
    height: region.rect_h * h * 100,
  };
}

/** The window's own outline in frame percents: null for a whole-frame basis
 *  (nothing to outline) and for events that carry no window. */
export function windowOutline(win: ActiveWindow | null): FrameBox | null {
  if (win === null || isWholeFrame(win)) return null;
  return { left: win.x * 100, top: win.y * 100, width: win.w * 100, height: win.h * 100 };
}

const pct = (f: number): string => `${Math.round(f * 1000) / 10}%`;

/** The crop-basis line the panel shows under the frame: which window the
 *  crop was cut through, and whether the tracker had settled it at the
 *  sample instant. */
export function cropBasisLabel(win: ActiveWindow | null): string {
  if (win === null) {
    return "Crop basis: not stored in this event (dev trace off or older session)";
  }
  if (win.settled && !isWholeFrame(win)) {
    return `Crop basis: active window ${pct(win.x)},${pct(win.y)} → ${pct(win.x + win.w)},${pct(
      win.y + win.h,
    )} (tracker settled)`;
  }
  return "Crop basis: full frame (window not settled)";
}
