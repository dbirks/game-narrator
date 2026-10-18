/**
 * `src/lib/regionGeometry.test.ts` — synthetic unit tests for the region
 * editor's box math (no canvas, no server). The rule under test is the same
 * one the API enforces: a box is frame fractions in [0,1] that stay inside
 * the unit square, and a nudge or resize never produces a box the save
 * would 422.
 */
import { describe, expect, it } from "vitest";

import {
  ASPECT_PRESETS,
  applyPreset,
  boxesEqual,
  clampUnit,
  normalizeBox,
  nudgeBox,
  nudgeStep,
  resizeBox,
  visibleRange,
  type RegionBox,
} from "./regionGeometry";

const base: RegionBox = { x: 0.1, y: 0.7, w: 0.5, h: 0.2 };

describe("clampUnit", () => {
  it("clamps into [0,1] and treats NaN as 0", () => {
    expect(clampUnit(-0.2)).toBe(0);
    expect(clampUnit(1.2)).toBe(1);
    expect(clampUnit(0.42)).toBe(0.42);
    expect(clampUnit(Number.NaN)).toBe(0);
    expect(clampUnit(Infinity)).toBe(1);
  });
});

describe("normalizeBox", () => {
  it("keeps a legal box unchanged", () => {
    expect(normalizeBox(base)).toMatchObject({ x: 0.1, y: 0.7, w: 0.5, h: 0.2 });
  });

  it("shrinks a box that runs past the right edge", () => {
    const n = normalizeBox({ x: 0.6, y: 0.2, w: 0.5, h: 0.2 });
    expect(n.w).toBeCloseTo(0.4);
    expect(n.x + n.w).toBeLessThanOrEqual(1);
  });

  it("collapses an off-frame box to the empty box at its corner", () => {
    const n = normalizeBox({ x: 1.5, y: -0.5, w: 0.5, h: 0.2 });
    expect(n.x).toBe(1);
    expect(n.y).toBe(0);
    expect(n.w).toBe(0);
    // The height is still legal room (y is on-frame), so only the width dies.
    expect(n.h).toBeCloseTo(0.2);
  });

  it("preserves the id of an unsaved box", () => {
    expect(normalizeBox({ ...base, id: "abc" }).id).toBe("abc");
    expect(normalizeBox(base).id).toBeUndefined();
  });
});

describe("nudgeBox", () => {
  it("moves a box and keeps it inside the frame", () => {
    const nudged = nudgeBox(base, 0.1, -0.1);
    expect(nudged.x).toBeCloseTo(0.2);
    expect(nudged.y).toBeCloseTo(0.6);
    expect(nudged.w).toBe(base.w);
  });

  it("does not push a box past the left edge", () => {
    expect(nudgeBox(base, -1, 0).x).toBe(0);
  });

  it("does not push a box past the right edge", () => {
    const nudged = nudgeBox(base, 1, 0);
    expect(nudged.x + nudged.w).toBeLessThanOrEqual(1);
  });

  it("nudge step is ten-to-one between plain and shifted arrows", () => {
    expect(nudgeStep(false)).toBe(0.005);
    expect(nudgeStep(true)).toBe(0.05);
  });
});

describe("resizeBox", () => {
  it("grows a box from its southeast corner", () => {
    const r = resizeBox(base, "se", 0.1, 0.05);
    expect(r.x).toBe(base.x);
    expect(r.y).toBe(base.y);
    expect(r.w).toBeCloseTo(0.6);
    expect(r.h).toBeCloseTo(0.25);
  });

  it("moves the northwest corner when resizing from it", () => {
    const r = resizeBox(base, "nw", -0.05, -0.05);
    expect(r.x).toBeCloseTo(0.05);
    expect(r.y).toBeCloseTo(0.65);
    expect(r.w).toBeCloseTo(0.55);
    expect(r.h).toBeCloseTo(0.25);
  });

  it("never resizes a box past the frame", () => {
    const r = resizeBox(base, "se", 1, 1);
    expect(r.x + r.w).toBeLessThanOrEqual(1);
    expect(r.y + r.h).toBeLessThanOrEqual(1);
  });
});

describe("applyPreset", () => {
  it("stamps the preset's aspect and pixel floor onto a box", () => {
    const preset = ASPECT_PRESETS[0]; // 4:3 (640×480)
    const { box, minAspect, maxAspect, minWidthPx, minHeightPx } = applyPreset(base, preset);
    expect(box.w / box.h).toBeCloseTo(4 / 3, 5);
    expect(minAspect).toBeCloseTo(4 / 3, 2);
    expect(maxAspect).toBeCloseTo(4 / 3, 2);
    expect(minWidthPx).toBe(640);
    expect(minHeightPx).toBe(480);
    expect(box.x + box.w).toBeLessThanOrEqual(1);
  });

  it("never stamps a box larger than the frame allows", () => {
    const preset = ASPECT_PRESETS[1]; // 16:9 (854×480)
    const { box } = applyPreset({ x: 0.95, y: 0.9, w: 0.05, h: 0.05 }, preset);
    expect(box.x + box.w).toBeLessThanOrEqual(1);
    expect(box.y + box.h).toBeLessThanOrEqual(1);
  });
});

describe("boxesEqual", () => {
  it("agrees on identical boxes and disagrees on any moved edge", () => {
    expect(boxesEqual(base, { ...base })).toBe(true);
    expect(boxesEqual(base, { ...base, x: base.x + 0.01 })).toBe(false);
  });
});

describe("visibleRange", () => {
  it("windows a long list around the scroll offset", () => {
    const [start, end] = visibleRange(500, 40, 400, 1000);
    expect(start).toBeGreaterThanOrEqual(0);
    expect(end).toBeLessThanOrEqual(1000);
    expect(end - start).toBeLessThan(40); // a window, not the whole list
    expect(start).toBeLessThan(13); // the row at 500/40 is inside
  });

  it("clamps to the list's ends", () => {
    expect(visibleRange(0, 40, 400, 10)).toEqual([0, 10]);
    const [, end] = visibleRange(10_000, 40, 400, 100);
    expect(end).toBe(100);
  });

  it("is empty for an empty list", () => {
    expect(visibleRange(0, 40, 400, 0)).toEqual([0, 0]);
  });
});
