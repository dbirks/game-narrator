// The region editor's box math (docs/spec/09 "/games/:id/regions").
//
// Every coordinate is a fraction of the frame in [0,1] — the wire form the
// API stores (normalized geometry, docs/spec/05) — and the one rule the store
// enforces on the way in is that the box stays inside the unit square
// (x + w ≤ 1, y + h ≤ 1). The editor applies the same rule locally so a drag
// or nudge never paints a box the save is going to refuse, and the pixel
// presets are the editor's shorthand for the min/max aspect and minimum
// resolution a textbox needs to read reliably.

/** One canvas box in normalized [0,1] frame fractions. */
export interface RegionBox {
  /** The stored region this box edits; empty while it is still unsaved. */
  id?: string;
  x: number;
  y: number;
  w: number;
  h: number;
}

/** A pixel/aspect preset the editor can stamp onto a box. */
export interface AspectPreset {
  label: string;
  /** min_aspect = max_aspect = the frame's aspect (width / height). */
  aspect: number;
  minWidthPx: number;
  minHeightPx: number;
}

/** The frame shapes the synthetic corpus and the captures are calibrated on. */
export const ASPECT_PRESETS: readonly AspectPreset[] = [
  { label: "4:3 (640×480)", aspect: 640 / 480, minWidthPx: 640, minHeightPx: 480 },
  { label: "16:9 (854×480)", aspect: 854 / 480, minWidthPx: 854, minHeightPx: 480 },
  { label: "1:1 (480×480)", aspect: 1, minWidthPx: 480, minHeightPx: 480 },
];

const EPSILON = 1e-9;

/** Clamp a number into [0, 1]; NaN reads as 0, ±∞ as the frame edge. */
export function clampUnit(value: number): number {
  if (Number.isNaN(value)) return 0;
  if (!Number.isFinite(value)) return value > 0 ? 1 : 0;
  return Math.min(1, Math.max(0, value));
}

/**
 * Make a box legal without moving what is already legal: coordinates clamp
 * into [0,1] and the size shrinks until the box fits the frame. A box that is
 * dragged entirely off-frame shrinks to the empty box at its corner — the
 * editor hides empty boxes, the API would 422 them.
 */
export function normalizeBox(box: RegionBox): RegionBox {
  const x = clampUnit(box.x);
  const y = clampUnit(box.y);
  const w = clampUnit(box.w);
  const h = clampUnit(box.h);
  return {
    ...(box.id !== undefined ? { id: box.id } : {}),
    x,
    y,
    w: Math.min(w, 1 - x),
    h: Math.min(h, 1 - y),
  };
}

/** Move a box by (dx, dy) frame fractions, keeping it inside the frame. */
export function nudgeBox(box: RegionBox, dx: number, dy: number): RegionBox {
  const x = Math.min(1 - box.w, Math.max(0, box.x + dx));
  const y = Math.min(1 - box.h, Math.max(0, box.y + dy));
  return { ...box, x, y };
}

/** Resize a box from its given corner, keeping it inside the frame. */
export function resizeBox(
  box: RegionBox,
  handle: "nw" | "ne" | "sw" | "se",
  dx: number,
  dy: number,
): RegionBox {
  const right = box.x + box.w;
  const bottom = box.y + box.h;
  let x = box.x;
  let y = box.y;
  let w = box.w;
  let h = box.h;
  if (handle === "se") {
    w = right + dx - box.x;
    h = bottom + dy - box.y;
  } else if (handle === "sw") {
    w = right - (box.x + dx);
    h = bottom + dy - box.y;
    x = box.x + dx;
  } else if (handle === "ne") {
    w = right + dx - box.x;
    h = bottom - (box.y + dy);
    y = box.y + dy;
  } else {
    w = right - (box.x + dx);
    h = bottom - (box.y + dy);
    x = box.x + dx;
    y = box.y + dy;
  }
  return normalizeBox({ ...box, x, y, w, h });
}

/**
 * Nudge in one editor step: a plain arrow moves a box by `step` frame
 * fractions, a held Shift moves it by ten steps — the same ten-to-one ratio
 * the pixel canvas applies, so coarse and fine motion feel like one control.
 */
export function nudgeStep(shift: boolean): number {
  return shift ? 0.05 : 0.005;
}

/**
 * Stamp a preset onto a box: the box takes the preset's aspect at the
 * largest size the frame's remaining room allows (at least the preset's
 * minimum width as a fraction of a 640-wide frame), and the min/max aspect
 * pair is set so the stored region reads "this box is for a frame of this
 * shape". Position is preserved; the box only shrinks, so a preset can never
 * produce a box the save would refuse.
 */
export function applyPreset(box: RegionBox, preset: AspectPreset) {
  const requested = Math.max(box.w, preset.minWidthPx / 640);
  const wMax = Math.max(0, Math.min(1 - clampUnit(box.x), (1 - clampUnit(box.y)) * preset.aspect));
  const w = Math.min(requested, wMax);
  const h = w / preset.aspect;
  return {
    box: normalizeBox({ ...box, w, h }),
    minAspect: round(preset.aspect),
    maxAspect: round(preset.aspect),
    minWidthPx: preset.minWidthPx,
    minHeightPx: preset.minHeightPx,
  };
}

/** Round to three decimals: the wire is a float and the editor is not a CAD tool. */
function round(value: number): number {
  return Math.round(value * 1000) / 1000;
}

/** Two boxes are the same box when all four fractions agree within float noise. */
export function boxesEqual(a: RegionBox, b: RegionBox): boolean {
  return (
    Math.abs(a.x - b.x) < EPSILON &&
    Math.abs(a.y - b.y) < EPSILON &&
    Math.abs(a.w - b.w) < EPSILON &&
    Math.abs(a.h - b.h) < EPSILON
  );
}

/**
 * The virtual list's window: given the scroll offset, the row height and the
 * viewport height, which rows are on screen (plus a small overscan so a fast
 * wheel does not paint a blank gap on the next frame).
 */
export function visibleRange(
  scrollTop: number,
  rowHeight: number,
  viewportHeight: number,
  total: number,
  overscan = 6,
): [number, number] {
  if (total <= 0 || rowHeight <= 0) return [0, 0];
  const start = Math.max(0, Math.floor(scrollTop / rowHeight) - overscan);
  const end = Math.min(total, Math.ceil((scrollTop + viewportHeight) / rowHeight) + overscan);
  return [start, end];
}
