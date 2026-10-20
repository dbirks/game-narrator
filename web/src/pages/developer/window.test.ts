/**
 * `window.test.ts` — the crop-basis math behind the developer inspector's
 * frame overlay (ADR-0082 amendment 1): payload parsing, the region rect
 * mapped through the tracked window, the window outline, and the basis label.
 */

import { describe, expect, it } from "vitest";

import {
  activeWindowFromPayload,
  cropBasisLabel,
  regionThroughWindow,
  windowOutline,
  type ActiveWindow,
} from "./window";

const win: ActiveWindow = { x: 0.05, y: 0.2, w: 0.9, h: 0.75, settled: true };

const region = { rect_x: 0.05, rect_y: 0.62, rect_w: 0.8, rect_h: 0.25 };

describe("activeWindowFromPayload", () => {
  it("reads a well-formed window", () => {
    const got = activeWindowFromPayload({ active_window: { ...win } });
    expect(got).toEqual(win);
  });

  it("reads a whole-frame window as unsettled-ish data, not as an outline", () => {
    const got = activeWindowFromPayload({
      active_window: { x: 0, y: 0, w: 1, h: 1, settled: false },
    });
    expect(got).not.toBeNull();
    if (got !== null) {
      expect(windowOutline(got)).toBeNull();
      expect(cropBasisLabel(got)).toBe("Crop basis: full frame (window not settled)");
    }
  });

  it("returns null for absent, malformed or out-of-unit windows", () => {
    expect(activeWindowFromPayload({})).toBeNull();
    expect(activeWindowFromPayload({ active_window: null })).toBeNull();
    expect(activeWindowFromPayload({ active_window: "nope" })).toBeNull();
    expect(
      activeWindowFromPayload({ active_window: { x: 0, y: 0, w: 2, h: 1, settled: true } }),
    ).toBeNull();
    expect(
      activeWindowFromPayload({ active_window: { x: -1, y: 0, w: 1, h: 1, settled: true } }),
    ).toBeNull();
    expect(
      activeWindowFromPayload({
        active_window: { x: 0.9, y: 0, w: 0.2, h: 1, settled: true }, // x + w > 1
      }),
    ).toBeNull();
    expect(
      activeWindowFromPayload({
        active_window: { x: 0.1, y: 0.1, w: 0.5, settled: true }, // h missing
      }),
    ).toBeNull();
  });
});

describe("regionThroughWindow", () => {
  it("maps the region through the window into frame percents", () => {
    // The ADR's measured 1080p window, in fractions: x 72..1835 / 1920,
    // y 136..989 / 1080.
    const hdmi: ActiveWindow = {
      x: 72 / 1920,
      y: 136 / 1080,
      w: (1835 - 72) / 1920,
      h: (989 - 136) / 1080,
      settled: true,
    };
    const box = regionThroughWindow(region, hdmi);
    expect(box).not.toBeNull();
    if (box !== null) {
      // left = (72 + 0.05 * 1763) / 1920, top = (136 + 0.62 * 853) / 1080, …
      const expectLeft = ((72 + 0.05 * 1763) / 1920) * 100;
      const expectTop = ((136 + 0.62 * 853) / 1080) * 100;
      const expectWidth = ((0.8 * 1763) / 1920) * 100;
      const expectHeight = ((0.25 * 853) / 1080) * 100;
      expect(box.left).toBeCloseTo(expectLeft, 6);
      expect(box.top).toBeCloseTo(expectTop, 6);
      expect(box.width).toBeCloseTo(expectWidth, 6);
      expect(box.height).toBeCloseTo(expectHeight, 6);
    }
  });

  it("keeps the historic whole-frame box without a window", () => {
    const box = regionThroughWindow(region, null);
    expect(box).toEqual({
      left: 5,
      top: 62,
      width: 80,
      height: 25,
    });
  });

  it("keeps the whole-frame box for a whole-frame basis", () => {
    const whole: ActiveWindow = { x: 0, y: 0, w: 1, h: 1, settled: false };
    expect(regionThroughWindow(region, whole)).toEqual({
      left: 5,
      top: 62,
      width: 80,
      height: 25,
    });
  });

  it("is null when the panel has no region to draw", () => {
    expect(regionThroughWindow(null, win)).toBeNull();
  });
});

describe("windowOutline", () => {
  it("outlines a settled non-trivial window", () => {
    expect(windowOutline(win)).toEqual({ left: 5, top: 20, width: 90, height: 75 });
  });

  it("outlines nothing for a whole-frame or missing window", () => {
    expect(windowOutline({ x: 0, y: 0, w: 1, h: 1, settled: true })).toBeNull();
    expect(windowOutline(null)).toBeNull();
  });
});

describe("cropBasisLabel", () => {
  it("names the settled window with its frame fractions", () => {
    const label = cropBasisLabel(win);
    expect(label).toContain("active window");
    expect(label).toContain("tracker settled");
  });

  it("says full frame when nothing settled", () => {
    expect(cropBasisLabel({ x: 0, y: 0, w: 1, h: 1, settled: false })).toBe(
      "Crop basis: full frame (window not settled)",
    );
  });

  it("says not stored when the event carries no window", () => {
    expect(cropBasisLabel(null)).toContain("not stored");
  });
});
